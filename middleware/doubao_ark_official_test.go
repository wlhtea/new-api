package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoubaoArkOfficialNativeQueryScopesProviderID(t *testing.T) {
	setupTaskPluginRouteDB(t)
	plugin, found := jsplugin.DefaultRegistry.Get("doubao_ark_official")
	require.True(t, found)
	require.GreaterOrEqual(t, len(plugin.Meta.Routes), 2)

	const (
		ownedProviderID   = "cgt-owned-native"
		foreignProviderID = "cgt-foreign-native"
		wrongPluginID     = "cgt-wrong-plugin"
		legacyChannelID   = "cgt-legacy-channel"
	)
	insertTaskPluginRouteTask(t, &model.Task{
		TaskID: "task_owned_internal", ProviderTaskID: ownedProviderID,
		UserId: 7, Platform: constant.TaskPlatform("doubao_ark_official"),
		Status: model.TaskStatusQueued, Progress: "10%",
		Data:        []byte(`{"id":"task_owned_internal","status":"queued"}`),
		PrivateData: model.TaskPrivateData{UpstreamTaskID: ownedProviderID},
	})
	// Provider IDs are only locally indexed: another tenant may have the same
	// upstream ID without changing which row the current user can retrieve.
	insertTaskPluginRouteTask(t, &model.Task{
		TaskID: "task_foreign_duplicate", ProviderTaskID: ownedProviderID,
		UserId: 8, Platform: constant.TaskPlatform("doubao_ark_official"),
		Data: []byte(`{"id":"cgt-owned-native","status":"failed"}`),
	})
	insertTaskPluginRouteTask(t, &model.Task{
		TaskID: "task_foreign_internal", ProviderTaskID: foreignProviderID,
		UserId: 8, Platform: constant.TaskPlatform("doubao_ark_official"),
	})
	insertTaskPluginRouteTask(t, &model.Task{
		TaskID: "task_wrong_plugin", ProviderTaskID: wrongPluginID,
		UserId: 7, Platform: constant.TaskPlatform("doubao"),
	})
	insertTaskPluginRouteTask(t, &model.Task{
		TaskID: "task_legacy_channel", ProviderTaskID: legacyChannelID,
		UserId: 7, Platform: constant.TaskPlatform("54"),
	})

	router := gin.New()
	router.GET("/doubao_ark_official/api/v3/contents/generations/tasks/:task_id",
		pinTaskPluginRoute(plugin, 1),
		func(c *gin.Context) {
			c.Set("id", 7)
			c.Next()
		},
		PrepareTaskPluginRoute(),
	)

	owned := httptest.NewRecorder()
	router.ServeHTTP(owned, httptest.NewRequest(http.MethodGet, "/doubao_ark_official/api/v3/contents/generations/tasks/"+ownedProviderID, nil))
	require.Equal(t, http.StatusOK, owned.Code, owned.Body.String())
	assert.JSONEq(t, `{"id":"cgt-owned-native","status":"queued"}`, owned.Body.String())
	assert.NotContains(t, owned.Body.String(), "task_owned_internal")

	var notFoundBody string
	for _, providerTaskID := range []string{foreignProviderID, wrongPluginID, legacyChannelID, "cgt-missing"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/doubao_ark_official/api/v3/contents/generations/tasks/"+providerTaskID, nil))
		require.Equal(t, http.StatusNotFound, recorder.Code, providerTaskID)
		assert.Contains(t, recorder.Body.String(), `"code":"task_not_found"`)
		if notFoundBody == "" {
			notFoundBody = recorder.Body.String()
		} else {
			assert.JSONEq(t, notFoundBody, recorder.Body.String())
		}
		assert.NotContains(t, recorder.Body.String(), providerTaskID)
	}
}
