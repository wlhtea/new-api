package middleware

import (
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

func TestAbortWithOpenAiMessageHidesOpenCodeGoInternalDetails(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeGo)

	abortWithOpenAiMessage(
		c,
		http.StatusServiceUnavailable,
		"OpenCode Go channel workspace is unavailable",
		types.ErrorCodeGetChannelFailed,
	)

	require.Equal(t, http.StatusTooManyRequests, recorder.Code)
	body := strings.ToLower(recorder.Body.String())
	assert.Contains(t, body, service.OpenCodeGoPublicOverloadMessage)
	assert.Contains(t, body, service.OpenCodeGoPublicRateLimitErrorCode)
	for _, marker := range []string{"opencode", "channel", "workspace", "wrk_"} {
		assert.NotContains(t, body, marker)
	}
}

func TestAbortWithOpenAiMessagePreservesOtherChannelErrors(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)

	abortWithOpenAiMessage(c, http.StatusServiceUnavailable, "workspace is unavailable")

	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "workspace is unavailable")
}
