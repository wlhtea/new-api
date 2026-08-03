package claudemessages

import (
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeMessagesRequestToOpenAIChatPreservesAssistantThinking(t *testing.T) {
	req := dto.ClaudeRequest{
		Model: "deepseek-test",
		Messages: []dto.ClaudeMessage{
			{
				Role: "assistant",
				Content: []dto.ClaudeMediaMessage{
					{Type: "thinking", Thinking: stringPointer("inspect the repository")},
					{Type: "thinking", Thinking: stringPointer("then call Bash")},
					{Type: "tool_use", Id: "toolu_1", Name: "Bash", Input: map[string]any{"command": "pwd"}},
				},
			},
		},
	}

	got, err := ClaudeMessagesRequestToOpenAIChat(req, nil)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "inspect the repository\n\nthen call Bash", got.Messages[0].GetReasoningContent())
	require.Len(t, got.Messages[0].ParseToolCalls(), 1)
}

func TestClaudeMessagesRequestToOpenAIChatIgnoresNonAssistantThinking(t *testing.T) {
	req := dto.ClaudeRequest{
		Model: "deepseek-test",
		Messages: []dto.ClaudeMessage{
			{
				Role: "user",
				Content: []dto.ClaudeMediaMessage{
					{Type: "thinking", Thinking: stringPointer("untrusted reasoning")},
					{Type: "text", Text: stringPointer("hello")},
				},
			},
		},
	}

	got, err := ClaudeMessagesRequestToOpenAIChat(req, nil)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Empty(t, got.Messages[0].GetReasoningContent())
}

func stringPointer(value string) *string {
	return &value
}
