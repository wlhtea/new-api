package service

import (
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

func TestPublicOpenCodeGoRelayErrorDoesNotRewriteOtherChannels(t *testing.T) {
	internalErr := types.WithOpenAIError(types.OpenAIError{
		Message: "workspace is unavailable",
		Type:    "upstream_error",
		Code:    "workspace_unavailable",
	}, http.StatusServiceUnavailable)

	assert.Same(t, internalErr, PublicOpenCodeGoRelayError(constant.ChannelTypeOpenAI, internalErr))
}

func TestPublicOpenCodeGoRelayErrorAlwaysHidesPoolSentinel(t *testing.T) {
	internalErr := types.NewOpenAIError(
		fmt.Errorf("setup request header failed: %w", ErrOpenCodeGoNoEligibleWorkspace),
		types.ErrorCodeDoRequestFailed,
		http.StatusInternalServerError,
	)

	publicErr := PublicOpenCodeGoRelayError(0, internalErr)
	require.NotSame(t, internalErr, publicErr)
	assert.Equal(t, http.StatusTooManyRequests, publicErr.StatusCode)
	assert.ErrorIs(t, internalErr, ErrOpenCodeGoNoEligibleWorkspace)
}
