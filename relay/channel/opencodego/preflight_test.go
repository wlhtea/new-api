package opencodego

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type requestPreflightEndpoint struct {
	name   string
	path   string
	format types.RelayFormat
}

var requestPreflightEndpoints = []requestPreflightEndpoint{
	{name: "messages", path: "/v1/messages", format: types.RelayFormatClaude},
	{name: "chat", path: "/v1/chat/completions", format: types.RelayFormatOpenAI},
	{name: "responses", path: "/v1/responses", format: types.RelayFormatOpenAIResponses},
}

func TestBuildRequestPreflightPlanRejectsExactGLM53PolicyBeforeAttempt(t *testing.T) {
	streamStates := []struct {
		name  string
		value *bool
	}{
		{name: "absent"},
		{name: "false", value: common.GetPointer(false)},
		{name: "true", value: common.GetPointer(true)},
	}
	modelCases := []struct {
		name    string
		model   string
		mapping string
	}{
		{name: "exact", model: "glm-5.3"},
		{name: "alias", model: "customer-glm", mapping: `{"customer-glm":"glm-5.3"}`},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, endpoint := range requestPreflightEndpoints {
			for _, stream := range streamStates {
				for _, modelCase := range modelCases {
					name := fmt.Sprintf("type-%d/%s/stream-%s/%s", channelType, endpoint.name, stream.name, modelCase.name)
					t.Run(name, func(t *testing.T) {
						c, info, _ := newRequestPreflightFixture(
							t,
							channelType,
							endpoint,
							modelCase.model,
							modelCase.mapping,
							nil,
							stream.value,
							true,
						)

						plan, err := BuildRequestPreflightPlan(c, info)
						require.Error(t, err)
						assert.Equal(t, RequestPreflightPlan{}, plan)
						preflightErr, ok := AsRequestPreflightError(err)
						require.True(t, ok)
						assert.Equal(t, http.StatusBadRequest, preflightErr.StatusCode)
						assert.Equal(t, types.ErrorOriginLocalValidation, preflightErr.Origin)
						assert.Equal(t, helper.OpenCodeGLM53ThinkingDisabledRule, preflightErr.RuleID)
						assert.Equal(t, helper.OpenCodeCapabilityStage, preflightErr.StageID)

						// Matched control: same selected channel, model mapping, endpoint,
						// and stream state, with only the prohibited value removed.
						controlContext, controlInfo, _ := newRequestPreflightFixture(
							t,
							channelType,
							endpoint,
							modelCase.model,
							modelCase.mapping,
							nil,
							stream.value,
							false,
						)
						controlPlan, controlErr := BuildRequestPreflightPlan(controlContext, controlInfo)
						require.NoError(t, controlErr)
						assert.Equal(t, channelType, controlPlan.ChannelType)
						assert.Equal(t, "glm-5.3", controlPlan.FinalModel)
						assert.Equal(t, ProtocolChat, controlPlan.FinalProtocol)
					})
				}
			}
		}
	}
}

func TestBuildRequestPreflightPlanDoesNotInventUnsupportedModelClaims(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, endpoint := range requestPreflightEndpoints {
			for _, modelName := range []string{"glm-5.2", "glm-5.30"} {
				name := fmt.Sprintf("type-%d/%s/%s", channelType, endpoint.name, modelName)
				t.Run(name, func(t *testing.T) {
					// Responses has no top-level thinking field and no raw finalizer
					// mapping for it. Its model-policy near-miss control therefore
					// uses only fields that the endpoint contract can represent.
					includeThinking := endpoint.format != types.RelayFormatOpenAIResponses
					c, info, _ := newRequestPreflightFixture(
						t,
						channelType,
						endpoint,
						modelName,
						"",
						nil,
						nil,
						includeThinking,
					)

					plan, err := BuildRequestPreflightPlan(c, info)
					require.NoError(t, err)
					assert.Equal(t, modelName, plan.FinalModel)
					assert.Equal(t, ProtocolChat, plan.FinalProtocol)
				})
			}
		}
	}
}

func TestBuildRequestPreflightPlanRejectsHistoricalSerializerDeletions(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		field    string
		value    any
		wantRule string
	}{
		{
			name:     "non-Qwen thinking budget",
			model:    "glm-5.2",
			field:    "thinking_budget",
			value:    128,
			wantRule: RequestContractThinkingBudgetRule,
		},
		{
			name:     "Kimi non-default temperature",
			model:    "kimi-k2.7-code",
			field:    "temperature",
			value:    0.6,
			wantRule: RequestContractKimiTemperatureRule,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := common.Marshal(map[string]any{
				"model":    test.model,
				"messages": []any{map[string]any{"role": "user", "content": "hello"}},
				test.field: test.value,
			})
			require.NoError(t, err)
			c, info, _ := newSameProtocolFinalizerFixture(
				t,
				types.RelayFormatOpenAI,
				"/v1/chat/completions",
				body,
			)

			_, err = BuildRequestPreflightPlan(c, info)
			require.Error(t, err)
			preflightErr, ok := AsRequestPreflightError(err)
			require.True(t, ok)
			assert.Equal(t, http.StatusBadRequest, preflightErr.StatusCode)
			assert.Equal(t, types.ErrorOriginLocalValidation, preflightErr.Origin)
			assert.Equal(t, test.wantRule, preflightErr.RuleID)
			assert.Equal(t, RequestContractPreflightStage, preflightErr.StageID)
		})
	}
}

func TestBuildRequestPreflightPlanRejectsUnmappedChatThinkingForResponsesTarget(t *testing.T) {
	config := &dto.OpenCodeGoConfig{
		ModelProtocols: map[string]string{"glm-5.3": dto.OpenCodeGoProtocolResponses},
	}
	c, info, _ := newRequestPreflightFixture(
		t,
		constant.ChannelTypeOpenCodeAPIKey,
		requestPreflightEndpoints[1],
		"glm-5.3",
		"",
		config,
		nil,
		true,
	)

	_, err := BuildRequestPreflightPlan(c, info)
	require.Error(t, err)
	preflightErr, ok := AsRequestPreflightError(err)
	require.True(t, ok)
	assert.Equal(t, http.StatusBadRequest, preflightErr.StatusCode)
	assert.Equal(t, types.ErrorOriginLocalValidation, preflightErr.Origin)
	assert.Equal(t, RequestContractUnmappedPathRule, preflightErr.RuleID)
	assert.Equal(t, RequestContractPreflightStage, preflightErr.StageID)

	controlContext, controlInfo, _ := newRequestPreflightFixture(
		t,
		constant.ChannelTypeOpenCodeAPIKey,
		requestPreflightEndpoints[1],
		"glm-5.3",
		"",
		config,
		nil,
		false,
	)
	controlPlan, controlErr := BuildRequestPreflightPlan(controlContext, controlInfo)
	require.NoError(t, controlErr)
	assert.Equal(t, ProtocolResponses, controlPlan.FinalProtocol)
	assert.Equal(t, ProtocolSourceExactOverride, controlPlan.ProtocolSource)
}

func TestBuildRequestPreflightPlanFailsClosedOnInvalidPersistedProtocolConfig(t *testing.T) {
	config := &dto.OpenCodeGoConfig{
		ModelProtocols: map[string]string{"bad[": dto.OpenCodeGoProtocolChat},
	}
	c, info, _ := newRequestPreflightFixture(
		t,
		constant.ChannelTypeOpenCodeGo,
		requestPreflightEndpoints[1],
		"glm-5.2",
		"",
		config,
		nil,
		false,
	)

	_, err := BuildRequestPreflightPlan(c, info)
	require.Error(t, err)
	preflightErr, ok := AsRequestPreflightError(err)
	require.True(t, ok)
	assert.Equal(t, types.ErrorOriginGatewayConfig, preflightErr.Origin)
	assert.Equal(t, PreflightProtocolConfigInvalidRule, preflightErr.RuleID)
	assert.Equal(t, PreflightRoutingStage, preflightErr.StageID)
}

func TestAssertRequestPreflightPlanDetectsAttemptDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*gin.Context, *relaycommon.RelayInfo, *dto.GeneralOpenAIRequest)
	}{
		{
			name: "channel id",
			mutate: func(_ *gin.Context, info *relaycommon.RelayInfo, _ *dto.GeneralOpenAIRequest) {
				info.ChannelId++
			},
		},
		{
			name: "selection group",
			mutate: func(_ *gin.Context, info *relaycommon.RelayInfo, _ *dto.GeneralOpenAIRequest) {
				info.SelectionGroup = "retry-group"
			},
		},
		{
			name: "mapped model",
			mutate: func(_ *gin.Context, info *relaycommon.RelayInfo, _ *dto.GeneralOpenAIRequest) {
				info.UpstreamModelName = "glm-5.30"
			},
		},
		{
			name: "mapping config",
			mutate: func(c *gin.Context, _ *relaycommon.RelayInfo, _ *dto.GeneralOpenAIRequest) {
				common.SetContextKey(c, constant.ContextKeyChannelModelMapping, `{"alias":"glm-5.30"}`)
			},
		},
		{
			name: "protocol config",
			mutate: func(_ *gin.Context, info *relaycommon.RelayInfo, _ *dto.GeneralOpenAIRequest) {
				info.ChannelOtherSettings.OpenCodeGo = &dto.OpenCodeGoConfig{
					ModelProtocols: map[string]string{"glm-5.2": dto.OpenCodeGoProtocolMessages},
				}
			},
		},
		{
			name: "dynamic route",
			mutate: func(_ *gin.Context, _ *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) {
				request.Tools = []dto.ToolCallRequest{{Type: "function", Function: dto.FunctionRequest{Name: "lookup"}}}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, info, rawRequest := newRequestPreflightFixture(
				t,
				constant.ChannelTypeOpenCodeAPIKey,
				requestPreflightEndpoints[1],
				"glm-5.2",
				"",
				nil,
				nil,
				false,
			)
			plan, err := BuildRequestPreflightPlan(c, info)
			require.NoError(t, err)
			require.NoError(t, StoreRequestPreflightPlan(c, plan))

			info.InitChannelMeta(c)
			request, err := common.DeepCopy(rawRequest.(*dto.GeneralOpenAIRequest))
			require.NoError(t, err)
			require.NoError(t, helper.ModelMappedHelper(c, info, request))
			asserted, found, err := AssertRequestPreflightPlan(c, info, request)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, plan, asserted)

			test.mutate(c, info, request)
			_, found, err = AssertRequestPreflightPlan(c, info, request)
			require.True(t, found)
			require.Error(t, err)
			preflightErr, ok := AsRequestPreflightError(err)
			require.True(t, ok)
			assert.Equal(t, types.ErrorOriginGatewayInvariant, preflightErr.Origin)
			assert.Equal(t, PreflightPlanMismatchRule, preflightErr.RuleID)
			assert.Equal(t, PreflightAssertionStage, preflightErr.StageID)
		})
	}
}

func TestRequestPreflightPlanRegistryKeysSameChannelBySelectionGroup(t *testing.T) {
	c, info, _ := newRequestPreflightFixture(
		t,
		constant.ChannelTypeOpenCodeAPIKey,
		requestPreflightEndpoints[1],
		"glm-5.2",
		"",
		nil,
		nil,
		false,
	)
	common.SetContextKey(c, constant.ContextKeyAutoGroup, "group-a")
	planA, err := BuildRequestPreflightPlan(c, info)
	require.NoError(t, err)

	common.SetContextKey(c, constant.ContextKeyAutoGroup, "group-b")
	planB, err := BuildRequestPreflightPlan(c, info)
	require.NoError(t, err)
	require.Equal(t, planA.ChannelID, planB.ChannelID)
	require.NotEqual(t, planA.Key(), planB.Key())
	require.NoError(t, StoreRequestPreflightPlans(c, []RequestPreflightPlan{planA, planB}))

	actualA, found, err := GetRequestPreflightPlanForSelection(c, "group-a", planA.ChannelID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, planA, actualA)

	actualB, found, err := GetRequestPreflightPlanForSelection(c, "group-b", planB.ChannelID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, planB, actualB)

	_, found, err = GetRequestPreflightPlanForSelection(c, "group-c", planA.ChannelID)
	require.NoError(t, err)
	assert.False(t, found)
}

func newRequestPreflightFixture(
	t *testing.T,
	channelType int,
	endpoint requestPreflightEndpoint,
	modelName string,
	mapping string,
	config *dto.OpenCodeGoConfig,
	stream *bool,
	includeDisabledThinking bool,
) (*gin.Context, *relaycommon.RelayInfo, dto.Request) {
	t.Helper()
	body := requestPreflightBody(t, endpoint.format, modelName, stream, includeDisabledThinking)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, endpoint.path, bytes.NewReader(body))
	storage, err := common.CreateBodyStorage(body)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })
	c.Set(common.KeyBodyStorage, storage)
	common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
	common.SetContextKey(c, constant.ContextKeyChannelId, 6200+channelType)
	common.SetContextKey(c, constant.ContextKeyChannelModelMapping, mapping)
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{OpenCodeGo: config})
	common.SetContextKey(c, constant.ContextKeyOriginalModel, modelName)

	request, err := helper.GetAndValidateRequest(c, endpoint.format)
	require.NoError(t, err)
	info, err := relaycommon.GenRelayInfo(c, endpoint.format, request, nil)
	require.NoError(t, err)
	return c, info, request
}

func requestPreflightBody(
	t *testing.T,
	format types.RelayFormat,
	modelName string,
	stream *bool,
	includeDisabledThinking bool,
) []byte {
	t.Helper()
	body := map[string]any{"model": modelName}
	switch format {
	case types.RelayFormatClaude:
		body["max_tokens"] = 16
		body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
	case types.RelayFormatOpenAI:
		body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
	case types.RelayFormatOpenAIResponses:
		body["input"] = "hello"
	default:
		t.Fatalf("unsupported test relay format %q", format)
	}
	if stream != nil {
		body["stream"] = *stream
	}
	if includeDisabledThinking {
		body["thinking"] = map[string]any{"type": "disabled"}
	}
	encoded, err := common.Marshal(body)
	require.NoError(t, err)
	return encoded
}
