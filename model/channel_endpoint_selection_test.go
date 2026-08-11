package model

import (
	"fmt"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var openCodeGoSupportedSelectionPaths = []string{
	"/v1/chat/completions",
	"/pg/chat/completions",
	"/v1/messages",
	"/v1/responses",
}

var openCodeGoUnsupportedSelectionPaths = []string{
	"",
	"/v1/completions",
	"/v1/moderations",
	"/v1/responses/compact",
	"/v1/responses-compact",
	"/v1/alpha/search",
	"/v1beta/models/gemini-2.5-flash:generateContent",
	"/v1/models/gemini-2.5-flash:generateContent",
	"/v1/embeddings",
	"/v1/engines/gpt-4/embeddings",
	"/v1/rerank",
	"/v1/audio/speech",
	"/v1/audio/transcriptions",
	"/v1/audio/translations",
	"/v1/edits",
	"/v1/images/generations",
	"/v1/images/edits",
	"/v1/realtime",
	"/v1/messages/count_tokens",
	"/v1/videos",
	"/mj/submit/imagine",
}

func TestOpenCodeGoSupportedRequestPathIsExact(t *testing.T) {
	for _, path := range openCodeGoSupportedSelectionPaths {
		t.Run("supported_"+path, func(t *testing.T) {
			assert.True(t, IsOpenCodeGoSupportedRequestPath(path))
			assert.True(t, ChannelTypeSupportsRequestPath(constant.ChannelTypeOpenCodeGo, path))
		})
	}
	for _, path := range openCodeGoUnsupportedSelectionPaths {
		t.Run("unsupported_"+path, func(t *testing.T) {
			assert.False(t, IsOpenCodeGoSupportedRequestPath(path))
			assert.False(t, ChannelTypeSupportsRequestPath(constant.ChannelTypeOpenCodeGo, path))
			assert.True(t, ChannelTypeSupportsRequestPath(constant.ChannelTypeOpenAI, path))
		})
	}
}

func seedEndpointSelectionChannels(t *testing.T) (group string, modelName string, openCodeGoID int, fallbackID int) {
	t.Helper()
	group = fmt.Sprintf("endpoint-guard-group-%d", time.Now().UnixNano())
	modelName = fmt.Sprintf("endpoint-guard-model-%d", time.Now().UnixNano())
	highPriority := int64(100)
	lowPriority := int64(1)
	weight := uint(100)

	openCodeGo := &Channel{
		Type:     constant.ChannelTypeOpenCodeGo,
		Status:   common.ChannelStatusEnabled,
		Name:     "endpoint guard OpenCode Go",
		Models:   modelName,
		Group:    group,
		Priority: &highPriority,
		Weight:   &weight,
	}
	fallback := &Channel{
		Type:     constant.ChannelTypeOpenAI,
		Key:      "fallback-key",
		Status:   common.ChannelStatusEnabled,
		Name:     "endpoint guard fallback",
		Models:   modelName,
		Group:    group,
		Priority: &lowPriority,
		Weight:   &weight,
	}
	require.NoError(t, DB.Create(openCodeGo).Error)
	require.NoError(t, DB.Create(fallback).Error)
	require.NoError(t, openCodeGo.AddAbilities(nil))
	require.NoError(t, fallback.AddAbilities(nil))

	openCodeGoID = openCodeGo.Id
	fallbackID = fallback.Id
	t.Cleanup(func() {
		previousMemoryCacheEnabled := common.MemoryCacheEnabled
		common.MemoryCacheEnabled = false
		require.NoError(t, DB.Where("channel_id IN ?", []int{openCodeGoID, fallbackID}).Delete(&Ability{}).Error)
		require.NoError(t, DB.Where("id IN ?", []int{openCodeGoID, fallbackID}).Delete(&Channel{}).Error)
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
		if previousMemoryCacheEnabled {
			InitChannelCache()
		}
	})
	return
}

func TestGetRandomSatisfiedChannelFiltersOpenCodeGoBeforePrioritySelection(t *testing.T) {
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
		if previousMemoryCacheEnabled {
			InitChannelCache()
		}
	})

	group, modelName, openCodeGoID, fallbackID := seedEndpointSelectionChannels(t)

	for _, memoryCacheEnabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("memory_cache_%t", memoryCacheEnabled), func(t *testing.T) {
			common.MemoryCacheEnabled = memoryCacheEnabled
			if memoryCacheEnabled {
				InitChannelCache()
			}

			for _, path := range openCodeGoUnsupportedSelectionPaths {
				channel, err := GetRandomSatisfiedChannel(group, modelName, 0, path)
				require.NoError(t, err, path)
				require.NotNil(t, channel, path)
				assert.Equal(t, fallbackID, channel.Id, path)
			}

			for _, path := range openCodeGoSupportedSelectionPaths {
				channel, err := GetRandomSatisfiedChannel(group, modelName, 0, path)
				require.NoError(t, err, path)
				require.NotNil(t, channel, path)
				assert.Equal(t, openCodeGoID, channel.Id, path)
			}
		})
	}
}
