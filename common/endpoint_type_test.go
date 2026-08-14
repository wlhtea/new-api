package common

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
)

func TestSeedDanceOnlyAdvertisesOpenAIVideo(t *testing.T) {
	expected := []constant.EndpointType{constant.EndpointTypeOpenAIVideo}

	assert.Equal(t, expected,
		GetEndpointTypesByChannelType(constant.ChannelTypeSeedDance, "seedance-uncensored"),
	)
	assert.Equal(t, expected,
		GetEndpointTypesByChannelType(constant.ChannelTypeSeedDance, "flux-1"),
	)
}

func TestOpenCodeChannelTypesAdvertiseOnlySupportedInferenceEndpoints(t *testing.T) {
	expected := []constant.EndpointType{
		constant.EndpointTypeOpenAI,
		constant.EndpointTypeOpenAIResponse,
		constant.EndpointTypeAnthropic,
	}

	assert.Equal(t, expected, GetEndpointTypesByChannelType(constant.ChannelTypeOpenCodeGo, "glm-5.2"))
	assert.Equal(t, expected, GetEndpointTypesByChannelType(constant.ChannelTypeOpenCodeAPIKey, "glm-5.2"))
}

func TestOpenCodeAPIKeyChannelMapsToDedicatedAPIType(t *testing.T) {
	apiType, ok := ChannelType2APIType(constant.ChannelTypeOpenCodeAPIKey)
	assert.True(t, ok)
	assert.Equal(t, constant.APITypeOpenCodeAPIKey, apiType)
}
