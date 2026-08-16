package service

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

// TieredResultWrapper wraps billingexpr.TieredResult for use at the service layer.
type TieredResultWrapper = billingexpr.TieredResult

// BuildTieredTokenParams constructs billingexpr.TokenParams from a dto.Usage,
// normalizing P and C so they mean "tokens not separately priced by the
// expression". Sub-categories (cache, image, audio) are only subtracted
// when the expression references them via their own variable.
//
// GPT-format APIs report prompt_tokens / completion_tokens as totals that
// include all sub-categories (cache, image, audio). Claude-format APIs
// report them as text-only. This function normalizes to text-only when
// sub-categories are separately priced.
func BuildTieredTokenParams(usage *dto.Usage, isClaudeUsageSemantic bool, usedVars map[string]bool) billingexpr.TokenParams {
	p := float64(usage.PromptTokens)
	c := float64(usage.CompletionTokens)
	cr := float64(usage.PromptTokensDetails.CachedTokens)
	cacheCreationAggregate := usage.PromptTokensDetails.CacheCreationTokensTotal()
	cacheCreation5m := 0
	cacheCreation1h := 0
	if isClaudeUsageSemantic {
		cacheCreation5m = usage.ClaudeCacheCreation5mTokens
		cacheCreation1h = usage.ClaudeCacheCreation1hTokens
	}
	cacheCreationTotal := reliableCacheCreationTokens(cacheCreationAggregate, cacheCreation5m, cacheCreation1h)
	cc := float64(cacheCreationTotal)
	cc1h := float64(0)

	if isClaudeUsageSemantic {
		cc1h = float64(max(cacheCreation1h, 0))
		// CC carries 5m writes plus any unsplit aggregate remainder.
		cc = float64(cacheCreationTotal) - cc1h
	}

	img := float64(usage.PromptTokensDetails.ImageTokens)
	ai := float64(usage.PromptTokensDetails.AudioTokens)
	imgO := float64(usage.CompletionTokenDetails.ImageTokens)
	ao := float64(usage.CompletionTokenDetails.AudioTokens)

	// len = total input context length for tier condition evaluation.
	// Non-Claude: prompt_tokens already includes everything.
	// Claude: input_tokens is text-only, so add cache read + cache creation.
	inputLen := p
	if isClaudeUsageSemantic {
		inputLen = p + cr + float64(cacheCreationTotal)
	}

	if !isClaudeUsageSemantic {
		if usedVars["cr"] {
			p -= cr
		}
		if usedVars["cc"] {
			p -= cc
		}
		if usedVars["cc1h"] {
			p -= cc1h
		}
		if usedVars["img"] {
			p -= img
		}
		if usedVars["ai"] {
			p -= ai
		}
		if usedVars["img_o"] {
			c -= imgO
		}
		if usedVars["ao"] {
			c -= ao
		}
	}

	// OpenAI cache-write usage reports unadjusted prefix counts, so cr + cc can
	// exceed the prompt and drive the remainder negative. Clamp at zero.
	if p < 0 {
		p = 0
	}
	if c < 0 {
		c = 0
	}

	return billingexpr.TokenParams{
		P:    p,
		C:    c,
		Len:  inputLen,
		CR:   cr,
		CC:   cc,
		CC1h: cc1h,
		Img:  img,
		ImgO: imgO,
		AI:   ai,
		AO:   ao,
	}
}

func refreshTieredBillingGroup(relayInfo *relaycommon.RelayInfo) (*billingexpr.BillingSnapshot, error) {
	if relayInfo == nil {
		return nil, nil
	}
	snap := relayInfo.TieredBillingSnapshot
	if snap == nil || snap.BillingMode != "tiered_expr" {
		return nil, nil
	}

	groupRatio := relayInfo.PriceData.GroupRatioInfo.GroupRatio
	if snap.GroupRatio == groupRatio {
		return snap, nil
	}

	estimatedQuotaAfterGroup := snap.EstimatedQuotaBeforeGroup * groupRatio
	estimatedQuota, err := billingexpr.QuotaRoundStrict(estimatedQuotaAfterGroup)
	if err != nil {
		return nil, err
	}
	snap.GroupRatio = groupRatio
	snap.EstimatedQuotaAfterGroup = estimatedQuota
	return snap, nil
}

// BindTieredBillingCandidate installs the finalized request view for the next
// physical attempt and refreshes its request-dependent estimate. Candidate
// pricing has already reserved the maximum, so a selected view that exceeds
// that ceiling is an invariant failure rather than an attempt-time top-up.
func BindTieredBillingCandidate(
	relayInfo *relaycommon.RelayInfo,
	requestInput billingexpr.RequestInput,
	estimatedPromptTokens int,
	estimatedCompletionTokens int,
) error {
	if relayInfo == nil {
		return nil
	}
	if estimatedPromptTokens < 0 || estimatedCompletionTokens < 0 {
		return errors.New("finalized candidate token estimate is invalid")
	}
	input := cloneTieredRequestInput(requestInput)
	relayInfo.BillingRequestInput = &input
	relayInfo.SetEstimatePromptTokens(estimatedPromptTokens)

	snap := relayInfo.TieredBillingSnapshot
	if snap == nil || snap.BillingMode != "tiered_expr" {
		return nil
	}
	rawCost, trace, err := billingexpr.RunExprWithRequest(
		snap.ExprString,
		billingexpr.TokenParams{
			P:   float64(estimatedPromptTokens),
			C:   float64(estimatedCompletionTokens),
			Len: float64(estimatedPromptTokens),
		},
		input,
	)
	if err != nil {
		return err
	}
	quotaPerUnit := snap.QuotaPerUnit
	if quotaPerUnit == 0 {
		quotaPerUnit = common.QuotaPerUnit
	}
	quotaBeforeGroup := rawCost / 1_000_000 * quotaPerUnit
	quotaAfterGroup, err := billingexpr.QuotaRoundStrict(
		quotaBeforeGroup * relayInfo.PriceData.GroupRatioInfo.GroupRatio,
	)
	if err != nil {
		return err
	}
	if quotaAfterGroup > relayInfo.PriceData.QuotaToPreConsume {
		return errors.New("selected finalized candidate exceeds the pre-billed maximum")
	}
	snap.EstimatedPromptTokens = estimatedPromptTokens
	snap.EstimatedCompletionTokens = estimatedCompletionTokens
	snap.EstimatedQuotaBeforeGroup = quotaBeforeGroup
	snap.EstimatedQuotaAfterGroup = quotaAfterGroup
	snap.EstimatedTier = trace.MatchedTier
	snap.GroupRatio = relayInfo.PriceData.GroupRatioInfo.GroupRatio
	return nil
}

func cloneTieredRequestInput(source billingexpr.RequestInput) billingexpr.RequestInput {
	clone := billingexpr.RequestInput{ResolveParam: source.ResolveParam}
	if len(source.Body) > 0 {
		clone.Body = append([]byte(nil), source.Body...)
	}
	if len(source.Headers) > 0 {
		clone.Headers = make(map[string]string, len(source.Headers))
		for key, value := range source.Headers {
			clone.Headers[key] = value
		}
	}
	return clone
}

// PrepareTieredBillingForSelectedGroup refreshes routing-dependent billing
// state before an upstream attempt. An existing session reserves any higher
// estimate before sending. If the initial group was free and skipped
// pre-consume, switching to a paid group creates the session at that point.
func PrepareTieredBillingForSelectedGroup(c *gin.Context, relayInfo *relaycommon.RelayInfo) *types.NewAPIError {
	snap, err := refreshTieredBillingGroup(relayInfo)
	if err != nil {
		return types.NewErrorWithStatusCode(
			err,
			types.ErrorCodeModelPriceError,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}
	if snap == nil {
		return nil
	}
	if snap.GroupRatio == 0 {
		// Paid-to-free keeps FreeModel as-is: FreeModel means "pre-consume was
		// skipped", which is not true once a session exists, and settlement
		// already yields 0 for a zero group ratio.
		return nil
	}

	// The selected group is paid; clear a FreeModel flag frozen when the
	// initial group was free so downstream state stays consistent.
	relayInfo.PriceData.FreeModel = false

	if relayInfo.Billing == nil {
		return PreConsumeBilling(c, snap.EstimatedQuotaAfterGroup, relayInfo)
	}
	if snap.EstimatedQuotaAfterGroup <= relayInfo.PriceData.QuotaToPreConsume {
		// Candidate-aware pricing already reserved the maximum before the first
		// physical attempt. Binding a winner only selects its settlement view.
		relayInfo.FinalPreConsumedQuota = relayInfo.Billing.GetPreConsumedQuota()
		return nil
	}
	if err := relayInfo.Billing.Reserve(snap.EstimatedQuotaAfterGroup); err != nil {
		return types.NewError(err, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
	}
	relayInfo.FinalPreConsumedQuota = relayInfo.Billing.GetPreConsumedQuota()
	return nil
}

// TryTieredSettle checks if the request uses tiered_expr billing and, if so,
// computes the actual quota using the captured BillingSnapshot. Returns:
//   - ok=true, quota, result  when tiered billing applies
//   - ok=false, 0, nil        when it doesn't (caller should fall through to existing logic)
func TryTieredSettle(relayInfo *relaycommon.RelayInfo, params billingexpr.TokenParams) (ok bool, quota int, result *billingexpr.TieredResult) {
	snap := relayInfo.TieredBillingSnapshot
	if snap == nil || snap.BillingMode != "tiered_expr" {
		return false, 0, nil
	}

	requestInput := billingexpr.RequestInput{}
	if relayInfo.BillingRequestInput != nil {
		requestInput = *relayInfo.BillingRequestInput
	}

	tr, err := billingexpr.ComputeTieredQuotaWithRequest(snap, params, requestInput)
	if err != nil {
		quota = relayInfo.FinalPreConsumedQuota
		if quota <= 0 {
			quota = snap.EstimatedQuotaAfterGroup
		}
		return true, quota, nil
	}

	// Surface any int32 saturation from settlement onto RelayInfo so the
	// consume log records it under admin_info, regardless of which caller
	// (text, audio, WSS) consumes the returned quota. First non-nil wins.
	noteQuotaClamp(relayInfo, tr.Clamp)

	return true, tr.ActualQuotaAfterGroup, &tr
}
