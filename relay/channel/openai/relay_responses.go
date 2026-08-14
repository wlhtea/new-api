package openai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

// responsesErrorHasDetails distinguishes an absent Responses error object from
// the standard code-only shape. Some Responses providers omit error.type while
// still returning a useful error.code (for example invalid_prompt or
// server_error), so type alone cannot decide whether classification is safe.
func responsesErrorHasDetails(oaiErr *types.OpenAIError) bool {
	return oaiErr != nil && oaiErr.HasDetails()
}

func markTransientResponsesError(info *relaycommon.RelayInfo, oaiErr *types.OpenAIError, statusCode int) {
	if info == nil || info.StreamStatus == nil || !responsesErrorHasDetails(oaiErr) {
		return
	}
	if relaycommon.IsTransientProviderStreamError(
		oaiErr.Type,
		fmt.Sprintf("%v", oaiErr.Code),
		oaiErr.Message,
		statusCode,
	) {
		info.StreamStatus.MarkUpstreamFailure()
	}
}

func copyResponsesUsage(dst *dto.Usage, src *dto.Usage) {
	if dst == nil || src == nil {
		return
	}
	*dst = *src
	dst.BillingUsage = dto.CloneBillingUsage(src.BillingUsage)
	dst.PromptTokens = src.InputTokens
	dst.CompletionTokens = src.OutputTokens
	if src.InputTokensDetails != nil {
		details := *src.InputTokensDetails
		dst.PromptTokensDetails = details
		dst.InputTokensDetails = &details
	}
}

func OaiResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	// read response body
	var responsesResponse dto.OpenAIResponsesResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	err = common.Unmarshal(responseBody, &responsesResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if oaiError := responsesResponse.GetOpenAIError(); responsesErrorHasDetails(oaiError) {
		responseErr := types.WithOpenAIError(*oaiError, resp.StatusCode)
		if constant.IsOpenCodeChannelType(info.GetChannelType()) {
			responseErr = withOpenCodeResponsesRetryPolicy(c, info, responseErr)
			responseErr = service.MarkOpenCodeGoUpstreamRelayErrorWithStatus(responseErr, resp.StatusCode)
		}
		return nil, responseErr
	}
	if constant.IsOpenCodeChannelType(info.GetChannelType()) {
		if rawError, present := rawResponsesError(responsesResponse.Error); present {
			return nil, newRawOpenCodeGoResponsesError(c, info, rawError, resp.StatusCode)
		}
	}

	// 写入新的 response body
	service.IOCopyBytesGracefully(c, resp, responseBody)

	// compute usage
	usage := dto.Usage{}
	copyResponsesUsage(&usage, responsesResponse.Usage)
	// Count actual tool invocations from Output (not tool declarations).
	for _, output := range responsesResponse.Output {
		switch output.Type {
		case dto.BuildInCallWebSearchCall:
			info.CountBillableToolCall(dto.BuildInCallWebSearchCall, "")
		case dto.BuildInCallFileSearchCall:
			info.CountBillableToolCall(dto.BuildInCallFileSearchCall, "")
		case dto.BuildInCallFunctionCall:
			info.CountBillableToolCall(dto.BuildInCallFunctionCall, output.Name)
		}
	}

	imageCounter := &relaycommon.ImageGenerationCallCounter{}
	if !relaycommon.IsNonBillableResponsesStatus(responsesResponse.Status) {
		for i := range responsesResponse.Output {
			idx := i
			imageCounter.Observe(&responsesResponse.Output[i], &idx)
		}
	}
	imageCounter.Commit(info)

	return &usage, nil
}

func OaiResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		logger.LogError(c, "invalid response or response body")
		return nil, types.NewError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse)
	}

	defer service.CloseResponseBodyGracefully(resp)

	var usage = &dto.Usage{}
	var responseTextBuilder strings.Builder
	imageCounter := &relaycommon.ImageGenerationCallCounter{}
	imageCommitted := false
	var streamErr *types.NewAPIError
	strictOpenCodeGo := constant.IsOpenCodeChannelType(info.GetChannelType())

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		if streamErr != nil {
			sr.Stop(streamErr)
			return
		}
		// 检查当前数据是否包含 completed 状态和 usage 信息
		var streamResponse dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResponse); err != nil {
			logger.LogError(c, "failed to unmarshal stream response: "+err.Error())
			if info.StreamStatus != nil {
				info.StreamStatus.MarkUpstreamFailure()
			}
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
			sr.Stop(streamErr)
			return
		}
		errorPayload := responsesErrorPayload(streamResponse.Type, data)
		privateTerminalPayload := responsesPrivateTerminalPayload(streamResponse.Type, data)
		switch streamResponse.Type {
		case "response.completed", "response.done":
			if info.StreamStatus != nil {
				info.StreamStatus.MarkProtocolTerminal()
			}
			if streamResponse.Response != nil {
				copyResponsesUsage(usage, streamResponse.Response.Usage)
				if !imageCommitted {
					if relaycommon.IsNonBillableResponsesStatus(streamResponse.Response.Status) {
						imageCounter.Reset()
						imageCounter.Commit(info)
						imageCommitted = true
					} else {
						for i := range streamResponse.Response.Output {
							idx := i
							imageCounter.Observe(&streamResponse.Response.Output[i], &idx)
						}
						imageCounter.Commit(info)
						imageCommitted = true
					}
				}
			} else if !imageCommitted {
				imageCounter.Commit(info)
				imageCommitted = true
			}
		case "response.error":
			if streamResponse.Response != nil {
				if oaiErr := streamResponse.Response.GetOpenAIError(); responsesErrorHasDetails(oaiErr) {
					markTransientResponsesError(info, oaiErr, resp.StatusCode)
					streamErr = types.WithOpenAIError(*oaiErr, resp.StatusCode)
				} else {
					streamErr = types.NewOpenAIError(fmt.Errorf("responses stream error: %s", streamResponse.Type), types.ErrorCodeBadResponse, http.StatusInternalServerError)
				}
			} else {
				streamErr = types.NewOpenAIError(fmt.Errorf("responses stream error: %s", streamResponse.Type), types.ErrorCodeBadResponse, http.StatusInternalServerError)
			}
		case "response.failed":
			if strictOpenCodeGo {
				if streamResponse.Response != nil {
					if oaiErr := streamResponse.Response.GetOpenAIError(); responsesErrorHasDetails(oaiErr) {
						markTransientResponsesError(info, oaiErr, resp.StatusCode)
						streamErr = types.WithOpenAIError(*oaiErr, resp.StatusCode)
					} else {
						if info.StreamStatus != nil {
							info.StreamStatus.MarkUpstreamFailure()
						}
						streamErr = types.NewOpenAIError(fmt.Errorf("responses stream ended with %s", streamResponse.Type), types.ErrorCodeBadResponse, http.StatusInternalServerError)
					}
				} else {
					if info.StreamStatus != nil {
						info.StreamStatus.MarkUpstreamFailure()
					}
					streamErr = types.NewOpenAIError(fmt.Errorf("responses stream ended with %s", streamResponse.Type), types.ErrorCodeBadResponse, http.StatusInternalServerError)
				}
			}
		case "response.incomplete", "response.cancelled", "response.canceled":
			// `incomplete` commonly means a deterministic stop such as
			// max_output_tokens/content_filter; a provider-reported `cancelled`
			// is not the same as this downstream request being cancelled.
			if strictOpenCodeGo {
				streamErr = newOpenCodeResponsesTerminalError(c, info, streamResponse.Type, resp.StatusCode)
			}
			if !imageCommitted {
				imageCounter.Reset()
				imageCounter.Commit(info)
				imageCommitted = true
			}
		case "response.output_text.delta":
			// 处理输出文本
			responseTextBuilder.WriteString(streamResponse.Delta)
		case dto.ResponsesOutputTypeItemDone:
			if streamResponse.Item != nil {
				switch streamResponse.Item.Type {
				case dto.BuildInCallWebSearchCall:
					info.CountBillableToolCall(dto.BuildInCallWebSearchCall, "")
				case dto.BuildInCallFileSearchCall:
					info.CountBillableToolCall(dto.BuildInCallFileSearchCall, "")
				case dto.BuildInCallFunctionCall:
					info.CountBillableToolCall(dto.BuildInCallFunctionCall, streamResponse.Item.Name)
				case dto.ResponsesOutputTypeImageGenerationCall:
					if !imageCommitted {
						imageCounter.Observe(streamResponse.Item, streamResponse.OutputIndex)
					}
				}
			}
		}
		if strictOpenCodeGo && len(errorPayload) > 0 {
			streamErr = newRawOpenCodeGoResponsesError(c, info, errorPayload, resp.StatusCode)
			sr.Stop(streamErr)
			return
		}
		if strictOpenCodeGo && responsesExplicitErrorEvent(streamResponse.Type) {
			if streamErr == nil {
				streamErr = types.NewOpenAIError(
					fmt.Errorf("responses stream ended with %s", streamResponse.Type),
					types.ErrorCodeBadResponse,
					http.StatusInternalServerError,
				)
			}
			streamErr = service.MarkOpenCodeGoUpstreamRelayErrorWithStatus(
				withOpenCodeResponsesRetryPolicy(c, info, streamErr),
				resp.StatusCode,
			)
			sr.Stop(streamErr)
			return
		}
		if strictOpenCodeGo && len(privateTerminalPayload) > 0 &&
			service.OpenCodeGoErrorHasPrivateDetail(string(privateTerminalPayload)) {
			streamErr = newRawOpenCodeGoResponsesError(c, info, privateTerminalPayload, resp.StatusCode)
			sr.Stop(streamErr)
			return
		}
		if err := sendResponsesStreamData(c, streamResponse, data); err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
			sr.Stop(streamErr)
			return
		}
		if streamErr != nil {
			sr.Stop(streamErr)
		}
	})
	if streamErr != nil {
		return nil, streamErr
	}

	if usage.CompletionTokens == 0 {
		// 计算输出文本的 token 数量
		tempStr := responseTextBuilder.String()
		if len(tempStr) > 0 {
			// 非正常结束，使用输出文本的 token 数量
			completionTokens := service.CountTextToken(tempStr, info.UpstreamModelName)
			usage.CompletionTokens = completionTokens
		}
	}

	if usage.PromptTokens == 0 && usage.CompletionTokens != 0 {
		usage.PromptTokens = info.GetEstimatePromptTokens()
	}

	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens

	return usage, nil
}

func newRawOpenCodeGoResponsesError(c *gin.Context, info *relaycommon.RelayInfo, payload []byte, statusCode int) *types.NewAPIError {
	if statusCode < 100 || statusCode > 599 {
		statusCode = http.StatusInternalServerError
	}
	payload = canonicalResponsesErrorPayload(payload)
	openAIError := types.OpenAIError{}
	if common.Unmarshal(payload, &openAIError) != nil || !openAIError.HasDetails() {
		openAIError = types.OpenAIError{
			Message: string(payload),
			Type:    "upstream_error",
			Code:    "upstream_error",
		}
	} else {
		if strings.TrimSpace(openAIError.Message) == "" {
			openAIError.Message = string(payload)
		}
		if strings.TrimSpace(openAIError.Type) == "" {
			openAIError.Type = "upstream_error"
		}
		if openAIError.Code == nil || strings.TrimSpace(fmt.Sprint(openAIError.Code)) == "" {
			openAIError.Code = "upstream_error"
		}
	}
	return service.MarkOpenCodeGoUpstreamRelayErrorWithStatus(
		withOpenCodeResponsesRetryPolicy(c, info, types.WithOpenAIError(openAIError, statusCode)),
		statusCode,
	)
}

func newOpenCodeResponsesTerminalError(c *gin.Context, info *relaycommon.RelayInfo, eventType string, upstreamStatusCode int) *types.NewAPIError {
	relayErr := types.NewOpenAIError(
		fmt.Errorf("responses stream ended with %s", eventType),
		types.ErrorCodeBadResponse,
		http.StatusBadGateway,
	)
	return service.MarkOpenCodeGoUpstreamRelayErrorWithStatus(
		withOpenCodeResponsesRetryPolicy(c, info, relayErr),
		upstreamStatusCode,
	)
}

func withOpenCodeResponsesRetryPolicy(c *gin.Context, info *relaycommon.RelayInfo, relayErr *types.NewAPIError) *types.NewAPIError {
	if relayErr == nil {
		return relayErr
	}
	if info != nil && info.GetChannelType() != constant.ChannelTypeOpenCodeGo &&
		(c == nil || c.Writer == nil || !c.Writer.Written()) {
		return relayErr
	}
	return types.NewError(relayErr, relayErr.GetErrorCode(), types.ErrOptionWithSkipRetry())
}

func rawResponsesError(errorField any) (json.RawMessage, bool) {
	if errorField == nil {
		return nil, false
	}
	raw, err := common.Marshal(errorField)
	if err != nil {
		return nil, false
	}
	return raw, responsesRawPayloadPresent(raw)
}

func responsesRawPayloadPresent(payload json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" || trimmed == "null" {
		return false
	}
	var decoded any
	if common.Unmarshal(payload, &decoded) != nil {
		return true
	}
	switch typed := decoded.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case map[string]any:
		return len(typed) != 0
	case []any:
		return len(typed) != 0
	default:
		return true
	}
}

func responsesExplicitErrorEvent(eventType string) bool {
	switch eventType {
	case "error", "response.error", "response.failed":
		return true
	default:
		return false
	}
}

func canonicalResponsesErrorPayload(payload []byte) []byte {
	var decoded any
	if common.Unmarshal(payload, &decoded) != nil {
		return payload
	}
	canonical, err := common.Marshal(decoded)
	if err != nil {
		return payload
	}
	return canonical
}

func responsesErrorPayload(eventType, data string) json.RawMessage {
	var envelope struct {
		Type     string          `json:"type"`
		Error    json.RawMessage `json:"error"`
		Message  json.RawMessage `json:"message"`
		Code     json.RawMessage `json:"code"`
		Detail   json.RawMessage `json:"detail"`
		Metadata json.RawMessage `json:"metadata"`
		Response *struct {
			Error             json.RawMessage `json:"error"`
			IncompleteDetails json.RawMessage `json:"incomplete_details"`
		} `json:"response"`
	}
	if common.UnmarshalJsonStr(data, &envelope) != nil {
		return nil
	}

	candidates := make([]json.RawMessage, 0, 3)
	switch eventType {
	case "error", "response.error", "response.failed":
		candidates = append(candidates, envelope.Error)
		if envelope.Response != nil {
			candidates = append(candidates, envelope.Response.Error)
		}
		if !responsesRawPayloadPresent(envelope.Error) &&
			(envelope.Response == nil || !responsesRawPayloadPresent(envelope.Response.Error)) {
			topLevel := responsesTopLevelErrorPayload(envelope.Type, envelope.Message, envelope.Code, envelope.Detail, envelope.Metadata)
			if len(topLevel) > 0 {
				candidates = append(candidates, topLevel)
			}
		}
	case "response.incomplete", "response.cancelled", "response.canceled":
		candidates = append(candidates, envelope.Error)
		if envelope.Response != nil {
			candidates = append(candidates, envelope.Response.Error)
		}
	case "":
		candidates = append(candidates, envelope.Error)
	default:
		return nil
	}

	for _, candidate := range candidates {
		if responsesRawPayloadPresent(candidate) {
			return candidate
		}
	}
	return nil
}

func responsesPrivateTerminalPayload(eventType, data string) json.RawMessage {
	switch eventType {
	case "response.incomplete", "response.cancelled", "response.canceled":
	default:
		return nil
	}
	var envelope struct {
		Type     string          `json:"type"`
		Message  json.RawMessage `json:"message"`
		Code     json.RawMessage `json:"code"`
		Detail   json.RawMessage `json:"detail"`
		Metadata json.RawMessage `json:"metadata"`
		Response *struct {
			IncompleteDetails json.RawMessage `json:"incomplete_details"`
		} `json:"response"`
	}
	if common.UnmarshalJsonStr(data, &envelope) != nil {
		return nil
	}
	if envelope.Response != nil {
		trimmed := strings.TrimSpace(string(envelope.Response.IncompleteDetails))
		if trimmed != "" && trimmed != "null" {
			return envelope.Response.IncompleteDetails
		}
	}
	return responsesTopLevelErrorPayload(
		envelope.Type,
		envelope.Message,
		envelope.Code,
		envelope.Detail,
		envelope.Metadata,
	)
}

func responsesTopLevelErrorPayload(eventType string, fields ...json.RawMessage) json.RawMessage {
	payload := map[string]json.RawMessage{"type": json.RawMessage(strconv.Quote(eventType))}
	names := []string{"message", "code", "detail", "metadata"}
	for index, field := range fields {
		trimmed := strings.TrimSpace(string(field))
		if trimmed != "" && trimmed != "null" {
			payload[names[index]] = field
		}
	}
	if len(payload) == 1 {
		return nil
	}
	encoded, err := common.Marshal(payload)
	if err != nil {
		return nil
	}
	return encoded
}
