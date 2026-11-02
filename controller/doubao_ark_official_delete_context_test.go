package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoubaoArkOfficialDeletePropagatesClientCancellationUpstream(t *testing.T) {
	fixture, task, _ := setupDoubaoOfficialDeleteStateTask(t, "cgt-delete-context", model.TaskStatusSuccess, "succeeded", 1000)
	deleteStarted := make(chan struct{})
	upstreamCancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(deleteStarted)
		select {
		case <-r.Context().Done():
			close(upstreamCancelled)
		case <-time.After(1500 * time.Millisecond):
		}
	}))
	defer upstream.Close()
	fixture.channel.BaseURL = &upstream.URL
	require.NoError(t, fixture.database.Save(fixture.channel).Error)

	requestContext, cancel := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- invokeDoubaoOfficialTaskRouteWithContext(t, fixture, http.MethodDelete, task.ProviderTaskID, 7, requestContext)
	}()
	select {
	case <-deleteStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("DELETE did not reach fake upstream")
	}
	cancel()

	observedCancellation := false
	select {
	case <-upstreamCancelled:
		observedCancellation = true
	case <-time.After(250 * time.Millisecond):
	}
	var recorder *httptest.ResponseRecorder
	select {
	case recorder = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("DELETE did not finish after client cancellation")
	}
	assert.True(t, observedCancellation, "upstream DELETE must use the downstream request context")
	assert.Equal(t, http.StatusBadGateway, recorder.Code, recorder.Body.String())

	var persisted model.Task
	require.NoError(t, fixture.database.Where("task_id = ?", task.TaskID).First(&persisted).Error)
	assert.False(t, persisted.ProviderDeleted)
}
