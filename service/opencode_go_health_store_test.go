package service

import (
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
