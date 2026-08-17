package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/relay"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/opencodego"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const (
	groupUpstreamOverloadedMessage = service.OpenCodeGoPublicOverloadMessage
)

func publicRelayError(c *gin.Context, newAPIError *types.NewAPIError) *types.NewAPIError {
	channelType := 0
	if c != nil {
		channelType = common.GetContextKeyInt(c, constant.ContextKeyChannelType)
	}
	return service.PublicOpenCodeGoRelayError(channelType, newAPIError)
}

func relayHandler(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	var err *types.NewAPIError
	switch info.RelayMode {
	case relayconstant.RelayModeImagesGenerations, relayconstant.RelayModeImagesEdits:
		err = relay.ImageHelper(c, info)
	case relayconstant.RelayModeAudioSpeech:
		fallthrough
	case relayconstant.RelayModeAudioTranslation:
		fallthrough
	case relayconstant.RelayModeAudioTranscription:
		err = relay.AudioHelper(c, info)
	case relayconstant.RelayModeRerank:
		err = relay.RerankHelper(c, info)
	case relayconstant.RelayModeEmbeddings:
		err = relay.EmbeddingHelper(c, info)
	case relayconstant.RelayModeResponses, relayconstant.RelayModeResponsesCompact:
		err = relay.ResponsesHelper(c, info)
	case relayconstant.RelayModeAlphaSearch:
		err = relay.AlphaSearchHelper(c, info)
	default:
		err = relay.TextHelper(c, info)
	}
	return err
}

func geminiRelayHandler(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	var err *types.NewAPIError
	if strings.Contains(c.Request.URL.Path, "embed") {
		err = relay.GeminiEmbeddingHandler(c, info)
	} else {
		err = relay.GeminiHelper(c, info)
	}
	return err
}

func renderRelayError(c *gin.Context, relayFormat types.RelayFormat, ws *websocket.Conn, newAPIError *types.NewAPIError, requestID string) {
	if newAPIError == nil {
		return
	}
	logMessage := newAPIError.Error()
	if c != nil && constant.IsOpenCodeChannelType(common.GetContextKeyInt(c, constant.ContextKeyChannelType)) {
		logMessage = service.OpenCodeGoAdminErrorWithStatusCode(newAPIError)
	}
	logger.LogError(c, fmt.Sprintf("relay error: %s", common.LocalLogPreview(logMessage)))
	if relayFormat != types.RelayFormatOpenAIRealtime && c.Writer.Written() {
		// The status is already committed (usually 200 for an SSE stream), so the
		// error cannot be rendered. Do not let the distributor treat this relay as
		// a success solely from that committed status.
		common.SetContextKey(c, constant.ContextKeyRelayFailed, true)
		return
	}
	channelType := common.GetContextKeyInt(c, constant.ContextKeyChannelType)
	publicError := publicRelayError(c, newAPIError)
	projectedOpenCodeError := publicError != newAPIError && constant.IsOpenCodeChannelType(channelType)
	if projectedOpenCodeError {
		resetOpenCodePublicResponseHeaders(c, requestID)
	} else if publicError != newAPIError {
		clearUnwrittenEventStreamHeaders(c)
	}
	newAPIError = publicError
	if constant.IsOpenCodeChannelType(channelType) {
		c.Header(common.RequestIdKey, requestID)
	} else {
		newAPIError.SetMessage(common.MessageWithRequestId(newAPIError.Error(), requestID))
	}
	if service.IsOpenCodeGoFixedInvalidRequestProjection(newAPIError) {
		renderOpenCodeFixedInvalidRequest(c, relayFormat, newAPIError)
		return
	}
	switch relayFormat {
	case types.RelayFormatOpenAIRealtime:
		helper.WssError(c, ws, newAPIError.ToOpenAIError())
	case types.RelayFormatClaude:
		c.JSON(newAPIError.StatusCode, gin.H{
			"type":  "error",
			"error": newAPIError.ToClaudeError(),
		})
	default:
		c.JSON(newAPIError.StatusCode, gin.H{
			"error": newAPIError.ToOpenAIError(),
		})
	}
}

func renderOpenCodeFixedInvalidRequest(c *gin.Context, relayFormat types.RelayFormat, relayErr *types.NewAPIError) {
	if relayFormat == types.RelayFormatClaude {
		c.JSON(relayErr.StatusCode, gin.H{
			"type": "error",
			"error": gin.H{
				"type":    constant.OpenCodeGoPublicInvalidRequestCode,
				"message": constant.OpenCodeGoPublicInvalidRequestMessage,
			},
		})
		return
	}
	c.JSON(relayErr.StatusCode, gin.H{
		"error": gin.H{
			"message": constant.OpenCodeGoPublicInvalidRequestMessage,
			"type":    constant.OpenCodeGoPublicInvalidRequestCode,
			"param":   "",
			"code":    constant.OpenCodeGoPublicInvalidRequestCode,
		},
	})
}

func resetOpenCodePublicResponseHeaders(c *gin.Context, requestID string) {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return
	}
	header := c.Writer.Header()
	localCORS := make(http.Header)
	for _, name := range []string{
		"Access-Control-Allow-Credentials",
		"Access-Control-Allow-Origin",
		"Access-Control-Expose-Headers",
		"Vary",
	} {
		if values := header.Values(name); len(values) > 0 {
			localCORS[name] = append([]string(nil), values...)
		}
	}
	for name := range header {
		header.Del(name)
	}
	for name, values := range localCORS {
		for _, value := range values {
			header.Add(name, value)
		}
	}
	if requestID != "" {
		header.Set(common.RequestIdKey, requestID)
	}
}

func clearUnwrittenEventStreamHeaders(c *gin.Context) {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return
	}
	if !strings.HasPrefix(strings.ToLower(c.Writer.Header().Get("Content-Type")), "text/event-stream") {
		return
	}
	for _, name := range []string{
		"Content-Type",
		"Content-Length",
		"Cache-Control",
		"Connection",
		"Transfer-Encoding",
		"X-Accel-Buffering",
		"X-Codex-Turn-State",
		"X-Reasoning-Included",
	} {
		c.Writer.Header().Del(name)
	}
}

func refundRelayBilling(c *gin.Context, relayInfo *relaycommon.RelayInfo) {
	if relayInfo != nil && relayInfo.Billing != nil && relayInfo.Billing.NeedsRefund() {
		relayInfo.Billing.Refund(c)
	}
}

func Relay(c *gin.Context, relayFormat types.RelayFormat) {

	requestId := c.GetString(common.RequestIdKey)
	//group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	//originalModel := common.GetContextKeyString(c, constant.ContextKeyOriginalModel)

	var (
		newAPIError *types.NewAPIError
		ws          *websocket.Conn
	)

	if relayFormat == types.RelayFormatOpenAIRealtime {
		var err error
		ws, err = upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			helper.WssError(c, ws, types.NewError(err, types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry()).ToOpenAIError())
			return
		}
		defer ws.Close()
	}

	defer func() {
		renderRelayError(c, relayFormat, ws, newAPIError, requestId)
	}()

	request, err := helper.GetAndValidateRequest(c, relayFormat)
	if err != nil {
		// A client disconnect/cancel can surface while the reusable request
		// body is being read. It is not malformed input and must not be
		// reported as a retryable client 400.
		if errors.Is(err, context.Canceled) {
			newAPIError = types.NewOpenAIError(
				context.Canceled,
				types.ErrorCodeBadResponse,
				499,
				types.ErrOptionWithSkipRetry(),
				types.ErrOptionWithProvenance(types.ErrorProvenance{
					Origin:  types.ErrorOriginLocalCancel,
					Subtype: "request_body_cancelled",
				}),
			)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			newAPIError = types.NewOpenAIError(
				context.DeadlineExceeded,
				types.ErrorCodeBadResponse,
				http.StatusGatewayTimeout,
				types.ErrOptionWithSkipRetry(),
				types.ErrOptionWithProvenance(types.ErrorProvenance{
					Origin:  types.ErrorOriginLocalDeadline,
					Subtype: "request_body_deadline",
				}),
			)
			return
		}
		// Map "request body too large" to 413 so clients can handle it correctly
		if common.IsRequestBodyTooLargeError(err) || errors.Is(err, common.ErrRequestBodyTooLarge) {
			newAPIError = types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed, http.StatusRequestEntityTooLarge, types.ErrOptionWithSkipRetry())
		} else {
			newAPIError = newRelayInvalidRequestError(err)
		}
		return
	}
	if securityErr := preflightOpenCodeRequestSensitiveValues(c, relayFormat); securityErr != nil {
		newAPIError = securityErr
		return
	}

	relayInfo, err := relaycommon.GenRelayInfo(c, relayFormat, request, ws)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeGenRelayInfoFailed)
		return
	}
	if preflightErr := relaychannel.PreflightOpenCodeRequestTransport(relayInfo, c); preflightErr != nil {
		newAPIError = preflightErr
		return
	}
	finalizedCandidates, finalizedErr := prepareOpenCodeFinalizedCandidatePlans(c, relayInfo)
	if finalizedErr != nil {
		newAPIError = finalizedErr
		return
	}
	if securityErr := preflightOpenCodeFinalizedCandidateSensitiveValues(c, finalizedCandidates); securityErr != nil {
		newAPIError = securityErr
		return
	}

	needSensitiveCheck := setting.ShouldCheckPromptSensitive()
	needCountToken := constant.CountToken
	// Avoid building huge CombineText (strings.Join) when token counting and sensitive check are both disabled.
	var meta *types.TokenCountMeta
	if needSensitiveCheck || needCountToken {
		meta = request.GetTokenCountMeta()
	} else {
		meta = fastTokenCountMetaForPricing(request)
	}

	if needSensitiveCheck && shouldRunTypedSensitiveScan(c) && meta != nil {
		contains, words := service.CheckSensitiveText(meta.CombineText)
		if contains {
			logger.LogWarn(c, fmt.Sprintf(
				"relay request security rejected: rule_id=request.security.typed-string match_count=%d",
				len(words),
			))
			newAPIError = types.NewOpenAIError(
				errors.New("request contains sensitive content"),
				types.ErrorCodeSensitiveWordsDetected,
				http.StatusBadRequest,
				types.ErrOptionWithSkipRetry(),
				types.ErrOptionWithNoRecordErrorLog(),
				types.ErrOptionWithProvenance(types.ErrorProvenance{
					Origin:  types.ErrorOriginLocalValidation,
					Subtype: "request.security.typed-string",
				}),
			)
			return
		}
	}

	tokens, err := service.EstimateRequestToken(c, meta, relayInfo)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeCountTokenFailed)
		return
	}

	relayInfo.SetEstimatePromptTokens(tokens)

	var billingCandidates []helper.BillingCandidateView
	if finalizedCandidates != nil {
		billingCandidates = finalizedCandidates.billingViews(tokens)
	}
	priceData, err := helper.ModelPriceHelperForCandidates(c, relayInfo, tokens, meta, billingCandidates)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeModelPriceError, types.ErrOptionWithStatusCode(http.StatusBadRequest))
		return
	}

	defer func() {
		if newAPIError != nil {
			newAPIError = service.NormalizeViolationFeeError(newAPIError)
			service.ChargeViolationFeeIfNeeded(c, relayInfo, newAPIError)
		}
		// Billing state, rather than the presence of a returned error, owns
		// refund eligibility. This also runs while a panic unwinds.
		refundRelayBilling(c, relayInfo)
	}()

	// common.SetContextKey(c, constant.ContextKeyTokenCountMeta, meta)

	if priceData.FreeModel {
		logger.LogInfo(c, fmt.Sprintf("模型 %s 免费，跳过预扣费", relayInfo.OriginModelName))
	} else {
		newAPIError = service.PreConsumeBilling(c, priceData.QuotaToPreConsume, relayInfo)
		if newAPIError != nil {
			return
		}
	}

	retryParam := &service.RetryParam{
		Ctx:         c,
		TokenGroup:  relayInfo.TokenGroup,
		ModelName:   relayInfo.OriginModelName,
		RequestPath: c.Request.URL.Path,
		Retry:       common.GetPointer(0),
	}
	relayInfo.RetryIndex = 0
	relayInfo.LastError = nil
	attemptBaseline := relayInfo.SnapshotRelayAttempt()
	attemptContextBaseline := snapshotRelayAttemptContext(c)
	retrySnapshot, hasRetrySnapshot, snapshotErr := getOpenCodeAPIKeyRetrySnapshot(c)
	if snapshotErr != nil {
		newAPIError = newOpenCodeRetrySnapshotAPIError(c, snapshotErr)
		return
	}
	physicalAttempt := 0

	for {
		if hasRetrySnapshot {
			if physicalAttempt >= len(retrySnapshot.selections) {
				break
			}
		} else if retryParam.GetRetry() > common.RetryTimes {
			break
		}

		var (
			channel    *model.Channel
			channelErr *types.NewAPIError
		)
		if hasRetrySnapshot {
			if physicalAttempt > 0 {
				resetOpenCodeAPIKeyRelayAttempt(c, relayInfo, attemptBaseline, attemptContextBaseline)
			}
			relayInfo.RetryIndex = physicalAttempt
			selected, selectErr := retrySnapshot.selectAttempt(c, physicalAttempt)
			if selectErr != nil {
				if _, typed := opencodego.AsRequestPreflightError(selectErr); typed {
					channelErr = newOpenCodeRequestPreflightAPIError(c, selectErr)
				} else {
					channelErr = newOpenCodeRetrySnapshotAPIError(c, selectErr)
				}
			} else {
				channel = selected
				relayInfo.PriceData.GroupRatioInfo = helper.HandleGroupRatio(c, relayInfo)
			}
		} else {
			relayInfo.RetryIndex = retryParam.GetRetry()
			channel, channelErr = getChannel(c, relayInfo, retryParam)
		}
		if channelErr != nil {
			logger.LogError(c, channelErr.Error())
			newAPIError = channelErr
			break
		}
		addUsedChannel(c, channel.Id)
		if billingErr := bindOpenCodeFinalizedCandidateBilling(c, relayInfo); billingErr != nil {
			newAPIError = billingErr
			break
		}
		if billingErr := service.PrepareTieredBillingForSelectedGroup(c, relayInfo); billingErr != nil {
			newAPIError = billingErr
			break
		}

		newAPIError = relaySelectedChannelWithOpenCodeGoRetry(c, relayFormat, relayInfo, channel.Type, func() *types.NewAPIError {
			bodyStorage, bodyErr := common.GetBodyStorage(c)
			if bodyErr != nil {
				// Ensure consistent 413 for oversized bodies even when error occurs later (e.g., retry path)
				if common.IsRequestBodyTooLargeError(bodyErr) || errors.Is(bodyErr, common.ErrRequestBodyTooLarge) {
					return types.NewErrorWithStatusCode(bodyErr, types.ErrorCodeReadRequestBodyFailed, http.StatusRequestEntityTooLarge, types.ErrOptionWithSkipRetry())
				}
				return newRelayInvalidRequestError(bodyErr)
			}
			c.Request.Body = io.NopCloser(bodyStorage)
			return relayOpenCodePhysicalAttempt(c, func() *types.NewAPIError {
				return relaySelectedChannel(c, relayFormat, relayInfo)
			})
		})

		if newAPIError == nil {
			relayInfo.LastError = nil
			return
		}

		newAPIError = service.NormalizeViolationFeeError(newAPIError)
		relayInfo.LastError = newAPIError

		processChannelError(c, *types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey, common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()), newAPIError)

		remainingRetries := common.RetryTimes - retryParam.GetRetry()
		if hasRetrySnapshot {
			remainingRetries = len(retrySnapshot.selections) - physicalAttempt - 1
		}
		if !shouldRetry(c, newAPIError, remainingRetries) {
			break
		}
		if hasRetrySnapshot {
			physicalAttempt++
		} else {
			retryParam.IncreaseRetry()
		}
	}

	useChannel := c.GetStringSlice("use_channel")
	if len(useChannel) > 1 {
		retryLogStr := fmt.Sprintf("重试：%s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(useChannel)), "->"), "[]"))
		logger.LogInfo(c, retryLogStr)
	}
	if newAPIError != nil {
		perfmetrics.ScheduleRelaySample(relayInfo, false, 0)
	}
}

func newRelayInvalidRequestError(err error) *types.NewAPIError {
	subtype := "request.body.invalid"
	if validationErr, ok := helper.AsClientRequestValidationError(err); ok {
		if ruleID := strings.TrimSpace(validationErr.RuleID); ruleID != "" {
			subtype = ruleID
		}
	}
	return types.NewErrorWithStatusCode(
		err,
		types.ErrorCodeInvalidRequest,
		http.StatusBadRequest,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithProvenance(types.ErrorProvenance{
			Origin:  types.ErrorOriginLocalValidation,
			Subtype: subtype,
		}),
	)
}

type relayAttemptContextSnapshot struct {
	requestIsStream          bool
	claudeWebSearchRequests  int
	geminiGoogleSearchCall   bool
	chatWebSearchContextSize interface{}
	systemPromptOverride     bool
	openCodeGoAffinitySource interface{}
	openCodeGoAffinityKey    interface{}
	openCodeGoWorkspaceUID   interface{}
}

func snapshotRelayAttemptContext(c *gin.Context) relayAttemptContextSnapshot {
	if c == nil {
		return relayAttemptContextSnapshot{}
	}
	chatWebSearchContextSize, _ := c.Get("chat_completion_web_search_context_size")
	affinitySource, _ := c.Get(string(constant.ContextKeyOpenCodeGoAffinitySource))
	affinityKey, _ := c.Get(string(constant.ContextKeyOpenCodeGoAffinityKey))
	workspaceUID, _ := c.Get(string(constant.ContextKeyOpenCodeGoWorkspaceUID))
	return relayAttemptContextSnapshot{
		requestIsStream:          common.GetContextKeyBool(c, constant.ContextKeyIsStream),
		claudeWebSearchRequests:  c.GetInt("claude_web_search_requests"),
		geminiGoogleSearchCall:   c.GetBool("gemini_google_search_call"),
		chatWebSearchContextSize: chatWebSearchContextSize,
		systemPromptOverride:     common.GetContextKeyBool(c, constant.ContextKeySystemPromptOverride),
		openCodeGoAffinitySource: affinitySource,
		openCodeGoAffinityKey:    affinityKey,
		openCodeGoWorkspaceUID:   workspaceUID,
	}
}

func resetOpenCodeAPIKeyRelayAttempt(
	c *gin.Context,
	relayInfo *relaycommon.RelayInfo,
	baseline relaycommon.RelayAttemptSnapshot,
	contextBaseline relayAttemptContextSnapshot,
) {
	if relayInfo != nil {
		relayInfo.RestoreRelayAttempt(baseline)
	}
	if c == nil {
		return
	}
	resetOpenCodePublicResponseHeaders(c, c.GetString(common.RequestIdKey))
	c.Set(common.UpstreamRequestIdKey, nil)
	c.Set("claude_web_search_requests", contextBaseline.claudeWebSearchRequests)
	c.Set("gemini_google_search_call", contextBaseline.geminiGoogleSearchCall)
	c.Set("chat_completion_web_search_context_size", contextBaseline.chatWebSearchContextSize)
	common.SetContextKey(c, constant.ContextKeyIsStream, contextBaseline.requestIsStream)
	common.SetContextKey(c, constant.ContextKeySystemPromptOverride, contextBaseline.systemPromptOverride)
	common.SetContextKey(c, constant.ContextKeyOpenCodeGoAffinitySource, contextBaseline.openCodeGoAffinitySource)
	common.SetContextKey(c, constant.ContextKeyOpenCodeGoAffinityKey, contextBaseline.openCodeGoAffinityKey)
	common.SetContextKey(c, constant.ContextKeyOpenCodeGoWorkspaceUID, contextBaseline.openCodeGoWorkspaceUID)
	common.SetContextKey(c, constant.ContextKeyRelayFailed, false)
	service.ResetResponseBodyWriteError(c)
	helper.ResetEventStreamHeaders(c)
}

func relaySelectedChannel(c *gin.Context, relayFormat types.RelayFormat, relayInfo *relaycommon.RelayInfo) *types.NewAPIError {
	switch relayFormat {
	case types.RelayFormatOpenAIRealtime:
		return relay.WssHelper(c, relayInfo)
	case types.RelayFormatClaude:
		return relay.ClaudeHelper(c, relayInfo)
	case types.RelayFormatGemini:
		return geminiRelayHandler(c, relayInfo)
	default:
		return relayHandler(c, relayInfo)
	}
}

func relaySelectedChannelWithOpenCodeGoRetry(
	c *gin.Context,
	relayFormat types.RelayFormat,
	relayInfo *relaycommon.RelayInfo,
	channelType int,
	attempt func() *types.NewAPIError,
) *types.NewAPIError {
	if attempt == nil {
		return nil
	}
	if channelType != constant.ChannelTypeOpenCodeGo {
		return attempt()
	}

	service.BeginOpenCodeGoImmediateRetry(c)
	defer service.EndOpenCodeGoImmediateRetry(c)
	attemptSnapshot := relayInfo.SnapshotRelayAttempt()
	claudeWebSearchRequests := c.GetInt("claude_web_search_requests")
	geminiGoogleSearchCall := c.GetBool("gemini_google_search_call")

	firstErr := service.NormalizeViolationFeeError(attempt())
	if firstErr == nil {
		return nil
	}
	if !shouldRetryOpenCodeGoImmediately(c, channelType, firstErr) {
		if openCodeGoImmediateRetryEndedLocally(c) {
			service.DiscardOpenCodeGoImmediateRetryFailover(c)
		} else {
			service.FlushOpenCodeGoImmediateRetryFailover(c)
		}
		return firstErr
	}

	logger.LogWarn(c, fmt.Sprintf(
		"OpenCode Go transient upstream failure; retrying once on the selected workspace: status=%d error=%s",
		firstErr.StatusCode,
		service.SanitizeOpenCodeGoAdminError(firstErr),
	))
	service.PrepareOpenCodeGoImmediateRetry(c)
	if err := relay.PrepareOpenCodeGoOutboundPlanReplay(c, relayInfo); err != nil {
		service.DiscardOpenCodeGoImmediateRetryFailover(c)
		return types.NewError(
			err,
			types.ErrorCodeConvertRequestFailed,
			types.ErrOptionWithSkipRetry(),
			types.ErrOptionWithProvenance(types.ErrorProvenance{
				Origin:  types.ErrorOriginGatewayInvariant,
				Subtype: "request.finalized-body-replay",
			}),
		)
	}
	resetOpenCodeGoRelayAttempt(c, relayInfo, attemptSnapshot, claudeWebSearchRequests, geminiGoogleSearchCall)
	return service.NormalizeViolationFeeError(attempt())
}

func shouldRetryOpenCodeGoImmediately(c *gin.Context, channelType int, relayErr *types.NewAPIError) bool {
	if !service.ShouldRetryOpenCodeGoRelayError(channelType, relayErr) || c == nil || c.Writer == nil || c.Writer.Written() {
		return false
	}
	if c.Request == nil || c.Request.Context().Err() != nil {
		return false
	}
	return service.ResponseBodyWriteError(c) == nil
}

func openCodeGoImmediateRetryEndedLocally(c *gin.Context) bool {
	if c == nil {
		return false
	}
	if c.Request != nil && c.Request.Context().Err() != nil {
		return true
	}
	return service.ResponseBodyWriteError(c) != nil
}

func resetOpenCodeGoRelayAttempt(
	c *gin.Context,
	relayInfo *relaycommon.RelayInfo,
	attemptSnapshot relaycommon.RelayAttemptSnapshot,
	claudeWebSearchRequests int,
	geminiGoogleSearchCall bool,
) {
	if c != nil && c.Writer != nil && !c.Writer.Written() {
		for _, name := range []string{
			"Content-Type",
			"Content-Length",
			"Cache-Control",
			"Connection",
			"Transfer-Encoding",
			"X-Accel-Buffering",
			"X-Codex-Turn-State",
			"X-Reasoning-Included",
			"Retry-After",
		} {
			c.Writer.Header().Del(name)
		}
	}
	if c != nil {
		c.Set(common.UpstreamRequestIdKey, nil)
		c.Set("claude_web_search_requests", claudeWebSearchRequests)
		c.Set("gemini_google_search_call", geminiGoogleSearchCall)
		service.ResetResponseBodyWriteError(c)
		helper.ResetEventStreamHeaders(c)
	}
	if relayInfo != nil {
		relayInfo.RestoreRelayAttempt(attemptSnapshot)
	}
}

var upgrader = websocket.Upgrader{
	Subprotocols: []string{"realtime"}, // WS 握手支持的协议，如果有使用 Sec-WebSocket-Protocol，则必须在此声明对应的 Protocol TODO add other protocol
	CheckOrigin: func(r *http.Request) bool {
		return true // 允许跨域
	},
}

func addUsedChannel(c *gin.Context, channelId int) {
	useChannel := c.GetStringSlice("use_channel")
	useChannel = append(useChannel, fmt.Sprintf("%d", channelId))
	c.Set("use_channel", useChannel)
}

func fastTokenCountMetaForPricing(request dto.Request) *types.TokenCountMeta {
	if request == nil {
		return &types.TokenCountMeta{}
	}
	meta := &types.TokenCountMeta{
		TokenType: types.TokenTypeTokenizer,
	}
	switch r := request.(type) {
	case *dto.GeneralOpenAIRequest:
		maxCompletionTokens := lo.FromPtrOr(r.MaxCompletionTokens, uint(0))
		maxTokens := lo.FromPtrOr(r.MaxTokens, uint(0))
		if maxCompletionTokens > maxTokens {
			meta.MaxTokens = int(maxCompletionTokens)
		} else {
			meta.MaxTokens = int(maxTokens)
		}
	case *dto.OpenAIResponsesRequest:
		meta.MaxTokens = int(lo.FromPtrOr(r.MaxOutputTokens, uint(0)))
	case *dto.ClaudeRequest:
		meta.MaxTokens = int(lo.FromPtr(r.MaxTokens))
	case *dto.ImageRequest:
		// Pricing for image requests depends on ImagePriceRatio; safe to compute even when CountToken is disabled.
		return r.GetTokenCountMeta()
	default:
		// Best-effort: leave CombineText empty to avoid large allocations.
	}
	return meta
}

func getChannel(c *gin.Context, info *relaycommon.RelayInfo, retryParam *service.RetryParam) (*model.Channel, *types.NewAPIError) {
	if info.ChannelMeta == nil {
		autoBan := c.GetBool("auto_ban")
		autoBanInt := 1
		if !autoBan {
			autoBanInt = 0
		}
		return &model.Channel{
			Id:      c.GetInt("channel_id"),
			Type:    c.GetInt("channel_type"),
			Name:    c.GetString("channel_name"),
			AutoBan: &autoBanInt,
		}, nil
	}
	channel, selectGroup, err := service.CacheGetRandomSatisfiedChannel(retryParam)
	if err != nil {
		return nil, types.NewError(fmt.Errorf("获取分组 %s 下模型 %s 的可用渠道失败（retry）: %s", selectGroup, info.OriginModelName, err.Error()), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}
	if channel == nil {
		return nil, types.NewError(fmt.Errorf("分组 %s 下模型 %s 的可用渠道不存在（retry）", selectGroup, info.OriginModelName), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}

	info.PriceData.GroupRatioInfo = helper.HandleGroupRatio(c, info)

	newAPIError := middleware.SetupContextForSelectedChannel(c, channel, info.OriginModelName)
	if newAPIError != nil {
		return nil, newAPIError
	}
	return channel, nil
}

var getTaskRelayChannel = getChannel

func shouldRetry(c *gin.Context, openaiErr *types.NewAPIError, retryTimes int) bool {
	if openaiErr == nil {
		return false
	}
	if openaiErr.Provenance().IsLocal() || openaiErr.Provenance().IsGateway() {
		return false
	}
	channelType := common.GetContextKeyInt(c, constant.ContextKeyChannelType)
	if channelType == constant.ChannelTypeOpenCodeGo {
		return false
	}
	if channelType == constant.ChannelTypeOpenCodeAPIKey {
		if c.Writer != nil && c.Writer.Written() {
			return false
		}
		if c.Request != nil && c.Request.Context().Err() != nil {
			return false
		}
		if service.ResponseBodyWriteError(c) != nil {
			return false
		}
		if service.IsOpenCodeGoRawInvalidRequestError(openaiErr) {
			return false
		}
	}
	if service.ShouldSkipRetryAfterChannelAffinityFailure(c) {
		return false
	}
	if types.IsChannelError(openaiErr) {
		return true
	}
	if types.IsSkipRetryError(openaiErr) {
		return false
	}
	if retryTimes <= 0 {
		return false
	}
	if _, ok := c.Get("specific_channel_id"); ok {
		return false
	}
	code := service.OpenCodeGoRelayPolicyStatusCode(openaiErr)
	if code >= 200 && code < 300 {
		return false
	}
	if code < 100 || code > 599 {
		return true
	}
	if operation_setting.IsAlwaysSkipRetryCode(openaiErr.GetErrorCode()) {
		return false
	}
	return operation_setting.ShouldRetryByStatusCode(code)
}

func processChannelError(c *gin.Context, channelError types.ChannelError, err *types.NewAPIError) {
	adminError := err.ErrorWithStatusCode()
	if constant.IsOpenCodeChannelType(channelError.ChannelType) {
		adminError = service.OpenCodeGoAdminErrorWithStatusCode(err)
	}
	logger.LogError(c, fmt.Sprintf("channel error (channel #%d): %s", channelError.ChannelId, common.LocalLogPreview(adminError)))
	// 不要使用context获取渠道信息，异步处理时可能会出现渠道信息不一致的情况
	// do not use context to get channel info, there may be inconsistent channel info when processing asynchronously
	if shouldDisableWholeChannel(channelError.ChannelType, err) && channelError.AutoBan {
		gopool.Go(func() {
			service.DisableChannel(channelError, adminError)
		})
	}

	if constant.ErrorLogEnabled && types.IsRecordErrorLog(err) {
		// 保存错误日志到mysql中
		userId := c.GetInt("id")
		tokenName := c.GetString("token_name")
		modelName := c.GetString("original_model")
		tokenId := c.GetInt("token_id")
		userGroup := c.GetString("group")
		channelId := c.GetInt("channel_id")
		other := make(map[string]interface{})
		if c.Request != nil && c.Request.URL != nil {
			other["request_path"] = c.Request.URL.Path
		}
		other["error_type"] = err.GetErrorType()
		other["error_code"] = err.GetErrorCode()
		other["status_code"] = err.StatusCode
		other["channel_id"] = channelId
		other["channel_name"] = c.GetString("channel_name")
		other["channel_type"] = c.GetInt("channel_type")
		adminInfo := make(map[string]interface{})
		adminInfo["use_channel"] = c.GetStringSlice("use_channel")
		isMultiKey := common.GetContextKeyBool(c, constant.ContextKeyChannelIsMultiKey)
		if isMultiKey {
			adminInfo["is_multi_key"] = true
			adminInfo["multi_key_index"] = common.GetContextKeyInt(c, constant.ContextKeyChannelMultiKeyIndex)
		}
		service.AppendChannelAffinityAdminInfo(c, adminInfo)
		service.AppendOpenCodeGoWorkspaceAdminInfo(c, adminInfo)
		if upstreamStatusCode, ok := service.OpenCodeGoUpstreamRelayStatusCode(err); ok {
			adminInfo["upstream_status_code"] = upstreamStatusCode
		}
		if provenance := err.Provenance(); !provenance.IsZero() {
			adminInfo["error_origin"] = provenance.Origin
			adminInfo["error_subtype"] = provenance.Subtype
		}
		other["admin_info"] = adminInfo
		startTime := common.GetContextKeyTime(c, constant.ContextKeyRequestStartTime)
		if startTime.IsZero() {
			startTime = time.Now()
		}
		useTimeSeconds := int(time.Since(startTime).Seconds())
		logMessage := err.MaskSensitiveErrorWithStatusCode()
		if constant.IsOpenCodeChannelType(channelError.ChannelType) {
			logMessage = adminError
		}
		model.RecordErrorLog(c, userId, channelId, modelName, tokenName, logMessage, tokenId, useTimeSeconds, common.GetContextKeyBool(c, constant.ContextKeyIsStream), userGroup, other)
	}

}

func shouldDisableWholeChannel(channelType int, err *types.NewAPIError) bool {
	return channelType != constant.ChannelTypeOpenCodeGo && service.ShouldDisableChannel(err)
}

func RelayMidjourney(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatMjProxy, nil, nil)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"description": fmt.Sprintf("failed to generate relay info: %s", err.Error()),
			"type":        "upstream_error",
			"code":        4,
		})
		return
	}

	var mjErr *taskdto.MidjourneyResponse
	switch relayInfo.RelayMode {
	case relayconstant.RelayModeMidjourneyNotify:
		mjErr = relay.RelayMidjourneyNotify(c)
	case relayconstant.RelayModeMidjourneyTaskFetch, relayconstant.RelayModeMidjourneyTaskFetchByCondition:
		mjErr = relay.RelayMidjourneyTask(c, relayInfo.RelayMode)
	case relayconstant.RelayModeMidjourneyTaskImageSeed:
		mjErr = relay.RelayMidjourneyTaskImageSeed(c)
	case relayconstant.RelayModeSwapFace:
		mjErr = relay.RelaySwapFace(c, relayInfo)
	default:
		mjErr = relay.RelayMidjourneySubmit(c, relayInfo)
	}
	//err = relayMidjourneySubmit(c, relayMode)
	log.Println(mjErr)
	if mjErr != nil {
		statusCode := http.StatusBadRequest
		if mjErr.Code == 30 {
			mjErr.Result = "当前分组负载已饱和，请稍后再试，或升级账户以提升服务质量。"
			statusCode = http.StatusTooManyRequests
		}
		c.JSON(statusCode, gin.H{
			"description": fmt.Sprintf("%s %s", mjErr.Description, mjErr.Result),
			"type":        "upstream_error",
			"code":        mjErr.Code,
		})
		channelId := c.GetInt("channel_id")
		logger.LogError(c, fmt.Sprintf("relay error (channel #%d, status code %d): %s", channelId, statusCode, fmt.Sprintf("%s %s", mjErr.Description, mjErr.Result)))
	}
}

func RelayNotImplemented(c *gin.Context) {
	err := types.OpenAIError{
		Message: "API not implemented",
		Type:    "new_api_error",
		Param:   "",
		Code:    "api_not_implemented",
	}
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": err,
	})
}

func RelayNotFound(c *gin.Context) {
	err := types.OpenAIError{
		Message: fmt.Sprintf("Invalid URL (%s %s)", c.Request.Method, c.Request.URL.Path),
		Type:    "invalid_request_error",
		Param:   "",
		Code:    "",
	}
	c.JSON(http.StatusNotFound, gin.H{
		"error": err,
	})
}

func RelayTaskFetch(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
	if err != nil {
		respondTaskErrorForRequest(c, &taskdto.TaskError{
			Code:       "gen_relay_info_failed",
			Message:    err.Error(),
			StatusCode: http.StatusInternalServerError,
		})
		return
	}
	if taskErr := relay.RelayTaskFetch(c, relayInfo.RelayMode); taskErr != nil {
		respondTaskErrorForRequest(c, taskErr)
	}
}

func mergeReliableTaskSubmitResult(
	existing *relay.TaskSubmitResult,
	incoming *relay.TaskSubmitResult,
) (*relay.TaskSubmitResult, *taskdto.TaskError) {
	if incoming == nil {
		return existing, nil
	}
	if existing == nil {
		return incoming, nil
	}
	if existing.UpstreamTaskID == "" {
		existing.UpstreamTaskID = incoming.UpstreamTaskID
	} else if incoming.UpstreamTaskID != "" &&
		incoming.UpstreamTaskID != existing.UpstreamTaskID {
		retryable := false
		err := errors.New("conflicting reliable upstream task identities")
		return existing, &taskdto.TaskError{
			Code:       "reliable_task_identity_conflict",
			Message:    "conflicting upstream task result",
			StatusCode: http.StatusInternalServerError,
			Retryable:  &retryable,
			LocalError: true,
			Error:      err,
		}
	}
	if len(existing.TaskData) == 0 && len(incoming.TaskData) != 0 {
		existing.TaskData = append([]byte(nil), incoming.TaskData...)
	}
	if existing.Platform == "" {
		existing.Platform = incoming.Platform
	}
	if existing.Quota == 0 {
		existing.Quota = incoming.Quota
	}
	if existing.HTTPResponse == nil {
		existing.HTTPResponse = incoming.HTTPResponse
	}
	return existing, nil
}

func durableTaskSettlementError(err error) *taskdto.TaskError {
	retryable := false
	if err == nil {
		err = errors.New("durable task billing settlement failed")
	}
	return &taskdto.TaskError{
		Code:       "seedance_billing_settlement_failed",
		Message:    "task billing settlement failed",
		StatusCode: http.StatusInternalServerError,
		Retryable:  &retryable,
		LocalError: true,
		Error:      err,
	}
}

func failDurableTaskAfterSettlementError(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	result *relay.TaskSubmitResult,
	settlementErr error,
) *taskdto.TaskError {
	if info == nil || info.TaskRelayInfo == nil {
		return durableTaskSettlementError(settlementErr)
	}
	upstreamTaskID := ""
	var taskData []byte
	if result != nil {
		upstreamTaskID = result.UpstreamTaskID
		taskData = result.TaskData
	}
	recoveryErr := service.FailAndRefundTaskSubmission(
		durableTaskRequestContext(c),
		info.PersistentTaskID,
		info.BillingAttemptRequestID,
		upstreamTaskID,
		taskData,
		"seedance_billing_settlement_failed",
		"task billing settlement failed",
	)
	return durableTaskSettlementError(errors.Join(settlementErr, recoveryErr))
}

func durableTaskRequestContext(c *gin.Context) context.Context {
	if c != nil && c.Request != nil {
		return c.Request.Context()
	}
	return context.Background()
}

func finalizeDurableTaskSubmission(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	result *relay.TaskSubmitResult,
) *taskdto.TaskError {
	if info == nil || info.TaskRelayInfo == nil || result == nil ||
		result.HTTPResponse == nil || info.BillingAttemptRequestID == "" {
		return failDurableTaskAfterSettlementError(
			c,
			info,
			result,
			errors.New("durable task success state is incomplete"),
		)
	}
	if taskErr := relay.ValidateFullPrepaidTaskBilling(info, result.Quota); taskErr != nil {
		return failDurableTaskAfterSettlementError(c, info, result, taskErr.Error)
	}
	relay.RecordTaskSubmissionEvent(c, "validate_full_prepaid_again")
	if err := service.SettleBilling(c, info, result.Quota); err != nil {
		return failDurableTaskAfterSettlementError(c, info, result, err)
	}
	relay.RecordTaskSubmissionEvent(c, "settle_zero_delta")
	if err := model.MarkTaskBillingAttemptSubmissionSettled(
		info.BillingAttemptRequestID,
	); err != nil {
		return failDurableTaskAfterSettlementError(c, info, result, err)
	}
	relay.RecordTaskSubmissionEvent(c, "mark_billing_attempt_submission_settled")
	service.LogTaskConsumption(c, info)
	relay.RecordTaskSubmissionEvent(c, "consume_log")
	c.JSON(result.HTTPResponse.StatusCode, result.HTTPResponse.Body)
	relay.RecordTaskSubmissionEvent(c, "write_http_200")
	return nil
}

func refundLegacyTaskBillingOnFailure(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	taskErr *taskdto.TaskError,
) {
	if info == nil || taskErr == nil || info.Billing == nil {
		return
	}
	durableAttempt := info.TaskRelayInfo != nil &&
		info.BillingAttemptRequestID != ""
	if !durableAttempt {
		info.Billing.Refund(c)
	}
}

func recoverDurableTaskSubmission(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	result *relay.TaskSubmitResult,
	taskErr *taskdto.TaskError,
) error {
	if info == nil || taskErr == nil || info.TaskRelayInfo == nil ||
		info.BillingAttemptRequestID == "" {
		return nil
	}
	upstreamTaskID := ""
	var taskData []byte
	if result != nil {
		upstreamTaskID = result.UpstreamTaskID
		taskData = result.TaskData
	}
	return service.FailAndRefundTaskSubmission(
		durableTaskRequestContext(c),
		info.PersistentTaskID,
		info.BillingAttemptRequestID,
		upstreamTaskID,
		taskData,
		taskErr.Code,
		"task submission failed",
	)
}

func RelayTask(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
	if err != nil {
		respondTaskErrorForRequest(c, &taskdto.TaskError{
			Code:       "gen_relay_info_failed",
			Message:    err.Error(),
			StatusCode: http.StatusInternalServerError,
		})
		return
	}

	if taskErr := relay.ResolveOriginTask(c, relayInfo); taskErr != nil {
		respondTaskErrorForRequest(c, taskErr)
		return
	}

	var result *relay.TaskSubmitResult
	var taskErr *taskdto.TaskError
	defer func() {
		refundLegacyTaskBillingOnFailure(c, relayInfo, taskErr)
	}()

	retryParam := &service.RetryParam{
		Ctx:         c,
		TokenGroup:  relayInfo.TokenGroup,
		ModelName:   relayInfo.OriginModelName,
		RequestPath: c.Request.URL.Path,
		Retry:       common.GetPointer(0),
	}

	for ; retryParam.GetRetry() <= common.RetryTimes; retryParam.IncreaseRetry() {
		var channel *model.Channel

		if lockedCh, ok := relayInfo.LockedChannel.(*model.Channel); ok && lockedCh != nil {
			channel = lockedCh
			if retryParam.GetRetry() > 0 {
				if setupErr := middleware.SetupContextForSelectedChannel(c, channel, relayInfo.OriginModelName); setupErr != nil {
					taskErr = service.TaskErrorWrapperLocal(setupErr.Err, "setup_locked_channel_failed", http.StatusInternalServerError)
					break
				}
			}
		} else {
			var channelErr *types.NewAPIError
			channel, channelErr = getTaskRelayChannel(c, relayInfo, retryParam)
			if channelErr != nil {
				logger.LogError(c, channelErr.Error())
				taskErr = service.TaskErrorWrapperLocal(channelErr.Err, "get_channel_failed", http.StatusInternalServerError)
				break
			}
		}

		addUsedChannel(c, channel.Id)
		bodyStorage, bodyErr := common.GetBodyStorage(c)
		if bodyErr != nil {
			if common.IsRequestBodyTooLargeError(bodyErr) || errors.Is(bodyErr, common.ErrRequestBodyTooLarge) {
				taskErr = service.TaskErrorWrapperLocal(bodyErr, "read_request_body_failed", http.StatusRequestEntityTooLarge)
			} else {
				taskErr = service.TaskErrorWrapperLocal(bodyErr, "read_request_body_failed", http.StatusBadRequest)
			}
			break
		}
		c.Request.Body = io.NopCloser(bodyStorage)

		attemptResult, attemptErr := relay.RelayTaskSubmit(c, relayInfo)
		var mergeErr *taskdto.TaskError
		result, mergeErr = mergeReliableTaskSubmitResult(result, attemptResult)
		if mergeErr != nil {
			taskErr = mergeErr
		} else {
			taskErr = attemptErr
		}
		if taskErr == nil {
			break
		}

		if !taskErr.LocalError {
			processChannelError(c,
				*types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey,
					common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()),
				types.NewOpenAIError(taskErr.Error, types.ErrorCodeBadResponseStatusCode, taskErr.StatusCode))
		}

		if result != nil && result.UpstreamTaskID != "" {
			break
		}
		if !shouldRetryTaskRelay(c, channel.Id, taskErr, common.RetryTimes-retryParam.GetRetry()) {
			break
		}
	}

	useChannel := c.GetStringSlice("use_channel")
	if len(useChannel) > 1 {
		retryLogStr := fmt.Sprintf("重试：%s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(useChannel)), "->"), "[]"))
		logger.LogInfo(c, retryLogStr)
	}

	if recoveryErr := recoverDurableTaskSubmission(
		c,
		relayInfo,
		result,
		taskErr,
	); recoveryErr != nil {
		common.SysError("durable task submission recovery incomplete")
	}

	// ── 成功：durable 路径先结算/marker/日志/HTTP；旧 adaptor 保持原行为 ──
	if taskErr == nil {
		durableAttempt := relayInfo.TaskRelayInfo != nil &&
			relayInfo.BillingAttemptRequestID != ""
		if durableAttempt {
			taskErr = finalizeDurableTaskSubmission(c, relayInfo, result)
		} else {
			if settleErr := service.SettleBilling(c, relayInfo, result.Quota); settleErr != nil {
				common.SysError("settle task billing error: " + settleErr.Error())
			}
			service.LogTaskConsumption(c, relayInfo)

			task := model.InitTask(result.Platform, relayInfo)
			task.PrivateData.UpstreamTaskID = result.UpstreamTaskID
			task.PrivateData.BillingSource = relayInfo.BillingSource
			task.PrivateData.SubscriptionId = relayInfo.SubscriptionId
			task.PrivateData.TokenId = relayInfo.TokenId
			task.PrivateData.NodeName = common.NodeName
			task.PrivateData.BillingContext = &model.TaskBillingContext{
				ModelPrice:      relayInfo.PriceData.ModelPrice,
				GroupRatio:      relayInfo.PriceData.GroupRatioInfo.GroupRatio,
				ModelRatio:      relayInfo.PriceData.ModelRatio,
				OtherRatios:     relayInfo.PriceData.OtherRatios(),
				OriginModelName: relayInfo.OriginModelName,
				PerCallBilling: common.StringsContains(
					constant.TaskPricePatches,
					relayInfo.OriginModelName,
				) || relayInfo.PriceData.UsePrice,
			}
			task.Quota = result.Quota
			task.Data = result.TaskData
			task.Action = relayInfo.Action
			if insertErr := task.Insert(); insertErr != nil {
				common.SysError("insert task error: " + insertErr.Error())
			}
		}
	}

	if taskErr != nil {
		respondTaskErrorForRequest(c, taskErr)
	}
}

// writeOpenAIVideoError writes the OpenAI video API's nested error schema.
// It is deliberately selected only by isOpenAIVideoPath so old task routes
// preserve their established flat TaskError response contract.
func writeOpenAIVideoError(
	c *gin.Context,
	status int,
	errorType string,
	code string,
	message string,
) {
	c.AbortWithStatusJSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    errorType,
			"code":    code,
		},
	})
}

func isOpenAIVideoPath(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	path := c.Request.URL.Path
	if path == "/v1/videos" {
		return true
	}
	if !strings.HasPrefix(path, "/v1/videos/") {
		return false
	}
	segments := strings.Split(strings.TrimPrefix(path, "/v1/videos/"), "/")
	if len(segments) == 1 {
		return segments[0] != ""
	}
	return len(segments) == 2 && segments[0] != "" && segments[1] == "content"
}

func openAIVideoTaskErrorType(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= http.StatusInternalServerError:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

func openAIVideoTaskError(taskErr *taskdto.TaskError) (int, string, string, string) {
	if taskErr == nil {
		return http.StatusInternalServerError, "server_error", "server_error", "internal server error"
	}
	if taskErr.Code == "task_not_exist" {
		return http.StatusNotFound, "invalid_request_error", "task_not_found", "video task was not found"
	}
	return taskErr.StatusCode,
		openAIVideoTaskErrorType(taskErr.StatusCode),
		taskErr.Code,
		taskErr.Message
}

func respondTaskErrorForRequest(c *gin.Context, taskErr *taskdto.TaskError) {
	if isOpenAIVideoPath(c) {
		status, errorType, code, message := openAIVideoTaskError(taskErr)
		writeOpenAIVideoError(c, status, errorType, code, message)
		return
	}
	respondTaskError(c, taskErr)
}

// respondTaskError 统一输出 Task 错误响应（含 429 限流提示改写）
func respondTaskError(c *gin.Context, taskErr *taskdto.TaskError) {
	if taskErr.StatusCode == http.StatusTooManyRequests {
		taskErr.Message = groupUpstreamOverloadedMessage
	}
	c.JSON(taskErr.StatusCode, taskErr)
}

func shouldRetryTaskRelay(c *gin.Context, channelId int, taskErr *taskdto.TaskError, retryTimes int) bool {
	if taskErr == nil {
		return false
	}
	if service.ShouldSkipRetryAfterChannelAffinityFailure(c) {
		return false
	}
	if retryTimes <= 0 {
		return false
	}
	if _, ok := c.Get("specific_channel_id"); ok {
		return false
	}
	if taskErr.Retryable != nil {
		return *taskErr.Retryable
	}
	if taskErr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if taskErr.StatusCode == 307 {
		return true
	}
	if taskErr.StatusCode/100 == 5 {
		// 超时不重试
		if operation_setting.IsAlwaysSkipRetryStatusCode(taskErr.StatusCode) {
			return false
		}
		return true
	}
	if taskErr.StatusCode == http.StatusBadRequest {
		return false
	}
	if taskErr.StatusCode == 408 {
		// azure处理超时不重试
		return false
	}
	if taskErr.LocalError {
		return false
	}
	if taskErr.StatusCode/100 == 2 {
		return false
	}
	return true
}
