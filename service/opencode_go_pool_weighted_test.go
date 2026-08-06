package service

import (
	"sync"
	"testing"
	"time"

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
