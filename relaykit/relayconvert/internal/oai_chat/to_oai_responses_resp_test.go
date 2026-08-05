package oaichat

import (
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatCompletionsResponseToResponsesPreservesTextToolCallsAndUsage(t *testing.T) {
	chat := &dto.OpenAITextResponse{
		Id:      "chatcmpl_1",
		Model:   "gpt-test",
		Created: 456,
		Choices: []dto.OpenAITextResponseChoice{
			{
				Message:      assistantMessageWithTool("I will call.", "call_1", "lookup", `{"q":"x"}`),
				FinishReason: "tool_calls",
			},
		},
		Usage: dto.Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8},
	}

	resp, usage, err := ChatCompletionsResponseToResponsesResponse(chat, "resp_1")
	require.NoError(t, err)
	require.NotNil(t, usage)

	assert.Equal(t, "resp_1", resp.ID)
	assert.Equal(t, "response", resp.Object)
	assert.Equal(t, `"completed"`, string(resp.Status))
	assert.Equal(t, 3, resp.Usage.InputTokens)
	assert.Equal(t, 5, resp.Usage.OutputTokens)
	require.Len(t, resp.Output, 2)
	assert.Equal(t, responsesOutputTypeMessage, resp.Output[0].Type)
	assert.Equal(t, "I will call.", resp.Output[0].Content[0].Text)
	assert.Equal(t, responsesOutputTypeFunctionCall, resp.Output[1].Type)
	assert.Equal(t, "call_1", resp.Output[1].CallId)
	assert.Equal(t, "lookup", resp.Output[1].Name)
	assert.Equal(t, `"{\"q\":\"x\"}"`, string(resp.Output[1].Arguments))
}

func TestChatCompletionsResponseToResponsesMapsIncompleteFinishReasons(t *testing.T) {
	tests := []struct {
		name         string
		finishReason string
		wantReason   string
	}{
		{name: "length", finishReason: "length", wantReason: responsesIncompleteReasonMaxTokens},
		{name: "content filter", finishReason: "content_filter", wantReason: responsesIncompleteReasonContentFilter},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, _, err := ChatCompletionsResponseToResponsesResponse(&dto.OpenAITextResponse{
				Id:    "chatcmpl_1",
				Model: "gpt-test",
				Choices: []dto.OpenAITextResponseChoice{
					{
						Message:      dto.Message{Role: "assistant", Content: "partial"},
						FinishReason: tt.finishReason,
					},
				},
			}, "resp_1")
			require.NoError(t, err)

			assert.Equal(t, `"incomplete"`, string(resp.Status))
			require.NotNil(t, resp.IncompleteDetails)
			assert.Equal(t, tt.wantReason, resp.IncompleteDetails.Reason)
			require.Len(t, resp.Output, 1)
			assert.Equal(t, "incomplete", resp.Output[0].Status)
		})
	}
}

func TestChatCompletionsStreamToResponsesEventsAggregatesUsageAndToolArgs(t *testing.T) {
	state := NewChatToResponsesStreamState("resp_1", "gpt-test")
	state.Created = 123
	toolIndex := 0

	var events []ChatToResponsesStreamEvent
	events = append(events, mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
		Id:      "chatcmpl_1",
		Model:   "gpt-test",
		Created: 123,
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{Index: 0, Delta: dto.ChatCompletionsStreamResponseChoiceDelta{Role: "assistant"}},
		},
	})...)
	events = append(events, mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{Index: 0, Delta: dto.ChatCompletionsStreamResponseChoiceDelta{Content: lo.ToPtr("hello")}},
		},
	})...)
	events = append(events, mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{Index: 0, Delta: dto.ChatCompletionsStreamResponseChoiceDelta{ToolCalls: []dto.ToolCallResponse{
				{Index: &toolIndex, ID: "call_1", Type: "function", Function: dto.FunctionResponse{Name: "lookup"}},
			}}},
		},
	})...)
	events = append(events, mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{Index: 0, Delta: dto.ChatCompletionsStreamResponseChoiceDelta{ToolCalls: []dto.ToolCallResponse{
				{Index: &toolIndex, Function: dto.FunctionResponse{Arguments: `{"q":"x"}`}},
			}}},
		},
	})...)
	finishReason := "tool_calls"
	events = append(events, mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{Index: 0, FinishReason: &finishReason},
		},
	})...)
	events = append(events, mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
		Usage: &dto.Usage{PromptTokens: 2, CompletionTokens: 4, TotalTokens: 6},
	})...)
	events = append(events, FinalizeChatCompletionsStreamToResponses(state)...)

	require.Len(t, events, 10)
	assert.Equal(t, responsesEventCreated, events[0].Type)
	assert.Equal(t, responsesEventOutputTextDelta, events[2].Type)
	assert.Equal(t, "hello", events[2].Payload.Delta)
	assert.Equal(t, responsesEventFunctionArgsDelta, events[4].Type)
	assert.Equal(t, `{"q":"x"}`, events[4].Payload.Delta)
	assert.Equal(t, responsesEventCompleted, events[9].Type)
	require.NotNil(t, events[9].Payload.Response)
	assert.Equal(t, 6, events[9].Payload.Response.Usage.TotalTokens)
	require.Len(t, events[9].Payload.Response.Output, 2)
	assert.Equal(t, "hello", events[9].Payload.Response.Output[0].Content[0].Text)
	assert.Equal(t, `"{\"q\":\"x\"}"`, string(events[9].Payload.Response.Output[1].Arguments))
}

func TestChatCompletionsStreamToResponsesReasoningUsesSummaryPartLifecycle(t *testing.T) {
	state := NewChatToResponsesStreamState("resp_reasoning", "gpt-test")
	events := mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
		Id:      "chat_reasoning",
		Model:   "gpt-test",
		Created: 123,
		Choices: []dto.ChatCompletionsStreamResponseChoice{{
			Delta: dto.ChatCompletionsStreamResponseChoiceDelta{
				ReasoningContent: lo.ToPtr("think"),
			},
		}},
	})
	finishReason := "stop"
	events = append(events, mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
		Choices: []dto.ChatCompletionsStreamResponseChoice{{FinishReason: &finishReason}},
	})...)
	events = append(events, FinalizeChatCompletionsStreamToResponses(state)...)

	require.Len(t, events, 8)
	assert.Equal(t, responsesEventCreated, events[0].Type)
	assert.Equal(t, responsesEventOutputItemAdded, events[1].Type)
	assert.Equal(t, responsesEventReasoningSummaryPartAdded, events[2].Type)
	assert.Equal(t, responsesEventReasoningSummaryDelta, events[3].Type)
	assert.Equal(t, responsesEventReasoningSummaryDone, events[4].Type)
	assert.Equal(t, responsesEventReasoningSummaryPartDone, events[5].Type)
	assert.Equal(t, responsesEventOutputItemDone, events[6].Type)
	assert.Equal(t, responsesEventCompleted, events[7].Type)
	assert.Equal(t, "think", events[3].Payload.Delta)
	require.NotNil(t, events[5].Payload.Part)
	assert.Equal(t, "think", events[5].Payload.Part.Text)
}

func mustResponsesEventsFromChatChunk(t *testing.T, state *ChatToResponsesStreamState, chunk *dto.ChatCompletionsStreamResponse) []ChatToResponsesStreamEvent {
	t.Helper()
	events, err := ChatCompletionsStreamChunkToResponsesEvents(chunk, state)
	require.NoError(t, err)
	return events
}
