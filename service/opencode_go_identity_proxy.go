package service

import (
	"container/list"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

const (
	openCodeGoIdentityProxySIDDomain       = "new-api/opencode-go/identity-proxy-sid/v1"
	openCodeGoIdentityProxyReferenceDomain = "new-api/opencode-go/identity-proxy-reference/v1"
	openCodeGoIdentityProxyCacheKeyDomain  = "new-api/opencode-go/identity-proxy-cache-key/v1"
	openCodeGoIdentityProxyCacheCapacity   = 1024
	openCodeGoIdentityProxyCacheMaxAge     = 6 * time.Hour
	openCodeGoIdentityProxySIDBase         = uint64(100_000_000_000)
	openCodeGoIdentityProxySIDRange        = uint64(900_000_000_000)
)

type openCodeGoIdentityProxyCacheEntry struct {
	key         string
	channelID   int
	identityRef string
	bucket      int64
	createdAt   time.Time
	client      *http.Client
	element     *list.Element
}

type openCodeGoIdentityProxyClientCache struct {
	mutex                sync.Mutex
	generationMutex      sync.Mutex
	entries              map[string]*openCodeGoIdentityProxyCacheEntry
	lru                  *list.List
	capacity             int
	maxAge               time.Duration
	globalGeneration     uint64
	channelGeneration    map[int]uint64
	invalidatingChannels map[int]uint64
	resetCount           uint64
}

// OpenCodeGoIdentityProxyGeneration binds a pool selection to the proxy-cache
// invalidation state of the snapshot that supplied it. Its fields remain
// private so callers can only carry the token, not construct or alter it.
type OpenCodeGoIdentityProxyGeneration struct {
	channelID int
	global    uint64
	channel   uint64
}

var ErrOpenCodeGoIdentityProxySelectionStale = errors.New("selected OpenCode Go workspace is no longer available")

var openCodeGoIdentityProxyClients = newOpenCodeGoIdentityProxyClientCache(
	openCodeGoIdentityProxyCacheCapacity,
	openCodeGoIdentityProxyCacheMaxAge,
)

type openCodeGoPoolMutationBarrier struct {
	global   sync.RWMutex
	channels sync.Map
}

var openCodeGoPoolMutations openCodeGoPoolMutationBarrier

func (barrier *openCodeGoPoolMutationBarrier) channel(channelID int) *sync.RWMutex {
	value, _ := barrier.channels.LoadOrStore(channelID, &sync.RWMutex{})
	return value.(*sync.RWMutex)
}

func (barrier *openCodeGoPoolMutationBarrier) beginRelay(channelID int) func() {
	barrier.global.RLock()
	channel := barrier.channel(channelID)
	channel.RLock()
	var once sync.Once
	return func() {
		once.Do(func() {
			channel.RUnlock()
			barrier.global.RUnlock()
		})
	}
}

// BeginOpenCodeGoPoolMutation prevents a relay request from crossing the local
// commit-to-cache-retirement window for one channel. Release it only after
// persistence, proxy-cache invalidation, and pool rebuild have completed.
func BeginOpenCodeGoPoolMutation(channelID int) func() {
	openCodeGoPoolMutations.global.RLock()
	channel := openCodeGoPoolMutations.channel(channelID)
	channel.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			channel.Unlock()
			openCodeGoPoolMutations.global.RUnlock()
		})
	}
}

// BeginOpenCodeGoPoolMutations acquires channel barriers in stable order for a
// batch mutation, preventing deadlocks between overlapping batches.
func BeginOpenCodeGoPoolMutations(channelIDs []int) func() {
	unique := make(map[int]struct{}, len(channelIDs))
	ordered := make([]int, 0, len(channelIDs))
	for _, channelID := range channelIDs {
		if channelID <= 0 {
			continue
		}
		if _, exists := unique[channelID]; exists {
			continue
		}
		unique[channelID] = struct{}{}
		ordered = append(ordered, channelID)
	}
	sort.Ints(ordered)

	openCodeGoPoolMutations.global.RLock()
	channels := make([]*sync.RWMutex, 0, len(ordered))
	for _, channelID := range ordered {
		channel := openCodeGoPoolMutations.channel(channelID)
		channel.Lock()
		channels = append(channels, channel)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			for index := len(channels) - 1; index >= 0; index-- {
				channels[index].Unlock()
			}
			openCodeGoPoolMutations.global.RUnlock()
		})
	}
}

// BeginOpenCodeGoGlobalPoolMutation is for mutations whose affected OpenCode
// Go channel IDs are not known until persistence has completed.
func BeginOpenCodeGoGlobalPoolMutation() func() {
	openCodeGoPoolMutations.global.Lock()
	var once sync.Once
	return func() {
		once.Do(openCodeGoPoolMutations.global.Unlock)
	}
}

func newOpenCodeGoIdentityProxyClientCache(capacity int, maxAge time.Duration) *openCodeGoIdentityProxyClientCache {
	if capacity < 1 {
		capacity = 1
	}
	return &openCodeGoIdentityProxyClientCache{
		entries:              make(map[string]*openCodeGoIdentityProxyCacheEntry),
		lru:                  list.New(),
		capacity:             capacity,
		maxAge:               maxAge,
		channelGeneration:    make(map[int]uint64),
		invalidatingChannels: make(map[int]uint64),
	}
}

func (cache *openCodeGoIdentityProxyClientCache) captureGeneration(channelID int) OpenCodeGoIdentityProxyGeneration {
	cache.generationMutex.Lock()
	defer cache.generationMutex.Unlock()
	return OpenCodeGoIdentityProxyGeneration{
		channelID: channelID,
		global:    cache.globalGeneration,
		channel:   cache.channelGeneration[channelID],
	}
}

// advanceSelectionGeneration invalidates selections made from the previous
// pool snapshot without retiring identity clients whose proxy binding did not
// change. The caller must publish a replacement snapshot before releasing its
// pool-mutation barrier.
func (cache *openCodeGoIdentityProxyClientCache) advanceSelectionGeneration(channelID int) {
	cache.generationMutex.Lock()
	cache.channelGeneration[channelID]++
	cache.generationMutex.Unlock()
}

func (cache *openCodeGoIdentityProxyClientCache) generationMatchesLocked(
	channelID int,
	generation *OpenCodeGoIdentityProxyGeneration,
) bool {
	if cache.resetCount != 0 || cache.invalidatingChannels[channelID] != 0 {
		return false
	}
	return generation == nil ||
		(generation.channelID == channelID &&
			generation.global == cache.globalGeneration &&
			generation.channel == cache.channelGeneration[channelID])
}

func (cache *openCodeGoIdentityProxyClientCache) generationMatches(
	channelID int,
	generation *OpenCodeGoIdentityProxyGeneration,
) bool {
	cache.generationMutex.Lock()
	defer cache.generationMutex.Unlock()
	return cache.generationMatchesLocked(channelID, generation)
}

func openCodeGoIdentityProxyHMAC(secret string, domain string, fields ...string) []byte {
	digest := hmac.New(sha256.New, []byte(secret))
	_, _ = digest.Write([]byte(domain))
	for _, field := range fields {
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(field))
	}
	return digest.Sum(nil)
}

func deriveOpenCodeGoIdentityProxySID(secret string, channelID int, identityUID string, bucket int64) string {
	digest := openCodeGoIdentityProxyHMAC(
		secret,
		openCodeGoIdentityProxySIDDomain,
		strconv.Itoa(channelID),
		identityUID,
		strconv.FormatInt(bucket, 10),
	)
	// IPWO rejects long uint64 decimal values. Keep the HMAC-derived session
	// identifier in a fixed 12-digit range accepted by the provider.
	value := openCodeGoIdentityProxySIDBase + binary.BigEndian.Uint64(digest[:8])%openCodeGoIdentityProxySIDRange
	return strconv.FormatUint(value, 10)
}

func openCodeGoIdentityProxyReference(secret string, channelID int, identityUID string) string {
	digest := openCodeGoIdentityProxyHMAC(
		secret,
		openCodeGoIdentityProxyReferenceDomain,
		strconv.Itoa(channelID),
		identityUID,
	)
	return fmt.Sprintf("%x", digest[:16])
}

func openCodeGoIdentityProxyCacheKey(
	secret string,
	channelID int,
	identityUID string,
	bucket int64,
	templateURL string,
	country string,
	minutes int,
	policy HTTPTransportPolicy,
) string {
	digest := openCodeGoIdentityProxyHMAC(
		secret,
		openCodeGoIdentityProxyCacheKeyDomain,
		strconv.Itoa(channelID),
		identityUID,
		strconv.FormatInt(bucket, 10),
		templateURL,
		country,
		strconv.Itoa(minutes),
		policy.cacheKeyPart(),
	)
	return fmt.Sprintf("%x", digest)
}

func openCodeGoIdentityProxyBucket(now time.Time, minutes int) int64 {
	return now.UTC().Unix() / int64(minutes*60)
}

func (cache *openCodeGoIdentityProxyClientCache) removeEntryLocked(entry *openCodeGoIdentityProxyCacheEntry) *http.Client {
	if entry == nil {
		return nil
	}
	delete(cache.entries, entry.key)
	if entry.element != nil {
		cache.lru.Remove(entry.element)
		entry.element = nil
	}
	return entry.client
}

func closeOpenCodeGoIdentityProxyClients(clients []*http.Client) {
	for _, client := range clients {
		if client != nil {
			client.CloseIdleConnections()
		}
	}
}

func (cache *openCodeGoIdentityProxyClientCache) getOrCreate(
	now time.Time,
	key string,
	channelID int,
	identityRef string,
	bucket int64,
	generation *OpenCodeGoIdentityProxyGeneration,
	factory func() (*http.Client, error),
) (*http.Client, error) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	cache.generationMutex.Lock()
	if !cache.generationMatchesLocked(channelID, generation) {
		cache.generationMutex.Unlock()
		return nil, ErrOpenCodeGoIdentityProxySelectionStale
	}
	cache.generationMutex.Unlock()

	retired := make([]*http.Client, 0)
	if cache.maxAge > 0 {
		for element := cache.lru.Back(); element != nil; {
			previous := element.Prev()
			entry := element.Value.(*openCodeGoIdentityProxyCacheEntry)
			if now.Sub(entry.createdAt) >= cache.maxAge {
				retired = append(retired, cache.removeEntryLocked(entry))
			}
			element = previous
		}
	}
	latestBucket := int64(-1)
	for _, entry := range cache.entries {
		if entry.channelID == channelID && entry.identityRef == identityRef && entry.bucket > latestBucket {
			latestBucket = entry.bucket
		}
	}
	staleArrival := latestBucket >= 0 && bucket < latestBucket
	if !staleArrival {
		for _, entry := range cache.entries {
			if entry.channelID == channelID && entry.identityRef == identityRef && entry.key != key {
				retired = append(retired, cache.removeEntryLocked(entry))
			}
		}
	}
	closeOpenCodeGoIdentityProxyClients(retired)

	if entry, ok := cache.entries[key]; ok {
		cache.generationMutex.Lock()
		if !cache.generationMatchesLocked(channelID, generation) {
			cache.generationMutex.Unlock()
			return nil, ErrOpenCodeGoIdentityProxySelectionStale
		}
		if staleArrival {
			cache.lru.MoveToBack(entry.element)
		} else {
			cache.lru.MoveToFront(entry.element)
		}
		client := entry.client
		cache.generationMutex.Unlock()
		return client, nil
	}
	client, err := factory()
	if err != nil {
		cache.generationMutex.Lock()
		matches := cache.generationMatchesLocked(channelID, generation)
		cache.generationMutex.Unlock()
		if !matches {
			return nil, ErrOpenCodeGoIdentityProxySelectionStale
		}
		return nil, err
	}
	cache.generationMutex.Lock()
	if !cache.generationMatchesLocked(channelID, generation) {
		cache.generationMutex.Unlock()
		client.CloseIdleConnections()
		return nil, ErrOpenCodeGoIdentityProxySelectionStale
	}
	entry := &openCodeGoIdentityProxyCacheEntry{
		key:         key,
		channelID:   channelID,
		identityRef: identityRef,
		bucket:      bucket,
		createdAt:   now,
		client:      client,
	}
	if staleArrival {
		// A request that began in an older bucket may finish on its old client,
		// but it must never become the current binding after a newer request won
		// the race. Keep it at the eviction end of the bounded cache.
		entry.element = cache.lru.PushBack(entry)
	} else {
		entry.element = cache.lru.PushFront(entry)
	}
	cache.entries[key] = entry

	retired = retired[:0]
	for len(cache.entries) > cache.capacity {
		oldest := cache.lru.Back().Value.(*openCodeGoIdentityProxyCacheEntry)
		retired = append(retired, cache.removeEntryLocked(oldest))
	}
	closeOpenCodeGoIdentityProxyClients(retired)
	cache.generationMutex.Unlock()
	return client, nil
}

func (cache *openCodeGoIdentityProxyClientCache) invalidateChannel(channelID int) {
	cache.generationMutex.Lock()
	cache.channelGeneration[channelID]++
	cache.invalidatingChannels[channelID]++
	cache.generationMutex.Unlock()
	cache.mutex.Lock()
	removed := make([]*http.Client, 0)
	for _, entry := range cache.entries {
		if entry.channelID == channelID {
			removed = append(removed, cache.removeEntryLocked(entry))
		}
	}
	cache.mutex.Unlock()
	cache.generationMutex.Lock()
	cache.invalidatingChannels[channelID]--
	cache.generationMutex.Unlock()
	closeOpenCodeGoIdentityProxyClients(removed)
}

func (cache *openCodeGoIdentityProxyClientCache) invalidateIdentity(channelID int, identityRef string) {
	cache.generationMutex.Lock()
	cache.channelGeneration[channelID]++
	cache.invalidatingChannels[channelID]++
	cache.generationMutex.Unlock()
	cache.mutex.Lock()
	removed := make([]*http.Client, 0)
	for _, entry := range cache.entries {
		if entry.channelID == channelID && (identityRef == "" || entry.identityRef == identityRef) {
			removed = append(removed, cache.removeEntryLocked(entry))
		}
	}
	cache.mutex.Unlock()
	cache.generationMutex.Lock()
	cache.invalidatingChannels[channelID]--
	cache.generationMutex.Unlock()
	closeOpenCodeGoIdentityProxyClients(removed)
}

func (cache *openCodeGoIdentityProxyClientCache) reset() {
	cache.generationMutex.Lock()
	cache.resetCount++
	cache.globalGeneration++
	refreshOpenCodeGoPoolSnapshotGenerationsLocked(cache)
	cache.generationMutex.Unlock()
	cache.mutex.Lock()
	removed := make([]*http.Client, 0, len(cache.entries))
	for _, entry := range cache.entries {
		removed = append(removed, entry.client)
	}
	cache.entries = make(map[string]*openCodeGoIdentityProxyCacheEntry)
	cache.lru.Init()
	cache.mutex.Unlock()
	cache.generationMutex.Lock()
	cache.resetCount--
	cache.generationMutex.Unlock()
	closeOpenCodeGoIdentityProxyClients(removed)
}

func resolveOpenCodeGoIdentityHTTPClientWithGeneration(
	channelID int,
	identityUID string,
	settings dto.ChannelSettings,
	config *dto.OpenCodeGoConfig,
	now time.Time,
	generation *OpenCodeGoIdentityProxyGeneration,
) (*http.Client, error) {
	if config == nil || !config.IdentityProxyEnabled {
		client, err := getOpenCodeGoHTTPClientForSettings(settings)
		if err != nil {
			return nil, err
		}
		openCodeGoIdentityProxyClients.generationMutex.Lock()
		defer openCodeGoIdentityProxyClients.generationMutex.Unlock()
		if !openCodeGoIdentityProxyClients.generationMatchesLocked(channelID, generation) {
			return nil, ErrOpenCodeGoIdentityProxySelectionStale
		}
		return client, nil
	}
	if strings.TrimSpace(identityUID) == "" {
		return nil, errors.New("OpenCode Go identity proxy requires an identity")
	}
	if !common.CryptoSecretExplicitlyConfigured || strings.TrimSpace(common.CryptoSecret) == "" {
		return nil, ErrOpenCodeGoCryptoSecretRequired
	}
	normalizedConfig := *config
	normalizedConfig.NormalizeIdentityProxy()
	if err := normalizedConfig.Validate(); err != nil {
		return nil, errors.New("OpenCode Go identity proxy settings are invalid")
	}
	if err := settings.ValidateHTTPTransport(); err != nil {
		return nil, errors.New("OpenCode Go channel HTTP settings are invalid")
	}
	template, err := dto.ParseOpenCodeGoIdentityProxyTemplate(settings.Proxy)
	if err != nil {
		return nil, errors.New("OpenCode Go identity proxy could not be configured")
	}
	bucket := openCodeGoIdentityProxyBucket(now, normalizedConfig.IdentityProxyRotateMinutes)
	sid := deriveOpenCodeGoIdentityProxySID(common.CryptoSecret, channelID, identityUID, bucket)
	derivedURL, err := template.Rewrite(
		normalizedConfig.IdentityProxyCountry,
		sid,
		normalizedConfig.IdentityProxyRotateMinutes,
	)
	if err != nil {
		return nil, errors.New("OpenCode Go identity proxy could not be configured")
	}
	parsedURL, _, err := common.ParseProxyURLRuntime(derivedURL)
	if err != nil || parsedURL == nil {
		return nil, errors.New("OpenCode Go identity proxy could not be configured")
	}
	policy := NormalizeHTTPTransportPolicy(settings)
	identityRef := openCodeGoIdentityProxyReference(common.CryptoSecret, channelID, identityUID)
	cacheKey := openCodeGoIdentityProxyCacheKey(
		common.CryptoSecret,
		channelID,
		identityUID,
		bucket,
		parsedURL.String(),
		normalizedConfig.IdentityProxyCountry,
		normalizedConfig.IdentityProxyRotateMinutes,
		policy,
	)
	return openCodeGoIdentityProxyClients.getOrCreate(now, cacheKey, channelID, identityRef, bucket, generation, func() (*http.Client, error) {
		client, err := newHTTPClientFromPolicy(policy, parsedURL, nil)
		if err != nil {
			return nil, errors.New("OpenCode Go identity proxy could not be configured")
		}
		return wrapOpenCodeGoProxyHTTPClient(client, settings.Proxy, parsedURL, identityUID, sid), nil
	})
}

func resolveOpenCodeGoIdentityHTTPClient(
	channelID int,
	identityUID string,
	settings dto.ChannelSettings,
	config *dto.OpenCodeGoConfig,
	now time.Time,
) (*http.Client, error) {
	return resolveOpenCodeGoIdentityHTTPClientWithGeneration(
		channelID,
		identityUID,
		settings,
		config,
		now,
		nil,
	)
}

func InvalidateOpenCodeGoIdentityProxyChannel(channelID int) {
	openCodeGoIdentityProxyClients.invalidateChannel(channelID)
}

func InvalidateOpenCodeGoIdentityProxyIdentity(channelID int, identityUID string) {
	if strings.TrimSpace(identityUID) == "" {
		return
	}
	identityRef := ""
	if strings.TrimSpace(common.CryptoSecret) != "" {
		identityRef = openCodeGoIdentityProxyReference(common.CryptoSecret, channelID, identityUID)
	}
	openCodeGoIdentityProxyClients.invalidateIdentity(channelID, identityRef)
}

func ResetOpenCodeGoIdentityProxyClientCache() {
	openCodeGoIdentityProxyClients.reset()
}
