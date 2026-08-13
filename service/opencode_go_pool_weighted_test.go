package service

import (
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	relaydto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openCodeGoWeightedTestCandidates(workspaceUIDs ...string) []openCodeGoRankedCandidate {
	ranked := make([]openCodeGoRankedCandidate, 0, len(workspaceUIDs))
	for index, uid := range workspaceUIDs {
		ranked = append(ranked, openCodeGoRankedCandidate{
			candidate: openCodeGoPoolCandidate{
				workspaceUID: uid,
				identityUID:  "identity-" + uid,
				quotaFactor:  1.0,
			},
			score: uint64(len(workspaceUIDs) - index),
		})
	}
	return ranked
}

func TestOpenCodeGoWeightedSelectionStaysHomeWhenHealthy(t *testing.T) {
	ranked := openCodeGoWeightedTestCandidates("home", "backup")
	assert.Equal(t, 0, openCodeGoWeightedSelectionIndex(999, ranked))
}

func TestOpenCodeGoWeightedSelectionMovesOffBusyHome(t *testing.T) {
	const channelID = 1001
	ranked := openCodeGoWeightedTestCandidates("home", "backup")
	AcquireOpenCodeGoWorkspaceInFlight(channelID, "home")
	for range 50 {
		AcquireOpenCodeGoWorkspaceInFlight(channelID, "home")
	}
	// home inflight=51 -> inflightFactor ~0.20 < 0.5*margin against backup 1.0
	assert.Equal(t, 1, openCodeGoWeightedSelectionIndex(channelID, ranked))
}

func TestOpenCodeGoWeightedSelectionToleratesModerateHomeLoad(t *testing.T) {
	const channelID = 1002
	ranked := openCodeGoWeightedTestCandidates("home", "backup")
	for range 10 {
		AcquireOpenCodeGoWorkspaceInFlight(channelID, "home")
	}
	// home inflight=10 -> inflightFactor ~0.84 >= 0.5*1.0 -> keep affinity
	assert.Equal(t, 0, openCodeGoWeightedSelectionIndex(channelID, ranked))
}

func TestOpenCodeGoWeightedSelectionSingleCandidateIgnoresLoad(t *testing.T) {
	ranked := openCodeGoWeightedTestCandidates("only")
	for range openCodeGoInflightSoftCap + 10 {
		AcquireOpenCodeGoWorkspaceInFlight(1003, "only")
	}
	assert.Equal(t, 0, openCodeGoWeightedSelectionIndex(1003, ranked))
}

func TestOpenCodeGoInflightCountersBalanceAndFloorAtZero(t *testing.T) {
	const channelID = 1004
	const total = 200
	var wait sync.WaitGroup
	// Phase 1: concurrent acquires (a request holds its slot before release).
	for index := 0; index < total; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			AcquireOpenCodeGoWorkspaceInFlight(channelID, "wrk")
		}()
	}
	wait.Wait()
	assert.Equal(t, int64(total), OpenCodeGoWorkspaceInFlight(channelID, "wrk"))

	// Phase 2: concurrent releases after every acquire has completed.
	for index := 0; index < total; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			ReleaseOpenCodeGoWorkspaceInFlight(channelID, "wrk")
		}()
	}
	wait.Wait()
	assert.Equal(t, int64(0), OpenCodeGoWorkspaceInFlight(channelID, "wrk"))

	// Extra releases never push below zero.
	for range 5 {
		ReleaseOpenCodeGoWorkspaceInFlight(channelID, "wrk")
	}
	assert.Equal(t, int64(0), OpenCodeGoWorkspaceInFlight(channelID, "wrk"))
}

func TestOpenCodeGoPoolLoadAwareSelectionIntegration(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	first := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})
	second := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "two", "workspace-two", "wrk_BETA2", []string{"model-a"})
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))

	options := OpenCodeGoPoolSelectOptions{
		AffinityKey: "token-42",
		LoadAware:   true,
	}

	// Without load, the rendezvous home is kept.
	home, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	require.Equal(t, 0, home.CandidateRank)

	// Load up the home; the same affinity should move to the other workspace.
	for range openCodeGoInflightSoftCap - 1 {
		AcquireOpenCodeGoWorkspaceInFlight(channel.Id, home.WorkspaceUID)
	}
	moved, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	require.NotEqual(t, home.WorkspaceUID, moved.WorkspaceUID)
	require.Equal(t, 1, moved.CandidateRank)

	// The other workspace is the only non-home candidate.
	other := second.UID
	if home.WorkspaceUID == second.UID {
		other = first.UID
	}
	assert.Equal(t, other, moved.WorkspaceUID)

	// Releasing load returns the affinity to the home workspace.
	for range openCodeGoInflightSoftCap - 1 {
		ReleaseOpenCodeGoWorkspaceInFlight(channel.Id, home.WorkspaceUID)
	}
	restored, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	assert.Equal(t, home.WorkspaceUID, restored.WorkspaceUID)

	// Without LoadAware the selection stays on the home regardless of load.
	for range openCodeGoInflightSoftCap - 1 {
		AcquireOpenCodeGoWorkspaceInFlight(channel.Id, home.WorkspaceUID)
	}
	withoutLoad, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", OpenCodeGoPoolSelectOptions{
		AffinityKey: "token-42",
	})
	require.NoError(t, err)
	assert.Equal(t, home.WorkspaceUID, withoutLoad.WorkspaceUID)
}

func TestOpenCodeGoLoadAwareDoesNotOverrideFailoverLease(t *testing.T) {
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
		LoadAware:   true,
	}

	first, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	backup := first.FailoverAttempt.PreferredBackupWorkspaceUID()
	require.NotEqual(t, first.WorkspaceUID, backup)
	_, err = ObserveOpenCodeGoFailoverFailure(first.FailoverAttempt, now)
	require.NoError(t, err)
	second, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	_, err = ObserveOpenCodeGoFailoverFailure(second.FailoverAttempt, now.Add(time.Second))
	require.NoError(t, err)

	// Promotion creates an active lease on the backup. Even with that backup
	// heavily loaded, the explicit failover lease wins over load weighting.
	for range openCodeGoInflightSoftCap - 1 {
		AcquireOpenCodeGoWorkspaceInFlight(channel.Id, backup)
	}
	leased, err := SelectOpenCodeGoWorkspaceWithFailover(channel.Id, "model-a", options)
	require.NoError(t, err)
	require.True(t, leased.FailoverActive)
	assert.Equal(t, backup, leased.WorkspaceUID)
}

func TestOpenCodeGoBulkFailureAutoDisablesWorkspace(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	first := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "two", "workspace-two", "wrk_BETA2", []string{"model-a"})
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))

	now := time.Unix(2_000_000_000, 0)
	for range openCodeGoBulkFailureThreshold {
		disabled, err := ObserveOpenCodeGoBulkProviderFailure(channel.Id, first.UID, OpenCodeGoProviderFailure{
			StatusCode: http.StatusUnauthorized,
			ErrorType:  "AuthError",
			Message:    "Invalid API key",
		}, now)
		require.NoError(t, err)
		if disabled {
			break
		}
		now = now.Add(time.Minute)
	}

	workspace, err := model.GetOpenCodeGoWorkspace(channel.Id, first.UID)
	require.NoError(t, err)
	require.Equal(t, model.OpenCodeGoStateBulkDisabled, workspace.EffectiveState)
	require.Greater(t, workspace.BulkFailureDetectedAt, int64(0))

	// The auto-disabled workspace is removed from the pool snapshot.
	selection, err := SelectOpenCodeGoWorkspace(channel.Id, "model-a")
	require.NoError(t, err)
	require.NotEqual(t, first.UID, selection.WorkspaceUID)
}

func TestOpenCodeGoBulkFailureBelowThresholdDoesNotAcquireMutationBarrier(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t,
		db,
		codec,
		channel.Id,
		"bulk-counter-no-barrier",
		"workspace-bulk-counter-no-barrier",
		"wrk_BULKCOUNTER",
		[]string{"model-a"},
	)
	relayRelease := openCodeGoPoolMutations.beginRelay(channel.Id)
	defer relayRelease()

	result := make(chan error, 1)
	go func() {
		disabled, err := ObserveOpenCodeGoBulkProviderFailure(
			channel.Id,
			workspace.UID,
			OpenCodeGoProviderFailure{
				StatusCode: http.StatusUnauthorized,
				ErrorType:  "AuthError",
				Message:    "Invalid API key",
			},
			time.Unix(2_000_000_000, 0),
		)
		if err == nil && disabled {
			err = errors.New("bulk failure unexpectedly reached the disable threshold")
		}
		result <- err
	}()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("below-threshold bulk counter waited on a relay lease")
	}
}

func TestOpenCodeGoBulkFailureIgnoresTransientAndQuotaStatuses(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	first := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))

	now := time.Unix(2_000_000_000, 0)
	for _, failure := range []OpenCodeGoProviderFailure{
		{StatusCode: http.StatusForbidden, ErrorType: "RegionError", Message: "region blocked"},
		{StatusCode: http.StatusUnauthorized, ErrorType: "ModelError", Message: "model blocked"},
		{StatusCode: http.StatusTooManyRequests, ErrorType: "RateLimitError", Message: "rate limited"},
		{StatusCode: http.StatusInternalServerError, ErrorType: "AuthError", Message: "transient"},
		{StatusCode: http.StatusServiceUnavailable, ErrorType: "upstream_error", Message: "transient"},
	} {
		disabled, err := ObserveOpenCodeGoBulkProviderFailure(channel.Id, first.UID, failure, now)
		require.NoError(t, err)
		require.False(t, disabled, "failure %#v must not auto-disable", failure)
		now = now.Add(time.Minute)
	}
	workspace, err := model.GetOpenCodeGoWorkspace(channel.Id, first.UID)
	require.NoError(t, err)
	require.Equal(t, model.OpenCodeGoStateEligible, workspace.EffectiveState)
	require.Zero(t, workspace.BulkFailureDetectedAt)
}

func TestOpenCodeGoBulkFailureWindowResetsStaleCount(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	first := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))

	now := time.Unix(2_000_000_000, 0)
	// Two failures, then a long gap resets the window before the threshold is met.
	for range openCodeGoBulkFailureThreshold - 1 {
		_, err := ObserveOpenCodeGoBulkProviderFailure(channel.Id, first.UID, OpenCodeGoProviderFailure{
			StatusCode: http.StatusUnauthorized,
			ErrorType:  "AuthError",
			Message:    "Invalid API key",
		}, now)
		require.NoError(t, err)
		now = now.Add(time.Minute)
	}
	now = now.Add(openCodeGoBulkFailureWindow + time.Minute)
	disabled, err := ObserveOpenCodeGoBulkProviderFailure(channel.Id, first.UID, OpenCodeGoProviderFailure{
		StatusCode: http.StatusUnauthorized,
		ErrorType:  "AuthError",
		Message:    "Invalid API key",
	}, now)
	require.NoError(t, err)
	require.False(t, disabled)
	workspace, err := model.GetOpenCodeGoWorkspace(channel.Id, first.UID)
	require.NoError(t, err)
	require.Zero(t, workspace.BulkFailureDetectedAt)
}

func TestOpenCodeGoBulkDisabledRecoversOnlyByManualEnable(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	first := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "two", "workspace-two", "wrk_BETA2", []string{"model-a"})
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))

	now := time.Unix(2_000_000_000, 0)
	for range openCodeGoBulkFailureThreshold - 1 {
		_, err := ObserveOpenCodeGoBulkProviderFailure(channel.Id, first.UID, OpenCodeGoProviderFailure{
			StatusCode: http.StatusUnauthorized,
			ErrorType:  "AuthError",
			Message:    "Invalid API key",
		}, now)
		require.NoError(t, err)
		now = now.Add(time.Minute)
	}
	disabled, err := ObserveOpenCodeGoBulkProviderFailure(channel.Id, first.UID, OpenCodeGoProviderFailure{
		StatusCode: http.StatusUnauthorized,
		ErrorType:  "AuthError",
		Message:    "Invalid API key",
	}, now)
	require.NoError(t, err)
	require.True(t, disabled)

	// Manual re-enable (human verification) clears the bulk-disable evidence.
	pool := &OpenCodeGoAccountPoolService{now: func() time.Time { return now.Add(time.Hour) }}
	require.NoError(t, pool.SetWorkspaceEnabled(channel.Id, first.UID, true))

	workspace, err := model.GetOpenCodeGoWorkspace(channel.Id, first.UID)
	require.NoError(t, err)
	require.Zero(t, workspace.BulkFailureDetectedAt)
	require.Equal(t, model.OpenCodeGoStateEligible, workspace.EffectiveState)
}

func TestOpenCodeGoPoolViewExposesInflight(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	first := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))

	AcquireOpenCodeGoWorkspaceInFlight(channel.Id, first.UID)
	AcquireOpenCodeGoWorkspaceInFlight(channel.Id, first.UID)
	t.Cleanup(func() {
		ReleaseOpenCodeGoWorkspaceInFlight(channel.Id, first.UID)
		ReleaseOpenCodeGoWorkspaceInFlight(channel.Id, first.UID)
	})

	view, err := GetOpenCodeGoPoolView(channel.Id)
	require.NoError(t, err)
	require.Len(t, view.Identities, 1)
	require.Len(t, view.Identities[0].Workspaces, 1)
	assert.Equal(t, int64(2), view.Identities[0].Workspaces[0].Inflight)
}

func TestListOpenCodeGoPoolModelsUsesDiscoveredInventoryNotEligibleSnapshot(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "inventory", "workspace-inventory", "wrk_INVENTORY1",
		[]string{"available-model", "temporarily-blocked-model"},
	)
	require.NoError(t, db.Model(&model.OpenCodeGoWorkspace{}).
		Where("id = ?", workspace.ID).
		Updates(map[string]interface{}{
			"manual_enabled":  false,
			"effective_state": model.OpenCodeGoStateRiskBlocked,
		}).Error)
	require.NoError(t, db.Model(&model.OpenCodeGoWorkspaceModel{}).
		Where("workspace_id = ? AND model = ?", workspace.ID, "temporarily-blocked-model").
		Update("state", model.OpenCodeGoModelRPMCooldown).Error)
	require.NoError(t, db.Create(&model.OpenCodeGoWorkspaceModel{
		WorkspaceID: workspace.ID,
		Model:       "no-longer-discovered",
		Discovered:  false,
		State:       model.OpenCodeGoModelDisabled,
	}).Error)
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))
	_, err := SelectOpenCodeGoWorkspace(channel.Id, "available-model")
	require.ErrorIs(t, err, ErrOpenCodeGoNoEligibleWorkspace)

	models, err := ListOpenCodeGoPoolModels(channel.Id)
	require.NoError(t, err)
	require.Equal(t, []string{"available-model", "temporarily-blocked-model"}, models)
}
