package service

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/stretchr/testify/assert"
)

func TestOpenCodeGoOwnProtocolSelectionOverridesGlobalChatUpgrade(t *testing.T) {
	policy := model_setting.ChatCompletionsToResponsesPolicy{
		Enabled:       true,
		AllChannels:   true,
		ModelPatterns: []string{".*"},
	}

	assert.True(t, ShouldChatCompletionsUseResponsesPolicy(policy, 1, constant.ChannelTypeOpenAI, "glm-5.2"))
	assert.False(t, ShouldChatCompletionsUseResponsesPolicy(policy, 1, constant.ChannelTypeOpenCodeGo, "glm-5.2"))
}
