package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const doubaoOfficialListDeclaredPath = "/doubao_ark_official/api/v3/contents/generations/tasks"

func invokeDoubaoOfficialListRoute(t *testing.T, fixture *doubaoNativeFixture, userID int) *httptest.ResponseRecorder {
	t.Helper()
	binding, found := fixture.generation.LookupDeclaredRoute(http.MethodGet, doubaoOfficialListDeclaredPath)
	require.True(t, found)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, doubaoOfficialListDeclaredPath, nil)
	ctx.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{
		Generation: fixture.generation, Plugin: binding.Plugin, Route: binding.Route,
	})
	common.SetContextKey(ctx, constant.ContextKeyUserId, userID)
	ctx.Set("id", userID)
	ctx.Set("role", common.RoleCommonUser)
	ctx.Set(common.RequestIdKey, "req-official-list-after-delete")
	middleware.PrepareTaskPluginRoute()(ctx)
	return recorder
}

func TestDoubaoArkOfficialTerminalDeleteTombstonesWithoutBilling(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		hostStatus model.TaskStatus
		status     string
	}{
		{name: "succeeded", hostStatus: model.TaskStatusSuccess, status: "succeeded"},
		{name: "failed", hostStatus: model.TaskStatusFailure, status: "failed"},
		{name: "cancelled", hostStatus: model.TaskStatusFailure, status: "cancelled"},
		{name: "expired", hostStatus: model.TaskStatusFailure, status: "expired"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			providerID := "cgt-delete-terminal-" + testCase.name
			fixture, task, quotaBefore := setupDoubaoOfficialDeleteStateTask(t, providerID, testCase.hostStatus, testCase.status, 1000)

			recorder := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodDelete, providerID, 7)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			assert.JSONEq(t, `{}`, recorder.Body.String())
			assert.Equal(t, int32(1), fixture.deleteCalls.Load())

			query := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodGet, providerID, 7)
			assert.Equal(t, http.StatusNotFound, query.Code, query.Body.String())
			assert.Contains(t, query.Body.String(), `"code":"task_not_found"`)
			list := invokeDoubaoOfficialListRoute(t, fixture, 7)
			require.Equal(t, http.StatusOK, list.Code, list.Body.String())
			assert.JSONEq(t, `{"total":0,"items":[]}`, list.Body.String())

			var persisted model.Task
			require.NoError(t, fixture.database.Where("task_id = ?", task.TaskID).First(&persisted).Error)
			assert.True(t, persisted.ProviderDeleted)
			assert.Equal(t, testCase.hostStatus, persisted.Status)
			var userAfter model.User
			require.NoError(t, fixture.database.Select("quota").First(&userAfter, 7).Error)
			assert.Equal(t, quotaBefore, userAfter.Quota)
		})
	}
}
