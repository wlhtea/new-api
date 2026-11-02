package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const doubaoOfficialDeleteDeclaredPath = "/doubao_ark_official/api/v3/contents/generations/tasks/:task_id"

func invokeDoubaoOfficialTaskRoute(t *testing.T, fixture *doubaoNativeFixture, method, providerID string, userID int) *httptest.ResponseRecorder {
	t.Helper()
	return invokeDoubaoOfficialTaskRouteWithContext(t, fixture, method, providerID, userID, context.Background())
}

func invokeDoubaoOfficialTaskRouteWithContext(t *testing.T, fixture *doubaoNativeFixture, method, providerID string, userID int, requestContext context.Context) *httptest.ResponseRecorder {
	t.Helper()
	binding, found := fixture.generation.LookupDeclaredRoute(method, doubaoOfficialDeleteDeclaredPath)
	require.True(t, found, "official native task route is not registered")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	actualPath := strings.Replace(doubaoOfficialDeleteDeclaredPath, ":task_id", providerID, 1)
	ctx.Request = httptest.NewRequest(method, actualPath, nil).WithContext(requestContext)
	ctx.Params = gin.Params{{Key: "task_id", Value: providerID}}
	ctx.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{
		Generation: fixture.generation, Plugin: binding.Plugin, Route: binding.Route,
	})
	common.SetContextKey(ctx, constant.ContextKeyUserId, userID)
	ctx.Set("id", userID)
	ctx.Set("role", common.RoleCommonUser)
	ctx.Set(common.RequestIdKey, "req-official-delete-test")
	middleware.PrepareTaskPluginRoute()(ctx)
	return recorder
}

func useRealDoubaoTaskAdaptors(t *testing.T) {
	t.Helper()
	previous := service.GetTaskAdaptorFunc
	service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor {
		return relay.GetTaskAdaptor(platform)
	}
	t.Cleanup(func() { service.GetTaskAdaptorFunc = previous })
}

func TestDoubaoArkOfficialQueuedDeleteCancelsUpstreamAndRefundsOnce(t *testing.T) {
	fixture := setupDoubaoNativeFixture(t, "cgt-delete-queued")
	require.NoError(t, fixture.database.AutoMigrate(&model.AuditLog{}))
	pluginSetting := `{"task_plugin_key":"doubao_ark_official"}`
	fixture.channel.Type = constant.ChannelTypeTaskPlugin
	fixture.channel.Setting = &pluginSetting
	require.NoError(t, fixture.database.Save(fixture.channel).Error)
	useRealDoubaoTaskAdaptors(t)

	now := time.Now().Unix()
	task := &model.Task{
		TaskID: "task-delete-queued-internal", ProviderTaskID: "cgt-delete-queued",
		UserId: 7, ChannelId: fixture.channel.Id, Platform: "doubao_ark_official",
		Status: model.TaskStatusQueued, Progress: "10%", Quota: 1000,
		CreatedAt: now, SubmitTime: now, UpdatedAt: now,
		Data: []byte(`{"id":"cgt-delete-queued","status":"queued"}`),
	}
	require.NoError(t, fixture.database.Create(task).Error)
	fixture.queryBody = string(task.Data)
	var userBefore model.User
	require.NoError(t, fixture.database.Select("quota").First(&userBefore, 7).Error)

	recorder := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodDelete, task.ProviderTaskID, 7)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.JSONEq(t, `{}`, recorder.Body.String())
	assert.Equal(t, int32(1), fixture.deleteCalls.Load())
	assert.Equal(t, "Bearer sk-test", <-fixture.deleteAuth)

	query := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodGet, task.ProviderTaskID, 7)
	require.Equal(t, http.StatusOK, query.Code, query.Body.String())
	assert.JSONEq(t, `{"id":"cgt-delete-queued","status":"cancelled"}`, query.Body.String())

	var persisted model.Task
	require.NoError(t, fixture.database.Where("task_id = ?", task.TaskID).First(&persisted).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), persisted.Status)
	assert.False(t, persisted.ProviderDeleted)
	var userAfter model.User
	require.NoError(t, fixture.database.Select("quota").First(&userAfter, 7).Error)
	assert.Equal(t, userBefore.Quota+1000, userAfter.Quota)

	var audit model.AuditLog
	require.NoError(t, fixture.database.Where("action = ?", "task_provider_cancel").Order("id DESC").First(&audit).Error)
	assert.Equal(t, 7, audit.UserId)
	assert.Equal(t, "req-official-delete-test", audit.RequestId)
	assert.True(t, audit.Success)
	assert.Contains(t, audit.Content, "cgt-delete-queued")
	assert.NotContains(t, audit.Content, "sk-test")
}
