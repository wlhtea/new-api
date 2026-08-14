package controller

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
}

func (*postResponseSettlementBilling) Settle(int) error { return nil }

func (b *postResponseSettlementBilling) Refund(*gin.Context) { b.refundCalls++ }

func (*postResponseSettlementBilling) NeedsRefund() bool { return true }

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

	billing := &postResponseSettlementBilling{}
	refundRelayBilling(c, &relaycommon.RelayInfo{Billing: billing})
	assert.Equal(t, 1, billing.refundCalls)
}
