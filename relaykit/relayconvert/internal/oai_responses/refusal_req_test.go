package oairesponses

import (
	"context"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponsesRefusalReplayPreservedForClaudeAndGemini(t *testing.T) {
	input := mustRawMessage(t, []map[string]any{
		{
			"type":   "message",
			"id":     "msg_refusal",
			"status": "completed",
			"role":   "assistant",
			"content": []map[string]any{
				{"type": "refusal", "refusal": "I cannot help with that."},
			},
		},
	})

	t.Run("claude", func(t *testing.T) {
		maxTokens := uint(16)
		got, err := OpenAIResponsesRequestToClaudeMessages(context.Background(), &convmeta.Values{}, &dto.OpenAIResponsesRequest{
			Model:           "claude-test",
			Input:           input,
			MaxOutputTokens: &maxTokens,
		})
		require.NoError(t, err)
		require.Len(t, got.Messages, 2)
		parts := claudeMessageContentParts(got.Messages[1].Content)
		require.Len(t, parts, 1)
		assert.Equal(t, "text", parts[0].Type)
		assert.Equal(t, "I cannot help with that.", parts[0].GetText())
	})

	t.Run("gemini", func(t *testing.T) {
		got, err := OpenAIResponsesRequestToGeminiChat(context.Background(), &dto.OpenAIResponsesRequest{
			Model: "gemini-test",
			Input: input,
		}, &convmeta.Values{})
		require.NoError(t, err)
		require.Len(t, got.Contents, 1)
		assert.Equal(t, "model", got.Contents[0].Role)
		require.Len(t, got.Contents[0].Parts, 1)
		assert.Equal(t, "I cannot help with that.", got.Contents[0].Parts[0].Text)
	})
}
