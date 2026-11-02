package controller

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoubaoArkOfficialDeleteFailsClosedAcrossOwnershipAndDuplicates(t *testing.T) {
	fixture, _, quotaBefore := setupDoubaoOfficialDeleteStateTask(t, "cgt-auth-owned", model.TaskStatusQueued, "queued", 1000)
	now := time.Now().Unix()
	for index, task := range []*model.Task{
		{TaskID: "task-auth-foreign", ProviderTaskID: "cgt-auth-foreign", UserId: 8, Platform: "doubao_ark_official"},
		{TaskID: "task-auth-wrong-plugin", ProviderTaskID: "cgt-auth-wrong-plugin", UserId: 7, Platform: "doubao"},
		{TaskID: "task-auth-duplicate-a", ProviderTaskID: "cgt-auth-duplicate", UserId: 7, Platform: "doubao_ark_official"},
		{TaskID: "task-auth-duplicate-b", ProviderTaskID: "cgt-auth-duplicate", UserId: 7, Platform: "doubao_ark_official"},
	} {
		task.ChannelId = fixture.channel.Id
		task.Status = model.TaskStatusQueued
		task.Quota = 1000
		task.CreatedAt, task.SubmitTime, task.UpdatedAt = now+int64(index), now, now
		task.Data = []byte(fmt.Sprintf(`{"id":%q,"status":"queued"}`, task.ProviderTaskID))
		require.NoError(t, fixture.database.Create(task).Error)
	}

	expectedBody := `{"error":{"code":"task_not_found","message":"Task not found"}}`
	for _, providerID := range []string{"cgt-auth-foreign", "cgt-auth-wrong-plugin", "cgt-auth-missing", "cgt-auth-duplicate"} {
		recorder := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodDelete, providerID, 7)
		assert.Equal(t, http.StatusNotFound, recorder.Code, providerID+": "+recorder.Body.String())
		assert.JSONEq(t, expectedBody, recorder.Body.String(), providerID)
	}
	assert.Equal(t, int32(0), fixture.deleteCalls.Load())
	var userAfter model.User
	require.NoError(t, fixture.database.Select("quota").First(&userAfter, 7).Error)
	assert.Equal(t, quotaBefore, userAfter.Quota)
	var tombstoned int64
	require.NoError(t, fixture.database.Model(&model.Task{}).Where("provider_deleted = ?", true).Count(&tombstoned).Error)
	assert.Zero(t, tombstoned)
}
