package controller

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoubaoArkOfficialDeleteRequiresEnabledChannelOptIn(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		setting       string
		channelStatus int
	}{
		{
			name:          "missing setting denies by default",
			setting:       `{"task_plugin_key":"doubao_ark_official"}`,
			channelStatus: common.ChannelStatusEnabled,
		},
		{
			name:          "explicit false denies",
			setting:       `{"task_plugin_key":"doubao_ark_official","allow_official_task_delete":false}`,
			channelStatus: common.ChannelStatusEnabled,
		},
		{
			name:          "disabled channel denies even when opted in",
			setting:       `{"task_plugin_key":"doubao_ark_official","allow_official_task_delete":true}`,
			channelStatus: common.ChannelStatusManuallyDisabled,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture, task, quotaBefore := setupDoubaoOfficialDeleteStateTask(t, "cgt-delete-permission-denied", model.TaskStatusQueued, "queued", 1000)
			fixture.channel.Setting = &testCase.setting
			fixture.channel.Status = testCase.channelStatus
			require.NoError(t, fixture.database.Save(fixture.channel).Error)

			recorder := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodDelete, task.ProviderTaskID, 7)

			assert.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
			assert.JSONEq(t, `{"error":{"code":"official_task_delete_disabled","message":"Official task deletion is disabled for this channel"}}`, recorder.Body.String())
			assert.Zero(t, fixture.queryCalls.Load(), "denial must precede the provider preflight GET")
			assert.Zero(t, fixture.deleteCalls.Load(), "denial must precede the provider DELETE")

			var persisted model.Task
			require.NoError(t, fixture.database.Where("id = ?", task.ID).First(&persisted).Error)
			assert.Equal(t, *task, persisted, "denial must not change task state, timestamps, data, or tombstones")
			var userAfter model.User
			require.NoError(t, fixture.database.Select("quota").First(&userAfter, 7).Error)
			assert.Equal(t, quotaBefore, userAfter.Quota, "denial must not refund or charge quota")
		})
	}
}

func TestDoubaoArkOfficialDeleteChecksOwnershipBeforeChannelPermission(t *testing.T) {
	fixture, owned, _ := setupDoubaoOfficialDeleteStateTask(t, "cgt-delete-permission-owned", model.TaskStatusQueued, "queued", 1000)
	deniedSetting := `{"task_plugin_key":"doubao_ark_official","allow_official_task_delete":false}`
	fixture.channel.Setting = &deniedSetting
	require.NoError(t, fixture.database.Save(fixture.channel).Error)
	now := time.Now().Unix()
	for index, task := range []*model.Task{
		{TaskID: "task-delete-permission-foreign", ProviderTaskID: "cgt-delete-permission-foreign", UserId: 8, Platform: "doubao_ark_official"},
		{TaskID: "task-delete-permission-wrong-plugin", ProviderTaskID: "cgt-delete-permission-wrong-plugin", UserId: 7, Platform: "doubao"},
		{TaskID: "task-delete-permission-duplicate-a", ProviderTaskID: "cgt-delete-permission-duplicate", UserId: 7, Platform: "doubao_ark_official"},
		{TaskID: "task-delete-permission-duplicate-b", ProviderTaskID: "cgt-delete-permission-duplicate", UserId: 7, Platform: "doubao_ark_official"},
	} {
		task.ChannelId = fixture.channel.Id
		task.Status = model.TaskStatusQueued
		task.Quota = 1000
		task.CreatedAt, task.SubmitTime, task.UpdatedAt = now+int64(index), now, now
		task.Data = []byte(fmt.Sprintf(`{"id":%q,"status":"queued"}`, task.ProviderTaskID))
		require.NoError(t, fixture.database.Create(task).Error)
	}

	for _, providerID := range []string{"cgt-delete-permission-foreign", "cgt-delete-permission-wrong-plugin", "cgt-delete-permission-duplicate", "cgt-delete-permission-missing"} {
		recorder := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodDelete, providerID, 7)
		assert.Equal(t, http.StatusNotFound, recorder.Code, providerID+": "+recorder.Body.String())
		assert.Contains(t, recorder.Body.String(), `"code":"task_not_found"`, providerID)
		assert.NotContains(t, recorder.Body.String(), "official_task_delete_disabled", providerID)
	}
	assert.Zero(t, fixture.queryCalls.Load())
	assert.Zero(t, fixture.deleteCalls.Load())

	var ownedAfter model.Task
	require.NoError(t, fixture.database.Where("id = ?", owned.ID).First(&ownedAfter).Error)
	assert.Equal(t, *owned, ownedAfter)
}
