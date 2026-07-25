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
	mu      sync.Mutex
	calls   int
	seenKey string
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
	a.mu.Unlock()
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

type finalSuccessPollingAdaptor struct {
	adjustReturn int
}

func (a *finalSuccessPollingAdaptor) Init(_ *relaycommon.RelayInfo) {}

func (a *finalSuccessPollingAdaptor) FetchTask(
	_ string,
	_ string,
	_ map[string]any,
	_ string,
) (*http.Response, error) {
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
	return a.adjustReturn
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
	require.ErrorIs(t, <-errCh, context.Canceled)
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
	task.SubmitTime = model.TaskRefundLegacyCutoff
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
	latestPrivate.BillingContext = &model.TaskBillingContext{
		ModelPrice:      9,
		GroupRatio:      8,
		ModelRatio:      7,
		OtherRatios:     map[string]float64{"latest": 6},
		OriginModelName: "LATEST_MODEL",
		PerCallBilling:  true,
	}
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
	assert.Equal(t, "LATEST_MODEL", reloaded.PrivateData.BillingContext.OriginModelName)
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
	latestPrivate.BillingContext = &model.TaskBillingContext{
		ModelPrice:      19,
		GroupRatio:      18,
		ModelRatio:      17,
		OtherRatios:     map[string]float64{"latest": 16},
		OriginModelName: "LATEST_VIDEO_MODEL",
		PerCallBilling:  true,
	}
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
	assert.Equal(t, "LATEST_VIDEO_MODEL", reloaded.PrivateData.BillingContext.OriginModelName)
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

func TestSweepUnrefundedFailedTasksRefundsModernTaskAndSkipsLegacy(t *testing.T) {
	truncate(t)

	const userID = 402
	const initialQuota, modernTaskQuota, legacyTaskQuota = 10_000, 1_200, 1_800
	seedUser(t, userID, initialQuota)

	modernTask := makeTask(userID, 0, modernTaskQuota, 0, BillingSourceWallet, 0)
	modernTask.TaskID = "modern_failed_pending_refund"
	modernTask.Status = model.TaskStatusFailure
	modernTask.Progress = "100%"
	modernTask.SubmitTime = model.TaskRefundLegacyCutoff
	modernTask.UpdatedAt = time.Now().Add(-time.Minute).Unix()
	require.NoError(t, model.DB.Create(modernTask).Error)

	legacyTask := makeTask(userID, 0, legacyTaskQuota, 0, BillingSourceWallet, 0)
	legacyTask.TaskID = "legacy_failed_without_refund"
	legacyTask.Status = model.TaskStatusFailure
	legacyTask.Progress = "100%"
	legacyTask.SubmitTime = model.TaskRefundLegacyCutoff - 1
	legacyTask.UpdatedAt = time.Now().Add(-time.Minute).Unix()
	require.NoError(t, model.DB.Create(legacyTask).Error)

	sweepUnrefundedFailedTasks(context.Background())
	sweepUnrefundedFailedTasks(context.Background())

	var reloadedModern model.Task
	var reloadedLegacy model.Task
	require.NoError(t, model.DB.First(&reloadedModern, modernTask.ID).Error)
	require.NoError(t, model.DB.First(&reloadedLegacy, legacyTask.ID).Error)
	assert.Zero(t, reloadedModern.Quota)
	assert.Equal(t, legacyTaskQuota, reloadedLegacy.Quota)
	assert.Equal(t, initialQuota+modernTaskQuota, getUserQuota(t, userID))
	assert.Equal(t, int64(1), countLogs(t))
}

func TestSweepUnrefundedFailedTasksRestoresMarkerAfterFundingFailure(t *testing.T) {
	truncate(t)

	const userID, subscriptionID, taskQuota = 404, 404, 900
	const subscriptionUsed int64 = 5_000
	seedUser(t, userID, 0)

	task := makeTask(userID, 0, taskQuota, 0, BillingSourceSubscription, subscriptionID)
	task.TaskID = "subscription_failed_pending_refund"
	task.Status = model.TaskStatusFailure
	task.Progress = "100%"
	task.SubmitTime = model.TaskRefundLegacyCutoff
	task.UpdatedAt = time.Now().Add(-time.Minute).Unix()
	require.NoError(t, model.DB.Create(task).Error)

	sweepUnrefundedFailedTasks(context.Background())

	var afterFailedRefund model.Task
	require.NoError(t, model.DB.First(&afterFailedRefund, task.ID).Error)
	assert.Equal(t, taskQuota, afterFailedRefund.Quota)
	assert.Equal(t, int64(0), countLogs(t))

	seedSubscription(t, subscriptionID, userID, 10_000, subscriptionUsed)
	require.NoError(t, model.DB.Model(&model.Task{}).
		Where("id = ?", task.ID).
		UpdateColumn("updated_at", time.Now().Add(-time.Minute).Unix()).Error)

	sweepUnrefundedFailedTasks(context.Background())

	var afterSuccessfulRetry model.Task
	require.NoError(t, model.DB.First(&afterSuccessfulRetry, task.ID).Error)
	assert.Zero(t, afterSuccessfulRetry.Quota)
	assert.Equal(t, subscriptionUsed-int64(taskQuota), getSubscriptionUsed(t, subscriptionID))
	assert.Equal(t, int64(1), countLogs(t))
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
