package controller

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoubaoArkOfficialQueuedUpstreamFailureKeepsTaskAndRefundConsistent(t *testing.T) {
	for _, upstreamStatus := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(fmt.Sprintf("upstream_%d", upstreamStatus), func(t *testing.T) {
			providerID := fmt.Sprintf("cgt-delete-upstream-%d", upstreamStatus)
			fixture, task, quotaBefore := setupDoubaoOfficialDeleteStateTask(t, providerID, model.TaskStatusQueued, "queued", 1000)
			fixture.deleteStatus.Store(int32(upstreamStatus))

			recorder := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodDelete, providerID, 7)
			assert.Equal(t, http.StatusBadGateway, recorder.Code, recorder.Body.String())
			assert.NotContains(t, recorder.Body.String(), "upstream-sensitive-detail")
			assert.NotContains(t, recorder.Body.String(), "sk-test")
			assert.Equal(t, int32(1), fixture.deleteCalls.Load())

			query := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodGet, providerID, 7)
			require.Equal(t, http.StatusOK, query.Code, query.Body.String())
			assert.Contains(t, query.Body.String(), `"status":"queued"`)
			var persisted model.Task
			require.NoError(t, fixture.database.Where("task_id = ?", task.TaskID).First(&persisted).Error)
			assert.False(t, persisted.ProviderDeleted)
			var userAfter model.User
			require.NoError(t, fixture.database.Select("quota").First(&userAfter, 7).Error)
			assert.Equal(t, quotaBefore, userAfter.Quota)
		})
	}
}

func TestDoubaoArkOfficialTerminalNotFoundRetriesSafelyWithoutRefund(t *testing.T) {
	fixture, task, quotaBefore := setupDoubaoOfficialDeleteStateTask(t, "cgt-delete-retry-terminal", model.TaskStatusSuccess, "succeeded", 1000)
	fixture.deleteStatus.Store(http.StatusInternalServerError)
	failed := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodDelete, task.ProviderTaskID, 7)
	assert.Equal(t, http.StatusBadGateway, failed.Code, failed.Body.String())
	assert.NotContains(t, failed.Body.String(), "upstream-sensitive-detail")
	assert.NotContains(t, failed.Body.String(), "sk-test")
	query := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodGet, task.ProviderTaskID, 7)
	require.Equal(t, http.StatusOK, query.Code, query.Body.String())
	assert.Contains(t, query.Body.String(), `"status":"succeeded"`)

	fixture.deleteStatus.Store(http.StatusNotFound)
	deleted := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodDelete, task.ProviderTaskID, 7)
	require.Equal(t, http.StatusOK, deleted.Code, deleted.Body.String())
	assert.JSONEq(t, `{}`, deleted.Body.String())
	assert.Equal(t, int32(2), fixture.deleteCalls.Load())
	query = invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodGet, task.ProviderTaskID, 7)
	assert.Equal(t, http.StatusNotFound, query.Code, query.Body.String())

	repeated := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodDelete, task.ProviderTaskID, 7)
	assert.Equal(t, http.StatusNotFound, repeated.Code, repeated.Body.String())
	assert.Equal(t, int32(2), fixture.deleteCalls.Load())
	var userAfter model.User
	require.NoError(t, fixture.database.Select("quota").First(&userAfter, 7).Error)
	assert.Equal(t, quotaBefore, userAfter.Quota)
}
