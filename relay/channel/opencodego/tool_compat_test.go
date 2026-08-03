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

func TestPrepareOpenCodeGoResponsesToolHistoryPreservesExistingID(t *testing.T) {
	request := &dto.OpenAIResponsesRequest{Input: json.RawMessage(`[
		{"type":"function_call","call_id":"call_missing","name":"first","arguments":"{}"},
		{"type":"function_call","id":"item_existing","call_id":"call_existing","name":"second","arguments":"{}"}
	]`)}

	require.NoError(t, prepareOpenCodeGoResponsesToolHistory(request))
	var input []map[string]any
	require.NoError(t, json.Unmarshal(request.Input, &input))
	assert.Equal(t, "call_missing", input[0]["id"])
	assert.Equal(t, "item_existing", input[1]["id"])
}
