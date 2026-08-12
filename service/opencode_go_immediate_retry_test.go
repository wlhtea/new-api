package service

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShouldRetryOpenCodeGoRelayError(t *testing.T) {
	marked := func(status int) *types.NewAPIError {
		return MarkOpenCodeGoUpstreamRelayError(types.NewOpenAIError(
			errors.New("private upstream failure"),
			types.ErrorCodeBadResponse,
			status,
			types.ErrOptionWithSkipRetry(),
		))
	}

	for _, test := range []struct {
		name        string
		channelType int
		err         *types.NewAPIError
		want        bool
	}{
		{name: "marked 500", channelType: constant.ChannelTypeOpenCodeGo, err: marked(http.StatusInternalServerError), want: true},
		{name: "marked 502", channelType: constant.ChannelTypeOpenCodeGo, err: marked(http.StatusBadGateway), want: true},
		{name: "marked 503", channelType: constant.ChannelTypeOpenCodeGo, err: marked(http.StatusServiceUnavailable), want: true},
		{name: "marked 504", channelType: constant.ChannelTypeOpenCodeGo, err: marked(http.StatusGatewayTimeout), want: true},
		{
			name:        "wrapped pool exhaustion",
			channelType: constant.ChannelTypeOpenCodeGo,
			err: types.NewOpenAIError(
				fmt.Errorf("setup request header failed: %w", ErrOpenCodeGoNoEligibleWorkspace),
				types.ErrorCodeDoRequestFailed,
				http.StatusInternalServerError,
			),
			want: true,
		},
		{name: "unmarked 503", channelType: constant.ChannelTypeOpenCodeGo, err: types.NewOpenAIError(errors.New("local failure"), types.ErrorCodeBadResponse, http.StatusServiceUnavailable)},
		{name: "other channel", channelType: constant.ChannelTypeOpenAI, err: marked(http.StatusServiceUnavailable)},
		{
			name:        "other channel pool exhaustion",
			channelType: constant.ChannelTypeOpenAI,
			err:         types.NewOpenAIError(ErrOpenCodeGoNoEligibleWorkspace, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError),
		},
		{name: "marked 400", channelType: constant.ChannelTypeOpenCodeGo, err: marked(http.StatusBadRequest)},
		{name: "marked 401", channelType: constant.ChannelTypeOpenCodeGo, err: marked(http.StatusUnauthorized)},
		{name: "marked 403", channelType: constant.ChannelTypeOpenCodeGo, err: marked(http.StatusForbidden)},
		{name: "marked 429", channelType: constant.ChannelTypeOpenCodeGo, err: marked(http.StatusTooManyRequests)},
		{name: "marked 501", channelType: constant.ChannelTypeOpenCodeGo, err: marked(http.StatusNotImplemented)},
		{name: "marked 505", channelType: constant.ChannelTypeOpenCodeGo, err: marked(http.StatusHTTPVersionNotSupported)},
		{name: "nil", channelType: constant.ChannelTypeOpenCodeGo},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, ShouldRetryOpenCodeGoRelayError(test.channelType, test.err))
		})
	}
}

func TestShouldRetryOpenCodeGoRelayErrorUsesRawUpstreamStatus(t *testing.T) {
	rawBadRequest := MarkOpenCodeGoUpstreamRelayError(types.NewOpenAIError(
		errors.New("raw bad request"),
		types.ErrorCodeBadResponse,
		http.StatusBadRequest,
		types.ErrOptionWithSkipRetry(),
	))
	ResetStatusCode(rawBadRequest, `{"400":503}`)
	assert.Equal(t, http.StatusServiceUnavailable, rawBadRequest.StatusCode)
	assert.False(t, ShouldRetryOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, rawBadRequest))

	rawServiceUnavailable := MarkOpenCodeGoUpstreamRelayError(types.NewOpenAIError(
		errors.New("raw service unavailable"),
		types.ErrorCodeBadResponse,
		http.StatusServiceUnavailable,
		types.ErrOptionWithSkipRetry(),
	))
	ResetStatusCode(rawServiceUnavailable, `{"503":400}`)
	assert.Equal(t, http.StatusBadRequest, rawServiceUnavailable.StatusCode)
	assert.True(t, ShouldRetryOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, rawServiceUnavailable))
}

func TestOpenCodeGoImmediateRetryLimitIsExactlyOne(t *testing.T) {
	assert.Equal(t, 1, OpenCodeGoImmediateRetryLimit)
}

func newOpenCodeGoImmediateRetryServiceContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c
}

func TestOpenCodeGoImmediateRetrySelectionLifecycle(t *testing.T) {
	c := newOpenCodeGoImmediateRetryServiceContext()
	first := &OpenCodeGoPoolSelection{WorkspaceUID: "workspace-first"}
	ignored := &OpenCodeGoPoolSelection{WorkspaceUID: "workspace-ignored"}

	selection, replaying := OpenCodeGoImmediateRetrySelection(c)
	assert.Nil(t, selection)
	assert.False(t, replaying)

	BeginOpenCodeGoImmediateRetry(c)
	RememberOpenCodeGoImmediateRetrySelection(c, first)
	RememberOpenCodeGoImmediateRetrySelection(c, ignored)
	selection, replaying = OpenCodeGoImmediateRetrySelection(c)
	assert.Nil(t, selection)
	assert.False(t, replaying)

	PrepareOpenCodeGoImmediateRetry(c)
	selection, replaying = OpenCodeGoImmediateRetrySelection(c)
	require.True(t, replaying)
	assert.Same(t, first, selection)

	EndOpenCodeGoImmediateRetry(c)
	selection, replaying = OpenCodeGoImmediateRetrySelection(c)
	assert.Nil(t, selection)
	assert.False(t, replaying)
}

func TestOpenCodeGoImmediateRetryFlushesDeferredFailoverOnce(t *testing.T) {
	c := newOpenCodeGoImmediateRetryServiceContext()
	firstCalls := 0
	ignoredCalls := 0

	BeginOpenCodeGoImmediateRetry(c)
	require.True(t, DeferOpenCodeGoImmediateRetryFailover(c, func() { firstCalls++ }))
	require.True(t, DeferOpenCodeGoImmediateRetryFailover(c, func() { ignoredCalls++ }))
	assert.Zero(t, firstCalls)
	assert.Zero(t, ignoredCalls)

	FlushOpenCodeGoImmediateRetryFailover(c)
	FlushOpenCodeGoImmediateRetryFailover(c)
	assert.Equal(t, 1, firstCalls)
	assert.Zero(t, ignoredCalls)
	assert.False(t, DeferOpenCodeGoImmediateRetryFailover(c, func() { ignoredCalls++ }))
	assert.Zero(t, ignoredCalls)

	EndOpenCodeGoImmediateRetry(c)
}

func TestOpenCodeGoImmediateRetryDiscardsDeferredFailover(t *testing.T) {
	c := newOpenCodeGoImmediateRetryServiceContext()
	calls := 0

	BeginOpenCodeGoImmediateRetry(c)
	require.True(t, DeferOpenCodeGoImmediateRetryFailover(c, func() { calls++ }))
	DiscardOpenCodeGoImmediateRetryFailover(c)
	FlushOpenCodeGoImmediateRetryFailover(c)
	assert.Zero(t, calls)
	assert.False(t, DeferOpenCodeGoImmediateRetryFailover(c, func() { calls++ }))

	EndOpenCodeGoImmediateRetry(c)
}

func TestOpenCodeGoImmediateRetryDiscardsFirstFailoverAndLeavesFinalUndeferred(t *testing.T) {
	c := newOpenCodeGoImmediateRetryServiceContext()
	firstCalls := 0
	finalCalls := 0

	BeginOpenCodeGoImmediateRetry(c)
	require.True(t, DeferOpenCodeGoImmediateRetryFailover(c, func() { firstCalls++ }))
	PrepareOpenCodeGoImmediateRetry(c)
	assert.Zero(t, firstCalls)

	final := func() { finalCalls++ }
	if !DeferOpenCodeGoImmediateRetryFailover(c, final) {
		final()
	}
	assert.Zero(t, firstCalls)
	assert.Equal(t, 1, finalCalls)

	EndOpenCodeGoImmediateRetry(c)
}
