package controller

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderRelayErrorHidesOpenCodeGoPoolExhaustion(t *testing.T) {
	internalErr := types.NewOpenAIError(
		fmt.Errorf("setup request header failed: %w", service.ErrOpenCodeGoNoEligibleWorkspace),
		types.ErrorCodeDoRequestFailed,
		http.StatusInternalServerError,
	)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeGo)

	renderRelayError(c, types.RelayFormatOpenAI, nil, internalErr, "request-id")

	require.Equal(t, http.StatusTooManyRequests, recorder.Code)
	assert.JSONEq(t, `{
		"error": {
			"message": "当前分组上游负载已饱和，请稍后再试",
			"type": "rate_limit_error",
			"param": "",
			"code": "rate_limit_error"
		}
	}`, recorder.Body.String())
	for _, privateDetail := range []string{
		"OpenCode Go",
		"workspace",
		"channel",
		"requested model",
		"setup request header failed",
	} {
		assert.NotContains(t, recorder.Body.String(), privateDetail)
	}

	assert.Equal(t, http.StatusInternalServerError, internalErr.StatusCode)
	assert.ErrorIs(t, internalErr, service.ErrOpenCodeGoNoEligibleWorkspace)
	assert.ErrorContains(t, internalErr, "setup request header failed")
}

func TestRenderRelayErrorFailsClosedForUnprovenancedOpenCodeGoFailures(t *testing.T) {
	internalErr := types.NewOpenAIError(
		errors.New("request setup failed"),
		types.ErrorCodeDoRequestFailed,
		http.StatusInternalServerError,
	)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeGo)

	renderRelayError(c, types.RelayFormatOpenAI, nil, internalErr, "request-id")

	assert.Equal(t, http.StatusTooManyRequests, recorder.Code)
	assert.Contains(t, recorder.Body.String(), groupUpstreamOverloadedMessage)
	assert.NotContains(t, recorder.Body.String(), "request setup failed")
	assert.Equal(t, http.StatusInternalServerError, internalErr.StatusCode)
	assert.Equal(t, "request setup failed", internalErr.Error())
}

func TestRenderRelayErrorHidesOpenCodeGoEndpointUnavailable(t *testing.T) {
	const internalMessage = "Error from provider (Console Go):\nUpstream request failed: Endpoint is unavailable."
	internalErr := types.WithOpenAIError(types.OpenAIError{
		Message: internalMessage,
		Type:    "upstream_error",
		Code:    "upstream_error",
	}, http.StatusServiceUnavailable, types.ErrOptionWithSkipRetry())
	var logOutput bytes.Buffer
	common.LogWriterMu.Lock()
	previousErrorWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logOutput
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = previousErrorWriter
		common.LogWriterMu.Unlock()
	})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeGo)
	for name, value := range map[string]string{
		"Content-Type":         "text/event-stream",
		"Cache-Control":        "no-cache",
		"Connection":           "keep-alive",
		"Transfer-Encoding":    "chunked",
		"X-Accel-Buffering":    "no",
		"X-Codex-Turn-State":   "internal-endpoint-state",
		"X-Reasoning-Included": "workspace=wrk_private",
	} {
		c.Writer.Header().Set(name, value)
	}

	renderRelayError(c, types.RelayFormatOpenAIResponses, nil, internalErr, "request-id")

	require.Equal(t, http.StatusTooManyRequests, recorder.Code)
	assert.Equal(t, "application/json; charset=utf-8", recorder.Header().Get("Content-Type"))
	for _, name := range []string{
		"Cache-Control",
		"Connection",
		"Transfer-Encoding",
		"X-Accel-Buffering",
		"X-Codex-Turn-State",
		"X-Reasoning-Included",
	} {
		assert.Empty(t, recorder.Header().Get(name))
	}
	assert.JSONEq(t, `{
		"error": {
			"message": "当前分组上游负载已饱和，请稍后再试",
			"type": "rate_limit_error",
			"param": "",
			"code": "rate_limit_error"
		}
	}`, recorder.Body.String())
	assert.NotContains(t, recorder.Body.String(), "Console Go")
	assert.NotContains(t, recorder.Body.String(), "Endpoint is unavailable")
	assert.Contains(t, logOutput.String(), "Error from provider (Console Go):")
	assert.Contains(t, logOutput.String(), "Upstream request failed: Endpoint is unavailable.")
	assert.Equal(t, http.StatusServiceUnavailable, internalErr.StatusCode)
	assert.Equal(t, internalMessage, internalErr.Error())
}

func TestRenderRelayErrorHidesMarkedUnknownOpenCodeGoUpstreamError(t *testing.T) {
	const internalMessage = "internal shard zen-primary failed"
	for _, test := range []struct {
		name        string
		relayFormat types.RelayFormat
		wantCode    bool
	}{
		{name: "openai", relayFormat: types.RelayFormatOpenAI, wantCode: true},
		{name: "responses", relayFormat: types.RelayFormatOpenAIResponses, wantCode: true},
		{name: "claude", relayFormat: types.RelayFormatClaude, wantCode: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			internalErr := service.MarkOpenCodeGoUpstreamRelayError(types.WithOpenAIError(types.OpenAIError{
				Message: internalMessage,
				Type:    "upstream_error",
				Code:    "shard_failure",
			}, http.StatusServiceUnavailable, types.ErrOptionWithSkipRetry()))
			var logOutput bytes.Buffer
			common.LogWriterMu.Lock()
			previousErrorWriter := gin.DefaultErrorWriter
			gin.DefaultErrorWriter = &logOutput
			common.LogWriterMu.Unlock()
			t.Cleanup(func() {
				common.LogWriterMu.Lock()
				gin.DefaultErrorWriter = previousErrorWriter
				common.LogWriterMu.Unlock()
			})

			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeGo)

			renderRelayError(c, test.relayFormat, nil, internalErr, "request-id")

			require.Equal(t, http.StatusTooManyRequests, recorder.Code)
			body := recorder.Body.String()
			assert.Contains(t, body, groupUpstreamOverloadedMessage)
			assert.Contains(t, body, `"type":"rate_limit_error"`)
			if test.wantCode {
				assert.Contains(t, body, `"code":"rate_limit_error"`)
			}
			assert.NotContains(t, body, internalMessage)
			assert.NotContains(t, body, "zen-primary")
			assert.Contains(t, logOutput.String(), internalMessage)
			assert.Equal(t, internalMessage, internalErr.Error())
			assert.True(t, service.IsOpenCodeGoUpstreamRelayError(internalErr))
		})
	}
}

func TestRenderRelayErrorDropsUntrustedNonStreamResponseHeaders(t *testing.T) {
	internalErr := types.NewOpenAIError(
		errors.New("OpenCode Go workspace is unavailable"),
		types.ErrorCodeBadResponse,
		http.StatusServiceUnavailable,
	)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeGo)
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.Header().Set("Cache-Control", "private, max-age=30")

	renderRelayError(c, types.RelayFormatOpenAI, nil, internalErr, "request-id")

	assert.Equal(t, http.StatusTooManyRequests, recorder.Code)
	assert.Empty(t, recorder.Header().Get("Cache-Control"))
	assert.Equal(t, "request-id", recorder.Header().Get(common.RequestIdKey))
}

func TestPublicRelayErrorDoesNotRewriteOtherChannelFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		statusCode int
		message    string
	}{
		{
			name:       "different provider error",
			statusCode: http.StatusServiceUnavailable,
			message:    "Error from provider (Console Go): Upstream request failed: Service is unavailable.",
		},
		{
			name:       "generic endpoint error",
			statusCode: http.StatusServiceUnavailable,
			message:    "Endpoint is unavailable.",
		},
		{
			name:       "same message with different status",
			statusCode: http.StatusBadGateway,
			message:    "Error from provider (Console Go): Upstream request failed: Endpoint is unavailable.",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			internalErr := types.WithOpenAIError(types.OpenAIError{
				Message: test.message,
				Type:    "upstream_error",
				Code:    "upstream_error",
			}, test.statusCode, types.ErrOptionWithSkipRetry())

			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
			assert.Same(t, internalErr, publicRelayError(c, internalErr))
		})
	}
}

func TestRenderRelayErrorHidesOpenCodeGoInternalFields(t *testing.T) {
	tests := []struct {
		name        string
		relayFormat types.RelayFormat
		err         *types.NewAPIError
	}{
		{
			name:        "chat message",
			relayFormat: types.RelayFormatOpenAI,
			err: types.NewOpenAIError(
				errors.New("OpenCode Go protocol is not configured"),
				types.ErrorCodeInvalidRequest,
				http.StatusBadRequest,
			),
		},
		{
			name:        "responses metadata",
			relayFormat: types.RelayFormatOpenAIResponses,
			err: types.WithOpenAIError(types.OpenAIError{
				Message:  "request failed",
				Type:     "upstream_error",
				Code:     "upstream_error",
				Metadata: []byte(`{"workspace":"wrk_private"}`),
			}, http.StatusOK),
		},
		{
			name:        "claude code",
			relayFormat: types.RelayFormatClaude,
			err: types.WithClaudeError(types.ClaudeError{
				Message: "request failed",
				Type:    "workspace_unavailable",
			}, http.StatusInternalServerError),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeGo)

			renderRelayError(c, test.relayFormat, nil, test.err, "request-id")

			assert.Equal(t, http.StatusTooManyRequests, recorder.Code)
			body := strings.ToLower(recorder.Body.String())
			assert.Contains(t, body, groupUpstreamOverloadedMessage)
			for _, marker := range []string{"opencode", "console go", "workspace", "wrk_"} {
				assert.NotContains(t, body, marker)
			}
		})
	}
}
