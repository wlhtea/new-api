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

func TestPrepareOpenCodeGoResponsesToolsLowersCustomToolHistory(t *testing.T) {
	request := &dto.OpenAIResponsesRequest{Input: json.RawMessage(`[
		{
			"type":"additional_tools",
			"tools":[{"type":"custom","name":"apply_patch","description":"Apply a patch"}]
		},
		{
			"type":"custom_tool_call",
			"call_id":"call_patch",
			"name":"apply_patch",
			"input":"*** Begin Patch\n*** End Patch",
			"metadata":{"attempt":1}
		},
		{
			"type":"custom_tool_call_output",
			"call_id":"call_patch",
			"output":"Done"
		},
		{
			"type":"function_call",
			"call_id":"call_lookup",
			"name":"lookup",
			"arguments":"{\"q\":\"x\"}"
		}
	]`)}

	_, err := prepareOpenCodeGoResponsesTools(request)
	require.NoError(t, err)

	var tools []map[string]any
	require.NoError(t, json.Unmarshal(request.Tools, &tools))
	require.Len(t, tools, 1)
	assert.Equal(t, "function", tools[0]["type"])
	assert.Equal(t, "apply_patch", tools[0]["name"])

	var input []map[string]any
	require.NoError(t, json.Unmarshal(request.Input, &input))
	require.Len(t, input, 3)
	assert.NotContains(t, string(request.Input), `"type":"custom`)
	assert.Equal(t, "function_call", input[0]["type"])
	assert.Equal(t, "call_patch", input[0]["call_id"])
	assert.Equal(t, "apply_patch", input[0]["name"])
	assert.JSONEq(t, `{"input":"*** Begin Patch\n*** End Patch"}`, input[0]["arguments"].(string))
	assert.Equal(t, float64(1), input[0]["metadata"].(map[string]any)["attempt"])
	_, hasInput := input[0]["input"]
	assert.False(t, hasInput)

	assert.Equal(t, "function_call_output", input[1]["type"])
	assert.Equal(t, "call_patch", input[1]["call_id"])
	assert.Equal(t, "Done", input[1]["output"])
	assert.Equal(t, "function_call", input[2]["type"])
	assert.Equal(t, `{"q":"x"}`, input[2]["arguments"])

	require.NoError(t, prepareOpenCodeGoResponsesToolHistory(request))
	require.NoError(t, json.Unmarshal(request.Input, &input))
	assert.Equal(t, "call_patch", input[0]["id"])
	assert.Equal(t, "call_lookup", input[2]["id"])
}

func TestPrepareOpenCodeGoResponsesToolsLowersCustomHistoryWithoutDeclarations(t *testing.T) {
	request := &dto.OpenAIResponsesRequest{Input: json.RawMessage(`[
		{
			"type":"custom_tool_call",
			"call_id":"call_structured",
			"name":"structured_tool",
			"input":{"path":"README.md"}
		},
		{
			"type":"custom_tool_call_output",
			"call_id":"call_structured",
			"output":{"ok":true}
		},
		{
			"type":"custom_tool_call",
			"call_id":"call_null",
			"name":"nullable_tool",
			"input":null
		}
	]`)}

	_, err := prepareOpenCodeGoResponsesTools(request)
	require.NoError(t, err)
	assert.Empty(t, request.Tools)

	var input []map[string]any
	require.NoError(t, json.Unmarshal(request.Input, &input))
	require.Len(t, input, 3)
	assert.Equal(t, "function_call", input[0]["type"])
	assert.JSONEq(t, `{"input":"{\"path\":\"README.md\"}"}`, input[0]["arguments"].(string))
	assert.Equal(t, "function_call_output", input[1]["type"])
	assert.Equal(t, map[string]any{"ok": true}, input[1]["output"])
	assert.Equal(t, "function_call", input[2]["type"])
	assert.JSONEq(t, `{"input":"null"}`, input[2]["arguments"].(string))

	require.NoError(t, prepareOpenCodeGoResponsesToolHistory(request))
	require.NoError(t, json.Unmarshal(request.Input, &input))
	assert.Equal(t, "call_structured", input[0]["id"])
	assert.Equal(t, "call_null", input[2]["id"])
}
