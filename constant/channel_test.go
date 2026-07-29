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
	assert.Equal(t, 62, ChannelTypeDummy)
	assert.Len(t, ChannelBaseURLs, ChannelTypeDummy)
	assert.Equal(t,
		"http://alb-o13xqj8f2cpjsa67ym.ap-northeast-1.alb.aliyuncsslbintl.com/v1/public_api/m-predict/polar4ai-i2v",
		ChannelBaseURLs[ChannelTypeSeedDance],
	)
	assert.Equal(t, "SeedDance", GetChannelTypeName(ChannelTypeSeedDance))
}
