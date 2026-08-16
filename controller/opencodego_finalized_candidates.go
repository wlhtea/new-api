package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/relay/channel/opencodego"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/gin-gonic/gin"
)

const (
	openCodeFinalizedCandidatePlansContextKey = "opencode_finalized_candidate_plans_v1"
	openCodeFinalizedSensitiveScanRuleID      = "request.security.finalized-string"
	maxOpenCodeFinalizedCandidateCount        = 64
	maxOpenCodeFinalizedCandidateBytes        = 256 << 20
	defaultOpenCodeCompletionReservation      = 8192
)

type openCodeFinalizedCandidatePlan struct {
	key                       opencodego.RequestPreflightPlanKey
	body                      []byte
	estimatedPromptTokens     int
	estimatedCompletionTokens int
}

type openCodeFinalizedCandidatePlans struct {
	plans            []openCodeFinalizedCandidatePlan
	basePromptTokens int
}

func prepareOpenCodeFinalizedCandidatePlans(
	c *gin.Context,
	info *relaycommon.RelayInfo,
) (*openCodeFinalizedCandidatePlans, *types.NewAPIError) {
	if c == nil || info == nil || !constant.IsOpenCodeChannelType(common.GetContextKeyInt(c, constant.ContextKeyChannelType)) {
		return nil, nil
	}

	type candidate struct {
		key       opencodego.RequestPreflightPlanKey
		selection *frozenOpenCodeAPIKeySelection
	}
	candidates := make([]candidate, 0, 1)
	if common.GetContextKeyInt(c, constant.ContextKeyChannelType) == constant.ChannelTypeOpenCodeAPIKey {
		snapshot, found, err := getOpenCodeAPIKeyRetrySnapshot(c)
		if err != nil || !found || snapshot == nil {
			if err == nil {
				err = errors.New("OpenCode API-key retry snapshot is unavailable")
			}
			return nil, newOpenCodeRetrySnapshotAPIError(c, err)
		}
		if len(snapshot.topology) > maxOpenCodeFinalizedCandidateCount {
			return nil, newOpenCodeRetrySnapshotAPIError(c, errors.New("OpenCode candidate count exceeds planning limit"))
		}
		for index := range snapshot.topology {
			selection := &snapshot.topology[index]
			candidates = append(candidates, candidate{
				key: opencodego.RequestPreflightPlanKey{
					SelectionGroup: selection.selectionGroup,
					ChannelID:      selection.channelID,
				},
				selection: selection,
			})
		}
	} else {
		candidates = append(candidates, candidate{key: opencodego.RequestPreflightPlanKey{
			SelectionGroup: relaycommon.ResolveSelectionGroup(c, info),
			ChannelID:      common.GetContextKeyInt(c, constant.ContextKeyChannelId),
		}})
	}

	result := &openCodeFinalizedCandidatePlans{plans: make([]openCodeFinalizedCandidatePlan, 0, len(candidates))}
	aggregateBytes := 0
	for _, candidate := range candidates {
		candidateContext := c.Copy()
		if candidate.selection != nil {
			if err := candidate.selection.apply(candidateContext); err != nil {
				return nil, newOpenCodeRetrySnapshotAPIError(c, err)
			}
		}
		if err := relaycommon.ValidateParamOverrideRequestStable(
			common.GetContextKeyStringMap(candidateContext, constant.ContextKeyChannelParamOverride),
		); err != nil {
			return nil, newOpenCodeRequestPreflightAPIError(
				c,
				opencodego.NewRequestPreflightCandidateConfigError(err),
			)
		}
		preflightPlan, found, planLookupErr := opencodego.GetRequestPreflightPlanForSelection(
			candidateContext,
			candidate.key.SelectionGroup,
			candidate.key.ChannelID,
		)
		if planLookupErr != nil || !found {
			if planLookupErr == nil {
				planLookupErr = errors.New("OpenCode candidate has no request preflight plan")
			}
			return nil, newOpenCodeRetrySnapshotAPIError(c, planLookupErr)
		}
		body, planErr := relay.PlanOpenCodeOutboundRequest(candidateContext, info)
		if planErr != nil {
			return nil, newOpenCodeRequestPreflightAPIError(
				c,
				opencodego.NewRequestPreflightFinalizationError(planErr),
			)
		}
		if len(body) == 0 || aggregateBytes > maxOpenCodeFinalizedCandidateBytes-len(body) {
			return nil, newOpenCodeRetrySnapshotAPIError(c, errors.New("OpenCode finalized candidate bodies exceed planning limit"))
		}
		aggregateBytes += len(body)
		estimatedPromptTokens, promptEstimateErr := finalizedCandidatePromptReservation(
			candidateContext.Request.Context(),
			body,
			preflightPlan.FinalModel,
		)
		if promptEstimateErr != nil {
			return nil, newOpenCodeRequestPreflightAPIError(
				c,
				opencodego.NewRequestPreflightCandidateConfigError(promptEstimateErr),
			)
		}
		estimatedCompletionTokens, estimateErr := finalizedCandidateCompletionReservation(body)
		if estimateErr != nil {
			return nil, newOpenCodeRequestPreflightAPIError(
				c,
				opencodego.NewRequestPreflightCandidateConfigError(estimateErr),
			)
		}
		result.plans = append(result.plans, openCodeFinalizedCandidatePlan{
			key:                       candidate.key,
			body:                      append([]byte(nil), body...),
			estimatedPromptTokens:     estimatedPromptTokens,
			estimatedCompletionTokens: estimatedCompletionTokens,
		})
	}
	if len(result.plans) == 0 {
		return nil, newOpenCodeRetrySnapshotAPIError(c, errors.New("OpenCode finalized candidate set is empty"))
	}
	c.Set(openCodeFinalizedCandidatePlansContextKey, result)
	if err := relay.RequireOpenCodeOutboundPlanBinding(c); err != nil {
		return nil, newOpenCodeRetrySnapshotAPIError(c, err)
	}
	return result, nil
}

func (plans *openCodeFinalizedCandidatePlans) billingViews(basePromptTokens int) []helper.BillingCandidateView {
	if plans == nil {
		return nil
	}
	if basePromptTokens < 0 {
		basePromptTokens = 0
	}
	plans.basePromptTokens = basePromptTokens
	views := make([]helper.BillingCandidateView, 0, len(plans.plans))
	for _, plan := range plans.plans {
		estimatedPromptTokens := max(basePromptTokens, plan.estimatedPromptTokens)
		views = append(views, helper.BillingCandidateView{
			SelectionGroup:            plan.key.SelectionGroup,
			Body:                      append([]byte(nil), plan.body...),
			EstimatedPromptTokens:     estimatedPromptTokens,
			EstimatedCompletionTokens: plan.estimatedCompletionTokens,
		})
	}
	return views
}

func finalizedCandidatePromptReservation(ctx context.Context, body []byte, model string) (int, error) {
	total := 0
	err := helper.VisitJSONStringValues(ctx, body, func(value string) error {
		// A tokenizer/heuristic estimate can undercount an adversarially long
		// unbroken string. UTF-8 byte length is a conservative upper bound for
		// byte-fallback tokenizers, so use the larger value for reservation.
		tokens := max(service.CountTextToken(value, model), len(value))
		if tokens < 0 || total > int(^uint(0)>>1)-tokens {
			return errors.New("finalized OpenCode prompt reservation overflows")
		}
		total += tokens
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("estimate finalized OpenCode prompt tokens: %w", err)
	}
	return total, nil
}

func finalizedCandidateCompletionReservation(body []byte) (int, error) {
	var fields map[string]json.RawMessage
	if err := common.Unmarshal(body, &fields); err != nil || fields == nil {
		return 0, errors.New("finalized OpenCode candidate is not a JSON object")
	}

	completionCap := 0
	for _, name := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "max_new_tokens"} {
		value, present, err := finalizedCandidateIntegerField(fields, name)
		if err != nil {
			return 0, err
		}
		if present && value > completionCap {
			completionCap = value
		}
	}
	if rawThinking, found := fields["thinking"]; found && !bytes.Equal(bytes.TrimSpace(rawThinking), []byte("null")) {
		var thinking map[string]json.RawMessage
		if err := common.Unmarshal(rawThinking, &thinking); err != nil {
			return 0, errors.New("finalized OpenCode thinking control is invalid")
		}
		budget, present, err := finalizedCandidateIntegerField(thinking, "budget_tokens")
		if err != nil {
			return 0, err
		}
		if present && budget > 0 {
			if completionCap > int(^uint(0)>>1)-budget {
				return 0, errors.New("finalized OpenCode completion bound overflows")
			}
			completionCap += budget
		}
	}
	if completionCap <= 0 {
		completionCap = defaultOpenCodeCompletionReservation
	}

	multiplier := 1
	for _, name := range []string{"n", "best_of", "num_return_sequences"} {
		value, present, err := finalizedCandidateIntegerField(fields, name)
		if err != nil {
			return 0, err
		}
		if !present || value <= 1 {
			continue
		}
		if multiplier > int(^uint(0)>>1)/value {
			return 0, errors.New("finalized OpenCode output multiplier overflows")
		}
		multiplier *= value
	}
	if completionCap > int(^uint(0)>>1)/multiplier {
		return 0, errors.New("finalized OpenCode completion reservation overflows")
	}
	return completionCap * multiplier, nil
}

func finalizedCandidateIntegerField(fields map[string]json.RawMessage, name string) (int, bool, error) {
	raw, found := fields[name]
	if !found || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false, nil
	}
	text := string(bytes.TrimSpace(raw))
	value, err := strconv.ParseUint(text, 10, 63)
	if err != nil || value > uint64(^uint(0)>>1) {
		return 0, true, fmt.Errorf("finalized OpenCode %s must be a non-negative integer", name)
	}
	return int(value), true, nil
}

func (plans *openCodeFinalizedCandidatePlans) find(c *gin.Context, info *relaycommon.RelayInfo) (openCodeFinalizedCandidatePlan, bool) {
	if plans == nil || c == nil {
		return openCodeFinalizedCandidatePlan{}, false
	}
	key := opencodego.RequestPreflightPlanKey{
		SelectionGroup: relaycommon.ResolveSelectionGroup(c, info),
		ChannelID:      common.GetContextKeyInt(c, constant.ContextKeyChannelId),
	}
	for _, plan := range plans.plans {
		if plan.key == key {
			clone := plan
			clone.body = append([]byte(nil), plan.body...)
			return clone, true
		}
	}
	return openCodeFinalizedCandidatePlan{}, false
}

func bindOpenCodeFinalizedCandidateBilling(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	if c == nil || info == nil || !constant.IsOpenCodeChannelType(common.GetContextKeyInt(c, constant.ContextKeyChannelType)) {
		return nil
	}
	value, found := c.Get(openCodeFinalizedCandidatePlansContextKey)
	plans, ok := value.(*openCodeFinalizedCandidatePlans)
	if !found || !ok || plans == nil {
		return newOpenCodeRetrySnapshotAPIError(c, errors.New("OpenCode finalized candidate plans are unavailable"))
	}
	plan, found := plans.find(c, info)
	if !found {
		return newOpenCodeRetrySnapshotAPIError(c, errors.New("selected OpenCode candidate has no finalized billing view"))
	}
	if err := relay.BindOpenCodeOutboundPlan(c, info, plan.body); err != nil {
		return newOpenCodeRetrySnapshotAPIError(c, err)
	}
	estimatedPromptTokens := max(plans.basePromptTokens, plan.estimatedPromptTokens)
	info.SetEstimatePromptTokens(estimatedPromptTokens)
	input := billingexpr.RequestInput{
		Headers: make(map[string]string, len(info.RequestHeaders)),
		Body:    plan.body,
	}
	for key, value := range info.RequestHeaders {
		input.Headers[key] = value
	}
	if err := service.BindTieredBillingCandidate(
		info,
		input,
		estimatedPromptTokens,
		plan.estimatedCompletionTokens,
	); err != nil {
		return types.NewOpenAIError(
			errors.New("OpenCode finalized candidate billing invariant failed"),
			types.ErrorCodeModelPriceError,
			http.StatusInternalServerError,
			types.ErrOptionWithSkipRetry(),
			types.ErrOptionWithProvenance(types.ErrorProvenance{
				Origin:  types.ErrorOriginGatewayInvariant,
				Subtype: "billing.finalized-candidate",
			}),
		)
	}
	return nil
}

func preflightOpenCodeFinalizedCandidateSensitiveValues(
	c *gin.Context,
	plans *openCodeFinalizedCandidatePlans,
) *types.NewAPIError {
	if c == nil || plans == nil || !setting.ShouldCheckPromptSensitive() {
		return nil
	}
	matched := false
	matchCount := 0
	for _, plan := range plans.plans {
		err := helper.VisitJSONStringValues(c.Request.Context(), plan.body, func(value string) error {
			contains, words := service.CheckSensitiveText(value)
			if contains {
				matched = true
				matchCount += len(words)
			}
			return nil
		})
		if err != nil {
			statusCode := http.StatusInternalServerError
			origin := types.ErrorOriginGatewayInvariant
			subtype := openCodeFinalizedSensitiveScanRuleID + ".scan-failed"
			message := "request security validation failed"
			if errors.Is(err, context.Canceled) {
				statusCode = 499
				origin = types.ErrorOriginLocalCancel
				subtype = openCodeFinalizedSensitiveScanRuleID + ".cancelled"
				message = "request was canceled"
			} else if errors.Is(err, context.DeadlineExceeded) {
				statusCode = http.StatusGatewayTimeout
				origin = types.ErrorOriginLocalDeadline
				subtype = openCodeFinalizedSensitiveScanRuleID + ".deadline"
				message = "request validation timed out"
			}
			logger.LogWarn(c.Request.Context(), fmt.Sprintf(
				"relay finalized request security scan failed: rule_id=%s status=%d",
				openCodeFinalizedSensitiveScanRuleID,
				statusCode,
			))
			return types.NewOpenAIError(
				errors.New(message),
				types.ErrorCodeBadResponse,
				statusCode,
				types.ErrOptionWithSkipRetry(),
				types.ErrOptionWithNoRecordErrorLog(),
				types.ErrOptionWithProvenance(types.ErrorProvenance{Origin: origin, Subtype: subtype}),
			)
		}
	}
	if !matched {
		return nil
	}
	logger.LogWarn(c.Request.Context(), fmt.Sprintf(
		"relay finalized request security rejected: rule_id=%s match_count=%d",
		openCodeFinalizedSensitiveScanRuleID,
		matchCount,
	))
	return types.NewOpenAIError(
		errors.New("request contains sensitive content"),
		types.ErrorCodeSensitiveWordsDetected,
		http.StatusBadRequest,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithNoRecordErrorLog(),
		types.ErrOptionWithProvenance(types.ErrorProvenance{
			Origin:  types.ErrorOriginLocalValidation,
			Subtype: openCodeFinalizedSensitiveScanRuleID,
		}),
	)
}
