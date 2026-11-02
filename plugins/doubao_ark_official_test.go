package plugins_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	taskplugin "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoubaoArkOfficialMatchesDoubaoCapabilitiesAndUsage(t *testing.T) {
	legacySource, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	officialSource, err := builtinplugins.Source("doubao_ark_official")
	require.NoError(t, err)

	legacyRegistry := jsplugin.NewRegistry()
	legacy, err := legacyRegistry.RegisterFactory(legacySource, jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)
	officialRegistry := jsplugin.NewRegistry()
	official, err := officialRegistry.RegisterFactory(officialSource, jsplugin.Options{Key: "doubao_ark_official"})
	require.NoError(t, err)

	assert.Equal(t, "doubao_ark_official", official.Meta.Key)
	assert.Equal(t, "1.0.0", official.Meta.Version)
	assert.Equal(t, legacy.Meta.Models, official.Meta.Models)
	assert.Empty(t, official.Meta.ChannelTypes, "the independent plugin is bound explicitly on a type-61 channel")
	assert.Equal(t, legacy.Meta.RequiredCapabilities, official.Meta.RequiredCapabilities)
	assert.Equal(t, legacy.Meta.UsageSchema, official.Meta.UsageSchema)
	assert.Equal(t, legacy.Meta.UsageExamples, official.Meta.UsageExamples)
	assert.Equal(t, legacy.Meta.UsageProfiles, official.Meta.UsageProfiles)

	newUsageAdaptor := func(plugin *jsplugin.LoadedPlugin, route string) (*taskplugin.TaskAdaptor, *gin.Context, *relaycommon.RelayInfo) {
		info := &relaycommon.RelayInfo{
			ChannelMeta: &relaycommon.ChannelMeta{
				ChannelBaseUrl: doubaoBaseURL, UpstreamModelName: doubaoNativeTestModelName,
			},
			OriginModelName: doubaoNativeTestModelName,
			TaskRelayInfo: &relaycommon.TaskRelayInfo{
				PublicTaskID: "task_usage_public", Action: "text_to_video",
			},
		}
		adaptor := taskplugin.New(plugin)
		adaptor.Init(info)
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, route, nil)
		ctx.Set("task_request", map[string]any{
			"model": doubaoNativeTestModelName, "prompt": "a lighthouse", "seconds": 5,
			"metadata": map[string]any{"resolution": "1080p", "content": []any{}},
		})
		require.Nil(t, adaptor.ValidateRequestAndSetAction(ctx, info))
		return adaptor, ctx, info
	}

	legacyAdaptor, legacyContext, legacyInfo := newUsageAdaptor(legacy, "/doubao/api/v3/contents/generations/tasks")
	officialAdaptor, officialContext, officialInfo := newUsageAdaptor(official, "/doubao_ark_official/api/v3/contents/generations/tasks")
	legacyFacts, err := legacyAdaptor.ExtractUsageFactsValidated(legacyContext, legacyInfo)
	require.NoError(t, err)
	officialFacts, err := officialAdaptor.ExtractUsageFactsValidated(officialContext, officialInfo)
	require.NoError(t, err)
	assert.Equal(t, legacyFacts, officialFacts)
	assert.Equal(t, "1080p", officialFacts["resolution"])
	assert.Equal(t, "none", officialFacts["video_input"])
	assert.Positive(t, officialFacts["tokens"])
}

const doubaoNativeTestModelName = "doubao-seedance-2-0-260128"
