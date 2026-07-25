package seedance

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	taskcommon "github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var _ channel.TaskAdaptor = (*TaskAdaptor)(nil)
var _ channel.FullPrepaidTaskSubmitter = (*TaskAdaptor)(nil)
var _ channel.DurableTaskSubmitter = (*TaskAdaptor)(nil)
var _ channel.DeferredTaskSubmitResponder = (*TaskAdaptor)(nil)
var _ channel.TaskSubmitFailureClassifier = (*TaskAdaptor)(nil)
var _ interface {
	FetchTaskWithContext(
		context.Context,
		string,
		string,
		map[string]any,
		string,
	) (*http.Response, error)
} = (*TaskAdaptor)(nil)

type errorReadCloser struct {
	err    error
	closed atomic.Bool
}

func (r *errorReadCloser) Read([]byte) (int, error) {
	return 0, r.err
}

func (r *errorReadCloser) Close() error {
	r.closed.Store(true)
	return nil
}

func seedanceContext(method, body string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(method, "/v1/videos", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}

func hijackAndWaitForClientClose(
	t *testing.T,
	writer http.ResponseWriter,
	response string,
	started chan<- struct{},
	disconnected chan<- struct{},
) {
	t.Helper()
	conn, buffer, err := writer.(http.Hijacker).Hijack()
	require.NoError(t, err)
	defer conn.Close()
	if response != "" {
		_, err = buffer.WriteString(response)
		require.NoError(t, err)
		require.NoError(t, buffer.Flush())
	}
	close(started)
	_, _ = conn.Read(make([]byte, 1))
	close(disconnected)
}

func TestSeedDanceTaskAdaptorValidationBuildsOnlyProviderFields(t *testing.T) {
	c := seedanceContext(http.MethodPost, `{
		"model":"seedance-uncensored",
		"prompt":"  flower  ",
		"duration":10,
		"size":"1920x1080",
		"metadata":{
			"prompt_optimization":false,
			"multi_shot":true,
			"strict_duration":false,
			"negative_prompt":"blur",
			"ignored":"DO_NOT_FORWARD"
		},
		"ignored_top_level":"DO_NOT_FORWARD"
	}`)
	info := &relaycommon.RelayInfo{
		OriginModelName: ModelName,
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiKey:         "TOKEN",
			ChannelBaseUrl: "https://HOST/root/",
		},
		TaskRelayInfo: &relaycommon.TaskRelayInfo{},
	}
	a := &TaskAdaptor{}
	a.Init(info)

	require.Nil(t, a.ValidateRequestAndSetAction(c, info))
	assert.Equal(t, constant.TaskActionGenerate, info.Action)
	body, err := a.BuildRequestBody(c, info)
	require.NoError(t, err)
	payload, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, int64(len(payload)), info.UpstreamRequestBodySize)
	assert.JSONEq(t, `{
		"prompt":"flower",
		"duration":10,
		"resolution":"1080P",
		"prompt_optimization":false,
		"multi_shot":true,
		"strict_duration":false,
		"negative_prompt":"blur"
	}`, string(payload))
	assert.NotContains(t, string(payload), "ignored")
	assert.NotContains(t, string(payload), "TOKEN")

	requestURL, err := a.BuildRequestURL(info)
	require.NoError(t, err)
	assert.Equal(t, "https://HOST/root/generate", requestURL)
	req := httptest.NewRequest(http.MethodPost, requestURL, nil)
	require.NoError(t, a.BuildRequestHeader(c, req, info))
	assert.Equal(t, "Bearer TOKEN", req.Header.Get("Authorization"))
	assert.Equal(t, "application/json", req.Header.Get("Content-Type"))
	assert.Equal(t, "application/json", req.Header.Get("Accept"))
	assert.Len(t, req.Header, 3)
}

func TestSeedDanceTaskAdaptorRejectsUnsupportedModel(t *testing.T) {
	c := seedanceContext(http.MethodPost, `{"prompt":"flower"}`)
	info := &relaycommon.RelayInfo{OriginModelName: "OTHER_MODEL"}

	taskErr := (&TaskAdaptor{}).ValidateRequestAndSetAction(c, info)

	require.NotNil(t, taskErr)
	assert.Equal(t, "model_not_supported", taskErr.Code)
	assert.Equal(t, http.StatusBadRequest, taskErr.StatusCode)
	assert.True(t, taskErr.LocalError)
}

func TestSeedDanceTaskAdaptorDoRequestUsesDeadlineAndProviderRequest(t *testing.T) {
	type observedRequest struct {
		method        string
		path          string
		authorization string
		contentType   string
		accept        string
		contentLength int64
		body          string
	}
	observedCh := make(chan observedRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		req *http.Request,
	) {
		payload, readErr := io.ReadAll(req.Body)
		require.NoError(t, readErr)
		observedCh <- observedRequest{
			method:        req.Method,
			path:          req.URL.Path,
			authorization: req.Header.Get("Authorization"),
			contentType:   req.Header.Get("Content-Type"),
			accept:        req.Header.Get("Accept"),
			contentLength: req.ContentLength,
			body:          string(payload),
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"task_id":"UPSTREAM_TASK_ID"}`))
	}))
	defer upstream.Close()

	normalized := &NormalizedRequest{
		Prompt:      "flower",
		ImageBase64: "PAYLOAD",
		Duration:    10,
		Resolution:  "720P",
	}
	c := seedanceContext(http.MethodPost, `CLIENT_BODY_MUST_NOT_BE_READ`)
	c.Set(normalizedRequestContextKey, normalized)
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiKey:         "TOKEN",
			ChannelBaseUrl: upstream.URL,
		},
		TaskRelayInfo: &relaycommon.TaskRelayInfo{},
	}
	a := &TaskAdaptor{}
	a.Init(info)
	body, err := a.BuildRequestBody(c, info)
	require.NoError(t, err)

	resp, err := a.DoRequest(c, info, body)
	require.NoError(t, err)
	require.NotNil(t, resp)
	observed := <-observedCh
	assert.Equal(t, http.MethodPost, observed.method)
	assert.Equal(t, "/generate", observed.path)
	assert.Equal(t, "Bearer TOKEN", observed.authorization)
	assert.Equal(t, "application/json", observed.contentType)
	assert.Equal(t, "application/json", observed.accept)
	assert.Equal(t, info.UpstreamRequestBodySize, observed.contentLength)
	assert.JSONEq(t, `{
		"prompt":"flower",
		"image_base64":"PAYLOAD",
		"duration":10,
		"resolution":"720P"
	}`, observed.body)
	assert.Equal(t, 60*time.Second, submitTimeout)
	require.NoError(t, resp.Body.Close())
}

func TestSeedDanceTaskAdaptorSubmitParentCancellationClosesGenerate(t *testing.T) {
	started := make(chan struct{})
	disconnected := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		req *http.Request,
	) {
		assert.Equal(t, "/generate", req.URL.Path)
		hijackAndWaitForClientClose(t, writer, "", started, disconnected)
	}))
	defer upstream.Close()

	parent, cancelParent := context.WithCancel(context.Background())
	c := seedanceContext(http.MethodPost, "")
	c.Request = c.Request.WithContext(parent)
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiKey:         "TOKEN",
			ChannelBaseUrl: upstream.URL,
		},
	}
	a := &TaskAdaptor{}
	a.Init(info)

	errCh := make(chan error, 1)
	go func() {
		_, err := a.DoRequest(c, info, bytes.NewReader([]byte(`{}`)))
		errCh <- err
	}()
	<-started
	cancelParent()

	require.ErrorIs(t, <-errCh, context.Canceled)
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not close /generate connection")
	}
}

func TestDoResponseTreatsAmbiguousHTTP200AsUnknown(t *testing.T) {
	interrupted := &errorReadCloser{err: io.ErrUnexpectedEOF}
	cases := []struct {
		name string
		body io.ReadCloser
	}{
		{"truncated", io.NopCloser(strings.NewReader(`{"requestId":"R"`))},
		{"invalid", io.NopCloser(strings.NewReader(`{not-json}`))},
		{"missing task id", io.NopCloser(strings.NewReader(
			`{"requestId":"R","status":"accepted"}`,
		))},
		{"empty task id", io.NopCloser(strings.NewReader(
			`{"requestId":"R","task_id":"","status":"accepted"}`,
		))},
		{"read interrupted", interrupted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &TaskAdaptor{}
			_, taskData, taskErr := a.DoResponse(
				&gin.Context{},
				&http.Response{StatusCode: http.StatusOK, Body: tc.body},
				&relaycommon.RelayInfo{},
			)
			require.NotNil(t, taskErr)
			assert.Empty(t, taskData)
			require.Equal(t, "seedance_submit_outcome_unknown", taskErr.Code)
			require.NotNil(t, taskErr.Retryable)
			assert.False(t, *taskErr.Retryable)
		})
	}
	assert.True(t, interrupted.closed.Load())
}

func TestDoResponseCleansSuccessfulAndExplicitBusinessFailureData(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantID    string
		wantCode  string
		wantClean string
	}{
		{
			name: "success",
			body: `{
				"requestId":"REQUEST_ID",
				"task_id":"UPSTREAM_TASK_ID",
				"status":"accepted",
				"success":true,
				"prompt":"SECRET_PROMPT",
				"image_base64":"SECRET_BASE64"
			}`,
			wantID: "UPSTREAM_TASK_ID",
			wantClean: `{
				"requestId":"REQUEST_ID",
				"status":"accepted",
				"success":true,
				"model":"seedance-uncensored",
				"seconds":"10",
				"size":"1080P"
			}`,
		},
		{
			name: "explicit business failure keeps reliable task id",
			body: `{
				"requestId":"REQUEST_ID",
				"task_id":"UPSTREAM_TASK_ID",
				"status":"failed",
				"success":false,
				"errCode":"CONTENT_POLICY",
				"errMessage":"rejected"
			}`,
			wantID:   "UPSTREAM_TASK_ID",
			wantCode: "upstream_error",
			wantClean: `{
				"requestId":"REQUEST_ID",
				"status":"failed",
				"success":false,
				"errCode":"CONTENT_POLICY",
				"errMessage":"rejected",
				"model":"seedance-uncensored",
				"seconds":"10",
				"size":"1080P"
			}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := seedanceContext(http.MethodPost, "")
			c.Set(normalizedRequestContextKey, &NormalizedRequest{
				Duration:   10,
				Resolution: "1080P",
			})
			upstreamID, taskData, taskErr := (&TaskAdaptor{}).DoResponse(
				c,
				&http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(test.body)),
				},
				&relaycommon.RelayInfo{},
			)
			assert.Equal(t, test.wantID, upstreamID)
			assert.JSONEq(t, test.wantClean, string(taskData))
			assert.NotContains(t, string(taskData), "UPSTREAM_TASK_ID")
			assert.NotContains(t, string(taskData), "SECRET_PROMPT")
			assert.NotContains(t, string(taskData), "SECRET_BASE64")
			if test.wantCode == "" {
				assert.Nil(t, taskErr)
				return
			}
			require.NotNil(t, taskErr)
			assert.Equal(t, test.wantCode, taskErr.Code)
			require.NotNil(t, taskErr.Retryable)
			assert.False(t, *taskErr.Retryable)
		})
	}
}

func TestClassifySubmitFailureRetryMatrix(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		requestErr error
		code       string
		retryable  bool
		upstreamID string
	}{
		{"auth 401", 401, `{"success":false,"errCode":"401"}`, nil,
			"upstream_authentication_error", false, ""},
		{"auth 403", 403, `{"success":false,"errCode":"403"}`, nil,
			"upstream_authentication_error", false, ""},
		{"confirmed empty 429", 429,
			`{"success":false,"errCode":"429","errMessage":"rate limited","data":null}`,
			nil, "upstream_rate_limit_error", true, ""},
		{"confirmed numeric 429", 429,
			`{"success":false,"errCode":429,"errMessage":"rate limited","data":{}}`,
			nil, "upstream_rate_limit_error", true, ""},
		{"429 missing data", 429,
			`{"success":false,"errCode":"429","errMessage":"rate limited"}`,
			nil, "seedance_submit_outcome_unknown", false, ""},
		{"429 with task", 429,
			`{"success":false,"errCode":"429","task_id":"UPSTREAM_TASK_ID"}`,
			nil, "seedance_submit_outcome_unknown", false, "UPSTREAM_TASK_ID"},
		{"429 with data", 429,
			`{"success":false,"errCode":"429","data":{"task_id":"MAYBE_CREATED"}}`,
			nil, "seedance_submit_outcome_unknown", false, ""},
		{"429 ambiguous business shape", 429,
			`{"errCode":"429","data":null}`,
			nil, "seedance_submit_outcome_unknown", false, ""},
		{"429 invalid json", 429, `{"success":false`, nil,
			"seedance_submit_outcome_unknown", false, ""},
		{"gateway 502", 502, ``, nil,
			"seedance_submit_outcome_unknown", false, ""},
		{"gateway 503", 503, ``, nil,
			"seedance_submit_outcome_unknown", false, ""},
		{"gateway 504", 504, ``, nil,
			"seedance_submit_outcome_unknown", false, ""},
		{"timeout", 0, ``, context.DeadlineExceeded,
			"seedance_submit_outcome_unknown", false, ""},
		{"eof", 0, ``, io.EOF,
			"seedance_submit_outcome_unknown", false, ""},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var resp *http.Response
			if test.status != 0 {
				reader := io.NopCloser(strings.NewReader(test.body))
				resp = &http.Response{StatusCode: test.status, Body: reader}
			}
			classification := (&TaskAdaptor{}).ClassifyTaskSubmitFailure(resp, test.requestErr)
			require.NotNil(t, classification)
			require.NotNil(t, classification.TaskError)
			assert.Equal(t, test.code, classification.TaskError.Code)
			require.NotNil(t, classification.TaskError.Retryable)
			assert.Equal(t, test.retryable, *classification.TaskError.Retryable)
			assert.Equal(t, test.upstreamID, classification.UpstreamTaskID)
			assert.NotContains(t, string(classification.TaskData), "UPSTREAM_TASK_ID")
			assert.NotContains(t, string(classification.TaskData), "MAYBE_CREATED")
		})
	}
}

func TestClassifySubmitFailureClosesResponseBody(t *testing.T) {
	body := &errorReadCloser{err: io.ErrUnexpectedEOF}
	classification := (&TaskAdaptor{}).ClassifyTaskSubmitFailure(
		&http.Response{StatusCode: http.StatusTooManyRequests, Body: body},
		nil,
	)
	require.NotNil(t, classification)
	assert.True(t, body.closed.Load())
	assert.Equal(t, "seedance_submit_outcome_unknown", classification.TaskError.Code)
}

func TestClassifySubmitFailureClosesResponseBodyWhenRequestFailed(t *testing.T) {
	body := &errorReadCloser{err: io.EOF}
	classification := (&TaskAdaptor{}).ClassifyTaskSubmitFailure(
		&http.Response{StatusCode: http.StatusBadGateway, Body: body},
		context.DeadlineExceeded,
	)
	require.NotNil(t, classification)
	assert.True(t, body.closed.Load())
	assert.Equal(t, "seedance_submit_outcome_unknown", classification.TaskError.Code)
	require.NotNil(t, classification.TaskError.Retryable)
	assert.False(t, *classification.TaskError.Retryable)
}

func TestSeedDanceTaskAdaptorBuildDeferredPublicResponse(t *testing.T) {
	a := &TaskAdaptor{}
	info := &relaycommon.RelayInfo{
		TaskRelayInfo: &relaycommon.TaskRelayInfo{
			PublicTaskID: "PUBLIC_TASK_ID",
		},
	}
	response, err := a.BuildTaskSubmitResponse(info, []byte(`{
		"requestId":"REQUEST_ID",
		"model":"seedance-uncensored",
		"seconds":"10",
		"size":"1080P"
	}`))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSubmitted), response.InitialStatus)
	assert.Equal(t, taskcommon.ProgressSubmitted, response.InitialProgress)
	body, ok := response.Body.(*dto.OpenAIVideo)
	require.True(t, ok)
	assert.Equal(t, "PUBLIC_TASK_ID", body.ID)
	assert.Equal(t, "PUBLIC_TASK_ID", body.TaskID)
	assert.Equal(t, ModelName, body.Model)
	assert.Equal(t, dto.VideoStatusQueued, body.Status)
	assert.Equal(t, 10, body.Progress)
	assert.Equal(t, "10", body.Seconds)
	assert.Equal(t, "1080P", body.Size)
	assert.NotZero(t, body.CreatedAt)
	encoded, err := common.Marshal(body)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "REQUEST_ID")
}

func TestFetchTaskWithContextUsesStatusDeadlineAndCancellation(t *testing.T) {
	started := make(chan struct{})
	disconnected := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		req *http.Request,
	) {
		assert.Equal(t, http.MethodGet, req.Method)
		assert.Equal(t, "/status/UPSTREAM_TASK_ID", req.URL.Path)
		assert.Equal(t, "Bearer TOKEN", req.Header.Get("Authorization"))
		hijackAndWaitForClientClose(t, writer, "", started, disconnected)
	}))
	defer upstream.Close()

	parent, cancelParent := context.WithCancel(context.Background())

	a := &TaskAdaptor{}
	errCh := make(chan error, 1)
	go func() {
		_, err := a.FetchTaskWithContext(
			parent,
			upstream.URL+"/",
			"TOKEN",
			map[string]any{"task_id": "UPSTREAM_TASK_ID"},
			"",
		)
		errCh <- err
	}()
	<-started
	cancelParent()

	require.ErrorIs(t, <-errCh, context.Canceled)
	assert.Equal(t, 30*time.Second, statusTimeout)
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("scheduler cancellation did not close /status connection")
	}
}

func TestFetchTaskWithContextRejectsMissingTaskID(t *testing.T) {
	_, err := (&TaskAdaptor{}).FetchTaskWithContext(
		context.Background(),
		"https://HOST",
		"TOKEN",
		map[string]any{},
		"",
	)
	require.EqualError(t, err, "task_id is required")
}

func TestFetchTaskSuccessfulResponseOwnsContextUntilBodyClose(t *testing.T) {
	responseStarted := make(chan struct{})
	disconnected := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		hijackAndWaitForClientClose(
			t,
			writer,
			"HTTP/1.1 200 OK\r\n"+
				"Content-Type: application/json\r\n"+
				"Transfer-Encoding: chunked\r\n\r\n",
			responseStarted,
			disconnected,
		)
	}))
	defer upstream.Close()

	resp, err := (&TaskAdaptor{}).FetchTaskWithContext(
		context.Background(),
		upstream.URL,
		"TOKEN",
		map[string]any{"task_id": "UPSTREAM_TASK_ID"},
		"",
	)
	require.NoError(t, err)
	<-responseStarted
	select {
	case <-disconnected:
		t.Fatal("successful response canceled before body close")
	default:
	}
	require.NoError(t, resp.Body.Close())
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("response body close did not cancel derived context")
	}
}

func TestParseTaskResultMapsStatusesAndRejectsUnknown(t *testing.T) {
	tests := []struct {
		status   string
		expected model.TaskStatus
		progress string
	}{
		{"accepted", model.TaskStatusSubmitted, "10%"},
		{"queued", model.TaskStatusQueued, "20%"},
		{"running", model.TaskStatusInProgress, "30%"},
		{"processing", model.TaskStatusInProgress, "30%"},
		{"completed", model.TaskStatusSuccess, "100%"},
		{"failed", model.TaskStatusFailure, "100%"},
	}
	for _, test := range tests {
		t.Run(test.status, func(t *testing.T) {
			result, err := (&TaskAdaptor{}).ParseTaskResult([]byte(`{
				"requestId":"REQUEST_ID",
				"task_id":"UPSTREAM_TASK_ID",
				"status":` + strconv.Quote(test.status) + `,
				"success":false,
				"errCode":"FAILED",
				"errMessage":"safe reason",
				"prompt":"SECRET_PROMPT",
				"image_base64":"SECRET_BASE64"
			}`))
			require.NoError(t, err)
			assert.Equal(t, string(test.expected), result.Status)
			assert.Equal(t, test.progress, result.Progress)
			if test.expected == model.TaskStatusFailure {
				assert.Equal(t, "safe reason", result.Reason)
			} else {
				assert.Empty(t, result.Reason)
			}
			assert.Empty(t, result.TaskID)
		})
	}

	result, err := (&TaskAdaptor{}).ParseTaskResult([]byte(`{"status":"FUTURE_STATUS"}`))
	require.ErrorContains(t, err, `unknown Seed Dance status "FUTURE_STATUS"`)
	assert.Nil(t, result)
}

func TestSeedDanceTaskAdaptorConvertToOpenAIVideoUsesPublicFieldsAndBillingSnapshot(t *testing.T) {
	task := &model.Task{
		TaskID:     "PUBLIC_TASK_ID",
		Status:     model.TaskStatusSuccess,
		Progress:   "100%",
		CreatedAt:  100,
		UpdatedAt:  190,
		FinishTime: 200,
		Data: json.RawMessage(`{
			"task_id":"UPSTREAM_TASK_ID",
			"prompt":"SECRET_PROMPT",
			"image_base64":"SECRET_BASE64",
			"seconds":"999",
			"size":"SECRET_SIZE"
		}`),
		PrivateData: model.TaskPrivateData{
			UpstreamTaskID: "UPSTREAM_TASK_ID",
			BillingContext: &model.TaskBillingContext{
				OriginModelName: ModelName,
				OtherRatios: map[string]float64{
					"seconds":    10,
					"resolution": 2.25,
				},
			},
		},
	}

	encoded, err := (&TaskAdaptor{}).ConvertToOpenAIVideo(task)
	require.NoError(t, err)
	var video dto.OpenAIVideo
	require.NoError(t, common.Unmarshal(encoded, &video))
	assert.Equal(t, "PUBLIC_TASK_ID", video.ID)
	assert.Equal(t, "PUBLIC_TASK_ID", video.TaskID)
	assert.Equal(t, ModelName, video.Model)
	assert.Equal(t, dto.VideoStatusCompleted, video.Status)
	assert.Equal(t, 100, video.Progress)
	assert.Equal(t, int64(100), video.CreatedAt)
	assert.Equal(t, int64(200), video.CompletedAt)
	assert.Equal(t, "10", video.Seconds)
	assert.Equal(t, "1080P", video.Size)
	assert.NotContains(t, string(encoded), "UPSTREAM_TASK_ID")
	assert.NotContains(t, string(encoded), "SECRET_PROMPT")
	assert.NotContains(t, string(encoded), "SECRET_BASE64")
	assert.NotContains(t, string(encoded), "SECRET_SIZE")
}
