package common

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
)

func TestSeedDanceOnlyAdvertisesOpenAIVideo(t *testing.T) {
	assert.Equal(t,
		[]constant.EndpointType{constant.EndpointTypeOpenAIVideo},
		GetEndpointTypesByChannelType(
			constant.ChannelTypeSeedDance,
			"seedance-uncensored",
		),
	)
}
