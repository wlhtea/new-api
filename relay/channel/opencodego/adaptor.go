package opencodego

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
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
	bufferClaudeToolCall  bool
	requestInputItems     int
	requestToolCount      int
	requestUpstreamStream bool
	converted             bool
	workspaceSelected     bool
	selectedWorkspaceUID  string
	openai                openai.Adaptor
	claude                claude.Adaptor
}

var selectOpenCodeGoWorkspace = service.SelectOpenCodeGoWorkspaceWithAffinity

var doOpenCodeGoAPIRequest = channel.DoApiRequest

var observeOpenCodeGoTransportFailure = service.ObserveOpenCodeGoTransportFailure

var openCodeGoHealthNow = time.Now

func (a *Adaptor) Init(info *relaycommon.RelayInfo) {
	a.protocol = ""
	a.protocolErr = nil
	a.cacheIdentity = ""
	a.affinityIdentity = ""
	a.bufferClaudeToolCall = false
	a.requestInputItems = 0
	a.requestToolCount = 0
	a.requestUpstreamStream = false
	a.converted = false
	a.workspaceSelected = false
	a.selectedWorkspaceUID = ""
	if info == nil {
		return
	}
	a.openai.Init(info)
	a.claude.Init(info)
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
		selection, selectErr := selectOpenCodeGoWorkspace(info.ChannelId, info.UpstreamModelName, a.affinityIdentity)
		if selectErr != nil {
			return selectErr
		}
		info.ApiKey = selection.APIKey
		a.selectedWorkspaceUID = selection.WorkspaceUID
		a.workspaceSelected = true
		common.SetContextKey(c, constant.ContextKeyOpenCodeGoWorkspaceUID, selection.WorkspaceUID)
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
	if request == nil {
		return nil, errors.New("OpenCode Go request is nil")
	}
	protocol, err := a.resolveProtocol(info)
	if err != nil {
		return nil, err
	}
	usesFunctionTools := requestUsesFunctionTools(request)
	if protocol == ProtocolChat && usesFunctionTools && !requestContainsAssistantReasoning(request) {
		protocol = ProtocolResponses
		a.protocol = protocol
	}

	a.cacheIdentity = cacheIdentityForRequest(c, info, request)
	a.affinityIdentity = affinityIdentityForRequest(c, request)
	// Console Go's streaming Responses result historically omitted complete
	// function-argument deltas. Buffer any streaming Claude function-tool turn
	// (Chat-family reasoning continuation or Responses-routed first turn) into
	// one non-streaming upstream request and synthesize Claude SSE without
	// retrying or switching accounts.
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
	if err != nil && a.workspaceSelected && !openCodeGoCallerCancelled(c, err) && info != nil {
		reason := sanitizeErrorMessage(err.Error())
		if _, observeErr := observeOpenCodeGoTransportFailure(
			info.ChannelId,
			a.selectedWorkspaceUID,
			info.UpstreamModelName,
			reason,
			openCodeGoHealthNow(),
		); observeErr != nil {
			common.SysError(fmt.Sprintf(
				"failed to persist OpenCode Go transport health observation: channel_id=%d workspace_uid=%s error=%v",
				info.ChannelId,
				a.selectedWorkspaceUID,
				observeErr,
			))
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

	clientModel := info.OriginModelName
	if clientModel == "" {
		clientModel = info.UpstreamModelName
	}
	state := &responseTransformState{model: clientModel, protocol: protocol}
	if err := prepareResponseForRelay(resp, state, info.IsStream && !a.bufferClaudeToolCall); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError, types.ErrOptionWithSkipRetry())
	}

	upstreamModel := info.UpstreamModelName
	info.FinalRequestRelayFormat = protocol.RelayFormat()
	info.UpstreamModelName = clientModel
	defer func() {
		info.UpstreamModelName = upstreamModel
	}()

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
	if responseErr != nil {
		return nil, responseErr
	}
	return finalizeResponseUsage(usage, state), nil
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return ChannelName
}
