package helper

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	hosttypes "github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func modelPriceNotConfiguredError(modelName string, userId int) error {
	if model.IsAdmin(userId) {
		return fmt.Errorf(
			"模型 %s 的价格未配置。请前往「系统设置 → 运营设置」开启自用模式，或在「系统设置 → 分组与模型定价设置」中为该模型配置价格；"+
				"Model %s price not configured. Go to System Settings → Operation Settings to enable self-use mode, or configure the model price in System Settings → Group & Model Pricing.",
			modelName, modelName,
		)
	}
	return fmt.Errorf(
		"模型 %s 的价格尚未由管理员配置，暂时无法使用，请联系站点管理员开启该模型；"+
			"Model %s has not been priced by the administrator yet. Please contact the site administrator to enable this model.",
		modelName, modelName,
	)
}

// https://docs.claude.com/en/docs/build-with-claude/prompt-caching#1-hour-cache-duration
const claudeCacheCreation1hMultiplier = 6 / 3.75

// defaultTieredPreConsumeMaxTokens is the fallback completion-token estimate
// used for tiered expression pre-consume when the client omits max_tokens, so
// the pre-consumed quota still reflects a plausible output cost in paid groups.
const defaultTieredPreConsumeMaxTokens = 8192

// BillingCandidateView is the side-effect-free billing projection of one
// preflighted OpenCode route. Body is the finalized JSON after conversion,
// merge, operator overrides, and the provider safety fence.
type BillingCandidateView struct {
	SelectionGroup            string
	Body                      []byte
	EstimatedPromptTokens     int
	EstimatedCompletionTokens int
}

// HandleGroupRatio checks for "auto_group" in the context and updates the group ratio and relayInfo.UsingGroup if present
func HandleGroupRatio(ctx *gin.Context, relayInfo *relaycommon.RelayInfo) hosttypes.GroupRatioInfo {
	// check auto group
	autoGroup, exists := ctx.Get("auto_group")
	if exists {
		logger.LogDebug(ctx, "final group: %s", autoGroup)
		relayInfo.UsingGroup = autoGroup.(string)
	}
	return ResolveGroupRatioInfo(relayInfo.UserGroup, relayInfo.UsingGroup)
}

// ResolveGroupRatioInfo computes the effective ratio without mutating routing
// state. Candidate planning uses it before any physical attempt is selected.
func ResolveGroupRatioInfo(userGroup, usingGroup string) hosttypes.GroupRatioInfo {
	groupRatioInfo := hosttypes.GroupRatioInfo{
		GroupRatio:        1.0,
		GroupSpecialRatio: -1,
	}

	// check user group special ratio
	userGroupRatio, ok := ratio_setting.GetGroupGroupRatio(userGroup, usingGroup)
	if ok {
		// user group special ratio
		groupRatioInfo.GroupSpecialRatio = userGroupRatio
		groupRatioInfo.GroupRatio = userGroupRatio
		groupRatioInfo.HasSpecialRatio = true
	} else {
		// normal group ratio
		groupRatioInfo.GroupRatio = ratio_setting.GetGroupRatio(usingGroup)
	}

	return groupRatioInfo
}

func ModelPriceHelper(c *gin.Context, info *relaycommon.RelayInfo, promptTokens int, meta *types.TokenCountMeta) (hosttypes.PriceData, error) {
	groupRatioInfo := HandleGroupRatio(c, info)
	return modelPriceHelperWithGroupRatio(c, info, promptTokens, meta, groupRatioInfo)
}

// ModelPriceHelperForCandidates reserves against the highest estimate across
// the immutable finalized candidate set. It leaves the request's selected
// routing group unchanged; each physical attempt installs its winning billing
// view immediately before transport.
func ModelPriceHelperForCandidates(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	promptTokens int,
	meta *types.TokenCountMeta,
	candidates []BillingCandidateView,
) (hosttypes.PriceData, error) {
	if len(candidates) == 0 {
		return ModelPriceHelper(c, info, promptTokens, meta)
	}

	if billing_setting.GetBillingMode(info.OriginModelName) == billing_setting.BillingModeTieredExpr {
		var selected *tieredPriceEstimate
		for index, candidate := range candidates {
			group := strings.TrimSpace(candidate.SelectionGroup)
			if group == "" {
				group = info.UsingGroup
			}
			requestInput := billingexpr.RequestInput{
				Headers: cloneStringMap(info.RequestHeaders),
				Body:    append([]byte(nil), candidate.Body...),
			}
			candidateMeta := billingCandidateTokenMeta(meta, candidate)
			candidatePromptTokens := max(promptTokens, candidate.EstimatedPromptTokens)
			estimate, err := evaluateTieredPrice(
				info,
				candidatePromptTokens,
				candidateMeta,
				ResolveGroupRatioInfo(info.UserGroup, group),
				requestInput,
			)
			if err != nil {
				return hosttypes.PriceData{}, fmt.Errorf("candidate %d tiered billing: %w", index, err)
			}
			if selected == nil || estimate.price.QuotaToPreConsume > selected.price.QuotaToPreConsume ||
				(estimate.price.QuotaToPreConsume == selected.price.QuotaToPreConsume &&
					selected.price.FreeModel && !estimate.price.FreeModel) {
				selected = &estimate
			}
		}
		if selected == nil {
			return hosttypes.PriceData{}, fmt.Errorf("OpenCode finalized candidate billing set is empty")
		}
		applyTieredPriceEstimate(info, *selected)
		logTieredPriceEstimate(c, info, *selected)
		return selected.price, nil
	}

	var selectedPrice *hosttypes.PriceData
	for index, candidate := range candidates {
		group := strings.TrimSpace(candidate.SelectionGroup)
		if group == "" {
			group = info.UsingGroup
		}
		candidateMeta := billingCandidateTokenMeta(meta, candidate)
		candidatePromptTokens := max(promptTokens, candidate.EstimatedPromptTokens)
		candidateInfo := *info
		price, err := modelPriceHelperWithGroupRatio(
			c,
			&candidateInfo,
			candidatePromptTokens,
			candidateMeta,
			ResolveGroupRatioInfo(info.UserGroup, group),
		)
		if err != nil {
			return hosttypes.PriceData{}, fmt.Errorf("candidate %d billing: %w", index, err)
		}
		if selectedPrice == nil || price.QuotaToPreConsume > selectedPrice.QuotaToPreConsume ||
			(price.QuotaToPreConsume == selectedPrice.QuotaToPreConsume && selectedPrice.FreeModel && !price.FreeModel) {
			copy := price
			selectedPrice = &copy
		}
	}
	if selectedPrice == nil {
		return hosttypes.PriceData{}, fmt.Errorf("OpenCode finalized candidate billing set is empty")
	}
	info.PriceData = *selectedPrice
	return *selectedPrice, nil
}

func billingCandidateTokenMeta(meta *types.TokenCountMeta, candidate BillingCandidateView) *types.TokenCountMeta {
	clone := types.TokenCountMeta{}
	if meta != nil {
		clone = *meta
		if meta.BillingRatios != nil {
			clone.BillingRatios = make(map[string]float64, len(meta.BillingRatios))
			for key, value := range meta.BillingRatios {
				clone.BillingRatios[key] = value
			}
		}
	}
	if candidate.EstimatedCompletionTokens > clone.MaxTokens {
		clone.MaxTokens = candidate.EstimatedCompletionTokens
	}
	return &clone
}

func modelPriceHelperWithGroupRatio(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	promptTokens int,
	meta *types.TokenCountMeta,
	groupRatioInfo hosttypes.GroupRatioInfo,
) (hosttypes.PriceData, error) {
	modelPrice, usePrice := ratio_setting.GetModelPrice(info.OriginModelName, false)

	// Check if this model uses tiered_expr billing
	if billing_setting.GetBillingMode(info.OriginModelName) == billing_setting.BillingModeTieredExpr {
		return modelPriceHelperTiered(c, info, promptTokens, meta, groupRatioInfo)
	}

	var preConsumedQuota int
	var modelRatio float64
	var completionRatio float64
	var cacheRatio float64
	var imageRatio float64
	var cacheCreationRatio float64
	var cacheCreationRatio5m float64
	var cacheCreationRatio1h float64
	var audioRatio float64
	var audioCompletionRatio float64
	var freeModel bool
	if !usePrice {
		preConsumedTokens := common.Max(promptTokens, common.PreConsumedQuota)
		if meta.MaxTokens != 0 {
			preConsumedTokens += meta.MaxTokens
		}
		var success bool
		var matchName string
		modelRatio, success, matchName = ratio_setting.GetModelRatio(info.OriginModelName)
		if !success {
			acceptUnsetRatio := false
			if info.UserSetting.AcceptUnsetRatioModel {
				acceptUnsetRatio = true
			}
			if !acceptUnsetRatio {
				return hosttypes.PriceData{}, modelPriceNotConfiguredError(matchName, info.UserId)
			}
		}
		completionRatio = ratio_setting.GetCompletionRatio(info.OriginModelName)
		cacheRatio, _ = ratio_setting.GetCacheRatio(info.OriginModelName)
		cacheCreationRatio, _ = ratio_setting.GetCreateCacheRatio(info.OriginModelName)
		cacheCreationRatio5m = cacheCreationRatio
		// 固定1h和5min缓存写入价格的比例
		cacheCreationRatio1h = cacheCreationRatio * claudeCacheCreation1hMultiplier
		imageRatio, _ = ratio_setting.GetImageRatio(info.OriginModelName)
		audioRatio = ratio_setting.GetAudioRatio(info.OriginModelName)
		audioCompletionRatio = ratio_setting.GetAudioCompletionRatio(info.OriginModelName)
		ratio := modelRatio * groupRatioInfo.GroupRatio
		quota, err := common.QuotaFromFloatStrict(float64(preConsumedTokens) * ratio)
		if err != nil {
			return hosttypes.PriceData{}, err
		}
		preConsumedQuota = quota
	} else {
		if meta.ImagePriceRatio != 0 {
			modelPrice = modelPrice * meta.ImagePriceRatio
		}
	}

	// check if free model pre-consume is disabled
	if !operation_setting.GetQuotaSetting().EnableFreeModelPreConsume {
		// if model price or ratio is 0, do not pre-consume quota
		if groupRatioInfo.GroupRatio == 0 {
			preConsumedQuota = 0
			freeModel = true
		} else if usePrice {
			if modelPrice == 0 {
				preConsumedQuota = 0
				freeModel = true
			}
		} else {
			if modelRatio == 0 {
				preConsumedQuota = 0
				freeModel = true
			}
		}
	}

	priceData := hosttypes.PriceData{
		FreeModel:            freeModel,
		ModelPrice:           modelPrice,
		ModelRatio:           modelRatio,
		CompletionRatio:      completionRatio,
		GroupRatioInfo:       groupRatioInfo,
		UsePrice:             usePrice,
		CacheRatio:           cacheRatio,
		ImageRatio:           imageRatio,
		AudioRatio:           audioRatio,
		AudioCompletionRatio: audioCompletionRatio,
		CacheCreationRatio:   cacheCreationRatio,
		CacheCreation5mRatio: cacheCreationRatio5m,
		CacheCreation1hRatio: cacheCreationRatio1h,
		QuotaToPreConsume:    preConsumedQuota,
	}
	if usePrice {
		for name, ratio := range meta.BillingRatios {
			priceData.AddOtherRatio(name, ratio)
		}
		quotaToPreConsume := priceData.ApplyOtherRatiosToFloat(modelPrice * common.QuotaPerUnit * groupRatioInfo.GroupRatio)
		quota, err := common.QuotaFromFloatStrict(quotaToPreConsume)
		if err != nil {
			return hosttypes.PriceData{}, err
		}
		priceData.QuotaToPreConsume = quota
	}

	if common.DebugEnabled {
		logger.LogDebug(c, "model_price_helper result: %s", priceData.ToSetting())
	}
	info.PriceData = priceData
	return priceData, nil
}

// ModelPriceHelperPerCall 按次/按量计费的 PriceHelper (MJ、Task)
func ModelPriceHelperPerCall(c *gin.Context, info *relaycommon.RelayInfo) (hosttypes.PriceData, error) {
	groupRatioInfo := HandleGroupRatio(c, info)

	modelPrice, success := ratio_setting.GetModelPrice(info.OriginModelName, true)
	usePrice := success
	var modelRatio float64

	if !success {
		defaultPrice, ok := ratio_setting.GetDefaultModelPriceMap()[info.OriginModelName]
		if ok {
			modelPrice = defaultPrice
			usePrice = true
		} else {
			var ratioSuccess bool
			var matchName string
			modelRatio, ratioSuccess, matchName = ratio_setting.GetModelRatio(info.OriginModelName)
			acceptUnsetRatio := false
			if info.UserSetting.AcceptUnsetRatioModel {
				acceptUnsetRatio = true
			}
			if !ratioSuccess && !acceptUnsetRatio {
				return hosttypes.PriceData{}, modelPriceNotConfiguredError(matchName, info.UserId)
			}
		}
	}

	var quota int
	freeModel := false

	if usePrice {
		var err error
		quota, err = common.QuotaFromFloatStrict(modelPrice * common.QuotaPerUnit * groupRatioInfo.GroupRatio)
		if err != nil {
			return hosttypes.PriceData{}, err
		}
		if !operation_setting.GetQuotaSetting().EnableFreeModelPreConsume {
			if groupRatioInfo.GroupRatio == 0 || modelPrice == 0 {
				quota = 0
				freeModel = true
			}
		}
	} else {
		// 按量计费：以模型倍率的一半作为预扣额度
		var err error
		quota, err = common.QuotaFromFloatStrict(modelRatio / 2 * common.QuotaPerUnit * groupRatioInfo.GroupRatio)
		if err != nil {
			return hosttypes.PriceData{}, err
		}
		modelPrice = -1
		if !operation_setting.GetQuotaSetting().EnableFreeModelPreConsume {
			if groupRatioInfo.GroupRatio == 0 || modelRatio == 0 {
				quota = 0
				freeModel = true
			}
		}
	}

	priceData := hosttypes.PriceData{
		FreeModel:      freeModel,
		ModelPrice:     modelPrice,
		ModelRatio:     modelRatio,
		UsePrice:       usePrice,
		Quota:          quota,
		GroupRatioInfo: groupRatioInfo,
	}
	return priceData, nil
}

func HasModelBillingConfig(modelName string) bool {
	if _, ok := ratio_setting.GetModelPrice(modelName, false); ok {
		return true
	}
	if _, ok, _ := ratio_setting.GetModelRatio(modelName); ok {
		return true
	}
	if billing_setting.GetBillingMode(modelName) != billing_setting.BillingModeTieredExpr {
		return false
	}
	expr, ok := billing_setting.GetBillingExpr(modelName)
	return ok && strings.TrimSpace(expr) != ""
}

func modelPriceHelperTiered(c *gin.Context, info *relaycommon.RelayInfo, promptTokens int, meta *types.TokenCountMeta, groupRatioInfo hosttypes.GroupRatioInfo) (hosttypes.PriceData, error) {
	requestInput, err := ResolveIncomingBillingExprRequestInput(c, info)
	if err != nil {
		return hosttypes.PriceData{}, err
	}
	estimate, err := evaluateTieredPrice(info, promptTokens, meta, groupRatioInfo, requestInput)
	if err != nil {
		return hosttypes.PriceData{}, err
	}
	applyTieredPriceEstimate(info, estimate)
	logTieredPriceEstimate(c, info, estimate)
	return estimate.price, nil
}

type tieredPriceEstimate struct {
	price        hosttypes.PriceData
	snapshot     billingexpr.BillingSnapshot
	requestInput billingexpr.RequestInput
}

func evaluateTieredPrice(
	info *relaycommon.RelayInfo,
	promptTokens int,
	meta *types.TokenCountMeta,
	groupRatioInfo hosttypes.GroupRatioInfo,
	requestInput billingexpr.RequestInput,
) (tieredPriceEstimate, error) {
	exprStr, ok := billing_setting.GetBillingExpr(info.OriginModelName)
	if !ok {
		return tieredPriceEstimate{}, fmt.Errorf("model %s is configured as tiered_expr but has no billing expression", info.OriginModelName)
	}

	estimatedCompletionTokens := meta.MaxTokens
	if estimatedCompletionTokens == 0 && groupRatioInfo.GroupRatio != 0 {
		estimatedCompletionTokens = defaultTieredPreConsumeMaxTokens
	}

	rawCost, trace, err := billingexpr.RunExprWithRequest(exprStr, billingexpr.TokenParams{
		P:   float64(promptTokens),
		C:   float64(estimatedCompletionTokens),
		Len: float64(promptTokens),
	}, requestInput)
	if err != nil {
		return tieredPriceEstimate{}, fmt.Errorf("model %s tiered expr run failed: %w", info.OriginModelName, err)
	}

	// Expression coefficients are $/1M tokens prices; convert to quota the same way per-call billing does.
	quotaBeforeGroup := rawCost / 1_000_000 * common.QuotaPerUnit
	preConsumedQuota, err := billingexpr.QuotaRoundStrict(quotaBeforeGroup * groupRatioInfo.GroupRatio)
	if err != nil {
		return tieredPriceEstimate{}, err
	}

	freeModel := false
	if !operation_setting.GetQuotaSetting().EnableFreeModelPreConsume {
		if groupRatioInfo.GroupRatio == 0 {
			preConsumedQuota = 0
			freeModel = true
		}
	}

	exprHash := billingexpr.ExprHashString(exprStr)
	snapshot := billingexpr.BillingSnapshot{
		BillingMode:               billing_setting.BillingModeTieredExpr,
		ModelName:                 info.OriginModelName,
		ExprString:                exprStr,
		ExprHash:                  exprHash,
		GroupRatio:                groupRatioInfo.GroupRatio,
		EstimatedPromptTokens:     promptTokens,
		EstimatedCompletionTokens: estimatedCompletionTokens,
		EstimatedQuotaBeforeGroup: quotaBeforeGroup,
		EstimatedQuotaAfterGroup:  preConsumedQuota,
		EstimatedTier:             trace.MatchedTier,
		QuotaPerUnit:              common.QuotaPerUnit,
		ExprVersion:               billingexpr.ExprVersion(exprStr),
	}
	priceData := hosttypes.PriceData{
		FreeModel:         freeModel,
		GroupRatioInfo:    groupRatioInfo,
		QuotaToPreConsume: preConsumedQuota,
	}

	return tieredPriceEstimate{
		price:        priceData,
		snapshot:     snapshot,
		requestInput: cloneRequestInput(requestInput),
	}, nil
}

func applyTieredPriceEstimate(info *relaycommon.RelayInfo, estimate tieredPriceEstimate) {
	snapshot := estimate.snapshot
	requestInput := cloneRequestInput(estimate.requestInput)
	info.TieredBillingSnapshot = &snapshot
	info.BillingRequestInput = &requestInput
	info.PriceData = estimate.price
}

func logTieredPriceEstimate(c *gin.Context, info *relaycommon.RelayInfo, estimate tieredPriceEstimate) {
	logger.LogDebug(
		c,
		"model_price_helper_tiered result: model=%s preConsume=%d quotaBeforeGroup=%.2f groupRatio=%.2f tier=%s",
		info.OriginModelName,
		estimate.price.QuotaToPreConsume,
		estimate.snapshot.EstimatedQuotaBeforeGroup,
		estimate.snapshot.GroupRatio,
		estimate.snapshot.EstimatedTier,
	)
}
