package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecoverOpenCodeGoModelCooldownsRecoversOnlyDueModelsAndRebuildsOnce(t *testing.T) {
	_, channel, _ := setupOpenCodeGoPoolTestDB(t)
	identity := model.OpenCodeGoIdentity{
		UID:                   "identity-recovery",
		ChannelID:             channel.Id,
		AuthCookieCiphertext:  "encrypted-cookie-recovery",
		AuthCookieFingerprint: "fingerprint-recovery",
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, model.DB.Create(&identity).Error)
	workspace := seedOpenCodeGoHealthWorkspace(
		t,
		channel.Id,
		identity.ID,
		"workspace-recovery",
		"glm-5.2",
		"kimi-k2.5",
		"minimax-m3",
	)
	now := time.Unix(1_900_000_500, 0)
	require.NoError(t, model.DB.Model(&model.OpenCodeGoWorkspaceModel{}).
		Where("workspace_id = ? AND model IN ?", workspace.ID, []string{"glm-5.2", "kimi-k2.5"}).
		Updates(map[string]interface{}{
			"state":              model.OpenCodeGoModelRPMCooldown,
			"disabled_until":     now.Add(-time.Second).Unix(),
			"health_observation": string(OpenCodeGoObservationRPMThrottled),
			"health_observed_at": now.Add(-time.Minute).UnixNano(),
		}).Error)
	require.NoError(t, model.DB.Model(&model.OpenCodeGoWorkspaceModel{}).
		Where("workspace_id = ? AND model = ?", workspace.ID, "minimax-m3").
		Updates(map[string]interface{}{
			"state":              model.OpenCodeGoModelTransient,
			"disabled_until":     now.Add(time.Minute).Unix(),
			"health_observation": string(OpenCodeGoObservationTransientFailure),
			"health_observed_at": now.Add(-time.Minute).UnixNano(),
		}).Error)

	rebuilds := 0
	summary, err := recoverOpenCodeGoModelCooldowns(channel.Id, now, 100, func(gotChannelID int) error {
		rebuilds++
		assert.Equal(t, channel.Id, gotChannelID)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoModelRecoverySummary{Total: 2, Recovered: 2}, summary)
	assert.Equal(t, 1, rebuilds)

	after, err := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
	require.NoError(t, err)
	states := make(map[string]model.OpenCodeGoWorkspaceModel, len(after.Models))
	for _, entry := range after.Models {
		states[entry.Model] = entry
	}
	assert.Equal(t, model.OpenCodeGoModelAvailable, states["glm-5.2"].State)
	assert.Equal(t, model.OpenCodeGoModelAvailable, states["kimi-k2.5"].State)
	assert.Equal(t, model.OpenCodeGoModelTransient, states["minimax-m3"].State)
	assert.Equal(t, string(OpenCodeGoObservationCooldownExpired), states["glm-5.2"].HealthObservation)
}
