package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRelayInvalidRequestErrorMarksTypedValidationForFixedOpenCode400(t *testing.T) {
	internal := newRelayInvalidRequestError(&helper.ClientRequestValidationError{
		StatusCode: http.StatusBadRequest,
		Message:    "private malformed request detail",
	})
	require.Equal(t, types.ErrorOriginLocalValidation, internal.Provenance().Origin)
	assert.Equal(t, "request.body.invalid", internal.Provenance().Subtype)

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		projected := service.PublicOpenCodeGoRelayError(channelType, internal)
		require.NotSame(t, internal, projected)
		assert.Equal(t, http.StatusBadRequest, projected.StatusCode)
		assert.Equal(t, constant.OpenCodeGoPublicInvalidRequestMessage, projected.Error())
		assert.NotContains(t, projected.Error(), "private")
	}
}

func TestRenderRelayErrorUsesExactOpenCodePublicContract(t *testing.T) {
	type errorCase struct {
		name        string
		newInternal func() *types.NewAPIError
		wantStatus  int
		wantMessage string
		wantCode    string
	}
	errorCases := []errorCase{
		{
			name: "local-validation",
			newInternal: func() *types.NewAPIError {
				return types.NewOpenAIError(
					errors.New("private rejected selector value"),
					types.ErrorCodeInvalidRequest,
					http.StatusUnprocessableEntity,
					types.ErrOptionWithProvenance(types.ErrorProvenance{
						Origin:  types.ErrorOriginLocalValidation,
						Subtype: "request.test.client-invalid",
					}),
				)
			},
			wantStatus:  http.StatusBadRequest,
			wantMessage: constant.OpenCodeGoPublicInvalidRequestMessage,
			wantCode:    constant.OpenCodeGoPublicInvalidRequestCode,
		},
		{
			name: "raw-upstream-400",
			newInternal: func() *types.NewAPIError {
				return service.MarkOpenCodeGoUpstreamRelayErrorWithStatus(
					types.NewOpenAIError(
						errors.New("private upstream 400 body"),
						types.ErrorCodeBadResponse,
						http.StatusServiceUnavailable,
					),
					http.StatusBadRequest,
				)
			},
			wantStatus:  http.StatusBadRequest,
			wantMessage: constant.OpenCodeGoPublicInvalidRequestMessage,
			wantCode:    constant.OpenCodeGoPublicInvalidRequestCode,
		},
		{
			name: "raw-upstream-422",
			newInternal: func() *types.NewAPIError {
				return service.MarkOpenCodeGoUpstreamRelayErrorWithStatus(
					types.NewOpenAIError(
						errors.New("private upstream 422 body"),
						types.ErrorCodeBadResponse,
						http.StatusServiceUnavailable,
					),
					http.StatusUnprocessableEntity,
				)
			},
			wantStatus:  http.StatusBadRequest,
			wantMessage: constant.OpenCodeGoPublicInvalidRequestMessage,
			wantCode:    constant.OpenCodeGoPublicInvalidRequestCode,
		},
		{
			name: "gateway-config",
			newInternal: func() *types.NewAPIError {
				return types.NewOpenAIError(
					errors.New("private operator configuration detail"),
					types.ErrorCodeChannelParamOverrideInvalid,
					http.StatusBadRequest,
					types.ErrOptionWithProvenance(types.ErrorProvenance{
						Origin:  types.ErrorOriginGatewayConfig,
						Subtype: "request.test.operator-config",
					}),
				)
			},
			wantStatus:  http.StatusServiceUnavailable,
			wantMessage: constant.OpenCodeGoPublicGatewayConfigMessage,
			wantCode:    constant.OpenCodeGoPublicServiceUnavailableCode,
		},
		{
			name: "capability-unknown",
			newInternal: func() *types.NewAPIError {
				return types.NewOpenAIError(
					errors.New("private capability revision and model detail"),
					types.ErrorCodeGetChannelFailed,
					http.StatusInternalServerError,
					types.ErrOptionWithProvenance(types.ErrorProvenance{
						Origin:  types.ErrorOriginGatewayDependency,
						Subtype: "request.test.capability-authority",
					}),
				)
			},
			wantStatus:  http.StatusServiceUnavailable,
			wantMessage: constant.OpenCodeGoPublicCapabilityMessage,
			wantCode:    constant.OpenCodeGoPublicServiceUnavailableCode,
		},
	}
	endpoints := []struct {
		name   string
		path   string
		format types.RelayFormat
	}{
		{name: "messages", path: "/v1/messages", format: types.RelayFormatClaude},
		{name: "chat", path: "/v1/chat/completions", format: types.RelayFormatOpenAI},
		{name: "responses", path: "/v1/responses", format: types.RelayFormatOpenAIResponses},
	}
	streamStates := []struct {
		name string
		body string
	}{
		{name: "absent", body: `{}`},
		{name: "false", body: `{"stream":false}`},
		{name: "true", body: `{"stream":true}`},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, endpoint := range endpoints {
			for _, stream := range streamStates {
				for _, test := range errorCases {
					name := fmt.Sprintf("type-%d/%s/stream-%s/%s", channelType, endpoint.name, stream.name, test.name)
					t.Run(name, func(t *testing.T) {
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Request = httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(stream.body))
						common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
						for header, value := range map[string]string{
							"Content-Type":          "text/event-stream",
							"Content-Length":        "999",
							"Cache-Control":         "no-cache",
							"Connection":            "keep-alive",
							"Transfer-Encoding":     "chunked",
							"X-Accel-Buffering":     "no",
							"X-Codex-Turn-State":    "private-turn-state",
							"X-Reasoning-Included":  "private-reasoning-state",
							"X-Upstream-Secret":     "private-upstream-value",
							"X-Upstream-Request-Id": "private-upstream-id",
						} {
							c.Writer.Header().Set(header, value)
						}

						internal := test.newInternal()
						originalMessage := internal.Error()
						renderRelayError(c, endpoint.format, nil, internal, "local-request-id")

						require.Equal(t, test.wantStatus, recorder.Code, recorder.Body.String())
						assert.Equal(t, "application/json; charset=utf-8", recorder.Header().Get("Content-Type"))
						assert.Equal(t, "local-request-id", recorder.Header().Get(common.RequestIdKey))
						for _, header := range []string{
							"Content-Length", "Cache-Control", "Connection", "Transfer-Encoding",
							"X-Accel-Buffering", "X-Codex-Turn-State", "X-Reasoning-Included",
							"X-Upstream-Secret", "X-Upstream-Request-Id",
						} {
							assert.Empty(t, recorder.Header().Values(header), header)
						}
						assert.NotContains(t, recorder.Body.String(), "private")
						assert.NotContains(t, recorder.Body.String(), "local-request-id")
						assert.Equal(t, originalMessage, internal.Error())

						var actual map[string]any
						require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &actual))
						assert.Equal(t, openCodePublicErrorEnvelope(endpoint.format, test.wantMessage, test.wantCode), actual)
					})
				}
			}
		}
	}
}

func openCodePublicErrorEnvelope(format types.RelayFormat, message, code string) map[string]any {
	if format == types.RelayFormatClaude {
		return map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    code,
				"message": message,
			},
		}
	}
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    code,
			"param":   "",
			"code":    code,
		},
	}
}
