package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaydto "github.com/QuantumNous/new-api/relaykit/dto"
	"gorm.io/gorm"
)

const (
	OpenCodeGoDefaultReferralRewardsPerRun = 3
	OpenCodeGoMaxReferralRewardsPerRun     = 20
)

var ErrOpenCodeGoReferralRewardUnavailable = errors.New("OpenCode Go referral reward is not currently available")

func SanitizeOpenCodeGoLifecycleError(err error) string {
	return sanitizeOpenCodeGoError(err)
}

type openCodeGoLifecycleReader interface {
	FetchWorkspacePage(ctx context.Context, authCookie string, workspaceID string) (*OpenCodeGoConsolePage, error)
}

type OpenCodeGoLifecyclePolicy struct {
	AutomationEnabled             bool `json:"automation_enabled"`
	AutoEnableChinaModels         bool `json:"auto_enable_china_models"`
	AutoApplyReferralRewards      bool `json:"auto_apply_referral_rewards"`
	ReferralRewardsMaxPerRun      int  `json:"referral_rewards_max_per_run"`
	AutoCancelSubscriptionRenewal bool `json:"auto_cancel_subscription_renewal"`
}

type OpenCodeGoReferralApplySummary struct {
	Attempted           int  `json:"attempted"`
	Applied             int  `json:"applied"`
	PoolRefreshRequired bool `json:"-"`
}

type OpenCodeGoLifecycleAutomationResult struct {
	WorkspaceUID string `json:"workspace_uid"`
	Action       string `json:"action"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
}

type OpenCodeGoLifecycleAutomationSummary struct {
	Enabled   bool                                  `json:"enabled"`
	Attempted int                                   `json:"attempted"`
	Succeeded int                                   `json:"succeeded"`
	Failed    int                                   `json:"failed"`
	Results   []OpenCodeGoLifecycleAutomationResult `json:"results"`
}

type OpenCodeGoLifecycleIdentityAutomationResult struct {
	ChannelID int                                  `json:"channel_id"`
	Summary   OpenCodeGoLifecycleAutomationSummary `json:"summary"`
	Error     string                               `json:"error,omitempty"`
}

type OpenCodeGoLifecycleBatchSummary struct {
	Enabled    bool                                          `json:"enabled"`
	Identities int                                           `json:"identities"`
	Attempted  int                                           `json:"attempted"`
	Succeeded  int                                           `json:"succeeded"`
	Failed     int                                           `json:"failed"`
	Results    []OpenCodeGoLifecycleIdentityAutomationResult `json:"results"`
}

type OpenCodeGoLifecycleService struct {
	reader        openCodeGoLifecycleReader
	mutator       openCodeGoLifecycleMutator
	pool          *OpenCodeGoAccountPoolService
	codec         *OpenCodeGoCredentialCodec
	scopedFactory func(channelID int, identityUID string) (*OpenCodeGoLifecycleService, error)
	now           func() time.Time
}

type openCodeGoLifecycleTarget struct {
	identity   *model.OpenCodeGoIdentity
	workspace  *model.OpenCodeGoWorkspace
	authCookie string
}

func NewConfiguredOpenCodeGoLifecycleService() (*OpenCodeGoLifecycleService, error) {
	codec, err := NewConfiguredOpenCodeGoCredentialCodec()
	if err != nil {
		return nil, err
	}
	return &OpenCodeGoLifecycleService{
		codec: codec,
		scopedFactory: func(channelID int, identityUID string) (*OpenCodeGoLifecycleService, error) {
			return newOpenCodeGoIdentityLifecycleService(channelID, identityUID, codec)
		},
		now: time.Now,
	}, nil
}

func newOpenCodeGoIdentityLifecycleService(
	channelID int,
	identityUID string,
	codec *OpenCodeGoCredentialCodec,
) (*OpenCodeGoLifecycleService, error) {
	baseClient, err := GetOpenCodeGoIdentityHTTPClient(channelID, identityUID)
	if err != nil {
		return nil, err
	}
	return newOpenCodeGoLifecycleServiceWithClient(baseClient, codec)
}

func newOpenCodeGoChannelLifecycleService(
	channelID int,
	codec *OpenCodeGoCredentialCodec,
) (*OpenCodeGoLifecycleService, error) {
	baseClient, err := getOpenCodeGoChannelHTTPClient(channelID)
	if err != nil {
		return nil, err
	}
	return newOpenCodeGoLifecycleServiceWithClient(baseClient, codec)
}

func newOpenCodeGoLifecycleServiceWithClient(
	baseClient *http.Client,
	codec *OpenCodeGoCredentialCodec,
) (*OpenCodeGoLifecycleService, error) {
	console, err := newOpenCodeGoConsoleClient(openCodeGoConsoleOrigin, openCodeGoInferenceOrigin, baseClient)
	if err != nil {
		return nil, err
	}
	mutator, err := newOpenCodeGoLifecycleClient(console, "https://billing.stripe.com", baseClient)
	if err != nil {
		return nil, err
	}
	pool := newOpenCodeGoAccountPoolService(console, codec)
	return newOpenCodeGoLifecycleService(console, mutator, pool), nil
}

func newOpenCodeGoLifecycleService(
	reader openCodeGoLifecycleReader,
	mutator openCodeGoLifecycleMutator,
	pool *OpenCodeGoAccountPoolService,
) *OpenCodeGoLifecycleService {
	return &OpenCodeGoLifecycleService{
		reader:  reader,
		mutator: mutator,
		pool:    pool,
		codec:   pool.codec,
		now:     time.Now,
	}
}

func (service *OpenCodeGoLifecycleService) scopedForIdentity(channelID int, identityUID string) (*OpenCodeGoLifecycleService, error) {
	if service == nil {
		return nil, errors.New("OpenCode Go lifecycle service is not configured")
	}
	if service.reader != nil && service.mutator != nil && service.pool != nil {
		return service, nil
	}
	if service.scopedFactory == nil {
		return nil, errors.New("OpenCode Go lifecycle service is not configured")
	}
	scoped, err := service.scopedFactory(channelID, identityUID)
	if err != nil {
		return nil, err
	}
	if scoped == nil {
		return nil, errors.New("OpenCode Go lifecycle service is not configured")
	}
	return scoped, nil
}

func GetOpenCodeGoLifecyclePolicy(channelID int) (OpenCodeGoLifecyclePolicy, error) {
	if err := validateOpenCodeGoPoolChannel(channelID); err != nil {
		return OpenCodeGoLifecyclePolicy{}, err
	}
	channel, err := model.GetChannelById(channelID, true)
	if err != nil {
		return OpenCodeGoLifecyclePolicy{}, err
	}
	settings, err := decodeOpenCodeGoChannelSettings(channel.OtherSettings)
	if err != nil {
		return OpenCodeGoLifecyclePolicy{}, err
	}
	return openCodeGoLifecyclePolicyFromConfig(settings.OpenCodeGo), nil
}

func UpdateOpenCodeGoLifecyclePolicy(channelID int, policy OpenCodeGoLifecyclePolicy) (OpenCodeGoLifecyclePolicy, error) {
	if err := validateOpenCodeGoPoolChannel(channelID); err != nil {
		return OpenCodeGoLifecyclePolicy{}, err
	}
	if policy.ReferralRewardsMaxPerRun < 0 || policy.ReferralRewardsMaxPerRun > OpenCodeGoMaxReferralRewardsPerRun {
		return OpenCodeGoLifecyclePolicy{}, fmt.Errorf(
			"OpenCode Go referral_rewards_max_per_run must be between 0 and %d",
			OpenCodeGoMaxReferralRewardsPerRun,
		)
	}
	policy.ReferralRewardsMaxPerRun = normalizeOpenCodeGoReferralRewardLimit(policy.ReferralRewardsMaxPerRun)
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		if err := validateOpenCodeGoPoolChannelTx(tx, channelID); err != nil {
			return err
		}
		var channel model.Channel
		if err := model.LockForUpdate(tx).Where("id = ?", channelID).First(&channel).Error; err != nil {
			return err
		}
		settings, err := decodeOpenCodeGoChannelSettings(channel.OtherSettings)
		if err != nil {
			return err
		}
		if settings.OpenCodeGo == nil {
			settings.OpenCodeGo = &relaydto.OpenCodeGoConfig{}
		}
		applyOpenCodeGoLifecyclePolicy(settings.OpenCodeGo, policy)
		if err := settings.OpenCodeGo.Validate(); err != nil {
			return err
		}
		encoded, err := common.Marshal(settings)
		if err != nil {
			return err
		}
		return tx.Model(&model.Channel{}).
			Where("id = ?", channelID).
			Update("settings", string(encoded)).Error
	})
	if err != nil {
		return OpenCodeGoLifecyclePolicy{}, err
	}
	model.InitChannelCache()
	return GetOpenCodeGoLifecyclePolicy(channelID)
}

func decodeOpenCodeGoChannelSettings(encoded string) (relaydto.ChannelOtherSettings, error) {
	settings := relaydto.ChannelOtherSettings{}
	if strings.TrimSpace(encoded) == "" {
		return settings, nil
	}
	if err := common.UnmarshalJsonStr(encoded, &settings); err != nil {
		return relaydto.ChannelOtherSettings{}, errors.New("OpenCode Go channel settings are invalid")
	}
	return settings, nil
}

func openCodeGoLifecyclePolicyFromConfig(config *relaydto.OpenCodeGoConfig) OpenCodeGoLifecyclePolicy {
	policy := OpenCodeGoLifecyclePolicy{
		AutomationEnabled:        openCodeGoLifecycleAutomationEnabled(),
		AutoEnableChinaModels:    true,
		AutoApplyReferralRewards: true,
		ReferralRewardsMaxPerRun: OpenCodeGoDefaultReferralRewardsPerRun,
	}
	if config == nil {
		return policy
	}
	if config.AutoEnableChinaModels != nil {
		policy.AutoEnableChinaModels = *config.AutoEnableChinaModels
	}
	if config.AutoApplyReferralRewards != nil {
		policy.AutoApplyReferralRewards = *config.AutoApplyReferralRewards
	}
	if config.ReferralRewardsMaxPerRun != nil {
		policy.ReferralRewardsMaxPerRun = normalizeOpenCodeGoReferralRewardLimit(*config.ReferralRewardsMaxPerRun)
	}
	policy.AutoCancelSubscriptionRenewal = config.AutoCancelSubscriptionRenewal
	return policy
}

func (service *OpenCodeGoLifecycleService) EnableChinaModels(
	ctx context.Context,
	channelID int,
	workspaceUID string,
	source string,
) (*model.OpenCodeGoOperation, error) {
	scoped, target, unlock, err := service.scopedTarget(channelID, workspaceUID)
	if err != nil {
		return nil, err
	}
	service = scoped
	defer unlock()

	startedAt := service.now()
	operation, err := startOpenCodeGoOperation(
		channelID,
		*target.workspace,
		OpenCodeGoOperationEnableChinaModels,
		source,
		startedAt,
	)
	if err != nil {
		return nil, err
	}
	page, err := service.reader.FetchWorkspacePage(ctx, target.authCookie, target.workspace.UpstreamWorkspaceID)
	if err != nil {
		return operation, service.failOperation(operation, err)
	}
	if page.ChinaModelsEnabled == nil {
		err = errors.New("OpenCode Go China-model state is not authoritative")
		return operation, service.failOperation(operation, err)
	}
	if *page.ChinaModelsEnabled {
		if err := service.persistChinaModelVerification(channelID, workspaceUID, true, nil); err != nil {
			return operation, service.failOperation(operation, err)
		}
		return operation, finishOpenCodeGoOperation(operation, OpenCodeGoOperationStatusSucceeded, "China-deployed models already enabled", nil, service.now())
	}
	if err := service.mutator.EnableChinaModels(ctx, target.authCookie, page); err != nil {
		_ = service.persistChinaModelVerification(channelID, workspaceUID, false, err)
		return operation, service.failOperation(operation, err)
	}
	verified, err := service.reader.FetchWorkspacePage(ctx, target.authCookie, target.workspace.UpstreamWorkspaceID)
	if err == nil && (verified.ChinaModelsEnabled == nil || !*verified.ChinaModelsEnabled) {
		err = errors.New("OpenCode Go China-model action could not be verified")
	}
	if err != nil {
		_ = service.persistChinaModelVerification(channelID, workspaceUID, false, err)
		return operation, service.failOperation(operation, err)
	}
	if err := service.persistChinaModelVerification(channelID, workspaceUID, true, nil); err != nil {
		return operation, service.failOperation(operation, err)
	}
	if err := finishOpenCodeGoOperation(operation, OpenCodeGoOperationStatusSucceeded, "China-deployed models enabled and verified", nil, service.now()); err != nil {
		return operation, err
	}
	return operation, nil
}

func (service *OpenCodeGoLifecycleService) DisableChinaModels(
	ctx context.Context,
	channelID int,
	workspaceUID string,
	source string,
) (*model.OpenCodeGoOperation, error) {
	scoped, target, unlock, err := service.scopedTarget(channelID, workspaceUID)
	if err != nil {
		return nil, err
	}
	service = scoped
	defer unlock()

	startedAt := service.now()
	operation, err := startOpenCodeGoOperation(
		channelID,
		*target.workspace,
		OpenCodeGoOperationDisableChinaModels,
		source,
		startedAt,
	)
	if err != nil {
		return nil, err
	}
	page, err := service.reader.FetchWorkspacePage(ctx, target.authCookie, target.workspace.UpstreamWorkspaceID)
	if err != nil {
		return operation, service.failOperation(operation, err)
	}
	if page.ChinaModelsEnabled == nil {
		err = errors.New("OpenCode Go China-model state is not authoritative")
		return operation, service.failOperation(operation, err)
	}
	if !*page.ChinaModelsEnabled {
		if err := service.persistChinaModelVerification(channelID, workspaceUID, false, nil); err != nil {
			return operation, service.failOperation(operation, err)
		}
		return operation, finishOpenCodeGoOperation(operation, OpenCodeGoOperationStatusSucceeded, "China-deployed models already disabled", nil, service.now())
	}
	if err := service.mutator.DisableChinaModels(ctx, target.authCookie, page); err != nil {
		_ = service.persistChinaModelVerification(channelID, workspaceUID, true, err)
		return operation, service.failOperation(operation, err)
	}
	verified, err := service.reader.FetchWorkspacePage(ctx, target.authCookie, target.workspace.UpstreamWorkspaceID)
	if err == nil && verified.ChinaModelsEnabled != nil && *verified.ChinaModelsEnabled {
		err = errors.New("OpenCode Go China-model action could not be verified")
	}
	if err != nil {
		_ = service.persistChinaModelVerification(channelID, workspaceUID, true, err)
		return operation, service.failOperation(operation, err)
	}
	if err := service.persistChinaModelVerification(channelID, workspaceUID, false, nil); err != nil {
		return operation, service.failOperation(operation, err)
	}
	if err := finishOpenCodeGoOperation(operation, OpenCodeGoOperationStatusSucceeded, "China-deployed models disabled and verified", nil, service.now()); err != nil {
		return operation, err
	}
	return operation, nil
}

func (service *OpenCodeGoLifecycleService) ApplyReferralRewards(
	ctx context.Context,
	channelID int,
	workspaceUID string,
	source string,
	maxRewards int,
) (OpenCodeGoReferralApplySummary, error) {
	return service.applyReferralRewards(ctx, channelID, workspaceUID, source, maxRewards, true)
}

func (service *OpenCodeGoLifecycleService) ApplyReferralReward(
	ctx context.Context,
	channelID int,
	workspaceUID string,
	source string,
) (OpenCodeGoReferralApplySummary, error) {
	return service.applyReferralRewards(ctx, channelID, workspaceUID, source, 1, false)
}

func (service *OpenCodeGoLifecycleService) applyReferralRewards(
	ctx context.Context,
	channelID int,
	workspaceUID string,
	source string,
	maxRewards int,
	requireExhaustedQuota bool,
) (OpenCodeGoReferralApplySummary, error) {
	summary := OpenCodeGoReferralApplySummary{}
	maxRewards = normalizeOpenCodeGoReferralRewardLimit(maxRewards)
	scoped, target, unlock, err := service.scopedTarget(channelID, workspaceUID)
	if err != nil {
		return summary, err
	}
	service = scoped
	defer unlock()
	if requireExhaustedQuota && !openCodeGoWorkspaceHasExhaustedQuota(*target.workspace) {
		return summary, nil
	}
	if !openCodeGoReferralRewardEligibleAt(*target.identity, *target.workspace, service.now()) {
		if requireExhaustedQuota {
			return summary, nil
		}
		return summary, ErrOpenCodeGoReferralRewardUnavailable
	}

	attempted := make(map[string]struct{})
	for summary.Attempted < maxRewards {
		if requireExhaustedQuota && !openCodeGoWorkspaceHasExhaustedQuota(*target.workspace) {
			break
		}
		page, inspectErr := service.reader.FetchWorkspacePage(ctx, target.authCookie, target.workspace.UpstreamWorkspaceID)
		if inspectErr != nil {
			return summary, inspectErr
		}
		if page.MembershipStatus != model.OpenCodeGoMembershipActive {
			return summary, errors.New("OpenCode Go referral reward requires an active membership")
		}
		if requireExhaustedQuota && !openCodeGoPageHasExhaustedQuota(page) {
			return summary, errors.New("OpenCode Go referral reward requires authoritative exhausted member quota")
		}

		rewardID := ""
		for _, candidate := range page.AvailableReferralRewardIDs {
			if _, seen := attempted[candidate]; !seen {
				rewardID = candidate
				break
			}
		}
		if rewardID == "" {
			if !requireExhaustedQuota && summary.Attempted == 0 {
				return summary, ErrOpenCodeGoReferralRewardUnavailable
			}
			break
		}
		attempted[rewardID] = struct{}{}
		summary.Attempted++
		operation, startErr := startOpenCodeGoOperation(
			channelID,
			*target.workspace,
			OpenCodeGoOperationApplyReferral,
			source,
			service.now(),
		)
		if startErr != nil {
			return summary, startErr
		}

		amount, mutationErr := service.mutator.ApplyReferralReward(ctx, target.authCookie, page, rewardID)
		if mutationErr != nil {
			return summary, service.failOperation(operation, mutationErr)
		}
		if amount <= 0 {
			err = errors.New("OpenCode Go referral action amount was invalid")
			return summary, service.failOperation(operation, err)
		}

		verifiedPage, verifyErr := service.reader.FetchWorkspacePage(ctx, target.authCookie, target.workspace.UpstreamWorkspaceID)
		if verifyErr != nil {
			return summary, service.failOperation(operation, verifyErr)
		}
		if verifiedPage == nil || !strings.EqualFold(verifiedPage.WorkspaceID, target.workspace.UpstreamWorkspaceID) {
			return summary, service.failOperation(operation, errors.New("OpenCode Go referral verification page is for another workspace"))
		}
		if _, verifyErr := validateOpenCodeGoReferralVerificationPage(verifiedPage, rewardID); verifyErr != nil {
			return summary, service.failOperation(operation, verifyErr)
		}
		if verifiedPage.Quota.FetchedAt < target.workspace.LastSyncedAt ||
			verifiedPage.Quota.FetchedAt < target.workspace.QuotaFetchedAt {
			return summary, service.failOperation(operation, errors.New("OpenCode Go referral verification page is older than the stored snapshot"))
		}
		refreshedWorkspace, persistErr := func() (*model.OpenCodeGoWorkspace, error) {
			releaseMutation := BeginOpenCodeGoPoolMutation(channelID)
			defer releaseMutation()
			refreshed, err := service.persistReferralVerificationAndFinishOperation(
				channelID, workspaceUID, rewardID, verifiedPage, operation, amount,
			)
			if err != nil {
				return nil, err
			}
			if service.pool.rebuild == nil {
				return refreshed, nil
			}
			openCodeGoIdentityProxyClients.advanceSelectionGeneration(channelID)
			// The provider mutation, post-state verification, local snapshot and
			// success operation are already committed. A rebuild failure must not
			// turn that irreversible success into a retryable claim failure.
			_ = service.pool.rebuild(channelID)
			return refreshed, nil
		}()
		if persistErr != nil {
			// The fresh provider read already proved this exact reward was applied.
			// Local persistence failure cannot make the mutation retryable.
			_ = finishOpenCodeGoOperation(
				operation,
				OpenCodeGoOperationStatusSucceeded,
				fmt.Sprintf("referral reward applied and verified; local refresh required (amount=%d)", amount),
				nil,
				service.now(),
			)
			summary.Applied++
			summary.PoolRefreshRequired = true
			return summary, nil
		}
		summary.Applied++
		target.workspace = refreshedWorkspace
	}
	return summary, nil
}

func (service *OpenCodeGoLifecycleService) CancelSubscriptionRenewal(
	ctx context.Context,
	channelID int,
	workspaceUID string,
	source string,
) (*model.OpenCodeGoOperation, OpenCodeGoSubscriptionCancellation, error) {
	result := OpenCodeGoSubscriptionCancellation{}
	scoped, target, unlock, err := service.scopedTarget(channelID, workspaceUID)
	if err != nil {
		return nil, result, err
	}
	service = scoped
	defer unlock()
	operation, err := startOpenCodeGoOperation(
		channelID,
		*target.workspace,
		OpenCodeGoOperationCancelSubscription,
		source,
		service.now(),
	)
	if err != nil {
		return nil, result, err
	}
	page, err := service.reader.FetchWorkspacePage(ctx, target.authCookie, target.workspace.UpstreamWorkspaceID)
	if err != nil {
		return operation, result, service.failOperation(operation, err)
	}
	if page.MembershipStatus != model.OpenCodeGoMembershipActive || page.SubscriptionReference == "" {
		err = errors.New("OpenCode Go workspace does not have an active subscription")
		return operation, result, service.failOperation(operation, err)
	}
	result, err = service.mutator.CancelSubscriptionRenewal(ctx, target.authCookie, page)
	if err != nil {
		_ = service.persistRenewalVerification(channelID, workspaceUID, result, err)
		return operation, result, service.failOperation(operation, err)
	}
	if result.CurrentPeriodEnd <= service.now().Unix() {
		err = errors.New("OpenCode Go subscription period end could not be verified")
		_ = service.persistRenewalVerification(channelID, workspaceUID, result, err)
		return operation, result, service.failOperation(operation, err)
	}
	refreshed, err := service.pool.refreshIdentityWithCookie(
		ctx,
		channelID,
		target.identity,
		target.authCookie,
		nil,
		true,
	)
	if err != nil {
		_ = service.persistRenewalVerification(channelID, workspaceUID, result, err)
		return operation, result, service.failOperation(operation, err)
	}
	refreshedWorkspace := findOpenCodeGoIdentityWorkspace(refreshed, workspaceUID)
	if refreshedWorkspace == nil || refreshedWorkspace.MembershipStatus != model.OpenCodeGoMembershipActive {
		err = errors.New("OpenCode Go subscription entitlement was not preserved after cancellation")
		_ = service.persistRenewalVerification(channelID, workspaceUID, result, err)
		return operation, result, service.failOperation(operation, err)
	}
	if err := service.persistRenewalVerification(channelID, workspaceUID, result, nil); err != nil {
		return operation, result, service.failOperation(operation, err)
	}
	operationResult := "subscription renewal cancelled at period end and entitlement verified"
	if result.AlreadyCancelled {
		operationResult = "subscription renewal was already cancelled at period end"
	}
	if err := finishOpenCodeGoOperation(operation, OpenCodeGoOperationStatusSucceeded, operationResult, nil, service.now()); err != nil {
		return operation, result, err
	}
	return operation, result, nil
}

func (service *OpenCodeGoLifecycleService) RunIdentityAutomations(
	ctx context.Context,
	channelID int,
	identityUID string,
	source string,
) (OpenCodeGoLifecycleAutomationSummary, error) {
	summary := OpenCodeGoLifecycleAutomationSummary{Results: make([]OpenCodeGoLifecycleAutomationResult, 0)}
	policy, err := GetOpenCodeGoLifecyclePolicy(channelID)
	if err != nil {
		return summary, err
	}
	summary.Enabled = policy.AutomationEnabled
	if !policy.AutomationEnabled {
		return summary, nil
	}
	identity, err := model.GetOpenCodeGoIdentityPool(channelID, identityUID)
	if err != nil {
		return summary, err
	}
	if identity == nil {
		return summary, gorm.ErrRecordNotFound
	}
	workspaceUIDs := make([]string, 0, len(identity.Workspaces))
	for _, workspace := range identity.Workspaces {
		workspaceUIDs = append(workspaceUIDs, workspace.UID)
	}
	for _, workspaceUID := range workspaceUIDs {
		workspace, loadErr := model.GetOpenCodeGoWorkspace(channelID, workspaceUID)
		if loadErr != nil {
			return summary, loadErr
		}
		if workspace == nil || !workspace.ManualEnabled || workspace.EffectiveState == model.OpenCodeGoStateRiskBlocked || workspace.EffectiveState == model.OpenCodeGoStateAuthError {
			continue
		}
		if policy.AutoEnableChinaModels && (workspace.ChinaModelsEnabled == nil || !*workspace.ChinaModelsEnabled) {
			_, actionErr := service.EnableChinaModels(ctx, channelID, workspaceUID, source)
			summary.append(workspaceUID, OpenCodeGoOperationEnableChinaModels, actionErr)
		}
		workspace, loadErr = model.GetOpenCodeGoWorkspace(channelID, workspaceUID)
		if loadErr != nil {
			return summary, loadErr
		}
		if workspace != nil && policy.AutoApplyReferralRewards && workspace.AvailableReferralRewards > 0 && openCodeGoWorkspaceHasExhaustedQuota(*workspace) {
			referralSummary, actionErr := service.ApplyReferralRewards(
				ctx,
				channelID,
				workspaceUID,
				source,
				policy.ReferralRewardsMaxPerRun,
			)
			if referralSummary.Attempted > 0 || actionErr != nil {
				summary.append(workspaceUID, OpenCodeGoOperationApplyReferral, actionErr)
			}
		}
		workspace, loadErr = model.GetOpenCodeGoWorkspace(channelID, workspaceUID)
		if loadErr != nil {
			return summary, loadErr
		}
		if workspace != nil && policy.AutoCancelSubscriptionRenewal && workspace.MembershipStatus == model.OpenCodeGoMembershipActive && workspace.SubscriptionReference != "" && workspace.RenewalCancelledAt == 0 {
			_, _, actionErr := service.CancelSubscriptionRenewal(ctx, channelID, workspaceUID, source)
			summary.append(workspaceUID, OpenCodeGoOperationCancelSubscription, actionErr)
		}
	}
	return summary, nil
}

func (service *OpenCodeGoLifecycleService) RunRefreshAutomations(
	ctx context.Context,
	refreshResults []OpenCodeGoRefreshResult,
	source string,
) (OpenCodeGoLifecycleBatchSummary, error) {
	summary := OpenCodeGoLifecycleBatchSummary{
		Enabled: openCodeGoLifecycleAutomationEnabled(),
		Results: make([]OpenCodeGoLifecycleIdentityAutomationResult, 0),
	}
	if !summary.Enabled {
		return summary, nil
	}
	for _, refresh := range refreshResults {
		if refresh.Status != "refreshed" || refresh.ChannelID <= 0 || refresh.IdentityUID == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		identitySummary, err := service.RunIdentityAutomations(
			ctx,
			refresh.ChannelID,
			refresh.IdentityUID,
			sanitizeOpenCodeGoLifecycleSource(source),
		)
		result := OpenCodeGoLifecycleIdentityAutomationResult{
			ChannelID: refresh.ChannelID,
			Summary:   identitySummary,
		}
		summary.Identities++
		summary.Attempted += identitySummary.Attempted
		summary.Succeeded += identitySummary.Succeeded
		summary.Failed += identitySummary.Failed
		if err != nil {
			result.Error = sanitizeOpenCodeGoError(err)
			summary.Failed++
		}
		summary.Results = append(summary.Results, result)
	}
	return summary, nil
}

func (summary *OpenCodeGoLifecycleAutomationSummary) append(workspaceUID string, action string, err error) {
	result := OpenCodeGoLifecycleAutomationResult{
		WorkspaceUID: workspaceUID,
		Action:       action,
		Status:       OpenCodeGoOperationStatusSucceeded,
	}
	summary.Attempted++
	if err != nil {
		result.Status = OpenCodeGoOperationStatusFailed
		result.Error = sanitizeOpenCodeGoError(err)
		summary.Failed++
	} else {
		summary.Succeeded++
	}
	summary.Results = append(summary.Results, result)
}

func (service *OpenCodeGoLifecycleService) validate() error {
	if service == nil || service.reader == nil || service.mutator == nil || service.pool == nil || service.pool.codec == nil || service.pool.console == nil {
		return errors.New("OpenCode Go lifecycle service is not configured")
	}
	return nil
}

func (service *OpenCodeGoLifecycleService) scopedTarget(
	channelID int,
	workspaceUID string,
) (*OpenCodeGoLifecycleService, *openCodeGoLifecycleTarget, func(), error) {
	if service == nil || service.codec == nil {
		return nil, nil, nil, errors.New("OpenCode Go lifecycle service is not configured")
	}
	target, unlock, err := service.lockTarget(channelID, workspaceUID)
	if err != nil {
		return nil, nil, nil, err
	}
	scoped, err := service.scopedForIdentity(channelID, target.identity.UID)
	if err != nil {
		unlock()
		return nil, nil, nil, err
	}
	if err := scoped.validate(); err != nil {
		unlock()
		return nil, nil, nil, err
	}
	return scoped, target, unlock, nil
}

func (service *OpenCodeGoLifecycleService) lockTarget(
	channelID int,
	workspaceUID string,
) (*openCodeGoLifecycleTarget, func(), error) {
	if err := validateOpenCodeGoPoolChannel(channelID); err != nil {
		return nil, nil, err
	}
	workspace, err := model.GetOpenCodeGoWorkspace(channelID, workspaceUID)
	if err != nil {
		return nil, nil, err
	}
	if workspace == nil {
		return nil, nil, gorm.ErrRecordNotFound
	}
	identity, err := model.GetOpenCodeGoIdentityByID(channelID, workspace.IdentityID)
	if err != nil {
		return nil, nil, err
	}
	if identity == nil {
		return nil, nil, gorm.ErrRecordNotFound
	}
	unlock := lockOpenCodeGoIdentityOperation(fmt.Sprintf("%d:identity:%s", channelID, identity.UID))
	identity, err = model.GetOpenCodeGoIdentityPool(channelID, identity.UID)
	if err != nil || identity == nil {
		unlock()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, gorm.ErrRecordNotFound
	}
	workspace = findOpenCodeGoIdentityWorkspace(identity, workspaceUID)
	if workspace == nil {
		unlock()
		return nil, nil, gorm.ErrRecordNotFound
	}
	authCookie, err := service.codec.Decrypt(
		OpenCodeGoCredentialAuthCookie,
		channelID,
		identity.UID,
		identity.AuthCookieCiphertext,
	)
	if err != nil {
		unlock()
		return nil, nil, err
	}
	return &openCodeGoLifecycleTarget{
		identity:   identity,
		workspace:  workspace,
		authCookie: authCookie,
	}, unlock, nil
}

func findOpenCodeGoIdentityWorkspace(identity *model.OpenCodeGoIdentity, workspaceUID string) *model.OpenCodeGoWorkspace {
	if identity == nil {
		return nil
	}
	for index := range identity.Workspaces {
		if identity.Workspaces[index].UID == workspaceUID {
			return &identity.Workspaces[index]
		}
	}
	return nil
}

func (service *OpenCodeGoLifecycleService) failOperation(operation *model.OpenCodeGoOperation, actionErr error) error {
	safeErr := errors.New(sanitizeOpenCodeGoError(actionErr))
	finishErr := finishOpenCodeGoOperation(
		operation,
		OpenCodeGoOperationStatusFailed,
		"",
		safeErr,
		service.now(),
	)
	return errors.Join(safeErr, finishErr)
}

func (service *OpenCodeGoLifecycleService) persistChinaModelVerification(
	channelID int,
	workspaceUID string,
	enabled bool,
	actionErr error,
) error {
	now := service.now().Unix()
	message := sanitizeOpenCodeGoError(actionErr)
	return model.DB.Transaction(func(tx *gorm.DB) error {
		var workspace model.OpenCodeGoWorkspace
		if err := model.LockForUpdate(tx).
			Where("channel_id = ? AND uid = ?", channelID, workspaceUID).
			First(&workspace).Error; err != nil {
			return err
		}
		updates := map[string]interface{}{
			"china_models_checked_at": now,
			"china_models_error":      message,
		}
		if actionErr == nil {
			updates["china_models_enabled"] = enabled
		}
		return tx.Model(&model.OpenCodeGoWorkspace{}).Where("id = ?", workspace.ID).Updates(updates).Error
	})
}

func (service *OpenCodeGoLifecycleService) persistReferralVerification(
	channelID int,
	workspaceUID string,
	rewardID string,
	page *OpenCodeGoConsolePage,
) error {
	_, err := service.persistReferralVerificationTx(channelID, workspaceUID, rewardID, page, nil, 0)
	return err
}

func (service *OpenCodeGoLifecycleService) persistReferralVerificationAndFinishOperation(
	channelID int,
	workspaceUID string,
	rewardID string,
	page *OpenCodeGoConsolePage,
	operation *model.OpenCodeGoOperation,
	amount int,
) (*model.OpenCodeGoWorkspace, error) {
	return service.persistReferralVerificationTx(channelID, workspaceUID, rewardID, page, operation, amount)
}

func (service *OpenCodeGoLifecycleService) persistReferralVerificationTx(
	channelID int,
	workspaceUID string,
	rewardID string,
	page *OpenCodeGoConsolePage,
	operation *model.OpenCodeGoOperation,
	amount int,
) (*model.OpenCodeGoWorkspace, error) {
	windows, err := validateOpenCodeGoReferralVerificationPage(page, rewardID)
	if err != nil {
		return nil, err
	}
	observedAt := service.now()
	var refreshed model.OpenCodeGoWorkspace
	err = model.DB.Transaction(func(tx *gorm.DB) error {
		var current model.OpenCodeGoWorkspace
		if err := model.LockForUpdate(tx).
			Where("channel_id = ? AND uid = ?", channelID, workspaceUID).
			Preload("QuotaWindows").
			Preload("Models").
			First(&current).Error; err != nil {
			return err
		}
		if !strings.EqualFold(page.WorkspaceID, current.UpstreamWorkspaceID) {
			return errors.New("OpenCode Go referral verification page is for another workspace")
		}
		if page.Quota.FetchedAt < current.LastSyncedAt || page.Quota.FetchedAt < current.QuotaFetchedAt {
			return errors.New("OpenCode Go referral verification page is older than the stored snapshot")
		}

		for index := range windows {
			windows[index].WorkspaceID = current.ID
		}
		candidate := current
		candidate.MembershipStatus = page.MembershipStatus
		candidate.QuotaSnapshotStatus = model.OpenCodeGoQuotaSnapshotComplete
		candidate.QuotaFetchedAt = page.Quota.FetchedAt
		candidate.QuotaNextRefreshAt = page.Quota.NextRefreshAt
		candidate.QuotaParserVersion = OpenCodeGoSSRParserVersion
		candidate.QuotaError = ""
		candidate.AvailableReferralRewards = page.AvailableReferralRewards
		candidate.UsedReferralRewards = page.UsedReferralRewards
		candidate.ReferralRewardAppliedAt = observedAt.Unix()
		candidate.LastSyncedAt = page.Quota.FetchedAt
		candidate.LastError = ""
		if current.RiskDetectedAt > 0 || current.BulkFailureDetectedAt > 0 {
			candidate.LastError = current.LastError
		}
		candidate.StateReason = ""

		// A verified reward is explicit quota-recovery evidence, so it may
		// replace the pre-reward reset fence with the authoritative post-state.
		reducerCurrent := current
		if reducerCurrent.EffectiveState == model.OpenCodeGoStateQuotaExhausted {
			reducerCurrent.QuotaRecoveryAt = 0
		}
		reduced, applied, err := ReduceOpenCodeGoWorkspaceHealth(
			reducerCurrent,
			candidate,
			windows,
			OpenCodeGoHealthObservation{
				Kind:            OpenCodeGoObservationConsoleSnapshot,
				ObservedAt:      observedAt,
				HasUsableModels: hasUsableOpenCodeGoModels(nil, false, &current),
			},
		)
		if err != nil {
			return err
		}
		if applied {
			candidate = reduced
		} else {
			copyOpenCodeGoWorkspaceHealthOutputs(&candidate, current)
			candidate.LastError = current.LastError
		}

		if err := tx.Model(&model.OpenCodeGoWorkspace{}).
			Where("id = ?", current.ID).
			Updates(map[string]interface{}{
				"membership_status":          candidate.MembershipStatus,
				"effective_state":            candidate.EffectiveState,
				"state_reason":               candidate.StateReason,
				"health_observation":         candidate.HealthObservation,
				"health_observed_at":         candidate.HealthObservedAt,
				"cooldown_until":             candidate.CooldownUntil,
				"quota_snapshot_status":      candidate.QuotaSnapshotStatus,
				"quota_fetched_at":           candidate.QuotaFetchedAt,
				"quota_next_refresh_at":      candidate.QuotaNextRefreshAt,
				"quota_recovery_at":          candidate.QuotaRecoveryAt,
				"quota_parser_version":       candidate.QuotaParserVersion,
				"quota_error":                candidate.QuotaError,
				"available_referral_rewards": candidate.AvailableReferralRewards,
				"used_referral_rewards":      candidate.UsedReferralRewards,
				"referral_reward_applied_at": candidate.ReferralRewardAppliedAt,
				"risk_detected_at":           candidate.RiskDetectedAt,
				"risk_last_checked_at":       candidate.RiskLastCheckedAt,
				"bulk_failure_detected_at":   candidate.BulkFailureDetectedAt,
				"last_synced_at":             candidate.LastSyncedAt,
				"last_error":                 candidate.LastError,
				"updated_at":                 common.GetTimestamp(),
			}).Error; err != nil {
			return err
		}
		if err := tx.Where("workspace_id = ?", current.ID).Delete(&model.OpenCodeGoQuotaWindow{}).Error; err != nil {
			return err
		}
		if err := tx.Create(&windows).Error; err != nil {
			return err
		}
		if operation != nil {
			if amount <= 0 {
				return errors.New("OpenCode Go referral action amount was invalid")
			}
			if err := finishOpenCodeGoOperationTx(
				tx,
				operation,
				OpenCodeGoOperationStatusSucceeded,
				fmt.Sprintf("referral reward applied and verified (amount=%d)", amount),
				nil,
				observedAt,
			); err != nil {
				return err
			}
		}
		refreshed = candidate
		refreshed.QuotaWindows = append([]model.OpenCodeGoQuotaWindow(nil), windows...)
		refreshed.Models = append([]model.OpenCodeGoWorkspaceModel(nil), current.Models...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &refreshed, nil
}

func validateOpenCodeGoReferralVerificationPage(
	page *OpenCodeGoConsolePage,
	rewardID string,
) ([]model.OpenCodeGoQuotaWindow, error) {
	if page == nil || page.MembershipStatus != model.OpenCodeGoMembershipActive ||
		page.Quota == nil || strings.TrimSpace(page.QuotaParseError) != "" {
		return nil, errors.New("OpenCode Go referral verification page is not authoritative")
	}
	if page.AvailableReferralRewards != len(page.AvailableReferralRewardIDs) ||
		page.UsedReferralRewards != len(page.UsedReferralRewardIDs) ||
		countOpenCodeGoReferralReward(page.AvailableReferralRewardIDs, rewardID) != 0 ||
		countOpenCodeGoReferralReward(page.UsedReferralRewardIDs, rewardID) != 1 {
		return nil, errors.New("OpenCode Go referral reward application could not be verified")
	}
	seenRewards := make(map[string]struct{}, page.AvailableReferralRewards+page.UsedReferralRewards)
	for _, rewardIDs := range [][]string{page.AvailableReferralRewardIDs, page.UsedReferralRewardIDs} {
		for _, candidate := range rewardIDs {
			if !openCodeGoReferralRewardIDPattern.MatchString(candidate) {
				return nil, errors.New("OpenCode Go referral verification page is not authoritative")
			}
			if _, exists := seenRewards[candidate]; exists {
				return nil, errors.New("OpenCode Go referral reward application could not be verified")
			}
			seenRewards[candidate] = struct{}{}
		}
	}

	quota := page.Quota
	if quota.FetchedAt <= 0 || quota.NextRefreshAt <= 0 || len(quota.Windows) != len(model.OpenCodeGoQuotaKinds) {
		return nil, errors.New("OpenCode Go referral verification quota snapshot is incomplete")
	}
	expectedKinds := make(map[string]struct{}, len(model.OpenCodeGoQuotaKinds))
	for _, kind := range model.OpenCodeGoQuotaKinds {
		expectedKinds[kind] = struct{}{}
	}
	seenKinds := make(map[string]struct{}, len(quota.Windows))
	windows := make([]model.OpenCodeGoQuotaWindow, 0, len(quota.Windows))
	nextRefreshAt := int64(0)
	for _, window := range quota.Windows {
		if _, expected := expectedKinds[window.Kind]; !expected {
			return nil, errors.New("OpenCode Go referral verification quota snapshot is incomplete")
		}
		if _, duplicate := seenKinds[window.Kind]; duplicate {
			return nil, errors.New("OpenCode Go referral verification quota snapshot is incomplete")
		}
		if math.IsNaN(window.UsedPercent) || math.IsInf(window.UsedPercent, 0) || window.UsedPercent < 0 ||
			window.FetchedAt != quota.FetchedAt || window.ResetSeconds < 0 ||
			window.ResetSeconds > openCodeGoMaxResetSeconds || window.ResetAt < quota.FetchedAt ||
			window.ResetAt-quota.FetchedAt != window.ResetSeconds {
			return nil, errors.New("OpenCode Go referral verification quota snapshot is incomplete")
		}
		seenKinds[window.Kind] = struct{}{}
		if nextRefreshAt == 0 || window.ResetAt < nextRefreshAt {
			nextRefreshAt = window.ResetAt
		}
		windows = append(windows, model.OpenCodeGoQuotaWindow{
			Kind:         window.Kind,
			UsedPercent:  window.UsedPercent,
			ResetSeconds: window.ResetSeconds,
			ResetAt:      window.ResetAt,
			FetchedAt:    window.FetchedAt,
		})
	}
	if len(seenKinds) != len(expectedKinds) || nextRefreshAt != quota.NextRefreshAt {
		return nil, errors.New("OpenCode Go referral verification quota snapshot is incomplete")
	}
	return windows, nil
}

func (service *OpenCodeGoLifecycleService) persistRenewalVerification(
	channelID int,
	workspaceUID string,
	result OpenCodeGoSubscriptionCancellation,
	actionErr error,
) error {
	now := service.now().Unix()
	return model.DB.Transaction(func(tx *gorm.DB) error {
		var workspace model.OpenCodeGoWorkspace
		if err := model.LockForUpdate(tx).
			Where("channel_id = ? AND uid = ?", channelID, workspaceUID).
			First(&workspace).Error; err != nil {
			return err
		}
		updates := map[string]interface{}{
			"renewal_checked_at":   now,
			"renewal_cancel_error": sanitizeOpenCodeGoError(actionErr),
		}
		if actionErr == nil {
			cancelledAt := workspace.RenewalCancelledAt
			if cancelledAt == 0 {
				cancelledAt = now
			}
			updates["renewal_cancelled_at"] = cancelledAt
			updates["subscription_ends_at"] = result.CurrentPeriodEnd
		}
		return tx.Model(&model.OpenCodeGoWorkspace{}).Where("id = ?", workspace.ID).Updates(updates).Error
	})
}

func openCodeGoWorkspaceHasExhaustedQuota(workspace model.OpenCodeGoWorkspace) bool {
	if workspace.MembershipStatus != model.OpenCodeGoMembershipActive ||
		workspace.QuotaSnapshotStatus != model.OpenCodeGoQuotaSnapshotComplete ||
		workspace.EffectiveState != model.OpenCodeGoStateQuotaExhausted {
		return false
	}
	for _, window := range workspace.QuotaWindows {
		if window.UsedPercent >= 100 {
			return true
		}
	}
	return false
}

func openCodeGoPageHasExhaustedQuota(page *OpenCodeGoConsolePage) bool {
	if page == nil || page.Quota == nil {
		return false
	}
	for _, window := range page.Quota.Windows {
		if window.UsedPercent >= 100 {
			return true
		}
	}
	return false
}

func countOpenCodeGoReferralReward(rewardIDs []string, rewardID string) int {
	count := 0
	for _, candidate := range rewardIDs {
		if candidate == rewardID {
			count++
		}
	}
	return count
}

func openCodeGoReferralRewardEligible(
	identity model.OpenCodeGoIdentity,
	workspace model.OpenCodeGoWorkspace,
) bool {
	return openCodeGoReferralRewardEligibleAt(identity, workspace, time.Now())
}

func openCodeGoReferralRewardEligibleAt(
	identity model.OpenCodeGoIdentity,
	workspace model.OpenCodeGoWorkspace,
	now time.Time,
) bool {
	if now.IsZero() {
		now = time.Now()
	}
	if (identity.Status != model.OpenCodeGoIdentityStatusActive && identity.Status != model.OpenCodeGoIdentityStatusStale) ||
		identity.AuthCookieCiphertext == "" ||
		!workspace.ManualEnabled ||
		workspace.MembershipStatus != model.OpenCodeGoMembershipActive ||
		(workspace.SubscriptionEndsAt > 0 && workspace.SubscriptionEndsAt <= now.Unix()) ||
		workspace.RiskDetectedAt > 0 ||
		workspace.BulkFailureDetectedAt > 0 {
		return false
	}
	switch workspace.EffectiveState {
	case model.OpenCodeGoStateManualDisabled,
		model.OpenCodeGoStateAuthError,
		model.OpenCodeGoStateMembershipExpired,
		model.OpenCodeGoStateRiskBlocked,
		model.OpenCodeGoStateBulkDisabled:
		return false
	default:
		return true
	}
}

func normalizeOpenCodeGoReferralRewardLimit(limit int) int {
	if limit < 0 {
		return 0
	}
	if limit > OpenCodeGoMaxReferralRewardsPerRun {
		return OpenCodeGoMaxReferralRewardsPerRun
	}
	return limit
}

func applyOpenCodeGoLifecyclePolicy(config *relaydto.OpenCodeGoConfig, policy OpenCodeGoLifecyclePolicy) {
	if config == nil {
		return
	}
	config.AutoEnableChinaModels = &policy.AutoEnableChinaModels
	config.AutoApplyReferralRewards = &policy.AutoApplyReferralRewards
	limit := normalizeOpenCodeGoReferralRewardLimit(policy.ReferralRewardsMaxPerRun)
	config.ReferralRewardsMaxPerRun = &limit
	config.AutoCancelSubscriptionRenewal = policy.AutoCancelSubscriptionRenewal
}

func sanitizeOpenCodeGoLifecycleSource(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return "system"
	}
	return source
}
