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
