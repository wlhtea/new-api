package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestDoubaoArkOfficialNativeAliasDispatchesCreateAndQuery(t *testing.T) {
	previousDB := model.DB
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.Task{}))
	model.DB = database
	t.Cleanup(func() { model.DB = previousDB })
	const providerTaskID = "cgt-alias-query"
	require.NoError(t, database.Create(&model.Task{
		TaskID: "task_alias_internal", ProviderTaskID: providerTaskID,
		UserId: 7, Platform: constant.TaskPlatform("doubao_ark_official"),
		Status: model.TaskStatusQueued, Progress: "10%",
		Data:        []byte(`{"id":"task_alias_internal","status":"queued"}`),
		PrivateData: model.TaskPrivateData{UpstreamTaskID: providerTaskID},
	}).Error)

	plugin, found := jsplugin.DefaultRegistry.Get("doubao_ark_official")
	require.True(t, found)
	var submitCalls atomic.Int32
	handlers := func(generation *jsplugin.RoutingGeneration, binding jsplugin.RouteBinding) []gin.HandlerFunc {
		production := productionPluginRouteHandlers(generation, binding)
		return []gin.HandlerFunc{
			production[0],
			func(c *gin.Context) {
				common.SetContextKey(c, constant.ContextKeyUserId, 7)
				common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
				common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
				c.Next()
			},
			production[4],
			func(c *gin.Context) {
				submitCalls.Add(1)
				assert.Equal(t, "doubao_ark_official", c.GetString("task_plugin_key"))
				assert.Equal(t, doubaoArkAliasTestModel, c.GetString("resolved_task_model"))
				c.JSON(http.StatusOK, gin.H{"id": "cgt-alias-create"})
			},
		}
	}
	outer, registry := newPluginRouterTest(t, []*jsplugin.LoadedPlugin{plugin}, handlers)
	outer.NoRoute(
		(&pluginRouteDispatcher{registry: registry}).dispatch,
		func(c *gin.Context) { c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "fallback"}) },
	)

	create := httptest.NewRecorder()
	createRequest := httptest.NewRequest(http.MethodPost, "/api/v3/contents/generations/tasks", strings.NewReader(
		`{"model":"`+doubaoArkAliasTestModel+`","content":[{"type":"text","text":"a lighthouse"}]}`,
	))
	createRequest.Header.Set("Content-Type", "application/json")
	outer.ServeHTTP(create, createRequest)
	require.Equal(t, http.StatusOK, create.Code, create.Body.String())
	assert.JSONEq(t, `{"id":"cgt-alias-create"}`, create.Body.String())
	assert.Equal(t, int32(1), submitCalls.Load())

	query := httptest.NewRecorder()
	outer.ServeHTTP(query, httptest.NewRequest(http.MethodGet, "/api/v3/contents/generations/tasks/"+providerTaskID, nil))
	require.Equal(t, http.StatusOK, query.Code, query.Body.String())
	assert.JSONEq(t, `{"id":"cgt-alias-query","status":"queued"}`, query.Body.String())
	assert.NotContains(t, query.Body.String(), "task_alias_internal")
	assert.Equal(t, int32(1), submitCalls.Load(), "query must not enter task submission")
}

const doubaoArkAliasTestModel = "doubao-seedance-2-0-260128"
