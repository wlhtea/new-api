package router

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelStatusRoutesUseOperatePermission(t *testing.T) {
	assertChannelRoutePermission(t, http.MethodPost, "/:id/status", authz.ChannelOperate, controller.UpdateChannelStatus)
	assertChannelRoutePermission(t, http.MethodPost, "/status/batch", authz.ChannelOperate, controller.BatchUpdateChannelStatus)
	assertChannelRoutePermission(t, http.MethodPut, "/", authz.ChannelWrite, controller.UpdateChannel)
}

func TestChannelDeleteRoutesUseSensitiveWritePermission(t *testing.T) {
	assertChannelRoutePermission(t, http.MethodDelete, "/:id", authz.ChannelSensitiveWrite, controller.DeleteChannel)
	assertChannelRoutePermission(t, http.MethodPost, "/batch", authz.ChannelSensitiveWrite, controller.DeleteChannelBatch)
	assertChannelRoutePermission(t, http.MethodDelete, "/disabled", authz.ChannelSensitiveWrite, controller.DeleteDisabledChannel)
	assertChannelRoutePermission(t, http.MethodPut, "/", authz.ChannelWrite, controller.UpdateChannel)
	assertChannelRoutePermission(t, http.MethodPut, "/tag", authz.ChannelWrite, controller.EditTagChannels)
	assertChannelRoutePermission(t, http.MethodPost, "/batch/tag", authz.ChannelWrite, controller.BatchSetChannelTag)
}

func TestOpenCodeGoPoolRoutesUseScopedChannelPermissions(t *testing.T) {
	assertChannelRoutePermission(t, http.MethodGet, "/:id/opencode-go/pool", authz.ChannelRead, controller.GetOpenCodeGoPool)
	assertChannelRoutePermission(t, http.MethodGet, "/:id/opencode-go/lifecycle-policy", authz.ChannelRead, controller.GetOpenCodeGoLifecyclePolicy)
	assertChannelRoutePermission(t, http.MethodGet, "/:id/opencode-go/refresh-tasks/:task_id", authz.ChannelRead, controller.GetOpenCodeGoRefreshTask)
	assertChannelRoutePermission(t, http.MethodGet, "/:id/opencode-go/risk-recheck-tasks/:task_id", authz.ChannelRead, controller.GetOpenCodeGoRiskRecheckTask)
	assertChannelRoutePermission(t, http.MethodPatch, "/:id/opencode-go/identities/:identity_uid", authz.ChannelOperate, controller.UpdateOpenCodeGoIdentity)
	assertChannelRoutePermission(t, http.MethodPost, "/:id/opencode-go/identities/:identity_uid/refresh", authz.ChannelOperate, controller.RefreshOpenCodeGoIdentity)
	assertChannelRoutePermission(t, http.MethodPatch, "/:id/opencode-go/identities/:identity_uid/enabled", authz.ChannelOperate, controller.SetOpenCodeGoIdentityEnabled)
	assertChannelRoutePermission(t, http.MethodPatch, "/:id/opencode-go/workspaces/:workspace_uid/enabled", authz.ChannelOperate, controller.SetOpenCodeGoWorkspaceEnabled)
	assertChannelRoutePermission(t, http.MethodPost, "/:id/opencode-go/workspaces/:workspace_uid/refresh", authz.ChannelOperate, controller.RefreshOpenCodeGoWorkspace)
	assertChannelRoutePermission(t, http.MethodPost, "/:id/opencode-go/workspaces/:workspace_uid/risk-recheck", authz.ChannelOperate, controller.RecheckOpenCodeGoWorkspaceRisk)
	assertChannelRoutePermission(t, http.MethodPost, "/:id/opencode-go/refresh-all", authz.ChannelOperate, controller.RefreshAllOpenCodeGoIdentities)
	assertChannelRoutePermission(t, http.MethodPost, "/:id/opencode-go/risk-recheck-all", authz.ChannelOperate, controller.RecheckAllOpenCodeGoWorkspaceRisks)
	assertChannelRoutePermission(t, http.MethodPut, "/:id/opencode-go/lifecycle-policy", authz.ChannelSensitiveWrite, controller.UpdateOpenCodeGoLifecyclePolicy)
	assertChannelRoutePermission(t, http.MethodPost, "/:id/opencode-go/workspaces/:workspace_uid/china-models/enable", authz.ChannelSensitiveWrite, controller.EnableOpenCodeGoChinaModels)
	assertChannelRoutePermission(t, http.MethodPost, "/:id/opencode-go/workspaces/:workspace_uid/referral-rewards/apply", authz.ChannelSensitiveWrite, controller.ApplyOpenCodeGoReferralReward)
	assertChannelRoutePermission(t, http.MethodPost, "/:id/opencode-go/workspaces/:workspace_uid/subscription/cancel-renewal", authz.ChannelSensitiveWrite, controller.CancelOpenCodeGoSubscriptionRenewal)
	assertChannelRoutePermission(t, http.MethodPost, "/:id/opencode-go/identities/import", authz.ChannelSensitiveWrite, controller.ImportOpenCodeGoIdentities)
	assertChannelRoutePermission(t, http.MethodPut, "/:id/opencode-go/identities/:identity_uid/cookie", authz.ChannelSensitiveWrite, controller.ReplaceOpenCodeGoIdentityCookie)
	assertChannelRoutePermission(t, http.MethodDelete, "/:id/opencode-go/identities/:identity_uid", authz.ChannelSensitiveWrite, controller.DeleteOpenCodeGoIdentity)
	assertChannelRoutePermission(t, http.MethodDelete, "/:id/opencode-go/workspaces/non-members", authz.ChannelSensitiveWrite, controller.DeleteOpenCodeGoNonMemberWorkspaces)
	assertChannelRoutePermission(t, http.MethodDelete, "/:id/opencode-go/workspaces/:workspace_uid", authz.ChannelSensitiveWrite, controller.DeleteOpenCodeGoWorkspace)
}

func TestOpenCodeGoSensitiveRouteRequiresDedicatedSecurityProofScope(t *testing.T) {
	var proofMiddleware gin.HandlerFunc
	for _, route := range channelPermissionRoutes {
		if route.method == http.MethodPost && route.path == "/:id/opencode-go/identities/import" {
			require.NotEmpty(t, route.middlewares)
			proofMiddleware = route.middlewares[len(route.middlewares)-1]
			break
		}
	}
	require.NotNil(t, proofMiddleware)

	previousSecret := common.SessionSecret
	common.SessionSecret = "router-security-proof-test-secret"
	t.Cleanup(func() { common.SessionSecret = previousSecret })
	identity := service.AuthIdentity{
		UserID:          42,
		SessionID:       "session-opencode-go",
		UserAuthVersion: 3,
		SessionVersion:  2,
	}
	wrongScopeProof, _, err := service.IssueSecurityProof(identity, "2fa", []string{"channel.key.read"})
	require.NoError(t, err)
	correctProof, _, err := service.IssueSecurityProof(
		identity,
		"2fa",
		[]string{controller.SecurityProofScopeOpenCodeGoPoolWrite},
	)
	require.NoError(t, err)

	run := func(proof string) (int, string, bool) {
		t.Helper()
		reached := false
		engine := gin.New()
		engine.POST("/test",
			func(c *gin.Context) {
				c.Set("id", identity.UserID)
				c.Set("session_id", identity.SessionID)
				c.Set("auth_version", identity.UserAuthVersion)
				c.Set("session_version", identity.SessionVersion)
				c.Next()
			},
			proofMiddleware,
			func(c *gin.Context) {
				reached = c.GetBool("secure_verified")
				c.Status(http.StatusNoContent)
			},
		)
		request := httptest.NewRequest(http.MethodPost, "/test", nil)
		request.Header.Set("X-Security-Proof", proof)
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		return response.Code, response.Body.String(), reached
	}

	status, body, reached := run(wrongScopeProof)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Contains(t, body, "SECURITY_PROOF_SCOPE_MISMATCH")
	assert.False(t, reached)

	status, _, reached = run(correctProof)
	assert.Equal(t, http.StatusNoContent, status)
	assert.True(t, reached)
}

func TestOpenCodeGoLifecycleWriteRoutesUseCriticalScopedMiddleware(t *testing.T) {
	paths := []struct {
		method string
		path   string
	}{
		{http.MethodPut, "/:id/opencode-go/lifecycle-policy"},
		{http.MethodPost, "/:id/opencode-go/workspaces/:workspace_uid/china-models/enable"},
		{http.MethodPost, "/:id/opencode-go/workspaces/:workspace_uid/referral-rewards/apply"},
		{http.MethodPost, "/:id/opencode-go/workspaces/:workspace_uid/subscription/cancel-renewal"},
	}
	for _, expected := range paths {
		found := false
		for _, route := range channelPermissionRoutes {
			if route.method != expected.method || route.path != expected.path {
				continue
			}
			found = true
			require.Equal(t, authz.ChannelSensitiveWrite, route.permission)
			require.Len(t, route.middlewares, 3)
			break
		}
		require.True(t, found, "route %s %s not found", expected.method, expected.path)
	}
}

func TestChannelStatusRoutesRegisterWithoutConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	api := engine.Group("/api")

	require.NotPanics(t, func() {
		registerChannelRoutes(api)
	})
}

func assertChannelRoutePermission(t *testing.T, method string, path string, permission authz.Permission, handler any) {
	t.Helper()
	for _, route := range channelPermissionRoutes {
		if route.method == method && route.path == path {
			assert.Equal(t, permission, route.permission)
			assert.Equal(t, reflect.ValueOf(handler).Pointer(), reflect.ValueOf(route.handler).Pointer())
			return
		}
	}
	t.Fatalf("route %s %s not found", method, path)
}
