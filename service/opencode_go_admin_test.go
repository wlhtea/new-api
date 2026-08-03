package service

import (
	"errors"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestOpenCodeGoPoolViewRedactsEveryCredentialAndStoredDiagnostic(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t,
		db,
		codec,
		channel.Id,
		"one",
		"workspace-one",
		"wrk_SECRET1",
		[]string{"model-a"},
	)
	diagnostic := "auth=hidden-cookie; sk-hidden-key wrk_SECRET1"
	require.NoError(t, db.Model(&model.OpenCodeGoIdentity{}).
		Where("id = ?", workspace.IdentityID).
		Updates(map[string]interface{}{
			"auth_cookie_ciphertext": "auth=hidden-cookie",
			"last_error":             diagnostic,
		}).Error)
	require.NoError(t, db.Model(&model.OpenCodeGoWorkspace{}).
		Where("id = ?", workspace.ID).
		Updates(map[string]interface{}{
			"api_key_ciphertext": "sk-hidden-key",
			"api_key_prefix":     "sk-hidd...",
			"state_reason":       diagnostic,
			"quota_error":        diagnostic,
			"last_error":         diagnostic,
		}).Error)
	require.NoError(t, db.Create(&model.OpenCodeGoOperation{
		UID:          "operation-redaction",
		ChannelID:    channel.Id,
		WorkspaceID:  workspace.ID,
		WorkspaceUID: workspace.UID,
		Action:       OpenCodeGoOperationApplyReferral,
		Source:       "manual",
		Status:       OpenCodeGoOperationStatusFailed,
		Error:        diagnostic + " ref_SECRET1",
	}).Error)

	view, err := GetOpenCodeGoPoolView(channel.Id)
	require.NoError(t, err)
	require.Len(t, view.Identities, 1)
	require.True(t, view.Identities[0].HasAuthCookie)
	require.Contains(t, view.Identities[0].LastError, "[redacted")
	require.Len(t, view.Identities[0].Workspaces, 1)
	workspaceView := view.Identities[0].Workspaces[0]
	require.True(t, workspaceView.HasAPIKey)
	require.Contains(t, workspaceView.StateReason, "[redacted")
	require.Len(t, workspaceView.QuotaWindows, 3)
	require.Equal(t, openCodeGoQuotaSourceConsole, workspaceView.QuotaWindows[0].Source)
	require.False(t, workspaceView.QuotaWindows[0].AmountsAuthoritative)
	require.Len(t, view.Operations, 1)
	require.Contains(t, view.Operations[0].Error, "[redacted")

	payload, err := common.Marshal(view)
	require.NoError(t, err)
	serialized := string(payload)
	for _, forbidden := range []string{
		"hidden-cookie",
		"hidden-key",
		"sk-hidd",
		"wrk_SECRET1",
		"ref_SECRET1",
		"auth_cookie_ciphertext",
		"api_key_ciphertext",
		"api_key_prefix",
		"upstream_workspace_id",
		"ocg:v1:",
	} {
		require.NotContains(t, serialized, forbidden)
	}
	require.Equal(t, "", sanitizeOpenCodeGoStoredMessage(""))
	require.Contains(t, sanitizeOpenCodeGoStoredMessage(errors.New(diagnostic).Error()), "[redacted")
}

func TestOpenCodeGoPoolViewConvertsHealthObservationNanosecondsToSeconds(t *testing.T) {
	observedAt := time.Unix(1_900_000_000, 987_654_321)
	workspace := model.OpenCodeGoWorkspace{
		HealthObservedAt: observedAt.UnixNano(),
		Models: []model.OpenCodeGoWorkspaceModel{
			{HealthObservedAt: observedAt.Add(time.Second).UnixNano()},
		},
	}

	view := openCodeGoWorkspaceToView(workspace)

	require.Equal(t, observedAt.Unix(), view.HealthObservedAt)
	require.Len(t, view.Models, 1)
	require.Equal(t, observedAt.Add(time.Second).Unix(), view.Models[0].HealthObservedAt)
	require.LessOrEqual(t, view.HealthObservedAt, int64(9_007_199_254_740_991))
	require.Zero(t, openCodeGoHealthObservedAtForView(0))
}

func TestOpenCodeGoManualEnablementInvalidatesModelsAndPoolSnapshots(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t,
		db,
		codec,
		channel.Id,
		"one",
		"workspace-one",
		"wrk_ALPHA1",
		[]string{"model-a"},
	)
	require.NoError(t, syncOpenCodeGoChannelModels(channel.Id))
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))
	_, err := SelectOpenCodeGoWorkspace(channel.Id, "model-a")
	require.NoError(t, err)

	adminService := NewOpenCodeGoAccountPoolAdminService()
	require.NoError(t, adminService.SetIdentityEnabled(channel.Id, "identity-one", false))
	view, err := GetOpenCodeGoPoolView(channel.Id)
	require.NoError(t, err)
	require.False(t, view.Identities[0].ManualEnabled)
	_, err = SelectOpenCodeGoWorkspace(channel.Id, "model-a")
	require.ErrorIs(t, err, ErrOpenCodeGoNoEligibleWorkspace)
	reloaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	require.Empty(t, reloaded.Models)

	require.NoError(t, adminService.SetIdentityEnabled(channel.Id, "identity-one", true))
	_, err = SelectOpenCodeGoWorkspace(channel.Id, "model-a")
	require.NoError(t, err)
	reloaded, err = model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	require.Equal(t, "model-a", reloaded.Models)

	require.NoError(t, adminService.SetWorkspaceEnabled(channel.Id, workspace.UID, false))
	_, err = SelectOpenCodeGoWorkspace(channel.Id, "model-a")
	require.ErrorIs(t, err, ErrOpenCodeGoNoEligibleWorkspace)
	require.NoError(t, adminService.SetWorkspaceEnabled(channel.Id, workspace.UID, true))
	_, err = SelectOpenCodeGoWorkspace(channel.Id, "model-a")
	require.NoError(t, err)
}

func TestSetOpenCodeGoWorkspaceEnabledCannotClearRiskBlock(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t,
		db,
		codec,
		channel.Id,
		"risk-enable",
		"workspace-risk-enable",
		"wrk_RISKENABLE",
		[]string{"model-a"},
	)
	riskAt := time.Unix(1_900_000_100, 0)
	classified, ok := ClassifyOpenCodeGoProviderFailure(OpenCodeGoProviderFailure{
		StatusCode: 401,
		ErrorType:  "AuthError",
		Message:    "Request blocked by upstream provider.",
	}, riskAt)
	require.True(t, ok)
	applied, err := applyOpenCodeGoClassifiedFailure(channel.Id, workspace.UID, "model-a", classified, nil)
	require.NoError(t, err)
	require.True(t, applied)

	adminService := NewOpenCodeGoAccountPoolAdminService()
	adminService.now = func() time.Time { return riskAt.Add(time.Minute) }
	require.NoError(t, adminService.SetWorkspaceEnabled(channel.Id, workspace.UID, false))

	disabled, err := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
	require.NoError(t, err)
	assert.False(t, disabled.ManualEnabled)
	assert.Equal(t, model.OpenCodeGoStateManualDisabled, disabled.EffectiveState)
	assert.Equal(t, riskAt.Unix(), disabled.RiskDetectedAt)

	adminService.now = func() time.Time { return riskAt.Add(2 * time.Minute) }
	require.NoError(t, adminService.SetWorkspaceEnabled(channel.Id, workspace.UID, true))

	after, err := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
	require.NoError(t, err)
	assert.True(t, after.ManualEnabled)
	assert.Equal(t, model.OpenCodeGoStateRiskBlocked, after.EffectiveState)
	assert.Equal(t, string(OpenCodeGoObservationManualEnabled), after.HealthObservation)
	assert.Equal(t, riskAt.Unix(), after.RiskDetectedAt)
}

func TestOpenCodeGoAdminDeletesOwnedRowsAndRebuildsDerivedState(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	member := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "member", "workspace-member", "wrk_MEMBER1", []string{"model-a"},
	)
	nonMember := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "nonmember", "workspace-nonmember", "wrk_NONMEMBER2", []string{"model-b"},
	)
	require.NoError(t, db.Model(&model.OpenCodeGoWorkspace{}).
		Where("id = ?", nonMember.ID).
		Update("membership_status", model.OpenCodeGoMembershipInactive).Error)
	require.NoError(t, db.Create(&model.OpenCodeGoOperation{
		UID:          "operation-member",
		ChannelID:    channel.Id,
		WorkspaceID:  member.ID,
		WorkspaceUID: member.UID,
		Action:       "test",
		Status:       "succeeded",
	}).Error)
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))

	adminService := NewOpenCodeGoAccountPoolAdminService()
	deleted, err := adminService.DeleteNonMemberWorkspaces(channel.Id)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)

	var nonMemberIdentityCount int64
	require.NoError(t, db.Model(&model.OpenCodeGoIdentity{}).
		Where("id = ?", nonMember.IdentityID).
		Count(&nonMemberIdentityCount).Error)
	require.Zero(t, nonMemberIdentityCount)

	require.NoError(t, adminService.DeleteIdentity(channel.Id, "identity-member"))
	for _, table := range []any{
		&model.OpenCodeGoIdentity{},
		&model.OpenCodeGoWorkspace{},
		&model.OpenCodeGoQuotaWindow{},
		&model.OpenCodeGoWorkspaceModel{},
		&model.OpenCodeGoOperation{},
	} {
		var count int64
		require.NoError(t, db.Model(table).Count(&count).Error)
		require.Zero(t, count)
	}
	_, err = SelectOpenCodeGoWorkspace(channel.Id, "model-a")
	require.ErrorIs(t, err, ErrOpenCodeGoNoEligibleWorkspace)
}

func TestOpenCodeGoAdminUpdatesIdentityAndDeletesOneWorkspace(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"},
	)
	adminService := NewOpenCodeGoAccountPoolAdminService()
	require.NoError(t, adminService.UpdateIdentityLabel(channel.Id, "identity-one", "  Primary account  "))

	view, err := GetOpenCodeGoPoolView(channel.Id)
	require.NoError(t, err)
	require.Len(t, view.Identities, 1)
	require.Equal(t, "Primary account", view.Identities[0].Label)

	require.NoError(t, adminService.DeleteWorkspace(channel.Id, workspace.UID))
	deleted, err := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
	require.NoError(t, err)
	require.Nil(t, deleted)
	identity, err := model.GetOpenCodeGoIdentityPool(channel.Id, "identity-one")
	require.NoError(t, err)
	require.NotNil(t, identity)
	require.Empty(t, identity.Workspaces)
	for _, table := range []any{
		&model.OpenCodeGoQuotaWindow{},
		&model.OpenCodeGoWorkspaceModel{},
	} {
		var count int64
		require.NoError(t, db.Model(table).Count(&count).Error)
		require.Zero(t, count)
	}
}

func TestOpenCodeGoChannelDeleteCascadesPoolRowsTransactionally(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"},
	)
	require.NoError(t, db.Create(&model.OpenCodeGoOperation{
		UID:          "operation-one",
		ChannelID:    channel.Id,
		WorkspaceID:  workspace.ID,
		WorkspaceUID: workspace.UID,
		Action:       "test",
		Status:       "succeeded",
	}).Error)

	require.NoError(t, channel.Delete())
	for _, table := range []any{
		&model.Channel{},
		&model.OpenCodeGoIdentity{},
		&model.OpenCodeGoWorkspace{},
		&model.OpenCodeGoQuotaWindow{},
		&model.OpenCodeGoWorkspaceModel{},
		&model.OpenCodeGoOperation{},
	} {
		var count int64
		require.NoError(t, db.Model(table).Count(&count).Error)
		require.Zero(t, count)
	}
}

func TestOpenCodeGoChannelDeleteRollsBackWhenPoolCleanupFails(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"},
	)
	callbackName := "opencode_go_test_fail_workspace_model_delete"
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "open_code_go_workspace_models" {
			tx.AddError(errors.New("injected workspace-model delete failure"))
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Delete().Remove(callbackName) })

	err := channel.Delete()
	require.ErrorContains(t, err, "injected workspace-model delete failure")
	for _, table := range []any{
		&model.Channel{},
		&model.OpenCodeGoIdentity{},
		&model.OpenCodeGoWorkspace{},
		&model.OpenCodeGoQuotaWindow{},
		&model.OpenCodeGoWorkspaceModel{},
	} {
		var count int64
		require.NoError(t, db.Model(table).Count(&count).Error)
		require.NotZero(t, count)
	}
}
