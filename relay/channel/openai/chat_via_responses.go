package openai

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

func OaiResponsesToChatHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	defer service.CloseResponseBodyGracefully(resp)

	var responsesResp dto.OpenAIResponsesResponse
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}

	if err := common.Unmarshal(body, &responsesResp); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	if oaiError := responsesResp.GetOpenAIError(); responsesErrorHasDetails(oaiError) {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	chatResult, err := relayconvert.ConvertResponse(c, info, types.RelayFormatOpenAI, &responsesResp)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	chatResp, ok := chatResult.Value.(*dto.OpenAITextResponse)
	if !ok {
		return nil, types.NewOpenAIError(fmt.Errorf("expected OpenAI chat response, got %T", chatResult.Value), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if chatID := helper.GetResponseID(c); chatID != "" {
		chatResp.Id = chatID
	}
	usage := chatResult.Usage

	if usage == nil || usage.TotalTokens == 0 {
		text := service.ExtractOutputTextFromResponses(&responsesResp)
		usage = service.ResponseText2Usage(c, text, info.UpstreamModelName, info.GetEstimatePromptTokens())
		chatResp.Usage = *usage
	}

	responseValue := any(chatResp)
	if info.RelayFormat != types.RelayFormatOpenAI {
		targetResult, err := relayconvert.ConvertResponse(c, info, info.RelayFormat, chatResp)
		if err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		responseValue = targetResult.Value
	}
	responseBody, err := common.Marshal(responseValue)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}

	service.IOCopyBytesGracefully(c, resp, responseBody)
	return usage, nil
}

// OaiChatToClaudeBufferedStreamHandler mirrors
// OaiResponsesToClaudeBufferedStreamHandler but consumes a non-streaming
// OpenAI Chat response. It exists for Chat-family reasoning models that must
// stay on Chat protocol to preserve assistant reasoning_content across turns
// (Console Go drops standalone `reasoning` Responses items). The upstream
// request is non-streaming; this handler emits the complete result as a valid
// Claude SSE sequence without retrying or selecting another account.
func OaiChatToClaudeBufferedStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	defer service.CloseResponseBodyGracefully(resp)
	ensureBufferedStreamStatus(info)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}

	var chatResp dto.OpenAITextResponse
	if err := common.Unmarshal(body, &chatResp); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if oaiError := chatResp.GetOpenAIError(); responsesErrorHasDetails(oaiError) {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	result, err := relayconvert.ConvertResponse(c, info, types.RelayFormatClaude, &chatResp)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	claudeResp, ok := result.Value.(*dto.ClaudeResponse)
	if !ok || claudeResp == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("expected Claude response, got %T", result.Value), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	usage := result.Usage
	if usage == nil || usage.TotalTokens == 0 {
		text := ""
		if len(chatResp.Choices) > 0 && chatResp.Choices[0].Message.StringContent() != "" {
			text = chatResp.Choices[0].Message.StringContent()
		}
		usage = service.ResponseText2Usage(c, text, info.UpstreamModelName, info.GetEstimatePromptTokens())
	}

	helper.SetEventStreamHeaders(c)
	if err := emitBufferedClaudeResponse(c, claudeResp); err != nil {
		if info.StreamStatus != nil {
			info.StreamStatus.MarkLocalFailure()
		}
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	if info.StreamStatus != nil {
		info.StreamStatus.MarkProtocolTerminal()
	}
	return usage, nil
}

// OaiResponsesToClaudeBufferedStreamHandler works around Responses providers
// that omit function arguments only in streaming mode. The upstream request is
// non-streaming; this handler emits the complete result as a valid Claude SSE
// sequence without retrying or selecting another account.
func OaiResponsesToClaudeBufferedStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	defer service.CloseResponseBodyGracefully(resp)
	ensureBufferedStreamStatus(info)

	var responsesResp dto.OpenAIResponsesResponse
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	if err := common.Unmarshal(body, &responsesResp); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if oaiError := responsesResp.GetOpenAIError(); responsesErrorHasDetails(oaiError) {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	result, err := relayconvert.ConvertResponse(c, info, types.RelayFormatClaude, &responsesResp)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	claudeResp, ok := result.Value.(*dto.ClaudeResponse)
	if !ok || claudeResp == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("expected Claude response, got %T", result.Value), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	usage := result.Usage
	if usage == nil || usage.TotalTokens == 0 {
		text := service.ExtractOutputTextFromResponses(&responsesResp)
		usage = service.ResponseText2Usage(c, text, info.UpstreamModelName, info.GetEstimatePromptTokens())
	}

	helper.SetEventStreamHeaders(c)
	if err := emitBufferedClaudeResponse(c, claudeResp); err != nil {
		if info.StreamStatus != nil {
			info.StreamStatus.MarkLocalFailure()
		}
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	if info.StreamStatus != nil {
		info.StreamStatus.MarkProtocolTerminal()
	}
	return usage, nil
}

func ensureBufferedStreamStatus(info *relaycommon.RelayInfo) {
	if info == nil {
		return
	}
	if info.StreamStatus == nil {
		info.StreamStatus = relaycommon.NewStreamStatus()
	}
	if info.StreamProtocolTerminalRequired {
		info.StreamStatus.RequireProtocolTerminal()
	}
}

func emitBufferedClaudeResponse(c *gin.Context, response *dto.ClaudeResponse) error {
	if response == nil {
		return nil
	}
	emit := func(event dto.ClaudeResponse) error {
		return helper.ClaudeData(c, event)
	}
	startUsage := cloneClaudeUsage(response.Usage)
	if startUsage != nil {
		startUsage.OutputTokens = 0
	}
	message := &dto.ClaudeMediaMessage{
		Id:    response.Id,
		Type:  "message",
		Role:  "assistant",
		Model: response.Model,
		Usage: startUsage,
	}
	message.SetContent(make([]any, 0))
	if err := emit(dto.ClaudeResponse{Type: "message_start", Message: message}); err != nil {
		return err
	}

	for index, block := range response.Content {
		startBlock := block
		switch block.Type {
		case "tool_use":
			startBlock.Input = map[string]any{}
		case "text":
			startBlock.SetText("")
		case "thinking":
			startBlock.Thinking = kitutil.GetPointer("")
		}
		if err := emit(dto.ClaudeResponse{
			Type:         "content_block_start",
			Index:        kitutil.GetPointer(index),
			ContentBlock: &startBlock,
		}); err != nil {
			return err
		}

		switch block.Type {
		case "tool_use":
			partialJSON, err := common.Marshal(block.Input)
			if err == nil {
				partial := string(partialJSON)
				if err := emit(dto.ClaudeResponse{
					Type:  "content_block_delta",
					Index: kitutil.GetPointer(index),
					Delta: &dto.ClaudeMediaMessage{Type: "input_json_delta", PartialJson: &partial},
				}); err != nil {
					return err
				}
			}
		case "text":
			if text := block.GetText(); text != "" {
				if err := emit(dto.ClaudeResponse{
					Type:  "content_block_delta",
					Index: kitutil.GetPointer(index),
					Delta: &dto.ClaudeMediaMessage{Type: "text_delta", Text: &text},
				}); err != nil {
					return err
				}
			}
		case "thinking":
			if block.Thinking != nil && *block.Thinking != "" {
				thinking := *block.Thinking
				if err := emit(dto.ClaudeResponse{
					Type:  "content_block_delta",
					Index: kitutil.GetPointer(index),
					Delta: &dto.ClaudeMediaMessage{Type: "thinking_delta", Thinking: &thinking},
				}); err != nil {
					return err
				}
			}
		}
		if err := emit(dto.ClaudeResponse{Type: "content_block_stop", Index: kitutil.GetPointer(index)}); err != nil {
			return err
		}
	}

	outputUsage := &dto.ClaudeUsage{}
	if response.Usage != nil {
		outputUsage.OutputTokens = response.Usage.OutputTokens
	}
	stopReason := response.StopReason
	if err := emit(dto.ClaudeResponse{
		Type:  "message_delta",
		Delta: &dto.ClaudeMediaMessage{StopReason: &stopReason},
		Usage: outputUsage,
	}); err != nil {
		return err
	}
	return emit(dto.ClaudeResponse{Type: "message_stop"})
}

func cloneClaudeUsage(usage *dto.ClaudeUsage) *dto.ClaudeUsage {
	if usage == nil {
		return nil
	}
	cloned := *usage
	if usage.CacheCreation != nil {
		cacheCreation := *usage.CacheCreation
		cloned.CacheCreation = &cacheCreation
	}
	cloned.BillingUsage = dto.CloneBillingUsage(usage.BillingUsage)
	return &cloned
}

func OaiResponsesToChatBufferedStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	defer service.CloseResponseBodyGracefully(resp)

	accumulator := relayconvert.NewResponsesBufferedAccumulator()
	var finalResponse *dto.OpenAIResponsesResponse
	var streamErr *types.NewAPIError
	strictOpenCodeGo := info != nil && info.ChannelType == constant.ChannelTypeOpenCodeGo
	ensureBufferedStreamStatus(info)
	if strictOpenCodeGo && info.StreamStatus != nil {
		info.StreamStatus.RequireProtocolTerminal()
	}

	scanner := helper.NewStreamScanner(resp.Body)
	scanner.Split(bufio.ScanLines)
	for scanner.Scan() {
		rawLine := strings.TrimSpace(scanner.Text())
		var data string
		switch {
		case strings.HasPrefix(rawLine, "data:"):
			data = strings.TrimSpace(rawLine[len("data:"):])
		case rawLine == "[DONE]":
			data = rawLine
		default:
			continue
		}
		if data == "" || data == "[DONE]" {
			if data == "[DONE]" {
				break
			}
			continue
		}

		var streamResp dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResp); err != nil {
			logger.LogError(c, "failed to unmarshal buffered responses stream event: "+err.Error())
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
			break
		}
		accumulator.ProcessEvent(&streamResp)
		switch streamResp.Type {
		case "response.completed", "response.done":
			if info.StreamStatus != nil {
				info.StreamStatus.MarkProtocolTerminal()
			}
			finalResponse = streamResp.Response
		case "response.incomplete":
			if strictOpenCodeGo {
				streamErr = types.NewOpenAIError(fmt.Errorf("responses stream ended with %s", streamResp.Type), types.ErrorCodeBadResponse, http.StatusInternalServerError)
			} else {
				finalResponse = streamResp.Response
				if finalResponse == nil {
					finalResponse = &dto.OpenAIResponsesResponse{}
				}
				if len(finalResponse.Status) == 0 {
					finalResponse.Status = []byte(`"incomplete"`)
				}
			}
		case "response.cancelled", "response.canceled":
			if strictOpenCodeGo {
				streamErr = types.NewOpenAIError(fmt.Errorf("responses stream ended with %s", streamResp.Type), types.ErrorCodeBadResponse, http.StatusInternalServerError)
			}
		case "response.failed", "response.error":
			if streamResp.Response != nil {
				if oaiErr := streamResp.Response.GetOpenAIError(); responsesErrorHasDetails(oaiErr) {
					markTransientResponsesError(info, oaiErr, resp.StatusCode)
					streamErr = types.WithOpenAIError(*oaiErr, http.StatusInternalServerError)
					break
				}
			}
			if info.StreamStatus != nil && streamResp.Type == "response.failed" {
				info.StreamStatus.MarkUpstreamFailure()
			}
			streamErr = types.NewOpenAIError(fmt.Errorf("responses stream error: %s", streamResp.Type), types.ErrorCodeBadResponse, http.StatusInternalServerError)
		}
		if streamErr != nil || finalResponse != nil {
			break
		}
	}
	if streamErr != nil {
		return nil, streamErr
	}
	if err := scanner.Err(); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	if strictOpenCodeGo && finalResponse == nil {
		if info != nil && info.StreamStatus != nil {
			info.StreamStatus.MarkUpstreamFailure()
		}
		return nil, types.NewOpenAIError(fmt.Errorf("responses stream ended without a terminal event"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	if finalResponse == nil {
		finalResponse = &dto.OpenAIResponsesResponse{
			ID:        helper.GetResponseID(c),
			CreatedAt: int(time.Now().Unix()),
			Model:     info.UpstreamModelName,
			Status:    []byte(`"completed"`),
		}
	}
	accumulator.SupplementResponseOutput(finalResponse)

	chatResult, err := relayconvert.ConvertResponse(c, info, types.RelayFormatOpenAI, finalResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	chatResp, ok := chatResult.Value.(*dto.OpenAITextResponse)
	if !ok {
		return nil, types.NewOpenAIError(fmt.Errorf("expected OpenAI chat response, got %T", chatResult.Value), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if chatID := helper.GetResponseID(c); chatID != "" {
		chatResp.Id = chatID
	}
	usage := chatResult.Usage
	if usage == nil || usage.TotalTokens == 0 {
		text := service.ExtractOutputTextFromResponses(finalResponse)
		usage = service.ResponseText2Usage(c, text, info.UpstreamModelName, info.GetEstimatePromptTokens())
		chatResp.Usage = *usage
	}

	responseValue := any(chatResp)
	if info.RelayFormat != types.RelayFormatOpenAI {
		targetResult, err := relayconvert.ConvertResponse(c, info, info.RelayFormat, chatResp)
		if err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		responseValue = targetResult.Value
	}
	responseBody, err := common.Marshal(responseValue)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}

	service.IOCopyBytesGracefully(c, resp, responseBody)
	return usage, nil
}

func OaiResponsesToChatStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	defer service.CloseResponseBodyGracefully(resp)

	responseId := helper.GetResponseID(c)
	createAt := time.Now().Unix()
	state, err := relayconvert.NewResponseStreamState(types.RelayFormatOpenAIResponses, info.RelayFormat, relayconvert.ResponseStreamOptions{
		ID:      responseId,
		Model:   info.UpstreamModelName,
		Created: createAt,
	})
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	streamErr := (*types.NewAPIError)(nil)
	strictOpenCodeGo := info != nil && info.ChannelType == constant.ChannelTypeOpenCodeGo

	if info.RelayFormat == types.RelayFormatClaude && info.ClaudeConvertInfo == nil {
		info.ClaudeConvertInfo = &relaycommon.ClaudeConvertInfo{LastMessagesType: relaycommon.LastMessageTypeNone}
	}

	sendGeminiResponse := func(geminiResponse *dto.GeminiChatResponse) bool {
		if geminiResponse == nil {
			return true
		}
		geminiResponseStr, err := common.Marshal(geminiResponse)
		if err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
			return false
		}
		if err := helper.StringData(c, string(geminiResponseStr)); err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
			return false
		}
		return true
	}

	sendStreamResult := func(result relayconvert.ResponseResult) bool {
		switch value := result.Value.(type) {
		case dto.ChatCompletionsStreamResponse:
			if len(value.Choices) == 0 && value.Usage == nil {
				return true
			}
			if err := helper.ObjectData(c, &value); err != nil {
				streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
				return false
			}
			return true
		case *dto.ChatCompletionsStreamResponse:
			if value == nil || (len(value.Choices) == 0 && value.Usage == nil) {
				return true
			}
			if err := helper.ObjectData(c, value); err != nil {
				streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
				return false
			}
			return true
		case dto.ClaudeResponse:
			if err := helper.ClaudeData(c, value); err != nil {
				streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
				return false
			}
			return true
		case *dto.ClaudeResponse:
			if value == nil {
				return true
			}
			if err := helper.ClaudeData(c, *value); err != nil {
				streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
				return false
			}
			return true
		case dto.GeminiChatResponse:
			return sendGeminiResponse(&value)
		case *dto.GeminiChatResponse:
			return sendGeminiResponse(value)
		default:
			streamErr = types.NewOpenAIError(fmt.Errorf("unsupported converted stream response type %T", result.Value), types.ErrorCodeBadResponse, http.StatusInternalServerError)
			return false
		}
	}

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		if streamErr != nil {
			sr.Stop(streamErr)
			return
		}

		var streamResp dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResp); err != nil {
			logger.LogError(c, "failed to unmarshal responses stream event: "+err.Error())
			if info.StreamStatus != nil {
				info.StreamStatus.MarkUpstreamFailure()
			}
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
			sr.Stop(streamErr)
			return
		}

		if streamResp.Type == "response.error" || streamResp.Type == "response.failed" {
			if streamResp.Response != nil {
				if oaiErr := streamResp.Response.GetOpenAIError(); responsesErrorHasDetails(oaiErr) {
					markTransientResponsesError(info, oaiErr, resp.StatusCode)
					streamErr = types.WithOpenAIError(*oaiErr, http.StatusInternalServerError)
					sr.Stop(streamErr)
					return
				}
			}
			if info.StreamStatus != nil && streamResp.Type == "response.failed" {
				// A failed Responses event without a structured error is still a
				// provider-side terminal failure; incomplete/cancelled events are
				// handled below as non-terminal outcomes.
				info.StreamStatus.MarkUpstreamFailure()
			}
			streamErr = types.NewOpenAIError(fmt.Errorf("responses stream error: %s", streamResp.Type), types.ErrorCodeBadResponse, http.StatusInternalServerError)
			sr.Stop(streamErr)
			return
		}
		if streamResp.Type == "response.incomplete" || streamResp.Type == "response.cancelled" || streamResp.Type == "response.canceled" {
			if strictOpenCodeGo {
				// `incomplete` commonly means a deterministic stop such as
				// max_output_tokens/content_filter; `cancelled` can be client-driven.
				// Surface both as failover evidence without rotating the workspace.
				// Other channels must keep converting them as normal terminal events.
				streamErr = types.NewOpenAIError(fmt.Errorf("responses stream ended with %s", streamResp.Type), types.ErrorCodeBadResponse, http.StatusInternalServerError)
				sr.Stop(streamErr)
				return
			}
		}
		if (streamResp.Type == "response.completed" || streamResp.Type == "response.done") && info.StreamStatus != nil {
			info.StreamStatus.MarkProtocolTerminal()
		}

		results, err := relayconvert.ConvertStreamResponseChunk(c, info, state, &streamResp)
		if err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
			sr.Stop(streamErr)
			return
		}
		for _, result := range results {
			if !sendStreamResult(result) {
				sr.Stop(streamErr)
				return
			}
		}
	})

	if streamErr != nil {
		return nil, streamErr
	}

	usage := state.Usage()
	if usage == nil || usage.TotalTokens == 0 {
		usage = service.ResponseText2Usage(c, state.UsageText(), info.UpstreamModelName, info.GetEstimatePromptTokens())
		state.SetUsage(usage)
	}

	if info.RelayFormat == types.RelayFormatClaude && info.ClaudeConvertInfo != nil {
		info.ClaudeConvertInfo.Usage = usage
	}
	finalResults, err := relayconvert.FinalizeStreamResponse(c, info, state)
	if err != nil {
		if info.StreamStatus != nil {
			info.StreamStatus.MarkLocalFailure()
		}
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	for _, result := range finalResults {
		if !sendStreamResult(result) {
			if info.StreamStatus != nil {
				info.StreamStatus.MarkLocalFailure()
			}
			return nil, streamErr
		}
	}
	if info.RelayFormat == types.RelayFormatOpenAI && info.ShouldIncludeUsage && usage != nil {
		if err := helper.ObjectData(c, helper.GenerateFinalUsageResponse(responseId, createAt, info.UpstreamModelName, *usage)); err != nil {
			if info.StreamStatus != nil {
				info.StreamStatus.MarkLocalFailure()
			}
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
		}
	}

	if info.RelayFormat == types.RelayFormatOpenAI {
		if err := helper.Done(c); err != nil {
			if info.StreamStatus != nil {
				info.StreamStatus.MarkLocalFailure()
			}
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
		}
	}
	return usage, nil
}
