package controller

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoubaoArkOfficialDeleteRejectsStaleQueuedWhenProviderSucceeded(t *testing.T) {
	fixture, task, quotaBefore := setupDoubaoOfficialDeleteStateTask(t, "cgt-delete-stale-succeeded", model.TaskStatusQueued, "queued", 1000)
	fixture.queryBody = `{"id":"cgt-delete-stale-succeeded","status":"succeeded","content":{"video_url":"https://cdn.example/succeeded.mp4"},"usage":{"total_tokens":123}}`

	recorder := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodDelete, task.ProviderTaskID, 7)
	require.Equal(t, http.StatusConflict, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), `"code":"request_conflict"`)
	assert.Zero(t, fixture.deleteCalls.Load(), "a provider-succeeded task must never receive a cancellation DELETE")
	assert.Equal(t, int32(1), fixture.queryCalls.Load(), "DELETE must authenticate and reconcile provider state first")

	var persisted model.Task
	require.NoError(t, fixture.database.Where("task_id = ?", task.TaskID).First(&persisted).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusQueued), persisted.Status)
	assert.Equal(t, "queued", officialStatusFromData(t, persisted.Data), "a stale local snapshot must not be overwritten as cancelled")
	assert.False(t, persisted.ProviderDeleted)
	var userAfter model.User
	require.NoError(t, fixture.database.Select("quota").First(&userAfter, 7).Error)
	assert.Equal(t, quotaBefore, userAfter.Quota, "a successful billable task must not be refunded as queued")
}
