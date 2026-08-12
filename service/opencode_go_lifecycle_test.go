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
) error {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.referralCalls++
	if fake.referralErr != nil {
		return fake.referralErr
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
	return nil
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
	enabled := true
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
	factoryCalls := make(map[int]int)
	root := &OpenCodeGoLifecycleService{
		scopedFactory: func(channelID int) (*OpenCodeGoLifecycleService, error) {
			factoryMutex.Lock()
			factoryCalls[channelID]++
			factoryMutex.Unlock()
			fake := &fakeOpenCodeGoLifecycleBackend{models: []string{"model-a"}}
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
	assert.Equal(t, map[int]int{firstChannel.Id: 1, secondChannel.Id: 1}, factoryCalls)
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
	assert.Equal(t, 2, fake.discoverCalls, "every successful reward must perform a complete quota refresh")
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
