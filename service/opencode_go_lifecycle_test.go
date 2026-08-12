package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaydto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type fakeOpenCodeGoLifecycleBackend struct {
	mutex               sync.Mutex
	page                *OpenCodeGoConsolePage
	pages               map[string]*OpenCodeGoConsolePage
	fetchPages          []*OpenCodeGoConsolePage
	fetchWorkspaceCalls int
	discoverCalls       int
	enableCalls         int
	referralCalls       int
	cancelCalls         int
	enableErr           error
	referralErr         error
	cancelErr           error
	enableChangesState  bool
	cancelResult        OpenCodeGoSubscriptionCancellation
	apiKey              string
	models              []string
}

func (fake *fakeOpenCodeGoLifecycleBackend) FetchWorkspacePage(
	_ context.Context,
	_ string,
	workspaceID string,
) (*OpenCodeGoConsolePage, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.fetchWorkspaceCalls++
	if len(fake.fetchPages) > 0 {
		page := fake.fetchPages[0]
		fake.fetchPages = fake.fetchPages[1:]
		if page == nil || page.WorkspaceID != workspaceID {
			return nil, errors.New("synthetic workspace page is unavailable")
		}
		return cloneOpenCodeGoLifecyclePage(page), nil
	}
	if page, ok := fake.pages[workspaceID]; ok {
		return cloneOpenCodeGoLifecyclePage(page), nil
	}
	if fake.page == nil || fake.page.WorkspaceID != workspaceID {
		return nil, errors.New("synthetic workspace page is unavailable")
	}
	return cloneOpenCodeGoLifecyclePage(fake.page), nil
}

func (fake *fakeOpenCodeGoLifecycleBackend) DiscoverWorkspacePages(
	_ context.Context,
	_ string,
	_ string,
) ([]OpenCodeGoWorkspacePageResult, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.discoverCalls++
	if fake.page == nil {
		return nil, errors.New("synthetic workspace discovery is unavailable")
	}
	page := cloneOpenCodeGoLifecyclePage(fake.page)
	return []OpenCodeGoWorkspacePageResult{{
		Workspace: OpenCodeGoDiscoveredWorkspace{ID: page.WorkspaceID, Name: page.WorkspaceName},
		Page:      page,
	}}, nil
}

func (fake *fakeOpenCodeGoLifecycleBackend) FetchAPIKey(_ context.Context, _ string, _ string) (string, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return fake.apiKey, nil
}

func (fake *fakeOpenCodeGoLifecycleBackend) FetchModels(_ context.Context, _ string) ([]string, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return append([]string(nil), fake.models...), nil
}

func (fake *fakeOpenCodeGoLifecycleBackend) EnableChinaModels(
	_ context.Context,
	_ string,
	page *OpenCodeGoConsolePage,
) error {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.enableCalls++
	if fake.enableErr != nil {
		return fake.enableErr
	}
	if fake.enableChangesState {
		enabled := true
		if fake.page != nil {
			fake.page.ChinaModelsEnabled = &enabled
		}
		if fake.pages != nil && page != nil {
			fake.pages[page.WorkspaceID] = &OpenCodeGoConsolePage{WorkspaceID: page.WorkspaceID, ChinaModelsEnabled: &enabled}
		}
	}
	return nil
}

func (fake *fakeOpenCodeGoLifecycleBackend) DisableChinaModels(
	_ context.Context,
	_ string,
	page *OpenCodeGoConsolePage,
) error {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.enableErr != nil {
		return fake.enableErr
	}
	enabled := false
	if fake.page != nil {
		fake.page.ChinaModelsEnabled = &enabled
	}
	if fake.pages != nil && page != nil {
		fake.pages[page.WorkspaceID] = &OpenCodeGoConsolePage{WorkspaceID: page.WorkspaceID, ChinaModelsEnabled: &enabled}
	}
	return nil
}

func (fake *fakeOpenCodeGoLifecycleBackend) ApplyReferralReward(
	_ context.Context,
	_ string,
	_ *OpenCodeGoConsolePage,
	rewardID string,
) (int, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.referralCalls++
	if fake.referralErr != nil {
		return 0, fake.referralErr
	}
	remaining := make([]string, 0, len(fake.page.AvailableReferralRewardIDs))
	removed := false
	for _, candidate := range fake.page.AvailableReferralRewardIDs {
		if !removed && candidate == rewardID {
			removed = true
			fake.page.UsedReferralRewardIDs = append(fake.page.UsedReferralRewardIDs, candidate)
			continue
		}
		remaining = append(remaining, candidate)
	}
	fake.page.AvailableReferralRewardIDs = remaining
	fake.page.AvailableReferralRewards = len(remaining)
	fake.page.UsedReferralRewards = len(fake.page.UsedReferralRewardIDs)
	return 500, nil
}

func (fake *fakeOpenCodeGoLifecycleBackend) CancelSubscriptionRenewal(
	_ context.Context,
	_ string,
	_ *OpenCodeGoConsolePage,
) (OpenCodeGoSubscriptionCancellation, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.cancelCalls++
	return fake.cancelResult, fake.cancelErr
}

func cloneOpenCodeGoLifecyclePage(page *OpenCodeGoConsolePage) *OpenCodeGoConsolePage {
	if page == nil {
		return nil
	}
	cloned := *page
	cloned.Workspaces = append([]OpenCodeGoDiscoveredWorkspace(nil), page.Workspaces...)
	cloned.AvailableReferralRewardIDs = append([]string(nil), page.AvailableReferralRewardIDs...)
	cloned.UsedReferralRewardIDs = append([]string(nil), page.UsedReferralRewardIDs...)
	cloned.RouteModuleAssets = append([]string(nil), page.RouteModuleAssets...)
	if page.ChinaModelsEnabled != nil {
		enabled := *page.ChinaModelsEnabled
		cloned.ChinaModelsEnabled = &enabled
	}
	if page.Quota != nil {
		quota := *page.Quota
		quota.Windows = append([]OpenCodeGoAuthoritativeQuotaWindow(nil), page.Quota.Windows...)
		cloned.Quota = &quota
	}
	return &cloned
}

func newOpenCodeGoLifecycleTestService(
	codec *OpenCodeGoCredentialCodec,
	fake *fakeOpenCodeGoLifecycleBackend,
	now time.Time,
) *OpenCodeGoLifecycleService {
	pool := newOpenCodeGoAccountPoolService(fake, codec)
	pool.now = func() time.Time { return now }
	pool.rebuild = nil
	service := newOpenCodeGoLifecycleService(fake, fake, pool)
	service.now = func() time.Time { return now }
	return service
}

func configureOpenCodeGoLifecycleWorkspace(
	t *testing.T,
	db *gorm.DB,
	workspace model.OpenCodeGoWorkspace,
	page *OpenCodeGoConsolePage,
) {
	t.Helper()
	state := model.OpenCodeGoStateEligible
	if openCodeGoPageHasExhaustedQuota(page) {
		state = model.OpenCodeGoStateQuotaExhausted
	}
	require.NoError(t, db.Model(&model.OpenCodeGoWorkspace{}).
		Where("id = ?", workspace.ID).
		Updates(map[string]interface{}{
			"membership_status":          page.MembershipStatus,
			"subscription_reference":     page.SubscriptionReference,
			"effective_state":            state,
			"quota_snapshot_status":      model.OpenCodeGoQuotaSnapshotComplete,
			"quota_fetched_at":           page.Quota.FetchedAt,
			"quota_next_refresh_at":      page.Quota.NextRefreshAt,
			"china_models_enabled":       page.ChinaModelsEnabled,
			"available_referral_rewards": page.AvailableReferralRewards,
			"used_referral_rewards":      page.UsedReferralRewards,
		}).Error)
	require.NoError(t, db.Where("workspace_id = ?", workspace.ID).Delete(&model.OpenCodeGoQuotaWindow{}).Error)
	for _, window := range page.Quota.Windows {
		require.NoError(t, db.Create(&model.OpenCodeGoQuotaWindow{
			WorkspaceID:  workspace.ID,
			Kind:         window.Kind,
			UsedPercent:  window.UsedPercent,
			ResetSeconds: window.ResetSeconds,
			ResetAt:      window.ResetAt,
			FetchedAt:    window.FetchedAt,
		}).Error)
	}
}

func newOpenCodeGoLifecyclePage(workspaceID string, usedPercent float64, now time.Time) *OpenCodeGoConsolePage {
	page := completeOpenCodeGoConsolePage(workspaceID, usedPercent, now.Unix())
	page.WorkspaceName = "Lifecycle workspace"
	page.Workspaces = []OpenCodeGoDiscoveredWorkspace{{ID: workspaceID, Name: page.WorkspaceName}}
	return page
}

func TestOpenCodeGoLifecycleAutomationMasterSwitchDefaultsOff(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "automation-off", "workspace-automation-off", "wrk_AUTOMATION1", []string{"model-a"},
	)
	now := time.Unix(1_900_100_000, 0)
	page := newOpenCodeGoLifecyclePage(workspace.UpstreamWorkspaceID, 10, now)
	disabled := false
	page.ChinaModelsEnabled = &disabled
	configureOpenCodeGoLifecycleWorkspace(t, db, workspace, page)
	fake := &fakeOpenCodeGoLifecycleBackend{page: page, enableChangesState: true, models: []string{"model-a"}}
	service := newOpenCodeGoLifecycleTestService(codec, fake, now)
	t.Setenv("OPENCODE_GO_LIFECYCLE_AUTOMATION_ENABLED", "false")

	summary, err := service.RunIdentityAutomations(context.Background(), channel.Id, "identity-automation-off", "scheduled")
	require.NoError(t, err)
	assert.False(t, summary.Enabled)
	assert.Zero(t, summary.Attempted)
	assert.Zero(t, fake.fetchWorkspaceCalls)
	assert.Zero(t, fake.discoverCalls)
	assert.Zero(t, fake.enableCalls)
	assert.Zero(t, fake.referralCalls)
	assert.Zero(t, fake.cancelCalls)
}

func TestOpenCodeGoLifecycleAutomationMasterSwitchOptionOverridesEnv(t *testing.T) {
	// A DB-backed option must take precedence over the deployment env value.
	snapshot := snapshotOpenCodeGoLifecycleAutomationOption(t)
	t.Cleanup(func() { restoreOpenCodeGoLifecycleAutomationOption(t, snapshot) })
	common.OptionMapRWMutex.Lock()
	common.OptionMap["OpenCodeGoLifecycleAutomationEnabled"] = "true"
	common.OptionMapRWMutex.Unlock()
	t.Setenv("OPENCODE_GO_LIFECYCLE_AUTOMATION_ENABLED", "false")
	assert.True(t, openCodeGoLifecycleAutomationEnabled())
}

func TestOpenCodeGoLifecycleAutomationMasterSwitchEnvFallbackWithoutOption(t *testing.T) {
	// Without a DB option the env value must be honored unchanged.
	snapshot := snapshotOpenCodeGoLifecycleAutomationOption(t)
	t.Cleanup(func() { restoreOpenCodeGoLifecycleAutomationOption(t, snapshot) })
	common.OptionMapRWMutex.Lock()
	if common.OptionMap == nil {
		common.OptionMap = map[string]string{}
	}
	delete(common.OptionMap, "OpenCodeGoLifecycleAutomationEnabled")
	common.OptionMapRWMutex.Unlock()
	t.Setenv("OPENCODE_GO_LIFECYCLE_AUTOMATION_ENABLED", "true")
	assert.True(t, openCodeGoLifecycleAutomationEnabled())
}

func TestOpenCodeGoLifecycleAutomationMasterSwitchMalformedOptionFallsBackToEnv(t *testing.T) {
	// An unparseable DB value must fall back to the env default rather than
	// silently disabling automation.
	snapshot := snapshotOpenCodeGoLifecycleAutomationOption(t)
	t.Cleanup(func() { restoreOpenCodeGoLifecycleAutomationOption(t, snapshot) })
	common.OptionMapRWMutex.Lock()
	common.OptionMap["OpenCodeGoLifecycleAutomationEnabled"] = "not-a-bool"
	common.OptionMapRWMutex.Unlock()
	t.Setenv("OPENCODE_GO_LIFECYCLE_AUTOMATION_ENABLED", "true")
	assert.True(t, openCodeGoLifecycleAutomationEnabled())
}

// openCodeGoLifecycleAutomationOptionSnapshot captures the previous value of
// the master-switch option (if any) so a test can restore it afterwards.
type openCodeGoLifecycleAutomationOptionSnapshot struct {
	value string
	had   bool
}

// snapshotOpenCodeGoLifecycleAutomationOption ensures common.OptionMap is
// non-nil (it may be nil in the test binary since InitOptionMap only runs in
// main) and records the previous option value.
func snapshotOpenCodeGoLifecycleAutomationOption(t *testing.T) openCodeGoLifecycleAutomationOptionSnapshot {
	t.Helper()
	common.OptionMapRWMutex.Lock()
	defer common.OptionMapRWMutex.Unlock()
	if common.OptionMap == nil {
		common.OptionMap = map[string]string{}
	}
	previous, had := common.OptionMap["OpenCodeGoLifecycleAutomationEnabled"]
	return openCodeGoLifecycleAutomationOptionSnapshot{value: previous, had: had}
}

// restoreOpenCodeGoLifecycleAutomationOption puts the captured option value
// back, removing it if it was absent before the test.
func restoreOpenCodeGoLifecycleAutomationOption(t *testing.T, snapshot openCodeGoLifecycleAutomationOptionSnapshot) {
	t.Helper()
	common.OptionMapRWMutex.Lock()
	defer common.OptionMapRWMutex.Unlock()
	if snapshot.had {
		common.OptionMap["OpenCodeGoLifecycleAutomationEnabled"] = snapshot.value
	} else {
		delete(common.OptionMap, "OpenCodeGoLifecycleAutomationEnabled")
	}
}

func TestOpenCodeGoLifecyclePolicyDefaultsAndUpdatePreserveProtocolSettings(t *testing.T) {
	db, channel, _ := setupOpenCodeGoPoolTestDB(t)
	t.Setenv("OPENCODE_GO_LIFECYCLE_AUTOMATION_ENABLED", "false")
	settings := channel.GetOtherSettings()
	settings.OpenCodeGo = &relaydto.OpenCodeGoConfig{
		DefaultProtocol: relaydto.OpenCodeGoProtocolMessages,
		ModelProtocols:  map[string]string{"model-a": relaydto.OpenCodeGoProtocolResponses},
	}
	encoded, err := common.Marshal(settings)
	require.NoError(t, err)
	var encodedSettings map[string]any
	require.NoError(t, common.Unmarshal(encoded, &encodedSettings))
	encodedSettings["future_root"] = map[string]any{"enabled": true}
	encodedSettings["opencode_go"].(map[string]any)["future_nested"] = map[string]any{"mode": "keep"}
	encoded, err = common.Marshal(encodedSettings)
	require.NoError(t, err)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("settings", string(encoded)).Error)

	defaults, err := GetOpenCodeGoLifecyclePolicy(channel.Id)
	require.NoError(t, err)
	assert.False(t, defaults.AutomationEnabled)
	assert.True(t, defaults.AutoEnableChinaModels)
	assert.True(t, defaults.AutoApplyReferralRewards)
	assert.Equal(t, OpenCodeGoDefaultReferralRewardsPerRun, defaults.ReferralRewardsMaxPerRun)
	assert.False(t, defaults.AutoCancelSubscriptionRenewal)

	updated, err := UpdateOpenCodeGoLifecyclePolicy(channel.Id, OpenCodeGoLifecyclePolicy{
		AutomationEnabled:             true,
		AutoEnableChinaModels:         false,
		AutoApplyReferralRewards:      false,
		ReferralRewardsMaxPerRun:      7,
		AutoCancelSubscriptionRenewal: true,
	})
	require.NoError(t, err)
	assert.False(t, updated.AutomationEnabled, "the channel policy cannot override the environment master switch")
	assert.False(t, updated.AutoEnableChinaModels)
	assert.False(t, updated.AutoApplyReferralRewards)
	assert.Equal(t, 7, updated.ReferralRewardsMaxPerRun)
	assert.True(t, updated.AutoCancelSubscriptionRenewal)

	reloaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	persisted := reloaded.GetOtherSettings().OpenCodeGo
	require.NotNil(t, persisted)
	assert.Equal(t, relaydto.OpenCodeGoProtocolMessages, persisted.DefaultProtocol)
	assert.Equal(t, relaydto.OpenCodeGoProtocolResponses, persisted.ModelProtocols["model-a"])
	var persistedSettings map[string]any
	require.NoError(t, common.Unmarshal([]byte(reloaded.OtherSettings), &persistedSettings))
	assert.Equal(t, map[string]any{"enabled": true}, persistedSettings["future_root"])
	assert.Equal(t, map[string]any{"mode": "keep"}, persistedSettings["opencode_go"].(map[string]any)["future_nested"])

	zeroLimit, err := UpdateOpenCodeGoLifecyclePolicy(channel.Id, OpenCodeGoLifecyclePolicy{
		ReferralRewardsMaxPerRun: 0,
	})
	require.NoError(t, err)
	assert.Zero(t, zeroLimit.ReferralRewardsMaxPerRun)
	reloaded, err = model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	require.NotNil(t, reloaded.GetOtherSettings().OpenCodeGo.ReferralRewardsMaxPerRun)
	assert.Zero(t, *reloaded.GetOtherSettings().OpenCodeGo.ReferralRewardsMaxPerRun)

	_, err = UpdateOpenCodeGoLifecyclePolicy(channel.Id, OpenCodeGoLifecyclePolicy{ReferralRewardsMaxPerRun: 21})
	require.ErrorContains(t, err, "between 0 and 20")
}

func TestOpenCodeGoRefreshAutomationRunsOnlyAfterSuccessfulRefreshResults(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "refresh-automation", "workspace-refresh-automation", "wrk_AUTOMATION2", []string{"model-a"},
	)
	now := time.Unix(1_900_100_000, 0)
	page := newOpenCodeGoLifecyclePage(workspace.UpstreamWorkspaceID, 10, now)
	disabled := false
	page.ChinaModelsEnabled = &disabled
	configureOpenCodeGoLifecycleWorkspace(t, db, workspace, page)
	fake := &fakeOpenCodeGoLifecycleBackend{
		page:               page,
		models:             []string{"model-a"},
		enableChangesState: true,
	}
	service := newOpenCodeGoLifecycleTestService(codec, fake, now)
	refreshResults := []OpenCodeGoRefreshResult{
		{ChannelID: channel.Id, IdentityUID: "identity-refresh-automation", Status: "refreshed"},
		{ChannelID: channel.Id, IdentityUID: "identity-skipped", Status: "error"},
	}

	t.Setenv("OPENCODE_GO_LIFECYCLE_AUTOMATION_ENABLED", "false")
	disabledSummary, err := service.RunRefreshAutomations(context.Background(), refreshResults, "refresh_task")
	require.NoError(t, err)
	assert.False(t, disabledSummary.Enabled)
	assert.Zero(t, fake.enableCalls)

	t.Setenv("OPENCODE_GO_LIFECYCLE_AUTOMATION_ENABLED", "true")
	enabledSummary, err := service.RunRefreshAutomations(context.Background(), refreshResults, "refresh_task")
	require.NoError(t, err)
	assert.True(t, enabledSummary.Enabled)
	assert.Equal(t, 1, enabledSummary.Identities)
	assert.Equal(t, 1, enabledSummary.Attempted)
	assert.Equal(t, 1, enabledSummary.Succeeded)
	assert.Zero(t, enabledSummary.Failed)
	assert.Equal(t, 1, fake.enableCalls)
}

func TestOpenCodeGoRefreshAndLifecycleSummariesDoNotSerializeIdentityUID(t *testing.T) {
	const identityUID = "123e4567-e89b-42d3-a456-426614174000"
	summary := OpenCodeGoRefreshSummary{
		Total:     1,
		Processed: 1,
		Succeeded: 1,
		Results: []OpenCodeGoRefreshResult{{
			ChannelID:   7,
			IdentityUID: identityUID,
			Status:      "refreshed",
		}},
		Lifecycle: OpenCodeGoLifecycleBatchSummary{
			Enabled:    true,
			Identities: 1,
			Results: []OpenCodeGoLifecycleIdentityAutomationResult{{
				ChannelID: 7,
			}},
		},
	}

	encoded, err := common.Marshal(summary)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "identity_uid")
	assert.NotContains(t, string(encoded), identityUID)
	assert.Contains(t, string(encoded), `"status":"refreshed"`)
}

func TestOpenCodeGoRefreshAutomationsScopeRuntimeByResultChannel(t *testing.T) {
	db, firstChannel, codec := setupOpenCodeGoPoolTestDB(t)
	secondChannel := &model.Channel{
		Type:   constant.ChannelTypeOpenCodeGo,
		Key:    "",
		Name:   "OpenCode Go second lifecycle channel",
		Status: common.ChannelStatusEnabled,
		Models: "model-a",
		Group:  "default",
	}
	require.NoError(t, db.Create(secondChannel).Error)
	firstWorkspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, firstChannel.Id, "lifecycle-first", "workspace-lifecycle-first", "wrk_LIFEFIRST1", []string{"model-a"},
	)
	secondWorkspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, secondChannel.Id, "lifecycle-second", "workspace-lifecycle-second", "wrk_LIFESECOND2", []string{"model-a"},
	)
	enabled := false
	require.NoError(t, db.Model(&model.OpenCodeGoWorkspace{}).
		Where("id IN ?", []int64{firstWorkspace.ID, secondWorkspace.ID}).
		Update("china_models_enabled", enabled).Error)

	snapshot := snapshotOpenCodeGoLifecycleAutomationOption(t)
	t.Cleanup(func() { restoreOpenCodeGoLifecycleAutomationOption(t, snapshot) })
	common.OptionMapRWMutex.Lock()
	delete(common.OptionMap, "OpenCodeGoLifecycleAutomationEnabled")
	common.OptionMapRWMutex.Unlock()
	t.Setenv("OPENCODE_GO_LIFECYCLE_AUTOMATION_ENABLED", "true")

	var factoryMutex sync.Mutex
	factoryCalls := make(map[string]int)
	root := &OpenCodeGoLifecycleService{
		codec: codec,
		scopedFactory: func(channelID int, identityUID string) (*OpenCodeGoLifecycleService, error) {
			factoryMutex.Lock()
			factoryCalls[fmt.Sprintf("%d:%s", channelID, identityUID)]++
			factoryMutex.Unlock()
			workspaceID := firstWorkspace.UpstreamWorkspaceID
			if channelID == secondChannel.Id {
				workspaceID = secondWorkspace.UpstreamWorkspaceID
			}
			page := newOpenCodeGoLifecyclePage(workspaceID, 10, time.Unix(1_900_100_000, 0))
			page.ChinaModelsEnabled = boolPtr(false)
			fake := &fakeOpenCodeGoLifecycleBackend{
				page:               page,
				models:             []string{"model-a"},
				enableChangesState: true,
			}
			return newOpenCodeGoLifecycleTestService(codec, fake, time.Unix(1_900_100_000, 0)), nil
		},
		now: time.Now,
	}

	summary, err := root.RunRefreshAutomations(
		context.Background(),
		[]OpenCodeGoRefreshResult{
			{ChannelID: firstChannel.Id, IdentityUID: "identity-lifecycle-first", Status: "refreshed"},
			{ChannelID: secondChannel.Id, IdentityUID: "identity-lifecycle-second", Status: "refreshed"},
		},
		"refresh_task",
	)
	require.NoError(t, err)
	assert.Equal(t, 2, summary.Identities)
	assert.Equal(t, 2, summary.Succeeded)
	assert.Equal(t, map[string]int{
		fmt.Sprintf("%d:%s", firstChannel.Id, "identity-lifecycle-first"):   1,
		fmt.Sprintf("%d:%s", secondChannel.Id, "identity-lifecycle-second"): 1,
	}, factoryCalls)
}

func TestOpenCodeGoEnableChinaModelsIsIdempotentAndReturnsFinishedOperation(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "china-enabled", "workspace-china-enabled", "wrk_CHINA1", []string{"model-a"},
	)
	now := time.Unix(1_900_100_000, 0)
	page := newOpenCodeGoLifecyclePage(workspace.UpstreamWorkspaceID, 10, now)
	enabled := true
	page.ChinaModelsEnabled = &enabled
	configureOpenCodeGoLifecycleWorkspace(t, db, workspace, page)
	fake := &fakeOpenCodeGoLifecycleBackend{page: page, models: []string{"model-a"}}
	service := newOpenCodeGoLifecycleTestService(codec, fake, now)

	operation, err := service.EnableChinaModels(context.Background(), channel.Id, workspace.UID, "manual")
	require.NoError(t, err)
	require.NotNil(t, operation)
	assert.Equal(t, OpenCodeGoOperationStatusSucceeded, operation.Status)
	assert.Equal(t, now.Unix(), operation.FinishedAt)
	assert.Zero(t, fake.enableCalls)
	assert.Equal(t, 1, fake.fetchWorkspaceCalls)
}

func TestOpenCodeGoEnableChinaModelsRequiresPostMutationVerification(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "china-verify", "workspace-china-verify", "wrk_CHINA2", []string{"model-a"},
	)
	now := time.Unix(1_900_100_000, 0)
	page := newOpenCodeGoLifecyclePage(workspace.UpstreamWorkspaceID, 10, now)
	disabled := false
	page.ChinaModelsEnabled = &disabled
	configureOpenCodeGoLifecycleWorkspace(t, db, workspace, page)
	fake := &fakeOpenCodeGoLifecycleBackend{page: page, models: []string{"model-a"}}
	service := newOpenCodeGoLifecycleTestService(codec, fake, now)

	operation, err := service.EnableChinaModels(context.Background(), channel.Id, workspace.UID, "manual")
	require.ErrorContains(t, err, "could not be verified")
	require.NotNil(t, operation)
	assert.Equal(t, OpenCodeGoOperationStatusFailed, operation.Status)
	assert.Equal(t, 1, fake.enableCalls)
	assert.Equal(t, 2, fake.fetchWorkspaceCalls)

	updated, loadErr := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
	require.NoError(t, loadErr)
	require.NotNil(t, updated)
	require.NotNil(t, updated.ChinaModelsEnabled)
	assert.False(t, *updated.ChinaModelsEnabled)
	assert.Contains(t, updated.ChinaModelsError, "could not be verified")
}

func TestOpenCodeGoReferralRewardsRefreshQuotaAfterEverySuccessAndRespectLimit(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "rewards", "workspace-rewards", "wrk_REWARDS1", []string{"model-a"},
	)
	now := time.Unix(1_900_100_000, 0)
	page := newOpenCodeGoLifecyclePage(workspace.UpstreamWorkspaceID, 100, now)
	page.AvailableReferralRewardIDs = []string{"ref_FIRST", "ref_SECOND", "ref_THIRD"}
	page.AvailableReferralRewards = len(page.AvailableReferralRewardIDs)
	configureOpenCodeGoLifecycleWorkspace(t, db, workspace, page)
	fake := &fakeOpenCodeGoLifecycleBackend{
		page:   page,
		apiKey: "synthetic-lifecycle-key",
		models: []string{"model-a"},
	}
	service := newOpenCodeGoLifecycleTestService(codec, fake, now)
	refreshStep := int64(0)
	service.pool.now = func() time.Time {
		refreshStep++
		return now.Add(time.Duration(refreshStep))
	}

	summary, err := service.ApplyReferralRewards(context.Background(), channel.Id, workspace.UID, "manual", 2)
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoReferralApplySummary{Attempted: 2, Applied: 2}, summary)
	assert.Equal(t, 2, fake.referralCalls)
	assert.Equal(t, 4, fake.fetchWorkspaceCalls, "each reward must have one preflight GET and one verification GET")
	assert.Zero(t, fake.discoverCalls)
	assert.Len(t, fake.page.AvailableReferralRewardIDs, 1)

	updated, loadErr := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
	require.NoError(t, loadErr)
	require.NotNil(t, updated)
	assert.Equal(t, 1, updated.AvailableReferralRewards)
	assert.Equal(t, model.OpenCodeGoStateQuotaExhausted, updated.EffectiveState)
	assert.Equal(t, now.Unix(), updated.ReferralRewardAppliedAt)

	var operations []model.OpenCodeGoOperation
	require.NoError(t, db.Order("id asc").Find(&operations).Error)
	require.Len(t, operations, 2)
	for _, operation := range operations {
		assert.Equal(t, OpenCodeGoOperationStatusSucceeded, operation.Status)
	}
}

func TestOpenCodeGoManualReferralRewardAppliesAtAnyUsagePercentage(t *testing.T) {
	for _, usedPercent := range []float64{0, 4, 31, 99, 100} {
		t.Run(fmt.Sprintf("used_%.0f_percent", usedPercent), func(t *testing.T) {
			db, channel, codec := setupOpenCodeGoPoolTestDB(t)
			workspace := createEligibleOpenCodeGoWorkspace(
				t,
				db,
				codec,
				channel.Id,
				fmt.Sprintf("manual-reward-%.0f", usedPercent),
				fmt.Sprintf("workspace-manual-reward-%.0f", usedPercent),
				fmt.Sprintf("wrk_MANUAL%.0f", usedPercent),
				[]string{"model-a"},
			)
			now := time.Unix(1_900_100_000, 0)
			page := newOpenCodeGoLifecyclePage(workspace.UpstreamWorkspaceID, usedPercent, now)
			page.AvailableReferralRewardIDs = []string{"ref_MANUAL"}
			page.AvailableReferralRewards = 1
			configureOpenCodeGoLifecycleWorkspace(t, db, workspace, page)
			fake := &fakeOpenCodeGoLifecycleBackend{page: page, models: []string{"model-a"}}
			service := newOpenCodeGoLifecycleTestService(codec, fake, now)

			summary, err := service.ApplyReferralReward(context.Background(), channel.Id, workspace.UID, "manual")
			require.NoError(t, err)
			assert.Equal(t, OpenCodeGoReferralApplySummary{Attempted: 1, Applied: 1}, summary)
			assert.Equal(t, 1, fake.referralCalls)
			assert.Equal(t, 2, fake.fetchWorkspaceCalls)
		})
	}
}

func TestOpenCodeGoManualReferralRewardRequiresAvailableReward(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "manual-no-reward", "workspace-manual-no-reward", "wrk_MANUALNONE", []string{"model-a"},
	)
	now := time.Unix(1_900_100_000, 0)
	page := newOpenCodeGoLifecyclePage(workspace.UpstreamWorkspaceID, 31, now)
	configureOpenCodeGoLifecycleWorkspace(t, db, workspace, page)
	fake := &fakeOpenCodeGoLifecycleBackend{page: page, models: []string{"model-a"}}
	service := newOpenCodeGoLifecycleTestService(codec, fake, now)

	summary, err := service.ApplyReferralReward(context.Background(), channel.Id, workspace.UID, "manual")
	require.ErrorIs(t, err, ErrOpenCodeGoReferralRewardUnavailable)
	assert.Equal(t, OpenCodeGoReferralApplySummary{}, summary)
	assert.Zero(t, fake.referralCalls)

	var operationCount int64
	require.NoError(t, db.Model(&model.OpenCodeGoOperation{}).Where("workspace_id = ?", workspace.ID).Count(&operationCount).Error)
	assert.Zero(t, operationCount)
}

func TestOpenCodeGoManualReferralRewardEligibilityUsesCookieAndUnexpiredMembership(t *testing.T) {
	now := time.Unix(1_900_100_000, 0)
	identity := model.OpenCodeGoIdentity{
		Status:               model.OpenCodeGoIdentityStatusActive,
		AuthCookieCiphertext: "ciphertext",
	}
	workspace := model.OpenCodeGoWorkspace{
		ManualEnabled:      true,
		MembershipStatus:   model.OpenCodeGoMembershipActive,
		EffectiveState:     model.OpenCodeGoStateKeyError,
		SubscriptionEndsAt: now.Add(time.Minute).Unix(),
	}
	require.True(t, openCodeGoReferralRewardEligibleAt(identity, workspace, now), "inference key errors must not block Cookie-authenticated rewards")

	workspace.AvailableReferralRewards = 0
	require.True(t, openCodeGoReferralRewardEligibleAt(identity, workspace, now), "a fresh console GET owns reward availability")

	workspace.SubscriptionEndsAt = now.Unix()
	require.False(t, openCodeGoReferralRewardEligibleAt(identity, workspace, now))
	workspace.SubscriptionEndsAt = now.Add(-time.Second).Unix()
	require.False(t, openCodeGoReferralRewardEligibleAt(identity, workspace, now))
}

func TestOpenCodeGoAutomaticReferralRewardStillRequiresExhaustedQuota(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "automatic-non-exhausted", "workspace-automatic-non-exhausted", "wrk_AUTONOTFULL", []string{"model-a"},
	)
	now := time.Unix(1_900_100_000, 0)
	page := newOpenCodeGoLifecyclePage(workspace.UpstreamWorkspaceID, 31, now)
	page.AvailableReferralRewardIDs = []string{"ref_AUTOMATIC"}
	page.AvailableReferralRewards = 1
	configureOpenCodeGoLifecycleWorkspace(t, db, workspace, page)
	fake := &fakeOpenCodeGoLifecycleBackend{page: page, models: []string{"model-a"}}
	service := newOpenCodeGoLifecycleTestService(codec, fake, now)

	summary, err := service.ApplyReferralRewards(context.Background(), channel.Id, workspace.UID, "scheduled", 1)
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoReferralApplySummary{}, summary)
	assert.Zero(t, fake.referralCalls)
}

func TestOpenCodeGoReferralRewardRejectsInvalidAppliedPostStates(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*OpenCodeGoConsolePage)
	}{
		{
			name: "reward still available",
			mutate: func(page *OpenCodeGoConsolePage) {
				page.AvailableReferralRewardIDs = []string{"ref_UNVERIFIED"}
				page.AvailableReferralRewards = 1
				page.UsedReferralRewardIDs = []string{"ref_UNVERIFIED"}
				page.UsedReferralRewards = 1
			},
		},
		{
			name: "used reward missing",
			mutate: func(page *OpenCodeGoConsolePage) {
				page.UsedReferralRewardIDs = nil
				page.UsedReferralRewards = 0
			},
		},
		{
			name: "used reward duplicated",
			mutate: func(page *OpenCodeGoConsolePage) {
				page.UsedReferralRewardIDs = []string{"ref_UNVERIFIED", "ref_UNVERIFIED"}
				page.UsedReferralRewards = 2
			},
		},
		{
			name: "quota snapshot incomplete",
			mutate: func(page *OpenCodeGoConsolePage) {
				page.Quota.Windows = page.Quota.Windows[:2]
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, channel, codec := setupOpenCodeGoPoolTestDB(t)
			workspace := createEligibleOpenCodeGoWorkspace(
				t, db, codec, channel.Id, "manual-unverified", "workspace-manual-unverified", "wrk_UNVERIFIED", []string{"model-a"},
			)
			now := time.Unix(1_900_100_000, 0)
			preflight := newOpenCodeGoLifecyclePage(workspace.UpstreamWorkspaceID, 31, now)
			preflight.AvailableReferralRewardIDs = []string{"ref_UNVERIFIED"}
			preflight.AvailableReferralRewards = 1
			configureOpenCodeGoLifecycleWorkspace(t, db, workspace, preflight)
			postState := cloneOpenCodeGoLifecyclePage(preflight)
			postState.AvailableReferralRewardIDs = nil
			postState.AvailableReferralRewards = 0
			postState.UsedReferralRewardIDs = []string{"ref_UNVERIFIED"}
			postState.UsedReferralRewards = 1
			test.mutate(postState)
			fake := &fakeOpenCodeGoLifecycleBackend{
				page:       preflight,
				fetchPages: []*OpenCodeGoConsolePage{preflight, postState},
				models:     []string{"model-a"},
			}
			service := newOpenCodeGoLifecycleTestService(codec, fake, now)

			_, err := service.ApplyReferralReward(context.Background(), channel.Id, workspace.UID, "manual")
			require.Error(t, err)
			assert.Equal(t, 1, fake.referralCalls, "an unverifiable mutation must never be replayed")
			assert.Equal(t, 2, fake.fetchWorkspaceCalls)

			var operation model.OpenCodeGoOperation
			require.NoError(t, db.Where("workspace_id = ?", workspace.ID).First(&operation).Error)
			assert.Equal(t, OpenCodeGoOperationStatusFailed, operation.Status)
			updated, loadErr := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
			require.NoError(t, loadErr)
			assert.Equal(t, 1, updated.AvailableReferralRewards)
			assert.Equal(t, 0, updated.UsedReferralRewards)
		})
	}
}

func TestOpenCodeGoReferralRewardPersistsCompleteVerifiedSnapshotAndDerivedHealth(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "manual-persisted", "workspace-manual-persisted", "wrk_PERSISTED", []string{"model-a"},
	)
	now := time.Unix(1_900_100_000, 0)
	preflight := newOpenCodeGoLifecyclePage(workspace.UpstreamWorkspaceID, 100, now)
	preflight.AvailableReferralRewardIDs = []string{"ref_PERSISTED", "ref_REMAINING"}
	preflight.AvailableReferralRewards = 2
	configureOpenCodeGoLifecycleWorkspace(t, db, workspace, preflight)
	postFetchedAt := now.Add(30 * time.Second)
	postState := newOpenCodeGoLifecyclePage(workspace.UpstreamWorkspaceID, 23, postFetchedAt)
	postState.Quota.Windows[1].UsedPercent = 47
	postState.Quota.Windows[2].UsedPercent = 68
	postState.AvailableReferralRewardIDs = []string{"ref_REMAINING"}
	postState.AvailableReferralRewards = 1
	postState.UsedReferralRewardIDs = []string{"ref_PERSISTED"}
	postState.UsedReferralRewards = 1
	fake := &fakeOpenCodeGoLifecycleBackend{
		page:       preflight,
		fetchPages: []*OpenCodeGoConsolePage{preflight, postState},
		models:     []string{"model-a"},
	}
	service := newOpenCodeGoLifecycleTestService(codec, fake, postFetchedAt)

	summary, err := service.ApplyReferralReward(context.Background(), channel.Id, workspace.UID, "manual")
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoReferralApplySummary{Attempted: 1, Applied: 1}, summary)
	assert.Equal(t, 1, fake.referralCalls)

	updated, loadErr := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
	require.NoError(t, loadErr)
	require.NotNil(t, updated)
	assert.Equal(t, 1, updated.AvailableReferralRewards)
	assert.Equal(t, 1, updated.UsedReferralRewards)
	assert.Equal(t, model.OpenCodeGoQuotaSnapshotComplete, updated.QuotaSnapshotStatus)
	assert.Equal(t, postState.Quota.FetchedAt, updated.QuotaFetchedAt)
	assert.Equal(t, postState.Quota.NextRefreshAt, updated.QuotaNextRefreshAt)
	assert.Equal(t, OpenCodeGoSSRParserVersion, updated.QuotaParserVersion)
	assert.Empty(t, updated.QuotaError)
	assert.Equal(t, postFetchedAt.Unix(), updated.LastSyncedAt)
	assert.Equal(t, postFetchedAt.Unix(), updated.ReferralRewardAppliedAt)
	assert.Equal(t, model.OpenCodeGoStateEligible, updated.EffectiveState)
	assert.Zero(t, updated.QuotaRecoveryAt)
	require.Len(t, updated.QuotaWindows, len(model.OpenCodeGoQuotaKinds))
	for _, expected := range postState.Quota.Windows {
		var actual *model.OpenCodeGoQuotaWindow
		for index := range updated.QuotaWindows {
			if updated.QuotaWindows[index].Kind == expected.Kind {
				actual = &updated.QuotaWindows[index]
				break
			}
		}
		require.NotNil(t, actual)
		assert.Equal(t, expected.UsedPercent, actual.UsedPercent)
		assert.Equal(t, expected.ResetSeconds, actual.ResetSeconds)
		assert.Equal(t, expected.ResetAt, actual.ResetAt)
		assert.Equal(t, expected.FetchedAt, actual.FetchedAt)
	}
}

func TestOpenCodeGoReferralRewardCommitsSuccessWithoutFinalReadback(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "readback", "workspace-readback", "wrk_READBACK", []string{"model-a"},
	)
	now := time.Unix(1_900_100_000, 0)
	preflight := newOpenCodeGoLifecyclePage(workspace.UpstreamWorkspaceID, 31, now)
	preflight.AvailableReferralRewardIDs = []string{"ref_READBACK"}
	preflight.AvailableReferralRewards = 1
	configureOpenCodeGoLifecycleWorkspace(t, db, workspace, preflight)
	postState := cloneOpenCodeGoLifecyclePage(preflight)
	postState.AvailableReferralRewardIDs = nil
	postState.AvailableReferralRewards = 0
	postState.UsedReferralRewardIDs = []string{"ref_READBACK"}
	postState.UsedReferralRewards = 1
	fake := &fakeOpenCodeGoLifecycleBackend{
		page:       preflight,
		fetchPages: []*OpenCodeGoConsolePage{preflight, postState},
		models:     []string{"model-a"},
	}
	service := newOpenCodeGoLifecycleTestService(codec, fake, now)

	queryCount := 0
	callbackName := "opencode_go_test_fail_final_referral_readback"
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "open_code_go_workspaces" {
			return
		}
		queryCount++
		if queryCount == 4 {
			tx.AddError(errors.New("injected final referral readback failure"))
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callbackName) })

	summary, err := service.ApplyReferralReward(context.Background(), channel.Id, workspace.UID, "manual")
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoReferralApplySummary{Attempted: 1, Applied: 1}, summary)
	assert.Equal(t, 1, fake.referralCalls)

	var operation model.OpenCodeGoOperation
	require.NoError(t, db.Where("workspace_id = ?", workspace.ID).First(&operation).Error)
	assert.Equal(t, OpenCodeGoOperationStatusSucceeded, operation.Status)
}

func TestPersistOpenCodeGoReferralVerificationPreservesManualAndRiskEvidence(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "manual-risk", "workspace-manual-risk", "wrk_MANUALRISK", []string{"model-a"},
	)
	now := time.Unix(1_900_100_000, 0)
	page := newOpenCodeGoLifecyclePage(workspace.UpstreamWorkspaceID, 18, now)
	page.UsedReferralRewardIDs = []string{"ref_PRESERVED"}
	page.UsedReferralRewards = 1
	riskAt := now.Add(-time.Minute).Unix()
	healthObservedAt := now.Add(-time.Minute).UnixNano()
	reason := "confirmed provider risk block"
	require.NoError(t, db.Model(&model.OpenCodeGoWorkspace{}).Where("id = ?", workspace.ID).Updates(map[string]interface{}{
		"manual_enabled":       false,
		"effective_state":      model.OpenCodeGoStateManualDisabled,
		"state_reason":         "workspace is manually disabled",
		"health_observation":   string(OpenCodeGoObservationManualDisabled),
		"health_observed_at":   healthObservedAt,
		"risk_detected_at":     riskAt,
		"risk_last_checked_at": riskAt,
		"last_error":           reason,
	}).Error)
	service := newOpenCodeGoLifecycleTestService(codec, &fakeOpenCodeGoLifecycleBackend{page: page}, now)

	require.NoError(t, service.persistReferralVerification(channel.Id, workspace.UID, "ref_PRESERVED", page))
	updated, err := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.False(t, updated.ManualEnabled)
	assert.Equal(t, model.OpenCodeGoStateManualDisabled, updated.EffectiveState)
	assert.Equal(t, riskAt, updated.RiskDetectedAt)
	assert.Equal(t, riskAt, updated.RiskLastCheckedAt)
	assert.Equal(t, reason, updated.LastError)
	assert.Equal(t, 1, updated.UsedReferralRewards)
}

func TestPersistOpenCodeGoReferralVerificationPreservesRiskBlockedState(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "risk-preserved", "workspace-risk-preserved", "wrk_RISKPRESERVED", []string{"model-a"},
	)
	now := time.Unix(1_900_100_000, 0)
	page := newOpenCodeGoLifecyclePage(workspace.UpstreamWorkspaceID, 18, now)
	page.UsedReferralRewardIDs = []string{"ref_RISKPRESERVED"}
	page.UsedReferralRewards = 1
	riskAt := now.Add(-time.Minute).Unix()
	reason := "confirmed provider risk block"
	require.NoError(t, db.Model(&model.OpenCodeGoWorkspace{}).Where("id = ?", workspace.ID).Updates(map[string]interface{}{
		"effective_state":      model.OpenCodeGoStateRiskBlocked,
		"state_reason":         reason,
		"health_observation":   string(OpenCodeGoObservationRiskBlocked),
		"health_observed_at":   now.Add(-time.Minute).UnixNano(),
		"risk_detected_at":     riskAt,
		"risk_last_checked_at": riskAt,
		"last_error":           reason,
	}).Error)
	service := newOpenCodeGoLifecycleTestService(codec, &fakeOpenCodeGoLifecycleBackend{page: page}, now)

	require.NoError(t, service.persistReferralVerification(channel.Id, workspace.UID, "ref_RISKPRESERVED", page))
	updated, err := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, model.OpenCodeGoStateRiskBlocked, updated.EffectiveState)
	assert.Equal(t, riskAt, updated.RiskDetectedAt)
	assert.Equal(t, riskAt, updated.RiskLastCheckedAt)
	assert.Equal(t, reason, updated.LastError)
}

func TestPersistOpenCodeGoReferralVerificationPreservesBulkDisableEvidence(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "bulk-preserved", "workspace-bulk-preserved", "wrk_BULKPRESERVED", []string{"model-a"},
	)
	now := time.Unix(1_900_100_000, 0)
	page := newOpenCodeGoLifecyclePage(workspace.UpstreamWorkspaceID, 18, now)
	page.UsedReferralRewardIDs = []string{"ref_BULKPRESERVED"}
	page.UsedReferralRewards = 1
	bulkAt := now.Add(-time.Minute).Unix()
	reason := "persistent provider failures require manual verification"
	require.NoError(t, db.Model(&model.OpenCodeGoWorkspace{}).Where("id = ?", workspace.ID).Updates(map[string]interface{}{
		"effective_state":          model.OpenCodeGoStateBulkDisabled,
		"state_reason":             reason,
		"health_observation":       string(OpenCodeGoObservationBulkFailure),
		"health_observed_at":       now.Add(-time.Minute).UnixNano(),
		"bulk_failure_detected_at": bulkAt,
		"last_error":               reason,
	}).Error)
	service := newOpenCodeGoLifecycleTestService(codec, &fakeOpenCodeGoLifecycleBackend{page: page}, now)

	require.NoError(t, service.persistReferralVerification(channel.Id, workspace.UID, "ref_BULKPRESERVED", page))
	updated, err := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, model.OpenCodeGoStateBulkDisabled, updated.EffectiveState)
	assert.Equal(t, bulkAt, updated.BulkFailureDetectedAt)
	assert.Equal(t, reason, updated.LastError)
	assert.Equal(t, 1, updated.UsedReferralRewards)
}

func TestPersistOpenCodeGoReferralVerificationRollsBackWorkspaceAndQuotaTogether(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "reward-rollback", "workspace-reward-rollback", "wrk_REWARDROLLBACK", []string{"model-a"},
	)
	now := time.Unix(1_900_100_000, 0)
	before, err := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
	require.NoError(t, err)
	require.NotNil(t, before)
	beforeWindows := append([]model.OpenCodeGoQuotaWindow(nil), before.QuotaWindows...)
	page := newOpenCodeGoLifecyclePage(workspace.UpstreamWorkspaceID, 42, now)
	page.UsedReferralRewardIDs = []string{"ref_ROLLBACK"}
	page.UsedReferralRewards = 1
	callbackName := "opencode_go_test_fail_referral_quota_create"
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "open_code_go_quota_windows" {
			tx.AddError(errors.New("injected referral quota insert failure"))
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Create().Remove(callbackName) })
	service := newOpenCodeGoLifecycleTestService(codec, &fakeOpenCodeGoLifecycleBackend{page: page}, now)

	err = service.persistReferralVerification(channel.Id, workspace.UID, "ref_ROLLBACK", page)
	require.ErrorContains(t, err, "injected referral quota insert failure")
	after, loadErr := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
	require.NoError(t, loadErr)
	require.NotNil(t, after)
	assert.Equal(t, before.AvailableReferralRewards, after.AvailableReferralRewards)
	assert.Equal(t, before.UsedReferralRewards, after.UsedReferralRewards)
	assert.Equal(t, before.ReferralRewardAppliedAt, after.ReferralRewardAppliedAt)
	assert.Equal(t, before.QuotaFetchedAt, after.QuotaFetchedAt)
	assert.Equal(t, before.EffectiveState, after.EffectiveState)
	assert.Equal(t, beforeWindows, after.QuotaWindows)
}

func TestOpenCodeGoReferralRewardReturnsVerifiedSuccessWhenLocalPersistenceFails(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "reward-degraded", "workspace-reward-degraded", "wrk_REWARDDEGRADED", []string{"model-a"},
	)
	now := time.Unix(1_900_100_000, 0)
	preflight := newOpenCodeGoLifecyclePage(workspace.UpstreamWorkspaceID, 42, now)
	preflight.AvailableReferralRewardIDs = []string{"ref_DEGRADED"}
	preflight.AvailableReferralRewards = 1
	configureOpenCodeGoLifecycleWorkspace(t, db, workspace, preflight)
	postState := cloneOpenCodeGoLifecyclePage(preflight)
	postState.AvailableReferralRewardIDs = nil
	postState.AvailableReferralRewards = 0
	postState.UsedReferralRewardIDs = []string{"ref_DEGRADED"}
	postState.UsedReferralRewards = 1
	fake := &fakeOpenCodeGoLifecycleBackend{
		page:       preflight,
		fetchPages: []*OpenCodeGoConsolePage{preflight, postState},
		models:     []string{"model-a"},
	}
	callbackName := "opencode_go_test_fail_verified_referral_persistence"
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "open_code_go_quota_windows" {
			tx.AddError(errors.New("injected verified referral persistence failure"))
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Create().Remove(callbackName) })
	service := newOpenCodeGoLifecycleTestService(codec, fake, now)

	summary, err := service.ApplyReferralReward(context.Background(), channel.Id, workspace.UID, "manual")
	require.NoError(t, err)
	assert.Equal(t, OpenCodeGoReferralApplySummary{
		Attempted:           1,
		Applied:             1,
		PoolRefreshRequired: true,
	}, summary)
	assert.Equal(t, 1, fake.referralCalls)
	var operation model.OpenCodeGoOperation
	require.NoError(t, db.Where("workspace_id = ?", workspace.ID).First(&operation).Error)
	assert.Equal(t, OpenCodeGoOperationStatusSucceeded, operation.Status)
	assert.Contains(t, operation.Result, "local refresh required")
}

func TestOpenCodeGoRenewalCancellationIsNotAutomaticByDefaultAndPreservesEntitlement(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "renewal", "workspace-renewal", "wrk_RENEWAL1", []string{"model-a"},
	)
	now := time.Unix(1_900_100_000, 0)
	page := newOpenCodeGoLifecyclePage(workspace.UpstreamWorkspaceID, 10, now)
	enabled := true
	page.ChinaModelsEnabled = &enabled
	page.SubscriptionReference = "sub_SYNTHETIC"
	configureOpenCodeGoLifecycleWorkspace(t, db, workspace, page)
	fake := &fakeOpenCodeGoLifecycleBackend{
		page:         page,
		apiKey:       "synthetic-lifecycle-key",
		models:       []string{"model-a"},
		cancelResult: OpenCodeGoSubscriptionCancellation{CurrentPeriodEnd: now.Add(30 * 24 * time.Hour).Unix()},
	}
	service := newOpenCodeGoLifecycleTestService(codec, fake, now)
	t.Setenv("OPENCODE_GO_LIFECYCLE_AUTOMATION_ENABLED", "true")

	summary, err := service.RunIdentityAutomations(context.Background(), channel.Id, "identity-renewal", "scheduled")
	require.NoError(t, err)
	assert.True(t, summary.Enabled)
	assert.Zero(t, fake.cancelCalls)

	operation, result, err := service.CancelSubscriptionRenewal(context.Background(), channel.Id, workspace.UID, "manual")
	require.NoError(t, err)
	assert.Equal(t, 1, fake.cancelCalls)
	assert.Equal(t, fake.cancelResult, result)
	assert.Equal(t, OpenCodeGoOperationStatusSucceeded, operation.Status)

	updated, loadErr := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
	require.NoError(t, loadErr)
	require.NotNil(t, updated)
	assert.Equal(t, model.OpenCodeGoMembershipActive, updated.MembershipStatus)
	assert.Equal(t, result.CurrentPeriodEnd, updated.SubscriptionEndsAt)
	assert.Equal(t, now.Unix(), updated.RenewalCancelledAt)

	settings := channel.GetOtherSettings()
	settings.OpenCodeGo = &relaydto.OpenCodeGoConfig{
		AutoCancelSubscriptionRenewal: true,
	}
	encoded, marshalErr := common.Marshal(settings)
	require.NoError(t, marshalErr)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("settings", string(encoded)).Error)
	automationSummary, automationErr := service.RunIdentityAutomations(context.Background(), channel.Id, "identity-renewal", "scheduled")
	require.NoError(t, automationErr)
	assert.Zero(t, automationSummary.Attempted)
	assert.Equal(t, 1, fake.cancelCalls, "a verified prior cancellation must not be repeated")
}

func TestOpenCodeGoFailedOperationRedactsCredentialsAndUpstreamIdentifiers(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t, db, codec, channel.Id, "redaction", "workspace-redaction", "wrk_REDACT1", []string{"model-a"},
	)
	now := time.Unix(1_900_100_000, 0)
	page := newOpenCodeGoLifecyclePage(workspace.UpstreamWorkspaceID, 100, now)
	page.AvailableReferralRewardIDs = []string{"ref_REDACT1"}
	page.AvailableReferralRewards = 1
	configureOpenCodeGoLifecycleWorkspace(t, db, workspace, page)
	fake := &fakeOpenCodeGoLifecycleBackend{
		page:        page,
		models:      []string{"model-a"},
		referralErr: errors.New("auth=synthetic-cookie sk-synthetic-key wrk_REDACT1 ref_REDACT1"),
	}
	service := newOpenCodeGoLifecycleTestService(codec, fake, now)

	_, err := service.ApplyReferralRewards(context.Background(), channel.Id, workspace.UID, "manual", 1)
	require.Error(t, err)
	assert.Equal(t, 1, fake.referralCalls, "a failed remote mutation must not be retried")
	publicError := err.Error()
	for _, forbidden := range []string{
		"synthetic-cookie",
		"sk-synthetic-key",
		"wrk_REDACT1",
		"ref_REDACT1",
	} {
		assert.NotContains(t, publicError, forbidden, fmt.Sprintf("public error exposed %s", forbidden))
	}

	var operation model.OpenCodeGoOperation
	require.NoError(t, db.Order("id desc").First(&operation).Error)
	assert.Equal(t, OpenCodeGoOperationStatusFailed, operation.Status)
	payload, marshalErr := common.Marshal(operation)
	require.NoError(t, marshalErr)
	serialized := string(payload)
	for _, forbidden := range []string{
		"synthetic-cookie",
		"sk-synthetic-key",
		"wrk_REDACT1",
		"ref_REDACT1",
	} {
		assert.NotContains(t, serialized, forbidden, fmt.Sprintf("operation exposed %s", forbidden))
	}
}
