package opencodego

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

type Adaptor struct {
	protocol              Protocol
	protocolErr           error
	cacheIdentity         string
	affinityIdentity      string
	affinitySource        string
	bufferClaudeToolCall  bool
	requestInputItems     int
	requestToolCount      int
	requestUpstreamStream bool
	converted             bool
	workspaceSelected     bool
	selectedWorkspaceUID  string
	statefulResponses     bool
	failoverAttempt       *service.OpenCodeGoFailoverAttempt
	namespaceTools        map[string]openCodeGoNamespaceTool
	releaseOnce           sync.Once
	releaseInFlightFn     func()
	openai                openai.Adaptor
	claude                claude.Adaptor
}

var selectOpenCodeGoWorkspace = service.SelectOpenCodeGoWorkspaceWithFailover

var doOpenCodeGoAPIRequest = channel.DoApiRequest

var observeOpenCodeGoTransportFailure = service.ObserveOpenCodeGoTransportFailure

var observeOpenCodeGoFailoverFailure = service.ObserveOpenCodeGoFailoverFailure

var observeOpenCodeGoFailoverSuccess = service.ObserveOpenCodeGoFailoverSuccess

var openCodeGoHealthNow = time.Now

func (a *Adaptor) Init(info *relaycommon.RelayInfo) {
	a.protocol = ""
	a.protocolErr = nil
	a.cacheIdentity = ""
	a.affinityIdentity = ""
	a.affinitySource = ""
	a.bufferClaudeToolCall = false
	a.requestInputItems = 0
	a.requestToolCount = 0
	a.requestUpstreamStream = false
	a.converted = false
	a.workspaceSelected = false
	a.selectedWorkspaceUID = ""
	a.statefulResponses = false
	a.failoverAttempt = nil
	a.releaseOnce = sync.Once{}
	a.releaseInFlightFn = nil
	a.namespaceTools = nil
	if info == nil {
		return
	}
	a.openai.Init(info)
	a.claude.Init(info)
}

// acquireInFlight registers the per-workspace in-flight slot for this request
// after a workspace is selected. It must be balanced by exactly one
// releaseInFlight call; releaseOnce guards the three terminal paths.
func (a *Adaptor) acquireInFlight(channelID int, workspaceUID string) {
	if channelID <= 0 || strings.TrimSpace(workspaceUID) == "" {
		return
	}
	service.AcquireOpenCodeGoWorkspaceInFlight(channelID, workspaceUID)
	a.releaseInFlightFn = func() {
		service.ReleaseOpenCodeGoWorkspaceInFlight(channelID, workspaceUID)
	}
}

func (a *Adaptor) releaseInFlight() {
	if a == nil || a.releaseInFlightFn == nil {
		return
	}
	a.releaseOnce.Do(a.releaseInFlightFn)
}

func (a *Adaptor) resolveProtocol(info *relaycommon.RelayInfo) (Protocol, error) {
	if a.protocol != "" || a.protocolErr != nil {
		return a.protocol, a.protocolErr
	}
	if info == nil || info.ChannelMeta == nil {
		a.protocolErr = errors.New("OpenCode Go relay info is missing channel metadata")
		return "", a.protocolErr
	}
	a.protocol, a.protocolErr = ResolveProtocol(info.UpstreamModelName, info.ChannelOtherSettings.OpenCodeGo)
	return a.protocol, a.protocolErr
}

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	protocol, err := a.resolveProtocol(info)
	if err != nil {
		return "", err
	}
	baseURL := strings.TrimRight(constant.ChannelBaseURLs[constant.ChannelTypeOpenCodeGo], "/")
	switch protocol {
	case ProtocolChat:
		return baseURL + "/chat/completions", nil
	case ProtocolMessages:
		return baseURL + "/messages", nil
	case ProtocolResponses:
		return baseURL + "/responses", nil
	default:
		return "", fmt.Errorf("unsupported OpenCode Go protocol %q", protocol)
	}
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, header *http.Header, info *relaycommon.RelayInfo) error {
	protocol, err := a.resolveProtocol(info)
	if err != nil {
		return err
	}
	if !a.workspaceSelected {
		// convertRequest normally resolves the affinity identity before this
		// point; for relay modes that skip conversion, resolve it here from the
		// incoming request so workspace selection and the consume-log attribution
		// always agree.
		if a.affinityIdentity == "" && a.affinitySource == "" {
			a.affinityIdentity, a.affinitySource = affinityIdentityForRequest(c, info, nil)
		}
		common.SetContextKey(c, constant.ContextKeyOpenCodeGoAffinitySource, a.affinitySource)
		common.SetContextKey(c, constant.ContextKeyOpenCodeGoAffinityKey, a.affinityIdentity)
		loadAware := info != nil && info.ChannelOtherSettings.OpenCodeGo != nil &&
			info.ChannelOtherSettings.OpenCodeGo.LoadAwareEnabled
		selection, selectErr := selectOpenCodeGoWorkspace(info.ChannelId, info.UpstreamModelName, service.OpenCodeGoPoolSelectOptions{
			AffinityKey: a.affinityIdentity,
			Protocol:    string(protocol),
			Stateful:    a.statefulResponses,
			Failover:    service.ResolveOpenCodeGoFailoverPolicy(info.ChannelOtherSettings.OpenCodeGo),
			LoadAware:   loadAware,
		})
		if selectErr != nil {
			return selectErr
		}
		info.ApiKey = selection.APIKey
		a.selectedWorkspaceUID = selection.WorkspaceUID
		a.failoverAttempt = selection.FailoverAttempt
		a.workspaceSelected = true
		a.acquireInFlight(info.ChannelId, selection.WorkspaceUID)
		common.SetContextKey(c, constant.ContextKeyOpenCodeGoWorkspaceUID, selection.WorkspaceUID)
		if selection.FailoverActive {
			leaseRemaining := time.Until(selection.FailoverLeaseExpiresAt).Round(time.Second)
			if leaseRemaining < 0 {
				leaseRemaining = 0
			}
			logger.LogInfo(c, fmt.Sprintf(
				"OpenCode Go failover selection: channel_id=%d model=%q protocol=%q rank=%d canonical_ref=%s workspace_ref=%s lease_remaining=%s",
				info.ChannelId,
				info.UpstreamModelName,
				protocol,
				selection.CandidateRank,
				hashCacheIdentity("diagnostic-workspace", selection.CanonicalWorkspaceUID),
				hashCacheIdentity("diagnostic-workspace", selection.WorkspaceUID),
				leaseRemaining,
			))
		}
	}
	if strings.TrimSpace(info.ApiKey) == "" {
		return errors.New("OpenCode Go request has no selected account API key")
	}
	if a.cacheIdentity == "" {
		a.cacheIdentity = cacheIdentityForRequest(c, info, nil)
	}

	channel.SetupApiRequestHeader(info, c, header)
	header.Del("Authorization")
	header.Del("x-api-key")
	header.Set(cacheIdentityHeader, a.cacheIdentity)
	header.Set("x-opencode-client", "new-api")

	if protocol == ProtocolMessages {
		header.Set("x-api-key", info.ApiKey)
		version := ""
		if c != nil && c.Request != nil {
			version = c.Request.Header.Get("anthropic-version")
		}
		if version == "" {
			version = "2023-06-01"
		}
		header.Set("anthropic-version", version)
		return nil
	}
	header.Set("Authorization", "Bearer "+info.ApiKey)
	return nil
}

func (a *Adaptor) convertRequest(c *gin.Context, info *relaycommon.RelayInfo, request any) (any, error) {
	if !isSupportedOpenCodeGoClientRequest(info) {
		if info == nil {
			return nil, errors.New("OpenCode Go relay info is nil")
		}
		return nil, fmt.Errorf("OpenCode Go does not support relay format %q in mode %d", info.RelayFormat, info.RelayMode)
	}
	if request == nil {
		return nil, errors.New("OpenCode Go request is nil")
	}
	protocol, err := a.resolveProtocol(info)
	if err != nil {
		return nil, err
	}
	a.namespaceTools = nil
	switch typed := request.(type) {
	case *dto.OpenAIResponsesRequest:
		a.namespaceTools, err = prepareOpenCodeGoResponsesTools(typed)
		if err != nil {
			return nil, err
		}
	case dto.OpenAIResponsesRequest:
		copy := typed
		a.namespaceTools, err = prepareOpenCodeGoResponsesTools(&copy)
		if err != nil {
			return nil, err
		}
		request = &copy
	}
	usesFunctionTools := requestUsesFunctionTools(request)
	a.statefulResponses = requestUsesStatefulResponses(request)
	// Claude tool turns must stay on Chat for Chat-family models. Console Go's
	// Responses bridge drops oa-compatible reasoning_content from the first
	// tool response, so Claude Code cannot replay the provider reasoning on the
	// following tool-result turn. The buffered Chat path below already solves
	// the incomplete streamed-tool-arguments problem without changing protocol.
	if protocol == ProtocolChat && usesFunctionTools && info.RelayFormat != types.RelayFormatClaude &&
		!requestContainsAssistantReasoning(request) {
		protocol = ProtocolResponses
		a.protocol = protocol
	}

	a.cacheIdentity = cacheIdentityForRequest(c, info, request)
	a.affinityIdentity, a.affinitySource = affinityIdentityForRequest(c, info, request)
	// Console Go can omit complete function-argument deltas from streaming tool
	// results. Buffer any streaming Claude function-tool turn, whether its model
	// uses Chat or Responses, into one non-streaming upstream request and
	// synthesize Claude SSE without retrying or switching accounts.
	a.bufferClaudeToolCall = (protocol == ProtocolResponses || protocol == ProtocolChat) &&
		info.IsStream && info.RelayFormat == types.RelayFormatClaude && usesFunctionTools
	a.captureRequestShape(request)
	result, err := service.ConvertRequest(c, info, protocol.RelayFormat(), request)
	if err != nil {
		return nil, err
	}
	if result == nil || result.Value == nil {
		return nil, errors.New("OpenCode Go request conversion returned no value")
	}

	switch converted := result.Value.(type) {
	case *dto.GeneralOpenAIRequest:
		converted.Model = info.UpstreamModelName
		if a.bufferClaudeToolCall {
			upstreamStream := false
			converted.Stream = &upstreamStream
		}
		a.requestUpstreamStream = converted.Stream != nil && *converted.Stream
		if info.IsStream && a.requestUpstreamStream {
			converted.StreamOptions = &dto.StreamOptions{IncludeUsage: true}
		}
		if strings.EqualFold(info.UpstreamModelName, "kimi-k2.7-code") &&
			converted.Temperature != nil && *converted.Temperature != 1 {
			converted.Temperature = nil
		}
	case *dto.ClaudeRequest:
		converted.Model = info.UpstreamModelName
		a.requestUpstreamStream = converted.Stream != nil && *converted.Stream
	case *dto.OpenAIResponsesRequest:
		converted.Model = info.UpstreamModelName
		if a.bufferClaudeToolCall {
			upstreamStream := false
			converted.Stream = &upstreamStream
		}
		if err := prepareOpenCodeGoResponsesToolHistory(converted); err != nil {
			return nil, err
		}
		a.requestUpstreamStream = converted.Stream != nil && *converted.Stream
		cacheKey, marshalErr := common.Marshal(a.cacheIdentity)
		if marshalErr != nil {
			return nil, marshalErr
		}
		converted.PromptCacheKey = cacheKey
	default:
		return nil, fmt.Errorf("unsupported OpenCode Go converted request type %T", result.Value)
	}
	info.FinalRequestRelayFormat = protocol.RelayFormat()
	a.converted = true
	return result.Value, nil
}

func isSupportedOpenCodeGoClientRequest(info *relaycommon.RelayInfo) bool {
	if info == nil {
		return false
	}
	switch info.RelayFormat {
	case types.RelayFormatOpenAI:
		return info.RelayMode == relayconstant.RelayModeChatCompletions
	case types.RelayFormatClaude:
		return info.RelayMode == relayconstant.RelayModeUnknown
	case types.RelayFormatOpenAIResponses:
		return info.RelayMode == relayconstant.RelayModeResponses
	default:
		return false
	}
}

func (a *Adaptor) ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	return a.convertRequest(c, info, request)
}

func (a *Adaptor) ConvertClaudeRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.ClaudeRequest) (any, error) {
	return a.convertRequest(c, info, request)
}

func (a *Adaptor) ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	return a.convertRequest(c, info, &request)
}

func (a *Adaptor) ConvertGeminiRequest(*gin.Context, *relaycommon.RelayInfo, *dto.GeminiChatRequest) (any, error) {
	return nil, errors.New("OpenCode Go does not support Gemini requests")
}

func (a *Adaptor) ConvertRerankRequest(*gin.Context, int, dto.RerankRequest) (any, error) {
	return nil, errors.New("OpenCode Go does not support rerank requests")
}

func (a *Adaptor) ConvertEmbeddingRequest(*gin.Context, *relaycommon.RelayInfo, dto.EmbeddingRequest) (any, error) {
	return nil, errors.New("OpenCode Go does not support embedding requests")
}

func (a *Adaptor) ConvertAudioRequest(*gin.Context, *relaycommon.RelayInfo, dto.AudioRequest) (io.Reader, error) {
	return nil, errors.New("OpenCode Go does not support audio requests")
}

func (a *Adaptor) ConvertImageRequest(*gin.Context, *relaycommon.RelayInfo, dto.ImageRequest) (any, error) {
	return nil, errors.New("OpenCode Go does not support image requests")
}

func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	if !a.converted {
		return nil, errors.New("OpenCode Go does not allow pass-through request bodies")
	}
	response, err := doOpenCodeGoAPIRequest(a, c, info, requestBody)
	if err != nil {
		a.releaseInFlight()
		if a.workspaceSelected && !openCodeGoCallerCancelled(c, err) && info != nil {
			reason := sanitizeErrorMessage(err.Error())
			if _, observeErr := observeOpenCodeGoTransportFailure(
				info.ChannelId,
				a.selectedWorkspaceUID,
				info.UpstreamModelName,
				reason,
				openCodeGoHealthNow(),
			); observeErr != nil {
				common.SysError(fmt.Sprintf(
					"failed to persist OpenCode Go transport health observation: channel_id=%d workspace_ref=%s error=%v",
					info.ChannelId,
					hashCacheIdentity("diagnostic-workspace", a.selectedWorkspaceUID),
					observeErr,
				))
			}
		}
	}
	return response, err
}

func openCodeGoCallerCancelled(c *gin.Context, err error) bool {
	if c != nil && c.Request != nil && c.Request.Context().Err() != nil {
		return true
	}
	return errors.Is(err, context.Canceled)
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	protocol, err := a.resolveProtocol(info)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	if info == nil {
		return nil, types.NewOpenAIError(errors.New("OpenCode Go relay info is nil"), types.ErrorCodeBadResponse, http.StatusInternalServerError, types.ErrOptionWithSkipRetry())
	}
	defer a.releaseInFlight()
	// Responses and Messages have explicit protocol terminal events. Preserve
	// that requirement through the stream helper so an early EOF cannot be
	// recorded as a successful request. Chat uses the SSE [DONE] sentinel.
	info.StreamProtocolTerminalRequired = info.IsStream && protocol != ProtocolChat

	clientModel := info.OriginModelName
	if clientModel == "" {
		clientModel = info.UpstreamModelName
	}
	state := &responseTransformState{
		model:                clientModel,
		protocol:             protocol,
		namespaceTools:       a.namespaceTools,
		estimatedInputTokens: info.GetEstimatePromptTokens(),
	}
	if err := prepareResponseForRelay(resp, state, info.IsStream && !a.bufferClaudeToolCall); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError, types.ErrOptionWithSkipRetry())
	}

	upstreamModel := info.UpstreamModelName
	info.FinalRequestRelayFormat = protocol.RelayFormat()
	info.UpstreamModelName = clientModel
	defer func() {
		info.UpstreamModelName = upstreamModel
	}()

	service.ResetResponseBodyWriteError(c)
	var usage any
	var responseErr *types.NewAPIError
	switch protocol {
	case ProtocolChat:
		if a.bufferClaudeToolCall {
			usage, responseErr = openai.OaiChatToClaudeBufferedStreamHandler(c, info, resp)
		} else if info.RelayFormat == types.RelayFormatOpenAIResponses {
			if info.IsStream {
				usage, responseErr = openai.OaiChatToResponsesStreamHandler(c, info, resp)
			} else {
				usage, responseErr = openai.OaiChatToResponsesHandler(c, info, resp)
			}
		} else {
			usage, responseErr = a.openai.DoResponse(c, resp, info)
		}
	case ProtocolMessages:
		if info.RelayFormat == types.RelayFormatOpenAIResponses {
			if info.IsStream {
				usage, responseErr = messagesToResponsesStreamHandler(c, info, resp)
			} else {
				usage, responseErr = messagesToResponsesHandler(c, info, resp)
			}
		} else {
			usage, responseErr = a.claude.DoResponse(c, resp, info)
		}
	case ProtocolResponses:
		if a.bufferClaudeToolCall {
			usage, responseErr = openai.OaiResponsesToClaudeBufferedStreamHandler(c, info, resp)
		} else if info.RelayFormat == types.RelayFormatOpenAIResponses {
			if info.IsStream {
				usage, responseErr = openai.OaiResponsesStreamHandler(c, info, resp)
			} else {
				usage, responseErr = openai.OaiResponsesHandler(c, info, resp)
			}
		} else if info.IsStream {
			usage, responseErr = openai.OaiResponsesToChatStreamHandler(c, info, resp)
		} else {
			usage, responseErr = openai.OaiResponsesToChatHandler(c, info, resp)
		}
	default:
		responseErr = types.NewOpenAIError(fmt.Errorf("unsupported OpenCode Go protocol %q", protocol), types.ErrorCodeBadResponse, http.StatusInternalServerError, types.ErrOptionWithSkipRetry())
	}
	streamFailureReason := a.openCodeGoStreamFailureReason(c, info, responseErr)
	if streamFailureReason != "" {
		a.recordFailoverFailure(c, info, streamFailureReason)
	}
	if responseErr != nil {
		if state.sawUpstreamError {
			responseErr = service.MarkOpenCodeGoUpstreamRelayError(responseErr)
		}
		return nil, responseErr
	}
	if state.sawUpstreamError {
		responseErr = types.WithOpenAIError(types.OpenAIError{
			Message: string(state.upstreamErrorPayload),
			Type:    "upstream_error",
			Code:    "upstream_error",
		}, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
		return nil, service.MarkOpenCodeGoUpstreamRelayError(responseErr)
	}
	if writeErr := service.ResponseBodyWriteError(c); writeErr != nil {
		return nil, types.NewOpenAIError(
			writeErr,
			types.ErrorCodeBadResponse,
			http.StatusInternalServerError,
			types.ErrOptionWithSkipRetry(),
		)
	}
	if settlementErr := a.openCodeGoStreamSettlementError(c, info); settlementErr != nil {
		return nil, settlementErr
	}
	if streamFailureReason == "" && !a.openCodeGoStreamHasLocalErrors(info) {
		a.recordFailoverSuccess(c, info)
	}
	return finalizeResponseUsage(usage, state), nil
}

func requestUsesStatefulResponses(request any) bool {
	var responses *dto.OpenAIResponsesRequest
	switch typed := request.(type) {
	case *dto.OpenAIResponsesRequest:
		responses = typed
	case dto.OpenAIResponsesRequest:
		copy := typed
		responses = &copy
	}
	if responses == nil {
		return false
	}
	return strings.TrimSpace(responses.PreviousResponseID) != "" ||
		rawJSONFieldPresent(responses.Conversation) ||
		rawJSONFieldPresent(responses.ContextManagement)
}

func rawJSONFieldPresent(raw []byte) bool {
	value := strings.TrimSpace(string(raw))
	return value != "" && value != "null"
}

func (a *Adaptor) openCodeGoStreamSettlementError(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	if a == nil || !a.requestUpstreamStream || info == nil {
		return nil
	}
	if openCodeGoCallerCancelled(c, nil) ||
		(info.StreamStatus != nil && info.StreamStatus.EndReason == relaycommon.StreamEndReasonClientGone) {
		return types.NewOpenAIError(
			context.Canceled,
			types.ErrorCodeBadResponse,
			499,
			types.ErrOptionWithSkipRetry(),
		)
	}
	if a.openCodeGoStreamHasLocalErrors(info) {
		return types.NewOpenAIError(
			errors.New("OpenCode Go stream terminated by a local relay failure"),
			types.ErrorCodeBadResponse,
			http.StatusInternalServerError,
			types.ErrOptionWithSkipRetry(),
		)
	}
	if a.openCodeGoStreamIncomplete(c, info) {
		return service.MarkOpenCodeGoUpstreamRelayError(types.NewOpenAIError(
			errors.New("OpenCode Go upstream stream ended before completion"),
			types.ErrorCodeBadResponseBody,
			http.StatusBadGateway,
			types.ErrOptionWithSkipRetry(),
		))
	}
	return nil
}

func (a *Adaptor) openCodeGoStreamIncomplete(c *gin.Context, info *relaycommon.RelayInfo) bool {
	if a == nil || !a.requestUpstreamStream || info == nil {
		return false
	}
	if openCodeGoCallerCancelled(c, nil) ||
		(info.StreamStatus != nil && info.StreamStatus.EndReason == relaycommon.StreamEndReasonClientGone) {
		return false
	}
	if info.StreamStatus == nil {
		return true
	}
	if a.protocol == ProtocolChat {
		return !info.StreamStatus.DoneSentinelObserved()
	}
	return !info.StreamStatus.ProtocolTerminalObserved()
}

func (a *Adaptor) openCodeGoStreamFailureReason(c *gin.Context, info *relaycommon.RelayInfo, responseErr *types.NewAPIError) string {
	if a == nil || a.failoverAttempt == nil || !a.requestUpstreamStream || info == nil ||
		openCodeGoCallerCancelled(c, responseErr) ||
		(info.StreamStatus != nil && info.StreamStatus.EndReason == relaycommon.StreamEndReasonClientGone) {
		return ""
	}
	if info.StreamStatus != nil && info.StreamStatus.UpstreamFailureObserved() {
		return "upstream_stream_error"
	}
	if a.openCodeGoStreamHasLocalErrors(info) {
		return ""
	}
	// A handler error without explicit upstream evidence is a local conversion or
	// client-write failure. The exception is an upstream stream that ended before
	// sending even one payload: some protocol handlers return a parse/finalize
	// error for that empty body, but EOF/timeout/scanner failure is still the
	// provider-side incomplete-stream evidence that failover is designed for.
	if responseErr != nil {
		if info.ReceivedResponseCount == 0 && openCodeGoStreamEndedUpstream(info.StreamStatus) {
			return "upstream_stream_incomplete"
		}
		return ""
	}
	if a.openCodeGoStreamIncomplete(c, info) {
		return "upstream_stream_incomplete"
	}
	return ""
}

func openCodeGoStreamEndedUpstream(status *relaycommon.StreamStatus) bool {
	if status == nil {
		return false
	}
	switch status.EndReason {
	case relaycommon.StreamEndReasonEOF,
		relaycommon.StreamEndReasonTimeout,
		relaycommon.StreamEndReasonScannerErr:
		return true
	default:
		return false
	}
}

func (a *Adaptor) openCodeGoStreamHasLocalErrors(info *relaycommon.RelayInfo) bool {
	if a == nil || info == nil || info.StreamStatus == nil {
		return false
	}
	if info.StreamStatus.UpstreamFailureObserved() {
		return false
	}
	// EndReason uses first-wins semantics. A terminal sentinel can therefore
	// be recorded before a later ping/handler/write failure; consult the
	// independent local-failure bit before treating the attempt as successful.
	if info.StreamStatus.LocalFailureObserved() {
		return true
	}
	switch info.StreamStatus.EndReason {
	case relaycommon.StreamEndReasonHandlerStop,
		relaycommon.StreamEndReasonPanic,
		relaycommon.StreamEndReasonPingFail:
		return true
	default:
		return info.StreamStatus.HasErrors()
	}
}

func (a *Adaptor) recordFailoverFailure(c *gin.Context, info *relaycommon.RelayInfo, reason string) {
	if a == nil || a.failoverAttempt == nil || info == nil || openCodeGoCallerCancelled(c, nil) {
		return
	}
	observation, err := observeOpenCodeGoFailoverFailure(a.failoverAttempt, openCodeGoHealthNow())
	if err != nil {
		logger.LogWarn(c, fmt.Sprintf(
			"OpenCode Go failover state update failed open: channel_id=%d model=%q protocol=%q error=%v",
			info.ChannelId,
			info.UpstreamModelName,
			a.protocol,
			err,
		))
		return
	}
	if observation.Action == service.OpenCodeGoFailoverActionNone ||
		observation.Action == service.OpenCodeGoFailoverActionStale {
		return
	}
	toRef := ""
	if observation.Action == service.OpenCodeGoFailoverActionPromoted {
		toRef = hashCacheIdentity("diagnostic-workspace", a.failoverAttempt.PreferredBackupWorkspaceUID())
	}
	logger.LogWarn(c, fmt.Sprintf(
		"OpenCode Go failover observation: channel_id=%d model=%q protocol=%q reason=%s action=%s failure_count=%d from_ref=%s to_ref=%s",
		info.ChannelId,
		info.UpstreamModelName,
		a.protocol,
		reason,
		observation.Action,
		observation.FailureCount,
		hashCacheIdentity("diagnostic-workspace", a.selectedWorkspaceUID),
		toRef,
	))
}

func (a *Adaptor) recordFailoverSuccess(c *gin.Context, info *relaycommon.RelayInfo) {
	if a == nil || a.failoverAttempt == nil || info == nil || openCodeGoCallerCancelled(c, nil) {
		return
	}
	if _, err := observeOpenCodeGoFailoverSuccess(a.failoverAttempt, openCodeGoHealthNow()); err != nil {
		logger.LogWarn(c, fmt.Sprintf(
			"OpenCode Go failover success update failed open: channel_id=%d model=%q protocol=%q error=%v",
			info.ChannelId,
			info.UpstreamModelName,
			a.protocol,
			err,
		))
	}
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return ChannelName
}
