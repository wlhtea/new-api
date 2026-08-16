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

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
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

func TestHandleNon2xxResponseMarksEveryUpstreamErrorBranch(t *testing.T) {
	tests := []struct {
		name        string
		response    *http.Response
		wantMessage string
	}{
		{
			name:        "missing response",
			wantMessage: "OpenCode Go returned no response",
		},
		{
			name: "body read failure",
			response: &http.Response{
				StatusCode: http.StatusBadGateway,
				Header:     http.Header{},
				Body:       errorReadCloser{err: io.ErrUnexpectedEOF},
			},
			wantMessage: "failed to read OpenCode Go error response",
		},
		{
			name: "parsed upstream error with unknown internal name",
			response: &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"internal shard zen-primary failed"}}`)),
			},
			wantMessage: "internal shard zen-primary failed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			apiErr, observation := (&Adaptor{}).HandleNon2xxResponse(newAdaptorTestContext(), test.response, nil)

			require.NotNil(t, apiErr)
			require.NotNil(t, observation)
			assert.Equal(t, test.wantMessage, apiErr.Error())
			assert.True(t, service.IsOpenCodeGoUpstreamRelayError(apiErr))

			publicErr := service.PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, apiErr)
			require.NotSame(t, apiErr, publicErr)
			assert.Equal(t, http.StatusTooManyRequests, publicErr.StatusCode)
			assert.Equal(t, service.OpenCodeGoPublicOverloadMessage, publicErr.Error())
			publicOpenAIError := publicErr.ToOpenAIError()
			assert.Equal(t, service.OpenCodeGoPublicRateLimitErrorCode, publicOpenAIError.Type)
			assert.Equal(t, service.OpenCodeGoPublicRateLimitErrorCode, publicOpenAIError.Code)
			assert.Equal(t, test.wantMessage, apiErr.Error())
		})
	}
}

func TestHandleNon2xxResponseRetryPolicyDiffersByOpenCodeChannelType(t *testing.T) {
	for _, test := range []struct {
		name          string
		channelType   int
		wantSkipRetry bool
	}{
		{name: "account pool owns dedicated retry", channelType: constant.ChannelTypeOpenCodeGo, wantSkipRetry: true},
		{name: "api key row uses generic retry", channelType: constant.ChannelTypeOpenCodeAPIKey, wantSkipRetry: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: test.channelType}}
			resp := &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"upstream_error","message":"temporary"}}`)),
			}

			apiErr, _ := (&Adaptor{}).HandleNon2xxResponse(newAdaptorTestContext(), resp, info)

			require.NotNil(t, apiErr)
			assert.Equal(t, test.wantSkipRetry, types.IsSkipRetryError(apiErr))
			assert.True(t, service.IsOpenCodeGoUpstreamRelayError(apiErr))
		})
	}
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
	require.Nil(t, observation)
	assert.ErrorIs(t, apiErr, errOpenCodeGoResponseLimitExceeded)
	assert.Equal(t, types.ErrorOriginUpstreamHTTP, apiErr.Provenance().Origin)
	assert.Equal(t, "error_body_limit", apiErr.Provenance().Subtype)
	assert.Equal(t, http.StatusBadGateway, apiErr.Provenance().RawStatusCode)
	assert.True(t, service.IsOpenCodeGoUpstreamRelayError(apiErr))
	assert.NotContains(t, apiErr.Error(), strings.Repeat("x", 100))
}

func TestHandleNon2xxResponseBoundsFailedErrorBodyReadsWithoutLeakingContent(t *testing.T) {
	oldDisableEnabled := common.AutomaticDisableChannelEnabled
	oldDisableRanges := append([]operation_setting.StatusCodeRange(nil), operation_setting.AutomaticDisableStatusCodeRanges...)
	oldRetryRanges := append([]operation_setting.StatusCodeRange(nil), operation_setting.AutomaticRetryStatusCodeRanges...)
	common.AutomaticDisableChannelEnabled = true
	operation_setting.AutomaticDisableStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 400, End: 599}}
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 400, End: 599}}
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = oldDisableEnabled
		operation_setting.AutomaticDisableStatusCodeRanges = oldDisableRanges
		operation_setting.AutomaticRetryStatusCodeRanges = oldRetryRanges
	})

	const privateBody = "private-upstream-body"
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, statusCode := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusServiceUnavailable} {
			for _, failureKind := range []string{"timeout", "limit"} {
				name := fmt.Sprintf("channel_%d_status_%d_%s", channelType, statusCode, failureKind)
				t.Run(name, func(t *testing.T) {
					info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: channelType}}
					limits := openCodeGoErrorResponseLimits{
						bodyBytes:   int64(len(privateBody) - 1),
						readTimeout: time.Second,
					}
					var source io.ReadCloser
					var closeCalls func() int32
					if failureKind == "timeout" {
						blocking := newCloseBlockingResponseBody()
						source = blocking
						closeCalls = blocking.closeCalls.Load
						limits.readTimeout = 20 * time.Millisecond
					} else {
						bounded := &responseLimitTestBody{reader: strings.NewReader(privateBody)}
						source = bounded
						closeCalls = func() int32 { return int32(bounded.closeCalls) }
					}
					response := &http.Response{
						StatusCode: statusCode,
						Header:     http.Header{"Retry-After": []string{"120"}},
						Body:       source,
					}
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

					apiErr, observation := (&Adaptor{}).handleNon2xxResponseWithLimits(
						c,
						response,
						info,
						limits,
					)

					require.NotNil(t, apiErr)
					require.Nil(t, observation)
					wantSubtype := "error_body_limit"
					wantCause := errOpenCodeGoResponseLimitExceeded
					if failureKind == "timeout" {
						wantSubtype = "error_body_timeout"
						wantCause = errOpenCodeGoResponseReadTimeout
					}
					assert.ErrorIs(t, apiErr, wantCause)
					assert.Equal(t, statusCode, apiErr.StatusCode)
					assert.Equal(t, types.ErrorOriginUpstreamHTTP, apiErr.Provenance().Origin)
					assert.Equal(t, wantSubtype, apiErr.Provenance().Subtype)
					assert.Equal(t, statusCode, apiErr.Provenance().RawStatusCode)
					rawStatus, hasRawStatus := service.OpenCodeGoUpstreamRelayStatusCode(apiErr)
					require.True(t, hasRawStatus)
					assert.Equal(t, statusCode, rawStatus)
					assert.True(t, service.IsOpenCodeGoUpstreamRelayError(apiErr))
					assert.Equal(t, channelType == constant.ChannelTypeOpenCodeGo, types.IsSkipRetryError(apiErr))
					assert.Equal(t, statusCode, service.OpenCodeGoRelayPolicyStatusCode(apiErr))
					assert.NotContains(t, apiErr.Error(), privateBody)
					assert.Empty(t, recorder.Header().Get("Retry-After"))
					assert.Equal(t, int32(1), closeCalls())

					wantDedicatedRetry := channelType == constant.ChannelTypeOpenCodeGo && statusCode == http.StatusServiceUnavailable
					assert.Equal(t, wantDedicatedRetry, service.ShouldRetryOpenCodeGoRelayError(channelType, apiErr))
					genericRetryEligible := !service.IsOpenCodeGoRawInvalidRequestError(apiErr) &&
						!types.IsSkipRetryError(apiErr) &&
						!operation_setting.IsAlwaysSkipRetryCode(apiErr.GetErrorCode()) &&
						operation_setting.ShouldRetryByStatusCode(service.OpenCodeGoRelayPolicyStatusCode(apiErr))
					assert.Equal(t, channelType == constant.ChannelTypeOpenCodeAPIKey && statusCode == http.StatusServiceUnavailable, genericRetryEligible)

					publicErr := service.PublicOpenCodeGoRelayError(channelType, apiErr)
					require.NotNil(t, publicErr)
					require.NotSame(t, apiErr, publicErr)
					assert.NotContains(t, publicErr.Error(), privateBody)
					if statusCode == http.StatusBadRequest || statusCode == http.StatusUnprocessableEntity {
						assert.True(t, service.IsOpenCodeGoRawInvalidRequestError(apiErr))
						assert.False(t, service.ShouldDisableChannel(apiErr))
						assert.Equal(t, http.StatusBadRequest, publicErr.StatusCode)
						assert.Equal(t, constant.OpenCodeGoPublicInvalidRequestMessage, publicErr.Error())
						assert.True(t, service.IsOpenCodeGoFixedInvalidRequestProjection(publicErr))
						publicBody := publicErr.ToOpenAIError()
						assert.Equal(t, constant.OpenCodeGoPublicInvalidRequestCode, publicBody.Type)
						assert.Equal(t, constant.OpenCodeGoPublicInvalidRequestCode, publicBody.Code)
						return
					}
					assert.False(t, service.IsOpenCodeGoRawInvalidRequestError(apiErr))
					assert.Equal(t, http.StatusTooManyRequests, publicErr.StatusCode)
					assert.Equal(t, service.OpenCodeGoPublicOverloadMessage, publicErr.Error())
					assert.False(t, service.IsOpenCodeGoFixedInvalidRequestProjection(publicErr))
				})
			}
		}
	}
}

func TestHandleNon2xxResponseErrorBodyExactLimitAndOneByteOver(t *testing.T) {
	body := `{"error":{"type":"invalid_request_error","message":"invalid"}}`
	for _, test := range []struct {
		name      string
		payload   string
		wantLimit bool
	}{
		{name: "at limit", payload: body},
		{name: "one byte over", payload: body + " ", wantLimit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &responseLimitTestBody{reader: strings.NewReader(test.payload)}
			response := &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{},
				Body:       source,
			}

			apiErr, observation := (&Adaptor{}).handleNon2xxResponseWithLimits(
				newAdaptorTestContext(),
				response,
				nil,
				openCodeGoErrorResponseLimits{bodyBytes: int64(len(body)), readTimeout: time.Second},
			)

			require.NotNil(t, apiErr)
			assert.Equal(t, 1, source.closeCalls)
			if test.wantLimit {
				require.Nil(t, observation)
				assert.Equal(t, "error_body_limit", apiErr.Provenance().Subtype)
				assert.Equal(t, types.ErrorOriginUpstreamHTTP, apiErr.Provenance().Origin)
				assert.Equal(t, http.StatusBadRequest, apiErr.Provenance().RawStatusCode)
				assert.True(t, service.IsOpenCodeGoUpstreamRelayError(apiErr))
				publicErr := service.PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, apiErr)
				require.NotNil(t, publicErr)
				assert.True(t, service.IsOpenCodeGoFixedInvalidRequestProjection(publicErr))
				return
			}
			require.NotNil(t, observation)
			assert.Equal(t, types.ErrorOriginUpstreamHTTP, apiErr.Provenance().Origin)
			assert.Equal(t, http.StatusBadRequest, apiErr.Provenance().RawStatusCode)
		})
	}
}

func TestHandleNon2xxResponseSlowDripUsesWholeErrorBodyDeadline(t *testing.T) {
	source := newSlowDripResponseBody(`{"error":{"message":"private"}}`, 10*time.Millisecond)
	response := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{},
		Body:       source,
	}
	startedAt := time.Now()

	apiErr, observation := (&Adaptor{}).handleNon2xxResponseWithLimits(
		newAdaptorTestContext(),
		response,
		nil,
		openCodeGoErrorResponseLimits{bodyBytes: 128, readTimeout: 25 * time.Millisecond},
	)

	require.NotNil(t, apiErr)
	require.Nil(t, observation)
	assert.ErrorIs(t, apiErr, errOpenCodeGoResponseReadTimeout)
	assert.Equal(t, types.ErrorOriginUpstreamHTTP, apiErr.Provenance().Origin)
	assert.Equal(t, "error_body_timeout", apiErr.Provenance().Subtype)
	assert.Equal(t, http.StatusServiceUnavailable, apiErr.Provenance().RawStatusCode)
	assert.True(t, service.IsOpenCodeGoUpstreamRelayError(apiErr))
	assert.True(t, service.ShouldRetryOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, apiErr))
	assert.NotContains(t, apiErr.Error(), "private")
	assert.Less(t, time.Since(startedAt), 250*time.Millisecond)
	assert.Equal(t, int32(1), source.closeCalls.Load())
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
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
		ChannelType:       constant.ChannelTypeOpenCodeGo,
		ChannelId:         42,
		UpstreamModelName: "kimi-k3",
	}}
	for _, test := range []struct {
		status int
		body   string
		want   int
	}{
		{status: http.StatusInternalServerError, body: `{"type":"upstream_error","message":"temporary"}`, want: 1},
		{status: http.StatusUnprocessableEntity, body: `{"type":"invalid_request_error","message":"bad input"}`, want: 1},
		// An AuthError body on a 5xx response is not authoritative account
		// evidence; only 401/403 may classify credential/workspace health.
		{status: http.StatusInternalServerError, body: `{"type":"AuthError","message":"credential rejected"}`, want: 2},
		{status: http.StatusGatewayTimeout, body: `{"type":"upstream_error","message":"temporary"}`, want: 3},
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
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
		ChannelType:       constant.ChannelTypeOpenCodeGo,
		ChannelId:         42,
		UpstreamModelName: "kimi-k3",
	}}
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

func TestHandleNon2xxResponseRawClientStatusReadFailuresDoNotMutateHealthOrFailover(t *testing.T) {
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

	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
		ChannelType:       constant.ChannelTypeOpenCodeGo,
		ChannelId:         42,
		UpstreamModelName: "kimi-k3",
	}}
	readFailures := []struct {
		name     string
		body     func() io.ReadCloser
		limits   openCodeGoErrorResponseLimits
		wantRule string
	}{
		{
			name:     "body limit",
			body:     func() io.ReadCloser { return io.NopCloser(strings.NewReader("too-large")) },
			limits:   openCodeGoErrorResponseLimits{bodyBytes: 4, readTimeout: time.Second},
			wantRule: "error_body_limit",
		},
		{
			name:     "body timeout",
			body:     func() io.ReadCloser { return newCloseBlockingResponseBody() },
			limits:   openCodeGoErrorResponseLimits{bodyBytes: 64, readTimeout: 20 * time.Millisecond},
			wantRule: "error_body_timeout",
		},
	}
	for _, statusCode := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		for _, readFailure := range readFailures {
			name := fmt.Sprintf("status_%d_%s", statusCode, readFailure.name)
			t.Run(name, func(t *testing.T) {
				adaptor := &Adaptor{
					protocol:             ProtocolResponses,
					workspaceSelected:    true,
					selectedWorkspaceUID: "workspace-provider",
					failoverAttempt:      &service.OpenCodeGoFailoverAttempt{},
				}
				apiErr, observation := adaptor.handleNon2xxResponseWithLimits(
					newAdaptorTestContext(),
					&http.Response{
						StatusCode: statusCode,
						Header:     http.Header{"Retry-After": []string{"120"}},
						Body:       readFailure.body(),
					},
					info,
					readFailure.limits,
				)

				require.NotNil(t, apiErr)
				require.Nil(t, observation)
				assert.Equal(t, types.ErrorOriginUpstreamHTTP, apiErr.Provenance().Origin)
				assert.Equal(t, readFailure.wantRule, apiErr.Provenance().Subtype)
				assert.Equal(t, statusCode, apiErr.Provenance().RawStatusCode)
				assert.True(t, types.IsSkipRetryError(apiErr))
				assert.True(t, service.IsOpenCodeGoUpstreamRelayError(apiErr))
				assert.False(t, service.ShouldRetryOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, apiErr))
				assert.False(t, service.ShouldDisableChannel(apiErr))
				assert.True(t, service.IsOpenCodeGoFixedInvalidRequestProjection(
					service.PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, apiErr),
				))
				assert.Zero(t, providerCalls)
				assert.Zero(t, failoverCalls)
			})
		}
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
	require.Nil(t, observation)
	assert.Equal(t, types.ErrorOriginLocalCancel, apiErr.Provenance().Origin)

	activeContext := newAdaptorTestContext()
	apiErr, observation = adaptor.HandleNon2xxResponse(activeContext, &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{},
		Body:       errorReadCloser{err: context.Canceled},
	}, info)
	require.NotNil(t, apiErr)
	require.NotNil(t, observation)

	assert.Equal(t, 1, providerCalls)
	assert.Equal(t, 1, failoverCalls)
	assert.Equal(t, types.ErrorOriginUpstreamHTTP, apiErr.Provenance().Origin)
}
