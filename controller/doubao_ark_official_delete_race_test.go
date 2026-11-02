package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoubaoArkOfficialPollerDeleteRaceRefundsExactlyOnce(t *testing.T) {
	providerID := "cgt-delete-race"
	fixture, task, quotaBefore := setupDoubaoOfficialDeleteStateTask(t, providerID, model.TaskStatusQueued, "queued", 1000)
	fixture.channel.SetOtherSettings(dto.ChannelOtherSettings{DisableTaskPollingSleep: true})
	require.NoError(t, fixture.database.Save(fixture.channel).Error)
	sqlDB, err := fixture.database.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	fixture.queryBodyForCall = func(call int32) string {
		if call == 1 {
			return `{"id":"cgt-delete-race","status":"failed","error":{"code":"upstream_failed","message":"generation failed"}}`
		}
		return `{"id":"cgt-delete-race","status":"queued"}`
	}
	fixture.queryStarted = make(chan struct{})
	fixture.queryRelease = make(chan struct{})
	fixture.deleteStarted = make(chan struct{})
	fixture.deleteRelease = make(chan struct{})

	pollDone := make(chan error, 1)
	go func() {
		pollDone <- service.UpdateVideoTasks(context.Background(), constant.TaskPlatform("doubao_ark_official"),
			map[int][]string{fixture.channel.Id: {providerID}}, map[string]*model.Task{providerID: task})
	}()
	select {
	case <-fixture.queryStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("poller did not reach fake upstream")
	}

	deleteDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		deleteDone <- invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodDelete, providerID, 7)
	}()
	select {
	case <-fixture.deleteStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("DELETE did not reach fake upstream")
	}
	close(fixture.queryRelease)
	select {
	case err := <-pollDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("poller did not finish")
	}
	close(fixture.deleteRelease)
	select {
	case recorder := <-deleteDone:
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	case <-time.After(10 * time.Second):
		t.Fatal("DELETE did not finish")
	}

	assert.Equal(t, int32(1), fixture.deleteCalls.Load())
	assert.Equal(t, int32(2), fixture.queryCalls.Load(), "poller and DELETE preflight must each reconcile provider state")
	var persisted model.Task
	require.NoError(t, fixture.database.Where("task_id = ?", task.TaskID).First(&persisted).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), persisted.Status)
	assert.Contains(t, []string{"cancelled", "failed"}, officialStatusFromData(t, persisted.Data))
	assert.True(t, persisted.ProviderDeleted, "the upstream record is gone even when the poller wins local settlement")
	query := invokeDoubaoOfficialTaskRoute(t, fixture, http.MethodGet, providerID, 7)
	assert.Equal(t, http.StatusNotFound, query.Code, query.Body.String())
	list := invokeDoubaoOfficialListRoute(t, fixture, 7)
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	assert.JSONEq(t, `{"total":0,"items":[]}`, list.Body.String())
	var userAfter model.User
	require.NoError(t, fixture.database.Select("quota").First(&userAfter, 7).Error)
	assert.Equal(t, quotaBefore+1000, userAfter.Quota, "poller and DELETE must have exactly one billing winner")
}

func officialStatusFromData(t *testing.T, data []byte) string {
	t.Helper()
	var payload struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(data, &payload))
	return payload.Status
}
