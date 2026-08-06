package service

import (
	"context"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCodeGoBatchSetChinaModelsEnablesAllEligible(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	first := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_BATCH1", []string{"model-a"})
	second := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "two", "workspace-two", "wrk_BATCH2", []string{"model-a"})
	now := time.Unix(1_900_100_000, 0)

	// first: China models currently disabled (false); second: nil (unknown).
	disabled := false
	require.NoError(t, db.Model(&model.OpenCodeGoWorkspace{}).Where("id = ?", first.ID).Update("china_models_enabled", disabled).Error)

	page := newOpenCodeGoLifecyclePage(first.UpstreamWorkspaceID, 10, now)
	falseState := false
	page.ChinaModelsEnabled = &falseState
	configureOpenCodeGoLifecycleWorkspace(t, db, first, page)

	page2 := newOpenCodeGoLifecyclePage(second.UpstreamWorkspaceID, 10, now)
	configureOpenCodeGoLifecycleWorkspace(t, db, second, page2) // ChinaModelsEnabled stays nil

	fake := &fakeOpenCodeGoLifecycleBackend{models: []string{"model-a"}, enableChangesState: true}
	firstFalse := false
	secondFalse := false
	fake.pages = map[string]*OpenCodeGoConsolePage{
		first.UpstreamWorkspaceID:  {WorkspaceID: first.UpstreamWorkspaceID, ChinaModelsEnabled: &firstFalse},
		second.UpstreamWorkspaceID: {WorkspaceID: second.UpstreamWorkspaceID, ChinaModelsEnabled: &secondFalse},
	}
	service := newOpenCodeGoLifecycleTestService(codec, fake, now)

	summary, err := service.BatchSetChinaModels(context.Background(), channel.Id, nil, true, "batch")
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, 2, summary.Attempted)
	assert.Equal(t, 2, summary.Succeeded)
	assert.Zero(t, summary.Failed)
	assert.Zero(t, summary.Skipped)
	for _, result := range summary.Results {
		assert.Equal(t, "ok", result.Status)
	}
}

func TestOpenCodeGoBatchSetChinaModelsDisablesEligible(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	first := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_BATCH3", []string{"model-a"})
	now := time.Unix(1_900_100_000, 0)

	enabled := true
	require.NoError(t, db.Model(&model.OpenCodeGoWorkspace{}).Where("id = ?", first.ID).Update("china_models_enabled", enabled).Error)
	page := newOpenCodeGoLifecyclePage(first.UpstreamWorkspaceID, 10, now)
	page.ChinaModelsEnabled = &enabled
	configureOpenCodeGoLifecycleWorkspace(t, db, first, page)

	fake := &fakeOpenCodeGoLifecycleBackend{page: page, models: []string{"model-a"}}
	service := newOpenCodeGoLifecycleTestService(codec, fake, now)

	summary, err := service.BatchSetChinaModels(context.Background(), channel.Id, []string{first.UID}, false, "batch")
	require.NoError(t, err)
	require.Len(t, summary.Results, 1)
	assert.Equal(t, "ok", summary.Results[0].Status)
	assert.Equal(t, 1, summary.Succeeded)

	workspace, err := model.GetOpenCodeGoWorkspace(channel.Id, first.UID)
	require.NoError(t, err)
	require.NotNil(t, workspace.ChinaModelsEnabled)
	assert.False(t, *workspace.ChinaModelsEnabled)
}

func TestOpenCodeGoBatchCancelSubscriptionRenewalSkipsAlreadyCancelled(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	first := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_BATCH4", []string{"model-a"})
	now := time.Unix(1_900_100_000, 0)

	page := newOpenCodeGoLifecyclePage(first.UpstreamWorkspaceID, 10, now)
	page.SubscriptionReference = "sub_test"
	configureOpenCodeGoLifecycleWorkspace(t, db, first, page)
	require.NoError(t, db.Model(&model.OpenCodeGoWorkspace{}).Where("id = ?", first.ID).Update("subscription_reference", "sub_test").Error)

	fake := &fakeOpenCodeGoLifecycleBackend{models: []string{"model-a"}}
	fakePage := newOpenCodeGoLifecyclePage(first.UpstreamWorkspaceID, 10, now)
	fakePage.SubscriptionReference = "sub_test"
	fake.page = fakePage
	fake.cancelResult = OpenCodeGoSubscriptionCancellation{CurrentPeriodEnd: now.Add(time.Hour).Unix()}
	service := newOpenCodeGoLifecycleTestService(codec, fake, now)

	summary, err := service.BatchCancelSubscriptionRenewal(context.Background(), channel.Id, []string{first.UID}, "batch")
	require.NoError(t, err)
	require.Len(t, summary.Results, 1)
	assert.Equal(t, "ok", summary.Results[0].Status)

	already := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "two", "workspace-two", "wrk_BATCH5", []string{"model-a"})
	require.NoError(t, db.Model(&model.OpenCodeGoWorkspace{}).Where("id = ?", already.ID).
		Updates(map[string]interface{}{"subscription_reference": "sub_done", "renewal_cancelled_at": now.Unix()}).Error)
	summary2, err := service.BatchCancelSubscriptionRenewal(context.Background(), channel.Id, []string{already.UID}, "batch")
	require.NoError(t, err)
	require.Len(t, summary2.Results, 1)
	assert.Equal(t, "skipped", summary2.Results[0].Status)
}

func TestOpenCodeGoRunIdentityAutomationsEnablesChinaModelsOnNilState(t *testing.T) {
	t.Setenv("OPENCODE_GO_LIFECYCLE_AUTOMATION_ENABLED", "true")
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	first := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_BATCH6", []string{"model-a"})
	now := time.Unix(1_900_100_000, 0)

	// ChinaModelsEnabled stays nil on the workspace (new import default).
	page := newOpenCodeGoLifecyclePage(first.UpstreamWorkspaceID, 10, now)
	configureOpenCodeGoLifecycleWorkspace(t, db, first, page)

	enabledPolicy := false
	fakePage := newOpenCodeGoLifecyclePage(first.UpstreamWorkspaceID, 10, now)
	fakePage.ChinaModelsEnabled = &enabledPolicy
	fake := &fakeOpenCodeGoLifecycleBackend{page: fakePage, models: []string{"model-a"}, enableChangesState: true}
	service := newOpenCodeGoLifecycleTestService(codec, fake, now)

	summary, err := service.RunIdentityAutomations(context.Background(), channel.Id, "identity-one", "test")
	require.NoError(t, err)
	assert.True(t, summary.Enabled)
	require.Equal(t, 1, fake.enableCalls)

	workspace, err := model.GetOpenCodeGoWorkspace(channel.Id, first.UID)
	require.NoError(t, err)
	require.NotNil(t, workspace.ChinaModelsEnabled)
	assert.True(t, *workspace.ChinaModelsEnabled)
}

func boolPtr(value bool) *bool {
	return &value
}
