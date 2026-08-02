package router

import (
	"net/http"

	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
)

type permissionRoute struct {
	method      string
	path        string
	permission  authz.Permission
	middlewares []gin.HandlerFunc
	handler     gin.HandlerFunc
}

func registerChannelRoutes(apiRouter *gin.RouterGroup) {
	channelRoute := apiRouter.Group("/channel")
	channelRoute.Use(middleware.AdminAuth())

	channelRoute.POST("/:id/key",
		middleware.RootAuth(),
		middleware.CriticalRateLimit(),
		middleware.DisableCache(),
		middleware.SecureVerificationRequired(),
		controller.GetChannelKey,
	)

	for _, route := range channelPermissionRoutes {
		handlers := []gin.HandlerFunc{middleware.RequirePermission(route.permission)}
		handlers = append(handlers, route.middlewares...)
		handlers = append(handlers, route.handler)
		channelRoute.Handle(route.method, route.path, handlers...)
	}
}

var channelPermissionRoutes = []permissionRoute{
	{method: http.MethodGet, path: "/", permission: authz.ChannelRead, handler: controller.GetAllChannels},
	{method: http.MethodGet, path: "/search", permission: authz.ChannelRead, handler: controller.SearchChannels},
	{method: http.MethodGet, path: "/models", permission: authz.ChannelRead, handler: controller.ChannelListModels},
	{method: http.MethodGet, path: "/models_enabled", permission: authz.ChannelRead, handler: controller.EnabledListModels},
	{method: http.MethodGet, path: "/ops", permission: authz.ChannelRead, handler: controller.GetChannelOps},
	{method: http.MethodGet, path: "/:id", permission: authz.ChannelRead, handler: controller.GetChannel},
	{method: http.MethodGet, path: "/test", permission: authz.ChannelOperate, handler: controller.TestAllChannels},
	{method: http.MethodGet, path: "/test/:id", permission: authz.ChannelOperate, handler: controller.TestChannel},
	{method: http.MethodGet, path: "/update_balance", permission: authz.ChannelOperate, handler: controller.UpdateAllChannelsBalance},
	{method: http.MethodGet, path: "/update_balance/:id", permission: authz.ChannelOperate, handler: controller.UpdateChannelBalance},
	{method: http.MethodPost, path: "/", permission: authz.ChannelSensitiveWrite, handler: controller.AddChannel},
	{method: http.MethodPut, path: "/", permission: authz.ChannelWrite, handler: controller.UpdateChannel},
	{method: http.MethodPost, path: "/status/batch", permission: authz.ChannelOperate, handler: controller.BatchUpdateChannelStatus},
	{method: http.MethodPost, path: "/:id/status", permission: authz.ChannelOperate, handler: controller.UpdateChannelStatus},
	{method: http.MethodDelete, path: "/disabled", permission: authz.ChannelSensitiveWrite, handler: controller.DeleteDisabledChannel},
	{method: http.MethodPost, path: "/tag/disabled", permission: authz.ChannelOperate, handler: controller.DisableTagChannels},
	{method: http.MethodPost, path: "/tag/enabled", permission: authz.ChannelOperate, handler: controller.EnableTagChannels},
	{method: http.MethodPut, path: "/tag", permission: authz.ChannelWrite, handler: controller.EditTagChannels},
	{method: http.MethodDelete, path: "/:id", permission: authz.ChannelSensitiveWrite, handler: controller.DeleteChannel},
	{method: http.MethodPost, path: "/batch", permission: authz.ChannelSensitiveWrite, handler: controller.DeleteChannelBatch},
	{method: http.MethodPost, path: "/fix", permission: authz.ChannelOperate, handler: controller.FixChannelsAbilities},
	{method: http.MethodGet, path: "/fetch_models/:id", permission: authz.ChannelOperate, handler: controller.FetchUpstreamModels},
	{method: http.MethodPost, path: "/fetch_models", permission: authz.ChannelSensitiveWrite, handler: controller.FetchModels},
	{method: http.MethodPost, path: "/:id/codex/refresh", permission: authz.ChannelSensitiveWrite, handler: controller.RefreshCodexChannelCredential},
	{method: http.MethodGet, path: "/:id/codex/usage", permission: authz.ChannelRead, handler: controller.GetCodexChannelUsage},
	{method: http.MethodGet, path: "/:id/codex/usage/reset-credits", permission: authz.ChannelRead, handler: controller.GetCodexChannelRateLimitResetCredits},
	{method: http.MethodPost, path: "/:id/codex/usage/reset", permission: authz.ChannelOperate, handler: controller.ResetCodexChannelUsage},
	{method: http.MethodPost, path: "/ollama/pull", permission: authz.ChannelSensitiveWrite, handler: controller.OllamaPullModel},
	{method: http.MethodPost, path: "/ollama/pull/stream", permission: authz.ChannelSensitiveWrite, handler: controller.OllamaPullModelStream},
	{method: http.MethodDelete, path: "/ollama/delete", permission: authz.ChannelSensitiveWrite, handler: controller.OllamaDeleteModel},
	{method: http.MethodGet, path: "/ollama/version/:id", permission: authz.ChannelSensitiveWrite, handler: controller.OllamaVersion},
	{method: http.MethodPost, path: "/batch/tag", permission: authz.ChannelWrite, handler: controller.BatchSetChannelTag},
	{method: http.MethodGet, path: "/tag/models", permission: authz.ChannelRead, handler: controller.GetTagModels},
	{method: http.MethodPost, path: "/copy/:id", permission: authz.ChannelSensitiveWrite, handler: controller.CopyChannel},
	{method: http.MethodPost, path: "/multi_key/manage", permission: authz.ChannelOperate, handler: controller.ManageMultiKeys},
	{method: http.MethodPost, path: "/upstream_updates/apply", permission: authz.ChannelWrite, handler: controller.ApplyChannelUpstreamModelUpdates},
	{method: http.MethodPost, path: "/upstream_updates/apply_all", permission: authz.ChannelWrite, handler: controller.ApplyAllChannelUpstreamModelUpdates},
	{method: http.MethodPost, path: "/upstream_updates/detect", permission: authz.ChannelOperate, handler: controller.DetectChannelUpstreamModelUpdates},
	{method: http.MethodPost, path: "/upstream_updates/detect_all", permission: authz.ChannelOperate, handler: controller.DetectAllChannelUpstreamModelUpdates},
	{
		method:      http.MethodGet,
		path:        "/:id/opencode-go/pool",
		permission:  authz.ChannelRead,
		middlewares: []gin.HandlerFunc{middleware.DisableCache()},
		handler:     controller.GetOpenCodeGoPool,
	},
	{
		method:      http.MethodGet,
		path:        "/:id/opencode-go/refresh-tasks/:task_id",
		permission:  authz.ChannelRead,
		middlewares: []gin.HandlerFunc{middleware.DisableCache()},
		handler:     controller.GetOpenCodeGoRefreshTask,
	},
	{method: http.MethodPatch, path: "/:id/opencode-go/identities/:identity_uid", permission: authz.ChannelOperate, handler: controller.UpdateOpenCodeGoIdentity},
	{method: http.MethodPatch, path: "/:id/opencode-go/identities/:identity_uid/enabled", permission: authz.ChannelOperate, handler: controller.SetOpenCodeGoIdentityEnabled},
	{method: http.MethodPost, path: "/:id/opencode-go/identities/:identity_uid/refresh", permission: authz.ChannelOperate, handler: controller.RefreshOpenCodeGoIdentity},
	{method: http.MethodPatch, path: "/:id/opencode-go/workspaces/:workspace_uid/enabled", permission: authz.ChannelOperate, handler: controller.SetOpenCodeGoWorkspaceEnabled},
	{method: http.MethodPost, path: "/:id/opencode-go/workspaces/:workspace_uid/refresh", permission: authz.ChannelOperate, handler: controller.RefreshOpenCodeGoWorkspace},
	{method: http.MethodPost, path: "/:id/opencode-go/refresh-all", permission: authz.ChannelOperate, handler: controller.RefreshAllOpenCodeGoIdentities},
	{
		method:     http.MethodPost,
		path:       "/:id/opencode-go/identities/import",
		permission: authz.ChannelSensitiveWrite,
		middlewares: []gin.HandlerFunc{
			middleware.CriticalRateLimit(),
			middleware.DisableCache(),
			middleware.SecurityProofRequired(controller.SecurityProofScopeOpenCodeGoPoolWrite, []string{"2fa", "passkey"}),
		},
		handler: controller.ImportOpenCodeGoIdentities,
	},
	{
		method:     http.MethodPut,
		path:       "/:id/opencode-go/identities/:identity_uid/cookie",
		permission: authz.ChannelSensitiveWrite,
		middlewares: []gin.HandlerFunc{
			middleware.CriticalRateLimit(),
			middleware.DisableCache(),
			middleware.SecurityProofRequired(controller.SecurityProofScopeOpenCodeGoPoolWrite, []string{"2fa", "passkey"}),
		},
		handler: controller.ReplaceOpenCodeGoIdentityCookie,
	},
	{
		method:     http.MethodDelete,
		path:       "/:id/opencode-go/identities/:identity_uid",
		permission: authz.ChannelSensitiveWrite,
		middlewares: []gin.HandlerFunc{
			middleware.DisableCache(),
			middleware.SecurityProofRequired(controller.SecurityProofScopeOpenCodeGoPoolWrite, []string{"2fa", "passkey"}),
		},
		handler: controller.DeleteOpenCodeGoIdentity,
	},
	{
		method:     http.MethodDelete,
		path:       "/:id/opencode-go/workspaces/non-members",
		permission: authz.ChannelSensitiveWrite,
		middlewares: []gin.HandlerFunc{
			middleware.DisableCache(),
			middleware.SecurityProofRequired(controller.SecurityProofScopeOpenCodeGoPoolWrite, []string{"2fa", "passkey"}),
		},
		handler: controller.DeleteOpenCodeGoNonMemberWorkspaces,
	},
	{
		method:     http.MethodDelete,
		path:       "/:id/opencode-go/workspaces/:workspace_uid",
		permission: authz.ChannelSensitiveWrite,
		middlewares: []gin.HandlerFunc{
			middleware.DisableCache(),
			middleware.SecurityProofRequired(controller.SecurityProofScopeOpenCodeGoPoolWrite, []string{"2fa", "passkey"}),
		},
		handler: controller.DeleteOpenCodeGoWorkspace,
	},
}
