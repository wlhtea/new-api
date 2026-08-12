package controller

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

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

func TestRenderRelayErrorDoesNotRewriteOtherOpenCodeGoFailures(t *testing.T) {
	internalErr := types.NewOpenAIError(
		errors.New("request setup failed"),
		types.ErrorCodeDoRequestFailed,
		http.StatusInternalServerError,
	)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	renderRelayError(c, types.RelayFormatOpenAI, nil, internalErr, "request-id")

	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "request setup failed")
	assert.NotContains(t, recorder.Body.String(), groupUpstreamOverloadedMessage)
}
