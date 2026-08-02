package opencodego

import (
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
