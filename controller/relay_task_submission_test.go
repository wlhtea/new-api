package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func taskSubmissionControllerContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	return c
}

type controllerSubmissionBilling struct {
	preConsumed int
	settleErr   error
	settleCalls int
	refundCalls int
}

func (b *controllerSubmissionBilling) Settle(actual int) error {
	b.settleCalls++
	if actual != b.preConsumed {
		return fmt.Errorf("unexpected delta")
	}
	return b.settleErr
}
func (b *controllerSubmissionBilling) Refund(*gin.Context) {
	b.refundCalls++
}
func (b *controllerSubmissionBilling) NeedsRefund() bool        { return false }
func (b *controllerSubmissionBilling) GetPreConsumedQuota() int { return b.preConsumed }
func (b *controllerSubmissionBilling) Reserve(int) error        { return nil }

func setupControllerTaskSubmissionDB(t *testing.T) {
	t.Helper()
	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousLogEnabled := common.LogConsumeEnabled
	previousBatch := common.BatchUpdateEnabled
	previousRedis := common.RedisEnabled
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf(
		"file:controller-task-submission-%d?mode=memory&cache=shared",
		time.Now().UnixNano(),
	)), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	model.DB = db
	model.LOG_DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.LogConsumeEnabled = true
	common.BatchUpdateEnabled = false
	common.RedisEnabled = false
	require.NoError(t, db.AutoMigrate(
		&model.Task{},
		&model.TaskBillingAttempt{},
		&model.User{},
		&model.Token{},
		&model.Log{},
		&model.Channel{},
		&model.SystemTask{},
		&model.SystemTaskLock{},
	))
	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.LogConsumeEnabled = previousLogEnabled
		common.BatchUpdateEnabled = previousBatch
		common.RedisEnabled = previousRedis
		_ = sqlDB.Close()
	})
}

func configureControllerTaskSubmissionPrice(t *testing.T) {
	t.Helper()
	oldModelPrices := ratio_setting.ModelPrice2JSONString()
	oldGroupRatios := ratio_setting.GroupRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(fmt.Sprintf(
		`{"seedance-uncensored":%g}`,
		25.0/common.QuotaPerUnit,
	)))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1}`))
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(oldModelPrices))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(oldGroupRatios))
	})
}

func seedControllerTaskBillingSubjects(t *testing.T, userID, tokenID int) {
	t.Helper()
	require.NoError(t, model.DB.Create(&model.User{
		Id:       userID,
		Username: fmt.Sprintf("controller-user-%d", userID),
		Quota:    1_000_000,
	}).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		Id:          tokenID,
		UserId:      userID,
		Key:         fmt.Sprintf("controller-token-%d", tokenID),
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
		RemainQuota: 1_000_000,
	}).Error)
}

func newSeedDanceControllerRelayContext(
	recorder http.ResponseWriter,
	baseURL string,
	requestBody string,
) *gin.Context {
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/videos",
		strings.NewReader(requestBody),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("platform", "59")
	c.Set(string(constant.ContextKeyChannelType), constant.ChannelTypeSeedDance)
	c.Set(string(constant.ContextKeyChannelId), 59)
	c.Set(string(constant.ContextKeyChannelKey), "SECRET_ROUTE_KEY")
	c.Set(string(constant.ContextKeyChannelBaseUrl), baseURL)
	c.Set(string(constant.ContextKeyOriginalModel), "seedance-uncensored")
	c.Set("id", 8301)
	c.Set("token_id", 9301)
	c.Set("token_key", "controller-token-9301")
	c.Set("token_name", "controller-token")
	c.Set("group", "default")
	c.Set("user_group", "default")
	c.Set("user_quota", 1_000_000)
	c.Set("user_setting", dto.UserSetting{BillingPreference: "wallet_only"})
	c.Set("channel_name", "seedance-fixture")
	return c
}

func newSeedDanceControllerRelayInfo(requestID string) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		UserId:          8301,
		TokenId:         9301,
		RequestId:       requestID,
		OriginModelName: "seedance-uncensored",
		UsingGroup:      "default",
		UserSetting: dto.UserSetting{
			BillingPreference: "wallet_only",
		},
		TaskRelayInfo: &relaycommon.TaskRelayInfo{},
	}
}

func createSubmittedControllerTask(
	t *testing.T,
	requestID string,
	billing *controllerSubmissionBilling,
) (*relaycommon.RelayInfo, *relay.TaskSubmitResult, *model.Task) {
	t.Helper()
	const userID = 8301
	const tokenID = 9301
	const quota = 25
	require.NoError(t, model.DB.Create(&model.User{
		Id:       userID,
		Username: "controller-submit-user",
		Quota:    100,
	}).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		Id:          tokenID,
		UserId:      userID,
		Key:         "controller-submit-token",
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
		RemainQuota: 100,
	}).Error)
	billingContext := &model.TaskBillingContext{
		ModelPrice:      0.2,
		GroupRatio:      1,
		ModelRatio:      1,
		OriginModelName: "seedance-uncensored",
		PerCallBilling:  true,
	}
	attempt, err := model.BeginTaskBillingAttempt(model.TaskBillingAttemptSnapshot{
		RequestID:      requestID,
		PublicTaskID:   "task_" + requestID,
		SubmitTime:     1_750_002_000,
		UserID:         userID,
		FundingSource:  "wallet",
		FundingAmount:  quota,
		TokenID:        tokenID,
		TokenAmount:    quota,
		BillingContext: billingContext,
	})
	require.NoError(t, err)
	_, err = model.ApplyTaskFundingPreconsume(requestID)
	require.NoError(t, err)
	_, err = model.ApplyTaskTokenPreconsume(requestID)
	require.NoError(t, err)
	task, _, err := model.PrepareTaskSubmissionAttempt(&model.Task{
		TaskID:     attempt.PublicTaskID,
		Platform:   constant.TaskPlatform("59"),
		UserId:     userID,
		Group:      "default",
		ChannelId:  constant.ChannelTypeSeedDance,
		Quota:      quota,
		Action:     "text2video",
		Status:     model.TaskStatusSubmitting,
		SubmitTime: attempt.SubmitTime,
		Progress:   "0%",
		Properties: model.Properties{OriginModelName: "seedance-uncensored"},
		PrivateData: model.TaskPrivateData{
			BillingSource:  "wallet",
			TokenId:        tokenID,
			BillingContext: billingContext,
		},
	}, 0, requestID)
	require.NoError(t, err)
	_, err = model.AttachTaskUpstreamResult(
		task.ID,
		task.TaskID,
		"UPSTREAM_DEFERRED",
		[]byte(`{"safe":"data"}`),
	)
	require.NoError(t, err)
	_, err = model.CommitTaskSubmission(task.ID, task.TaskID)
	require.NoError(t, err)

	info := &relaycommon.RelayInfo{
		UserId:          userID,
		TokenId:         tokenID,
		RequestId:       requestID,
		OriginModelName: "seedance-uncensored",
		UsingGroup:      "default",
		ForcePreConsume: true,
		Billing:         billing,
		BillingSource:   "wallet",
		PriceData: types.PriceData{
			UsePrice:       true,
			Quota:          quota,
			ModelPrice:     0.2,
			ModelRatio:     1,
			GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1},
		},
		TaskRelayInfo: &relaycommon.TaskRelayInfo{
			PublicTaskID:            task.TaskID,
			PersistentTaskID:        task.ID,
			BillingAttemptRequestID: requestID,
			DurableSubmitTime:       task.SubmitTime,
			Action:                  "text2video",
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:   constant.ChannelTypeSeedDance,
			ChannelType: constant.ChannelTypeSeedDance,
		},
	}
	result := &relay.TaskSubmitResult{
		UpstreamTaskID: "UPSTREAM_DEFERRED",
		TaskData:       []byte(`{"safe":"data"}`),
		Platform:       constant.TaskPlatform("59"),
		Quota:          quota,
		HTTPResponse: &channel.TaskSubmitHTTPResponse{
			StatusCode: http.StatusOK,
			Body:       map[string]any{"id": task.TaskID},
		},
	}
	return info, result, task
}

type settlementOrderWriter struct {
	header    http.Header
	requestID string
	status    int
	body      []byte
	orderErr  error
}

func (w *settlementOrderWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *settlementOrderWriter) verifyOrder() {
	if w.orderErr != nil {
		return
	}
	attempt, err := model.GetTaskBillingAttemptByRequestID(w.requestID)
	if err != nil {
		w.orderErr = err
		return
	}
	if attempt.SubmissionSettledAt == 0 {
		w.orderErr = errors.New("HTTP written before submission settlement marker")
		return
	}
	var count int64
	if err := model.DB.Model(&model.Log{}).
		Where("request_id = ? AND type = ?", w.requestID, model.LogTypeConsume).
		Count(&count).Error; err != nil {
		w.orderErr = err
		return
	}
	if count != 1 {
		w.orderErr = fmt.Errorf("HTTP written before consumption log: count=%d", count)
	}
}

func (w *settlementOrderWriter) WriteHeader(status int) {
	w.verifyOrder()
	w.status = status
}

func (w *settlementOrderWriter) Write(body []byte) (int, error) {
	w.verifyOrder()
	w.body = append(w.body, body...)
	return len(body), nil
}

func TestReliablePartialMergeNeverOverwritesKnownID(t *testing.T) {
	existing := &relay.TaskSubmitResult{
		UpstreamTaskID: "UPSTREAM_A",
		TaskData:       []byte(`{"safe":"a"}`),
	}
	merged, taskErr := mergeReliableTaskSubmitResult(existing, nil)
	require.Nil(t, taskErr)
	assert.Same(t, existing, merged)

	same := &relay.TaskSubmitResult{
		UpstreamTaskID: "UPSTREAM_A",
		HTTPResponse:   &channel.TaskSubmitHTTPResponse{StatusCode: http.StatusOK},
	}
	merged, taskErr = mergeReliableTaskSubmitResult(existing, same)
	require.Nil(t, taskErr)
	assert.Same(t, existing, merged)
	require.NotNil(t, merged.HTTPResponse)
	assert.Equal(t, http.StatusOK, merged.HTTPResponse.StatusCode)

	different := &relay.TaskSubmitResult{UpstreamTaskID: "UPSTREAM_B"}
	merged, taskErr = mergeReliableTaskSubmitResult(existing, different)
	require.NotNil(t, taskErr)
	assert.Same(t, existing, merged)
	assert.Equal(t, "UPSTREAM_A", merged.UpstreamTaskID)
	assert.NotContains(t, taskErr.Message, "UPSTREAM_A")
	assert.NotContains(t, taskErr.Message, "UPSTREAM_B")
}

func TestReliablePartialFillsOnlyEmptyIdentity(t *testing.T) {
	incoming := &relay.TaskSubmitResult{
		UpstreamTaskID: "UPSTREAM_A",
		TaskData:       []byte(`{"safe":"a"}`),
	}
	merged, taskErr := mergeReliableTaskSubmitResult(nil, incoming)
	require.Nil(t, taskErr)
	assert.Same(t, incoming, merged)
}

func TestShouldRetryTaskUsesExplicitRetryableAfterGlobalGuards(t *testing.T) {
	retryable := true
	nonRetryable := false
	tests := []struct {
		name       string
		taskErr    *dto.TaskError
		retryTimes int
		specific   bool
		want       bool
	}{
		{
			name: "explicit false overrides 429",
			taskErr: &dto.TaskError{
				StatusCode: http.StatusTooManyRequests,
				Retryable:  &nonRetryable,
			},
			retryTimes: 1,
		},
		{
			name: "explicit true allows safe retry",
			taskErr: &dto.TaskError{
				StatusCode: http.StatusBadRequest,
				Retryable:  &retryable,
			},
			retryTimes: 1,
			want:       true,
		},
		{
			name: "nil keeps legacy status rule",
			taskErr: &dto.TaskError{
				StatusCode: http.StatusTooManyRequests,
			},
			retryTimes: 1,
			want:       true,
		},
		{
			name: "retry budget wins over explicit true",
			taskErr: &dto.TaskError{
				StatusCode: http.StatusTooManyRequests,
				Retryable:  &retryable,
			},
			retryTimes: 0,
		},
		{
			name: "specific channel wins over explicit true",
			taskErr: &dto.TaskError{
				StatusCode: http.StatusTooManyRequests,
				Retryable:  &retryable,
			},
			retryTimes: 1,
			specific:   true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := taskSubmissionControllerContext()
			if test.specific {
				c.Set("specific_channel_id", 59)
			}
			assert.Equal(t, test.want, shouldRetryTaskRelay(c, 59, test.taskErr, test.retryTimes))
		})
	}
}

func TestTaskErrorNeverSerializesReliablePartial(t *testing.T) {
	taskErr := &dto.TaskError{
		Code:       "local_failure",
		Message:    "local failure",
		StatusCode: http.StatusInternalServerError,
		Data:       nil,
	}
	encoded, err := json.Marshal(taskErr)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "UPSTREAM_")
	assert.JSONEq(t, `{"code":"local_failure","message":"local failure","data":null}`, string(encoded))
}

func TestReliablePartialProviderSentinelsNeverReachHTTPOrLogs(t *testing.T) {
	setupControllerTaskSubmissionDB(t)
	configureControllerTaskSubmissionPrice(t)
	seedControllerTaskBillingSubjects(t, 8301, 9301)
	const providerMessage = "UPSTREAM_SECRET_ID PROMPT_SENTINEL MEDIA_SENTINEL"
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		assert.Equal(t, "/generate", request.URL.Path)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"task_id":"UPSTREAM_SECRET_ID",
			"success":false,
			"status":"failed",
			"errCode":"PROVIDER_FAILURE",
			"errMessage":"UPSTREAM_SECRET_ID PROMPT_SENTINEL MEDIA_SENTINEL",
			"prompt":"PROMPT_SENTINEL",
			"image_base64":"MEDIA_SENTINEL"
		}`))
	}))
	defer upstream.Close()

	var logBuffer bytes.Buffer
	common.LogWriterMu.Lock()
	oldErrorWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logBuffer
	common.LogWriterMu.Unlock()
	oldErrorLogEnabled := constant.ErrorLogEnabled
	constant.ErrorLogEnabled = true
	t.Cleanup(func() {
		constant.ErrorLogEnabled = oldErrorLogEnabled
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = oldErrorWriter
		common.LogWriterMu.Unlock()
	})

	recorder := httptest.NewRecorder()
	c := newSeedDanceControllerRelayContext(
		recorder,
		upstream.URL,
		`{
			"model":"seedance-uncensored",
			"prompt":"PROMPT_SENTINEL",
			"duration":10,
			"size":"1920x1080"
		}`,
	)
	c.Set(common.RequestIdKey, "controller-sanitized-boundary")
	info := newSeedDanceControllerRelayInfo("controller-sanitized-boundary")
	result, taskErr := relay.RelayTaskSubmit(c, info)
	require.NotNil(t, result, "taskErr=%+v", taskErr)
	assert.Equal(t, "UPSTREAM_SECRET_ID", result.UpstreamTaskID)
	require.NotNil(t, taskErr)
	assert.Equal(t, "upstream task submission failed after task creation", taskErr.Message)
	require.Error(t, taskErr.Error)
	assert.Equal(t, taskErr.Message, taskErr.Error.Error())
	assert.Nil(t, taskErr.Data)

	processChannelError(
		c,
		*types.NewChannelError(
			59,
			constant.ChannelTypeSeedDance,
			"seedance-fixture",
			false,
			"SECRET_ROUTE_KEY",
			false,
		),
		types.NewOpenAIError(
			taskErr.Error,
			types.ErrorCodeBadResponseStatusCode,
			taskErr.StatusCode,
		),
	)
	respondTaskError(c, taskErr)

	var errorLogs []model.Log
	require.NoError(t, model.DB.
		Where("type = ?", model.LogTypeError).
		Find(&errorLogs).Error)
	require.Len(t, errorLogs, 1)
	sinks := recorder.Body.String() + "\n" + logBuffer.String()
	for _, entry := range errorLogs {
		sinks += "\n" + entry.Content + "\n" + entry.Other
	}
	for _, sentinel := range strings.Fields(providerMessage) {
		assert.NotContains(t, sinks, sentinel)
	}
	assert.NotContains(t, sinks, "SECRET_ROUTE_KEY")
	assert.JSONEq(t, `{
		"code":"upstream_error",
		"message":"upstream task submission failed after task creation",
		"data":null
	}`, recorder.Body.String())
}

func TestDurableSubmissionExactEndToEndEventOrder(t *testing.T) {
	setupControllerTaskSubmissionDB(t)
	common.BatchUpdateEnabled = true
	configureControllerTaskSubmissionPrice(t)
	seedControllerTaskBillingSubjects(t, 8301, 9301)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "/generate", request.URL.Path)
		attempt, err := model.GetTaskBillingAttemptByRequestID("controller-exact-order")
		assert.NoError(t, err)
		if err == nil {
			assert.Equal(t, model.TaskBillingOwnerTask, attempt.Owner)
			assert.NotZero(t, attempt.FundingConsumedAt)
			assert.NotZero(t, attempt.TokenConsumedAt)
			assert.NotZero(t, attempt.PreconsumeCompletedAt)
			assert.NotNil(t, attempt.TaskID)
		}
		var user model.User
		assert.NoError(t, model.DB.First(&user, 8301).Error)
		assert.Less(t, user.Quota, 1_000_000)
		var token model.Token
		assert.NoError(t, model.DB.First(&token, 9301).Error)
		assert.Less(t, token.RemainQuota, 1_000_000)
		assert.Greater(t, token.UsedQuota, 0)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"task_id":"UPSTREAM_ORDER",
			"success":true,
			"status":"submitted"
		}`))
	}))
	defer upstream.Close()

	recorder := httptest.NewRecorder()
	c := newSeedDanceControllerRelayContext(
		recorder,
		upstream.URL,
		`{
			"model":"seedance-uncensored",
			"prompt":"flower",
			"duration":10,
			"size":"1920x1080"
		}`,
	)
	c.Set(common.RequestIdKey, "controller-exact-order")
	var events []string
	relay.SetTaskSubmissionEventObserver(c, func(event string) {
		events = append(events, event)
	})
	info := newSeedDanceControllerRelayInfo("controller-exact-order")

	result, taskErr := relay.RelayTaskSubmit(c, info)
	require.Nil(t, taskErr)
	require.NotNil(t, result)
	require.NotNil(t, result.HTTPResponse)
	taskErr = finalizeDurableTaskSubmission(c, info, result)
	require.Nil(t, taskErr)

	assert.Equal(t, []string{
		"validate_full_prepaid_shape",
		"begin_billing_attempt_owner_request",
		"sync_funding_preconsume_and_marker",
		"sync_token_preconsume_and_marker",
		"primary_db_verify_preconsume",
		"validate_full_prepaid_before_build",
		"build_body",
		"insert_provisional_link_attempt_transfer_owner",
		"post_generate",
		"attach_upstream_id",
		"build_public_response",
		"mark_submitted",
		"validate_full_prepaid_again",
		"settle_zero_delta",
		"mark_billing_attempt_submission_settled",
		"consume_log",
		"write_http_200",
	}, events)
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), "UPSTREAM_ORDER")

	attempt, err := model.GetTaskBillingAttemptByRequestID(info.BillingAttemptRequestID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskBillingOwnerTask, attempt.Owner)
	assert.NotZero(t, attempt.SubmissionSettledAt)
	assert.Zero(t, attempt.SucceededAt)
	assert.Zero(t, attempt.RefundCompletedAt)
	var task model.Task
	require.NoError(t, model.DB.First(&task, info.PersistentTaskID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSubmitted), task.Status)
	assert.Equal(t, "10%", task.Progress)
	assert.Equal(t, "UPSTREAM_ORDER", task.PrivateData.UpstreamTaskID)
	var consumeLogCount int64
	require.NoError(t, model.DB.Model(&model.Log{}).
		Where("request_id = ? AND type = ?", info.BillingAttemptRequestID, model.LogTypeConsume).
		Count(&consumeLogCount).Error)
	assert.Equal(t, int64(1), consumeLogCount)
}

func TestControllerSafe429RetryReusesDurableIdentityAndStopsAfterReliableID(t *testing.T) {
	setupControllerTaskSubmissionDB(t)
	configureControllerTaskSubmissionPrice(t)
	seedControllerTaskBillingSubjects(t, 8301, 9301)
	var postCount atomic.Int32
	var fakeNow atomic.Int64
	const firstSubmitTime = int64(1_750_020_000)
	fakeNow.Store(firstSubmitTime)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		currentPost := postCount.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		if currentPost == 1 {
			fakeNow.Add(3)
			writer.WriteHeader(http.StatusTooManyRequests)
			_, _ = writer.Write([]byte(`{
				"success":false,
				"errCode":"rate_limit",
				"errMessage":"try later",
				"data":{}
			}`))
			return
		}
		_, _ = writer.Write([]byte(`{
			"task_id":"UPSTREAM_RETRY_SUCCESS",
			"success":true,
			"status":"submitted"
		}`))
	}))
	defer upstream.Close()

	recorder := httptest.NewRecorder()
	c := newSeedDanceControllerRelayContext(
		recorder,
		upstream.URL,
		`{
			"model":"seedance-uncensored",
			"prompt":"flower",
			"duration":10,
			"size":"1920x1080"
		}`,
	)
	c.Set(common.RequestIdKey, "controller-safe-429-retry")
	relay.SetTaskSubmissionNow(c, func() time.Time {
		return time.Unix(fakeNow.Load(), 0)
	})

	baseURL := upstream.URL
	autoBan := 0
	fixtureChannel := &model.Channel{
		Id:       59,
		Type:     constant.ChannelTypeSeedDance,
		Key:      "SECRET_ROUTE_KEY",
		Name:     "seedance-fixture",
		BaseURL:  &baseURL,
		AutoBan:  &autoBan,
		Status:   common.ChannelStatusEnabled,
		Models:   "seedance-uncensored",
		Group:    "default",
		Priority: common.GetPointer(int64(0)),
	}
	oldGetTaskRelayChannel := getTaskRelayChannel
	getTaskRelayChannel = func(
		context *gin.Context,
		_ *relaycommon.RelayInfo,
		_ *service.RetryParam,
	) (*model.Channel, *types.NewAPIError) {
		context.Set(string(constant.ContextKeyChannelId), fixtureChannel.Id)
		context.Set(string(constant.ContextKeyChannelName), fixtureChannel.Name)
		context.Set(string(constant.ContextKeyChannelType), fixtureChannel.Type)
		context.Set(string(constant.ContextKeyChannelKey), fixtureChannel.Key)
		context.Set(string(constant.ContextKeyChannelBaseUrl), *fixtureChannel.BaseURL)
		context.Set(string(constant.ContextKeyChannelAutoBan), false)
		return fixtureChannel, nil
	}
	t.Cleanup(func() { getTaskRelayChannel = oldGetTaskRelayChannel })
	oldRetryTimes := common.RetryTimes
	common.RetryTimes = 3
	t.Cleanup(func() { common.RetryTimes = oldRetryTimes })

	RelayTask(c)

	assert.Equal(t, int32(2), postCount.Load())
	assert.Equal(t, firstSubmitTime+3, fakeNow.Load())
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), "UPSTREAM_RETRY_SUCCESS")
	var attempts []model.TaskBillingAttempt
	require.NoError(t, model.DB.Find(&attempts).Error)
	require.Len(t, attempts, 1)
	assert.Equal(t, "controller-safe-429-retry", attempts[0].RequestID)
	assert.Equal(t, firstSubmitTime, attempts[0].SubmitTime)
	assert.Equal(t, model.TaskBillingOwnerTask, attempts[0].Owner)
	require.NotNil(t, attempts[0].TaskID)
	assert.NotZero(t, attempts[0].SubmissionSettledAt)
	assert.Zero(t, attempts[0].SucceededAt)
	var tasks []model.Task
	require.NoError(t, model.DB.Find(&tasks).Error)
	require.Len(t, tasks, 1)
	assert.Equal(t, *attempts[0].TaskID, tasks[0].ID)
	assert.Equal(t, attempts[0].PublicTaskID, tasks[0].TaskID)
	assert.Equal(t, firstSubmitTime, tasks[0].SubmitTime)
	assert.Equal(t, "UPSTREAM_RETRY_SUCCESS", tasks[0].PrivateData.UpstreamTaskID)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSubmitted), tasks[0].Status)
}

func TestDeferredSuccessMarksSubmissionSettledBeforeLogAndHTTP(t *testing.T) {
	setupControllerTaskSubmissionDB(t)
	billing := &controllerSubmissionBilling{preConsumed: 25}
	info, result, task := createSubmittedControllerTask(t, "deferred-success", billing)
	require.NoError(t, model.DB.Exec(`
		CREATE TRIGGER require_submission_settled_before_log
		BEFORE INSERT ON logs
		WHEN (
			SELECT submission_settled_at
			FROM task_billing_attempts
			WHERE request_id = NEW.request_id
		) = 0
		BEGIN
			SELECT RAISE(FAIL, 'consumption log before settlement marker');
		END
	`).Error)

	writer := &settlementOrderWriter{requestID: info.BillingAttemptRequestID}
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	c.Set(common.RequestIdKey, info.BillingAttemptRequestID)
	taskErr := finalizeDurableTaskSubmission(c, info, result)
	require.Nil(t, taskErr)
	require.NoError(t, writer.orderErr)
	assert.Equal(t, http.StatusOK, writer.status)
	assert.Equal(t, 1, billing.settleCalls)

	attempt, err := model.GetTaskBillingAttemptByRequestID(info.BillingAttemptRequestID)
	require.NoError(t, err)
	assert.NotZero(t, attempt.SubmissionSettledAt)
	assert.Zero(t, attempt.SucceededAt)
	assert.Zero(t, attempt.RefundStartedAt)
	assert.Zero(t, attempt.RefundCompletedAt)
	reloaded, err := model.GetTaskByPrimaryID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSubmitted), reloaded.Status)
	assert.Equal(t, "10%", reloaded.Progress)
}

func TestZeroDeltaSettleCallsNoBalanceAdjustment(t *testing.T) {
	setupControllerTaskSubmissionDB(t)
	billing := &controllerSubmissionBilling{preConsumed: 25}
	info, result, _ := createSubmittedControllerTask(t, "zero-delta-settle", billing)
	var userBefore model.User
	require.NoError(t, model.DB.Where("id = ?", info.UserId).First(&userBefore).Error)
	var tokenBefore model.Token
	require.NoError(t, model.DB.Where("id = ?", info.TokenId).First(&tokenBefore).Error)

	writer := &settlementOrderWriter{requestID: info.BillingAttemptRequestID}
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	c.Set(common.RequestIdKey, info.BillingAttemptRequestID)
	taskErr := finalizeDurableTaskSubmission(c, info, result)
	require.Nil(t, taskErr)
	require.NoError(t, writer.orderErr)

	var userAfter model.User
	require.NoError(t, model.DB.Where("id = ?", info.UserId).First(&userAfter).Error)
	var tokenAfter model.Token
	require.NoError(t, model.DB.Where("id = ?", info.TokenId).First(&tokenAfter).Error)
	assert.Equal(t, userBefore.Quota, userAfter.Quota)
	assert.Equal(t, tokenBefore.RemainQuota, tokenAfter.RemainQuota)
	assert.Equal(t, tokenBefore.UsedQuota, tokenAfter.UsedQuota)
	assert.Equal(t, 1, billing.settleCalls)
}

func TestSettleFailureTransitionsAndRefundsTaskAttempt(t *testing.T) {
	setupControllerTaskSubmissionDB(t)
	billing := &controllerSubmissionBilling{
		preConsumed: 25,
		settleErr:   errors.New("forced zero-delta settle failure"),
	}
	info, result, task := createSubmittedControllerTask(t, "settle-failure-refund", billing)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	c.Set(common.RequestIdKey, info.BillingAttemptRequestID)

	taskErr := finalizeDurableTaskSubmission(c, info, result)
	require.NotNil(t, taskErr)
	require.NotNil(t, taskErr.Retryable)
	assert.False(t, *taskErr.Retryable)
	assert.False(t, c.Writer.Written())
	assert.Zero(t, recorder.Body.Len())

	failed, err := model.GetTaskByPrimaryID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), failed.Status)
	assert.Equal(t, "100%", failed.Progress)
	assert.Equal(t, "UPSTREAM_DEFERRED", failed.PrivateData.UpstreamTaskID)
	assert.Zero(t, failed.Quota)
	attempt, err := model.GetTaskBillingAttemptByRequestID(info.BillingAttemptRequestID)
	require.NoError(t, err)
	assert.Zero(t, attempt.SubmissionSettledAt)
	assert.Zero(t, attempt.SucceededAt)
	assert.NotZero(t, attempt.FundingRefundedAt)
	assert.NotZero(t, attempt.TokenRefundedAt)
	assert.NotZero(t, attempt.RefundCompletedAt)
	var logCount int64
	require.NoError(t, model.DB.Model(&model.Log{}).
		Where("request_id = ? AND type = ?", info.BillingAttemptRequestID, model.LogTypeConsume).
		Count(&logCount).Error)
	assert.Zero(t, logCount)
}

func TestSubmissionMarkerFailurePreservesReliableIDAndBlocksLogAndHTTP(t *testing.T) {
	setupControllerTaskSubmissionDB(t)
	billing := &controllerSubmissionBilling{preConsumed: 25}
	info, result, task := createSubmittedControllerTask(t, "submission-marker-failure", billing)
	require.NoError(t, model.DB.Exec(`
		CREATE TRIGGER fail_submission_settled_marker
		BEFORE UPDATE OF submission_settled_at ON task_billing_attempts
		WHEN NEW.submission_settled_at != OLD.submission_settled_at
		BEGIN
			SELECT RAISE(FAIL, 'forced submission marker failure');
		END
	`).Error)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	c.Set(common.RequestIdKey, info.BillingAttemptRequestID)

	taskErr := finalizeDurableTaskSubmission(c, info, result)
	require.NotNil(t, taskErr)
	assert.Equal(t, "seedance_billing_settlement_failed", taskErr.Code)
	assert.Equal(t, 1, billing.settleCalls)
	assert.False(t, c.Writer.Written())
	assert.Zero(t, recorder.Body.Len())

	failed, err := model.GetTaskByPrimaryID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), failed.Status)
	assert.Equal(t, "100%", failed.Progress)
	assert.Equal(t, "UPSTREAM_DEFERRED", failed.PrivateData.UpstreamTaskID)
	assert.Zero(t, failed.Quota)
	attempt, err := model.GetTaskBillingAttemptByRequestID(info.BillingAttemptRequestID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskBillingOwnerTask, attempt.Owner)
	assert.Zero(t, attempt.SubmissionSettledAt)
	assert.Zero(t, attempt.SucceededAt)
	assert.NotZero(t, attempt.FundingRefundedAt)
	assert.NotZero(t, attempt.TokenRefundedAt)
	assert.NotZero(t, attempt.RefundCompletedAt)
	var consumeLogCount int64
	require.NoError(t, model.DB.Model(&model.Log{}).
		Where("request_id = ? AND type = ?", info.BillingAttemptRequestID, model.LogTypeConsume).
		Count(&consumeLogCount).Error)
	assert.Zero(t, consumeLogCount)
}

func TestControllerUnreadableDurableOwnerSkipsBareRefundAndRecoversLater(t *testing.T) {
	setupControllerTaskSubmissionDB(t)
	billing := &controllerSubmissionBilling{preConsumed: 25}
	info, result, task := createSubmittedControllerTask(t, "controller-owner-unreadable", billing)
	var userBefore model.User
	require.NoError(t, model.DB.First(&userBefore, info.UserId).Error)
	var tokenBefore model.Token
	require.NoError(t, model.DB.First(&tokenBefore, info.TokenId).Error)
	c := taskSubmissionControllerContext()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	taskErr := &dto.TaskError{
		Code:       "forced_controller_failure",
		Message:    "forced controller failure",
		StatusCode: http.StatusInternalServerError,
		Error:      errors.New("forced controller failure"),
	}

	primaryDB := model.DB
	closedDB, err := gorm.Open(
		sqlite.Open("file:controller-owner-unreadable-closed?mode=memory&cache=shared"),
		&gorm.Config{},
	)
	require.NoError(t, err)
	sqlDB, err := closedDB.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	model.DB = closedDB
	recoveryErr := recoverDurableTaskSubmission(c, info, result, taskErr)
	refundLegacyTaskBillingOnFailure(c, info, taskErr)
	model.DB = primaryDB

	require.Error(t, recoveryErr)
	assert.Equal(t, 0, billing.refundCalls)
	var userAfterFailure model.User
	require.NoError(t, model.DB.First(&userAfterFailure, info.UserId).Error)
	var tokenAfterFailure model.Token
	require.NoError(t, model.DB.First(&tokenAfterFailure, info.TokenId).Error)
	assert.Equal(t, userBefore.Quota, userAfterFailure.Quota)
	assert.Equal(t, tokenBefore.RemainQuota, tokenAfterFailure.RemainQuota)
	attempt, err := model.GetTaskBillingAttemptByRequestID(info.BillingAttemptRequestID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskBillingOwnerTask, attempt.Owner)
	assert.Zero(t, attempt.RefundStartedAt)
	assert.Zero(t, attempt.RefundCompletedAt)

	require.NoError(t, recoverDurableTaskSubmission(c, info, result, taskErr))
	refunded, err := model.GetTaskBillingAttemptByRequestID(info.BillingAttemptRequestID)
	require.NoError(t, err)
	assert.NotZero(t, refunded.FundingRefundedAt)
	assert.NotZero(t, refunded.TokenRefundedAt)
	assert.NotZero(t, refunded.RefundCompletedAt)
	failed, err := model.GetTaskByPrimaryID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), failed.Status)
	assert.Equal(t, "100%", failed.Progress)
	assert.Equal(t, "UPSTREAM_DEFERRED", failed.PrivateData.UpstreamTaskID)
	assert.Zero(t, failed.Quota)
	assert.Equal(t, 0, billing.refundCalls)
}

func createSeedDanceReconciliationFixture(
	t *testing.T,
	requestID string,
	alreadyFailed bool,
) (*model.Task, *model.TaskBillingAttempt) {
	t.Helper()
	const userID = 8401
	const tokenID = 9401
	const quota = 35
	require.NoError(t, model.DB.Create(&model.User{
		Id:       userID,
		Username: "reconciliation-user",
		Quota:    100,
	}).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		Id:          tokenID,
		UserId:      userID,
		Key:         "reconciliation-token-key",
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
		RemainQuota: 100,
	}).Error)
	require.NoError(t, model.DB.Create(&model.Channel{
		Id:     constant.ChannelTypeSeedDance,
		Type:   constant.ChannelTypeSeedDance,
		Key:    "reconciliation-channel-key",
		Name:   "SeedDance reconciliation",
		Status: common.ChannelStatusEnabled,
	}).Error)
	billingContext := &model.TaskBillingContext{
		OriginModelName: "seedance-uncensored",
		GroupRatio:      1,
		PerCallBilling:  true,
	}
	attempt, err := model.BeginTaskBillingAttempt(model.TaskBillingAttemptSnapshot{
		RequestID:      requestID,
		PublicTaskID:   "task_" + requestID,
		SubmitTime:     1_750_004_000,
		UserID:         userID,
		FundingSource:  "wallet",
		FundingAmount:  quota,
		TokenID:        tokenID,
		TokenAmount:    quota,
		BillingContext: billingContext,
	})
	require.NoError(t, err)
	_, err = model.ApplyTaskFundingPreconsume(requestID)
	require.NoError(t, err)
	_, err = model.ApplyTaskTokenPreconsume(requestID)
	require.NoError(t, err)
	task, linked, err := model.PrepareTaskSubmissionAttempt(&model.Task{
		TaskID:     attempt.PublicTaskID,
		Platform:   constant.TaskPlatform("59"),
		UserId:     userID,
		Group:      "default",
		ChannelId:  constant.ChannelTypeSeedDance,
		Quota:      quota,
		Action:     "text2video",
		Status:     model.TaskStatusSubmitting,
		SubmitTime: attempt.SubmitTime,
		Progress:   "0%",
		Properties: model.Properties{OriginModelName: "seedance-uncensored"},
		PrivateData: model.TaskPrivateData{
			Key:            "reconciliation-channel-key",
			BillingSource:  "wallet",
			TokenId:        tokenID,
			NodeName:       "node-reconcile",
			BillingContext: billingContext,
		},
	}, 0, requestID)
	require.NoError(t, err)
	if alreadyFailed {
		task, err = model.TransitionTaskSubmissionToFailure(
			task.ID,
			task.TaskID,
			"",
			"submit_failed",
			"submission failed",
		)
		require.NoError(t, err)
	}
	return task, linked
}

func runSeedDanceReconciliationHandler(
	t *testing.T,
	payload service.SeedDanceSubmitReconciliationPayload,
) *model.SystemTask {
	t.Helper()
	systemTask, err := service.CreateSeedDanceSubmitReconciliation(payload)
	require.NoError(t, err)
	const runnerID = "seedance-reconciliation-test-runner"
	claimed, ok, err := model.ClaimSystemTask(
		systemTask.ID,
		model.SystemTaskTypeSeedDanceSubmitReconciliation,
		runnerID,
		common.GetTimestamp()+60,
	)
	require.NoError(t, err)
	require.True(t, ok)
	seedDanceSubmitReconciliationHandler{}.Run(context.Background(), claimed, runnerID)
	finished, err := model.GetSystemTaskByTaskID(systemTask.TaskID)
	require.NoError(t, err)
	require.NotNil(t, finished)
	return finished
}

func TestSeedDanceReconciliationUsesChannelPrimaryKeyNotTypeCode(t *testing.T) {
	setupControllerTaskSubmissionDB(t)
	task, _ := createSeedDanceReconciliationFixture(t, "reconcile-channel-primary-key", false)

	const channelID = 5901
	require.NoError(t, model.DB.Create(&model.Channel{
		Id:     channelID,
		Type:   constant.ChannelTypeSeedDance,
		Key:    "reconciliation-channel-primary-key",
		Name:   "SeedDance reconciliation primary key",
		Status: common.ChannelStatusEnabled,
	}).Error)
	require.NoError(t, model.DB.Model(&model.Task{}).
		Where("id = ?", task.ID).
		Update("channel_id", channelID).Error)

	finished := runSeedDanceReconciliationHandler(
		t,
		service.SeedDanceSubmitReconciliationPayload{
			PublicTaskID:     task.TaskID,
			UpstreamTaskID:   "UPSTREAM_CHANNEL_PRIMARY_KEY",
			PersistentTaskID: task.ID,
			ChannelID:        channelID,
			NodeName:         "node-reconcile",
			ErrorCode:        "persist_task_submit_result_failed",
			ObservedAt:       1_750_004_006,
		},
	)
	assert.Equal(t, model.SystemTaskStatusSucceeded, finished.Status)

	reloaded, err := model.GetTaskByPrimaryID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, channelID, reloaded.ChannelId)
	assert.Equal(t, "UPSTREAM_CHANNEL_PRIMARY_KEY", reloaded.PrivateData.UpstreamTaskID)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), reloaded.Status)
}

func TestSeedDanceReconciliationHandlerCompletesFailureAndRefund(t *testing.T) {
	setupControllerTaskSubmissionDB(t)
	task, attempt := createSeedDanceReconciliationFixture(t, "reconcile-complete", false)
	finished := runSeedDanceReconciliationHandler(
		t,
		service.SeedDanceSubmitReconciliationPayload{
			PublicTaskID:     task.TaskID,
			UpstreamTaskID:   "UPSTREAM_RECONCILED",
			PersistentTaskID: task.ID,
			ChannelID:        constant.ChannelTypeSeedDance,
			NodeName:         "node-reconcile",
			ErrorCode:        "persist_task_submit_result_failed",
			ObservedAt:       1_750_004_001,
		},
	)
	assert.Equal(t, model.SystemTaskStatusSucceeded, finished.Status)

	reloaded, err := model.GetTaskByPrimaryID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), reloaded.Status)
	assert.Equal(t, "100%", reloaded.Progress)
	assert.Equal(t, "UPSTREAM_RECONCILED", reloaded.PrivateData.UpstreamTaskID)
	assert.Zero(t, reloaded.Quota)
	refunded, err := model.GetTaskBillingAttemptByRequestID(attempt.RequestID)
	require.NoError(t, err)
	assert.NotZero(t, refunded.FundingRefundedAt)
	assert.NotZero(t, refunded.TokenRefundedAt)
	assert.NotZero(t, refunded.RefundCompletedAt)
	assert.Zero(t, refunded.SucceededAt)
}

func TestSeedDanceReconciliationNeverRevivesFailure(t *testing.T) {
	setupControllerTaskSubmissionDB(t)
	task, _ := createSeedDanceReconciliationFixture(t, "reconcile-failure", true)
	finishTime := task.FinishTime
	finished := runSeedDanceReconciliationHandler(
		t,
		service.SeedDanceSubmitReconciliationPayload{
			PublicTaskID:     task.TaskID,
			UpstreamTaskID:   "UPSTREAM_FAILURE_ATTACH",
			PersistentTaskID: task.ID,
			ChannelID:        constant.ChannelTypeSeedDance,
			NodeName:         "node-reconcile",
			ErrorCode:        "persist_task_submit_result_failed",
			ObservedAt:       1_750_004_002,
		},
	)
	assert.Equal(t, model.SystemTaskStatusSucceeded, finished.Status)
	reloaded, err := model.GetTaskByPrimaryID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), reloaded.Status)
	assert.Equal(t, "100%", reloaded.Progress)
	assert.Equal(t, finishTime, reloaded.FinishTime)
	assert.Equal(t, "UPSTREAM_FAILURE_ATTACH", reloaded.PrivateData.UpstreamTaskID)
}

func TestSeedDanceReconciliationIdentityConflictFailsClosed(t *testing.T) {
	setupControllerTaskSubmissionDB(t)
	task, attempt := createSeedDanceReconciliationFixture(t, "reconcile-identity", false)
	require.NoError(t, model.DB.Model(&model.Task{}).
		Where("id = ?", task.ID).
		Update("platform", constant.TaskPlatform("58")).Error)
	finished := runSeedDanceReconciliationHandler(
		t,
		service.SeedDanceSubmitReconciliationPayload{
			PublicTaskID:     task.TaskID,
			UpstreamTaskID:   "UPSTREAM_IDENTITY_CONFLICT",
			PersistentTaskID: task.ID,
			ChannelID:        constant.ChannelTypeSeedDance,
			NodeName:         "node-reconcile",
			ErrorCode:        "persist_task_submit_result_failed",
			ObservedAt:       1_750_004_003,
		},
	)
	assert.Equal(t, model.SystemTaskStatusFailed, finished.Status)
	reloaded, err := model.GetTaskByPrimaryID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSubmitting), reloaded.Status)
	assert.Empty(t, reloaded.PrivateData.UpstreamTaskID)
	unrefunded, err := model.GetTaskBillingAttemptByRequestID(attempt.RequestID)
	require.NoError(t, err)
	assert.Zero(t, unrefunded.RefundStartedAt)
	assert.Zero(t, unrefunded.RefundCompletedAt)
}

func TestSeedDanceReconciliationFinancialShapeConflictDoesNotMutateTask(t *testing.T) {
	setupControllerTaskSubmissionDB(t)
	task, attempt := createSeedDanceReconciliationFixture(t, "reconcile-shape", false)
	require.NoError(t, model.DB.Model(&model.TaskBillingAttempt{}).
		Where("id = ?", attempt.ID).
		Update("is_free", true).Error)
	finished := runSeedDanceReconciliationHandler(
		t,
		service.SeedDanceSubmitReconciliationPayload{
			PublicTaskID:     task.TaskID,
			UpstreamTaskID:   "UPSTREAM_SHAPE_CONFLICT",
			PersistentTaskID: task.ID,
			ChannelID:        constant.ChannelTypeSeedDance,
			NodeName:         "node-reconcile",
			ErrorCode:        "persist_task_submit_result_failed",
			ObservedAt:       1_750_004_005,
		},
	)
	assert.Equal(t, model.SystemTaskStatusFailed, finished.Status)
	reloaded, err := model.GetTaskByPrimaryID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSubmitting), reloaded.Status)
	assert.Empty(t, reloaded.PrivateData.UpstreamTaskID)
	assert.Equal(t, attempt.FundingAmount, reloaded.Quota)
}

func TestSeedDanceReconciliationPartialRefundKeepsFailedSystemTask(t *testing.T) {
	setupControllerTaskSubmissionDB(t)
	task, attempt := createSeedDanceReconciliationFixture(t, "reconcile-partial", false)
	require.NoError(t, model.DB.Model(&model.User{}).
		Where("id = ?", attempt.UserID).
		Update("quota", common.MaxQuota).Error)
	finished := runSeedDanceReconciliationHandler(
		t,
		service.SeedDanceSubmitReconciliationPayload{
			PublicTaskID:     task.TaskID,
			UpstreamTaskID:   "UPSTREAM_PARTIAL_REFUND",
			PersistentTaskID: task.ID,
			ChannelID:        constant.ChannelTypeSeedDance,
			NodeName:         "node-reconcile",
			ErrorCode:        "persist_task_submit_result_failed",
			ObservedAt:       1_750_004_004,
		},
	)
	assert.Equal(t, model.SystemTaskStatusFailed, finished.Status)
	assert.NotEmpty(t, finished.Error)

	reloaded, err := model.GetTaskByPrimaryID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), reloaded.Status)
	assert.Equal(t, "UPSTREAM_PARTIAL_REFUND", reloaded.PrivateData.UpstreamTaskID)
	assert.Equal(t, attempt.FundingAmount, reloaded.Quota)
	partial, err := model.GetTaskBillingAttemptByRequestID(attempt.RequestID)
	require.NoError(t, err)
	assert.Zero(t, partial.FundingRefundedAt)
	assert.NotZero(t, partial.TokenRefundedAt)
	assert.Zero(t, partial.RefundCompletedAt)
}

func TestSeedDanceReconciliationHandlerIsNonScheduled(t *testing.T) {
	var handler service.SystemTaskHandler = seedDanceSubmitReconciliationHandler{}
	assert.Equal(t, model.SystemTaskTypeSeedDanceSubmitReconciliation, handler.Type())
	_, scheduled := handler.(service.ScheduledSystemTaskHandler)
	assert.False(t, scheduled)
}
