package opencodego

import (
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestUsesFunctionToolsDistinguishesHostedWebSearch(t *testing.T) {
	assert.False(t, requestUsesFunctionTools(&dto.ClaudeRequest{}))
	assert.False(t, requestUsesFunctionTools(&dto.ClaudeRequest{Tools: []map[string]any{
		{"type": "web_search_20250305", "name": "web_search"},
	}}))
	assert.True(t, requestUsesFunctionTools(&dto.ClaudeRequest{Tools: []map[string]any{
		{"name": "Bash", "input_schema": map[string]any{"type": "object"}},
	}}))
	assert.True(t, requestUsesFunctionTools(&dto.ClaudeRequest{Tools: []map[string]any{
		{"type": "computer_20250124", "name": "computer"},
	}}))
}

func TestRequestContainsAssistantReasoningDetectsHistoryAcrossFormats(t *testing.T) {
	assert.False(t, requestContainsAssistantReasoning(&dto.ClaudeRequest{}))
	assert.False(t, requestContainsAssistantReasoning(&dto.GeneralOpenAIRequest{}))
	assert.False(t, requestContainsAssistantReasoning(&dto.OpenAIResponsesRequest{}))

	thinking := "I should inspect the repository."
	claude := &dto.ClaudeRequest{Messages: []dto.ClaudeMessage{
		{Role: "assistant", Content: []any{
			map[string]any{"type": "thinking", "thinking": thinking},
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{}},
		}},
	}}
	assert.True(t, requestContainsAssistantReasoning(claude))

	chatMsg := dto.Message{Role: "assistant"}
	chatMsg.ReasoningContent = &thinking
	chatMsg.SetToolCalls([]dto.ToolCallRequest{
		{ID: "call_1", Type: "function", Function: dto.FunctionRequest{Name: "Bash"}},
	})
	chat := &dto.GeneralOpenAIRequest{Messages: []dto.Message{chatMsg}}
	assert.True(t, requestContainsAssistantReasoning(chat))

	responses := &dto.OpenAIResponsesRequest{Input: json.RawMessage(`[
		{"type":"reasoning","id":"rs_1","status":"completed","summary":[{"type":"summary_text","text":"x"}]},
		{"type":"function_call","id":"call_1","call_id":"call_1","name":"Bash","arguments":"{}"}
	]`)}
	assert.True(t, requestContainsAssistantReasoning(responses))

	emptyThinking := ""
	claudeEmpty := &dto.ClaudeRequest{Messages: []dto.ClaudeMessage{
		{Role: "assistant", Content: []any{
			map[string]any{"type": "thinking", "thinking": emptyThinking},
		}},
	}}
	assert.False(t, requestContainsAssistantReasoning(claudeEmpty))

	claudeUser := &dto.ClaudeRequest{Messages: []dto.ClaudeMessage{{
		Role: "user", Content: []any{map[string]any{"type": "thinking", "thinking": thinking}},
	}}}
	assert.False(t, requestContainsAssistantReasoning(claudeUser))

	chatUserMessage := dto.Message{Role: "user"}
	chatUserMessage.ReasoningContent = &thinking
	assert.False(t, requestContainsAssistantReasoning(&dto.GeneralOpenAIRequest{
		Messages: []dto.Message{chatUserMessage},
	}))
}

func TestPrepareOpenCodeGoResponsesToolHistoryPreservesExistingID(t *testing.T) {
	request := &dto.OpenAIResponsesRequest{Input: json.RawMessage(`[
		{"type":"reasoning","summary":[{"type":"summary_text","text":"inspect first"}]},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"inspect first"}]},
		{"type":"function_call","call_id":"call_missing","name":"first","arguments":"{}"},
		{"type":"function_call","id":"item_existing","call_id":"call_existing","name":"second","arguments":"{}"}
	]`)}

	require.NoError(t, prepareOpenCodeGoResponsesToolHistory(request))
	var input []map[string]any
	require.NoError(t, json.Unmarshal(request.Input, &input))
	assert.Regexp(t, `^rs_[0-9a-f]{24}$`, input[0]["id"])
	assert.Equal(t, "completed", input[0]["status"])
	assert.Regexp(t, `^rs_[0-9a-f]{24}$`, input[1]["id"])
	assert.NotEqual(t, input[0]["id"], input[1]["id"])
	assert.Equal(t, "call_missing", input[2]["id"])
	assert.Equal(t, "item_existing", input[3]["id"])
}
