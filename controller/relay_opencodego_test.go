package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderRelayErrorUsesFixedRawUpstream400ContractForBothOpenCodeTypes(t *testing.T) {
	oldRetryRanges := append([]operation_setting.StatusCodeRange(nil), operation_setting.AutomaticRetryStatusCodeRanges...)
	oldDisableRanges := append([]operation_setting.StatusCodeRange(nil), operation_setting.AutomaticDisableStatusCodeRanges...)
	oldDisableEnabled := common.AutomaticDisableChannelEnabled
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 400, End: 422}}
	operation_setting.AutomaticDisableStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 400, End: 422}}
	common.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() {
		operation_setting.AutomaticRetryStatusCodeRanges = oldRetryRanges
		operation_setting.AutomaticDisableStatusCodeRanges = oldDisableRanges
		common.AutomaticDisableChannelEnabled = oldDisableEnabled
	})

	endpoints := []struct {
		path   string
		format types.RelayFormat
	}{
		{path: "/v1/messages", format: types.RelayFormatClaude},
		{path: "/v1/chat/completions", format: types.RelayFormatOpenAI},
		{path: "/v1/responses", format: types.RelayFormatOpenAIResponses},
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
				for _, rawStatus := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
					name := fmt.Sprintf("type-%d:%s:stream-%s:raw-%d", channelType, endpoint.path, stream.name, rawStatus)
					t.Run(name, func(t *testing.T) {
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Request = httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(stream.body))
						if stream.name == "true" {
							c.Request.Header.Set("Accept", "text/event-stream")
						}
						common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
						for header, value := range map[string]string{
							"Retry-After":           "workspace=private",
							"Location":              "http://internal.invalid/private",
							"Server":                "private-provider",
							"Set-Cookie":            "session=private",
							"WWW-Authenticate":      "Bearer private",
							"X-Upstream-Request-Id": "upstream-private-id",
							"X-Upstream-Secret":     "private-value",
						} {
							c.Writer.Header().Set(header, value)
						}

						internal := service.MarkOpenCodeGoUpstreamRelayErrorWithStatus(types.WithOpenAIError(types.OpenAIError{
							Message:  "credential=private endpoint=http://internal.invalid workspace=private",
							Type:     "provider_private_type",
							Code:     "provider_private_code",
							Param:    "private_param",
							Metadata: json.RawMessage(`{"private":"metadata"}`),
						}, rawStatus), rawStatus)
						service.ResetStatusCode(internal, fmt.Sprintf(`{"%d":503}`, rawStatus))
						originalMessage := internal.Error()

						renderRelayError(c, endpoint.format, nil, internal, "local-request-id")

						assert.Equal(t, http.StatusBadRequest, recorder.Code)
						assert.Equal(t, "local-request-id", recorder.Header().Get(common.RequestIdKey))
						for _, header := range []string{"Retry-After", "Location", "Server", "Set-Cookie", "WWW-Authenticate", "X-Upstream-Request-Id", "X-Upstream-Secret"} {
							assert.Empty(t, recorder.Header().Values(header), header)
						}
						assert.NotContains(t, recorder.Body.String(), "local-request-id")
						assert.NotContains(t, recorder.Body.String(), "private")
						var actual map[string]any
						require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &actual))
						if endpoint.format == types.RelayFormatClaude {
							assert.Equal(t, map[string]any{
								"type": "error",
								"error": map[string]any{
									"type":    constant.OpenCodeGoPublicInvalidRequestCode,
									"message": constant.OpenCodeGoPublicInvalidRequestMessage,
								},
							}, actual)
						} else {
							assert.Equal(t, map[string]any{
								"error": map[string]any{
									"message": constant.OpenCodeGoPublicInvalidRequestMessage,
									"type":    constant.OpenCodeGoPublicInvalidRequestCode,
									"param":   "",
									"code":    constant.OpenCodeGoPublicInvalidRequestCode,
								},
							}, actual)
						}
						assert.Equal(t, originalMessage, internal.Error())
						assert.Equal(t, http.StatusServiceUnavailable, internal.StatusCode)
						assert.False(t, shouldRetry(c, internal, 3))
						assert.False(t, shouldDisableWholeChannel(channelType, internal))
					})
				}
			}
		}
	}
}

func TestOpenCodeAPIKeyRetryResetDiscardsResponsesSSEAttemptBeforeChatJSON(t *testing.T) {
	start := time.Now()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set(common.RequestIdKey, "local-request-id")
	c.Header(common.RequestIdKey, "local-request-id")
	common.SetContextKey(c, constant.ContextKeyIsStream, false)
	common.SetContextKey(c, constant.ContextKeyOpenCodeGoAffinitySource, "token")
	common.SetContextKey(c, constant.ContextKeyOpenCodeGoAffinityKey, "safe-affinity")

	info := &relaycommon.RelayInfo{
		RelayFormat:            types.RelayFormatOpenAI,
		RelayMode:              relayconstant.RelayModeChatCompletions,
		RequestURLPath:         "/v1/chat/completions",
		StartTime:              start,
		FirstResponseTime:      start.Add(-time.Second),
		IsStream:               false,
		RequestConversionChain: []types.RelayFormat{types.RelayFormatOpenAI},
		ResponsesUsageInfo:     &relaycommon.ResponsesUsageInfo{BuiltInTools: map[string]*relaycommon.BuildInToolInfo{}},
	}
	baseline := info.SnapshotRelayAttempt()
	contextBaseline := snapshotRelayAttemptContext(c)

	// Attempt A resolved to Responses and returned malformed SSE before any
	// public byte. These are exactly the fields that previously contaminated B.
	info.IsStream = true
	info.ChannelMeta = &relaycommon.ChannelMeta{
		ChannelType:       constant.ChannelTypeOpenCodeAPIKey,
		ChannelId:         63,
		UpstreamModelName: "attempt-a-model",
	}
	info.FinalRequestRelayFormat = types.RelayFormatOpenAIResponses
	info.RequestConversionChain = append(info.RequestConversionChain, types.RelayFormatOpenAIResponses)
	info.RuntimeHeadersOverride = map[string]interface{}{"x-attempt-a": "private"}
	info.UseRuntimeHeadersOverride = true
	info.ParamOverrideAudit = []string{"attempt-a-audit"}
	info.UpstreamRequestBodySize = 4096
	info.StreamStatus = relaycommon.NewStreamStatus()
	info.StreamStatus.RecordError("malformed SSE")
	info.StreamProtocolTerminalRequired = true
	info.SendResponseCount = 3
	info.ReceivedResponseCount = 4
	info.ResponsesUsageInfo.BuiltInTools["web_search"] = &relaycommon.BuildInToolInfo{ToolName: "web_search", CallCount: 2}
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Retry-After", "attempt-a-private")
	c.Writer.Header().Set("X-Codex-Turn-State", "attempt-a")
	c.Set(common.UpstreamRequestIdKey, "attempt-a-upstream-id")
	c.Set("claude_web_search_requests", 7)
	c.Set("gemini_google_search_call", true)
	c.Set("chat_completion_web_search_context_size", "large")
	common.SetContextKey(c, constant.ContextKeyIsStream, true)
	common.SetContextKey(c, constant.ContextKeySystemPromptOverride, true)
	common.SetContextKey(c, constant.ContextKeyRelayFailed, true)

	resetOpenCodeAPIKeyRelayAttempt(c, info, baseline, contextBaseline)

	assert.False(t, info.IsStream)
	assert.Nil(t, info.ChannelMeta)
	assert.Equal(t, []types.RelayFormat{types.RelayFormatOpenAI}, info.RequestConversionChain)
	assert.Empty(t, info.FinalRequestRelayFormat)
	assert.Nil(t, info.RuntimeHeadersOverride)
	assert.False(t, info.UseRuntimeHeadersOverride)
	assert.Nil(t, info.ParamOverrideAudit)
	assert.Zero(t, info.UpstreamRequestBodySize)
	assert.Nil(t, info.StreamStatus)
	assert.False(t, info.StreamProtocolTerminalRequired)
	assert.Zero(t, info.SendResponseCount)
	assert.Zero(t, info.ReceivedResponseCount)
	assert.Empty(t, info.ResponsesUsageInfo.BuiltInTools)
	assert.Equal(t, "local-request-id", recorder.Header().Get(common.RequestIdKey))
	assert.Empty(t, recorder.Header().Get("Content-Type"))
	assert.Empty(t, recorder.Header().Get("Retry-After"))
	assert.Empty(t, recorder.Header().Get("X-Codex-Turn-State"))
	upstreamID, exists := c.Get(common.UpstreamRequestIdKey)
	require.True(t, exists)
	assert.Nil(t, upstreamID)
	assert.Zero(t, c.GetInt("claude_web_search_requests"))
	assert.False(t, c.GetBool("gemini_google_search_call"))
	assert.Nil(t, c.MustGet("chat_completion_web_search_context_size"))
	assert.False(t, common.GetContextKeyBool(c, constant.ContextKeyIsStream))
	assert.False(t, common.GetContextKeyBool(c, constant.ContextKeySystemPromptOverride))
	assert.False(t, common.GetContextKeyBool(c, constant.ContextKeyRelayFailed))
	assert.Equal(t, "token", common.GetContextKeyString(c, constant.ContextKeyOpenCodeGoAffinitySource))
	assert.Equal(t, "safe-affinity", common.GetContextKeyString(c, constant.ContextKeyOpenCodeGoAffinityKey))

	// Candidate B may now select Chat and consume a normal JSON response without
	// being routed through the SSE scanner by A's media type.
	info.FinalRequestRelayFormat = types.RelayFormatOpenAI
	assert.Equal(t, types.RelayFormatOpenAI, info.GetFinalRequestRelayFormat())
	assert.False(t, info.IsStream)
}

func TestRenderRelayErrorRedactsOpenCodeHTTP200EnvelopeBeforeServerLog(t *testing.T) {
	previousWriter := gin.DefaultErrorWriter
	var logBuffer bytes.Buffer
	gin.DefaultErrorWriter = &logBuffer
	t.Cleanup(func() { gin.DefaultErrorWriter = previousWriter })

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeAPIKey)
	relayErr := types.WithOpenAIError(types.OpenAIError{
		Message: "upstream error",
		Type:    "upstream_error",
		Code:    "upstream_error",
	}, http.StatusBadGateway)
	relayErr.SetMessage(`{"error":{"message":"Authorization: Bearer private-bearer; endpoint=http://internal-control.local/v1/private"}}`)
	service.MarkOpenCodeGoUpstreamRelayErrorWithStatus(relayErr, http.StatusOK)

	renderRelayError(c, types.RelayFormatOpenAIResponses, nil, relayErr, "request-id")

	serverLog := logBuffer.String()
	require.NotContains(t, serverLog, "private-bearer")
	require.NotContains(t, serverLog, "internal-control.local")
	require.NotContains(t, serverLog, "/v1/private")
	require.Contains(t, serverLog, "[redacted]")
}

func TestRenderRelayErrorMarksCommittedRelayFailure(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Data(http.StatusOK, "text/event-stream", []byte("data: partial\n\n"))

	relayErr := types.NewOpenAIError(
		errors.New("upstream stream terminated"),
		types.ErrorCodeBadResponse,
		http.StatusBadGateway,
	)
	renderRelayError(c, types.RelayFormatOpenAIResponses, nil, relayErr, "request-id")

	assert.True(t, common.GetContextKeyBool(c, constant.ContextKeyRelayFailed))
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "data: partial\n\n", recorder.Body.String())
}

type postResponseSettlementBilling struct {
	refundCalls int
	needsRefund bool
}

func (*postResponseSettlementBilling) Settle(int) error { return nil }

func (b *postResponseSettlementBilling) Refund(*gin.Context) { b.refundCalls++ }

func (b *postResponseSettlementBilling) NeedsRefund() bool { return b.needsRefund }

func (*postResponseSettlementBilling) GetPreConsumedQuota() int { return 50 }

func (*postResponseSettlementBilling) Reserve(int) error { return nil }

func TestOpenCodeGoFailuresNeverEnterGenericRetry(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeGo)
	err := types.NewOpenAIError(errors.New("temporary upstream failure"), types.ErrorCodeBadResponseStatusCode, http.StatusInternalServerError)

	assert.False(t, shouldRetry(c, err, 3))
}

func TestOpenCodeGoFailuresNeverDisableWholePoolChannel(t *testing.T) {
	oldEnabled := common.AutomaticDisableChannelEnabled
	common.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() { common.AutomaticDisableChannelEnabled = oldEnabled })
	err := types.NewError(errors.New("invalid account"), types.ErrorCodeChannelInvalidKey)

	assert.True(t, shouldDisableWholeChannel(constant.ChannelTypeOpenAI, err))
	assert.True(t, shouldDisableWholeChannel(constant.ChannelTypeOpenCodeAPIKey, err))
	assert.False(t, shouldDisableWholeChannel(constant.ChannelTypeOpenCodeGo, err))

	localWriterErr := types.NewOpenAIError(
		errors.New("downstream writer failed"),
		types.ErrorCodeChannelInvalidKey,
		http.StatusUnauthorized,
		types.ErrOptionWithProvenance(types.ErrorProvenance{
			Origin:  types.ErrorOriginLocalWriter,
			Subtype: "downstream_write",
		}),
	)
	assert.False(t, shouldDisableWholeChannel(constant.ChannelTypeOpenCodeAPIKey, localWriterErr))
	assert.False(t, shouldRetry(newOpenCodeAPIKeyRetryContext(), localWriterErr, 3))
}

func newOpenCodeAPIKeyRetryContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeAPIKey)
	return c
}

func TestOpenCodeAPIKeyAutoDisableUsesRawUpstreamStatusBeforePublicProjection(t *testing.T) {
	oldEnabled := common.AutomaticDisableChannelEnabled
	oldRanges := append([]operation_setting.StatusCodeRange(nil), operation_setting.AutomaticDisableStatusCodeRanges...)
	common.AutomaticDisableChannelEnabled = true
	operation_setting.AutomaticDisableStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: http.StatusUnauthorized, End: http.StatusUnauthorized}}
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = oldEnabled
		operation_setting.AutomaticDisableStatusCodeRanges = oldRanges
	})

	rawAuthErr := service.MarkOpenCodeGoUpstreamRelayError(types.WithOpenAIError(types.OpenAIError{
		Message: "operator-managed API key was rejected",
		Type:    "authentication_error",
		Code:    "invalid_api_key",
	}, http.StatusUnauthorized, types.ErrOptionWithSkipRetry()))
	service.ResetStatusCode(rawAuthErr, `{"401":429}`)
	publicErr := service.PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeAPIKey, rawAuthErr)

	require.NotSame(t, rawAuthErr, publicErr)
	assert.Equal(t, http.StatusTooManyRequests, rawAuthErr.StatusCode)
	assert.Equal(t, http.StatusTooManyRequests, publicErr.StatusCode)
	assert.True(t, shouldDisableWholeChannel(constant.ChannelTypeOpenCodeAPIKey, rawAuthErr))
	assert.False(t, shouldDisableWholeChannel(constant.ChannelTypeOpenCodeAPIKey, publicErr))
	assert.False(t, shouldDisableWholeChannel(constant.ChannelTypeOpenCodeGo, rawAuthErr))
	localWriteErr := types.NewOpenAIError(
		errors.New("downstream write failed"),
		types.ErrorCodeBadResponse,
		http.StatusUnauthorized,
		types.ErrOptionWithSkipRetry(),
	)
	assert.False(t, shouldDisableWholeChannel(constant.ChannelTypeOpenCodeAPIKey, localWriteErr))

	rawServerErr := service.MarkOpenCodeGoUpstreamRelayError(types.WithOpenAIError(types.OpenAIError{
		Message: "temporary upstream failure",
		Type:    "server_error",
		Code:    "server_error",
	}, http.StatusInternalServerError))
	service.ResetStatusCode(rawServerErr, `{"500":401}`)

	assert.Equal(t, http.StatusUnauthorized, rawServerErr.StatusCode)
	assert.False(t, shouldDisableWholeChannel(constant.ChannelTypeOpenCodeAPIKey, rawServerErr))
	assert.False(t, shouldDisableWholeChannel(constant.ChannelTypeOpenCodeGo, rawServerErr))
}

func TestOpenCodeAPIKeyGenericRetryUsesRawUpstreamStatusBeforeMapping(t *testing.T) {
	oldRanges := append([]operation_setting.StatusCodeRange(nil), operation_setting.AutomaticRetryStatusCodeRanges...)
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{
		{Start: http.StatusInternalServerError, End: http.StatusServiceUnavailable},
	}
	t.Cleanup(func() { operation_setting.AutomaticRetryStatusCodeRanges = oldRanges })

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeAPIKey)

	rawServerErr := service.MarkOpenCodeGoUpstreamRelayError(types.WithOpenAIError(types.OpenAIError{
		Message: "temporary upstream failure",
		Type:    "server_error",
		Code:    "server_error",
	}, http.StatusInternalServerError))
	service.ResetStatusCode(rawServerErr, `{"500":400}`)
	require.Equal(t, http.StatusBadRequest, rawServerErr.StatusCode)
	assert.True(t, shouldRetry(c, rawServerErr, 1), "a mapped 400 must not hide the raw upstream 500")

	rawClientErr := service.MarkOpenCodeGoUpstreamRelayError(types.WithOpenAIError(types.OpenAIError{
		Message: "messages[0].role is required",
		Type:    "invalid_request_error",
		Code:    "invalid_request_error",
	}, http.StatusBadRequest))
	service.ResetStatusCode(rawClientErr, `{"400":503}`)
	require.Equal(t, http.StatusServiceUnavailable, rawClientErr.StatusCode)
	assert.False(t, shouldRetry(c, rawClientErr, 1), "a mapped 503 must not turn the raw upstream 400 into a retry")
}

func TestOpenCodeAPIKeyResponseLimitDoesNotRetryAsFabricatedBadGateway(t *testing.T) {
	oldRetryRanges := append([]operation_setting.StatusCodeRange(nil), operation_setting.AutomaticRetryStatusCodeRanges...)
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{{
		Start: http.StatusBadGateway,
		End:   http.StatusBadGateway,
	}}
	t.Cleanup(func() { operation_setting.AutomaticRetryStatusCodeRanges = oldRetryRanges })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeAPIKey)

	limitErr := types.NewOpenAIError(
		errors.New("response exceeded local limit"),
		types.ErrorCodeReadResponseBodyFailed,
		http.StatusBadGateway,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithProvenance(types.ErrorProvenance{
			Origin:  types.ErrorOriginGatewayInvariant,
			Subtype: "response_limit",
		}),
	)
	assert.False(t, shouldRetry(c, limitErr, 1))
	assert.False(t, service.IsOpenCodeGoUpstreamRelayError(limitErr))
	assert.Zero(t, limitErr.Provenance().RawStatusCode)

	rawBadGateway := service.MarkOpenCodeGoUpstreamRelayErrorWithStatus(types.NewOpenAIError(
		errors.New("trusted upstream HTTP failure"),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusBadGateway,
	), http.StatusBadGateway)
	assert.True(t, shouldRetry(c, rawBadGateway, 1), "the control proves the retry range is reachable")
}

func TestOpenCodeAPIKeyGenericRetryClassifiesHTTP200ErrorEnvelope(t *testing.T) {
	oldRanges := append([]operation_setting.StatusCodeRange(nil), operation_setting.AutomaticRetryStatusCodeRanges...)
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{
		{Start: http.StatusTooManyRequests, End: http.StatusTooManyRequests},
		{Start: http.StatusInternalServerError, End: http.StatusBadGateway},
	}
	t.Cleanup(func() { operation_setting.AutomaticRetryStatusCodeRanges = oldRanges })

	tests := []struct {
		name        string
		channelType int
		errorType   string
		errorCode   string
		wantRetry   bool
	}{
		{
			name:        "server error retries",
			channelType: constant.ChannelTypeOpenCodeAPIKey,
			errorType:   "server_error",
			errorCode:   "server_error",
			wantRetry:   true,
		},
		{
			name:        "rate limit retries",
			channelType: constant.ChannelTypeOpenCodeAPIKey,
			errorType:   "rate_limit_error",
			errorCode:   "rate_limit_exceeded",
			wantRetry:   true,
		},
		{
			name:        "client request does not retry",
			channelType: constant.ChannelTypeOpenCodeAPIKey,
			errorType:   "invalid_request_error",
			errorCode:   "invalid_prompt",
			wantRetry:   false,
		},
		{
			name:        "unknown envelope fails closed",
			channelType: constant.ChannelTypeOpenCodeAPIKey,
			errorType:   "upstream_error",
			errorCode:   "shard_failure",
			wantRetry:   true,
		},
		{
			name:        "type 62 remains outside generic retry",
			channelType: constant.ChannelTypeOpenCodeGo,
			errorType:   "server_error",
			errorCode:   "server_error",
			wantRetry:   false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			common.SetContextKey(c, constant.ContextKeyChannelType, test.channelType)
			err := service.MarkOpenCodeGoUpstreamRelayError(types.WithOpenAIError(types.OpenAIError{
				Message: "structured upstream error envelope",
				Type:    test.errorType,
				Code:    test.errorCode,
			}, http.StatusOK))

			require.Equal(t, http.StatusOK, err.StatusCode)
			assert.Equal(t, test.wantRetry, shouldRetry(c, err, 1))
		})
	}
}

func TestOpenCodeAPIKeyGenericRetryStopsAfterPublicOutputOrCancellation(t *testing.T) {
	newContext := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeAPIKey)
		return c
	}
	upstreamErr := func() *types.NewAPIError {
		return service.MarkOpenCodeGoUpstreamRelayError(types.WithOpenAIError(types.OpenAIError{
			Message: "temporary upstream failure",
			Type:    "server_error",
			Code:    "server_error",
		}, http.StatusInternalServerError))
	}

	written := newContext()
	written.Data(http.StatusOK, "text/event-stream", []byte("data: partial\n\n"))
	assert.False(t, shouldRetry(written, upstreamErr(), 1))

	cancelled := newContext()
	requestContext, cancel := context.WithCancel(cancelled.Request.Context())
	cancel()
	cancelled.Request = cancelled.Request.WithContext(requestContext)
	assert.False(t, shouldRetry(cancelled, upstreamErr(), 1))
}

func TestPostResponseSettlementFailureRefundsWithoutRetryDisableOrResponseAppend(t *testing.T) {
	oldEnabled := common.AutomaticDisableChannelEnabled
	common.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() { common.AutomaticDisableChannelEnabled = oldEnabled })

	apiErr := types.NewErrorWithStatusCode(
		errors.New("forced funding settlement failure"),
		types.ErrorCodeBillingSettlementFailed,
		http.StatusInternalServerError,
		types.ErrOptionWithSkipRetry(),
	)

	for _, test := range []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "json", contentType: "application/json", body: `{"id":"completed"}`},
		{name: "sse", contentType: "text/event-stream", body: "data: [DONE]\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			c.Data(http.StatusOK, test.contentType, []byte(test.body))

			renderRelayError(c, types.RelayFormatOpenAI, nil, apiErr, "request-id")

			assert.Equal(t, http.StatusOK, recorder.Code)
			assert.Equal(t, test.body, recorder.Body.String())
			assert.NotContains(t, recorder.Body.String(), `"error"`)
		})
	}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	assert.False(t, shouldRetry(c, apiErr, 3))
	assert.False(t, shouldDisableWholeChannel(constant.ChannelTypeOpenAI, apiErr))

	billing := &postResponseSettlementBilling{needsRefund: true}
	refundRelayBilling(c, &relaycommon.RelayInfo{Billing: billing})
	assert.Equal(t, 1, billing.refundCalls)

	settledBilling := &postResponseSettlementBilling{needsRefund: false}
	refundRelayBilling(c, &relaycommon.RelayInfo{Billing: settledBilling})
	assert.Equal(t, 0, settledBilling.refundCalls)
}
