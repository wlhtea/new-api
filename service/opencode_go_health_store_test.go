package service

import (
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedOpenCodeGoHealthWorkspace(
	t *testing.T,
	channelID int,
	identityID int64,
	uid string,
	models ...string,
) model.OpenCodeGoWorkspace {
	t.Helper()
	now := time.Unix(1_900_000_000, 0).Unix()
	workspace := model.OpenCodeGoWorkspace{
		UID:                 uid,
		ChannelID:           channelID,
		IdentityID:          identityID,
		UpstreamWorkspaceID: "wrk_SYNTHETIC_" + uid,
		APIKeyCiphertext:    "encrypted-fixture",
		CredentialStatus:    model.OpenCodeGoCredentialValid,
		MembershipStatus:    model.OpenCodeGoMembershipActive,
		ManualEnabled:       true,
		EffectiveState:      model.OpenCodeGoStateEligible,
		QuotaSnapshotStatus: model.OpenCodeGoQuotaSnapshotComplete,
		QuotaFetchedAt:      now,
	}
	require.NoError(t, model.DB.Create(&workspace).Error)
	for index, kind := range model.OpenCodeGoQuotaKinds {
		require.NoError(t, model.DB.Create(&model.OpenCodeGoQuotaWindow{
			WorkspaceID: workspace.ID,
			Kind:        kind,
			UsedPercent: 10,
			ResetAt:     now + int64((index+1)*3600),
			FetchedAt:   now,
		}).Error)
	}
	for _, modelID := range models {
		require.NoError(t, model.DB.Create(&model.OpenCodeGoWorkspaceModel{
			WorkspaceID: workspace.ID,
			Model:       modelID,
			Discovered:  true,
			State:       model.OpenCodeGoModelAvailable,
		}).Error)
	}
	return workspace
}

func TestApplyOpenCodeGoClassifiedFailureUpdatesOnlySelectedWorkspaceModel(t *testing.T) {
	_, channel, _ := setupOpenCodeGoPoolTestDB(t)
	identity := model.OpenCodeGoIdentity{
		UID:                   "identity-health-a",
		ChannelID:             channel.Id,
		AuthCookieCiphertext:  "encrypted-cookie-a",
		AuthCookieFingerprint: "fingerprint-health-a",
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, model.DB.Create(&identity).Error)
	selected := seedOpenCodeGoHealthWorkspace(t, channel.Id, identity.ID, "workspace-health-a", "glm-5.2", "kimi-k2.5")
	unrelated := seedOpenCodeGoHealthWorkspace(t, channel.Id, identity.ID, "workspace-health-b", "glm-5.2")

	now := time.Unix(1_900_000_100, 0)
	classified, ok := ClassifyOpenCodeGoProviderFailure(OpenCodeGoProviderFailure{
		StatusCode: 429,
		ErrorType:  "RateLimitError",
		ErrorCode:  "rate_limit",
		Message:    "rate limited",
		RetryAfter: "90",
	}, now)
	require.True(t, ok)
	rebuilds := 0
	applied, err := applyOpenCodeGoClassifiedFailure(
		channel.Id,
		selected.UID,
		"glm-5.2",
		classified,
		func(gotChannelID int) error {
			rebuilds++
			assert.Equal(t, channel.Id, gotChannelID)
			return nil
		},
	)
	require.NoError(t, err)
	require.True(t, applied)
	assert.Equal(t, 1, rebuilds)

	selectedAfter, err := model.GetOpenCodeGoWorkspace(channel.Id, selected.UID)
	require.NoError(t, err)
	require.Equal(t, model.OpenCodeGoStateEligible, selectedAfter.EffectiveState)
	states := map[string]string{}
	for _, entry := range selectedAfter.Models {
		states[entry.Model] = entry.State
	}
	assert.Equal(t, model.OpenCodeGoModelRPMCooldown, states["glm-5.2"])
	assert.Equal(t, model.OpenCodeGoModelAvailable, states["kimi-k2.5"])

	unrelatedAfter, err := model.GetOpenCodeGoWorkspace(channel.Id, unrelated.UID)
	require.NoError(t, err)
	require.Len(t, unrelatedAfter.Models, 1)
	assert.Equal(t, model.OpenCodeGoModelAvailable, unrelatedAfter.Models[0].State)
}

func TestApplyOpenCodeGoClassifiedFailureRejectsOlderWorkspaceObservation(t *testing.T) {
	_, channel, _ := setupOpenCodeGoPoolTestDB(t)
	identity := model.OpenCodeGoIdentity{
		UID:                   "identity-health-risk",
		ChannelID:             channel.Id,
		AuthCookieCiphertext:  "encrypted-cookie-risk",
		AuthCookieFingerprint: "fingerprint-health-risk",
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, model.DB.Create(&identity).Error)
	workspace := seedOpenCodeGoHealthWorkspace(t, channel.Id, identity.ID, "workspace-health-risk", "glm-5.2")

	later := time.Unix(1_900_000_200, 0)
	risk, ok := ClassifyOpenCodeGoProviderFailure(OpenCodeGoProviderFailure{
		StatusCode: 401,
		ErrorType:  "AuthError",
		Message:    "Request blocked by upstream provider.",
	}, later)
	require.True(t, ok)
	rebuilds := 0
	applied, err := applyOpenCodeGoClassifiedFailure(channel.Id, workspace.UID, "glm-5.2", risk, func(int) error {
		rebuilds++
		return nil
	})
	require.NoError(t, err)
	require.True(t, applied)

	older, ok := ClassifyOpenCodeGoProviderFailure(OpenCodeGoProviderFailure{
		StatusCode: 401,
		ErrorType:  "AuthError",
		Message:    "Invalid API key",
	}, later.Add(-time.Second))
	require.True(t, ok)
	applied, err = applyOpenCodeGoClassifiedFailure(channel.Id, workspace.UID, "glm-5.2", older, func(int) error {
		rebuilds++
		return nil
	})
	require.NoError(t, err)
	assert.False(t, applied)
	assert.Equal(t, 1, rebuilds)

	after, err := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
	require.NoError(t, err)
	assert.Equal(t, model.OpenCodeGoStateRiskBlocked, after.EffectiveState)
	assert.Equal(t, model.OpenCodeGoCredentialValid, after.CredentialStatus)
}

func TestRestrictiveOpenCodeGoHealthWriteKeepsRelayBlockedUntilRebuildCompletes(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t,
		db,
		codec,
		channel.Id,
		"health-barrier",
		"workspace-health-barrier",
		"wrk_HEALTHBARRIER",
		[]string{"model-a"},
	)
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))
	classified, ok := ClassifyOpenCodeGoProviderFailure(OpenCodeGoProviderFailure{
		StatusCode: http.StatusUnauthorized,
		ErrorType:  "AuthError",
		Message:    "Invalid API key",
	}, time.Unix(1_900_000_100, 0))
	require.True(t, ok)

	rebuildStarted := make(chan struct{})
	allowRebuild := make(chan struct{})
	applyResult := make(chan error, 1)
	go func() {
		_, applyErr := applyOpenCodeGoClassifiedFailure(
			channel.Id,
			workspace.UID,
			"model-a",
			classified,
			func(channelID int) error {
				close(rebuildStarted)
				<-allowRebuild
				return RebuildOpenCodeGoPoolChannel(channelID)
			},
		)
		applyResult <- applyErr
	}()
	<-rebuildStarted

	relayAcquired := make(chan struct{}, 1)
	go func() {
		release := openCodeGoPoolMutations.beginRelay(channel.Id)
		release()
		relayAcquired <- struct{}{}
	}()
	select {
	case <-relayAcquired:
		t.Fatal("relay crossed the health commit-to-rebuild window")
	case <-time.After(50 * time.Millisecond):
	}

	close(allowRebuild)
	require.NoError(t, <-applyResult)
	select {
	case <-relayAcquired:
	case <-time.After(time.Second):
		t.Fatal("relay lease did not resume after health rebuild")
	}
}

func TestRestorativeOpenCodeGoHealthWriteDoesNotAcquireRestrictiveMutationBarrier(t *testing.T) {
	_, channel, _ := setupOpenCodeGoPoolTestDB(t)
	identity := model.OpenCodeGoIdentity{
		UID:                   "identity-health-recovery-no-barrier",
		ChannelID:             channel.Id,
		AuthCookieCiphertext:  "encrypted-cookie-health-recovery-no-barrier",
		AuthCookieFingerprint: "fingerprint-health-recovery-no-barrier",
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, model.DB.Create(&identity).Error)
	workspace := seedOpenCodeGoHealthWorkspace(t, channel.Id, identity.ID, "workspace-health-recovery-no-barrier", "glm-5.2")
	riskAt := time.Unix(1_900_000_100, 0)
	require.NoError(t, model.DB.Model(&model.OpenCodeGoWorkspace{}).
		Where("id = ?", workspace.ID).
		Updates(map[string]interface{}{
			"effective_state":    model.OpenCodeGoStateRiskBlocked,
			"risk_detected_at":   riskAt.Unix(),
			"health_observation": string(OpenCodeGoObservationRiskBlocked),
			"health_observed_at": riskAt.UnixNano(),
		}).Error)

	relayRelease := openCodeGoPoolMutations.beginRelay(channel.Id)
	defer relayRelease()
	result := make(chan error, 1)
	go func() {
		_, applyErr := applyOpenCodeGoClassifiedFailure(
			channel.Id,
			workspace.UID,
			"glm-5.2",
			OpenCodeGoClassifiedFailure{
				Scope: OpenCodeGoHealthScopeWorkspace,
				Observation: OpenCodeGoHealthObservation{
					Kind:            OpenCodeGoObservationRiskProbeSucceeded,
					ObservedAt:      riskAt.Add(time.Minute),
					HasUsableModels: true,
				},
			},
			func(int) error { return nil },
		)
		result <- applyErr
	}()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("restorative health observation waited on a relay lease")
	}
}
