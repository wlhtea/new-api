package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublicOpenCodeGoRelayErrorHidesPrivateDetails(t *testing.T) {
	tests := []struct {
		name string
		err  *types.NewAPIError
	}{
		{
			name: "message branding",
			err: types.NewOpenAIError(
				errors.New("failed to read OpenCode Go error response"),
				types.ErrorCodeReadResponseBodyFailed,
				http.StatusBadGateway,
			),
		},
		{
			name: "console endpoint unavailable",
			err: types.WithOpenAIError(types.OpenAIError{
				Message: "Error from provider (Console Go): Upstream request failed: Endpoint is unavailable.",
				Type:    "upstream_error",
				Code:    "upstream_error",
			}, http.StatusServiceUnavailable),
		},
		{
			name: "unnamed internal endpoint",
			err: types.WithOpenAIError(types.OpenAIError{
				Message: "internal endpoint zen-primary failed",
				Type:    "upstream_error",
				Code:    "upstream_error",
			}, http.StatusBadGateway),
		},
		{
			name: "mixed case code",
			err: types.WithOpenAIError(types.OpenAIError{
				Message: "request failed",
				Type:    "upstream_error",
				Code:    "Open_Code_Go_WorkSpace_Unavailable",
			}, http.StatusInternalServerError),
		},
		{
			name: "private metadata",
			err: types.WithOpenAIError(types.OpenAIError{
				Message:  "request failed",
				Type:     "upstream_error",
				Code:     "upstream_error",
				Metadata: []byte(`{"workspace":"wrk_private"}`),
			}, http.StatusInternalServerError),
		},
		{
			name: "escaped private metadata",
			err: types.WithOpenAIError(types.OpenAIError{
				Message:  "request failed",
				Type:     "upstream_error",
				Code:     "upstream_error",
				Metadata: []byte(`{"work\u0073pace":"wrk\u005fprivate"}`),
			}, http.StatusInternalServerError),
		},
		{
			name: "claude workspace message",
			err: types.WithClaudeError(types.ClaudeError{
				Message: "selected workspace credential is unavailable",
				Type:    "upstream_error",
			}, http.StatusInternalServerError),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			originalStatus := test.err.StatusCode
			originalMessage := test.err.Error()
			publicErr := PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, test.err)

			require.NotSame(t, test.err, publicErr)
			assert.Equal(t, http.StatusTooManyRequests, publicErr.StatusCode)
			publicOpenAIError := publicErr.ToOpenAIError()
			assert.Equal(t, OpenCodeGoPublicOverloadMessage, publicOpenAIError.Message)
			assert.Equal(t, OpenCodeGoPublicRateLimitErrorCode, publicOpenAIError.Type)
			assert.Equal(t, OpenCodeGoPublicRateLimitErrorCode, publicOpenAIError.Code)
			assert.Empty(t, publicOpenAIError.Metadata)
			for _, marker := range []string{"opencode", "console go", "workspace", "wrk_"} {
				assert.NotContains(t, strings.ToLower(fmt.Sprint(publicOpenAIError)), marker)
			}
			assert.Equal(t, originalStatus, test.err.StatusCode)
			assert.Equal(t, originalMessage, test.err.Error())
		})
	}
}

func TestOpenCodeGoErrorHasPrivateDetailDecodesJSONEscapes(t *testing.T) {
	assert.True(t, OpenCodeGoErrorHasPrivateDetail(`{"message":"Console G\u006f is unavailable"}`))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail(`{"work\u0073pace":"wrk\u005fprivate"}`))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail(`{"details":[{"Open-Code":"internal"}]}`))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail("Console_Go is unavailable"))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail("work-space is unavailable"))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail("Authorization: Bearer private-upstream-token"))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail("Proxy-Authorization: Basic private-proxy-credential"))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail("Cookie: session=private-session"))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail("x-api-key=private-api-key"))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail("socks5://proxy-user:proxy-password@10.0.0.8:1080"))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail(`{"x-api-key":"private-api-key"}`))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail(`{"message":"session_id=private-session"}`))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail("request_id=req_private"))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail("traceId=trace_private"))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail("proxy_host=proxy.internal"))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail("upstream key sk-private-key-value"))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail("Open/Code upstream failed"))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail("Open.Code upstream failed"))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail("Console.Go upstream failed"))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail("work.space is unavailable"))
	assert.True(t, OpenCodeGoErrorHasPrivateDetail(`{"requestId":"req_private"}`))
	assert.False(t, OpenCodeGoErrorHasPrivateDetail(`{"message":"provider request timed out"}`))
}

func TestPublicOpenCodeGoRelayErrorPreservesPublicProviderErrors(t *testing.T) {
	providerErr := types.WithOpenAIError(types.OpenAIError{
		Message: "provider request timed out",
		Type:    "upstream_error",
		Code:    "timeout",
	}, http.StatusGatewayTimeout)

	assert.Same(t, providerErr, PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, providerErr))
}

func TestPublicOpenCodeGoRelayErrorHidesMarkedUpstreamWithoutKnownMarkers(t *testing.T) {
	upstreamErr := types.WithOpenAIError(types.OpenAIError{
		Message: "internal shard zen-primary failed",
		Type:    "upstream_error",
		Code:    "upstream_error",
	}, http.StatusServiceUnavailable)
	originalMessage := upstreamErr.Error()

	marked := MarkOpenCodeGoUpstreamRelayError(upstreamErr)
	publicErr := PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, marked)

	require.Same(t, upstreamErr, marked)
	assert.True(t, IsOpenCodeGoUpstreamRelayError(marked))
	assert.Equal(t, originalMessage, marked.Error())
	require.NotSame(t, marked, publicErr)
	assert.Equal(t, http.StatusTooManyRequests, publicErr.StatusCode)
	assert.Equal(t, OpenCodeGoPublicOverloadMessage, publicErr.Error())
	assert.False(t, IsOpenCodeGoUpstreamRelayError(publicErr))
}

func TestMarkOpenCodeGoUpstreamRelayErrorIsIdempotent(t *testing.T) {
	relayErr := types.NewOpenAIError(
		errors.New("internal shard zen-primary failed"),
		types.ErrorCodeBadResponse,
		http.StatusBadGateway,
	)

	first := MarkOpenCodeGoUpstreamRelayError(relayErr)
	second := MarkOpenCodeGoUpstreamRelayError(first)

	require.Same(t, first, second)
	assert.Equal(t, "internal shard zen-primary failed", second.Error())
	assert.True(t, IsOpenCodeGoUpstreamRelayError(second))
}

func TestOpenCodeGoAdminErrorWithStatusCodeRedactsCredentialsAndSessionValues(t *testing.T) {
	relayErr := types.WithOpenAIError(types.OpenAIError{
		Message: "invalid request",
		Type:    "invalid_request_error",
		Code:    "invalid_request_error",
	}, http.StatusBadGateway)
	relayErr.SetMessage(
		`{"error":{"message":"Authorization: Bearer private-bearer; x-api-key=private-key; Cookie: session=private-cookie; session_id=private-session; proxy=socks5://proxy-user:proxy-password@10.0.0.8:1080; endpoint=http://internal-control.local/v1/private"}}`,
	)
	MarkOpenCodeGoUpstreamRelayErrorWithStatus(relayErr, http.StatusOK)

	adminMessage := OpenCodeGoAdminErrorWithStatusCode(relayErr)

	assert.Contains(t, adminMessage, "status_code=502")
	for _, secret := range []string{
		"private-bearer",
		"private-key",
		"private-cookie",
		"private-session",
		"proxy-user",
		"proxy-password",
		"10.0.0.8",
		"internal-control.local",
		"/v1/private",
	} {
		assert.NotContains(t, adminMessage, secret)
	}
	assert.Contains(t, adminMessage, "[redacted]")
}

func TestOpenCodeGoAdminErrorRedactsQuotedAndEscapedJSONFields(t *testing.T) {
	relayErr := types.WithOpenAIError(types.OpenAIError{
		Message: "invalid request",
		Type:    "invalid_request_error",
		Code:    "invalid_request_error",
	}, http.StatusBadGateway)
	relayErr.SetMessage(
		`{"error":{"authoriz\u0061tion":"Bearer quoted-private-bearer","x-api-key":"quoted-private-key","cookie":{"session":"quoted-private-session"},"proxy_url":"socks5://quoted-user:quoted-password@10.0.0.9:1080","message":"useful validation detail"}}`,
	)
	MarkOpenCodeGoUpstreamRelayErrorWithStatus(relayErr, http.StatusOK)

	adminMessage := OpenCodeGoAdminErrorWithStatusCode(relayErr)

	assert.Contains(t, adminMessage, "status_code=502")
	assert.Contains(t, adminMessage, "useful validation detail")
	for _, secret := range []string{
		"quoted-private-bearer",
		"quoted-private-key",
		"quoted-private-session",
		"quoted-user",
		"quoted-password",
		"10.0.0.9",
	} {
		assert.NotContains(t, adminMessage, secret)
	}
	assert.Contains(t, adminMessage, "[redacted]")
}

func TestPublicOpenCodeGoRelayErrorUsesNeutralMessageForAdditionalPrivateClientDetails(t *testing.T) {
	for _, message := range []string{
		"invalid tool schema request_id=req_private",
		"invalid tool schema traceId=trace_private",
		"invalid tool schema proxy_host=proxy.internal",
		"invalid tool schema key sk-private-key-value",
		"Open/Code invalid tool schema",
	} {
		t.Run(message, func(t *testing.T) {
			internalErr := MarkOpenCodeGoUpstreamRelayError(types.WithOpenAIError(types.OpenAIError{
				Message: message,
				Type:    "invalid_request_error",
				Code:    "invalid_prompt",
			}, http.StatusBadRequest))

			publicErr := PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeAPIKey, internalErr)

			require.Equal(t, http.StatusBadRequest, publicErr.StatusCode)
			require.Equal(t, constant.OpenCodeGoPublicInvalidRequestMessage, publicErr.Error())
		})
	}
}

func TestPublicOpenCodeGoRelayErrorDoesNotRewriteOtherChannels(t *testing.T) {
	internalErr := types.WithOpenAIError(types.OpenAIError{
		Message: "workspace is unavailable",
		Type:    "upstream_error",
		Code:    "workspace_unavailable",
	}, http.StatusServiceUnavailable)

	assert.Same(t, internalErr, PublicOpenCodeGoRelayError(constant.ChannelTypeOpenAI, internalErr))
}

func TestPublicOpenCodeGoRelayErrorHidesPoolSentinelWithoutChannelContext(t *testing.T) {
	internalErr := types.NewOpenAIError(
		fmt.Errorf("setup request header failed: %w", ErrOpenCodeGoNoEligibleWorkspace),
		types.ErrorCodeDoRequestFailed,
		http.StatusInternalServerError,
	)

	publicErr := PublicOpenCodeGoRelayError(constant.ChannelTypeUnknown, internalErr)
	require.NotSame(t, internalErr, publicErr)
	assert.Equal(t, http.StatusTooManyRequests, publicErr.StatusCode)
	assert.ErrorIs(t, internalErr, ErrOpenCodeGoNoEligibleWorkspace)
}

type opaqueOpenCodeGoPoolSentinelError struct {
	cause error
}

func (e opaqueOpenCodeGoPoolSentinelError) Error() string {
	return "opaque local relay setup failure"
}

func (e opaqueOpenCodeGoPoolSentinelError) Unwrap() error {
	return e.cause
}

func TestPublicOpenCodeGoRelayErrorHidesPrivatePoolSentinelTextForAPIKeyChannel(t *testing.T) {
	internalErr := types.NewOpenAIError(
		fmt.Errorf("setup request header failed: %w", ErrOpenCodeGoNoEligibleWorkspace),
		types.ErrorCodeDoRequestFailed,
		http.StatusInternalServerError,
	)

	publicErr := PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeAPIKey, internalErr)
	require.NotSame(t, internalErr, publicErr)
	assert.Equal(t, http.StatusTooManyRequests, publicErr.StatusCode)
	assert.Equal(t, constant.OpenCodeGoPublicOverloadMessage, publicErr.Error())
}

func TestPublicOpenCodeGoRelayErrorDoesNotUsePoolSentinelBranchForAPIKeyChannel(t *testing.T) {
	internalErr := types.NewOpenAIError(
		opaqueOpenCodeGoPoolSentinelError{cause: ErrOpenCodeGoNoEligibleWorkspace},
		types.ErrorCodeDoRequestFailed,
		http.StatusInternalServerError,
	)

	assert.ErrorIs(t, internalErr, ErrOpenCodeGoNoEligibleWorkspace)
	assert.Same(t, internalErr, PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeAPIKey, internalErr))
}

func TestPublicOpenCodeGoRelayErrorMatrix(t *testing.T) {
	marked := func(statusCode int, message string) *types.NewAPIError {
		return MarkOpenCodeGoUpstreamRelayError(types.NewOpenAIError(
			errors.New(message),
			types.ErrorCodeBadResponse,
			statusCode,
			types.ErrOptionWithSkipRetry(),
		))
	}
	tests := []struct {
		name        string
		err         *types.NewAPIError
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			name: "client request body",
			err: types.NewOpenAIError(
				errors.New("invalid JSON request body"),
				types.ErrorCodeInvalidRequest,
				http.StatusInternalServerError,
			),
			wantStatus: http.StatusBadRequest,
			wantCode:   constant.OpenCodeGoPublicInvalidRequestCode,
		},
		{
			name: "client parameter override",
			err: types.NewOpenAIError(
				errors.New("channel parameter override is invalid"),
				types.ErrorCodeChannelParamOverrideInvalid,
				http.StatusInternalServerError,
			),
			wantStatus: http.StatusBadRequest,
			wantCode:   constant.OpenCodeGoPublicInvalidRequestCode,
		},
		{
			name: "raw client 400 survives later mapped 503",
			err: MarkOpenCodeGoUpstreamRelayErrorWithStatus(types.NewOpenAIError(
				errors.New("request rejected"),
				types.ErrorCodeInvalidRequest,
				http.StatusServiceUnavailable,
			), http.StatusBadRequest),
			wantStatus: http.StatusBadRequest,
			wantCode:   constant.OpenCodeGoPublicInvalidRequestCode,
		},
		{
			name: "provider invalid request keeps actionable safe detail",
			err: MarkOpenCodeGoUpstreamRelayError(types.WithOpenAIError(types.OpenAIError{
				Message: "Error from provider (Console Go): Upstream request failed: [invalid_request_error] Failed to deserialize the JSON body: messages[0].role is required",
				Type:    "invalid_request_error",
				Code:    "invalid_request_error",
			}, http.StatusBadRequest, types.ErrOptionWithSkipRetry())),
			wantStatus:  http.StatusBadRequest,
			wantCode:    constant.OpenCodeGoPublicInvalidRequestCode,
			wantMessage: "Failed to deserialize the JSON body: messages[0].role is required",
		},
		{
			name: "provider validation wrapper keeps actionable safe detail",
			err: MarkOpenCodeGoUpstreamRelayError(types.WithOpenAIError(types.OpenAIError{
				Message: "OpenCode Go rejected messages[0].role: Unsupported role",
				Type:    "validation_error",
				Code:    "invalid_value",
			}, http.StatusUnprocessableEntity, types.ErrOptionWithSkipRetry())),
			wantStatus:  http.StatusBadRequest,
			wantCode:    constant.OpenCodeGoPublicInvalidRequestCode,
			wantMessage: "messages[0].role: Unsupported role",
		},
		{
			name: "provider OpenCode wrapper keeps actionable safe detail",
			err: MarkOpenCodeGoUpstreamRelayError(types.WithOpenAIError(types.OpenAIError{
				Message: "Error from provider (OpenCode): [validation_error] messages[0].content is required",
				Type:    "validation_error",
				Code:    "invalid_value",
			}, http.StatusUnprocessableEntity, types.ErrOptionWithSkipRetry())),
			wantStatus:  http.StatusBadRequest,
			wantCode:    constant.OpenCodeGoPublicInvalidRequestCode,
			wantMessage: "messages[0].content is required",
		},
		{
			name: "provider ConsoleGo wrapper keeps actionable safe detail",
			err: MarkOpenCodeGoUpstreamRelayError(types.WithOpenAIError(types.OpenAIError{
				Message: "ConsoleGo rejected input[0].content: expected a string",
				Type:    "validation_error",
				Code:    "invalid_value",
			}, http.StatusUnprocessableEntity, types.ErrOptionWithSkipRetry())),
			wantStatus:  http.StatusBadRequest,
			wantCode:    constant.OpenCodeGoPublicInvalidRequestCode,
			wantMessage: "input[0].content: expected a string",
		},
		{
			name: "provider invalid request hides private detail",
			err: MarkOpenCodeGoUpstreamRelayError(types.WithOpenAIError(types.OpenAIError{
				Message: "invalid request for workspace wrk_private",
				Type:    "invalid_request_error",
				Code:    "invalid_request_error",
			}, http.StatusBadRequest, types.ErrOptionWithSkipRetry())),
			wantStatus:  http.StatusBadRequest,
			wantCode:    constant.OpenCodeGoPublicInvalidRequestCode,
			wantMessage: constant.OpenCodeGoPublicInvalidRequestMessage,
		},
		{
			name: "caller canceled",
			err: types.NewOpenAIError(
				fmt.Errorf("request context done: %w", context.Canceled),
				types.ErrorCodeBadResponse,
				http.StatusInternalServerError,
			),
			wantStatus: 499,
			wantCode:   constant.OpenCodeGoPublicRequestCanceledCode,
		},
		{
			name: "stale selected workspace",
			err: types.NewOpenAIError(
				fmt.Errorf("resolve request HTTP client failed: %w", ErrOpenCodeGoIdentityProxySelectionStale),
				types.ErrorCodeDoRequestFailed,
				http.StatusInternalServerError,
			),
			wantStatus: http.StatusTooManyRequests,
			wantCode:   constant.OpenCodeGoPublicRateLimitErrorCode,
		},
		{
			name: "internal protocol configuration",
			err: types.NewOpenAIError(
				errors.New("OpenCode Go protocol is not configured"),
				types.ErrorCodeInvalidRequest,
				http.StatusBadRequest,
			),
			wantStatus: http.StatusTooManyRequests,
			wantCode:   constant.OpenCodeGoPublicRateLimitErrorCode,
		},
		{
			name: "upstream transport failure",
			err: types.NewOpenAIError(
				errors.New("upstream error: do request failed"),
				types.ErrorCodeDoRequestFailed,
				http.StatusInternalServerError,
			),
			wantStatus: http.StatusTooManyRequests,
			wantCode:   constant.OpenCodeGoPublicRateLimitErrorCode,
		},
		{
			name:       "upstream stream incomplete",
			err:        marked(http.StatusBadGateway, "OpenCode Go upstream stream ended before completion"),
			wantStatus: http.StatusTooManyRequests,
			wantCode:   constant.OpenCodeGoPublicRateLimitErrorCode,
		},
		{
			name: "upstream JSON truncated",
			err: types.NewOpenAIError(
				errors.New("unexpected end of JSON input"),
				types.ErrorCodeBadResponseBody,
				http.StatusInternalServerError,
			),
			wantStatus: http.StatusTooManyRequests,
			wantCode:   constant.OpenCodeGoPublicRateLimitErrorCode,
		},
		{name: "marked upstream 500", err: marked(http.StatusInternalServerError, "provider failed"), wantStatus: http.StatusTooManyRequests, wantCode: constant.OpenCodeGoPublicRateLimitErrorCode},
		{name: "marked upstream 502", err: marked(http.StatusBadGateway, "provider failed"), wantStatus: http.StatusTooManyRequests, wantCode: constant.OpenCodeGoPublicRateLimitErrorCode},
		{name: "marked upstream 503", err: marked(http.StatusServiceUnavailable, "provider failed"), wantStatus: http.StatusTooManyRequests, wantCode: constant.OpenCodeGoPublicRateLimitErrorCode},
		{name: "marked upstream 504", err: marked(http.StatusGatewayTimeout, "provider failed"), wantStatus: http.StatusTooManyRequests, wantCode: constant.OpenCodeGoPublicRateLimitErrorCode},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			originalStatus := test.err.StatusCode
			originalMessage := test.err.Error()

			publicErr := PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, test.err)

			require.NotSame(t, test.err, publicErr)
			assert.Equal(t, test.wantStatus, publicErr.StatusCode)
			assert.Equal(t, test.wantCode, fmt.Sprint(publicErr.ToOpenAIError().Code))
			if test.wantMessage != "" {
				assert.Equal(t, test.wantMessage, publicErr.Error())
			}
			assert.Equal(t, originalStatus, test.err.StatusCode)
			assert.Equal(t, originalMessage, test.err.Error())
		})
	}
}

func TestPublicOpenCodeGoRelayErrorKeepsUnmarkedLocalFailure(t *testing.T) {
	localErr := types.NewOpenAIError(
		errors.New("request setup failed"),
		types.ErrorCodeDoRequestFailed,
		http.StatusInternalServerError,
	)

	assert.Same(t, localErr, PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, localErr))
}

func TestPublicOpenCodeGoRelayErrorDoesNotProjectStaleSentinelForOtherChannel(t *testing.T) {
	otherErr := types.NewOpenAIError(
		ErrOpenCodeGoIdentityProxySelectionStale,
		types.ErrorCodeDoRequestFailed,
		http.StatusInternalServerError,
	)

	assert.Same(t, otherErr, PublicOpenCodeGoRelayError(constant.ChannelTypeOpenAI, otherErr))
}

func TestPublicOpenCodeGoRelayErrorProjectsAPIKeyChannelUpstreamErrors(t *testing.T) {
	tests := []struct {
		name        string
		err         *types.NewAPIError
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			name: "unclassified upstream failure is fail closed",
			err: MarkOpenCodeGoUpstreamRelayError(types.WithOpenAIError(types.OpenAIError{
				Message: "backend shard zeta rejected the request",
				Type:    "upstream_error",
				Code:    "upstream_error",
			}, http.StatusBadGateway, types.ErrOptionWithSkipRetry())),
			wantStatus:  http.StatusTooManyRequests,
			wantCode:    constant.OpenCodeGoPublicRateLimitErrorCode,
			wantMessage: constant.OpenCodeGoPublicOverloadMessage,
		},
		{
			name: "explicit client request keeps safe detail",
			err: MarkOpenCodeGoUpstreamRelayError(types.WithOpenAIError(types.OpenAIError{
				Message: "Error from provider (Console Go): [invalid_request_error] messages[0].role is required",
				Type:    "invalid_request_error",
				Code:    "invalid_request_error",
			}, http.StatusBadRequest, types.ErrOptionWithSkipRetry())),
			wantStatus:  http.StatusBadRequest,
			wantCode:    constant.OpenCodeGoPublicInvalidRequestCode,
			wantMessage: "messages[0].role is required",
		},
		{
			name: "explicit client request with private detail uses neutral message",
			err: MarkOpenCodeGoUpstreamRelayError(types.WithOpenAIError(types.OpenAIError{
				Message: "invalid request for workspace wrk_private",
				Type:    "invalid_request_error",
				Code:    "invalid_request_error",
			}, http.StatusBadRequest, types.ErrOptionWithSkipRetry())),
			wantStatus:  http.StatusBadRequest,
			wantCode:    constant.OpenCodeGoPublicInvalidRequestCode,
			wantMessage: constant.OpenCodeGoPublicInvalidRequestMessage,
		},
		{
			name: "explicit client request with credentials uses neutral message",
			err: MarkOpenCodeGoUpstreamRelayError(types.WithOpenAIError(types.OpenAIError{
				Message: "invalid request: Authorization: Bearer private-upstream-token; proxy socks5://proxy-user:proxy-password@10.0.0.8:1080",
				Type:    "invalid_request_error",
				Code:    "invalid_request_error",
			}, http.StatusBadRequest, types.ErrOptionWithSkipRetry())),
			wantStatus:  http.StatusBadRequest,
			wantCode:    constant.OpenCodeGoPublicInvalidRequestCode,
			wantMessage: constant.OpenCodeGoPublicInvalidRequestMessage,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := test.err.Error()
			publicErr := PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeAPIKey, test.err)

			require.NotSame(t, test.err, publicErr)
			assert.Equal(t, test.wantStatus, publicErr.StatusCode)
			assert.Equal(t, test.wantCode, fmt.Sprint(publicErr.ToOpenAIError().Code))
			assert.Equal(t, test.wantMessage, publicErr.Error())
			assert.Equal(t, original, test.err.Error())
		})
	}
}

func TestPublicOpenCodeGoRelayErrorRejectsConflictingUpstreamClientClassifications(t *testing.T) {
	newUpstreamError := func(upstreamStatusCode int, errorType, errorCode string) *types.NewAPIError {
		relayErr := types.WithOpenAIError(types.OpenAIError{
			Message: "upstream rejected the request",
			Type:    errorType,
			Code:    errorCode,
		}, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
		return MarkOpenCodeGoUpstreamRelayErrorWithStatus(relayErr, upstreamStatusCode)
	}

	tests := []struct {
		name               string
		upstreamStatusCode int
		errorType          string
		errorCode          string
		wantPolicyStatus   int
	}{
		{
			name:               "non-2xx invalid api key vetoes invalid request type",
			upstreamStatusCode: http.StatusUnauthorized,
			errorType:          "invalid_request_error",
			errorCode:          "invalid_api_key",
			wantPolicyStatus:   http.StatusUnauthorized,
		},
		{
			name:               "non-2xx rate limit vetoes validation type",
			upstreamStatusCode: http.StatusTooManyRequests,
			errorType:          "validation_error",
			errorCode:          "rate_limit_exceeded",
			wantPolicyStatus:   http.StatusTooManyRequests,
		},
		{
			name:               "non-2xx quota vetoes invalid request type",
			upstreamStatusCode: http.StatusTooManyRequests,
			errorType:          "invalid_request_error",
			errorCode:          "quota_exhausted",
			wantPolicyStatus:   http.StatusTooManyRequests,
		},
		{
			name:               "raw 400 policy vetoes invalid request type",
			upstreamStatusCode: http.StatusBadRequest,
			errorType:          "invalid_request_error",
			errorCode:          "policy_violation",
			wantPolicyStatus:   http.StatusBadRequest,
		},
		{
			name:               "http 200 envelope invalid prompt fails closed",
			upstreamStatusCode: http.StatusOK,
			errorType:          "invalid_request_error",
			errorCode:          "invalid_prompt",
			wantPolicyStatus:   http.StatusBadRequest,
		},
		{
			name:               "http 200 envelope validation rate limit fails closed",
			upstreamStatusCode: http.StatusOK,
			errorType:          "validation_error",
			errorCode:          "rate_limit_exceeded",
			wantPolicyStatus:   http.StatusTooManyRequests,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			internalErr := newUpstreamError(test.upstreamStatusCode, test.errorType, test.errorCode)
			originalStatus := internalErr.StatusCode
			originalMessage := internalErr.Error()

			assert.Equal(t, test.wantPolicyStatus, OpenCodeGoRelayPolicyStatusCode(internalErr))
			publicErr := PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeAPIKey, internalErr)

			require.NotSame(t, internalErr, publicErr)
			assert.Equal(t, http.StatusTooManyRequests, publicErr.StatusCode)
			assert.Equal(t, constant.OpenCodeGoPublicRateLimitErrorCode, fmt.Sprint(publicErr.ToOpenAIError().Code))
			assert.Equal(t, constant.OpenCodeGoPublicOverloadMessage, publicErr.Error())
			assert.Equal(t, originalStatus, internalErr.StatusCode)
			assert.Equal(t, originalMessage, internalErr.Error())
		})
	}
}

func TestPublicOpenCodeGoRelayErrorKeepsSafeUpstreamClientErrorsOnlyForRaw400Or422(t *testing.T) {
	newUpstreamError := func(upstreamStatusCode int) *types.NewAPIError {
		relayErr := types.WithOpenAIError(types.OpenAIError{
			Message: "messages[0].role is required",
			Type:    "invalid_request_error",
			Code:    "invalid_prompt",
		}, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
		return MarkOpenCodeGoUpstreamRelayErrorWithStatus(relayErr, upstreamStatusCode)
	}

	for _, test := range []struct {
		name               string
		upstreamStatusCode int
		wantStatus         int
	}{
		{name: "raw 400", upstreamStatusCode: http.StatusBadRequest, wantStatus: http.StatusBadRequest},
		{name: "raw 422", upstreamStatusCode: http.StatusUnprocessableEntity, wantStatus: http.StatusBadRequest},
		{name: "raw 401", upstreamStatusCode: http.StatusUnauthorized, wantStatus: http.StatusTooManyRequests},
		{name: "raw 429", upstreamStatusCode: http.StatusTooManyRequests, wantStatus: http.StatusTooManyRequests},
		{name: "http 200 envelope", upstreamStatusCode: http.StatusOK, wantStatus: http.StatusTooManyRequests},
	} {
		t.Run(test.name, func(t *testing.T) {
			internalErr := newUpstreamError(test.upstreamStatusCode)
			publicErr := PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeAPIKey, internalErr)

			assert.Equal(t, test.wantStatus, publicErr.StatusCode)
			if test.wantStatus == http.StatusBadRequest {
				assert.Equal(t, "messages[0].role is required", publicErr.Error())
			} else {
				assert.Equal(t, constant.OpenCodeGoPublicOverloadMessage, publicErr.Error())
			}
		})
	}
}

func TestPublicOpenCodeGoRelayErrorProjectsMarkedUpstreamCancellation(t *testing.T) {
	for _, test := range []struct {
		name               string
		upstreamStatusCode int
		message            string
	}{
		{name: "raw upstream 499", upstreamStatusCode: 499, message: "upstream request context canceled"},
		{name: "http 200 cancellation envelope", upstreamStatusCode: http.StatusOK, message: "context canceled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			internalErr := MarkOpenCodeGoUpstreamRelayErrorWithStatus(types.WithOpenAIError(types.OpenAIError{
				Message: test.message,
				Type:    "upstream_error",
				Code:    "upstream_error",
			}, http.StatusBadGateway), test.upstreamStatusCode)

			publicErr := PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeAPIKey, internalErr)

			require.NotSame(t, internalErr, publicErr)
			assert.Equal(t, http.StatusTooManyRequests, publicErr.StatusCode)
			assert.Equal(t, constant.OpenCodeGoPublicRateLimitErrorCode, fmt.Sprint(publicErr.ToOpenAIError().Code))
			assert.Equal(t, constant.OpenCodeGoPublicOverloadMessage, publicErr.Error())
		})
	}
}
