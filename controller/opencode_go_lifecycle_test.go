package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCancelOpenCodeGoSubscriptionRenewalRequiresExplicitConfirmation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST(
		"/channel/:id/opencode-go/workspaces/:workspace_uid/subscription/cancel-renewal",
		CancelOpenCodeGoSubscriptionRenewal,
	)
	request := httptest.NewRequest(
		http.MethodPost,
		"/channel/1/opencode-go/workspaces/workspace-test/subscription/cancel-renewal",
		strings.NewReader(`{"confirmation":"cancel"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	engine.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, response.Body.String(), `"success":false`)
	assert.Contains(t, response.Body.String(), openCodeGoCancelRenewalConfirmation)
}
