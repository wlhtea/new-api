package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func doubaoArkOfficialRoute(t *testing.T, method, path string) (*jsplugin.LoadedPlugin, int) {
	t.Helper()
	plugin, found := jsplugin.DefaultRegistry.Get("doubao_ark_official")
	require.True(t, found)
	for index, route := range plugin.Meta.Routes {
		if route.Method == method && route.Path == path {
			return plugin, index
		}
	}
	require.FailNow(t, "official native route is not registered", "%s %s", method, path)
	return nil, 0
}

func newDoubaoArkOfficialListRouter(t *testing.T, userID int) *gin.Engine {
	t.Helper()
	plugin, routeIndex := doubaoArkOfficialRoute(t, http.MethodGet, "/doubao_ark_official/api/v3/contents/generations/tasks")
	router := gin.New()
	router.GET("/doubao_ark_official/api/v3/contents/generations/tasks",
		pinTaskPluginRoute(plugin, routeIndex),
		func(c *gin.Context) {
			c.Set("id", userID)
			c.Next()
		},
		PrepareTaskPluginRoute(),
	)
	return router
}

func TestDoubaoArkOfficialNativeListDefaultShape(t *testing.T) {
	setupTaskPluginRouteDB(t)
	now := time.Now().Unix()
	for _, task := range []*model.Task{
		{
			TaskID: "task_internal_old", ProviderTaskID: "cgt-old", UserId: 7,
			Platform: constant.TaskPlatform("doubao_ark_official"), Status: model.TaskStatusQueued,
			CreatedAt: now - 200, UpdatedAt: now - 200,
			Data: []byte(`{"id":"task_internal_old","status":"queued","model":"doubao-seedance-2-0-260128","private_data":{"key":"must-not-leak"},"channel_id":91,"billing_snapshot":{"quota":12}}`),
		},
		{
			TaskID: "task_internal_new", ProviderTaskID: "cgt-new", UserId: 7,
			Platform: constant.TaskPlatform("doubao_ark_official"), Status: model.TaskStatusSuccess,
			CreatedAt: now - 100, UpdatedAt: now - 100,
			Data: []byte(`{"id":"task_internal_new","status":"succeeded","model":"doubao-seedance-2-0-260128"}`),
		},
	} {
		insertTaskPluginRouteTask(t, task)
	}

	recorder := httptest.NewRecorder()
	newDoubaoArkOfficialListRouter(t, 7).ServeHTTP(recorder,
		httptest.NewRequest(http.MethodGet, "/doubao_ark_official/api/v3/contents/generations/tasks", nil))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var response struct {
		Total int `json:"total"`
		Items []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Model  string `json:"model"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, 2, response.Total)
	require.Len(t, response.Items, 2)
	assert.Equal(t, "cgt-new", response.Items[0].ID)
	assert.Equal(t, "succeeded", response.Items[0].Status)
	assert.Equal(t, "cgt-old", response.Items[1].ID)
	assert.Equal(t, "queued", response.Items[1].Status)
	for _, forbidden := range []string{"task_internal", "private_data", "must-not-leak", "channel_id", "billing_snapshot"} {
		assert.NotContains(t, recorder.Body.String(), forbidden)
	}
}
