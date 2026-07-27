package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type taskPollingFetchAdaptor struct {
	mu           sync.Mutex
	taskIDs      []string
	fetched      chan string
	blockTaskID  string
	blockStarted chan struct{}
	releaseBlock chan struct{}
	blockOnce    sync.Once
}

type contextTaskPollingAdaptor struct {
	contextCalled chan struct{}
	legacyCalled  chan struct{}
	seenKey       chan string
}

func (a *contextTaskPollingAdaptor) Init(_ *relaycommon.RelayInfo) {}

func (a *contextTaskPollingAdaptor) FetchTask(
	_ string,
	_ string,
	_ map[string]any,
	_ string,
) (*http.Response, error) {
	select {
	case a.legacyCalled <- struct{}{}:
	default:
	}
	return nil, errors.New("legacy FetchTask must not be used")
}

func (a *contextTaskPollingAdaptor) FetchTaskWithContext(
	ctx context.Context,
	_ string,
	key string,
	_ map[string]any,
	_ string,
) (*http.Response, error) {
	select {
	case a.seenKey <- key:
	default:
	}
	close(a.contextCalled)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (a *contextTaskPollingAdaptor) ParseTaskResult([]byte) (*relaycommon.TaskInfo, error) {
	return nil, nil
}

func (a *contextTaskPollingAdaptor) AdjustBillingOnComplete(
	_ *model.Task,
	_ *relaycommon.TaskInfo,
) int {
	return 0
}

type legacyTaskPollingAdaptor struct {
	mu            sync.Mutex
	calls         int
	seenKey       string
	seenAction    string
	requireAction bool
}

func (a *legacyTaskPollingAdaptor) Init(_ *relaycommon.RelayInfo) {}

func (a *legacyTaskPollingAdaptor) FetchTask(
	_ string,
	key string,
	body map[string]any,
	_ string,
) (*http.Response, error) {
	a.mu.Lock()
	a.calls++
	a.seenKey = key
	a.seenAction, _ = body["action"].(string)
	missingAction := a.requireAction && a.seenAction == ""
	a.mu.Unlock()
	if missingAction {
		return nil, errors.New("invalid action")
	}
	taskID, _ := body["task_id"].(string)
	encoded, err := common.Marshal(dto.TaskResponse[model.Task]{
		Code: dto.TaskSuccessCode,
		Data: model.Task{
			TaskID:   taskID,
			Status:   model.TaskStatusInProgress,
			Progress: "30%",
		},
	})
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(encoded)),
	}, nil
}

func (a *legacyTaskPollingAdaptor) ParseTaskResult([]byte) (*relaycommon.TaskInfo, error) {
	return nil, errors.New("unexpected parser fallback")
}

func (a *legacyTaskPollingAdaptor) AdjustBillingOnComplete(
	_ *model.Task,
	_ *relaycommon.TaskInfo,
) int {
	return 0
}

func (a *legacyTaskPollingAdaptor) snapshot() (int, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls, a.seenKey
}

func (a *legacyTaskPollingAdaptor) actionSnapshot() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seenAction
}

type finalSuccessPollingAdaptor struct {
	adjustReturn int
	adjustCalls  atomic.Int32
	fetchCalls   atomic.Int32
}

func (a *finalSuccessPollingAdaptor) Init(_ *relaycommon.RelayInfo) {}

func (a *finalSuccessPollingAdaptor) FetchTask(
	_ string,
	_ string,
	_ map[string]any,
	_ string,
) (*http.Response, error) {
	a.fetchCalls.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			`{"requestId":"REQUEST_ID","status":"completed","success":true}`,
		)),
	}, nil
}

func (a *finalSuccessPollingAdaptor) ParseTaskResult(
	_ []byte,
) (*relaycommon.TaskInfo, error) {
	return &relaycommon.TaskInfo{
		Status:   model.TaskStatusSuccess,
		Progress: taskcommon.ProgressComplete,
	}, nil
}

func (a *finalSuccessPollingAdaptor) AdjustBillingOnComplete(
	_ *model.Task,
	_ *relaycommon.TaskInfo,
) int {
	a.adjustCalls.Add(1)
	return a.adjustReturn
}

type seedDanceStatePollingAdaptor struct {
	status     model.TaskStatus
	progress   string
	reason     string
	fetchCalls atomic.Int32
}

type barrierFinalSuccessAdaptor struct {
	adjustCalls atomic.Int32
	entered     chan struct{}
	release     chan struct{}
}

func (a *barrierFinalSuccessAdaptor) Init(_ *relaycommon.RelayInfo) {}

func (a *barrierFinalSuccessAdaptor) FetchTask(
	_ string,
	_ string,
	_ map[string]any,
	_ string,
) (*http.Response, error) {
	return nil, errors.New("final-success recovery must not fetch provider status")
}

func (a *barrierFinalSuccessAdaptor) ParseTaskResult(
	_ []byte,
) (*relaycommon.TaskInfo, error) {
	return nil, errors.New("final-success recovery must not parse provider status")
}

func (a *barrierFinalSuccessAdaptor) AdjustBillingOnComplete(
	_ *model.Task,
	_ *relaycommon.TaskInfo,
) int {
	a.adjustCalls.Add(1)
	a.entered <- struct{}{}
	<-a.release
	return 0
}

func (a *seedDanceStatePollingAdaptor) Init(_ *relaycommon.RelayInfo) {}

func (a *seedDanceStatePollingAdaptor) FetchTask(
	_ string,
	_ string,
	_ map[string]any,
	_ string,
) (*http.Response, error) {
	a.fetchCalls.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			`{"requestId":"REQUEST_ID","status":"sanitized"}`,
		)),
	}, nil
}

func (a *seedDanceStatePollingAdaptor) ParseTaskResult(
	_ []byte,
) (*relaycommon.TaskInfo, error) {
	return &relaycommon.TaskInfo{
		Status:   string(a.status),
		Progress: a.progress,
		Reason:   a.reason,
	}, nil
}

func (a *seedDanceStatePollingAdaptor) AdjustBillingOnComplete(
	_ *model.Task,
	_ *relaycommon.TaskInfo,
) int {
	return 0
}

type sunoFailurePollingAdaptor struct {
	failReason string
	data       json.RawMessage
}

func (a *sunoFailurePollingAdaptor) Init(_ *relaycommon.RelayInfo) {}

func (a *sunoFailurePollingAdaptor) FetchTask(_ string, _ string, body map[string]any, _ string) (*http.Response, error) {
	taskIDs, _ := body["ids"].([]string)
	items := make([]dto.SunoDataResponse, 0, len(taskIDs))
	for _, taskID := range taskIDs {
		items = append(items, dto.SunoDataResponse{
			TaskID:     taskID,
			Status:     string(model.TaskStatusFailure),
			FailReason: a.failReason,
			FinishTime: time.Now().Unix(),
			Data:       a.data,
		})
	}

	responseBody, err := common.Marshal(dto.TaskResponse[[]dto.SunoDataResponse]{
		Code: dto.TaskSuccessCode,
		Data: items,
	})
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(responseBody)),
	}, nil
}

func (a *sunoFailurePollingAdaptor) ParseTaskResult([]byte) (*relaycommon.TaskInfo, error) {
	return nil, nil
}

func (a *sunoFailurePollingAdaptor) AdjustBillingOnComplete(_ *model.Task, _ *relaycommon.TaskInfo) int {
	return 0
}

type videoFailurePollingAdaptor struct {
	reason string
}

func (a *videoFailurePollingAdaptor) Init(_ *relaycommon.RelayInfo) {}

func (a *videoFailurePollingAdaptor) FetchTask(_ string, _ string, body map[string]any, _ string) (*http.Response, error) {
	taskID, _ := body["task_id"].(string)
	responseBody, err := common.Marshal(dto.TaskResponse[model.Task]{
		Code: dto.TaskSuccessCode,
		Data: model.Task{
			TaskID:     taskID,
			Status:     model.TaskStatus(model.TaskStatusFailure),
			Progress:   "100%",
			FailReason: a.reason,
			Data:       json.RawMessage(`{"provider":"sanitized"}`),
		},
	})
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(responseBody)),
	}, nil
}

func (a *videoFailurePollingAdaptor) ParseTaskResult([]byte) (*relaycommon.TaskInfo, error) {
	return nil, errors.New("unexpected parse fallback")
}

func (a *videoFailurePollingAdaptor) AdjustBillingOnComplete(_ *model.Task, _ *relaycommon.TaskInfo) int {
	return 0
}

func (a *taskPollingFetchAdaptor) Init(_ *relaycommon.RelayInfo) {}

func (a *taskPollingFetchAdaptor) FetchTask(_ string, _ string, body map[string]any, _ string) (*http.Response, error) {
	taskID, _ := body["task_id"].(string)
	if taskID == a.blockTaskID && a.releaseBlock != nil {
		a.blockOnce.Do(func() {
			if a.blockStarted != nil {
				close(a.blockStarted)
			}
		})
		<-a.releaseBlock
	}

	a.mu.Lock()
	a.taskIDs = append(a.taskIDs, taskID)
	a.mu.Unlock()
	if a.fetched != nil {
		select {
		case a.fetched <- taskID:
		default:
		}
	}

	response := dto.TaskResponse[model.Task]{
		Code: dto.TaskSuccessCode,
		Data: model.Task{
			TaskID:   taskID,
			Status:   model.TaskStatusInProgress,
			Progress: "30%",
		},
	}
	responseBody, err := common.Marshal(response)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(responseBody)),
	}, nil
}

func (a *taskPollingFetchAdaptor) ParseTaskResult([]byte) (*relaycommon.TaskInfo, error) {
	return &relaycommon.TaskInfo{Status: model.TaskStatusInProgress}, nil
}

func (a *taskPollingFetchAdaptor) AdjustBillingOnComplete(_ *model.Task, _ *relaycommon.TaskInfo) int {
	return 0
}

func (a *taskPollingFetchAdaptor) fetchCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.taskIDs)
}

func (a *taskPollingFetchAdaptor) fetchedTaskIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.taskIDs...)
}

func seedTaskPollingChannel(t *testing.T, id int, disableSleep bool) {
	t.Helper()
	ch := &model.Channel{
		Id:     id,
		Type:   constant.ChannelTypeKling,
		Name:   "polling_channel",
		Key:    "sk-test",
		Status: common.ChannelStatusEnabled,
	}
	if disableSleep {
		ch.SetOtherSettings(dto.ChannelOtherSettings{DisableTaskPollingSleep: true})
	}
	require.NoError(t, model.DB.Create(ch).Error)
}

func seedPollingTask(t *testing.T, channelID int, publicID string, upstreamID string) *model.Task {
	t.Helper()
	task := &model.Task{
		TaskID:    publicID,
		Platform:  constant.TaskPlatform("kling"),
		UserId:    1,
		ChannelId: channelID,
		Action:    constant.TaskActionGenerate,
		Status:    model.TaskStatusInProgress,
		Progress:  "30%",
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		PrivateData: model.TaskPrivateData{
			Key:            "sk-test",
			UpstreamTaskID: upstreamID,
		},
	}
	require.NoError(t, model.DB.Create(task).Error)
	return task
}

func TestTaskPollingContextReachesContextAwareFetcherWithStoredKey(t *testing.T) {
	truncate(t)

	const channelID = 91
	seedTaskPollingChannel(t, channelID, true)
	task := seedPollingTask(t, channelID, "task_context", "upstream_context")
	adaptor := &contextTaskPollingAdaptor{
		contextCalled: make(chan struct{}),
		legacyCalled:  make(chan struct{}, 1),
		seenKey:       make(chan string, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- updateVideoSingleTask(
			ctx,
			adaptor,
			&model.Channel{
				Id:   channelID,
				Type: constant.ChannelTypeKling,
				Key:  "sk-test",
			},
			task.GetUpstreamTaskID(),
			map[string]*model.Task{task.GetUpstreamTaskID(): task},
		)
	}()

	<-adaptor.contextCalled
	cancel()
	err := <-errCh
	require.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, err.Error(), "upstream_context")
	assert.NotContains(t, err.Error(), "sk-test")
	assert.Equal(t, "sk-test", <-adaptor.seenKey)
	select {
	case <-adaptor.legacyCalled:
		t.Fatal("context-aware adaptor fell back to legacy FetchTask")
	default:
	}
}

func TestTaskPollingContextLegacyAdaptorUsesFetchTaskWithStoredKey(t *testing.T) {
	truncate(t)

	task := &model.Task{
		TaskID:    "task_legacy_fetch",
		ChannelId: 92,
		Action:    constant.TaskActionGenerate,
		Status:    model.TaskStatusInProgress,
		Progress:  "30%",
		PrivateData: model.TaskPrivateData{
			Key:            "STORED_KEY",
			UpstreamTaskID: "upstream_legacy_fetch",
		},
	}
	adaptor := &legacyTaskPollingAdaptor{}
	err := updateVideoSingleTask(
		context.Background(),
		adaptor,
		&model.Channel{
			Id:   92,
			Type: constant.ChannelTypeKling,
			Key:  "STORED_KEY",
		},
		task.GetUpstreamTaskID(),
		map[string]*model.Task{task.GetUpstreamTaskID(): task},
	)
	require.NoError(t, err)
	calls, seenKey := adaptor.snapshot()
	assert.Equal(t, 1, calls)
	assert.Equal(t, "STORED_KEY", seenKey)
}

func TestTaskPollingLegacyKlingRequestPreservesAction(t *testing.T) {
	task := &model.Task{
		TaskID:    "task_kling_action",
		ChannelId: 97,
		Action:    constant.TaskActionGenerate,
		Status:    model.TaskStatusInProgress,
		Progress:  "30%",
		PrivateData: model.TaskPrivateData{
			Key:            "STORED_KEY",
			UpstreamTaskID: "upstream_kling_action",
		},
	}
	adaptor := &legacyTaskPollingAdaptor{requireAction: true}
	err := updateVideoSingleTask(
		context.Background(),
		adaptor,
		&model.Channel{
			Id:   97,
			Type: constant.ChannelTypeKling,
			Key:  "STORED_KEY",
		},
		task.GetUpstreamTaskID(),
		map[string]*model.Task{task.GetUpstreamTaskID(): task},
	)
	require.NoError(t, err)
	assert.Equal(t, constant.TaskActionGenerate, adaptor.actionSnapshot())
}

func TestTaskPollingContextNeverFallsBackFromDisabledStoredKey(t *testing.T) {
	task := &model.Task{
		TaskID:    "task_disabled_stored_key",
		ChannelId: 93,
		Status:    model.TaskStatusInProgress,
		Progress:  "30%",
		PrivateData: model.TaskPrivateData{
			Key:            "STORED_KEY",
			UpstreamTaskID: "upstream_disabled_stored_key",
		},
	}
	channel := &model.Channel{
		Id:   93,
		Type: constant.ChannelTypeKling,
		Key:  "STORED_KEY\nCURRENT_RANDOM_KEY",
		Keys: []string{"STORED_KEY", "CURRENT_RANDOM_KEY"},
		ChannelInfo: model.ChannelInfo{
			IsMultiKey: true,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusManuallyDisabled,
				1: common.ChannelStatusEnabled,
			},
		},
	}
	adaptor := &legacyTaskPollingAdaptor{}
	err := updateVideoSingleTask(
		context.Background(),
		adaptor,
		channel,
		task.GetUpstreamTaskID(),
		map[string]*model.Task{task.GetUpstreamTaskID(): task},
	)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "STORED_KEY")
	assert.NotContains(t, err.Error(), "CURRENT_RANDOM_KEY")
	calls, _ := adaptor.snapshot()
	assert.Zero(t, calls)
}

func TestTaskPollingContextStoredKeyErrorsAndLogsAreCredentialFree(t *testing.T) {
	truncate(t)

	channel := &model.Channel{
		Id:     95,
		Type:   constant.ChannelTypeKling,
		Name:   "stored-key-log-channel",
		Key:    "STORED_KEY\nCURRENT_RANDOM_KEY",
		Status: common.ChannelStatusEnabled,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey: true,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusManuallyDisabled,
				1: common.ChannelStatusEnabled,
			},
		},
	}
	channel.SetOtherSettings(dto.ChannelOtherSettings{DisableTaskPollingSleep: true})
	require.NoError(t, model.DB.Create(channel).Error)
	task := &model.Task{
		TaskID:    "task_stored_key_log",
		Platform:  constant.TaskPlatform("kling"),
		ChannelId: channel.Id,
		Status:    model.TaskStatusInProgress,
		Progress:  "30%",
		PrivateData: model.TaskPrivateData{
			Key:            "STORED_KEY",
			UpstreamTaskID: "upstream_stored_key_log",
		},
	}
	require.NoError(t, model.DB.Create(task).Error)
	adaptor := &legacyTaskPollingAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor {
		return adaptor
	}
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	var logs bytes.Buffer
	common.LogWriterMu.Lock()
	previousErrorWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logs
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = previousErrorWriter
		common.LogWriterMu.Unlock()
	})

	require.NoError(t, UpdateVideoTasks(
		context.Background(),
		task.Platform,
		map[int][]string{channel.Id: {task.GetUpstreamTaskID()}},
		map[string]*model.Task{task.GetUpstreamTaskID(): task},
	))
	assert.Contains(t, logs.String(), "stored task credential is disabled or removed")
	assert.NotContains(t, logs.String(), "STORED_KEY")
	assert.NotContains(t, logs.String(), "CURRENT_RANDOM_KEY")
	assert.NotContains(t, logs.String(), "upstream_stored_key_log")
	calls, _ := adaptor.snapshot()
	assert.Zero(t, calls)
}

func TestSubmittingTaskPollingSkipsFetch(t *testing.T) {
	task := &model.Task{
		TaskID:    "task_submitting",
		ChannelId: 94,
		Status:    model.TaskStatusSubmitting,
		Progress:  "0%",
		PrivateData: model.TaskPrivateData{
			Key: "STORED_KEY",
		},
	}
	adaptor := &legacyTaskPollingAdaptor{}
	err := updateVideoSingleTask(
		context.Background(),
		adaptor,
		&model.Channel{
			Id:   94,
			Type: constant.ChannelTypeKling,
			Key:  "STORED_KEY",
		},
		task.TaskID,
		map[string]*model.Task{task.TaskID: task},
	)
	require.NoError(t, err)
	calls, _ := adaptor.snapshot()
	assert.Zero(t, calls)
}

func TestSubmittingTaskPollingSkipsNullTaskFailurePath(t *testing.T) {
	truncate(t)
	task := &model.Task{
		TaskID:     "task_submitting_without_upstream",
		Platform:   constant.TaskPlatform("kling"),
		ChannelId:  96,
		Status:     model.TaskStatusSubmitting,
		Progress:   "0%",
		SubmitTime: time.Now().Unix(),
		PrivateData: model.TaskPrivateData{
			Key: "STORED_KEY",
		},
	}
	require.NoError(t, model.DB.Create(task).Error)
	adaptor := &legacyTaskPollingAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor {
		return adaptor
	}
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	summary := RunTaskPollingOnce(context.Background(), nil)
	assert.Zero(t, summary.NullTasksFailed)
	calls, _ := adaptor.snapshot()
	assert.Zero(t, calls)

	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSubmitting), reloaded.Status)
	assert.Equal(t, "0%", reloaded.Progress)
}

func seedSettledDurablePollingTask(
	t *testing.T,
	requestID string,
	userID int,
	tokenID int,
) *model.Task {
	t.Helper()
	attempt := seedDurableBillingAttempt(t, requestID, userID, tokenID, 100)
	task := linkDurableBillingAttempt(t, attempt, requestID)
	require.NoError(t, model.DB.Model(&model.Task{}).
		Where("id = ?", task.ID).
		Update("platform", seedDanceTaskPlatform).Error)
	require.NoError(t, model.DB.First(task, task.ID).Error)
	_, err := model.AttachTaskUpstreamResult(
		task.ID,
		task.TaskID,
		"UPSTREAM_"+requestID,
		json.RawMessage(`{"status":"accepted"}`),
	)
	require.NoError(t, err)
	task, err = model.CommitTaskSubmission(task.ID, task.TaskID)
	require.NoError(t, err)
	require.NoError(t, model.MarkTaskBillingAttemptSubmissionSettled(requestID))
	task.PrivateData.Key = "STORED_KEY"
	require.NoError(t, model.DB.Model(&model.Task{}).
		Where("id = ?", task.ID).
		Update("private_data", task.PrivateData).Error)
	require.NoError(t, model.DB.First(task, task.ID).Error)
	return task
}

func TestTaskPollingSuccessTransitionSettlesBeforeSucceededMarker(t *testing.T) {
	truncate(t)
	task := seedSettledDurablePollingTask(
		t,
		"poll-success-order",
		621,
		721,
	)
	previousSettlement := settleTaskBillingOnCompleteForPolling
	settlementCalled := false
	settleTaskBillingOnCompleteForPolling = func(
		_ context.Context,
		_ TaskPollingAdaptor,
		settledTask *model.Task,
		_ *relaycommon.TaskInfo,
	) error {
		settlementCalled = true
		var persisted model.Task
		require.NoError(t, model.DB.First(&persisted, settledTask.ID).Error)
		assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), persisted.Status)
		assert.Equal(t, taskcommon.ProgressComplete, persisted.Progress)
		attempt, err := model.GetTaskBillingAttemptByTaskID(settledTask.ID)
		require.NoError(t, err)
		assert.Zero(t, attempt.SucceededAt)
		return nil
	}
	t.Cleanup(func() {
		settleTaskBillingOnCompleteForPolling = previousSettlement
	})

	err := updateVideoSingleTask(
		context.Background(),
		&finalSuccessPollingAdaptor{},
		&model.Channel{
			Id:   task.ChannelId,
			Type: constant.ChannelTypeSeedDance,
			Key:  "STORED_KEY",
		},
		task.GetUpstreamTaskID(),
		map[string]*model.Task{task.GetUpstreamTaskID(): task},
	)
	require.NoError(t, err)
	assert.True(t, settlementCalled)

	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), reloaded.Status)
	assert.Equal(t, taskcommon.ProgressComplete, reloaded.Progress)
	assert.Contains(t, reloaded.PrivateData.ResultURL, "/v1/videos/"+task.TaskID+"/content")
	attempt, err := model.GetTaskBillingAttemptByTaskID(task.ID)
	require.NoError(t, err)
	assert.NotZero(t, attempt.SubmissionSettledAt)
	assert.NotZero(t, attempt.SucceededAt)
	assert.Zero(t, attempt.RefundStartedAt)
	assert.Zero(t, attempt.RefundCompletedAt)
}

func TestTaskPollingFinalSettlementFailureLeavesSucceededMarkerUnset(t *testing.T) {
	truncate(t)
	task := seedSettledDurablePollingTask(
		t,
		"poll-settlement-failure",
		622,
		722,
	)
	err := updateVideoSingleTask(
		context.Background(),
		&finalSuccessPollingAdaptor{adjustReturn: 200},
		&model.Channel{
			Id:   task.ChannelId,
			Type: constant.ChannelTypeSeedDance,
			Key:  "STORED_KEY",
		},
		task.GetUpstreamTaskID(),
		map[string]*model.Task{task.GetUpstreamTaskID(): task},
	)
	require.ErrorContains(t, err, "final settlement")

	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), reloaded.Status)
	assert.Equal(t, taskcommon.ProgressComplete, reloaded.Progress)
	attempt, err := model.GetTaskBillingAttemptByTaskID(task.ID)
	require.NoError(t, err)
	assert.Zero(t, attempt.SucceededAt)
	assert.Zero(t, attempt.RefundStartedAt)
	assert.Zero(t, attempt.RefundCompletedAt)
}

func TestTaskPollingMarkerFailureIsRecoveredWithoutProviderRefetch(t *testing.T) {
	truncate(t)
	task := seedSettledDurablePollingTask(
		t,
		"poll-marker-failure-recovery",
		624,
		724,
	)
	const triggerName = "test_fail_task_success_marker"
	require.NoError(t, model.DB.Exec(
		"CREATE TRIGGER "+triggerName+" "+
			"BEFORE UPDATE OF succeeded_at ON task_billing_attempts "+
			"WHEN NEW.succeeded_at <> 0 "+
			"BEGIN SELECT RAISE(FAIL, 'marker unavailable'); END",
	).Error)
	t.Cleanup(func() {
		_ = model.DB.Exec("DROP TRIGGER IF EXISTS " + triggerName).Error
	})

	initialAdaptor := &finalSuccessPollingAdaptor{}
	err := updateVideoSingleTask(
		context.Background(),
		initialAdaptor,
		&model.Channel{
			Id:   task.ChannelId,
			Type: constant.ChannelTypeSeedDance,
			Key:  "STORED_KEY",
		},
		task.GetUpstreamTaskID(),
		map[string]*model.Task{task.GetUpstreamTaskID(): task},
	)
	require.ErrorContains(t, err, "mark durable task success")
	assert.Equal(t, int32(1), initialAdaptor.fetchCalls.Load())
	assert.Equal(t, int32(1), initialAdaptor.adjustCalls.Load())

	var afterFailure model.Task
	require.NoError(t, model.DB.First(&afterFailure, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), afterFailure.Status)
	assert.Equal(t, taskcommon.ProgressComplete, afterFailure.Progress)
	assert.Equal(t, task.Quota, afterFailure.Quota)
	attempt, err := model.GetTaskBillingAttemptByTaskID(task.ID)
	require.NoError(t, err)
	assert.Zero(t, attempt.SucceededAt)
	assert.True(t, model.HasTaskPollingWork())

	require.NoError(t, model.DB.Exec("DROP TRIGGER "+triggerName).Error)
	recoveryAdaptor := &finalSuccessPollingAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(platform constant.TaskPlatform) TaskPollingAdaptor {
		assert.Equal(t, seedDanceTaskPlatform, platform)
		return recoveryAdaptor
	}
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	RunTaskPollingOnce(context.Background(), nil)
	attempt, err = model.GetTaskBillingAttemptByTaskID(task.ID)
	require.NoError(t, err)
	firstSucceededAt := attempt.SucceededAt
	assert.NotZero(t, firstSucceededAt)
	assert.Zero(t, recoveryAdaptor.fetchCalls.Load())
	assert.Equal(t, int32(1), recoveryAdaptor.adjustCalls.Load())
	assert.False(t, model.HasTaskPollingWork())

	RunTaskPollingOnce(context.Background(), nil)
	attempt, err = model.GetTaskBillingAttemptByTaskID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, firstSucceededAt, attempt.SucceededAt)
	assert.Zero(t, recoveryAdaptor.fetchCalls.Load())
	assert.Equal(t, int32(1), recoveryAdaptor.adjustCalls.Load())
}

func TestTaskPollingSettledSubmissionFailureRefundsBothComponents(t *testing.T) {
	truncate(t)
	task := seedSettledDurablePollingTask(
		t,
		"poll-settled-provider-failure",
		623,
		723,
	)

	err := updateVideoSingleTask(
		context.Background(),
		&videoFailurePollingAdaptor{reason: "safe provider failure"},
		&model.Channel{
			Id:   task.ChannelId,
			Type: constant.ChannelTypeSeedDance,
			Key:  "STORED_KEY",
		},
		task.GetUpstreamTaskID(),
		map[string]*model.Task{task.GetUpstreamTaskID(): task},
	)
	require.NoError(t, err)

	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), reloaded.Status)
	assert.Equal(t, taskcommon.ProgressComplete, reloaded.Progress)
	assert.Zero(t, reloaded.Quota)
	attempt, err := model.GetTaskBillingAttemptByTaskID(task.ID)
	require.NoError(t, err)
	assert.NotZero(t, attempt.SubmissionSettledAt)
	assert.Zero(t, attempt.SucceededAt)
	assert.NotZero(t, attempt.FundingRefundedAt)
	assert.NotZero(t, attempt.TokenRefundedAt)
	assert.NotZero(t, attempt.RefundStartedAt)
	assert.NotZero(t, attempt.RefundCompletedAt)
}

func seedRecoverableFinalSuccessTask(
	t *testing.T,
	requestID string,
	userID int,
	tokenID int,
	channelID int,
) *model.Task {
	t.Helper()
	task := seedSettledDurablePollingTask(t, requestID, userID, tokenID)
	channel := &model.Channel{
		Id:     channelID,
		Type:   constant.ChannelTypeSeedDance,
		Name:   "seed-dance-recovery",
		Key:    "STORED_KEY",
		Status: common.ChannelStatusEnabled,
	}
	channel.SetOtherSettings(dto.ChannelOtherSettings{DisableTaskPollingSleep: true})
	require.NoError(t, model.DB.Create(channel).Error)
	require.NoError(t, model.DB.Model(&model.Task{}).
		Where("id = ?", task.ID).
		Updates(map[string]any{
			"channel_id": channelID,
			"platform": constant.TaskPlatform(
				fmt.Sprintf("%d", constant.ChannelTypeSeedDance),
			),
		}).Error)
	require.NoError(t, model.DB.First(task, task.ID).Error)
	data := json.RawMessage(`{"status":"completed"}`)
	transitioned, _, err := model.TransitionTaskToSuccess(
		task.ID,
		task.TaskID,
		model.TaskSuccessTransition{
			ExpectedStatus:   model.TaskStatusSubmitted,
			ExpectedProgress: taskcommon.ProgressSubmitted,
			ResultURL:        "/v1/videos/" + task.TaskID + "/content",
			Data:             &data,
		},
	)
	require.NoError(t, err)
	recoverable, err := model.ListRecoverableFinalSuccessTasks(10)
	require.NoError(t, err)
	if len(recoverable) != 1 {
		attempt, attemptErr := model.GetTaskBillingAttemptByTaskID(task.ID)
		t.Logf(
			"recoverable fixture task=%+v attempt=%+v attemptErr=%v",
			transitioned,
			attempt,
			attemptErr,
		)
	}
	require.Len(t, recoverable, 1)
	return transitioned
}

func TestTaskPollingFinalSuccessRecoveryReplaysCrashAfterTransition(t *testing.T) {
	truncate(t)
	task := seedRecoverableFinalSuccessTask(
		t,
		"poll-success-crash-recovery",
		631,
		731,
		5_901,
	)
	assert.True(t, model.HasTaskPollingWork())
	adaptor := &finalSuccessPollingAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(platform constant.TaskPlatform) TaskPollingAdaptor {
		assert.Equal(
			t,
			constant.TaskPlatform(fmt.Sprintf("%d", constant.ChannelTypeSeedDance)),
			platform,
		)
		return adaptor
	}
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	RunTaskPollingOnce(context.Background(), nil)

	attempt, err := model.GetTaskBillingAttemptByTaskID(task.ID)
	require.NoError(t, err)
	assert.NotZero(t, attempt.SucceededAt)
	assert.Zero(t, attempt.RefundStartedAt)
	assert.Zero(t, adaptor.fetchCalls.Load())
	assert.Equal(t, int32(1), adaptor.adjustCalls.Load())
	assert.False(t, model.HasTaskPollingWork())
}

func TestTaskPollingFinalSuccessRecoveryRetriesSettlementFailure(t *testing.T) {
	truncate(t)
	task := seedRecoverableFinalSuccessTask(
		t,
		"poll-success-settlement-retry",
		632,
		732,
		5_902,
	)
	failingAdaptor := &finalSuccessPollingAdaptor{adjustReturn: 200}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor {
		return failingAdaptor
	}
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	err := replayDurableFinalSuccessTasks(context.Background())
	require.ErrorContains(t, err, "final settlement")
	attempt, err := model.GetTaskBillingAttemptByTaskID(task.ID)
	require.NoError(t, err)
	assert.Zero(t, attempt.SucceededAt)
	assert.True(t, model.HasTaskPollingWork())

	successAdaptor := &finalSuccessPollingAdaptor{}
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor {
		return successAdaptor
	}
	require.NoError(t, replayDurableFinalSuccessTasks(context.Background()))
	attempt, err = model.GetTaskBillingAttemptByTaskID(task.ID)
	require.NoError(t, err)
	assert.NotZero(t, attempt.SucceededAt)
	assert.Equal(t, int32(1), successAdaptor.adjustCalls.Load())
	require.NoError(t, replayDurableFinalSuccessTasks(context.Background()))
	assert.Equal(t, int32(1), successAdaptor.adjustCalls.Load())
}

func TestTaskPollingFinalSuccessRecoveryIsIdempotentAcrossTwoWorkers(t *testing.T) {
	truncate(t)
	task := seedRecoverableFinalSuccessTask(
		t,
		"poll-success-two-workers",
		634,
		734,
		5_904,
	)
	adaptor := &barrierFinalSuccessAdaptor{
		entered: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor {
		return adaptor
	}
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	errCh := make(chan error, 2)
	for worker := 0; worker < 2; worker++ {
		go func() {
			errCh <- replayDurableFinalSuccessTasks(context.Background())
		}()
	}
	for worker := 0; worker < 2; worker++ {
		select {
		case <-adaptor.entered:
		case <-time.After(time.Second):
			t.Fatal("both recovery workers did not reach zero-delta settlement")
		}
	}
	close(adaptor.release)
	require.NoError(t, <-errCh)
	require.NoError(t, <-errCh)

	attempt, err := model.GetTaskBillingAttemptByTaskID(task.ID)
	require.NoError(t, err)
	assert.NotZero(t, attempt.SucceededAt)
	assert.Equal(t, int32(2), adaptor.adjustCalls.Load())
	require.NoError(t, replayDurableFinalSuccessTasks(context.Background()))
	assert.Equal(t, int32(2), adaptor.adjustCalls.Load())
}

func TestSeedDancePollingUsesNarrowNonterminalTransition(t *testing.T) {
	truncate(t)
	task := seedSettledDurablePollingTask(
		t,
		"poll-nonterminal-production",
		633,
		733,
	)
	channel := &model.Channel{
		Id:     5_903,
		Type:   constant.ChannelTypeSeedDance,
		Name:   "seed-dance-nonterminal",
		Key:    "STORED_KEY",
		Status: common.ChannelStatusEnabled,
	}
	require.NoError(t, model.DB.Create(channel).Error)
	require.NoError(t, model.DB.Model(&model.Task{}).
		Where("id = ?", task.ID).
		Updates(map[string]any{
			"channel_id": channel.Id,
			"platform": constant.TaskPlatform(
				fmt.Sprintf("%d", constant.ChannelTypeSeedDance),
			),
		}).Error)
	require.NoError(t, model.DB.First(task, task.ID).Error)
	stale := *task
	latestPrivate := task.PrivateData
	latestPrivate.NodeName = "latest-concurrent-node"
	latestProperties := model.Properties{
		Input:             "latest-concurrent-input",
		UpstreamModelName: "latest-concurrent-route",
		OriginModelName:   task.Properties.OriginModelName,
	}
	require.NoError(t, model.DB.Model(&model.Task{}).Where("id = ?", task.ID).
		Updates(map[string]any{
			"private_data": latestPrivate,
			"properties":   latestProperties,
		}).Error)
	adaptor := &seedDanceStatePollingAdaptor{
		status:   model.TaskStatusInProgress,
		progress: taskcommon.ProgressInProgress,
	}

	require.NoError(t, updateVideoSingleTask(
		context.Background(),
		adaptor,
		channel,
		stale.GetUpstreamTaskID(),
		map[string]*model.Task{stale.GetUpstreamTaskID(): &stale},
	))

	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusInProgress), reloaded.Status)
	assert.Equal(t, taskcommon.ProgressInProgress, reloaded.Progress)
	assert.Equal(t, "latest-concurrent-node", reloaded.PrivateData.NodeName)
	assert.Equal(t, latestProperties, reloaded.Properties)
	assert.Equal(t, task.Quota, reloaded.Quota)
}

func TestSeedDancePollingMissingLedgerFailsClosedBeforeFetch(t *testing.T) {
	tests := []struct {
		name     string
		status   model.TaskStatus
		progress string
	}{
		{
			name:     "nonterminal provider result",
			status:   model.TaskStatusInProgress,
			progress: taskcommon.ProgressInProgress,
		},
		{
			name:     "success provider result",
			status:   model.TaskStatusSuccess,
			progress: taskcommon.ProgressComplete,
		},
		{
			name:     "failure provider result",
			status:   model.TaskStatusFailure,
			progress: taskcommon.ProgressComplete,
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			truncate(t)
			channelID := 5_910 + index
			channel := &model.Channel{
				Id:     channelID,
				Type:   constant.ChannelTypeSeedDance,
				Name:   "seed-dance-missing-ledger",
				Key:    "STORED_KEY",
				Status: common.ChannelStatusEnabled,
			}
			require.NoError(t, model.DB.Create(channel).Error)
			task := &model.Task{
				TaskID:    fmt.Sprintf("task_missing_ledger_%d", index),
				Platform:  constant.TaskPlatform("59"),
				UserId:    1,
				ChannelId: channelID,
				Quota:     100,
				Action:    constant.TaskActionGenerate,
				Status:    model.TaskStatusSubmitted,
				Progress:  taskcommon.ProgressSubmitted,
				PrivateData: model.TaskPrivateData{
					Key:            "STORED_KEY",
					UpstreamTaskID: fmt.Sprintf("UPSTREAM_MISSING_LEDGER_%d", index),
				},
			}
			require.NoError(t, model.DB.Create(task).Error)
			adaptor := &seedDanceStatePollingAdaptor{
				status:   test.status,
				progress: test.progress,
				reason:   "safe failure",
			}

			err := updateVideoSingleTask(
				context.Background(),
				adaptor,
				channel,
				task.GetUpstreamTaskID(),
				map[string]*model.Task{task.GetUpstreamTaskID(): task},
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "durable billing attempt")
			assert.NotContains(t, err.Error(), task.GetUpstreamTaskID())
			assert.NotContains(t, err.Error(), "STORED_KEY")
			assert.Zero(t, adaptor.fetchCalls.Load())

			var reloaded model.Task
			require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
			assert.Equal(t, model.TaskStatus(model.TaskStatusSubmitted), reloaded.Status)
			assert.Equal(t, taskcommon.ProgressSubmitted, reloaded.Progress)
			assert.Equal(t, 100, reloaded.Quota)
		})
	}
}

func TestSeedDanceMissingDurableUpstreamIDFailsClosedBeforeFetch(t *testing.T) {
	truncate(t)
	previousQueryLimit := constant.TaskQueryLimit
	constant.TaskQueryLimit = 100
	t.Cleanup(func() { constant.TaskQueryLimit = previousQueryLimit })

	task := seedSettledDurablePollingTask(
		t,
		"poll-missing-durable-upstream-id",
		637,
		737,
	)
	channel := &model.Channel{
		Id:     5_930,
		Type:   constant.ChannelTypeSeedDance,
		Name:   "seed-dance-missing-upstream-id",
		Key:    "STORED_KEY",
		Status: common.ChannelStatusEnabled,
	}
	require.NoError(t, model.DB.Create(channel).Error)
	require.NoError(t, model.DB.Model(&model.Task{}).
		Where("id = ?", task.ID).
		Updates(map[string]any{
			"channel_id": channel.Id,
			"platform":   seedDanceTaskPlatform,
		}).Error)
	require.NoError(t, model.DB.First(task, task.ID).Error)
	task.PrivateData.UpstreamTaskID = ""
	require.NoError(t, model.DB.Model(&model.Task{}).
		Where("id = ?", task.ID).
		Update("private_data", task.PrivateData).Error)
	require.NoError(t, model.DB.First(task, task.ID).Error)

	beforeAttempt, err := model.GetTaskBillingAttemptByTaskID(task.ID)
	require.NoError(t, err)
	var beforeUser model.User
	require.NoError(t, model.DB.First(&beforeUser, beforeAttempt.UserID).Error)
	var beforeToken model.Token
	require.NoError(t, model.DB.First(&beforeToken, beforeAttempt.TokenID).Error)

	adaptor := &seedDanceStatePollingAdaptor{
		status:   model.TaskStatusFailure,
		progress: taskcommon.ProgressComplete,
		reason:   "provider did not recognize the public id",
	}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(platform constant.TaskPlatform) TaskPollingAdaptor {
		if platform == seedDanceTaskPlatform {
			return adaptor
		}
		return nil
	}
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	summary := RunTaskPollingOnce(context.Background(), nil)
	assert.Zero(t, summary.NullTasksFailed)
	assert.Zero(t, adaptor.fetchCalls.Load())

	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSubmitted), reloaded.Status)
	assert.Equal(t, taskcommon.ProgressSubmitted, reloaded.Progress)
	assert.Equal(t, 100, reloaded.Quota)
	assert.Empty(t, reloaded.PrivateData.UpstreamTaskID)
	assert.Zero(t, reloaded.FinishTime)
	assert.Empty(t, reloaded.FailReason)

	afterAttempt, err := model.GetTaskBillingAttemptByTaskID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, beforeAttempt.SucceededAt, afterAttempt.SucceededAt)
	assert.Equal(t, beforeAttempt.FundingRefundedAt, afterAttempt.FundingRefundedAt)
	assert.Equal(t, beforeAttempt.TokenRefundedAt, afterAttempt.TokenRefundedAt)
	assert.Equal(t, beforeAttempt.RefundStartedAt, afterAttempt.RefundStartedAt)
	assert.Equal(t, beforeAttempt.RefundCompletedAt, afterAttempt.RefundCompletedAt)

	var afterUser model.User
	require.NoError(t, model.DB.First(&afterUser, beforeAttempt.UserID).Error)
	assert.Equal(t, beforeUser.Quota, afterUser.Quota)
	var afterToken model.Token
	require.NoError(t, model.DB.First(&afterToken, beforeAttempt.TokenID).Error)
	assert.Equal(t, beforeToken.RemainQuota, afterToken.RemainQuota)
}

func TestSeedDanceMissingChannelWithoutLedgerFailsClosed(t *testing.T) {
	truncate(t)
	const missingChannelID = 5_920
	task := &model.Task{
		TaskID:     "task_seedance_missing_channel_ledger",
		Platform:   seedDanceTaskPlatform,
		UserId:     1,
		ChannelId:  missingChannelID,
		Quota:      100,
		Action:     constant.TaskActionGenerate,
		Status:     model.TaskStatusInProgress,
		Progress:   taskcommon.ProgressInProgress,
		SubmitTime: time.Now().Unix(),
		PrivateData: model.TaskPrivateData{
			Key:            "STORED_KEY",
			UpstreamTaskID: "UPSTREAM_MISSING_CHANNEL_LEDGER",
		},
	}
	require.NoError(t, model.DB.Create(task).Error)

	err := updateVideoTasks(
		context.Background(),
		seedDanceTaskPlatform,
		missingChannelID,
		[]string{task.GetUpstreamTaskID()},
		map[string]*model.Task{task.GetUpstreamTaskID(): task},
	)
	require.Error(t, err)

	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusInProgress), reloaded.Status)
	assert.Equal(t, taskcommon.ProgressInProgress, reloaded.Progress)
	assert.Equal(t, 100, reloaded.Quota)
	assert.Zero(t, reloaded.FinishTime)
	assert.Empty(t, reloaded.FailReason)
}

func TestSeedDanceTimeoutWithoutLedgerFailsClosed(t *testing.T) {
	truncate(t)
	previousTimeout := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 1
	t.Cleanup(func() {
		constant.TaskTimeoutMinutes = previousTimeout
	})

	task := &model.Task{
		TaskID:     "task_seedance_timeout_without_ledger",
		Platform:   seedDanceTaskPlatform,
		UserId:     1,
		ChannelId:  5_921,
		Quota:      100,
		Action:     constant.TaskActionGenerate,
		Status:     model.TaskStatusInProgress,
		Progress:   taskcommon.ProgressInProgress,
		SubmitTime: time.Now().Add(-2 * time.Minute).Unix(),
		PrivateData: model.TaskPrivateData{
			Key:            "STORED_KEY",
			UpstreamTaskID: "UPSTREAM_TIMEOUT_WITHOUT_LEDGER",
		},
	}
	require.NoError(t, model.DB.Create(task).Error)

	sweepTimedOutTasks(context.Background())

	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusInProgress), reloaded.Status)
	assert.Equal(t, taskcommon.ProgressInProgress, reloaded.Progress)
	assert.Equal(t, 100, reloaded.Quota)
	assert.Zero(t, reloaded.FinishTime)
	assert.Empty(t, reloaded.FailReason)
}

func TestSeedDancePlatformChannelDriftFailsClosedBeforeFetch(t *testing.T) {
	truncate(t)
	task := seedSettledDurablePollingTask(
		t,
		"poll-seedance-channel-drift",
		635,
		735,
	)
	adaptor := &seedDanceStatePollingAdaptor{
		status:   model.TaskStatusInProgress,
		progress: taskcommon.ProgressInProgress,
	}

	err := updateVideoSingleTask(
		context.Background(),
		adaptor,
		&model.Channel{
			Id:   task.ChannelId,
			Type: constant.ChannelTypeKling,
			Key:  "STORED_KEY",
		},
		task.GetUpstreamTaskID(),
		map[string]*model.Task{task.GetUpstreamTaskID(): task},
	)
	require.ErrorContains(t, err, "identity mismatch")
	assert.NotContains(t, err.Error(), task.GetUpstreamTaskID())
	assert.Zero(t, adaptor.fetchCalls.Load())

	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSubmitted), reloaded.Status)
	assert.Equal(t, taskcommon.ProgressSubmitted, reloaded.Progress)
}

func TestUpdateVideoTasksDefaultSleepWaitsBetweenTasks(t *testing.T) {
	truncate(t)

	const channelID = 101
	seedTaskPollingChannel(t, channelID, false)
	first := seedPollingTask(t, channelID, "task_public_1", "upstream_1")
	second := seedPollingTask(t, channelID, "task_public_2", "upstream_2")

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
		channelID: {
			first.GetUpstreamTaskID(),
			second.GetUpstreamTaskID(),
		},
	}, map[string]*model.Task{
		first.GetUpstreamTaskID():  first,
		second.GetUpstreamTaskID(): second,
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 1, adaptor.fetchCount())
}

func TestUpdateVideoTasksCanSkipPollingSleepPerChannel(t *testing.T) {
	truncate(t)

	const channelID = 102
	seedTaskPollingChannel(t, channelID, true)
	first := seedPollingTask(t, channelID, "task_public_3", "upstream_3")
	second := seedPollingTask(t, channelID, "task_public_4", "upstream_4")

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
		channelID: {
			first.GetUpstreamTaskID(),
			second.GetUpstreamTaskID(),
		},
	}, map[string]*model.Task{
		first.GetUpstreamTaskID():  first,
		second.GetUpstreamTaskID(): second,
	})

	require.NoError(t, err)
	assert.Equal(t, 2, adaptor.fetchCount())
}

func TestUpdateVideoTasksDefaultSleepDoesNotBlockOtherChannels(t *testing.T) {
	truncate(t)

	const firstChannelID = 201
	const secondChannelID = 202
	seedTaskPollingChannel(t, firstChannelID, false)
	seedTaskPollingChannel(t, secondChannelID, false)
	firstChannelFirst := seedPollingTask(t, firstChannelID, "task_public_5", "upstream_a_1")
	firstChannelSecond := seedPollingTask(t, firstChannelID, "task_public_6", "upstream_a_2")
	secondChannelFirst := seedPollingTask(t, secondChannelID, "task_public_7", "upstream_b_1")
	secondChannelSecond := seedPollingTask(t, secondChannelID, "task_public_8", "upstream_b_2")

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
		firstChannelID: {
			firstChannelFirst.GetUpstreamTaskID(),
			firstChannelSecond.GetUpstreamTaskID(),
		},
		secondChannelID: {
			secondChannelFirst.GetUpstreamTaskID(),
			secondChannelSecond.GetUpstreamTaskID(),
		},
	}, map[string]*model.Task{
		firstChannelFirst.GetUpstreamTaskID():   firstChannelFirst,
		firstChannelSecond.GetUpstreamTaskID():  firstChannelSecond,
		secondChannelFirst.GetUpstreamTaskID():  secondChannelFirst,
		secondChannelSecond.GetUpstreamTaskID(): secondChannelSecond,
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.ElementsMatch(t, []string{"upstream_a_1", "upstream_b_1"}, adaptor.fetchedTaskIDs())
}

func TestUpdateVideoTasksSlowChannelDoesNotBlockOtherChannels(t *testing.T) {
	truncate(t)

	const slowChannelID = 251
	const fastChannelID = 252
	seedTaskPollingChannel(t, slowChannelID, false)
	seedTaskPollingChannel(t, fastChannelID, true)
	slowTask := seedPollingTask(t, slowChannelID, "task_public_slow", "upstream_slow_1")
	fastFirst := seedPollingTask(t, fastChannelID, "task_public_fast_1", "upstream_fast_parallel_1")
	fastSecond := seedPollingTask(t, fastChannelID, "task_public_fast_2", "upstream_fast_parallel_2")

	adaptor := &taskPollingFetchAdaptor{
		fetched:      make(chan string, 4),
		blockTaskID:  slowTask.GetUpstreamTaskID(),
		blockStarted: make(chan struct{}),
		releaseBlock: make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseBlockedTask := func() {
		releaseOnce.Do(func() {
			close(adaptor.releaseBlock)
		})
	}
	t.Cleanup(releaseBlockedTask)
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	errCh := make(chan error, 1)
	gopool.Go(func() {
		errCh <- UpdateVideoTasks(context.Background(), constant.TaskPlatform("kling"), map[int][]string{
			slowChannelID: {
				slowTask.GetUpstreamTaskID(),
			},
			fastChannelID: {
				fastFirst.GetUpstreamTaskID(),
				fastSecond.GetUpstreamTaskID(),
			},
		}, map[string]*model.Task{
			slowTask.GetUpstreamTaskID():   slowTask,
			fastFirst.GetUpstreamTaskID():  fastFirst,
			fastSecond.GetUpstreamTaskID(): fastSecond,
		})
	})

	select {
	case <-adaptor.blockStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("slow channel did not start blocking")
	}

	require.Eventually(t, func() bool {
		fetchedTaskIDs := adaptor.fetchedTaskIDs()
		return len(fetchedTaskIDs) == 2 &&
			fetchedTaskIDs[0] == fastFirst.GetUpstreamTaskID() &&
			fetchedTaskIDs[1] == fastSecond.GetUpstreamTaskID()
	}, 500*time.Millisecond, 10*time.Millisecond)

	releaseBlockedTask()
	require.NoError(t, <-errCh)
	assert.ElementsMatch(t, []string{
		slowTask.GetUpstreamTaskID(),
		fastFirst.GetUpstreamTaskID(),
		fastSecond.GetUpstreamTaskID(),
	}, adaptor.fetchedTaskIDs())
}

func TestUpdateVideoTasksMixedChannelSleepSettings(t *testing.T) {
	truncate(t)

	const sleepyChannelID = 301
	const fastChannelID = 302
	seedTaskPollingChannel(t, sleepyChannelID, false)
	seedTaskPollingChannel(t, fastChannelID, true)
	sleepyFirst := seedPollingTask(t, sleepyChannelID, "task_public_9", "upstream_sleepy_1")
	sleepySecond := seedPollingTask(t, sleepyChannelID, "task_public_10", "upstream_sleepy_2")
	fastFirst := seedPollingTask(t, fastChannelID, "task_public_11", "upstream_fast_1")
	fastSecond := seedPollingTask(t, fastChannelID, "task_public_12", "upstream_fast_2")

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
		sleepyChannelID: {
			sleepyFirst.GetUpstreamTaskID(),
			sleepySecond.GetUpstreamTaskID(),
		},
		fastChannelID: {
			fastFirst.GetUpstreamTaskID(),
			fastSecond.GetUpstreamTaskID(),
		},
	}, map[string]*model.Task{
		sleepyFirst.GetUpstreamTaskID():  sleepyFirst,
		sleepySecond.GetUpstreamTaskID(): sleepySecond,
		fastFirst.GetUpstreamTaskID():    fastFirst,
		fastSecond.GetUpstreamTaskID():   fastSecond,
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.ElementsMatch(t, []string{"upstream_sleepy_1", "upstream_fast_1", "upstream_fast_2"}, adaptor.fetchedTaskIDs())
}

func TestUpdateSunoTasksStalePollsRefundExactlyOnce(t *testing.T) {
	truncate(t)

	const userID, tokenID, channelID = 401, 401, 401
	const initialUserQuota, initialTokenQuota, taskQuota = 10_000, 6_000, 2_500
	const publicTaskID, upstreamTaskID = "suno_public_refund_once", "suno_upstream_refund_once"

	seedUser(t, userID, initialUserQuota)
	seedToken(t, tokenID, userID, "sk-suno-refund-once", initialTokenQuota)
	baseURL := "https://suno.invalid"
	require.NoError(t, model.DB.Create(&model.Channel{
		Id:      channelID,
		Type:    constant.ChannelTypeSunoAPI,
		Name:    "suno_refund_once",
		Key:     "sk-suno-channel",
		Status:  common.ChannelStatusEnabled,
		BaseURL: &baseURL,
	}).Error)

	task := makeTask(userID, channelID, taskQuota, tokenID, BillingSourceWallet, 0)
	task.TaskID = publicTaskID
	task.Platform = constant.TaskPlatformSuno
	task.Status = model.TaskStatusInProgress
	task.Progress = "50%"
	task.SubmitTime = time.Now().Unix()
	task.PrivateData.UpstreamTaskID = upstreamTaskID
	require.NoError(t, model.DB.Create(task).Error)

	var firstPollTask model.Task
	var staleSecondPollTask model.Task
	require.NoError(t, model.DB.First(&firstPollTask, task.ID).Error)
	require.NoError(t, model.DB.First(&staleSecondPollTask, task.ID).Error)

	adaptor := &sunoFailurePollingAdaptor{failReason: "upstream failed"}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	require.NoError(t, updateSunoTasks(context.Background(), channelID, []string{upstreamTaskID}, map[string]*model.Task{
		upstreamTaskID: &firstPollTask,
	}))
	require.NoError(t, updateSunoTasks(context.Background(), channelID, []string{upstreamTaskID}, map[string]*model.Task{
		upstreamTaskID: &staleSecondPollTask,
	}))

	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, reloaded.Status)
	assert.Zero(t, reloaded.Quota)
	assert.Equal(t, initialUserQuota+taskQuota, getUserQuota(t, userID))
	assert.Equal(t, initialTokenQuota+taskQuota, getTokenRemainQuota(t, tokenID))
	assert.Equal(t, int64(1), countLogs(t))
}

func TestDurableSunoFailurePreservesLatestQueuedTaskColumns(t *testing.T) {
	truncate(t)

	const userID, tokenID, channelID = 861, 961, 1061
	const requestID = "durable-suno-narrow-queued"
	attempt := seedDurableBillingAttempt(t, requestID, userID, tokenID, 0)
	task := linkDurableBillingAttempt(t, attempt, requestID)
	baseURL := "https://suno-durable.invalid"
	require.NoError(t, model.DB.Create(&model.Channel{
		Id:      channelID,
		Type:    constant.ChannelTypeSunoAPI,
		Name:    "durable_suno_narrow",
		Key:     "sk-suno-channel",
		Status:  common.ChannelStatusEnabled,
		BaseURL: &baseURL,
	}).Error)

	initialPrivate := task.PrivateData
	initialPrivate.Key = "STALE_KEY"
	initialPrivate.UpstreamTaskID = "STALE_SUNO_UPSTREAM"
	require.NoError(t, model.DB.Model(&model.Task{}).Where("id = ?", task.ID).
		Updates(map[string]any{
			"channel_id":   channelID,
			"platform":     constant.TaskPlatformSuno,
			"status":       model.TaskStatusQueued,
			"progress":     "20%",
			"private_data": initialPrivate,
			"properties": model.Properties{
				Input:             "STALE_INPUT",
				UpstreamModelName: "STALE_UPSTREAM_MODEL",
			},
			"data": json.RawMessage(`{"stale":"discovery"}`),
		}).Error)

	var stale model.Task
	require.NoError(t, model.DB.First(&stale, task.ID).Error)
	latestPrivate := stale.PrivateData
	latestPrivate.Key = "LATEST_KEY"
	latestPrivate.UpstreamTaskID = "LATEST_SUNO_UPSTREAM"
	latestPrivate.ResultURL = "LATEST_RESULT"
	latestPrivate.NodeName = "LATEST_NODE"
	latestProperties := model.Properties{
		Input:             "LATEST_INPUT",
		UpstreamModelName: "LATEST_UPSTREAM_MODEL",
		OriginModelName:   "LATEST_ORIGIN_MODEL",
	}
	require.NoError(t, model.DB.Model(&model.Task{}).Where("id = ?", task.ID).
		Updates(map[string]any{
			"private_data": latestPrivate,
			"properties":   latestProperties,
			"data":         json.RawMessage(`{"latest":"before-poll"}`),
		}).Error)

	adaptor := &sunoFailurePollingAdaptor{
		failReason: "durable suno failed",
		data:       json.RawMessage(`{"sanitized":"suno"}`),
	}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	require.NoError(t, updateSunoTasks(
		context.Background(),
		channelID,
		[]string{"STALE_SUNO_UPSTREAM"},
		map[string]*model.Task{"STALE_SUNO_UPSTREAM": &stale},
	))

	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), reloaded.Status)
	assert.Equal(t, "100%", reloaded.Progress)
	assert.Equal(t, "LATEST_KEY", reloaded.PrivateData.Key)
	assert.Equal(t, "LATEST_SUNO_UPSTREAM", reloaded.PrivateData.UpstreamTaskID)
	assert.Equal(t, "LATEST_RESULT", reloaded.PrivateData.ResultURL)
	assert.Equal(t, "LATEST_NODE", reloaded.PrivateData.NodeName)
	require.NotNil(t, reloaded.PrivateData.BillingContext)
	assert.Equal(
		t,
		stale.PrivateData.BillingContext,
		reloaded.PrivateData.BillingContext,
	)
	assert.Equal(t, latestProperties, reloaded.Properties)
	assert.JSONEq(t, `{"sanitized":"suno"}`, string(reloaded.Data))
}

func TestDurableVideoFailurePreservesLatestInProgressTaskColumns(t *testing.T) {
	truncate(t)

	const userID, tokenID, channelID = 862, 962, 1062
	const requestID = "durable-video-narrow-in-progress"
	attempt := seedDurableBillingAttempt(t, requestID, userID, tokenID, 0)
	task := linkDurableBillingAttempt(t, attempt, requestID)
	seedTaskPollingChannel(t, channelID, true)

	initialPrivate := task.PrivateData
	initialPrivate.Key = "STALE_VIDEO_KEY"
	initialPrivate.UpstreamTaskID = "STALE_VIDEO_UPSTREAM"
	require.NoError(t, model.DB.Model(&model.Task{}).Where("id = ?", task.ID).
		Updates(map[string]any{
			"channel_id":   channelID,
			"platform":     constant.TaskPlatform("kling"),
			"status":       model.TaskStatusInProgress,
			"progress":     "50%",
			"private_data": initialPrivate,
			"properties": model.Properties{
				Input:             "STALE_VIDEO_INPUT",
				UpstreamModelName: "STALE_VIDEO_MODEL",
			},
			"data": json.RawMessage(`{"stale":"video-discovery"}`),
		}).Error)

	var stale model.Task
	require.NoError(t, model.DB.First(&stale, task.ID).Error)
	latestPrivate := stale.PrivateData
	latestPrivate.Key = "LATEST_VIDEO_KEY"
	latestPrivate.UpstreamTaskID = "LATEST_VIDEO_UPSTREAM"
	latestPrivate.ResultURL = "LATEST_VIDEO_RESULT"
	latestPrivate.NodeName = "LATEST_VIDEO_NODE"
	latestProperties := model.Properties{
		Input:             "LATEST_VIDEO_INPUT",
		UpstreamModelName: "LATEST_VIDEO_UPSTREAM_MODEL",
		OriginModelName:   "LATEST_VIDEO_ORIGIN_MODEL",
	}
	require.NoError(t, model.DB.Model(&model.Task{}).Where("id = ?", task.ID).
		Updates(map[string]any{
			"private_data": latestPrivate,
			"properties":   latestProperties,
			"data":         json.RawMessage(`{"latest":"video-before-poll"}`),
		}).Error)

	var channel model.Channel
	require.NoError(t, model.DB.First(&channel, channelID).Error)
	channel.Key = "STALE_VIDEO_KEY\nLATEST_VIDEO_KEY"
	channel.Keys = []string{"STALE_VIDEO_KEY", "LATEST_VIDEO_KEY"}
	channel.ChannelInfo = model.ChannelInfo{
		IsMultiKey: true,
		MultiKeyStatusList: map[int]int{
			0: common.ChannelStatusEnabled,
			1: common.ChannelStatusEnabled,
		},
	}
	err := updateVideoSingleTask(
		context.Background(),
		&videoFailurePollingAdaptor{reason: "durable video failed"},
		&channel,
		"STALE_VIDEO_UPSTREAM",
		map[string]*model.Task{"STALE_VIDEO_UPSTREAM": &stale},
	)
	require.NoError(t, err)

	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), reloaded.Status)
	assert.Equal(t, "100%", reloaded.Progress)
	assert.Equal(t, "LATEST_VIDEO_KEY", reloaded.PrivateData.Key)
	assert.Equal(t, "LATEST_VIDEO_UPSTREAM", reloaded.PrivateData.UpstreamTaskID)
	assert.Equal(t, "LATEST_VIDEO_RESULT", reloaded.PrivateData.ResultURL)
	assert.Equal(t, "LATEST_VIDEO_NODE", reloaded.PrivateData.NodeName)
	require.NotNil(t, reloaded.PrivateData.BillingContext)
	assert.Equal(
		t,
		stale.PrivateData.BillingContext,
		reloaded.PrivateData.BillingContext,
	)
	assert.Equal(t, latestProperties, reloaded.Properties)
	assert.Contains(t, string(reloaded.Data), `"code":"success"`)
	assert.Contains(t, string(reloaded.Data), `"status":"FAILURE"`)
}

func TestMissingSunoChannelRefundsDurableAttemptThroughFailurePrimitive(t *testing.T) {
	truncate(t)

	const userID, tokenID, missingChannelID = 863, 963, 1063
	const requestID = "durable-suno-missing-channel"
	attempt := seedDurableBillingAttempt(t, requestID, userID, tokenID, 0)
	task := linkDurableBillingAttempt(t, attempt, requestID)
	privateData := task.PrivateData
	privateData.UpstreamTaskID = "MISSING_CHANNEL_UPSTREAM"
	require.NoError(t, model.DB.Model(&model.Task{}).Where("id = ?", task.ID).
		Updates(map[string]any{
			"channel_id":   missingChannelID,
			"platform":     constant.TaskPlatformSuno,
			"status":       model.TaskStatusQueued,
			"progress":     "20%",
			"private_data": privateData,
		}).Error)
	var stale model.Task
	require.NoError(t, model.DB.First(&stale, task.ID).Error)

	_ = updateSunoTasks(
		context.Background(),
		missingChannelID,
		[]string{"MISSING_CHANNEL_UPSTREAM"},
		map[string]*model.Task{"MISSING_CHANNEL_UPSTREAM": &stale},
	)

	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), reloaded.Status)
	assert.Equal(t, "100%", reloaded.Progress)
	assert.NotZero(t, reloaded.FinishTime)
	reloadedAttempt, err := model.GetTaskBillingAttemptByRequestID(requestID)
	require.NoError(t, err)
	assert.NotZero(t, reloadedAttempt.RefundCompletedAt)
}

func TestMissingVideoChannelRefundsDurableAttemptThroughFailurePrimitive(t *testing.T) {
	truncate(t)

	const userID, tokenID, missingChannelID = 864, 964, 1064
	const requestID = "durable-video-missing-channel"
	attempt := seedDurableBillingAttempt(t, requestID, userID, tokenID, 0)
	task := linkDurableBillingAttempt(t, attempt, requestID)
	privateData := task.PrivateData
	privateData.UpstreamTaskID = "MISSING_VIDEO_CHANNEL_UPSTREAM"
	require.NoError(t, model.DB.Model(&model.Task{}).Where("id = ?", task.ID).
		Updates(map[string]any{
			"channel_id":   missingChannelID,
			"platform":     constant.TaskPlatform("kling"),
			"status":       model.TaskStatusInProgress,
			"progress":     "45%",
			"private_data": privateData,
		}).Error)
	var stale model.Task
	require.NoError(t, model.DB.First(&stale, task.ID).Error)

	require.Error(t, updateVideoTasks(
		context.Background(),
		constant.TaskPlatform("kling"),
		missingChannelID,
		[]string{"MISSING_VIDEO_CHANNEL_UPSTREAM"},
		map[string]*model.Task{"MISSING_VIDEO_CHANNEL_UPSTREAM": &stale},
	))

	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), reloaded.Status)
	assert.Equal(t, "100%", reloaded.Progress)
	assert.NotZero(t, reloaded.FinishTime)
	reloadedAttempt, err := model.GetTaskBillingAttemptByRequestID(requestID)
	require.NoError(t, err)
	assert.NotZero(t, reloadedAttempt.RefundCompletedAt)
}

func TestTaskPollingReconciliationSeedDanceMissingLedgerFailsClosed(t *testing.T) {
	truncate(t)

	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = nil
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	const (
		walletUserID       = 871
		walletTokenID      = 971
		walletQuota        = 120
		subscriptionUserID = 872
		subscriptionToken  = 972
		subscriptionID     = 1_072
		subscriptionQuota  = 170
	)
	seedUser(t, walletUserID, 5_000)
	seedToken(t, walletTokenID, walletUserID, "sk-seedance-missing-wallet", 4_000)
	seedUser(t, subscriptionUserID, 6_000)
	seedToken(
		t,
		subscriptionToken,
		subscriptionUserID,
		"sk-seedance-missing-subscription",
		3_000,
	)
	seedSubscription(t, subscriptionID, subscriptionUserID, 10_000, 2_500)

	oldUpdatedAt := time.Now().Add(-time.Minute).Unix()
	walletTask := makeTask(
		walletUserID,
		0,
		walletQuota,
		walletTokenID,
		BillingSourceWallet,
		0,
	)
	walletTask.TaskID = "seedance_failed_missing_wallet_ledger"
	walletTask.Platform = seedDanceTaskPlatform
	walletTask.Status = model.TaskStatusFailure
	walletTask.Progress = taskcommon.ProgressComplete
	walletTask.SubmitTime = model.TaskRefundLegacyCutoff
	walletTask.UpdatedAt = oldUpdatedAt
	require.NoError(t, model.DB.Create(walletTask).Error)

	subscriptionTask := makeTask(
		subscriptionUserID,
		0,
		subscriptionQuota,
		subscriptionToken,
		BillingSourceSubscription,
		subscriptionID,
	)
	subscriptionTask.TaskID = "seedance_failed_missing_subscription_ledger"
	subscriptionTask.Platform = seedDanceTaskPlatform
	subscriptionTask.Status = model.TaskStatusFailure
	subscriptionTask.Progress = taskcommon.ProgressComplete
	subscriptionTask.SubmitTime = model.TaskRefundLegacyCutoff
	subscriptionTask.UpdatedAt = oldUpdatedAt
	require.NoError(t, model.DB.Create(subscriptionTask).Error)

	walletUserBefore := getUserQuota(t, walletUserID)
	walletTokenBefore := getTokenRemainQuota(t, walletTokenID)
	subscriptionUserBefore := getUserQuota(t, subscriptionUserID)
	subscriptionTokenBefore := getTokenRemainQuota(t, subscriptionToken)
	subscriptionUsedBefore := getSubscriptionUsed(t, subscriptionID)
	logsBefore := countLogs(t)

	assert.False(
		t,
		model.HasTaskPollingWork(),
		"missing-ledger Type 59 failures must not keep the legacy refund scheduler active",
	)
	RunTaskPollingOnce(context.Background(), nil)
	RunTaskPollingOnce(context.Background(), nil)

	var reloadedWallet model.Task
	var reloadedSubscription model.Task
	require.NoError(t, model.DB.First(&reloadedWallet, walletTask.ID).Error)
	require.NoError(t, model.DB.First(&reloadedSubscription, subscriptionTask.ID).Error)
	assert.Equal(t, walletQuota, reloadedWallet.Quota)
	assert.Equal(t, subscriptionQuota, reloadedSubscription.Quota)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), reloadedWallet.Status)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), reloadedSubscription.Status)
	assert.Equal(t, taskcommon.ProgressComplete, reloadedWallet.Progress)
	assert.Equal(t, taskcommon.ProgressComplete, reloadedSubscription.Progress)

	assert.Equal(t, walletUserBefore, getUserQuota(t, walletUserID))
	assert.Equal(t, walletTokenBefore, getTokenRemainQuota(t, walletTokenID))
	assert.Equal(t, subscriptionUserBefore, getUserQuota(t, subscriptionUserID))
	assert.Equal(t, subscriptionTokenBefore, getTokenRemainQuota(t, subscriptionToken))
	assert.Equal(t, subscriptionUsedBefore, getSubscriptionUsed(t, subscriptionID))
	assert.Equal(t, logsBefore, countLogs(t))

	var attemptCount int64
	require.NoError(t, model.DB.Model(&model.TaskBillingAttempt{}).
		Where("task_id IN ?", []int64{walletTask.ID, subscriptionTask.ID}).
		Count(&attemptCount).Error)
	assert.Zero(t, attemptCount, "reconciliation must not fabricate component markers")
	assert.False(t, model.HasTaskPollingWork())
}

func TestRefundTaskQuotaSeedDanceMissingLedgerCentralGate(t *testing.T) {
	truncate(t)

	const userID, tokenID, quota = 873, 973, 190
	seedUser(t, userID, 7_000)
	seedToken(t, tokenID, userID, "sk-seedance-central-gate", 6_000)
	task := makeTask(userID, 0, quota, tokenID, BillingSourceWallet, 0)
	task.TaskID = "seedance_missing_ledger_central_gate"
	task.Platform = seedDanceTaskPlatform
	task.Status = model.TaskStatusFailure
	task.Progress = taskcommon.ProgressComplete
	task.SubmitTime = model.TaskRefundLegacyCutoff
	require.NoError(t, model.DB.Create(task).Error)

	userBefore := getUserQuota(t, userID)
	tokenBefore := getTokenRemainQuota(t, tokenID)
	logsBefore := countLogs(t)
	staleCaller := *task
	staleCaller.Platform = constant.TaskPlatform("kling")
	staleCaller.Quota = quota + 50
	assert.False(t, RefundTaskQuota(
		context.Background(),
		&staleCaller,
		"missing ledger through stale caller",
	))
	assert.False(t, RefundTaskQuota(
		context.Background(),
		&staleCaller,
		"missing ledger retry through stale caller",
	))

	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, quota, reloaded.Quota)
	assert.Equal(t, userBefore, getUserQuota(t, userID))
	assert.Equal(t, tokenBefore, getTokenRemainQuota(t, tokenID))
	assert.Equal(t, logsBefore, countLogs(t))

	var attemptCount int64
	require.NoError(t, model.DB.Model(&model.TaskBillingAttempt{}).
		Where("task_id = ?", task.ID).
		Count(&attemptCount).Error)
	assert.Zero(t, attemptCount)
}

func TestTaskPollingReconciliationRefundsSeedDanceDurableAttempt(t *testing.T) {
	truncate(t)

	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = nil
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	const userID, tokenID, quota = 874, 974, 210
	const requestID = "seedance-durable-failure-reconciliation"
	attempt := seedDurableBillingAttempt(t, requestID, userID, tokenID, quota)
	task := linkDurableBillingAttempt(t, attempt, requestID)
	require.NoError(t, model.DB.Model(&model.Task{}).
		Where("id = ?", task.ID).
		Update("platform", seedDanceTaskPlatform).Error)
	task, err := model.TransitionTaskSubmissionToFailure(
		task.ID,
		task.TaskID,
		"",
		"provider_failure",
		"provider failed",
	)
	require.NoError(t, err)

	assert.True(t, model.HasTaskPollingWork())
	RunTaskPollingOnce(context.Background(), nil)
	RunTaskPollingOnce(context.Background(), nil)

	reloadedAttempt, err := model.GetTaskBillingAttemptByTaskID(task.ID)
	require.NoError(t, err)
	assert.NotZero(t, reloadedAttempt.FundingRefundedAt)
	assert.NotZero(t, reloadedAttempt.TokenRefundedAt)
	assert.NotZero(t, reloadedAttempt.RefundStartedAt)
	assert.NotZero(t, reloadedAttempt.RefundCompletedAt)
	assert.Zero(t, reloadedAttempt.SucceededAt)

	var reloadedTask model.Task
	require.NoError(t, model.DB.First(&reloadedTask, task.ID).Error)
	assert.Zero(t, reloadedTask.Quota)
	assert.Equal(t, 10_000, getUserQuota(t, userID))
	assert.Equal(t, 10_000, getTokenRemainQuota(t, tokenID))
	assert.Equal(t, int64(1), countLogs(t))
	assert.False(t, model.HasTaskPollingWork())
}

func TestStaleRequestOwnedAttemptSweepRecoversPartialPreconsume(t *testing.T) {
	truncate(t)

	testCases := []struct {
		name           string
		userID         int
		tokenID        int
		quota          int
		consumeFunding bool
		consumeToken   bool
	}{
		{name: "funding only", userID: 851, tokenID: 951, quota: 100, consumeFunding: true},
		{name: "token only", userID: 852, tokenID: 952, quota: 110, consumeToken: true},
		{name: "both", userID: 853, tokenID: 953, quota: 120, consumeFunding: true, consumeToken: true},
		{name: "paid zero", userID: 854, tokenID: 954, quota: 0, consumeFunding: true, consumeToken: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			seedUser(t, testCase.userID, 1_000)
			seedToken(t, testCase.tokenID, testCase.userID, "sk-stale-"+testCase.name, 1_000)
			requestID := fmt.Sprintf("stale-request-%d", testCase.userID)
			attempt, err := model.BeginTaskBillingAttempt(model.TaskBillingAttemptSnapshot{
				RequestID:     requestID,
				PublicTaskID:  "task_" + requestID,
				SubmitTime:    time.Now().Add(-time.Minute).Unix(),
				UserID:        testCase.userID,
				FundingSource: BillingSourceWallet,
				FundingAmount: testCase.quota,
				TokenID:       testCase.tokenID,
				TokenAmount:   testCase.quota,
			})
			require.NoError(t, err)
			if testCase.consumeFunding {
				_, err = model.ApplyTaskFundingPreconsume(requestID)
				require.NoError(t, err)
			}
			if testCase.consumeToken {
				_, err = model.ApplyTaskTokenPreconsume(requestID)
				require.NoError(t, err)
			}
			require.NoError(t, model.DB.Model(&model.TaskBillingAttempt{}).
				Where("id = ?", attempt.ID).
				Update("updated_at", time.Now().Add(-time.Minute).Unix()).Error)

			sweepUnrefundedFailedTasks(context.Background())
			sweepUnrefundedFailedTasks(context.Background())

			reloaded, err := model.GetTaskBillingAttemptByRequestID(requestID)
			require.NoError(t, err)
			assert.Equal(t, model.TaskBillingOwnerRequest, reloaded.Owner)
			assert.Nil(t, reloaded.TaskID)
			assert.NotZero(t, reloaded.RefundStartedAt)
			assert.NotZero(t, reloaded.FundingRefundedAt)
			assert.NotZero(t, reloaded.TokenRefundedAt)
			assert.NotZero(t, reloaded.RefundCompletedAt)
			assert.Equal(t, 1_000, getUserQuota(t, testCase.userID))
			assert.Equal(t, 1_000, getTokenRemainQuota(t, testCase.tokenID))
		})
	}
}

func TestRecoverySweepConvergesWalletRefundAfterUserSoftDelete(t *testing.T) {
	truncate(t)

	const userID, tokenID, quota = 865, 965, 125
	const requestID = "soft-deleted-wallet-recovery"
	attempt := seedDurableBillingAttempt(t, requestID, userID, tokenID, quota)
	require.NoError(t, model.DB.Delete(&model.User{Id: userID}).Error)
	require.NoError(t, model.DB.Model(&model.TaskBillingAttempt{}).
		Where("id = ?", attempt.ID).
		Update("updated_at", time.Now().Add(-time.Minute).Unix()).Error)

	sweepUnrefundedFailedTasks(context.Background())
	sweepUnrefundedFailedTasks(context.Background())

	reloadedAttempt, err := model.GetTaskBillingAttemptByRequestID(requestID)
	require.NoError(t, err)
	assert.NotZero(t, reloadedAttempt.FundingRefundedAt)
	assert.NotZero(t, reloadedAttempt.TokenRefundedAt)
	assert.NotZero(t, reloadedAttempt.RefundCompletedAt)
	var deletedUser model.User
	require.NoError(t, model.DB.Unscoped().Where("id = ?", userID).First(&deletedUser).Error)
	assert.Equal(t, 10_000, deletedUser.Quota)
	assert.Equal(t, 10_000, getTokenRemainQuota(t, tokenID))
}

func TestRunTaskPollingOnceDoesNotRefundHistoricalFailedTask(t *testing.T) {
	truncate(t)

	const userID, initialQuota, taskQuota = 402, 10_000, 1_200
	seedUser(t, userID, initialQuota)

	task := makeTask(userID, 0, taskQuota, 0, BillingSourceWallet, 0)
	task.TaskID = "historical_failed_already_refunded"
	task.Status = model.TaskStatusFailure
	task.Progress = "100%"
	task.SubmitTime = time.Now().Add(-90 * 24 * time.Hour).Unix()
	task.UpdatedAt = time.Now().Add(-time.Minute).Unix()
	require.NoError(t, model.DB.Create(task).Error)

	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor {
		return &taskPollingFetchAdaptor{}
	}
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	summary := RunTaskPollingOnce(context.Background(), nil)

	assert.Zero(t, summary.UnfinishedTasks)
	assert.Equal(t, initialQuota, getUserQuota(t, userID))
	assert.Equal(t, taskQuota, getTaskQuota(t, task.ID))
	assert.Equal(t, int64(0), countLogs(t))
}

func TestSweepTimedOutTasksHonorsRefundRolloutBoundary(t *testing.T) {
	truncate(t)

	const (
		userID          = 403
		initialQuota    = 10_000
		legacyTaskQuota = 1_800
		modernTaskQuota = 1_200
	)
	seedUser(t, userID, initialQuota)

	legacyTask := makeTask(userID, 0, legacyTaskQuota, 0, BillingSourceWallet, 0)
	legacyTask.TaskID = "legacy_timeout_without_refund"
	legacyTask.Progress = "50%"
	legacyTask.SubmitTime = 1771718399 // 2026-02-21 23:59:59 UTC
	require.NoError(t, model.DB.Create(legacyTask).Error)

	modernTask := makeTask(userID, 0, modernTaskQuota, 0, BillingSourceWallet, 0)
	modernTask.TaskID = "modern_timeout_with_refund"
	modernTask.Progress = "50%"
	modernTask.SubmitTime = 1771718400 // 2026-02-22 00:00:00 UTC
	require.NoError(t, model.DB.Create(modernTask).Error)

	previousTimeout := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 1
	t.Cleanup(func() { constant.TaskTimeoutMinutes = previousTimeout })

	sweepTimedOutTasks(context.Background())

	var reloadedLegacy model.Task
	var reloadedModern model.Task
	require.NoError(t, model.DB.First(&reloadedLegacy, legacyTask.ID).Error)
	require.NoError(t, model.DB.First(&reloadedModern, modernTask.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, reloadedLegacy.Status)
	assert.EqualValues(t, model.TaskStatusFailure, reloadedModern.Status)
	assert.Zero(t, reloadedLegacy.Quota)
	assert.Zero(t, reloadedModern.Quota)
	assert.Contains(t, reloadedLegacy.FailReason, "旧系统遗留任务")
	assert.Contains(t, reloadedModern.FailReason, "任务超时")
	assert.Equal(t, initialQuota+modernTaskQuota, getUserQuota(t, userID))
	assert.Equal(t, int64(1), countLogs(t))
}
