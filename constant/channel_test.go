package constant

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSeedDanceChannelConstantContract(t *testing.T) {
	require.Equal(t, 59, ChannelTypeSeedDance)
	require.Equal(t, 60, ChannelTypeSub2API)
	require.Equal(t, 61, ChannelTypeNewAPI)
	require.Equal(t, 62, ChannelTypeOpenCodeGo)
	require.Equal(t, 63, ChannelTypeOpenCodeAPIKey)
	assert.Equal(t, 64, ChannelTypeDummy)
	assert.Len(t, ChannelBaseURLs, ChannelTypeDummy)
	assert.Equal(t,
		"http://alb-o13xqj8f2cpjsa67ym.ap-northeast-1.alb.aliyuncsslbintl.com/v1/public_api/m-predict/polar4ai-i2v",
		ChannelBaseURLs[ChannelTypeSeedDance],
	)
	assert.Equal(t, "SeedDance", GetChannelTypeName(ChannelTypeSeedDance))
	assert.Equal(t, "https://opencode.ai/zen/go/v1", ChannelBaseURLs[ChannelTypeOpenCodeGo])
	assert.Equal(t, "OpenCode Go", GetChannelTypeName(ChannelTypeOpenCodeGo))
	assert.Equal(t, "https://opencode.ai/zen/go/v1", ChannelBaseURLs[ChannelTypeOpenCodeAPIKey])
	assert.Equal(t, "OpenCode API Key", GetChannelTypeName(ChannelTypeOpenCodeAPIKey))
	assert.True(t, IsOpenCodeChannelType(ChannelTypeOpenCodeGo))
	assert.True(t, IsOpenCodeChannelType(ChannelTypeOpenCodeAPIKey))
	assert.False(t, IsOpenCodeChannelType(ChannelTypeOpenAI))
	assert.True(t, IsOpenCodeGoPoolChannelType(ChannelTypeOpenCodeGo))
	assert.False(t, IsOpenCodeGoPoolChannelType(ChannelTypeOpenCodeAPIKey))
	require.Equal(t, APITypeOpenCodeGo+1, APITypeOpenCodeAPIKey)
	assert.Equal(t, APITypeOpenCodeAPIKey+1, APITypeDummy)
}
