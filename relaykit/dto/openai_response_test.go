package dto

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAIResponsesResponseUnmarshalOutputTokenDetailsAlias(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want OutputTokenDetails
	}{
		{
			name: "responses wire alias",
			wire: `{
				"output_tokens_details": {
					"text_tokens": 31,
					"audio_tokens": 7,
					"image_tokens": 5,
					"reasoning_tokens": 13
				}
			}`,
			want: OutputTokenDetails{
				TextTokens:      31,
				AudioTokens:     7,
				ImageTokens:     5,
				ReasoningTokens: 13,
			},
		},
		{
			name: "canonical field",
			wire: `{
				"completion_tokens_details": {
					"text_tokens": 17,
					"audio_tokens": 3,
					"image_tokens": 2,
					"reasoning_tokens": 11
				}
			}`,
			want: OutputTokenDetails{
				TextTokens:      17,
				AudioTokens:     3,
				ImageTokens:     2,
				ReasoningTokens: 11,
			},
		},
		{
			name: "canonical field wins conflict",
			wire: `{
				"completion_tokens_details": {"reasoning_tokens": 19},
				"output_tokens_details": {"reasoning_tokens": 23}
			}`,
			want: OutputTokenDetails{ReasoningTokens: 19},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var response OpenAIResponsesResponse
			require.NoError(t, json.Unmarshal([]byte(`{"usage":`+tt.wire+`}`), &response))
			require.NotNil(t, response.Usage)
			assert.Equal(t, tt.want, response.Usage.CompletionTokenDetails)
		})
	}
}

func TestOpenAIResponsesCompactionResponseUnmarshalOutputTokenDetailsAlias(t *testing.T) {
	var response OpenAIResponsesCompactionResponse
	require.NoError(t, json.Unmarshal([]byte(`{
		"usage": {
			"input_tokens": 9,
			"output_tokens": 4,
			"output_tokens_details": {"reasoning_tokens": 3}
		}
	}`), &response))
	require.NotNil(t, response.Usage)
	assert.Equal(t, 9, response.Usage.InputTokens)
	assert.Equal(t, 4, response.Usage.OutputTokens)
	assert.Equal(t, 3, response.Usage.CompletionTokenDetails.ReasoningTokens)
}

func TestUsageMarshalKeepsCanonicalOutputTokenDetailsSchema(t *testing.T) {
	encoded, err := json.Marshal(Usage{
		CompletionTokenDetails: OutputTokenDetails{ReasoningTokens: 13},
	})
	require.NoError(t, err)

	assert.Contains(t, string(encoded), `"completion_tokens_details":{"text_tokens":0,"audio_tokens":0,"image_tokens":0,"reasoning_tokens":13}`)
	assert.NotContains(t, string(encoded), `"output_tokens_details"`)
}

func TestInputTokenDetailsUnmarshalCacheCreationInputTokensAlias(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want InputTokenDetails
	}{
		{
			name: "openai-compatible alias",
			wire: `{"cached_tokens":80,"cache_creation_input_tokens":30}`,
			want: InputTokenDetails{CachedTokens: 80, CachedCreationTokens: 30},
		},
		{
			name: "larger alias wins canonical aggregate",
			wire: `{"cached_creation_tokens":20,"cache_creation_input_tokens":30,"cache_write_tokens":25}`,
			want: InputTokenDetails{CachedCreationTokens: 30, CacheWriteTokens: 25},
		},
		{
			name: "larger canonical aggregate wins alias",
			wire: `{"cached_creation_tokens":40,"cache_creation_input_tokens":30,"cache_write_tokens":25}`,
			want: InputTokenDetails{CachedCreationTokens: 40, CacheWriteTokens: 25},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var details InputTokenDetails
			require.NoError(t, json.Unmarshal([]byte(tt.wire), &details))
			assert.Equal(t, tt.want, details)

			encoded, err := json.Marshal(details)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "cache_creation_input_tokens")
		})
	}
}

func TestUsageUnmarshalTopLevelCachedTokensAlias(t *testing.T) {
	var usage Usage
	require.NoError(t, json.Unmarshal([]byte(`{"prompt_tokens":210,"cached_tokens":80}`), &usage))

	assert.Equal(t, 210, usage.PromptTokens)
	assert.Equal(t, 80, usage.CachedTokens)
	assert.True(t, HasOpenAIUsageTokens(&Usage{CachedTokens: usage.CachedTokens}))
	require.NotNil(t, NewOpenAIChatBillingUsage(&Usage{CachedTokens: usage.CachedTokens}))
}

func TestUsageMarshalOmitsInternalBillingMetadata(t *testing.T) {
	usage := Usage{
		UsageSemantic: BillingUsageSemanticAnthropic,
		UsageSource:   BillingUsageSourceClaudeMessages,
		BillingUsage: NewClaudeMessagesBillingUsage(&ClaudeUsage{
			InputTokens: 7,
		}),
	}

	encoded, err := json.Marshal(usage)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), `"usage_semantic"`)
	assert.NotContains(t, string(encoded), `"usage_source"`)
	assert.NotContains(t, string(encoded), `"billing_usage"`)
}

func TestOpenAITextResponseMarshalKeepsEnvelopeAndOmitsInternalBillingMetadata(t *testing.T) {
	response := OpenAITextResponse{
		Id:     "chatcmpl-usage-privacy",
		Model:  "public-model",
		Object: "chat.completion",
		Choices: []OpenAITextResponseChoice{
			{Index: 0, FinishReason: "stop"},
		},
		Usage: Usage{
			PromptTokens:  7,
			TotalTokens:   7,
			UsageSemantic: BillingUsageSemanticAnthropic,
			UsageSource:   BillingUsageSourceClaudeMessages,
			BillingUsage: NewClaudeMessagesBillingUsage(&ClaudeUsage{
				InputTokens: 7,
			}),
		},
	}

	encoded, err := json.Marshal(response)
	require.NoError(t, err)

	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &envelope))
	assert.JSONEq(t, `"chatcmpl-usage-privacy"`, string(envelope["id"]))
	assert.JSONEq(t, `"public-model"`, string(envelope["model"]))
	require.Contains(t, envelope, "choices")
	require.Contains(t, envelope, "usage")

	var publicUsage map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(envelope["usage"], &publicUsage))
	assert.JSONEq(t, `7`, string(publicUsage["prompt_tokens"]))
	assert.NotContains(t, publicUsage, "usage_semantic")
	assert.NotContains(t, publicUsage, "usage_source")
	assert.NotContains(t, publicUsage, "billing_usage")
}

func TestProviderUsageMarshalOmitsInternalBillingUsage(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		usage := ClaudeUsage{
			InputTokens:  7,
			BillingUsage: NewOpenAIChatBillingUsage(&Usage{PromptTokens: 7}),
		}

		encoded, err := json.Marshal(usage)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), `"billing_usage"`)
	})

	t.Run("gemini", func(t *testing.T) {
		usage := GeminiUsageMetadata{
			PromptTokenCount: 7,
			BillingUsage:     NewOpenAIChatBillingUsage(&Usage{PromptTokens: 7}),
		}

		encoded, err := json.Marshal(usage)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), `"billing_usage"`)
	})
}
