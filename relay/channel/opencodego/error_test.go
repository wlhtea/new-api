package opencodego

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type errorReadCloser struct {
	err error
}

func (r errorReadCloser) Read([]byte) (int, error) {
	return 0, r.err
}

func (errorReadCloser) Close() error {
	return nil
}

func TestHandleNon2xxResponseLogsOnlyPrivacySafeRequestShape(t *testing.T) {
	originalWriter := gin.DefaultErrorWriter
	var output bytes.Buffer
	gin.DefaultErrorWriter = &output
	t.Cleanup(func() { gin.DefaultErrorWriter = originalWriter })

	adaptor := &Adaptor{
		protocol:              ProtocolResponses,
		bufferClaudeToolCall:  true,
		requestInputItems:     37,
		requestToolCount:      12,
		requestUpstreamStream: false,
		workspaceSelected:     true,
		selectedWorkspaceUID:  "private-workspace-value",
		affinityIdentity:      "private-session-value",
	}
	info := &relaycommon.RelayInfo{
		RelayFormat:             types.RelayFormatClaude,
		UpstreamRequestBodySize: 123_456,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:         42,
			UpstreamModelName: "glm-5.2",
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"Internal server error"}}`)),
	}

	_, observation := adaptor.HandleNon2xxResponse(newAdaptorTestContext(), resp, info)

	require.NotNil(t, observation)
	logOutput := output.String()
	assert.Contains(t, logOutput, "status=500")
	assert.Contains(t, logOutput, `protocol="responses"`)
	assert.Contains(t, logOutput, `client_format="claude"`)
	assert.Contains(t, logOutput, "buffered_tool=true")
	assert.Contains(t, logOutput, "input_items=37")
	assert.Contains(t, logOutput, "tools=12")
	assert.Contains(t, logOutput, "body_bytes=123456")
	assert.NotContains(t, logOutput, "private-workspace-value")
	assert.NotContains(t, logOutput, "private-session-value")
}

func TestHandleNon2xxResponsePreservesSafeMetadataAndRedactsSecrets(t *testing.T) {
	secret := "sk-" + strings.Repeat("A", 24)
	workspace := "wrk_" + strings.Repeat("B", 20)
	body := fmt.Sprintf(`{"type":"error","error":{"type":"GoUsageLimitError","code":"go_limit","message":"limit for %s at https://opencode.ai/workspace/%s/go using %s"},"metadata":{"workspace":"%s","limitName":"weekly"}}`, workspace, workspace, secret, workspace)
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"321"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	apiErr, observation := (&Adaptor{}).HandleNon2xxResponse(c, resp, nil)

	require.NotNil(t, apiErr)
	require.NotNil(t, observation)
	assert.Equal(t, http.StatusTooManyRequests, apiErr.StatusCode)
	assert.True(t, types.IsSkipRetryError(apiErr))
	assert.Equal(t, "321", recorder.Header().Get("Retry-After"))
	assert.Equal(t, "GoUsageLimitError", observation.ErrorType)
	assert.Equal(t, "go_limit", observation.ErrorCode)
	assert.Equal(t, "weekly", observation.LimitName)
	assert.NotContains(t, observation.Message, secret)
	assert.NotContains(t, observation.Message, workspace)
	assert.NotContains(t, string(apiErr.Metadata), workspace)
	assert.LessOrEqual(t, len(observation.Message), maxErrorMessageBytes)
}

func TestTryHandleNon2xxResponseStoresTypedObservation(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"RegionError","message":"region unavailable"}}`)),
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	apiErr, handled := channel.TryHandleNon2xxResponse(c, &Adaptor{}, resp, nil)

	require.True(t, handled)
	require.NotNil(t, apiErr)
	value, exists := c.Get(channel.ContextKeyNon2xxResponseObservation)
	require.True(t, exists)
	observation, ok := value.(*channel.Non2xxResponseObservation)
	require.True(t, ok)
	assert.Equal(t, "RegionError", observation.ErrorType)
	assert.Equal(t, http.StatusForbidden, observation.StatusCode)
}

func TestHandleNon2xxResponseBoundsUnstructuredBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", maxErrorBodyBytes*2))),
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	apiErr, observation := (&Adaptor{}).HandleNon2xxResponse(c, resp, nil)

	require.NotNil(t, apiErr)
	require.NotNil(t, observation)
	assert.Equal(t, "OpenCode Go returned status 502", observation.Message)
	assert.NotContains(t, apiErr.Error(), strings.Repeat("x", 100))
}

func TestHandleNon2xxResponsePersistsSelectedWorkspaceObservation(t *testing.T) {
	originalObserver := observeOpenCodeGoProviderFailure
	originalNow := openCodeGoHealthNow
	t.Cleanup(func() {
		observeOpenCodeGoProviderFailure = originalObserver
		openCodeGoHealthNow = originalNow
	})

	fixedNow := time.Unix(1_900_000_000, 0)
	openCodeGoHealthNow = func() time.Time { return fixedNow }
	calls := 0
	observeOpenCodeGoProviderFailure = func(channelID int, workspaceUID string, upstreamModel string, failure service.OpenCodeGoProviderFailure, observedAt time.Time) (bool, error) {
		calls++
		assert.Equal(t, 42, channelID)
		assert.Equal(t, "workspace-provider", workspaceUID)
		assert.Equal(t, "glm-5.2", upstreamModel)
		assert.Equal(t, http.StatusForbidden, failure.StatusCode)
		assert.Equal(t, "RegionError", failure.ErrorType)
		assert.Equal(t, fixedNow, observedAt)
		return true, nil
	}

	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"RegionError","message":"region unavailable"}}`)),
	}
	adaptor := &Adaptor{workspaceSelected: true, selectedWorkspaceUID: "workspace-provider"}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 42, UpstreamModelName: "glm-5.2"}}
	_, observation := adaptor.HandleNon2xxResponse(newAdaptorTestContext(), resp, info)
	require.NotNil(t, observation)
	assert.Equal(t, 1, calls)
}

func TestHandleNon2xxResponseCountsOnlyGenericFailoverStatuses(t *testing.T) {
	originalProviderObserver := observeOpenCodeGoProviderFailure
	originalFailoverObserver := observeOpenCodeGoFailoverFailure
	originalNow := openCodeGoHealthNow
	t.Cleanup(func() {
		observeOpenCodeGoProviderFailure = originalProviderObserver
		observeOpenCodeGoFailoverFailure = originalFailoverObserver
		openCodeGoHealthNow = originalNow
	})

	fixedNow := time.Unix(1_900_000_000, 0)
	openCodeGoHealthNow = func() time.Time { return fixedNow }
	observeOpenCodeGoProviderFailure = func(_ int, _ string, _ string, _ service.OpenCodeGoProviderFailure, _ time.Time) (bool, error) {
		return false, nil
	}
	failoverCalls := 0
	observeOpenCodeGoFailoverFailure = func(_ *service.OpenCodeGoFailoverAttempt, observedAt time.Time) (service.OpenCodeGoFailoverObservation, error) {
		failoverCalls++
		assert.Equal(t, fixedNow, observedAt)
		return service.OpenCodeGoFailoverObservation{Action: service.OpenCodeGoFailoverActionSuspect, FailureCount: 1}, nil
	}

	adaptor := &Adaptor{
		protocol:             ProtocolResponses,
		workspaceSelected:    true,
		selectedWorkspaceUID: "workspace-provider",
		failoverAttempt:      &service.OpenCodeGoFailoverAttempt{},
	}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 42, UpstreamModelName: "kimi-k3"}}
	for _, test := range []struct {
		status int
		body   string
		want   int
	}{
		{status: http.StatusInternalServerError, body: `{"type":"upstream_error","message":"temporary"}`, want: 1},
		{status: http.StatusUnprocessableEntity, body: `{"type":"invalid_request_error","message":"bad input"}`, want: 1},
		{status: http.StatusInternalServerError, body: `{"type":"AuthError","message":"credential rejected"}`, want: 1},
		{status: http.StatusGatewayTimeout, body: `{"type":"upstream_error","message":"temporary"}`, want: 2},
	} {
		resp := &http.Response{
			StatusCode: test.status,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(test.body)),
		}
		apiErr, observation := adaptor.HandleNon2xxResponse(newAdaptorTestContext(), resp, info)
		require.NotNil(t, apiErr)
		require.NotNil(t, observation)
		assert.True(t, types.IsSkipRetryError(apiErr))
		assert.Equal(t, test.want, failoverCalls)
	}
}

func TestHandleNon2xxResponseReadFailureStillClassifiesKnownStatus(t *testing.T) {
	originalProviderObserver := observeOpenCodeGoProviderFailure
	originalFailoverObserver := observeOpenCodeGoFailoverFailure
	t.Cleanup(func() {
		observeOpenCodeGoProviderFailure = originalProviderObserver
		observeOpenCodeGoFailoverFailure = originalFailoverObserver
	})

	providerStatuses := make([]int, 0, 3)
	observeOpenCodeGoProviderFailure = func(_ int, _ string, _ string, failure service.OpenCodeGoProviderFailure, _ time.Time) (bool, error) {
		providerStatuses = append(providerStatuses, failure.StatusCode)
		return false, nil
	}
	failoverCalls := 0
	observeOpenCodeGoFailoverFailure = func(_ *service.OpenCodeGoFailoverAttempt, _ time.Time) (service.OpenCodeGoFailoverObservation, error) {
		failoverCalls++
		return service.OpenCodeGoFailoverObservation{Action: service.OpenCodeGoFailoverActionSuspect}, nil
	}

	adaptor := &Adaptor{
		protocol:             ProtocolResponses,
		workspaceSelected:    true,
		selectedWorkspaceUID: "workspace-provider",
		failoverAttempt:      &service.OpenCodeGoFailoverAttempt{},
	}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 42, UpstreamModelName: "kimi-k3"}}
	for index, test := range []struct {
		status       int
		wantFailover int
	}{
		{status: http.StatusServiceUnavailable, wantFailover: 1},
		{status: http.StatusUnauthorized, wantFailover: 1},
		{status: http.StatusTooManyRequests, wantFailover: 1},
	} {
		resp := &http.Response{
			StatusCode: test.status,
			Header:     http.Header{},
			Body:       errorReadCloser{err: io.ErrUnexpectedEOF},
		}

		apiErr, observation := adaptor.HandleNon2xxResponse(newAdaptorTestContext(), resp, info)

		require.NotNil(t, apiErr)
		require.NotNil(t, observation)
		assert.Equal(t, test.status, apiErr.StatusCode)
		assert.Equal(t, test.status, observation.StatusCode)
		assert.Equal(t, string(types.ErrorCodeReadResponseBodyFailed), observation.ErrorCode)
		assert.True(t, types.IsSkipRetryError(apiErr))
		assert.Equal(t, index+1, len(providerStatuses))
		assert.Equal(t, test.status, providerStatuses[index])
		assert.Equal(t, test.wantFailover, failoverCalls)
	}
}

func TestHandleNon2xxResponseCancellationDoesNotMutateHealthOrFailover(t *testing.T) {
	originalProviderObserver := observeOpenCodeGoProviderFailure
	originalFailoverObserver := observeOpenCodeGoFailoverFailure
	t.Cleanup(func() {
		observeOpenCodeGoProviderFailure = originalProviderObserver
		observeOpenCodeGoFailoverFailure = originalFailoverObserver
	})

	providerCalls := 0
	failoverCalls := 0
	observeOpenCodeGoProviderFailure = func(_ int, _ string, _ string, _ service.OpenCodeGoProviderFailure, _ time.Time) (bool, error) {
		providerCalls++
		return false, nil
	}
	observeOpenCodeGoFailoverFailure = func(_ *service.OpenCodeGoFailoverAttempt, _ time.Time) (service.OpenCodeGoFailoverObservation, error) {
		failoverCalls++
		return service.OpenCodeGoFailoverObservation{}, nil
	}

	adaptor := &Adaptor{
		protocol:             ProtocolResponses,
		workspaceSelected:    true,
		selectedWorkspaceUID: "workspace-provider",
		failoverAttempt:      &service.OpenCodeGoFailoverAttempt{},
	}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 42, UpstreamModelName: "kimi-k3"}}

	c := newAdaptorTestContext()
	requestContext, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(requestContext)
	cancel()
	apiErr, observation := adaptor.HandleNon2xxResponse(c, &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"type":"upstream_error"}`)),
	}, info)
	require.NotNil(t, apiErr)
	require.NotNil(t, observation)

	activeContext := newAdaptorTestContext()
	apiErr, observation = adaptor.HandleNon2xxResponse(activeContext, &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{},
		Body:       errorReadCloser{err: context.Canceled},
	}, info)
	require.NotNil(t, apiErr)
	require.NotNil(t, observation)

	assert.Zero(t, providerCalls)
	assert.Zero(t, failoverCalls)
}
