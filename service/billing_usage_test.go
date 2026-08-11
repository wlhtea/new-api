package service

import (
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEffectiveBillingUsageNormalizesResponsesInputTokenDetails(t *testing.T) {
	responsesUsage := &dto.Usage{
		InputTokens:  345,
		OutputTokens: 2,
		TotalTokens:  347,
		InputTokensDetails: &dto.InputTokenDetails{
			CachedTokens:         256,
			CachedCreationTokens: 64,
			CacheWriteTokens:     32,
		},
	}
	usage := &dto.Usage{BillingUsage: dto.NewOpenAIResponsesBillingUsage(responsesUsage)}

	effective := effectiveBillingUsage(usage)
	require.NotNil(t, effective)
	assert.Equal(t, 345, effective.PromptTokens)
	assert.Equal(t, 2, effective.CompletionTokens)
	assert.Equal(t, 256, effective.PromptTokensDetails.CachedTokens)
	assert.Equal(t, 64, effective.PromptTokensDetails.CachedCreationTokens)
	assert.Equal(t, 32, effective.PromptTokensDetails.CacheWriteTokens)
}

func TestEffectiveBillingUsageClaudeCacheCreationUsesReliableTotal(t *testing.T) {
	tests := []struct {
		name      string
		aggregate int
		split5m   int
		split1h   int
		wantWrite int
	}{
		{name: "aggregate matches splits", aggregate: 30, split5m: 20, split1h: 10, wantWrite: 30},
		{name: "split fallback", split5m: 20, split1h: 10, wantWrite: 30},
		{name: "aggregate preserves generic remainder", aggregate: 50, split5m: 20, split1h: 10, wantWrite: 50},
		{name: "splits override incomplete aggregate", aggregate: 20, split5m: 20, split1h: 10, wantWrite: 30},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage := &dto.Usage{BillingUsage: dto.NewClaudeMessagesBillingUsage(&dto.ClaudeUsage{
				InputTokens:              100,
				CacheReadInputTokens:     80,
				CacheCreationInputTokens: tt.aggregate,
				OutputTokens:             40,
				CacheCreation: &dto.ClaudeCacheCreationUsage{
					Ephemeral5mInputTokens: tt.split5m,
					Ephemeral1hInputTokens: tt.split1h,
				},
			})}

			effective := effectiveBillingUsage(usage)

			require.NotNil(t, effective)
			assert.Equal(t, 100, effective.PromptTokens)
			assert.Equal(t, 40, effective.CompletionTokens)
			assert.Equal(t, 140, effective.TotalTokens)
			assert.Equal(t, 100+80+tt.wantWrite, effective.InputTokens)
			assert.Equal(t, 80, effective.PromptTokensDetails.CachedTokens)
			assert.Equal(t, tt.wantWrite, effective.PromptTokensDetails.CachedCreationTokens)
			assert.Equal(t, tt.split5m, effective.ClaudeCacheCreation5mTokens)
			assert.Equal(t, tt.split1h, effective.ClaudeCacheCreation1hTokens)
		})
	}
}

func TestEffectiveBillingUsageKeepsClaudeCacheOnlyUsage(t *testing.T) {
	usage := &dto.Usage{BillingUsage: dto.NewClaudeMessagesBillingUsage(&dto.ClaudeUsage{
		CacheReadInputTokens: 80,
		CacheCreation: &dto.ClaudeCacheCreationUsage{
			Ephemeral5mInputTokens: 20,
			Ephemeral1hInputTokens: 10,
		},
	})}

	effective := effectiveBillingUsage(usage)

	require.NotNil(t, effective)
	assert.Zero(t, effective.PromptTokens)
	assert.Zero(t, effective.CompletionTokens)
	assert.Zero(t, effective.TotalTokens)
	assert.Equal(t, 110, effective.InputTokens)
	assert.Equal(t, 80, effective.PromptTokensDetails.CachedTokens)
	assert.Equal(t, 30, effective.PromptTokensDetails.CachedCreationTokens)
}
