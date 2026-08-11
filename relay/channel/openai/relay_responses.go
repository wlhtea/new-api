package openai

import (
	"fmt"
	"io"
	"net/http"
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
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
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
	strictOpenCodeGo := info != nil && info.ChannelType == constant.ChannelTypeOpenCodeGo

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
		if err := sendResponsesStreamData(c, streamResponse, data); err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
			sr.Stop(streamErr)
			return
		}
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
			if strictOpenCodeGo {
				sr.Stop(streamErr)
				return
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
				sr.Stop(streamErr)
				return
			}
		case "response.incomplete", "response.cancelled", "response.canceled":
			// `incomplete` commonly means a deterministic stop such as
			// max_output_tokens/content_filter; `cancelled` can be client-driven.
			// Surface both without rotating the workspace.
			if strictOpenCodeGo {
				streamErr = types.NewOpenAIError(fmt.Errorf("responses stream ended with %s", streamResponse.Type), types.ErrorCodeBadResponse, http.StatusInternalServerError)
				sr.Stop(streamErr)
				return
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
