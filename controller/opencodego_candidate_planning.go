package controller

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/relay/channel/opencodego"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

const (
	openCodeCapabilityUnsupportedRule = "request.effort.capability-unsupported"
	openCodeCapabilityUnknownRule     = "request.effort.capability-unknown"
	openCodeCapabilityStage           = "preflight.capability"
)

type openCodeCandidateFailureClass uint8

const (
	openCodeCandidateFailureClient openCodeCandidateFailureClass = iota + 1
	openCodeCandidateFailureCapability
	openCodeCandidateFailureConfig
	openCodeCandidateFailureFatal
)

type openCodeCandidateFailure struct {
	class openCodeCandidateFailureClass
	err   error
}

type openCodeCandidateDraft struct {
	key       opencodego.RequestPreflightPlanKey
	selection *frozenOpenCodeAPIKeySelection
	channel   *model.Channel
}

type openCodeRetainedCandidate struct {
	draft     openCodeCandidateDraft
	preflight opencodego.RequestPreflightPlan
	finalized openCodeFinalizedCandidatePlan
}

func prepareAndFreezeOpenCodeCandidatePlans(
	c *gin.Context,
	info *relaycommon.RelayInfo,
) (*openCodeFinalizedCandidatePlans, *types.NewAPIError) {
	if c == nil || info == nil || !constant.IsOpenCodeChannelType(common.GetContextKeyInt(c, constant.ContextKeyChannelType)) {
		return nil, nil
	}

	capabilityView := service.CurrentOpenCodeGoCapabilityView()
	drafts, initialKey, groupOrder, specific, failures, fatalErr := enumerateOpenCodeCandidateDrafts(c, info)
	if fatalErr != nil {
		return nil, newOpenCodeRetrySnapshotAPIError(c, fatalErr)
	}
	if len(drafts) > maxOpenCodeFinalizedCandidateCount {
		return nil, newOpenCodeRetrySnapshotAPIError(c, errors.New("OpenCode candidate count exceeds planning limit"))
	}

	retained := make([]openCodeRetainedCandidate, 0, len(drafts))
	aggregateBytes := 0
	for _, draft := range drafts {
		candidate, failure := planOpenCodeCandidate(c, info, draft, capabilityView)
		if failure != nil {
			if failure.class == openCodeCandidateFailureFatal {
				return nil, newOpenCodeRequestPreflightAPIError(c, failure.err)
			}
			failures = append(failures, *failure)
			continue
		}
		if len(candidate.finalized.body) == 0 ||
			aggregateBytes > maxOpenCodeFinalizedCandidateBytes-len(candidate.finalized.body) {
			return nil, newOpenCodeRetrySnapshotAPIError(c, errors.New("OpenCode finalized candidate bodies exceed planning limit"))
		}
		aggregateBytes += len(candidate.finalized.body)
		retained = append(retained, candidate)
	}

	if len(retained) == 0 {
		return nil, reduceOpenCodeCandidateFailures(c, failures)
	}
	return commitOpenCodeCandidatePlans(
		c,
		info,
		retained,
		initialKey,
		groupOrder,
		specific,
		capabilityView.SemanticRevision(),
	)
}

func enumerateOpenCodeCandidateDrafts(
	c *gin.Context,
	info *relaycommon.RelayInfo,
) (
	[]openCodeCandidateDraft,
	opencodego.RequestPreflightPlanKey,
	[]string,
	bool,
	[]openCodeCandidateFailure,
	error,
) {
	channelType := common.GetContextKeyInt(c, constant.ContextKeyChannelType)
	initialKey := opencodego.RequestPreflightPlanKey{
		SelectionGroup: relaycommon.ResolveSelectionGroup(c, info),
		ChannelID:      common.GetContextKeyInt(c, constant.ContextKeyChannelId),
	}
	if strings.TrimSpace(initialKey.SelectionGroup) == "" || initialKey.ChannelID <= 0 {
		return nil, initialKey, nil, false, nil, errors.New("initial OpenCode selection is invalid")
	}
	if channelType != constant.ChannelTypeOpenCodeAPIKey {
		return []openCodeCandidateDraft{{key: initialKey}}, initialKey,
			[]string{initialKey.SelectionGroup}, true, nil, nil
	}

	_, specific := c.Get(string(constant.ContextKeyTokenSpecificChannelId))
	if specific {
		selection, err := captureFrozenOpenCodeAPIKeySelection(c, initialKey.SelectionGroup)
		if err != nil {
			return nil, initialKey, nil, true, nil, err
		}
		var channel *model.Channel
		if strings.TrimSpace(selection.channelKey) == "" {
			var found bool
			channel, found = middleware.SelectedChannelPlanningSource(c)
			if !found || channel.Id != initialKey.ChannelID {
				return nil, initialKey, nil, true, nil,
					errors.New("selected OpenCode API-key planning source is unavailable")
			}
		}
		return []openCodeCandidateDraft{{key: initialKey, selection: &selection, channel: channel}}, initialKey,
			[]string{initialKey.SelectionGroup}, true, nil, nil
	}

	candidates, topologyErr := service.SnapshotOpenCodeAPIKeyCandidateTopology(
		c,
		info.TokenGroup,
		info.UserGroup,
		info.OriginModelName,
		c.Request.URL.Path,
	)
	if topologyErr != nil {
		if errors.Is(topologyErr, service.ErrOpenCodeAPIKeyMixedTopology) ||
			errors.Is(topologyErr, service.ErrOpenCodeAPIKeyInconsistentTopology) {
			return nil, initialKey, nil, false, []openCodeCandidateFailure{{
				class: openCodeCandidateFailureConfig,
				err:   opencodego.NewRequestPreflightCandidateConfigError(topologyErr),
			}}, nil
		}
		return nil, initialKey, nil, false, nil, topologyErr
	}
	if len(candidates) > maxOpenCodeFinalizedCandidateCount {
		return nil, initialKey, nil, false, nil, errors.New("OpenCode candidate count exceeds planning limit")
	}

	allGroupOrder := make([]string, 0)
	seenGroups := make(map[string]struct{})
	initialSeen := false
	for _, candidate := range candidates {
		if candidate.Channel == nil {
			return nil, initialKey, nil, false, nil, errors.New("OpenCode API-key topology contains an empty channel")
		}
		group := strings.TrimSpace(candidate.SelectionGroup)
		if _, found := seenGroups[group]; !found {
			seenGroups[group] = struct{}{}
			allGroupOrder = append(allGroupOrder, group)
		}
		if group == initialKey.SelectionGroup && candidate.Channel.Id == initialKey.ChannelID {
			initialSeen = true
		}
	}
	if !initialSeen {
		return nil, initialKey, nil, false, nil,
			errors.New("selected OpenCode API-key channel is absent from its eligible topology")
	}

	permittedGroups, err := permittedOpenCodeCandidateGroups(
		allGroupOrder,
		initialKey.SelectionGroup,
		info.TokenGroup,
		common.GetContextKeyBool(c, constant.ContextKeyTokenCrossGroupRetry),
	)
	if err != nil {
		return nil, initialKey, nil, false, nil, err
	}
	permitted := make(map[string]struct{}, len(permittedGroups))
	for _, group := range permittedGroups {
		permitted[group] = struct{}{}
	}

	drafts := make([]openCodeCandidateDraft, 0, len(candidates))
	failures := make([]openCodeCandidateFailure, 0)
	for _, candidate := range candidates {
		selectionGroup := strings.TrimSpace(candidate.SelectionGroup)
		if _, allowed := permitted[selectionGroup]; !allowed {
			continue
		}
		candidateContext := c.Copy()
		if setupErr := middleware.SetupContextForSelectedChannelPlanning(
			candidateContext,
			candidate.Channel,
			info.OriginModelName,
		); setupErr != nil {
			failures = append(failures, openCodeCandidateFailure{
				class: openCodeCandidateFailureConfig,
				err:   opencodego.NewRequestPreflightCandidateConfigError(setupErr),
			})
			continue
		}
		common.SetContextKey(candidateContext, constant.ContextKeyAutoGroup, selectionGroup)
		selection, captureErr := captureFrozenOpenCodeAPIKeySelection(candidateContext, selectionGroup)
		if captureErr != nil {
			failures = append(failures, openCodeCandidateFailure{
				class: openCodeCandidateFailureConfig,
				err:   opencodego.NewRequestPreflightCandidateConfigError(captureErr),
			})
			continue
		}
		selection.priority = candidate.Priority
		selection.weight = candidate.Weight
		key := opencodego.RequestPreflightPlanKey{
			SelectionGroup: selectionGroup,
			ChannelID:      candidate.Channel.Id,
		}
		drafts = append(drafts, openCodeCandidateDraft{
			key:       key,
			selection: &selection,
			channel:   candidate.Channel,
		})
	}
	return drafts, initialKey, permittedGroups, false, failures, nil
}

func permittedOpenCodeCandidateGroups(
	groupOrder []string,
	initialGroup string,
	tokenGroup string,
	crossGroupRetry bool,
) ([]string, error) {
	initialIndex := -1
	for index, group := range groupOrder {
		if group == strings.TrimSpace(initialGroup) {
			initialIndex = index
			break
		}
	}
	if initialIndex < 0 {
		return nil, errors.New("initial OpenCode API-key selection group is unavailable")
	}
	last := initialIndex + 1
	if strings.TrimSpace(tokenGroup) == "auto" && crossGroupRetry {
		last = len(groupOrder)
	}
	return append([]string(nil), groupOrder[initialIndex:last]...), nil
}

func planOpenCodeCandidate(
	root *gin.Context,
	rootInfo *relaycommon.RelayInfo,
	draft openCodeCandidateDraft,
	capabilityView service.OpenCodeGoCapabilityView,
) (openCodeRetainedCandidate, *openCodeCandidateFailure) {
	candidateContext, bodyReader, err := cloneOpenCodePlanningContext(root)
	if err != nil {
		return openCodeRetainedCandidate{}, fatalOpenCodeCandidateFailure(err)
	}
	if bodyReader != nil {
		defer bodyReader.Close()
	}
	if draft.selection != nil {
		if err := draft.selection.apply(candidateContext); err != nil {
			return openCodeRetainedCandidate{}, configOpenCodeCandidateFailure(err)
		}
	}
	candidateInfo, err := rootInfo.CloneForOpenCodePlanning()
	if err != nil {
		return openCodeRetainedCandidate{}, fatalOpenCodeCandidateFailure(err)
	}
	candidateInfo.UsingGroup = draft.key.SelectionGroup

	if err := relaycommon.ValidateParamOverrideRequestStable(
		common.GetContextKeyStringMap(candidateContext, constant.ContextKeyChannelParamOverride),
	); err != nil {
		return openCodeRetainedCandidate{}, configOpenCodeCandidateFailure(err)
	}
	preflightPlan, err := opencodego.BuildRequestPreflightPlan(candidateContext, candidateInfo)
	if err != nil {
		return openCodeRetainedCandidate{}, classifyOpenCodeCandidateFailure(err)
	}
	preflightPlan.CapabilityRevision = capabilityView.SemanticRevision()
	if preflightPlan.Key() != draft.key {
		return openCodeRetainedCandidate{}, fatalOpenCodeCandidateFailure(
			errors.New("OpenCode candidate selection does not match its preflight plan"),
		)
	}
	if err := opencodego.StoreRequestPreflightPlan(candidateContext, preflightPlan); err != nil {
		return openCodeRetainedCandidate{}, fatalOpenCodeCandidateFailure(err)
	}

	body, planErr := relay.PlanOpenCodeOutboundRequest(candidateContext, candidateInfo)
	if planErr != nil {
		return openCodeRetainedCandidate{}, classifyOpenCodeCandidateFailure(
			opencodego.NewRequestPreflightFinalizationError(planErr),
		)
	}
	effort, found, effortErr := opencodego.GetFinalEffortSelection(candidateContext)
	if effortErr != nil || !found {
		if effortErr == nil {
			effortErr = errors.New("OpenCode finalized effort selection is unavailable")
		}
		return openCodeRetainedCandidate{}, fatalOpenCodeCandidateFailure(effortErr)
	}
	if effort.Present && !effort.Null {
		switch capabilityView.CheckEffort(preflightPlan.FinalModel, effort.Value) {
		case service.OpenCodeGoCapabilitySupported:
		case service.OpenCodeGoCapabilityUnsupported:
			if effort.Origin == opencodego.EffortSelectorOriginOperatorOverride {
				return openCodeRetainedCandidate{}, configOpenCodeCandidateFailure(
					errors.New("operator effort is unsupported by the finalized model"),
				)
			}
			return openCodeRetainedCandidate{}, clientUnsupportedOpenCodeCandidateFailure()
		default:
			return openCodeRetainedCandidate{}, &openCodeCandidateFailure{
				class: openCodeCandidateFailureCapability,
				err:   errors.New("OpenCode effort capability is unavailable"),
			}
		}
	}

	estimatedPromptTokens, promptErr := finalizedCandidatePromptReservation(
		candidateContext.Request.Context(),
		body,
		preflightPlan.FinalModel,
	)
	if promptErr != nil {
		return openCodeRetainedCandidate{}, configOpenCodeCandidateFailure(promptErr)
	}
	estimatedCompletionTokens, completionErr := finalizedCandidateCompletionReservation(body)
	if completionErr != nil {
		return openCodeRetainedCandidate{}, configOpenCodeCandidateFailure(completionErr)
	}
	return openCodeRetainedCandidate{
		draft:     draft,
		preflight: preflightPlan,
		finalized: openCodeFinalizedCandidatePlan{
			key:                       draft.key,
			body:                      append([]byte(nil), body...),
			effort:                    effort,
			capabilityRevision:        capabilityView.SemanticRevision(),
			estimatedPromptTokens:     estimatedPromptTokens,
			estimatedCompletionTokens: estimatedCompletionTokens,
		},
	}, nil
}

func cloneOpenCodePlanningContext(c *gin.Context) (*gin.Context, io.ReadCloser, error) {
	if c == nil || c.Request == nil {
		return nil, nil, errors.New("OpenCode planning context is invalid")
	}
	clone := c.Copy()
	request := c.Request.Clone(c.Request.Context())
	request.Header = c.Request.Header.Clone()
	if c.Request.URL != nil {
		urlCopy := *c.Request.URL
		request.URL = &urlCopy
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, nil, err
	}
	reader, err := storage.NewReader()
	if err != nil {
		return nil, nil, err
	}
	request.Body = reader
	request.GetBody = storage.NewReader
	request.ContentLength = storage.Size()
	clone.Request = request
	return clone, reader, nil
}

func commitOpenCodeCandidatePlans(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	retained []openCodeRetainedCandidate,
	initialKey opencodego.RequestPreflightPlanKey,
	groupOrder []string,
	specific bool,
	capabilityRevision string,
) (*openCodeFinalizedCandidatePlans, *types.NewAPIError) {
	channelType := common.GetContextKeyInt(c, constant.ContextKeyChannelType)
	preflightPlans := make([]opencodego.RequestPreflightPlan, 0, len(retained))
	result := &openCodeFinalizedCandidatePlans{plans: make([]openCodeFinalizedCandidatePlan, 0, len(retained))}
	topology := make([]frozenOpenCodeAPIKeySelection, 0, len(retained))
	keySources := make(map[opencodego.RequestPreflightPlanKey]*model.Channel, len(retained))
	seen := make(map[opencodego.RequestPreflightPlanKey]struct{}, len(retained))
	var initialSelection frozenOpenCodeAPIKeySelection
	initialFound := false
	for _, candidate := range retained {
		if candidate.preflight.Key() != candidate.draft.key || candidate.finalized.key != candidate.draft.key ||
			candidate.preflight.CapabilityRevision != capabilityRevision ||
			candidate.finalized.capabilityRevision != capabilityRevision {
			return nil, newOpenCodeRetrySnapshotAPIError(c, errors.New("OpenCode retained candidate revisions or keys disagree"))
		}
		if _, duplicate := seen[candidate.draft.key]; duplicate {
			return nil, newOpenCodeRetrySnapshotAPIError(c, errors.New("OpenCode retained candidate key is duplicated"))
		}
		seen[candidate.draft.key] = struct{}{}
		preflightPlans = append(preflightPlans, candidate.preflight)
		result.plans = append(result.plans, candidate.finalized)
		if candidate.draft.selection != nil {
			topology = append(topology, *candidate.draft.selection)
			if candidate.draft.channel != nil {
				keySources[candidate.draft.key] = candidate.draft.channel
			}
			if candidate.draft.key == initialKey {
				initialSelection = *candidate.draft.selection
				initialFound = true
			}
		}
	}

	var retrySnapshot *openCodeAPIKeyRetrySnapshot
	if channelType == constant.ChannelTypeOpenCodeAPIKey {
		if len(topology) != len(preflightPlans) || len(topology) != len(result.plans) {
			return nil, newOpenCodeRetrySnapshotAPIError(c, errors.New("OpenCode retained topology and plan sets disagree"))
		}
		if !initialFound {
			replacement, err := selectCompatibleOpenCodeAPIKeyInitialSelection(
				topology,
				groupOrder,
				initialKey.SelectionGroup,
				info.TokenGroup,
				common.GetContextKeyBool(c, constant.ContextKeyTokenCrossGroupRetry),
				common.MemoryCacheEnabled,
				common.GetRandomInt,
			)
			if err != nil {
				return nil, newOpenCodeRetrySnapshotAPIError(c, err)
			}
			initialSelection = replacement
			initialKey = opencodego.RequestPreflightPlanKey{
				SelectionGroup: replacement.selectionGroup,
				ChannelID:      replacement.channelID,
			}
		}
		selections := []frozenOpenCodeAPIKeySelection{initialSelection}
		if !specific {
			var err error
			selections, err = deriveOpenCodeAPIKeyPhysicalSchedule(
				topology,
				initialKey,
				info.TokenGroup,
				common.GetContextKeyBool(c, constant.ContextKeyTokenCrossGroupRetry),
				common.RetryTimes,
			)
			if err != nil {
				return nil, newOpenCodeRetrySnapshotAPIError(c, err)
			}
		}
		for _, selection := range selections {
			key := opencodego.RequestPreflightPlanKey{
				SelectionGroup: selection.selectionGroup,
				ChannelID:      selection.channelID,
			}
			if _, found := seen[key]; !found {
				return nil, newOpenCodeRetrySnapshotAPIError(c, errors.New("OpenCode physical schedule references a discarded candidate"))
			}
		}
		retrySnapshot = &openCodeAPIKeyRetrySnapshot{
			version:      openCodeAPIKeyRetrySnapshotVersion,
			topology:     topology,
			selections:   selections,
			keySources:   keySources,
			materialized: make(map[opencodego.RequestPreflightPlanKey]frozenOpenCodeAPIKeyCredential),
		}
	}

	// Everything above is side-effect free. Commit request-scoped state only
	// after topology, registry, schedule, finalized bodies, and revisions agree.
	if retrySnapshot != nil {
		if err := initialSelection.apply(c); err != nil {
			return nil, newOpenCodeRetrySnapshotAPIError(c, err)
		}
	}
	if err := opencodego.StoreRequestPreflightPlans(c, preflightPlans); err != nil {
		return nil, newOpenCodeRetrySnapshotAPIError(c, err)
	}
	if retrySnapshot != nil {
		c.Set(openCodeAPIKeyRetrySnapshotContextKey, retrySnapshot)
	}
	c.Set(openCodeFinalizedCandidatePlansContextKey, result)
	if err := relay.RequireOpenCodeOutboundPlanBinding(c); err != nil {
		return nil, newOpenCodeRetrySnapshotAPIError(c, err)
	}
	return result, nil
}

func classifyOpenCodeCandidateFailure(err error) *openCodeCandidateFailure {
	preflightErr, ok := opencodego.AsRequestPreflightError(err)
	if !ok || preflightErr == nil {
		return fatalOpenCodeCandidateFailure(err)
	}
	switch preflightErr.Origin {
	case types.ErrorOriginLocalValidation:
		return &openCodeCandidateFailure{class: openCodeCandidateFailureClient, err: err}
	case types.ErrorOriginGatewayConfig:
		return &openCodeCandidateFailure{class: openCodeCandidateFailureConfig, err: err}
	case types.ErrorOriginGatewayInvariant:
		return &openCodeCandidateFailure{class: openCodeCandidateFailureFatal, err: err}
	default:
		return fatalOpenCodeCandidateFailure(err)
	}
}

func fatalOpenCodeCandidateFailure(err error) *openCodeCandidateFailure {
	return &openCodeCandidateFailure{
		class: openCodeCandidateFailureFatal,
		err:   opencodego.NewRequestPreflightPlanStorageError(err),
	}
}

func configOpenCodeCandidateFailure(err error) *openCodeCandidateFailure {
	return &openCodeCandidateFailure{
		class: openCodeCandidateFailureConfig,
		err:   opencodego.NewRequestPreflightCandidateConfigError(err),
	}
}

func clientUnsupportedOpenCodeCandidateFailure() *openCodeCandidateFailure {
	return &openCodeCandidateFailure{
		class: openCodeCandidateFailureClient,
		err: &opencodego.RequestPreflightError{
			StatusCode: http.StatusBadRequest,
			Origin:     types.ErrorOriginLocalValidation,
			RuleID:     openCodeCapabilityUnsupportedRule,
			StageID:    openCodeCapabilityStage,
			Message:    "OpenCode effort is unsupported by the finalized model",
		},
	}
}

func reduceOpenCodeCandidateFailures(c *gin.Context, failures []openCodeCandidateFailure) *types.NewAPIError {
	var configErr error
	var capabilityErr error
	var clientErr error
	for _, failure := range failures {
		switch failure.class {
		case openCodeCandidateFailureFatal:
			return newOpenCodeRequestPreflightAPIError(c, failure.err)
		case openCodeCandidateFailureConfig:
			if configErr == nil {
				configErr = failure.err
			}
		case openCodeCandidateFailureCapability:
			if capabilityErr == nil {
				capabilityErr = failure.err
			}
		case openCodeCandidateFailureClient:
			if clientErr == nil {
				clientErr = failure.err
			}
		}
	}
	if configErr != nil {
		return newOpenCodeRequestPreflightAPIError(c, configErr)
	}
	if capabilityErr != nil {
		_ = opencodego.StoreRequestPreflightRejection(c, opencodego.RequestPreflightRejection{
			RuleID:  openCodeCapabilityUnknownRule,
			StageID: openCodeCapabilityStage,
		})
		return types.NewOpenAIError(
			capabilityErr,
			types.ErrorCodeGetChannelFailed,
			http.StatusServiceUnavailable,
			types.ErrOptionWithSkipRetry(),
			types.ErrOptionWithProvenance(types.ErrorProvenance{
				Origin:  types.ErrorOriginGatewayDependency,
				Subtype: openCodeCapabilityUnknownRule,
			}),
		)
	}
	if clientErr != nil {
		return newOpenCodeRequestPreflightAPIError(c, clientErr)
	}
	return newOpenCodeRequestPreflightAPIError(
		c,
		opencodego.NewRequestPreflightCandidateConfigError(errors.New("OpenCode candidate set is empty")),
	)
}
