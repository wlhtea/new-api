package controller

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testOfficialPreflightMaxBodyBytes = 1 << 20

type countingPreflightBody struct {
	reader   io.Reader
	read     atomic.Int64
	isClosed atomic.Bool
}

func (b *countingPreflightBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read.Add(int64(n))
	return n, err
}

func (b *countingPreflightBody) Close() error {
	b.isClosed.Store(true)
	return nil
}

type preflightLimitAdaptor struct {
	service.TaskPollingAdaptor
	body             *countingPreflightBody
	deleteCalls      atomic.Int32
	preflightContext context.Context
}

func (a *preflightLimitAdaptor) FetchTaskContext(ctx context.Context, _, _ string, _ *model.Task, _ string) (*http.Response, error) {
	a.preflightContext = ctx
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: a.body}, nil
}

func (a *preflightLimitAdaptor) DeleteTask(context.Context, string, string, *model.Task, string) (*http.Response, error) {
	a.deleteCalls.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}, nil
}

func TestDoubaoArkOfficialDeleteRejectsOversizedPreflightWithinLimit(t *testing.T) {
	fixture, task, quotaBefore := setupDoubaoOfficialDeleteStateTask(t, "cgt-delete-preflight-limit", model.TaskStatusQueued, "queued", 1000)
	payload := `{"id":"` + task.ProviderTaskID + `","status":"queued","padding":"` +
		strings.Repeat("x", testOfficialPreflightMaxBodyBytes) + `"}`
	body := &countingPreflightBody{reader: strings.NewReader(payload)}
	adaptor := &preflightLimitAdaptor{
		TaskPollingAdaptor: relay.GetTaskAdaptor(constant.TaskPlatform("doubao_ark_official")),
		body:               body,
	}
	previousAdaptor := service.GetTaskAdaptorFunc
	service.GetTaskAdaptorFunc = func(constant.TaskPlatform) service.TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { service.GetTaskAdaptorFunc = previousAdaptor })

	type requestContextKey struct{}
	requestContext := context.WithValue(context.Background(), requestContextKey{}, "downstream-context")
	recorder := invokeDoubaoOfficialTaskRouteWithContext(t, fixture, http.MethodDelete, task.ProviderTaskID, 7, requestContext)

	assert.Equal(t, http.StatusBadGateway, recorder.Code, recorder.Body.String())
	assert.LessOrEqual(t, body.read.Load(), int64(testOfficialPreflightMaxBodyBytes+1), "preflight must read only the limit plus one overflow-detection byte")
	assert.True(t, body.isClosed.Load())
	assert.Zero(t, adaptor.deleteCalls.Load(), "an unverified oversized preflight must never issue DELETE")
	require.NotNil(t, adaptor.preflightContext)
	assert.Equal(t, "downstream-context", adaptor.preflightContext.Value(requestContextKey{}), "preflight must retain the downstream request context")

	var persisted model.Task
	require.NoError(t, fixture.database.Where("task_id = ?", task.TaskID).First(&persisted).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusQueued), persisted.Status)
	assert.False(t, persisted.ProviderDeleted)
	var userAfter model.User
	require.NoError(t, fixture.database.Select("quota").First(&userAfter, 7).Error)
	assert.Equal(t, quotaBefore, userAfter.Quota)
}
