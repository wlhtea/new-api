package controller

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel/opencodego"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreflightOpenCodeRequestRejectsGLM53WithStableRuleAndEndpointEnvelope(t *testing.T) {
	endpoints := []struct {
		name   string
		path   string
		format types.RelayFormat
	}{
		{name: "messages", path: "/v1/messages", format: types.RelayFormatClaude},
		{name: "chat", path: "/v1/chat/completions", format: types.RelayFormatOpenAI},
		{name: "responses", path: "/v1/responses", format: types.RelayFormatOpenAIResponses},
	}
	models := []struct {
		name    string
		model   string
		mapping string
	}{
		{name: "exact", model: "glm-5.3"},
		{name: "alias", model: "client-glm", mapping: `{"client-glm":"glm-5.3"}`},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, endpoint := range endpoints {
			for _, modelCase := range models {
				name := fmt.Sprintf("type-%d/%s/%s", channelType, endpoint.name, modelCase.name)
				t.Run(name, func(t *testing.T) {
					body := openCodeControllerPreflightBody(t, endpoint.format, modelCase.model, true)
					c, info, recorder := newOpenCodeControllerPreflightFixture(
						t,
						channelType,
						endpoint.path,
						endpoint.format,
						modelCase.model,
						modelCase.mapping,
						body,
					)

					relayErr := preflightOpenCodeRequest(c, info)
					require.NotNil(t, relayErr)
					assert.Equal(t, http.StatusBadRequest, relayErr.StatusCode)
					assert.Equal(t, types.ErrorOriginLocalValidation, relayErr.Provenance().Origin)
					assert.Equal(t, helper.OpenCodeGLM53ThinkingDisabledRule, relayErr.Provenance().Subtype)
					preflightErr, ok := opencodego.AsRequestPreflightError(relayErr)
					require.True(t, ok)
					assert.Equal(t, helper.OpenCodeGLM53ThinkingDisabledRule, preflightErr.RuleID)
					assert.Equal(t, helper.OpenCodeCapabilityStage, preflightErr.StageID)
					ruleID, stageID, found := openCodeRequestPreflightRejection(c)
					require.True(t, found)
					assert.Equal(t, preflightErr.RuleID, ruleID)
					assert.Equal(t, preflightErr.StageID, stageID)
					_, planFound, planErr := opencodego.GetRequestPreflightPlan(c)
					require.NoError(t, planErr)
					assert.False(t, planFound)

					renderRelayError(c, endpoint.format, nil, relayErr, "local-request-id")
					assert.Equal(t, http.StatusBadRequest, recorder.Code)
					assert.Equal(t, "local-request-id", recorder.Header().Get(common.RequestIdKey))
					assert.Contains(t, recorder.Body.String(), constant.OpenCodeGoPublicInvalidRequestMessage)
					assert.NotContains(t, recorder.Body.String(), helper.OpenCodeGLM53ThinkingDisabledPublicMessage)
					assert.NotContains(t, recorder.Body.String(), "local-request-id")
					assert.Contains(t, recorder.Body.String(), `"type":"invalid_request_error"`)
					if endpoint.format == types.RelayFormatClaude {
						assert.NotContains(t, recorder.Body.String(), `"code"`)
					} else {
						assert.Contains(t, recorder.Body.String(), `"code":"invalid_request_error"`)
					}
				})
			}
		}
	}
}

func TestPreflightOpenCodeRequestStoresUnknownGLM52PlanForBothChannelTypes(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			body := openCodeControllerPreflightBody(t, types.RelayFormatOpenAI, "glm-5.2", true)
			c, info, _ := newOpenCodeControllerPreflightFixture(
				t,
				channelType,
				"/v1/chat/completions",
				types.RelayFormatOpenAI,
				"glm-5.2",
				"",
				body,
			)

			require.Nil(t, preflightOpenCodeRequest(c, info))
			plan, found, err := opencodego.GetRequestPreflightPlan(c)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, channelType, plan.ChannelType)
			assert.Equal(t, "glm-5.2", plan.FinalModel)
			assert.Equal(t, opencodego.ProtocolChat, plan.FinalProtocol)
			_, _, rejected := openCodeRequestPreflightRejection(c)
			assert.False(t, rejected)
		})
	}
}

func newOpenCodeControllerPreflightFixture(
	t *testing.T,
	channelType int,
	path string,
	format types.RelayFormat,
	modelName string,
	mapping string,
	body []byte,
) (*gin.Context, *relaycommon.RelayInfo, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	storage, err := common.CreateBodyStorage(body)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })
	c.Set(common.KeyBodyStorage, storage)
	common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
	common.SetContextKey(c, constant.ContextKeyChannelId, 7000+channelType)
	common.SetContextKey(c, constant.ContextKeyChannelModelMapping, mapping)
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{})
	common.SetContextKey(c, constant.ContextKeyOriginalModel, modelName)
	c.Set(common.RequestIdKey, "local-request-id")

	request, err := helper.GetAndValidateRequest(c, format)
	require.NoError(t, err)
	info, err := relaycommon.GenRelayInfo(c, format, request, nil)
	require.NoError(t, err)
	return c, info, recorder
}

func openCodeControllerPreflightBody(t *testing.T, format types.RelayFormat, modelName string, disabled bool) []byte {
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
		t.Fatalf("unsupported relay format %q", format)
	}
	if disabled {
		body["thinking"] = map[string]any{"type": "disabled"}
	}
	encoded, err := common.Marshal(body)
	require.NoError(t, err)
	return encoded
}
