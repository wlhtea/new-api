package service

import (
	"container/heap"
	"container/list"
	"context"
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaydto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/go-redis/redis/v8"
)

const (
	openCodeGoFailoverStateVersion = 2
	openCodeGoFailoverStateDomain  = "new-api/opencode-go/failover-state/v1"
	openCodeGoFailoverRedisPrefix  = "new-api:opencode-go-failover:{v1}"
	openCodeGoFailoverMaxEntries   = 100_000
	openCodeGoFailoverRedisTimeout = 50 * time.Millisecond
)

const (
	OpenCodeGoFailoverActionNone            = "none"
	OpenCodeGoFailoverActionSuspect         = "suspect"
	OpenCodeGoFailoverActionPromoted        = "promoted"
	OpenCodeGoFailoverActionBackupExhausted = "backup_exhausted"
	OpenCodeGoFailoverActionCleared         = "cleared"
	OpenCodeGoFailoverActionLeaseRefreshed  = "lease_refreshed"
	OpenCodeGoFailoverActionStale           = "stale"
)

type OpenCodeGoFailoverPolicy struct {
	Enabled          bool
	FailureThreshold int
	FailureWindow    time.Duration
	MaxBackups       int
	LeaseDuration    time.Duration
}

type OpenCodeGoPoolSelectOptions struct {
	AffinityKey string
	Protocol    string
	Stateful    bool
	Failover    OpenCodeGoFailoverPolicy
}

type OpenCodeGoFailoverAttempt struct {
	stateKey                 string
	expectedGeneration       int64
	incarnation              string
	selectedWorkspaceUID     string
	canonicalWorkspaceUID    string
	preferredBackupWorkspace string
	excludedWorkspaceUID     string
	policy                   OpenCodeGoFailoverPolicy
	suppressFailure          bool
	observeSuccess           bool
}

func (a *OpenCodeGoFailoverAttempt) CanonicalWorkspaceUID() string {
	if a == nil {
		return ""
	}
	return a.canonicalWorkspaceUID
}

func (a *OpenCodeGoFailoverAttempt) PreferredBackupWorkspaceUID() string {
	if a == nil {
		return ""
	}
	return a.preferredBackupWorkspace
}

type OpenCodeGoFailoverObservation struct {
	Action         string
	FailureCount   int
	Generation     int64
	LeaseExpiresAt time.Time
}

type openCodeGoFailoverState struct {
	Version             int
	Generation          int64
	Incarnation         string
	PrimaryWorkspaceUID string
	ActiveWorkspaceUID  string
	FailureCount        int
	FailureWindowAtMS   int64
	LeaseExpiresAtMS    int64
	BackupUsed          bool
	BackupExhausted     bool
	ExpiresAtMS         int64
}

type openCodeGoFailoverMemoryEntry struct {
	key         string
	state       openCodeGoFailoverState
	expiryIndex int
}

type openCodeGoFailoverExpiryHeap []*openCodeGoFailoverMemoryEntry

func (h openCodeGoFailoverExpiryHeap) Len() int {
	return len(h)
}

func (h openCodeGoFailoverExpiryHeap) Less(left int, right int) bool {
	if h[left].state.ExpiresAtMS != h[right].state.ExpiresAtMS {
		return h[left].state.ExpiresAtMS < h[right].state.ExpiresAtMS
	}
	return h[left].key < h[right].key
}

func (h openCodeGoFailoverExpiryHeap) Swap(left int, right int) {
	h[left], h[right] = h[right], h[left]
	h[left].expiryIndex = left
	h[right].expiryIndex = right
}

func (h *openCodeGoFailoverExpiryHeap) Push(value interface{}) {
	entry := value.(*openCodeGoFailoverMemoryEntry)
	entry.expiryIndex = len(*h)
	*h = append(*h, entry)
}

func (h *openCodeGoFailoverExpiryHeap) Pop() interface{} {
	old := *h
	last := len(old) - 1
	entry := old[last]
	old[last] = nil
	entry.expiryIndex = -1
	*h = old[:last]
	return entry
}

type openCodeGoFailoverMemoryStore struct {
	mu       sync.Mutex
	capacity int
	entries  map[string]*list.Element
	lru      *list.List
	expiry   openCodeGoFailoverExpiryHeap
}

var (
	openCodeGoFailoverNow                = time.Now
	openCodeGoFailoverIncarnationCounter atomic.Uint64
	openCodeGoFailoverMemoryState        = newOpenCodeGoFailoverMemoryStore(openCodeGoFailoverMaxEntries)
	openCodeGoFailoverScript             = redis.NewScript(openCodeGoFailoverMutationLua)
)

func ResolveOpenCodeGoFailoverPolicy(config *relaydto.OpenCodeGoConfig) OpenCodeGoFailoverPolicy {
	policy := OpenCodeGoFailoverPolicy{
		FailureThreshold: relaydto.OpenCodeGoGenericFailoverDefaultThreshold,
		FailureWindow:    time.Duration(relaydto.OpenCodeGoGenericFailoverDefaultWindowSeconds) * time.Second,
		MaxBackups:       relaydto.OpenCodeGoGenericFailoverDefaultMaxBackups,
		LeaseDuration:    time.Duration(relaydto.OpenCodeGoGenericFailoverDefaultLeaseSeconds) * time.Second,
	}
	if config == nil {
		return policy
	}
	policy.Enabled = config.GenericFailoverEnabled
	if config.GenericFailoverThreshold > 0 {
		policy.FailureThreshold = config.GenericFailoverThreshold
	}
	if config.GenericFailoverWindowSeconds > 0 {
		policy.FailureWindow = time.Duration(config.GenericFailoverWindowSeconds) * time.Second
	}
	if config.GenericFailoverMaxBackups > 0 {
		policy.MaxBackups = config.GenericFailoverMaxBackups
	}
	if config.GenericFailoverLeaseSeconds > 0 {
		policy.LeaseDuration = time.Duration(config.GenericFailoverLeaseSeconds) * time.Second
	}
	return policy
}

func IsOpenCodeGoGenericFailoverStatus(statusCode int) bool {
	switch statusCode {
	case 500, 502, 503, 504:
		return true
	default:
		return false
	}
}

func ObserveOpenCodeGoFailoverFailure(attempt *OpenCodeGoFailoverAttempt, observedAt time.Time) (OpenCodeGoFailoverObservation, error) {
	if attempt == nil || !attempt.policy.Enabled || attempt.suppressFailure {
		return OpenCodeGoFailoverObservation{Action: OpenCodeGoFailoverActionNone}, nil
	}
	if observedAt.IsZero() {
		return OpenCodeGoFailoverObservation{}, errors.New("OpenCode Go failover observation time is required")
	}
	return mutateOpenCodeGoFailoverState("failure", attempt, observedAt)
}

func ObserveOpenCodeGoFailoverSuccess(attempt *OpenCodeGoFailoverAttempt, observedAt time.Time) (OpenCodeGoFailoverObservation, error) {
	if attempt == nil || !attempt.policy.Enabled || !attempt.observeSuccess {
		return OpenCodeGoFailoverObservation{Action: OpenCodeGoFailoverActionNone}, nil
	}
	if observedAt.IsZero() {
		return OpenCodeGoFailoverObservation{}, errors.New("OpenCode Go failover observation time is required")
	}
	return mutateOpenCodeGoFailoverState("success", attempt, observedAt)
}

func resolveOpenCodeGoFailoverSelection(
	channelID int,
	upstreamModel string,
	protocol string,
	affinityKey string,
	policy OpenCodeGoFailoverPolicy,
	ranked []openCodeGoRankedCandidate,
	now time.Time,
) (int, *OpenCodeGoFailoverAttempt, bool, time.Time, bool) {
	canonical := ranked[0].candidate
	preferredBackup := preferredOpenCodeGoFailoverBackup(ranked)
	stateKey := openCodeGoFailoverKey(channelID, upstreamModel, protocol, affinityKey)
	state, exists, err := loadOpenCodeGoFailoverState(stateKey, now)
	if err != nil {
		common.SysError(fmt.Sprintf("OpenCode Go failover state read failed open: channel_id=%d error=%v", channelID, err))
		exists = false
	}

	selectedRank := 0
	failoverActive := false
	leaseExpiresAt := time.Time{}
	leasedWorkspaceMissing := false
	missingWorkspaceUID := ""
	if exists && state.ActiveWorkspaceUID != "" && state.LeaseExpiresAtMS > now.UnixMilli() {
		leasedWorkspaceMissing = true
		missingWorkspaceUID = state.ActiveWorkspaceUID
		for index := range ranked {
			if ranked[index].candidate.workspaceUID == state.ActiveWorkspaceUID {
				selectedRank = index
				failoverActive = true
				leasedWorkspaceMissing = false
				leaseExpiresAt = time.UnixMilli(state.LeaseExpiresAtMS)
				break
			}
		}
	}

	expectedGeneration := int64(0)
	incarnation := newOpenCodeGoFailoverIncarnation()
	if exists {
		expectedGeneration = state.Generation
		incarnation = state.Incarnation
	}
	attempt := &OpenCodeGoFailoverAttempt{
		stateKey:                 stateKey,
		expectedGeneration:       expectedGeneration,
		incarnation:              incarnation,
		selectedWorkspaceUID:     ranked[selectedRank].candidate.workspaceUID,
		canonicalWorkspaceUID:    canonical.workspaceUID,
		preferredBackupWorkspace: preferredBackup.workspaceUID,
		excludedWorkspaceUID:     missingWorkspaceUID,
		policy:                   policy,
		observeSuccess:           exists && (state.ActiveWorkspaceUID != "" || state.PrimaryWorkspaceUID != "" || state.FailureCount > 0),
	}
	return selectedRank, attempt, failoverActive, leaseExpiresAt, leasedWorkspaceMissing
}

func resetOpenCodeGoMissingFailoverLease(attempt *OpenCodeGoFailoverAttempt, observedAt time.Time) (OpenCodeGoFailoverObservation, error) {
	if attempt == nil || !attempt.policy.Enabled {
		return OpenCodeGoFailoverObservation{Action: OpenCodeGoFailoverActionNone}, nil
	}
	return mutateOpenCodeGoFailoverState("rebind", attempt, observedAt)
}

func newOpenCodeGoFailoverIncarnation() string {
	var random [16]byte
	if _, err := cryptorand.Read(random[:]); err == nil {
		return base64.RawURLEncoding.EncodeToString(random[:])
	}
	hash := hmac.New(sha256.New, []byte(common.CryptoSecret))
	_, _ = fmt.Fprintf(hash, "%s\x00incarnation\x00%d\x00%d", openCodeGoFailoverStateDomain, time.Now().UnixNano(), openCodeGoFailoverIncarnationCounter.Add(1))
	return base64.RawURLEncoding.EncodeToString(hash.Sum(nil)[:16])
}

func openCodeGoFailoverRetention(policy OpenCodeGoFailoverPolicy) time.Duration {
	retention := policy.LeaseDuration
	if policy.FailureWindow > retention {
		retention = policy.FailureWindow
	}
	if retention <= 0 {
		retention = time.Duration(relaydto.OpenCodeGoGenericFailoverDefaultLeaseSeconds) * time.Second
	}
	return retention
}

func preferredOpenCodeGoFailoverBackup(ranked []openCodeGoRankedCandidate) openCodeGoPoolCandidate {
	if len(ranked) < 2 {
		return openCodeGoPoolCandidate{}
	}
	primaryIdentity := ranked[0].candidate.identityUID
	for index := 1; index < len(ranked); index++ {
		if ranked[index].candidate.identityUID != primaryIdentity {
			return ranked[index].candidate
		}
	}
	return ranked[1].candidate
}

func openCodeGoFailoverKey(channelID int, upstreamModel string, protocol string, affinityKey string) string {
	hash := hmac.New(sha256.New, []byte(common.CryptoSecret))
	_, _ = fmt.Fprintf(
		hash,
		"%s\x00%d\x00%s\x00%s\x00%s",
		openCodeGoFailoverStateDomain,
		channelID,
		strings.TrimSpace(upstreamModel),
		strings.TrimSpace(protocol),
		affinityKey,
	)
	digest := hash.Sum(nil)
	return openCodeGoFailoverRedisPrefix + ":state:" + base64.RawURLEncoding.EncodeToString(digest[:24])
}

func loadOpenCodeGoFailoverState(key string, now time.Time) (openCodeGoFailoverState, bool, error) {
	if common.RedisEnabled && common.RDB != nil {
		ctx, cancel := context.WithTimeout(context.Background(), openCodeGoFailoverRedisTimeout)
		defer cancel()
		values, err := common.RDB.HGetAll(ctx, key).Result()
		if err != nil {
			return openCodeGoFailoverState{}, false, err
		}
		if len(values) == 0 {
			return openCodeGoFailoverState{}, false, nil
		}
		state, err := decodeOpenCodeGoFailoverRedisState(values)
		if err != nil || state.Version != openCodeGoFailoverStateVersion || state.ExpiresAtMS <= now.UnixMilli() {
			return openCodeGoFailoverState{}, false, err
		}
		return state, true, nil
	}
	return openCodeGoFailoverMemoryState.load(key, now)
}

func mutateOpenCodeGoFailoverState(operation string, attempt *OpenCodeGoFailoverAttempt, observedAt time.Time) (OpenCodeGoFailoverObservation, error) {
	if common.RedisEnabled && common.RDB != nil {
		return mutateOpenCodeGoFailoverRedisState(operation, attempt, observedAt)
	}
	return openCodeGoFailoverMemoryState.mutate(operation, attempt, observedAt), nil
}

func mutateOpenCodeGoFailoverRedisState(operation string, attempt *OpenCodeGoFailoverAttempt, observedAt time.Time) (OpenCodeGoFailoverObservation, error) {
	ctx, cancel := context.WithTimeout(context.Background(), openCodeGoFailoverRedisTimeout)
	defer cancel()
	result, err := openCodeGoFailoverScript.Run(
		ctx,
		common.RDB,
		[]string{attempt.stateKey, openCodeGoFailoverRedisPrefix + ":index"},
		openCodeGoFailoverStateVersion,
		operation,
		observedAt.UnixMilli(),
		attempt.expectedGeneration,
		attempt.incarnation,
		attempt.selectedWorkspaceUID,
		attempt.canonicalWorkspaceUID,
		attempt.preferredBackupWorkspace,
		attempt.policy.FailureThreshold,
		attempt.policy.FailureWindow.Milliseconds(),
		attempt.policy.LeaseDuration.Milliseconds(),
		openCodeGoFailoverRetention(attempt.policy).Milliseconds(),
		attempt.policy.MaxBackups,
		openCodeGoFailoverMaxEntries,
	).Result()
	if err != nil {
		return OpenCodeGoFailoverObservation{}, err
	}
	values, ok := result.([]interface{})
	if !ok || len(values) < 5 {
		return OpenCodeGoFailoverObservation{}, fmt.Errorf("invalid OpenCode Go failover Redis result %T", result)
	}
	action := redisFailoverResultString(values[0])
	generation, _ := strconv.ParseInt(redisFailoverResultString(values[1]), 10, 64)
	failureCount, _ := strconv.Atoi(redisFailoverResultString(values[2]))
	leaseMS, _ := strconv.ParseInt(redisFailoverResultString(values[4]), 10, 64)
	observation := OpenCodeGoFailoverObservation{
		Action:       action,
		FailureCount: failureCount,
		Generation:   generation,
	}
	if leaseMS > 0 {
		observation.LeaseExpiresAt = time.UnixMilli(leaseMS)
	}
	return observation, nil
}

func redisFailoverResultString(value interface{}) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	default:
		return fmt.Sprint(typed)
	}
}

func decodeOpenCodeGoFailoverRedisState(values map[string]string) (openCodeGoFailoverState, error) {
	parse := func(name string) (int64, error) {
		value := values[name]
		if value == "" {
			return 0, nil
		}
		return strconv.ParseInt(value, 10, 64)
	}
	version, err := parse("v")
	if err != nil {
		return openCodeGoFailoverState{}, err
	}
	generation, err := parse("g")
	if err != nil {
		return openCodeGoFailoverState{}, err
	}
	failureCount, err := parse("n")
	if err != nil {
		return openCodeGoFailoverState{}, err
	}
	windowAt, err := parse("w")
	if err != nil {
		return openCodeGoFailoverState{}, err
	}
	leaseAt, err := parse("l")
	if err != nil {
		return openCodeGoFailoverState{}, err
	}
	expiresAt, err := parse("e")
	if err != nil {
		return openCodeGoFailoverState{}, err
	}
	incarnation := strings.TrimSpace(values["i"])
	if incarnation == "" {
		return openCodeGoFailoverState{}, errors.New("OpenCode Go failover Redis state has no incarnation")
	}
	return openCodeGoFailoverState{
		Version:             int(version),
		Generation:          generation,
		Incarnation:         incarnation,
		PrimaryWorkspaceUID: values["p"],
		ActiveWorkspaceUID:  values["a"],
		FailureCount:        int(failureCount),
		FailureWindowAtMS:   windowAt,
		LeaseExpiresAtMS:    leaseAt,
		BackupUsed:          values["b"] == "1",
		BackupExhausted:     values["x"] == "1",
		ExpiresAtMS:         expiresAt,
	}, nil
}

func newOpenCodeGoFailoverMemoryStore(capacity int) *openCodeGoFailoverMemoryStore {
	if capacity < 1 {
		capacity = 1
	}
	return &openCodeGoFailoverMemoryStore{
		capacity: capacity,
		entries:  make(map[string]*list.Element),
		lru:      list.New(),
		expiry:   make(openCodeGoFailoverExpiryHeap, 0, capacity),
	}
}

func (s *openCodeGoFailoverMemoryStore) load(key string, now time.Time) (openCodeGoFailoverState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpired(now.UnixMilli())
	element, exists := s.entries[key]
	if !exists {
		return openCodeGoFailoverState{}, false, nil
	}
	entry := element.Value.(*openCodeGoFailoverMemoryEntry)
	s.lru.MoveToFront(element)
	return entry.state, true, nil
}

func (s *openCodeGoFailoverMemoryStore) mutate(operation string, attempt *OpenCodeGoFailoverAttempt, observedAt time.Time) OpenCodeGoFailoverObservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpired(observedAt.UnixMilli())
	element, exists := s.entries[attempt.stateKey]
	state := openCodeGoFailoverState{}
	if exists {
		state = element.Value.(*openCodeGoFailoverMemoryEntry).state
	}

	next, keep, observation := reduceOpenCodeGoFailoverState(operation, state, exists, attempt, observedAt)
	if observation.Action == OpenCodeGoFailoverActionStale || observation.Action == OpenCodeGoFailoverActionNone {
		return observation
	}
	if !keep {
		if element != nil {
			s.remove(element)
		}
		return observation
	}
	if element == nil {
		entry := &openCodeGoFailoverMemoryEntry{key: attempt.stateKey, state: next, expiryIndex: -1}
		element = s.lru.PushFront(entry)
		s.entries[attempt.stateKey] = element
		heap.Push(&s.expiry, entry)
	} else {
		entry := element.Value.(*openCodeGoFailoverMemoryEntry)
		entry.state = next
		heap.Fix(&s.expiry, entry.expiryIndex)
		s.lru.MoveToFront(element)
	}
	for len(s.entries) > s.capacity {
		s.remove(s.lru.Back())
	}
	return observation
}

func (s *openCodeGoFailoverMemoryStore) remove(element *list.Element) {
	if element == nil {
		return
	}
	entry := element.Value.(*openCodeGoFailoverMemoryEntry)
	delete(s.entries, entry.key)
	s.lru.Remove(element)
	if entry.expiryIndex >= 0 {
		heap.Remove(&s.expiry, entry.expiryIndex)
	}
}

func (s *openCodeGoFailoverMemoryStore) purgeExpired(nowMS int64) {
	for len(s.expiry) > 0 && s.expiry[0].state.ExpiresAtMS <= nowMS {
		entry := s.expiry[0]
		if element, exists := s.entries[entry.key]; exists {
			s.remove(element)
			continue
		}
		// Keep the heap self-healing if a corrupted or legacy entry is found.
		heap.Pop(&s.expiry)
	}
}

func reduceOpenCodeGoFailoverState(
	operation string,
	state openCodeGoFailoverState,
	exists bool,
	attempt *OpenCodeGoFailoverAttempt,
	observedAt time.Time,
) (openCodeGoFailoverState, bool, OpenCodeGoFailoverObservation) {
	if (exists && (state.Generation != attempt.expectedGeneration || state.Incarnation != attempt.incarnation)) ||
		(!exists && attempt.expectedGeneration != 0) {
		return state, exists, OpenCodeGoFailoverObservation{Action: OpenCodeGoFailoverActionStale, Generation: state.Generation}
	}
	nowMS := observedAt.UnixMilli()
	if operation == "rebind" {
		if !exists {
			return state, false, OpenCodeGoFailoverObservation{Action: OpenCodeGoFailoverActionNone}
		}
		state.Generation++
		state.PrimaryWorkspaceUID = ""
		state.ActiveWorkspaceUID = ""
		state.FailureCount = 0
		state.FailureWindowAtMS = 0
		state.LeaseExpiresAtMS = 0
		state.BackupUsed = false
		state.BackupExhausted = false
		state.ExpiresAtMS = nowMS + openCodeGoFailoverRetention(attempt.policy).Milliseconds()
		return state, true, OpenCodeGoFailoverObservation{Action: OpenCodeGoFailoverActionCleared, Generation: state.Generation}
	}
	if operation == "success" {
		if !exists {
			return state, false, OpenCodeGoFailoverObservation{Action: OpenCodeGoFailoverActionNone}
		}
		if state.ActiveWorkspaceUID != "" {
			if state.ActiveWorkspaceUID != attempt.selectedWorkspaceUID {
				return state, true, OpenCodeGoFailoverObservation{Action: OpenCodeGoFailoverActionStale, Generation: state.Generation}
			}
			state.Generation++
			state.BackupExhausted = false
			state.LeaseExpiresAtMS = nowMS + attempt.policy.LeaseDuration.Milliseconds()
			state.ExpiresAtMS = state.LeaseExpiresAtMS
			return state, true, OpenCodeGoFailoverObservation{
				Action:         OpenCodeGoFailoverActionLeaseRefreshed,
				Generation:     state.Generation,
				LeaseExpiresAt: time.UnixMilli(state.LeaseExpiresAtMS),
			}
		}
		if state.PrimaryWorkspaceUID == "" && state.FailureCount == 0 {
			return state, true, OpenCodeGoFailoverObservation{Action: OpenCodeGoFailoverActionNone, Generation: state.Generation}
		}
		state.Generation++
		state.PrimaryWorkspaceUID = ""
		state.ActiveWorkspaceUID = ""
		state.FailureCount = 0
		state.FailureWindowAtMS = 0
		state.LeaseExpiresAtMS = 0
		state.BackupUsed = false
		state.BackupExhausted = false
		state.ExpiresAtMS = nowMS + openCodeGoFailoverRetention(attempt.policy).Milliseconds()
		return state, true, OpenCodeGoFailoverObservation{Action: OpenCodeGoFailoverActionCleared, Generation: state.Generation}
	}
	if operation != "failure" {
		return state, exists, OpenCodeGoFailoverObservation{Action: OpenCodeGoFailoverActionNone}
	}

	if exists && state.ActiveWorkspaceUID != "" {
		if state.ActiveWorkspaceUID != attempt.selectedWorkspaceUID {
			return state, true, OpenCodeGoFailoverObservation{Action: OpenCodeGoFailoverActionStale, Generation: state.Generation}
		}
		state.Generation++
		state.BackupExhausted = true
		return state, true, OpenCodeGoFailoverObservation{
			Action:         OpenCodeGoFailoverActionBackupExhausted,
			Generation:     state.Generation,
			LeaseExpiresAt: time.UnixMilli(state.LeaseExpiresAtMS),
		}
	}

	if !exists || state.PrimaryWorkspaceUID != attempt.selectedWorkspaceUID ||
		state.FailureWindowAtMS <= 0 || nowMS-state.FailureWindowAtMS > attempt.policy.FailureWindow.Milliseconds() {
		state = openCodeGoFailoverState{
			Version:             openCodeGoFailoverStateVersion,
			Generation:          attempt.expectedGeneration,
			Incarnation:         attempt.incarnation,
			PrimaryWorkspaceUID: attempt.selectedWorkspaceUID,
			FailureWindowAtMS:   nowMS,
		}
	}
	state.Generation++
	state.FailureCount++
	if state.FailureCount >= attempt.policy.FailureThreshold &&
		attempt.policy.MaxBackups > 0 && !state.BackupUsed &&
		attempt.preferredBackupWorkspace != "" &&
		attempt.preferredBackupWorkspace != attempt.selectedWorkspaceUID {
		state.ActiveWorkspaceUID = attempt.preferredBackupWorkspace
		state.FailureCount = 0
		state.BackupUsed = true
		state.BackupExhausted = false
		state.LeaseExpiresAtMS = nowMS + attempt.policy.LeaseDuration.Milliseconds()
		state.ExpiresAtMS = state.LeaseExpiresAtMS
		return state, true, OpenCodeGoFailoverObservation{
			Action:         OpenCodeGoFailoverActionPromoted,
			Generation:     state.Generation,
			LeaseExpiresAt: time.UnixMilli(state.LeaseExpiresAtMS),
		}
	}
	state.ExpiresAtMS = state.FailureWindowAtMS + attempt.policy.FailureWindow.Milliseconds()
	return state, true, OpenCodeGoFailoverObservation{
		Action:       OpenCodeGoFailoverActionSuspect,
		FailureCount: state.FailureCount,
		Generation:   state.Generation,
	}
}

const openCodeGoFailoverMutationLua = `
local version = tonumber(ARGV[1])
local operation = ARGV[2]
local now = tonumber(ARGV[3])
local expected = tonumber(ARGV[4])
local expected_incarnation = ARGV[5]
local selected = ARGV[6]
local canonical = ARGV[7]
local backup = ARGV[8]
local threshold = tonumber(ARGV[9])
local window = tonumber(ARGV[10])
local lease = tonumber(ARGV[11])
local retention = tonumber(ARGV[12])
local max_backups = math.max(tonumber(ARGV[13]) or 0, 0)
local max_entries = math.max(tonumber(ARGV[14]) or 1, 1)
local cleanup_batch = 128

local function clear_state()
  redis.call('DEL', KEYS[1])
  redis.call('ZREM', KEYS[2], KEYS[1])
end

local function remove_indexed_state(key, indexed_expiry)
  if key == KEYS[1] then
    return false
  end
  local stored_expiry = tonumber(redis.call('HGET', key, 'e'))
  if stored_expiry and stored_expiry > indexed_expiry then
    redis.call('ZADD', KEYS[2], stored_expiry, key)
    return false
  end
  redis.call('DEL', key)
  redis.call('ZREM', KEYS[2], key)
  return true
end

local function cleanup_expired()
  local expired = redis.call(
    'ZRANGE', KEYS[2], 0, cleanup_batch - 1, 'WITHSCORES')
  for index = 1, #expired, 2 do
    -- Some Redis-compatible servers parse ZRANGEBYSCORE LIMIT differently.
    -- Fetch only the bounded oldest slice, then enforce the expiry predicate
    -- explicitly so a future entry is never removed as a side effect of cleanup.
    local indexed_expiry = tonumber(expired[index + 1]) or 0
    if indexed_expiry <= now then
      remove_indexed_state(expired[index], indexed_expiry)
    end
  end
end

local function enforce_capacity()
  local overflow = redis.call('ZCARD', KEYS[2]) - max_entries
  if overflow <= 0 then
    return
  end
  local scan_count = math.min(overflow + 1, cleanup_batch + 1)
  local candidates = redis.call('ZRANGE', KEYS[2], 0, scan_count - 1, 'WITHSCORES')
  local removed = 0
  for index = 1, #candidates, 2 do
    if removed >= overflow then
      break
    end
    if remove_indexed_state(candidates[index], tonumber(candidates[index + 1]) or 0) then
      removed = removed + 1
    end
  end
end

local function persist_state(g, incarnation, p, a, n, w, l, b, x, e)
  cleanup_expired()
  redis.call('HSET', KEYS[1],
    'v', version, 'g', g, 'i', incarnation, 'p', p, 'a', a, 'n', n, 'w', w,
    'l', l, 'b', b, 'x', x, 'e', e)
  redis.call('PEXPIREAT', KEYS[1], e)
  redis.call('ZADD', KEYS[2], e, KEYS[1])
  enforce_capacity()
end

local stored_version = tonumber(redis.call('HGET', KEYS[1], 'v'))
local expires = tonumber(redis.call('HGET', KEYS[1], 'e')) or 0
if stored_version and (stored_version ~= version or expires <= now) then
  clear_state()
  stored_version = nil
end

local exists = stored_version ~= nil
local generation = tonumber(redis.call('HGET', KEYS[1], 'g')) or 0
local incarnation = exists and (redis.call('HGET', KEYS[1], 'i') or '') or expected_incarnation
if (exists and (generation ~= expected or incarnation ~= expected_incarnation)) or ((not exists) and expected ~= 0) then
  return {'stale', tostring(generation), '0', '', '0'}
end

if operation == 'rebind' then
  if not exists then
    return {'none', '0', '0', '', '0'}
  end
  generation = generation + 1
  local retention_at = now + retention
  persist_state(generation, incarnation, '', '', 0, 0, 0, 0, 0, retention_at)
  return {'cleared', tostring(generation), '0', '', '0'}
end

if operation == 'success' then
  if not exists then
    return {'none', '0', '0', '', '0'}
  end
  local active = redis.call('HGET', KEYS[1], 'a') or ''
  if active ~= '' then
    if active ~= selected then
      return {'stale', tostring(generation), '0', active, redis.call('HGET', KEYS[1], 'l') or '0'}
    end
    generation = generation + 1
    local lease_at = now + lease
    persist_state(generation, incarnation,
      redis.call('HGET', KEYS[1], 'p') or canonical,
      active, 0, redis.call('HGET', KEYS[1], 'w') or now,
      lease_at, 1, 0, lease_at)
    return {'lease_refreshed', tostring(generation), '0', active, tostring(lease_at)}
  end
  local primary = redis.call('HGET', KEYS[1], 'p') or ''
  local count = tonumber(redis.call('HGET', KEYS[1], 'n')) or 0
  if primary == '' and count == 0 then
    return {'none', tostring(generation), '0', '', '0'}
  end
  generation = generation + 1
  local retention_at = now + retention
  persist_state(generation, incarnation, '', '', 0, 0, 0, 0, 0, retention_at)
  return {'cleared', tostring(generation), '0', '', '0'}
end

if operation ~= 'failure' then
  return {'none', tostring(generation), '0', '', '0'}
end

if exists then
  local active = redis.call('HGET', KEYS[1], 'a') or ''
  if active ~= '' then
    if active ~= selected then
      return {'stale', tostring(generation), '0', active, redis.call('HGET', KEYS[1], 'l') or '0'}
    end
    generation = generation + 1
    local lease_at = tonumber(redis.call('HGET', KEYS[1], 'l')) or (now + lease)
    persist_state(generation, incarnation,
      redis.call('HGET', KEYS[1], 'p') or canonical,
      active, 0, redis.call('HGET', KEYS[1], 'w') or now,
      lease_at, 1, 1, lease_at)
    return {'backup_exhausted', tostring(generation), '0', active, tostring(lease_at)}
  end
end

local primary = exists and (redis.call('HGET', KEYS[1], 'p') or '') or ''
local window_at = exists and (tonumber(redis.call('HGET', KEYS[1], 'w')) or 0) or 0
local count = exists and (tonumber(redis.call('HGET', KEYS[1], 'n')) or 0) or 0
local backup_used = exists and (tonumber(redis.call('HGET', KEYS[1], 'b')) or 0) or 0
if (not exists) or primary ~= selected or window_at <= 0 or (now - window_at) > window then
  primary = selected
  window_at = now
  count = 0
  backup_used = 0
end
generation = generation + 1
count = count + 1

if count >= threshold and max_backups > 0 and backup_used == 0 and backup ~= '' and backup ~= selected then
  local lease_at = now + lease
  persist_state(generation, incarnation, primary, backup, 0, window_at, lease_at, 1, 0, lease_at)
  return {'promoted', tostring(generation), '0', backup, tostring(lease_at)}
end

local expires_at = window_at + window
persist_state(generation, incarnation, primary, '', count, window_at, 0, backup_used, 0, expires_at)
return {'suspect', tostring(generation), tostring(count), '', '0'}
`
