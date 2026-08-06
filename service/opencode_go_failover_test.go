package service

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaydto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func testOpenCodeGoFailoverPolicy() OpenCodeGoFailoverPolicy {
	return OpenCodeGoFailoverPolicy{
		Enabled:          true,
		FailureThreshold: 2,
		FailureWindow:    30 * time.Second,
		MaxBackups:       1,
		LeaseDuration:    30 * time.Minute,
	}
}

func testOpenCodeGoFailoverAttempt(key string, generation int64, selected string) *OpenCodeGoFailoverAttempt {
	return testOpenCodeGoFailoverAttemptWithIncarnation(key, generation, selected, "test-incarnation")
}

func testOpenCodeGoFailoverAttemptWithIncarnation(key string, generation int64, selected string, incarnation string) *OpenCodeGoFailoverAttempt {
	return &OpenCodeGoFailoverAttempt{
		stateKey:                 key,
		expectedGeneration:       generation,
		incarnation:              incarnation,
		selectedWorkspaceUID:     selected,
		canonicalWorkspaceUID:    "workspace-primary",
		preferredBackupWorkspace: "workspace-backup",
		policy:                   testOpenCodeGoFailoverPolicy(),
		observeSuccess:           true,
	}
}

func useOpenCodeGoFailoverMemoryForTest(t *testing.T, capacity int) {
	t.Helper()
	previousEnabled := common.RedisEnabled
	previousRedis := common.RDB
	previousMemory := openCodeGoFailoverMemoryState
	common.RedisEnabled = false
	common.RDB = nil
	openCodeGoFailoverMemoryState = newOpenCodeGoFailoverMemoryStore(capacity)
	t.Cleanup(func() {
		common.RedisEnabled = previousEnabled
		common.RDB = previousRedis
		openCodeGoFailoverMemoryState = previousMemory
	})
}

func useOpenCodeGoFailoverRedisForTest(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: 0})
	previousEnabled := common.RedisEnabled
	previousRedis := common.RDB
	common.RedisEnabled = true
	common.RDB = client
	t.Cleanup(func() {
		_ = client.Close()
		common.RedisEnabled = previousEnabled
		common.RDB = previousRedis
	})
	return server
}

func TestResolveOpenCodeGoFailoverPolicyDefaultsAndOverrides(t *testing.T) {
	defaults := ResolveOpenCodeGoFailoverPolicy(nil)
	assert.False(t, defaults.Enabled)
	assert.Equal(t, 2, defaults.FailureThreshold)
	assert.Equal(t, 30*time.Second, defaults.FailureWindow)
	assert.Equal(t, 1, defaults.MaxBackups)
	assert.Equal(t, 30*time.Minute, defaults.LeaseDuration)

	overrides := ResolveOpenCodeGoFailoverPolicy(&relaydto.OpenCodeGoConfig{
		GenericFailoverEnabled:       true,
		GenericFailoverThreshold:     3,
		GenericFailoverWindowSeconds: 45,
		GenericFailoverMaxBackups:    1,
		GenericFailoverLeaseSeconds:  600,
	})
	assert.True(t, overrides.Enabled)
	assert.Equal(t, 3, overrides.FailureThreshold)
	assert.Equal(t, 45*time.Second, overrides.FailureWindow)
	assert.Equal(t, 10*time.Minute, overrides.LeaseDuration)
}

func TestOpenCodeGoFailoverMemoryStateMachine(t *testing.T) {
	useOpenCodeGoFailoverMemoryForTest(t, 100)
	now := time.Unix(2_000_000_000, 0)
	key := "test-state"

	first, err := ObserveOpenCodeGoFailoverFailure(testOpenCodeGoFailoverAttempt(key, 0, "workspace-primary"), now)
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionSuspect, first.Action)
	assert.Equal(t, 1, first.FailureCount)

	state, exists, err := loadOpenCodeGoFailoverState(key, now)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, int64(1), state.Generation)
	assert.Empty(t, state.ActiveWorkspaceUID)

	second, err := ObserveOpenCodeGoFailoverFailure(testOpenCodeGoFailoverAttempt(key, state.Generation, "workspace-primary"), now.Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionPromoted, second.Action)
	state, exists, err = loadOpenCodeGoFailoverState(key, now.Add(time.Second))
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, "workspace-backup", state.ActiveWorkspaceUID)
	assert.True(t, state.BackupUsed)

	latePrimary, err := ObserveOpenCodeGoFailoverSuccess(testOpenCodeGoFailoverAttempt(key, 1, "workspace-primary"), now.Add(2*time.Second))
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionStale, latePrimary.Action)

	backupSuccess, err := ObserveOpenCodeGoFailoverSuccess(testOpenCodeGoFailoverAttempt(key, state.Generation, "workspace-backup"), now.Add(3*time.Second))
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionLeaseRefreshed, backupSuccess.Action)
	assert.Equal(t, now.Add(3*time.Second+30*time.Minute), backupSuccess.LeaseExpiresAt)

	state, exists, err = loadOpenCodeGoFailoverState(key, now.Add(3*time.Second))
	require.NoError(t, err)
	require.True(t, exists)
	backupFailure, err := ObserveOpenCodeGoFailoverFailure(testOpenCodeGoFailoverAttempt(key, state.Generation, "workspace-backup"), now.Add(4*time.Second))
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionBackupExhausted, backupFailure.Action)
	state, exists, err = loadOpenCodeGoFailoverState(key, now.Add(4*time.Second))
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, "workspace-backup", state.ActiveWorkspaceUID)
	assert.True(t, state.BackupExhausted)
	assert.True(t, state.BackupUsed)

	_, exists, err = loadOpenCodeGoFailoverState(key, backupSuccess.LeaseExpiresAt.Add(time.Millisecond))
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestOpenCodeGoFailoverMaxBackupsZeroMatchesMemoryAndRedis(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T)
	}{
		{name: "memory", setup: func(t *testing.T) { useOpenCodeGoFailoverMemoryForTest(t, 100) }},
		{name: "redis", setup: func(t *testing.T) { useOpenCodeGoFailoverRedisForTest(t) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.setup(t)
			now := time.Unix(2_000_000_000, 0)
			key := "max-backups-zero-" + test.name

			firstAttempt := testOpenCodeGoFailoverAttempt(key, 0, "workspace-primary")
			firstAttempt.policy.MaxBackups = 0
			first, err := ObserveOpenCodeGoFailoverFailure(firstAttempt, now)
			require.NoError(t, err)
			require.Equal(t, OpenCodeGoFailoverActionSuspect, first.Action)

			state, exists, err := loadOpenCodeGoFailoverState(key, now)
			require.NoError(t, err)
			require.True(t, exists)
			secondAttempt := testOpenCodeGoFailoverAttemptWithIncarnation(
				key,
				state.Generation,
				"workspace-primary",
				state.Incarnation,
			)
			secondAttempt.policy.MaxBackups = 0
			second, err := ObserveOpenCodeGoFailoverFailure(secondAttempt, now.Add(time.Second))
			require.NoError(t, err)
			assert.Equal(t, OpenCodeGoFailoverActionSuspect, second.Action)
			assert.Equal(t, 2, second.FailureCount)
			assert.True(t, second.LeaseExpiresAt.IsZero())
		})
	}
}

func TestOpenCodeGoFailoverCanonicalSuccessAndWindowExpiry(t *testing.T) {
	useOpenCodeGoFailoverMemoryForTest(t, 100)
	now := time.Unix(2_000_000_000, 0)
	key := "test-clear"
	_, err := ObserveOpenCodeGoFailoverFailure(testOpenCodeGoFailoverAttempt(key, 0, "workspace-primary"), now)
	require.NoError(t, err)
	state, exists, err := loadOpenCodeGoFailoverState(key, now)
	require.NoError(t, err)
	require.True(t, exists)
	cleared, err := ObserveOpenCodeGoFailoverSuccess(testOpenCodeGoFailoverAttempt(key, state.Generation, "workspace-primary"), now.Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionCleared, cleared.Action)
	state, exists, err = loadOpenCodeGoFailoverState(key, now.Add(time.Second))
	require.NoError(t, err)
	require.True(t, exists)
	assert.Empty(t, state.PrimaryWorkspaceUID)
	assert.Empty(t, state.ActiveWorkspaceUID)
	assert.Zero(t, state.FailureCount)

	first, err := ObserveOpenCodeGoFailoverFailure(testOpenCodeGoFailoverAttempt(key, state.Generation, "workspace-primary"), now.Add(2*time.Second))
	require.NoError(t, err)
	assert.Equal(t, 1, first.FailureCount)
	state, exists, err = loadOpenCodeGoFailoverState(key, now.Add(2*time.Second))
	require.NoError(t, err)
	require.True(t, exists)
	reset, err := ObserveOpenCodeGoFailoverFailure(
		testOpenCodeGoFailoverAttemptWithIncarnation(key, 0, "workspace-primary", "post-expiry-incarnation"),
		now.Add(33*time.Second),
	)
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionSuspect, reset.Action)
	assert.Equal(t, 1, reset.FailureCount)
}

func TestOpenCodeGoFailoverMemoryStoreIsBoundedLRU(t *testing.T) {
	useOpenCodeGoFailoverMemoryForTest(t, 2)
	now := time.Unix(2_000_000_000, 0)
	for index := 0; index < 3; index++ {
		attempt := testOpenCodeGoFailoverAttempt(fmt.Sprintf("key-%d", index), 0, "workspace-primary")
		_, err := ObserveOpenCodeGoFailoverFailure(attempt, now.Add(time.Duration(index)*time.Second))
		require.NoError(t, err)
	}
	assert.Len(t, openCodeGoFailoverMemoryState.entries, 2)
	_, firstExists, err := loadOpenCodeGoFailoverState("key-0", now.Add(3*time.Second))
	require.NoError(t, err)
	assert.False(t, firstExists)
}

func TestOpenCodeGoFailoverMemoryPurgesExpiredMRUBeforeEviction(t *testing.T) {
	useOpenCodeGoFailoverMemoryForTest(t, 2)
	now := time.Unix(2_000_000_000, 0)

	_, err := ObserveOpenCodeGoFailoverFailure(
		testOpenCodeGoFailoverAttemptWithIncarnation("live-lease", 0, "workspace-primary", "live-incarnation"),
		now,
	)
	require.NoError(t, err)
	liveState, exists, err := loadOpenCodeGoFailoverState("live-lease", now)
	require.NoError(t, err)
	require.True(t, exists)
	_, err = ObserveOpenCodeGoFailoverFailure(
		testOpenCodeGoFailoverAttemptWithIncarnation("live-lease", liveState.Generation, "workspace-primary", liveState.Incarnation),
		now.Add(time.Second),
	)
	require.NoError(t, err)

	_, err = ObserveOpenCodeGoFailoverFailure(
		testOpenCodeGoFailoverAttemptWithIncarnation("expired-mru", 0, "workspace-primary", "expired-incarnation"),
		now.Add(2*time.Second),
	)
	require.NoError(t, err)
	_, expiredExists, err := loadOpenCodeGoFailoverState("expired-mru", now.Add(2*time.Second))
	require.NoError(t, err)
	require.True(t, expiredExists)

	_, err = ObserveOpenCodeGoFailoverFailure(
		testOpenCodeGoFailoverAttemptWithIncarnation("new-state", 0, "workspace-primary", "new-incarnation"),
		now.Add(33*time.Second),
	)
	require.NoError(t, err)

	_, liveExists, err := loadOpenCodeGoFailoverState("live-lease", now.Add(33*time.Second))
	require.NoError(t, err)
	assert.True(t, liveExists)
	_, expiredExists, err = loadOpenCodeGoFailoverState("expired-mru", now.Add(33*time.Second))
	require.NoError(t, err)
	assert.False(t, expiredExists)
	assert.Len(t, openCodeGoFailoverMemoryState.entries, 2)
	assert.Len(t, openCodeGoFailoverMemoryState.expiry, 2)
}

func TestOpenCodeGoFailoverMemoryRejectsOutcomeAfterEvictionAndRecreation(t *testing.T) {
	useOpenCodeGoFailoverMemoryForTest(t, 1)
	now := time.Unix(2_000_000_000, 0)

	_, err := ObserveOpenCodeGoFailoverFailure(
		testOpenCodeGoFailoverAttemptWithIncarnation("recreated", 0, "workspace-primary", "old-incarnation"),
		now,
	)
	require.NoError(t, err)
	oldState, exists, err := loadOpenCodeGoFailoverState("recreated", now)
	require.NoError(t, err)
	require.True(t, exists)
	oldOutcome := testOpenCodeGoFailoverAttemptWithIncarnation(
		"recreated",
		oldState.Generation,
		"workspace-primary",
		oldState.Incarnation,
	)

	_, err = ObserveOpenCodeGoFailoverFailure(
		testOpenCodeGoFailoverAttemptWithIncarnation("evictor", 0, "workspace-primary", "evictor-incarnation"),
		now.Add(time.Second),
	)
	require.NoError(t, err)
	_, err = ObserveOpenCodeGoFailoverFailure(
		testOpenCodeGoFailoverAttemptWithIncarnation("recreated", 0, "workspace-primary", "new-incarnation"),
		now.Add(2*time.Second),
	)
	require.NoError(t, err)

	stale, err := ObserveOpenCodeGoFailoverSuccess(oldOutcome, now.Add(3*time.Second))
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionStale, stale.Action)
	newState, exists, err := loadOpenCodeGoFailoverState("recreated", now.Add(3*time.Second))
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, "new-incarnation", newState.Incarnation)
	assert.Equal(t, 1, newState.FailureCount)
}

func TestOpenCodeGoFailoverRedisIsAtomicAndVisible(t *testing.T) {
	useOpenCodeGoFailoverRedisForTest(t)
	now := time.Unix(2_000_000_000, 0)
	key := "redis-state"
	_, err := ObserveOpenCodeGoFailoverFailure(testOpenCodeGoFailoverAttempt(key, 0, "workspace-primary"), now)
	require.NoError(t, err)
	state, exists, err := loadOpenCodeGoFailoverState(key, now)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, int64(1), state.Generation)

	var promoted atomic.Int64
	var stale atomic.Int64
	var wait sync.WaitGroup
	for index := 0; index < 24; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, observeErr := ObserveOpenCodeGoFailoverFailure(
				testOpenCodeGoFailoverAttempt(key, state.Generation, "workspace-primary"),
				now.Add(time.Second),
			)
			assert.NoError(t, observeErr)
			switch result.Action {
			case OpenCodeGoFailoverActionPromoted:
				promoted.Add(1)
			case OpenCodeGoFailoverActionStale:
				stale.Add(1)
			}
		}()
	}
	wait.Wait()
	assert.Equal(t, int64(1), promoted.Load())
	assert.Equal(t, int64(23), stale.Load())
	state, exists, err = loadOpenCodeGoFailoverState(key, now.Add(time.Second))
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, "workspace-backup", state.ActiveWorkspaceUID)
	assert.Equal(t, int64(2), state.Generation)
}

func TestOpenCodeGoFailoverRedisNaturalExpiryRecreatesStateWithoutABA(t *testing.T) {
	server := useOpenCodeGoFailoverRedisForTest(t)
	now := time.Now().Truncate(time.Millisecond)
	key := openCodeGoFailoverRedisPrefix + ":state:natural-expiry"

	_, err := ObserveOpenCodeGoFailoverFailure(
		testOpenCodeGoFailoverAttemptWithIncarnation(key, 0, "workspace-primary", "old-incarnation"),
		now,
	)
	require.NoError(t, err)
	oldState, exists, err := loadOpenCodeGoFailoverState(key, now)
	require.NoError(t, err)
	require.True(t, exists)
	oldOutcome := testOpenCodeGoFailoverAttemptWithIncarnation(
		key,
		oldState.Generation,
		"workspace-primary",
		oldState.Incarnation,
	)

	server.FastForward(31 * time.Second)
	postExpiry := now.Add(31 * time.Second)
	_, exists, err = loadOpenCodeGoFailoverState(key, postExpiry)
	require.NoError(t, err)
	assert.False(t, exists)

	first, err := ObserveOpenCodeGoFailoverFailure(
		testOpenCodeGoFailoverAttemptWithIncarnation(key, 0, "workspace-primary", "new-incarnation"),
		postExpiry,
	)
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionSuspect, first.Action)
	assert.Equal(t, 1, first.FailureCount)
	newState, exists, err := loadOpenCodeGoFailoverState(key, postExpiry)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, "new-incarnation", newState.Incarnation)

	stale, err := ObserveOpenCodeGoFailoverSuccess(oldOutcome, postExpiry.Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionStale, stale.Action)
	second, err := ObserveOpenCodeGoFailoverFailure(
		testOpenCodeGoFailoverAttemptWithIncarnation(key, newState.Generation, "workspace-primary", newState.Incarnation),
		postExpiry.Add(2*time.Second),
	)
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionPromoted, second.Action)
}

func TestOpenCodeGoFailoverRedisFailureReturnsWithinShortTimeout(t *testing.T) {
	client := redis.NewClient(&redis.Options{
		Addr:       "blocked.test:6379",
		MaxRetries: -1,
		Dialer: func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	previousEnabled := common.RedisEnabled
	previousRedis := common.RDB
	common.RedisEnabled = true
	common.RDB = client
	t.Cleanup(func() {
		_ = client.Close()
		common.RedisEnabled = previousEnabled
		common.RDB = previousRedis
	})

	started := time.Now()
	_, _, err := loadOpenCodeGoFailoverState("unreachable", time.Now())
	elapsed := time.Since(started)
	require.Error(t, err)
	assert.GreaterOrEqual(t, elapsed, 30*time.Millisecond)
	assert.Less(t, elapsed, 500*time.Millisecond)
}

func TestOpenCodeGoFailoverSelectorFailsOpenWhenRedisTimesOut(t *testing.T) {
	useOpenCodeGoFailoverMemoryForTest(t, 100)
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "two", "workspace-two", "wrk_BETA2", []string{"model-a"})
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))

	canonical, err := SelectOpenCodeGoWorkspaceWithAffinity(channel.Id, "model-a", "redis-timeout-session")
	require.NoError(t, err)

	client := redis.NewClient(&redis.Options{
		Addr:       "blocked.test:6379",
		MaxRetries: -1,
		Dialer: func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	previousEnabled := common.RedisEnabled
	previousRedis := common.RDB
	common.RedisEnabled = true
	common.RDB = client
	t.Cleanup(func() {
		_ = client.Close()
		common.RedisEnabled = previousEnabled
		common.RDB = previousRedis
	})

	started := time.Now()
	selection, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", OpenCodeGoPoolSelectOptions{
		AffinityKey: "redis-timeout-session",
		Protocol:    relaydto.OpenCodeGoProtocolResponses,
		Failover:    testOpenCodeGoFailoverPolicy(),
	})
	elapsed := time.Since(started)
	require.NoError(t, err)
	assert.Equal(t, canonical.WorkspaceUID, selection.WorkspaceUID)
	assert.False(t, selection.FailoverActive)
	assert.NotNil(t, selection.FailoverAttempt)
	assert.GreaterOrEqual(t, elapsed, 30*time.Millisecond)
	assert.Less(t, elapsed, 500*time.Millisecond)
}

func TestOpenCodeGoFailoverSelectionPromotesOnlyOnLaterRequest(t *testing.T) {
	useOpenCodeGoFailoverMemoryForTest(t, 100)
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "two", "workspace-two", "wrk_BETA2", []string{"model-a"})
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))

	now := time.Unix(2_000_000_000, 0)
	previousNow := openCodeGoFailoverNow
	openCodeGoFailoverNow = func() time.Time { return now }
	t.Cleanup(func() { openCodeGoFailoverNow = previousNow })
	options := OpenCodeGoPoolSelectOptions{
		AffinityKey: "stable-session",
		Protocol:    relaydto.OpenCodeGoProtocolResponses,
		Failover:    testOpenCodeGoFailoverPolicy(),
	}

	first, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	primary := first.WorkspaceUID
	backup := first.FailoverAttempt.PreferredBackupWorkspaceUID()
	require.NotEqual(t, primary, backup)
	result, err := ObserveOpenCodeGoFailoverFailure(first.FailoverAttempt, now)
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionSuspect, result.Action)

	second, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	assert.Equal(t, primary, second.WorkspaceUID)
	result, err = ObserveOpenCodeGoFailoverFailure(second.FailoverAttempt, now.Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionPromoted, result.Action)

	third, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	assert.Equal(t, backup, third.WorkspaceUID)
	assert.True(t, third.FailoverActive)
	assert.Equal(t, 1, third.CandidateRank)

	withoutAffinity, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", OpenCodeGoPoolSelectOptions{
		Protocol: relaydto.OpenCodeGoProtocolResponses,
		Failover: testOpenCodeGoFailoverPolicy(),
	})
	require.NoError(t, err)
	assert.Nil(t, withoutAffinity.FailoverAttempt)

	stateful, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", OpenCodeGoPoolSelectOptions{
		AffinityKey: "stateful-session",
		Protocol:    relaydto.OpenCodeGoProtocolResponses,
		Stateful:    true,
		Failover:    testOpenCodeGoFailoverPolicy(),
	})
	require.NoError(t, err)
	require.NotNil(t, stateful.FailoverAttempt)
	assert.True(t, stateful.FailoverAttempt.suppressFailure)
	assert.False(t, stateful.FailoverAttempt.observeSuccess)
}

func TestOpenCodeGoStatefulRequestKeepsAndRefreshesSoleActiveLease(t *testing.T) {
	useOpenCodeGoFailoverMemoryForTest(t, 100)
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "two", "workspace-two", "wrk_BETA2", []string{"model-a"})
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))

	current := time.Unix(2_000_000_000, 0)
	previousNow := openCodeGoFailoverNow
	openCodeGoFailoverNow = func() time.Time { return current }
	t.Cleanup(func() { openCodeGoFailoverNow = previousNow })
	options := OpenCodeGoPoolSelectOptions{
		AffinityKey: "stateful-promoted-session",
		Protocol:    relaydto.OpenCodeGoProtocolResponses,
		Failover:    testOpenCodeGoFailoverPolicy(),
	}

	first, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	_, err = ObserveOpenCodeGoFailoverFailure(first.FailoverAttempt, current)
	require.NoError(t, err)
	current = current.Add(time.Second)
	second, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	promoted, err := ObserveOpenCodeGoFailoverFailure(second.FailoverAttempt, current)
	require.NoError(t, err)
	require.Equal(t, OpenCodeGoFailoverActionPromoted, promoted.Action)
	backupUID := second.FailoverAttempt.PreferredBackupWorkspaceUID()

	stateValue, exists := openCodeGoPoolChannels.Load(channel.Id)
	require.True(t, exists)
	channelState := stateValue.(*openCodeGoPoolChannelState)
	originalSnapshot := channelState.snapshot.Load()
	require.NotNil(t, originalSnapshot)
	filtered := *originalSnapshot
	filtered.byModel = make(map[string][]openCodeGoPoolCandidate, len(originalSnapshot.byModel))
	for modelID, candidates := range originalSnapshot.byModel {
		for _, candidate := range candidates {
			if candidate.workspaceUID == backupUID {
				filtered.byModel[modelID] = append(filtered.byModel[modelID], candidate)
			}
		}
	}
	channelState.snapshot.Store(&filtered)
	t.Cleanup(func() { channelState.snapshot.Store(originalSnapshot) })

	current = promoted.LeaseExpiresAt.Add(-time.Second)
	statefulOptions := options
	statefulOptions.Stateful = true
	stateful, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", statefulOptions)
	require.NoError(t, err)
	assert.Equal(t, backupUID, stateful.WorkspaceUID)
	assert.True(t, stateful.FailoverActive)
	require.NotNil(t, stateful.FailoverAttempt)
	assert.True(t, stateful.FailoverAttempt.suppressFailure)
	assert.True(t, stateful.FailoverAttempt.observeSuccess)

	suppressed, err := ObserveOpenCodeGoFailoverFailure(stateful.FailoverAttempt, current)
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionNone, suppressed.Action)
	refreshed, err := ObserveOpenCodeGoFailoverSuccess(stateful.FailoverAttempt, current)
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionLeaseRefreshed, refreshed.Action)
	assert.Equal(t, current.Add(30*time.Minute), refreshed.LeaseExpiresAt)
	assert.True(t, refreshed.LeaseExpiresAt.After(promoted.LeaseExpiresAt))
}

func TestOpenCodeGoSoleCandidateSuccessClearsSuspicion(t *testing.T) {
	useOpenCodeGoFailoverMemoryForTest(t, 100)
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "two", "workspace-two", "wrk_BETA2", []string{"model-a"})
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))

	current := time.Unix(2_000_000_000, 0)
	previousNow := openCodeGoFailoverNow
	openCodeGoFailoverNow = func() time.Time { return current }
	t.Cleanup(func() { openCodeGoFailoverNow = previousNow })
	options := OpenCodeGoPoolSelectOptions{
		AffinityKey: "sole-candidate-session",
		Protocol:    relaydto.OpenCodeGoProtocolResponses,
		Failover:    testOpenCodeGoFailoverPolicy(),
	}

	first, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	firstFailure, err := ObserveOpenCodeGoFailoverFailure(first.FailoverAttempt, current)
	require.NoError(t, err)
	require.Equal(t, OpenCodeGoFailoverActionSuspect, firstFailure.Action)

	stateValue, exists := openCodeGoPoolChannels.Load(channel.Id)
	require.True(t, exists)
	channelState := stateValue.(*openCodeGoPoolChannelState)
	originalSnapshot := channelState.snapshot.Load()
	require.NotNil(t, originalSnapshot)
	filtered := *originalSnapshot
	filtered.byModel = make(map[string][]openCodeGoPoolCandidate, len(originalSnapshot.byModel))
	for modelID, candidates := range originalSnapshot.byModel {
		for _, candidate := range candidates {
			if candidate.workspaceUID == first.WorkspaceUID {
				filtered.byModel[modelID] = append(filtered.byModel[modelID], candidate)
			}
		}
	}
	channelState.snapshot.Store(&filtered)

	current = current.Add(time.Second)
	sole, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	require.NotNil(t, sole.FailoverAttempt)
	cleared, err := ObserveOpenCodeGoFailoverSuccess(sole.FailoverAttempt, current)
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionCleared, cleared.Action)

	channelState.snapshot.Store(originalSnapshot)
	current = current.Add(time.Second)
	restored, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	afterRestore, err := ObserveOpenCodeGoFailoverFailure(restored.FailoverAttempt, current)
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionSuspect, afterRestore.Action)
	assert.Equal(t, 1, afterRestore.FailureCount)
}

func TestOpenCodeGoFailoverSelectionHandlesLeasedCandidateChurn(t *testing.T) {
	useOpenCodeGoFailoverMemoryForTest(t, 100)
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "two", "workspace-two", "wrk_BETA2", []string{"model-a"})
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "three", "workspace-three", "wrk_GAMMA3", []string{"model-a"})
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))

	now := time.Unix(2_000_000_000, 0)
	previousNow := openCodeGoFailoverNow
	openCodeGoFailoverNow = func() time.Time { return now }
	t.Cleanup(func() { openCodeGoFailoverNow = previousNow })
	options := OpenCodeGoPoolSelectOptions{
		AffinityKey: "candidate-churn-session",
		Protocol:    relaydto.OpenCodeGoProtocolResponses,
		Failover:    testOpenCodeGoFailoverPolicy(),
	}

	first, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	_, err = ObserveOpenCodeGoFailoverFailure(first.FailoverAttempt, now)
	require.NoError(t, err)
	second, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	_, err = ObserveOpenCodeGoFailoverFailure(second.FailoverAttempt, now.Add(time.Second))
	require.NoError(t, err)
	promoted, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	require.True(t, promoted.FailoverActive)
	leasedWorkspaceUID := promoted.WorkspaceUID

	stateValue, exists := openCodeGoPoolChannels.Load(channel.Id)
	require.True(t, exists)
	channelState := stateValue.(*openCodeGoPoolChannelState)
	originalSnapshot := channelState.snapshot.Load()
	require.NotNil(t, originalSnapshot)
	filtered := *originalSnapshot
	filtered.byModel = make(map[string][]openCodeGoPoolCandidate, len(originalSnapshot.byModel))
	for modelID, candidates := range originalSnapshot.byModel {
		for _, candidate := range candidates {
			if candidate.workspaceUID != leasedWorkspaceUID {
				filtered.byModel[modelID] = append(filtered.byModel[modelID], candidate)
			}
		}
	}
	channelState.snapshot.Store(&filtered)

	statefulOptions := options
	statefulOptions.Stateful = true
	_, err = SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", statefulOptions)
	assert.ErrorIs(t, err, ErrOpenCodeGoStatefulResponsesWorkspaceUnavailable)

	withoutLeasedCandidate, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	assert.NotEqual(t, leasedWorkspaceUID, withoutLeasedCandidate.WorkspaceUID)
	assert.False(t, withoutLeasedCandidate.FailoverActive)
	require.NotNil(t, withoutLeasedCandidate.FailoverAttempt)
	assert.NotEqual(t, leasedWorkspaceUID, withoutLeasedCandidate.FailoverAttempt.PreferredBackupWorkspaceUID())
	fallbackSuccess, err := ObserveOpenCodeGoFailoverSuccess(withoutLeasedCandidate.FailoverAttempt, now.Add(2*time.Second))
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionNone, fallbackSuccess.Action)

	channelState.snapshot.Store(originalSnapshot)
	restored, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	assert.Equal(t, first.WorkspaceUID, restored.WorkspaceUID)
	assert.NotEqual(t, leasedWorkspaceUID, restored.WorkspaceUID)
	assert.False(t, restored.FailoverActive)
}

func TestOpenCodeGoFailoverEnabledSelectionPerformsZeroDatabaseQueries(t *testing.T) {
	useOpenCodeGoFailoverMemoryForTest(t, 100)
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "two", "workspace-two", "wrk_BETA2", []string{"model-a"})
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))

	var queryCount atomic.Int64
	callbackName := "opencode_go_test_count_failover_selector_queries"
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callbackName, func(_ *gorm.DB) {
		queryCount.Add(1)
	}))
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callbackName) })

	for index := 0; index < 50; index++ {
		selection, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", OpenCodeGoPoolSelectOptions{
			AffinityKey: fmt.Sprintf("zero-query-session-%d", index),
			Protocol:    relaydto.OpenCodeGoProtocolResponses,
			Failover:    testOpenCodeGoFailoverPolicy(),
		})
		require.NoError(t, err)
		require.NotNil(t, selection.FailoverAttempt)
	}
	assert.Zero(t, queryCount.Load())
}

func TestPreferredOpenCodeGoFailoverBackupUsesAnotherIdentity(t *testing.T) {
	ranked := []openCodeGoRankedCandidate{
		{candidate: openCodeGoPoolCandidate{workspaceUID: "primary", identityUID: "identity-a"}, score: 30},
		{candidate: openCodeGoPoolCandidate{workspaceUID: "sibling", identityUID: "identity-a"}, score: 20},
		{candidate: openCodeGoPoolCandidate{workspaceUID: "other", identityUID: "identity-b"}, score: 10},
	}
	assert.Equal(t, "other", preferredOpenCodeGoFailoverBackup(ranked).workspaceUID)
}

func TestOpenCodeGoFailoverKeyContainsNoRawAffinity(t *testing.T) {
	previousSecret := common.CryptoSecret
	common.CryptoSecret = "test-only-failover-key"
	t.Cleanup(func() { common.CryptoSecret = previousSecret })
	key := openCodeGoFailoverKey(12, "kimi-k3", "responses", "customer-session-secret")
	assert.NotContains(t, key, "customer-session-secret")
	assert.NotContains(t, key, "kimi-k3")
	assert.Contains(t, key, openCodeGoFailoverRedisPrefix+":state:")
}

func TestOpenCodeGoFailoverRedisIndexIsBounded(t *testing.T) {
	useOpenCodeGoFailoverRedisForTest(t)
	// Exercise the same Redis script with a tiny index cap so the production
	// 100k bound does not require creating a large fixture.
	policy := testOpenCodeGoFailoverPolicy()
	now := time.Unix(2_000_000_000, 0)
	for index := 0; index < 3; index++ {
		attempt := &OpenCodeGoFailoverAttempt{
			stateKey:                 fmt.Sprintf("bounded-%d", index),
			selectedWorkspaceUID:     "workspace-primary",
			canonicalWorkspaceUID:    "workspace-primary",
			preferredBackupWorkspace: "workspace-backup",
			policy:                   policy,
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := openCodeGoFailoverScript.Run(
			ctx,
			common.RDB,
			[]string{attempt.stateKey, openCodeGoFailoverRedisPrefix + ":bounded-index"},
			openCodeGoFailoverStateVersion,
			"failure",
			now.Add(time.Duration(index)*time.Second).UnixMilli(),
			0,
			fmt.Sprintf("bounded-incarnation-%d", index),
			attempt.selectedWorkspaceUID,
			attempt.canonicalWorkspaceUID,
			attempt.preferredBackupWorkspace,
			policy.FailureThreshold,
			policy.FailureWindow.Milliseconds(),
			policy.LeaseDuration.Milliseconds(),
			openCodeGoFailoverRetention(policy).Milliseconds(),
			policy.MaxBackups,
			2,
		).Result()
		cancel()
		require.NoError(t, err)
	}
	size, err := common.RDB.ZCard(context.Background(), openCodeGoFailoverRedisPrefix+":bounded-index").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(2), size)
}

func TestOpenCodeGoFailoverMemoryExpiryHeapPreservesLiveLease(t *testing.T) {
	useOpenCodeGoFailoverMemoryForTest(t, 2)
	start := time.Unix(2_000_000_000, 0)

	short := testOpenCodeGoFailoverAttempt("short-lived", 0, "workspace-primary")
	_, err := ObserveOpenCodeGoFailoverFailure(short, start)
	require.NoError(t, err)

	long := testOpenCodeGoFailoverAttempt("live-lease", 0, "workspace-primary")
	_, err = ObserveOpenCodeGoFailoverFailure(long, start)
	require.NoError(t, err)
	state, exists, err := loadOpenCodeGoFailoverState(long.stateKey, start)
	require.NoError(t, err)
	require.True(t, exists)
	_, err = ObserveOpenCodeGoFailoverFailure(
		testOpenCodeGoFailoverAttemptWithIncarnation(long.stateKey, state.Generation, long.selectedWorkspaceUID, state.Incarnation),
		start.Add(time.Second),
	)
	require.NoError(t, err)

	// Touch the short entry so it is the LRU victim; the expiry heap must still
	// remove it before capacity eviction can remove the active long lease.
	_, _, err = loadOpenCodeGoFailoverState(short.stateKey, start.Add(2*time.Second))
	require.NoError(t, err)
	insert := testOpenCodeGoFailoverAttempt("new-entry", 0, "workspace-primary")
	_, err = ObserveOpenCodeGoFailoverFailure(insert, start.Add(31*time.Second))
	require.NoError(t, err)

	_, shortExists, err := loadOpenCodeGoFailoverState(short.stateKey, start.Add(31*time.Second))
	require.NoError(t, err)
	assert.False(t, shortExists)
	_, leaseExists, err := loadOpenCodeGoFailoverState(long.stateKey, start.Add(31*time.Second))
	require.NoError(t, err)
	assert.True(t, leaseExists)
}

func TestOpenCodeGoFailoverMemoryOldGenerationStaysStaleAfterClear(t *testing.T) {
	useOpenCodeGoFailoverMemoryForTest(t, 10)
	start := time.Unix(2_000_000_000, 0)
	old := testOpenCodeGoFailoverAttemptWithIncarnation("aba-memory", 0, "workspace-primary", "old-incarnation")
	_, err := ObserveOpenCodeGoFailoverFailure(old, start)
	require.NoError(t, err)
	state, exists, err := loadOpenCodeGoFailoverState(old.stateKey, start)
	require.NoError(t, err)
	require.True(t, exists)
	_, err = ObserveOpenCodeGoFailoverSuccess(
		testOpenCodeGoFailoverAttemptWithIncarnation(old.stateKey, state.Generation, old.selectedWorkspaceUID, state.Incarnation),
		start.Add(time.Second),
	)
	require.NoError(t, err)
	newState, exists, err := loadOpenCodeGoFailoverState(old.stateKey, start.Add(time.Second))
	require.NoError(t, err)
	require.True(t, exists)
	assert.NotEqual(t, int64(0), newState.Generation)

	late, err := ObserveOpenCodeGoFailoverFailure(old, start.Add(2*time.Second))
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionStale, late.Action)
}

func TestOpenCodeGoFailoverRedisRecreatesStateAfterNaturalExpiry(t *testing.T) {
	server := useOpenCodeGoFailoverRedisForTest(t)
	start := time.Now().Truncate(time.Millisecond)
	key := "redis-natural-expiry"
	first := testOpenCodeGoFailoverAttemptWithIncarnation(key, 0, "workspace-primary", "redis-expiry-incarnation")
	_, err := ObserveOpenCodeGoFailoverFailure(first, start)
	require.NoError(t, err)

	server.FastForward(31 * time.Minute)
	after := start.Add(31 * time.Minute)
	recreated, err := ObserveOpenCodeGoFailoverFailure(
		testOpenCodeGoFailoverAttemptWithIncarnation(key, 0, "workspace-primary", "post-expiry-incarnation"),
		after,
	)
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionSuspect, recreated.Action)
	assert.Equal(t, 1, recreated.FailureCount)
	state, exists, err := loadOpenCodeGoFailoverState(key, after)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, int64(1), state.Generation)

	promoted, err := ObserveOpenCodeGoFailoverFailure(
		testOpenCodeGoFailoverAttemptWithIncarnation(key, state.Generation, "workspace-primary", state.Incarnation),
		after.Add(time.Second),
	)
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionPromoted, promoted.Action)
}

func TestOpenCodeGoFailoverStatefulLeaseRemainsPinnedAndMissingLeaseFails(t *testing.T) {
	useOpenCodeGoFailoverMemoryForTest(t, 100)
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "two", "workspace-two", "wrk_BETA2", []string{"model-a"})
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))

	now := time.Unix(2_000_000_000, 0)
	previousNow := openCodeGoFailoverNow
	openCodeGoFailoverNow = func() time.Time { return now }
	t.Cleanup(func() { openCodeGoFailoverNow = previousNow })
	policy := testOpenCodeGoFailoverPolicy()
	options := OpenCodeGoPoolSelectOptions{AffinityKey: "stateful-lease", Protocol: relaydto.OpenCodeGoProtocolResponses, Failover: policy}

	first, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	primary := first.WorkspaceUID
	_, err = ObserveOpenCodeGoFailoverFailure(first.FailoverAttempt, now)
	require.NoError(t, err)
	second, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	_, err = ObserveOpenCodeGoFailoverFailure(second.FailoverAttempt, now.Add(time.Second))
	require.NoError(t, err)
	promoted, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	backup := promoted.WorkspaceUID
	require.NotEqual(t, primary, backup)

	continuation, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", OpenCodeGoPoolSelectOptions{
		AffinityKey: "stateful-lease",
		Protocol:    relaydto.OpenCodeGoProtocolResponses,
		Stateful:    true,
		Failover:    policy,
	})
	require.NoError(t, err)
	assert.Equal(t, backup, continuation.WorkspaceUID)
	require.NotNil(t, continuation.FailoverAttempt)
	assert.True(t, continuation.FailoverAttempt.suppressFailure)
	assert.True(t, continuation.FailoverAttempt.observeSuccess)
	refreshed, err := ObserveOpenCodeGoFailoverSuccess(continuation.FailoverAttempt, now.Add(2*time.Second))
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionLeaseRefreshed, refreshed.Action)

	stateValue, exists := openCodeGoPoolChannels.Load(channel.Id)
	require.True(t, exists)
	channelState := stateValue.(*openCodeGoPoolChannelState)
	originalSnapshot := channelState.snapshot.Load()
	require.NotNil(t, originalSnapshot)
	filtered := *originalSnapshot
	filtered.byModel = make(map[string][]openCodeGoPoolCandidate, len(originalSnapshot.byModel))
	for modelID, candidates := range originalSnapshot.byModel {
		for _, candidate := range candidates {
			if candidate.workspaceUID != backup {
				filtered.byModel[modelID] = append(filtered.byModel[modelID], candidate)
			}
		}
	}
	channelState.snapshot.Store(&filtered)
	t.Cleanup(func() { channelState.snapshot.Store(originalSnapshot) })

	_, err = SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", OpenCodeGoPoolSelectOptions{
		AffinityKey: "stateful-lease",
		Protocol:    relaydto.OpenCodeGoProtocolResponses,
		Stateful:    true,
		Failover:    policy,
	})
	assert.ErrorIs(t, err, ErrOpenCodeGoStatefulResponsesWorkspaceUnavailable)
}

func TestOpenCodeGoFailoverSingleCandidateStillResetsAndRefreshes(t *testing.T) {
	useOpenCodeGoFailoverMemoryForTest(t, 100)
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "two", "workspace-two", "wrk_BETA2", []string{"model-a"})
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))

	now := time.Unix(2_000_000_000, 0)
	previousNow := openCodeGoFailoverNow
	openCodeGoFailoverNow = func() time.Time { return now }
	t.Cleanup(func() { openCodeGoFailoverNow = previousNow })
	policy := testOpenCodeGoFailoverPolicy()
	options := OpenCodeGoPoolSelectOptions{AffinityKey: "single-candidate", Protocol: relaydto.OpenCodeGoProtocolResponses, Failover: policy}

	first, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	primary := first.WorkspaceUID
	_, err = ObserveOpenCodeGoFailoverFailure(first.FailoverAttempt, now)
	require.NoError(t, err)

	stateValue, exists := openCodeGoPoolChannels.Load(channel.Id)
	require.True(t, exists)
	channelState := stateValue.(*openCodeGoPoolChannelState)
	originalSnapshot := channelState.snapshot.Load()
	require.NotNil(t, originalSnapshot)
	filtered := *originalSnapshot
	filtered.byModel = map[string][]openCodeGoPoolCandidate{"model-a": make([]openCodeGoPoolCandidate, 0, 1)}
	for _, candidate := range originalSnapshot.byModel["model-a"] {
		if candidate.workspaceUID == primary {
			filtered.byModel["model-a"] = append(filtered.byModel["model-a"], candidate)
		}
	}
	channelState.snapshot.Store(&filtered)

	canonical, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	assert.Equal(t, primary, canonical.WorkspaceUID)
	cleared, err := ObserveOpenCodeGoFailoverSuccess(canonical.FailoverAttempt, now.Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionCleared, cleared.Action)

	channelState.snapshot.Store(originalSnapshot)
	next, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	assert.Equal(t, primary, next.WorkspaceUID)
	oneMore, err := ObserveOpenCodeGoFailoverFailure(next.FailoverAttempt, now.Add(2*time.Second))
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionSuspect, oneMore.Action)
	assert.Equal(t, 1, oneMore.FailureCount)

	second, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	_, err = ObserveOpenCodeGoFailoverFailure(second.FailoverAttempt, now.Add(3*time.Second))
	require.NoError(t, err)
	leased, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	leaseUID := leased.WorkspaceUID
	require.NotEqual(t, primary, leaseUID)

	filtered.byModel["model-a"] = make([]openCodeGoPoolCandidate, 0, 1)
	for _, candidate := range originalSnapshot.byModel["model-a"] {
		if candidate.workspaceUID == leaseUID {
			filtered.byModel["model-a"] = append(filtered.byModel["model-a"], candidate)
		}
	}
	channelState.snapshot.Store(&filtered)
	sole, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	assert.Equal(t, leaseUID, sole.WorkspaceUID)
	refreshed, err := ObserveOpenCodeGoFailoverSuccess(sole.FailoverAttempt, now.Add(4*time.Second))
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionLeaseRefreshed, refreshed.Action)
	channelState.snapshot.Store(originalSnapshot)
}

func TestOpenCodeGoFailoverRedisOldGenerationStaysStaleAfterClear(t *testing.T) {
	useOpenCodeGoFailoverRedisForTest(t)
	start := time.Now().Truncate(time.Millisecond)
	key := "aba-redis"
	old := testOpenCodeGoFailoverAttemptWithIncarnation(key, 0, "workspace-primary", "old-redis-incarnation")
	_, err := ObserveOpenCodeGoFailoverFailure(old, start)
	require.NoError(t, err)
	state, exists, err := loadOpenCodeGoFailoverState(key, start)
	require.NoError(t, err)
	require.True(t, exists)
	_, err = ObserveOpenCodeGoFailoverSuccess(
		testOpenCodeGoFailoverAttemptWithIncarnation(key, state.Generation, old.selectedWorkspaceUID, state.Incarnation),
		start.Add(time.Second),
	)
	require.NoError(t, err)
	newState, exists, err := loadOpenCodeGoFailoverState(key, start.Add(time.Second))
	require.NoError(t, err)
	require.True(t, exists)
	assert.NotEqual(t, int64(0), newState.Generation)

	late, err := ObserveOpenCodeGoFailoverFailure(old, start.Add(2*time.Second))
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoFailoverActionStale, late.Action)
}

func TestOpenCodeGoFailoverRedisCleanupIsBoundedAndPreservesCurrentState(t *testing.T) {
	useOpenCodeGoFailoverRedisForTest(t)
	policy := testOpenCodeGoFailoverPolicy()
	now := time.Now().Truncate(time.Millisecond)
	nowMS := now.UnixMilli()
	indexKey := openCodeGoFailoverRedisPrefix + ":cleanup-index"
	currentKey := openCodeGoFailoverRedisPrefix + ":state:cleanup-current"
	members := make([]*redis.Z, 0, 130)
	// Put the current key first in score order. A cleanup implementation that
	// scans after HSET would delete the freshly recreated state here.
	members = append(members, &redis.Z{Score: float64(nowMS - 2_000), Member: currentKey})
	for index := 0; index < 129; index++ {
		members = append(members, &redis.Z{
			Score:  float64(nowMS - 1_000),
			Member: fmt.Sprintf("%s:state:expired-%03d", openCodeGoFailoverRedisPrefix, index),
		})
	}
	require.NoError(t, common.RDB.ZAdd(context.Background(), indexKey, members...).Err())

	attempt := &OpenCodeGoFailoverAttempt{
		stateKey:                 currentKey,
		incarnation:              "cleanup-incarnation",
		selectedWorkspaceUID:     "workspace-primary",
		canonicalWorkspaceUID:    "workspace-primary",
		preferredBackupWorkspace: "workspace-backup",
		policy:                   policy,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := openCodeGoFailoverScript.Run(
		ctx,
		common.RDB,
		[]string{currentKey, indexKey},
		openCodeGoFailoverStateVersion,
		"failure",
		nowMS,
		0,
		attempt.incarnation,
		attempt.selectedWorkspaceUID,
		attempt.canonicalWorkspaceUID,
		attempt.preferredBackupWorkspace,
		policy.FailureThreshold,
		policy.FailureWindow.Milliseconds(),
		policy.LeaseDuration.Milliseconds(),
		openCodeGoFailoverRetention(policy).Milliseconds(),
		policy.MaxBackups,
		1_000,
	).Result()
	require.NoError(t, err)

	values, err := common.RDB.HGetAll(context.Background(), currentKey).Result()
	require.NoError(t, err)
	state, err := decodeOpenCodeGoFailoverRedisState(values)
	require.NoError(t, err)
	assert.Equal(t, "cleanup-incarnation", state.Incarnation)
	assert.Equal(t, int64(1), state.Generation)
	currentScore, err := common.RDB.ZScore(context.Background(), indexKey, currentKey).Result()
	require.NoError(t, err)
	assert.Greater(t, int64(currentScore), nowMS)
	expiredCount, err := common.RDB.ZCount(context.Background(), indexKey, "-inf", fmt.Sprint(nowMS)).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(2), expiredCount)
}
