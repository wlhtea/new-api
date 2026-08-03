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
