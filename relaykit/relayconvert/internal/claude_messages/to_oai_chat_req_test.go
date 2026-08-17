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

func TestClaudeMessagesRequestToOpenAIChatPreservesPathToolReplay(t *testing.T) {
	req := dto.ClaudeRequest{
		Model: "deepseek-test",
		Tools: []dto.Tool{{
			Name: "Bash", InputSchema: map[string]interface{}{"type": "object"},
		}},
		Messages: []dto.ClaudeMessage{
			{Role: "user", Content: []dto.ClaudeMediaMessage{
				{Type: "text", Text: stringPointer("read the attached local path")},
			}},
			{Role: "assistant", Content: []dto.ClaudeMediaMessage{
				{Type: "thinking", Thinking: stringPointer("inspect the file")},
				{Type: "text", Text: stringPointer("I will read it.")},
				{Type: "tool_use", Id: "toolu_1", Name: "Bash", Input: map[string]any{"command": "cat -- <local-path>"}},
			}},
			{Role: "user", Content: []dto.ClaudeMediaMessage{
				{Type: "tool_result", ToolUseId: "toolu_1", Content: []any{
					map[string]any{"type": "text", "text": "first"},
					map[string]any{"type": "text", "text": " second"},
				}},
			}},
		},
	}

	got, err := ClaudeMessagesRequestToOpenAIChat(req, nil)
	require.NoError(t, err)
	require.Len(t, got.Messages, 3)
	assert.Equal(t, "user", got.Messages[0].Role)
	require.Len(t, got.Messages[0].ParseContent(), 1)
	assert.Equal(t, "read the attached local path", got.Messages[0].ParseContent()[0].Text)
	assert.Equal(t, "assistant", got.Messages[1].Role)
	assert.Equal(t, "I will read it.", got.Messages[1].StringContent())
	assert.Equal(t, "inspect the file", got.Messages[1].GetReasoningContent())
	require.Len(t, got.Messages[1].ParseToolCalls(), 1)
	assert.Equal(t, "toolu_1", got.Messages[1].ParseToolCalls()[0].ID)
	assert.Equal(t, "tool", got.Messages[2].Role)
	assert.Equal(t, "toolu_1", got.Messages[2].ToolCallId)
	assert.Equal(t, "first second", got.Messages[2].StringContent())
}

func TestClaudeMessagesRequestToOpenAIChatRejectsUnsupportedToolResultBlocks(t *testing.T) {
	for _, test := range []struct {
		name    string
		content any
	}{
		{name: "image", content: []any{
			map[string]any{"type": "image", "source": map[string]any{"type": "base64"}},
		}},
		{name: "document", content: []any{
			map[string]any{"type": "document", "source": map[string]any{"type": "text"}},
		}},
		{name: "unknown", content: []any{map[string]any{"type": "future_private_block"}}},
		{name: "mixed", content: []any{
			map[string]any{"type": "text", "text": "visible"},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64"}},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := dto.ClaudeRequest{
				Model: "deepseek-test",
				Messages: []dto.ClaudeMessage{{
					Role: "user",
					Content: []dto.ClaudeMediaMessage{{
						Type: "tool_result", ToolUseId: "toolu_1", Content: test.content,
					}},
				}},
			}

			_, err := ClaudeMessagesRequestToOpenAIChat(req, nil)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "visible")
			assert.NotContains(t, err.Error(), "future_private_block")
		})
	}
}

func stringPointer(value string) *string {
	return &value
}
