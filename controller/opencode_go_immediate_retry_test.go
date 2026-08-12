package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newOpenCodeGoImmediateRetryContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c
}

func markedOpenCodeGoImmediateRetryError(message string, status int) *types.NewAPIError {
	return service.MarkOpenCodeGoUpstreamRelayError(types.NewOpenAIError(
		errors.New(message),
		types.ErrorCodeBadResponse,
		status,
		types.ErrOptionWithSkipRetry(),
	))
}

func deferOpenCodeGoImmediateRetryFailure(c *gin.Context, calls *int) {
	run := func() { (*calls)++ }
	if !service.DeferOpenCodeGoImmediateRetryFailover(c, run) {
		run()
	}
}

func TestOpenCodeGoImmediateRetrySucceedsOnSecondAttemptAndResetsState(t *testing.T) {
	previousRetryTimes := common.RetryTimes
	common.RetryTimes = 0
	t.Cleanup(func() { common.RetryTimes = previousRetryTimes })

	c := newOpenCodeGoImmediateRetryContext()
	info := &relaycommon.RelayInfo{IsStream: true}
	info.ResponsesUsageInfo = &relaycommon.ResponsesUsageInfo{BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
		"web_search": {ToolName: "web_search", CallCount: 1},
	}}
	c.Set("claude_web_search_requests", 2)
	c.Set("gemini_google_search_call", false)
	selection := &service.OpenCodeGoPoolSelection{WorkspaceUID: "workspace-pinned"}
	firstFailureCalls := 0
	attemptCalls := 0
	dirtyHeaders := []string{
		"Content-Type",
		"Content-Length",
		"Cache-Control",
		"Connection",
		"Transfer-Encoding",
		"X-Accel-Buffering",
		"X-Codex-Turn-State",
		"X-Reasoning-Included",
		"Retry-After",
	}

	result := relaySelectedChannelWithOpenCodeGoRetry(
		c,
		types.RelayFormatOpenAIResponses,
		info,
		constant.ChannelTypeOpenCodeGo,
		func() *types.NewAPIError {
			attemptCalls++
			if attemptCalls == 1 {
				service.RememberOpenCodeGoImmediateRetrySelection(c, selection)
				deferOpenCodeGoImmediateRetryFailure(c, &firstFailureCalls)
				for _, name := range dirtyHeaders {
					c.Writer.Header().Set(name, "first-attempt-value")
				}
				c.Set(common.UpstreamRequestIdKey, "first-attempt-request-id")
				info.IsStream = false
				info.StreamStatus = relaycommon.NewStreamStatus()
				info.StreamProtocolTerminalRequired = true
				info.ReceivedResponseCount = 7
				info.ResponsesUsageInfo.BuiltInTools["web_search"].CallCount = 3
				c.Set("claude_web_search_requests", 5)
				c.Set("gemini_google_search_call", true)
				return markedOpenCodeGoImmediateRetryError("first upstream failure", http.StatusServiceUnavailable)
			}

			pinned, replaying := service.OpenCodeGoImmediateRetrySelection(c)
			require.True(t, replaying)
			require.Same(t, selection, pinned)
			for _, name := range dirtyHeaders {
				assert.Empty(t, c.Writer.Header().Get(name), name)
			}
			upstreamRequestID, exists := c.Get(common.UpstreamRequestIdKey)
			assert.True(t, exists)
			assert.Nil(t, upstreamRequestID)
			assert.True(t, info.IsStream)
			assert.Nil(t, info.StreamStatus)
			assert.False(t, info.StreamProtocolTerminalRequired)
			assert.Zero(t, info.ReceivedResponseCount)
			assert.Equal(t, 1, info.ResponsesUsageInfo.BuiltInTools["web_search"].CallCount)
			assert.Equal(t, 2, c.GetInt("claude_web_search_requests"))
			assert.False(t, c.GetBool("gemini_google_search_call"))
			return nil
		},
	)

	require.Nil(t, result)
	assert.Equal(t, 2, attemptCalls)
	assert.Zero(t, firstFailureCalls)
	_, replaying := service.OpenCodeGoImmediateRetrySelection(c)
	assert.False(t, replaying)
}

func TestOpenCodeGoImmediateRetryReturnsSecondFailureAndCountsOnlyFinalFailure(t *testing.T) {
	c := newOpenCodeGoImmediateRetryContext()
	info := &relaycommon.RelayInfo{}
	selection := &service.OpenCodeGoPoolSelection{WorkspaceUID: "workspace-pinned"}
	firstFailureCalls := 0
	finalFailureCalls := 0
	attemptCalls := 0

	result := relaySelectedChannelWithOpenCodeGoRetry(
		c,
		types.RelayFormatOpenAIResponses,
		info,
		constant.ChannelTypeOpenCodeGo,
		func() *types.NewAPIError {
			attemptCalls++
			if attemptCalls == 1 {
				service.RememberOpenCodeGoImmediateRetrySelection(c, selection)
				deferOpenCodeGoImmediateRetryFailure(c, &firstFailureCalls)
				return markedOpenCodeGoImmediateRetryError("first upstream failure", http.StatusServiceUnavailable)
			}

			pinned, replaying := service.OpenCodeGoImmediateRetrySelection(c)
			require.True(t, replaying)
			require.Same(t, selection, pinned)
			deferOpenCodeGoImmediateRetryFailure(c, &finalFailureCalls)
			return markedOpenCodeGoImmediateRetryError("second upstream failure", http.StatusBadGateway)
		},
	)

	require.NotNil(t, result)
	assert.Equal(t, http.StatusBadGateway, result.StatusCode)
	assert.ErrorContains(t, result, "second upstream failure")
	assert.NotContains(t, result.Error(), "first upstream failure")
	assert.Equal(t, 2, attemptCalls)
	assert.Zero(t, firstFailureCalls)
	assert.Equal(t, 1, finalFailureCalls)
}

func TestOpenCodeGoImmediateRetryRetriesWrappedPoolExhaustion(t *testing.T) {
	c := newOpenCodeGoImmediateRetryContext()
	attemptCalls := 0

	result := relaySelectedChannelWithOpenCodeGoRetry(
		c,
		types.RelayFormatOpenAIResponses,
		&relaycommon.RelayInfo{},
		constant.ChannelTypeOpenCodeGo,
		func() *types.NewAPIError {
			attemptCalls++
			if attemptCalls == 1 {
				return types.NewOpenAIError(
					fmt.Errorf("setup request header failed: %w", service.ErrOpenCodeGoNoEligibleWorkspace),
					types.ErrorCodeDoRequestFailed,
					http.StatusInternalServerError,
				)
			}
			return nil
		},
	)

	require.Nil(t, result)
	assert.Equal(t, 2, attemptCalls)
}

type openCodeGoUnwrittenFailingWriter struct {
	gin.ResponseWriter
	err error
}

func (w *openCodeGoUnwrittenFailingWriter) Write([]byte) (int, error) { return 0, w.err }
func (w *openCodeGoUnwrittenFailingWriter) WriteHeader(int)           {}
func (w *openCodeGoUnwrittenFailingWriter) WriteHeaderNow()           {}
func (w *openCodeGoUnwrittenFailingWriter) Flush()                    {}
func (w *openCodeGoUnwrittenFailingWriter) Written() bool             { return false }

func TestOpenCodeGoImmediateRetryRejectsIneligibleOrUnsafeFailures(t *testing.T) {
	tests := []struct {
		name             string
		channelType      int
		prepare          func(*gin.Context)
		err              func() *types.NewAPIError
		wantFailureCalls int
	}{
		{
			name:             "unmarked 503",
			wantFailureCalls: 1,
			err: func() *types.NewAPIError {
				return types.NewOpenAIError(errors.New("local failure"), types.ErrorCodeBadResponse, http.StatusServiceUnavailable)
			},
		},
		{
			name:             "marked 429",
			wantFailureCalls: 1,
			err: func() *types.NewAPIError {
				return markedOpenCodeGoImmediateRetryError("rate limited", http.StatusTooManyRequests)
			},
		},
		{
			name:             "other channel marked 503",
			channelType:      constant.ChannelTypeOpenAI,
			wantFailureCalls: 1,
			err: func() *types.NewAPIError {
				return markedOpenCodeGoImmediateRetryError("other channel failure", http.StatusServiceUnavailable)
			},
		},
		{
			name:             "response already written",
			wantFailureCalls: 1,
			prepare: func(c *gin.Context) {
				c.Writer.WriteHeaderNow()
			},
			err: func() *types.NewAPIError {
				return markedOpenCodeGoImmediateRetryError("too late", http.StatusServiceUnavailable)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newOpenCodeGoImmediateRetryContext()
			channelType := test.channelType
			if channelType == 0 {
				channelType = constant.ChannelTypeOpenCodeGo
			}
			if test.prepare != nil {
				test.prepare(c)
			}
			attemptCalls := 0
			failureCalls := 0
			wantErr := test.err()

			result := relaySelectedChannelWithOpenCodeGoRetry(
				c,
				types.RelayFormatOpenAIResponses,
				&relaycommon.RelayInfo{},
				channelType,
				func() *types.NewAPIError {
					attemptCalls++
					deferOpenCodeGoImmediateRetryFailure(c, &failureCalls)
					return wantErr
				},
			)

			require.NotNil(t, result)
			assert.Equal(t, wantErr.Error(), result.Error())
			assert.Equal(t, 1, attemptCalls)
			assert.Equal(t, test.wantFailureCalls, failureCalls)
		})
	}
}

func TestOpenCodeGoImmediateRetryDiscardsPendingFailoverForLocalAbort(t *testing.T) {
	tests := []struct {
		name  string
		abort func(*testing.T, *gin.Context)
	}{
		{
			name: "request cancelled",
			abort: func(t *testing.T, c *gin.Context) {
				requestContext, cancel := context.WithCancel(c.Request.Context())
				cancel()
				c.Request = c.Request.WithContext(requestContext)
			},
		},
		{
			name: "local response write failure",
			abort: func(t *testing.T, c *gin.Context) {
				c.Writer = &openCodeGoUnwrittenFailingWriter{
					ResponseWriter: c.Writer,
					err:            errors.New("downstream write failed"),
				}
				service.IOCopyBytesGracefully(c, nil, []byte("partial"))
				require.False(t, c.Writer.Written())
				require.Error(t, service.ResponseBodyWriteError(c))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newOpenCodeGoImmediateRetryContext()
			attemptCalls := 0
			failureCalls := 0

			result := relaySelectedChannelWithOpenCodeGoRetry(
				c,
				types.RelayFormatOpenAIResponses,
				&relaycommon.RelayInfo{},
				constant.ChannelTypeOpenCodeGo,
				func() *types.NewAPIError {
					attemptCalls++
					deferOpenCodeGoImmediateRetryFailure(c, &failureCalls)
					test.abort(t, c)
					return markedOpenCodeGoImmediateRetryError("upstream failure", http.StatusServiceUnavailable)
				},
			)

			require.NotNil(t, result)
			assert.Equal(t, 1, attemptCalls)
			assert.Zero(t, failureCalls)
		})
	}
}

func TestOpenCodeGoImmediateRetryUsesRawStatusBeforeChannelMapping(t *testing.T) {
	tests := []struct {
		name         string
		rawStatus    int
		mapping      string
		wantAttempts int
		wantStatus   int
	}{
		{
			name:         "raw 400 mapped to 503 is not retried",
			rawStatus:    http.StatusBadRequest,
			mapping:      `{"400":503}`,
			wantAttempts: 1,
			wantStatus:   http.StatusServiceUnavailable,
		},
		{
			name:         "raw 503 mapped to 400 is retried",
			rawStatus:    http.StatusServiceUnavailable,
			mapping:      `{"503":400}`,
			wantAttempts: 2,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newOpenCodeGoImmediateRetryContext()
			attemptCalls := 0

			result := relaySelectedChannelWithOpenCodeGoRetry(
				c,
				types.RelayFormatOpenAIResponses,
				&relaycommon.RelayInfo{},
				constant.ChannelTypeOpenCodeGo,
				func() *types.NewAPIError {
					attemptCalls++
					if attemptCalls > 1 {
						return nil
					}
					relayErr := markedOpenCodeGoImmediateRetryError("upstream failure", test.rawStatus)
					service.ResetStatusCode(relayErr, test.mapping)
					return relayErr
				},
			)

			assert.Equal(t, test.wantAttempts, attemptCalls)
			if test.wantStatus == 0 {
				require.Nil(t, result)
				return
			}
			require.NotNil(t, result)
			assert.Equal(t, test.wantStatus, result.StatusCode)
		})
	}
}
