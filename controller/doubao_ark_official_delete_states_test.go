package controller

import (
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupDoubaoOfficialDeleteStateTask(t *testing.T, providerID string, hostStatus model.TaskStatus, officialStatus string, quota int) (*doubaoNativeFixture, *model.Task, int) {
	t.Helper()
	fixture := setupDoubaoNativeFixture(t, providerID)
	require.NoError(t, fixture.database.AutoMigrate(&model.AuditLog{}))
	pluginSetting := `{"task_plugin_key":"doubao_ark_official"}`
	fixture.channel.Type = constant.ChannelTypeTaskPlugin
	fixture.channel.Setting = &pluginSetting
	require.NoError(t, fixture.database.Save(fixture.channel).Error)
	useRealDoubaoTaskAdaptors(t)
	now := time.Now().Unix()
	task := &model.Task{
		TaskID: "task-internal-" + providerID, ProviderTaskID: providerID,
		UserId: 7, ChannelId: fixture.channel.Id, Platform: "doubao_ark_official",
		Status: hostStatus, Progress: "50%", Quota: quota,
		CreatedAt: now, SubmitTime: now, UpdatedAt: now,
		Data: []byte(`{"id":"` + providerID + `","status":"` + officialStatus + `"}`),
		PrivateData: model.TaskPrivateData{
			UpstreamTaskID: providerID,
		},
	}
	require.NoError(t, fixture.database.Create(task).Error)
	var user model.User
	require.NoError(t, fixture.database.Select("quota").First(&user, 7).Error)
	return fixture, task, user.Quota
}

func TestDoubaoArkOfficialRunningDeleteIsRejectedWithoutMutationOrRefund(t *testing.T) {
	fixture, task, quotaBefore := setupDoubaoOfficialDeleteStateTask(t, "cgt-delete-running", model.TaskStatusInProgress, "running", 1000)

	recorder := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodDelete, task.ProviderTaskID, 7)
	assert.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), `"code":"invalid_request"`)
	assert.Equal(t, int32(0), fixture.deleteCalls.Load())

	query := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodGet, task.ProviderTaskID, 7)
	require.Equal(t, http.StatusOK, query.Code, query.Body.String())
	assert.Contains(t, query.Body.String(), `"status":"running"`)
	var userAfter model.User
	require.NoError(t, fixture.database.Select("quota").First(&userAfter, 7).Error)
	assert.Equal(t, quotaBefore, userAfter.Quota)
}
