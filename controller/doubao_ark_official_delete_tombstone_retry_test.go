package controller

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestDoubaoArkOfficialTombstoneFailureIsRetrySafe(t *testing.T) {
	fixture, task, quotaBefore := setupDoubaoOfficialDeleteStateTask(t, "cgt-delete-tombstone-retry", model.TaskStatusSuccess, "succeeded", 1000)
	const callbackName = "test:doubao_official_fail_tombstone_once"
	var failOnce atomic.Bool
	failOnce.Store(true)
	require.NoError(t, fixture.database.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		updates, ok := tx.Statement.Dest.(map[string]any)
		if !ok || tx.Statement.Table != "tasks" {
			return
		}
		if _, tombstone := updates["provider_deleted"]; tombstone && failOnce.CompareAndSwap(true, false) {
			tx.AddError(errors.New("injected tombstone write failure"))
		}
	}))

	first := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodDelete, task.ProviderTaskID, 7)
	assert.Equal(t, http.StatusInternalServerError, first.Code, first.Body.String())
	query := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodGet, task.ProviderTaskID, 7)
	require.Equal(t, http.StatusOK, query.Code, query.Body.String())
	assert.Contains(t, query.Body.String(), `"status":"succeeded"`)

	fixture.deleteStatus.Store(http.StatusNotFound)
	retry := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodDelete, task.ProviderTaskID, 7)
	require.Equal(t, http.StatusOK, retry.Code, retry.Body.String())
	assert.JSONEq(t, `{}`, retry.Body.String())
	assert.Equal(t, int32(2), fixture.deleteCalls.Load())
	query = invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodGet, task.ProviderTaskID, 7)
	assert.Equal(t, http.StatusNotFound, query.Code, query.Body.String())

	var persisted model.Task
	require.NoError(t, fixture.database.Where("task_id = ?", task.TaskID).First(&persisted).Error)
	assert.True(t, persisted.ProviderDeleted)
	var userAfter model.User
	require.NoError(t, fixture.database.Select("quota").First(&userAfter, 7).Error)
	assert.Equal(t, quotaBefore, userAfter.Quota)
}
