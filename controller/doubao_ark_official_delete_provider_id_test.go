package controller

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoubaoArkOfficialDeleteRequiresProviderTaskID(t *testing.T) {
	fixture := setupDoubaoNativeFixture(t, "cgt-delete-expected-provider")
	task := &model.Task{
		TaskID: "task-delete-internal-must-not-be-upstream", ProviderTaskID: "",
		UserId: 7, ChannelId: fixture.channel.Id, Platform: "doubao_ark_official",
		Status: model.TaskStatusQueued, Quota: 1000,
		Data: []byte(`{"id":"task-delete-internal-must-not-be-upstream","status":"queued"}`),
	}
	require.NoError(t, fixture.database.Create(task).Error)
	require.NoError(t, fixture.database.AutoMigrate(&model.AuditLog{}))
	var userBefore model.User
	require.NoError(t, fixture.database.Select("quota").First(&userBefore, 7).Error)

	response := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodDelete, task.TaskID, 7)
	require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "task_not_found")
	assert.Equal(t, int32(0), fixture.deleteCalls.Load(), "an internal task ID must never be sent to Ark")

	var userAfter model.User
	require.NoError(t, fixture.database.Select("quota").First(&userAfter, 7).Error)
	assert.Equal(t, userBefore.Quota, userAfter.Quota, "rejected deletion must not refund quota")
	var persisted model.Task
	require.NoError(t, fixture.database.Where("task_id = ?", task.TaskID).First(&persisted).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusQueued), persisted.Status)
	assert.False(t, persisted.ProviderDeleted)

	adaptor := relay.GetTaskAdaptor(task.Platform)
	deletion, ok := adaptor.(service.TaskDeletionAdaptor)
	require.True(t, ok)
	upstreamResponse, err := deletion.DeleteTask(fixture.upstream.URL, "sk-test", task, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider task ID is required")
	assert.Nil(t, upstreamResponse)
	assert.Equal(t, int32(0), fixture.deleteCalls.Load(), "the direct adaptor guard must also reject an internal-ID fallback")
}
