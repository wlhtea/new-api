package opencodego

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func messagesToResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, markOpenCodeGoUpstreamResponseError(types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusBadGateway))
	}
	defer service.CloseResponseBodyGracefully(resp)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, markOpenCodeGoUpstreamResponseError(types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusBadGateway))
	}
	var claudeResponse dto.ClaudeResponse
	if err := common.Unmarshal(body, &claudeResponse); err != nil {
		return nil, markOpenCodeGoUpstreamResponseError(types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway))
	}
	if claudeError := claudeResponse.GetClaudeError(); claudeError != nil && claudeError.Type != "" {
		return nil, markOpenCodeGoUpstreamResponseError(types.WithClaudeError(*claudeError, resp.StatusCode))
	}

	result, err := relayconvert.ConvertResponse(c, info, types.RelayFormatOpenAIResponses, &claudeResponse)
	if err != nil {
		return nil, markOpenCodeGoUpstreamResponseError(types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway))
	}
	response, ok := result.Value.(*dto.OpenAIResponsesResponse)
	if !ok {
		return nil, markOpenCodeGoUpstreamResponseError(types.NewOpenAIError(fmt.Errorf("expected OpenAI responses response, got %T", result.Value), types.ErrorCodeBadResponseBody, http.StatusBadGateway))
	}
	usage := result.Usage
	if !hasMessagesToResponsesUsage(usage) {
		usage = service.ResponseText2Usage(c, service.ExtractOutputTextFromResponses(response), info.UpstreamModelName, info.GetEstimatePromptTokens())
		response.Usage = relayconvert.UsageFromChatUsage(usage)
	}
	encoded, err := common.Marshal(response)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}
	service.IOCopyBytesGracefully(c, resp, encoded)
	return usage, nil
}

func messagesToResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, markOpenCodeGoUpstreamResponseError(types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusBadGateway))
	}
	state, err := relayconvert.NewResponseStreamState(types.RelayFormatClaude, types.RelayFormatOpenAIResponses, relayconvert.ResponseStreamOptions{
		ID:      helper.GetResponseID(c),
		Model:   info.UpstreamModelName,
		Created: time.Now().Unix(),
	})
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	claudeInfo := &relayconvert.ClaudeResponseInfo{
		ResponseId:   helper.GetResponseID(c),
		Created:      time.Now().Unix(),
		Model:        info.UpstreamModelName,
		ResponseText: strings.Builder{},
		Usage:        &dto.Usage{},
	}
	var streamErr *types.NewAPIError

	sendResult := func(result relayconvert.ResponseResult) bool {
		event, ok := result.Value.(relayconvert.ChatToResponsesStreamEvent)
		if !ok {
			streamErr = types.NewOpenAIError(fmt.Errorf("expected OpenAI responses stream event, got %T", result.Value), types.ErrorCodeBadResponse, http.StatusInternalServerError)
			return false
		}
		payload, err := common.Marshal(event.Payload)
		if err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
			return false
		}
		if err := helper.ResponseChunkData(c, dto.ResponsesStreamResponse{Type: event.Type}, string(payload)); err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
			return false
		}
		return true
	}

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		if streamErr != nil {
			sr.Stop(streamErr)
			return
		}
		var claudeResponse dto.ClaudeResponse
		if err := common.UnmarshalJsonStr(data, &claudeResponse); err != nil {
			if info.StreamStatus != nil {
				info.StreamStatus.MarkUpstreamFailure()
			}
			streamErr = markOpenCodeGoUpstreamResponseError(types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway))
			sr.Stop(streamErr)
			return
		}
		if claudeError := claudeResponse.GetClaudeError(); claudeError != nil && claudeError.Type != "" {
			if info.StreamStatus != nil && relaycommon.IsTransientProviderStreamError(claudeError.Type, "", claudeError.Message, resp.StatusCode) {
				info.StreamStatus.MarkUpstreamFailure()
			}
			streamErr = markOpenCodeGoUpstreamResponseError(types.WithClaudeError(*claudeError, resp.StatusCode))
			sr.Stop(streamErr)
			return
		}
		if claudeResponse.Type == "message_stop" && info.StreamStatus != nil {
			info.StreamStatus.MarkProtocolTerminal()
		}
		relayconvert.FormatClaudeResponseInfo(&claudeResponse, nil, claudeInfo)
		if claudeResponse.Type == "message_delta" {
			claudeResponse.Usage = relayconvert.BuildMessageDeltaPatchUsage(&claudeResponse, claudeInfo)
		}
		results, err := relayconvert.ConvertStreamResponseChunk(c, info, state, &claudeResponse)
		if err != nil {
			streamErr = markOpenCodeGoUpstreamResponseError(types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway))
			sr.Stop(streamErr)
			return
		}
		for _, result := range results {
			if !sendResult(result) {
				sr.Stop(streamErr)
				return
			}
		}
	})
	if streamErr != nil {
		return nil, streamErr
	}

	if claudeInfo.Usage != nil {
		state.SetUsage(relayconvert.UsageFromClaudeUsage(claudeInfo.Usage))
	}
	usage := state.Usage()
	if !hasMessagesToResponsesUsage(usage) {
		usage = service.ResponseText2Usage(c, state.UsageText(), info.UpstreamModelName, info.GetEstimatePromptTokens())
		state.SetUsage(usage)
	}
	finalResults, err := relayconvert.FinalizeStreamResponse(c, info, state)
	if err != nil {
		if info.StreamStatus != nil {
			info.StreamStatus.MarkLocalFailure()
		}
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	for _, result := range finalResults {
		if !sendResult(result) {
			if info.StreamStatus != nil {
				info.StreamStatus.MarkLocalFailure()
			}
			return nil, streamErr
		}
	}
	return usage, nil
}

func markOpenCodeGoUpstreamResponseError(err *types.NewAPIError) *types.NewAPIError {
	return service.MarkOpenCodeGoUpstreamRelayError(err)
}

func hasMessagesToResponsesUsage(usage *dto.Usage) bool {
	if usage == nil {
		return false
	}
	return usage.BillingUsage != nil || dto.HasOpenAIUsageTokens(usage)
}
