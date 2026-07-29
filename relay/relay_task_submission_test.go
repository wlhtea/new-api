package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	rootcommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/common"
	relaydto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type taskSubmissionTestBilling struct {
	preConsumed int
}

func (b *taskSubmissionTestBilling) Settle(int) error         { return nil }
func (b *taskSubmissionTestBilling) Refund(*gin.Context)      {}
func (b *taskSubmissionTestBilling) NeedsRefund() bool        { return false }
func (b *taskSubmissionTestBilling) GetPreConsumedQuota() int { return b.preConsumed }
func (b *taskSubmissionTestBilling) Reserve(int) error        { return nil }

func setupRelayTaskSubmissionDB(t *testing.T) {
	t.Helper()
	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousBatch := rootcommon.BatchUpdateEnabled
	previousRedis := rootcommon.RedisEnabled
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf(
		"file:relay-task-submission-%d?mode=memory&cache=shared",
		time.Now().UnixNano(),
	)), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	model.DB = db
	model.LOG_DB = db
	rootcommon.SetDatabaseTypes(rootcommon.DatabaseTypeSQLite, rootcommon.DatabaseTypeSQLite)
	rootcommon.RedisEnabled = false
	rootcommon.BatchUpdateEnabled = false
	require.NoError(t, db.AutoMigrate(
		&model.Task{},
		&model.TaskBillingAttempt{},
		&model.User{},
		&model.Token{},
		&model.UserSubscription{},
		&model.SubscriptionPreConsumeRecord{},
		&model.SystemTask{},
		&model.SystemTaskLock{},
	))
	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		rootcommon.BatchUpdateEnabled = previousBatch
		rootcommon.RedisEnabled = previousRedis
		_ = sqlDB.Close()
	})
}

type durableSubmissionAdaptor struct {
	info              *common.RelayInfo
	events            []string
	response          *http.Response
	requestErr        error
	responseID        string
	responseData      []byte
	responseErr       *dto.TaskError
	classification    *channel.TaskSubmitFailureClassification
	publicResponse    *channel.TaskSubmitHTTPResponse
	publicResponseErr error
	postCount         int
	onPost            func(int, *common.RelayInfo)
}

func (a *durableSubmissionAdaptor) Init(info *common.RelayInfo) { a.info = info }
func (a *durableSubmissionAdaptor) ValidateRequestAndSetAction(*gin.Context, *common.RelayInfo) *dto.TaskError {
	return nil
}
func (a *durableSubmissionAdaptor) EstimateBilling(*gin.Context, *common.RelayInfo) map[string]float64 {
	return nil
}
func (a *durableSubmissionAdaptor) AdjustBillingOnSubmit(*common.RelayInfo, []byte) map[string]float64 {
	return nil
}
func (a *durableSubmissionAdaptor) AdjustBillingOnComplete(*model.Task, *common.TaskInfo) int {
	return 0
}
func (a *durableSubmissionAdaptor) BuildRequestURL(*common.RelayInfo) (string, error) {
	return "", nil
}
func (a *durableSubmissionAdaptor) BuildRequestHeader(*gin.Context, *http.Request, *common.RelayInfo) error {
	return nil
}
func (a *durableSubmissionAdaptor) BuildRequestBody(_ *gin.Context, info *common.RelayInfo) (io.Reader, error) {
	attempt, err := model.GetTaskBillingAttemptByRequestID(info.BillingAttemptRequestID)
	if err != nil {
		return nil, err
	}
	if attempt.Owner != model.TaskBillingOwnerRequest ||
		attempt.PreconsumeCompletedAt == 0 ||
		attempt.TaskID != nil {
		return nil, model.ErrTaskBillingAttemptState
	}
	a.events = append(a.events, "build_body")
	return strings.NewReader(`{"safe":"body"}`), nil
}
func (a *durableSubmissionAdaptor) DoRequest(_ *gin.Context, info *common.RelayInfo, _ io.Reader) (*http.Response, error) {
	attempt, err := model.GetTaskBillingAttemptByRequestID(info.BillingAttemptRequestID)
	if err != nil {
		return nil, err
	}
	if attempt.Owner != model.TaskBillingOwnerTask || attempt.TaskID == nil ||
		*attempt.TaskID != info.PersistentTaskID {
		return nil, model.ErrTaskBillingAttemptState
	}
	a.events = append(a.events, "post_generate")
	a.postCount++
	if a.onPost != nil {
		a.onPost(a.postCount, info)
	}
	return a.response, a.requestErr
}
func (a *durableSubmissionAdaptor) DoResponse(*gin.Context, *http.Response, *common.RelayInfo) (string, []byte, *dto.TaskError) {
	a.events = append(a.events, "do_response")
	return a.responseID, append([]byte(nil), a.responseData...), a.responseErr
}
func (a *durableSubmissionAdaptor) GetModelList() []string { return nil }
func (a *durableSubmissionAdaptor) GetChannelName() string { return "durable-test" }
func (a *durableSubmissionAdaptor) FetchTask(string, string, map[string]any, string) (*http.Response, error) {
	return nil, nil
}
func (a *durableSubmissionAdaptor) ParseTaskResult([]byte) (*common.TaskInfo, error) {
	return nil, nil
}
func (a *durableSubmissionAdaptor) RequiresFullPrepaidBilling() bool      { return true }
func (a *durableSubmissionAdaptor) RequiresDurableTaskBeforeSubmit() bool { return true }
func (a *durableSubmissionAdaptor) BuildTaskSubmitResponse(*common.RelayInfo, []byte) (*channel.TaskSubmitHTTPResponse, error) {
	a.events = append(a.events, "build_public_response")
	if a.publicResponse == nil && a.publicResponseErr == nil {
		a.publicResponse = &channel.TaskSubmitHTTPResponse{
			StatusCode:      http.StatusOK,
			Body:            map[string]any{"id": "public"},
			InitialStatus:   model.TaskStatusSubmitted,
			InitialProgress: "10%",
		}
	}
	return a.publicResponse, a.publicResponseErr
}
func (a *durableSubmissionAdaptor) ClassifyTaskSubmitFailure(*http.Response, error) *channel.TaskSubmitFailureClassification {
	a.events = append(a.events, "classify_failure")
	return a.classification
}

var _ channel.TaskAdaptor = (*durableSubmissionAdaptor)(nil)
var _ channel.FullPrepaidTaskSubmitter = (*durableSubmissionAdaptor)(nil)
var _ channel.DurableTaskSubmitter = (*durableSubmissionAdaptor)(nil)
var _ channel.DeferredTaskSubmitResponder = (*durableSubmissionAdaptor)(nil)
var _ channel.TaskSubmitFailureClassifier = (*durableSubmissionAdaptor)(nil)

type durableGateBaseAdaptor struct{}

func (*durableGateBaseAdaptor) Init(*common.RelayInfo) {}
func (*durableGateBaseAdaptor) ValidateRequestAndSetAction(
	*gin.Context,
	*common.RelayInfo,
) *dto.TaskError {
	return nil
}
func (*durableGateBaseAdaptor) BuildRequestURL(*common.RelayInfo) (string, error) {
	return "", nil
}
func (*durableGateBaseAdaptor) BuildRequestHeader(
	*gin.Context,
	*http.Request,
	*common.RelayInfo,
) error {
	return nil
}
func (*durableGateBaseAdaptor) BuildRequestBody(
	*gin.Context,
	*common.RelayInfo,
) (io.Reader, error) {
	return strings.NewReader(`{}`), nil
}
func (*durableGateBaseAdaptor) DoRequest(
	*gin.Context,
	*common.RelayInfo,
	io.Reader,
) (*http.Response, error) {
	return nil, nil
}
func (*durableGateBaseAdaptor) DoResponse(
	*gin.Context,
	*http.Response,
	*common.RelayInfo,
) (string, []byte, *dto.TaskError) {
	return "", nil, nil
}
func (*durableGateBaseAdaptor) GetModelList() []string { return nil }
func (*durableGateBaseAdaptor) GetChannelName() string { return "durable-gate-test" }
func (*durableGateBaseAdaptor) FetchTask(
	string,
	string,
	map[string]any,
	string,
) (*http.Response, error) {
	return nil, nil
}
func (*durableGateBaseAdaptor) ParseTaskResult([]byte) (*common.TaskInfo, error) {
	return nil, nil
}
func (*durableGateBaseAdaptor) EstimateBilling(
	*gin.Context,
	*common.RelayInfo,
) map[string]float64 {
	return nil
}
func (*durableGateBaseAdaptor) AdjustBillingOnSubmit(
	*common.RelayInfo,
	[]byte,
) map[string]float64 {
	return nil
}
func (*durableGateBaseAdaptor) AdjustBillingOnComplete(
	*model.Task,
	*common.TaskInfo,
) int {
	return 0
}

type durableGateMissingFullPrepaid struct{ durableGateBaseAdaptor }

func (*durableGateMissingFullPrepaid) RequiresDurableTaskBeforeSubmit() bool { return true }
func (*durableGateMissingFullPrepaid) BuildTaskSubmitResponse(
	*common.RelayInfo,
	[]byte,
) (*channel.TaskSubmitHTTPResponse, error) {
	return &channel.TaskSubmitHTTPResponse{}, nil
}

type durableGateMissingDurable struct{ durableGateBaseAdaptor }

func (*durableGateMissingDurable) RequiresFullPrepaidBilling() bool { return true }
func (*durableGateMissingDurable) BuildTaskSubmitResponse(
	*common.RelayInfo,
	[]byte,
) (*channel.TaskSubmitHTTPResponse, error) {
	return &channel.TaskSubmitHTTPResponse{}, nil
}

type durableGateMissingResponder struct{ durableGateBaseAdaptor }

func (*durableGateMissingResponder) RequiresFullPrepaidBilling() bool { return true }
func (*durableGateMissingResponder) RequiresDurableTaskBeforeSubmit() bool {
	return true
}

type responderlessCompatibilityAdaptor struct {
	durableGateBaseAdaptor
	postCount       int
	doResponseCount int
}

func (*responderlessCompatibilityAdaptor) RequiresFullPrepaidBilling() bool { return true }
func (*responderlessCompatibilityAdaptor) RequiresDurableTaskBeforeSubmit() bool {
	return true
}
func (*responderlessCompatibilityAdaptor) BuildRequestBody(
	*gin.Context,
	*common.RelayInfo,
) (io.Reader, error) {
	return strings.NewReader(`{"legacy":true}`), nil
}
func (a *responderlessCompatibilityAdaptor) DoRequest(
	*gin.Context,
	*common.RelayInfo,
	io.Reader,
) (*http.Response, error) {
	a.postCount++
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"id":"LEGACY_UPSTREAM"}`)),
	}, nil
}
func (a *responderlessCompatibilityAdaptor) DoResponse(
	*gin.Context,
	*http.Response,
	*common.RelayInfo,
) (string, []byte, *dto.TaskError) {
	a.doResponseCount++
	return "LEGACY_UPSTREAM", []byte(`{"id":"LEGACY_UPSTREAM"}`), nil
}

func TestDurableOptionalInterfacesGateSubmission(t *testing.T) {
	assert.True(t, isDurableFullPrepaidTaskAdaptor(&durableSubmissionAdaptor{}))
	assert.False(t, isDurableFullPrepaidTaskAdaptor(&durableGateMissingFullPrepaid{}))
	assert.False(t, isDurableFullPrepaidTaskAdaptor(&durableGateMissingDurable{}))
	assert.False(t, isDurableFullPrepaidTaskAdaptor(&durableGateMissingResponder{}))
	assert.False(t, isDurableFullPrepaidTaskAdaptor(nil))
}

func TestResponderlessAdaptorUsesLegacySubmissionWithoutDurableRows(t *testing.T) {
	setupRelayTaskSubmissionDB(t)
	oldModelPrices := ratio_setting.ModelPrice2JSONString()
	oldGroupRatios := ratio_setting.GroupRatio2JSONString()
	oldFreeModelPreconsume := operation_setting.GetQuotaSetting().EnableFreeModelPreConsume
	operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = false
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"legacy-compat-model":0}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1}`))
	t.Cleanup(func() {
		operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = oldFreeModelPreconsume
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(oldModelPrices))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(oldGroupRatios))
	})

	adaptor := &responderlessCompatibilityAdaptor{}
	oldFactory := getTaskSubmitAdaptor
	getTaskSubmitAdaptor = func(constant.TaskPlatform) channel.TaskAdaptor {
		return adaptor
	}
	t.Cleanup(func() { getTaskSubmitAdaptor = oldFactory })

	c := relayTaskTestContext()
	c.Set("platform", "59")
	c.Set(string(constant.ContextKeyChannelType), constant.ChannelTypeSeedDance)
	c.Set(string(constant.ContextKeyChannelId), 59)
	c.Set(string(constant.ContextKeyOriginalModel), "legacy-compat-model")
	info := &common.RelayInfo{
		RequestId:       "responderless-compatibility",
		OriginModelName: "legacy-compat-model",
		UsingGroup:      "default",
		TaskRelayInfo:   &common.TaskRelayInfo{},
	}

	result, taskErr := RelayTaskSubmit(c, info)
	require.Nil(t, taskErr)
	require.NotNil(t, result)
	assert.Equal(t, "LEGACY_UPSTREAM", result.UpstreamTaskID)
	assert.Equal(t, 1, adaptor.postCount)
	assert.Equal(t, 1, adaptor.doResponseCount)
	assert.Nil(t, info.Billing)
	assert.Empty(t, info.BillingAttemptRequestID)
	assert.Zero(t, info.PersistentTaskID)

	var attemptCount int64
	require.NoError(t, model.DB.Model(&model.TaskBillingAttempt{}).Count(&attemptCount).Error)
	assert.Zero(t, attemptCount)
	var taskCount int64
	require.NoError(t, model.DB.Model(&model.Task{}).Count(&taskCount).Error)
	assert.Zero(t, taskCount)
}

func seedRelayTaskBillingSubjects(t *testing.T, userID, tokenID, quota int) {
	t.Helper()
	require.NoError(t, model.DB.Create(&model.User{
		Id:       userID,
		Username: fmt.Sprintf("relay-user-%d", userID),
		Quota:    quota,
	}).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		Id:          tokenID,
		UserId:      userID,
		Key:         fmt.Sprintf("relay-token-%d", tokenID),
		Status:      rootcommon.TokenStatusEnabled,
		ExpiredTime: -1,
		RemainQuota: quota,
	}).Error)
}

func paidDurableRelayInfo(requestID string, submitTime int64) *common.RelayInfo {
	info := &common.RelayInfo{
		UserId:          7001,
		TokenId:         8001,
		RequestId:       requestID,
		OriginModelName: "seedance-uncensored",
		UsingGroup:      "default",
		UserSetting: relaydto.UserSetting{
			BillingPreference: "wallet_only",
		},
		PriceData: types.PriceData{
			ModelPrice:     0.2,
			ModelRatio:     2,
			UsePrice:       true,
			Quota:          25,
			GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1.5},
		},
		TaskRelayInfo: &common.TaskRelayInfo{
			PublicTaskID:      "task_" + requestID,
			DurableSubmitTime: submitTime,
		},
		ChannelMeta: &common.ChannelMeta{},
	}
	info.PriceData.AddOtherRatio("seconds", 5)
	return info
}

func relayTaskTestContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	return c
}

func TestValidateFullPrepaidTaskBilling(t *testing.T) {
	paidBilling := &taskSubmissionTestBilling{preConsumed: 21}
	tests := []struct {
		name    string
		info    *common.RelayInfo
		quota   int
		wantErr bool
	}{
		{name: "nil info", wantErr: true},
		{
			name: "free exact shape",
			info: &common.RelayInfo{PriceData: types.PriceData{FreeModel: true}},
		},
		{
			name:    "free nonzero quota",
			info:    &common.RelayInfo{PriceData: types.PriceData{FreeModel: true}},
			quota:   1,
			wantErr: true,
		},
		{
			name: "free billing session",
			info: &common.RelayInfo{
				PriceData: types.PriceData{FreeModel: true},
				Billing:   paidBilling,
			},
			wantErr: true,
		},
		{
			name: "paid without force preconsume",
			info: &common.RelayInfo{
				PriceData: types.PriceData{FreeModel: false},
				Billing:   paidBilling,
			},
			quota:   21,
			wantErr: true,
		},
		{
			name: "paid without billing",
			info: &common.RelayInfo{
				PriceData:       types.PriceData{FreeModel: false},
				ForcePreConsume: true,
			},
			quota:   21,
			wantErr: true,
		},
		{
			name: "paid quota drift",
			info: &common.RelayInfo{
				PriceData:       types.PriceData{FreeModel: false},
				ForcePreConsume: true,
				Billing:         paidBilling,
			},
			quota:   20,
			wantErr: true,
		},
		{
			name: "paid exact shape",
			info: &common.RelayInfo{
				PriceData:       types.PriceData{FreeModel: false},
				ForcePreConsume: true,
				Billing:         paidBilling,
			},
			quota: 21,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			taskErr := ValidateFullPrepaidTaskBilling(test.info, test.quota)
			if !test.wantErr {
				require.Nil(t, taskErr)
				return
			}
			require.NotNil(t, taskErr)
			assert.Equal(t, "seedance_billing_invariant_failed", taskErr.Code)
			assert.Equal(t, 500, taskErr.StatusCode)
			assert.True(t, taskErr.LocalError)
			require.NotNil(t, taskErr.Retryable)
			assert.False(t, *taskErr.Retryable)
		})
	}
}

func TestTaskPricePatchPerCall(t *testing.T) {
	previousPatches := constant.TaskPricePatches
	constant.TaskPricePatches = []string{"patched-task-model"}
	t.Cleanup(func() { constant.TaskPricePatches = previousPatches })

	info := &common.RelayInfo{
		OriginModelName: "patched-task-model",
		PriceData:       types.PriceData{UsePrice: false},
		TaskRelayInfo:   &common.TaskRelayInfo{PublicTaskID: "task_price_patch"},
	}
	billingContext := taskSubmissionBillingContext(info)
	require.NotNil(t, billingContext)
	assert.True(t, billingContext.PerCallBilling)

	info.OriginModelName = "ordinary-model"
	info.PriceData.UsePrice = true
	assert.True(t, taskSubmissionBillingContext(info).PerCallBilling)
}

func TestBillingAttemptSnapshot(t *testing.T) {
	billingContext := &model.TaskBillingContext{
		ModelPrice:      0.2,
		GroupRatio:      1.5,
		ModelRatio:      2,
		OtherRatios:     map[string]float64{"seconds": 5},
		OriginModelName: "seedance-uncensored",
		PerCallBilling:  true,
	}
	tests := []struct {
		name string
		info *common.RelayInfo
		plan *service.DurableTaskBillingPlan
		want model.TaskBillingAttemptSnapshot
	}{
		{
			name: "free preserves token identity",
			info: &common.RelayInfo{
				UserId:        501,
				TokenId:       601,
				RequestId:     "request-free",
				TaskRelayInfo: &common.TaskRelayInfo{PublicTaskID: "task_free", DurableSubmitTime: 101},
			},
			plan: &service.DurableTaskBillingPlan{
				IsFree:        true,
				FundingAmount: 0,
				TokenID:       601,
				TokenAmount:   0,
			},
			want: model.TaskBillingAttemptSnapshot{
				RequestID:      "request-free",
				PublicTaskID:   "task_free",
				SubmitTime:     101,
				IsFree:         true,
				UserID:         501,
				TokenID:        601,
				BillingContext: billingContext,
			},
		},
		{
			name: "paid zero preserves original subscription source",
			info: &common.RelayInfo{
				UserId:        502,
				TokenId:       602,
				RequestId:     "request-paid-zero",
				TaskRelayInfo: &common.TaskRelayInfo{PublicTaskID: "task_paid_zero", DurableSubmitTime: 102},
			},
			plan: &service.DurableTaskBillingPlan{
				IsFree:         false,
				FundingSource:  service.BillingSourceSubscription,
				SubscriptionID: 77,
				FundingAmount:  0,
				TokenID:        602,
				TokenAmount:    0,
			},
			want: model.TaskBillingAttemptSnapshot{
				RequestID:      "request-paid-zero",
				PublicTaskID:   "task_paid_zero",
				SubmitTime:     102,
				IsFree:         false,
				UserID:         502,
				FundingSource:  service.BillingSourceSubscription,
				SubscriptionID: 77,
				TokenID:        602,
				BillingContext: billingContext,
			},
		},
		{
			name: "paid positive copies exact immutable identity",
			info: &common.RelayInfo{
				UserId:        503,
				TokenId:       603,
				RequestId:     "request-paid",
				TaskRelayInfo: &common.TaskRelayInfo{PublicTaskID: "task_paid", DurableSubmitTime: 103},
			},
			plan: &service.DurableTaskBillingPlan{
				IsFree:         false,
				FundingSource:  service.BillingSourceWallet,
				SubscriptionID: 0,
				FundingAmount:  29,
				TokenID:        603,
				TokenAmount:    29,
			},
			want: model.TaskBillingAttemptSnapshot{
				RequestID:      "request-paid",
				PublicTaskID:   "task_paid",
				SubmitTime:     103,
				IsFree:         false,
				UserID:         503,
				FundingSource:  service.BillingSourceWallet,
				FundingAmount:  29,
				TokenID:        603,
				TokenAmount:    29,
				BillingContext: billingContext,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := taskBillingAttemptSnapshot(test.info, test.plan, billingContext)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestDurableAttemptExistsBeforeAnyPreconsume(t *testing.T) {
	setupRelayTaskSubmissionDB(t)
	seedRelayTaskBillingSubjects(t, 7001, 8001, 100)
	info := paidDurableRelayInfo("attempt-before-preconsume", 1_750_000_001)
	billingContext := taskSubmissionBillingContext(info)

	_, attempt, taskErr := beginDurableTaskBilling(
		relayTaskTestContext(),
		info,
		25,
		billingContext,
	)
	require.Nil(t, taskErr)
	require.NotNil(t, attempt)
	assert.Equal(t, model.TaskBillingOwnerRequest, attempt.Owner)
	assert.Zero(t, attempt.FundingConsumedAt)
	assert.Zero(t, attempt.TokenConsumedAt)

	var user model.User
	require.NoError(t, model.DB.Where("id = ?", 7001).First(&user).Error)
	assert.Equal(t, 100, user.Quota)
	var token model.Token
	require.NoError(t, model.DB.Where("id = ?", 8001).First(&token).Error)
	assert.Equal(t, 100, token.RemainQuota)
}

func TestGenerateVerifiesPrimaryDBConsumedMarkersBeforePost(t *testing.T) {
	setupRelayTaskSubmissionDB(t)
	seedRelayTaskBillingSubjects(t, 7001, 8001, 100)
	info := paidDurableRelayInfo("primary-proof", 1_750_000_002)
	billingContext := taskSubmissionBillingContext(info)

	_, attempt, taskErr := beginDurableTaskBilling(
		relayTaskTestContext(),
		info,
		25,
		billingContext,
	)
	require.Nil(t, taskErr)
	verified, taskErr := applyDurableTaskPreconsume(nil, info, attempt)
	require.Nil(t, taskErr)
	require.NotNil(t, verified)
	assert.NotZero(t, verified.FundingConsumedAt)
	assert.NotZero(t, verified.TokenConsumedAt)
	assert.NotZero(t, verified.PreconsumeCompletedAt)

	var user model.User
	require.NoError(t, model.DB.Where("id = ?", 7001).First(&user).Error)
	assert.Equal(t, 75, user.Quota)
	var token model.Token
	require.NoError(t, model.DB.Where("id = ?", 8001).First(&token).Error)
	assert.Equal(t, 75, token.RemainQuota)
	assert.Equal(t, 25, token.UsedQuota)
}

func TestGenerateUsesMainDBWithBatchUpdateEnabled(t *testing.T) {
	setupRelayTaskSubmissionDB(t)
	rootcommon.BatchUpdateEnabled = true
	seedRelayTaskBillingSubjects(t, 7001, 8001, 100)
	info := paidDurableRelayInfo("batch-main-db", 1_750_000_003)
	billingContext := taskSubmissionBillingContext(info)

	_, attempt, taskErr := beginDurableTaskBilling(
		relayTaskTestContext(),
		info,
		25,
		billingContext,
	)
	require.Nil(t, taskErr)
	verified, taskErr := applyDurableTaskPreconsume(nil, info, attempt)
	require.Nil(t, taskErr)
	require.NotNil(t, verified)

	var wallet int
	require.NoError(t, model.DB.Model(&model.User{}).
		Where("id = ?", 7001).
		Select("quota").
		Scan(&wallet).Error)
	assert.Equal(t, 75, wallet)
	var token model.Token
	require.NoError(t, model.DB.Where("id = ?", 8001).First(&token).Error)
	assert.Equal(t, 75, token.RemainQuota)
	assert.NotZero(t, verified.PreconsumeCompletedAt)
}

func TestOwnerTransferAndTaskInsertAreAtomic(t *testing.T) {
	setupRelayTaskSubmissionDB(t)
	seedRelayTaskBillingSubjects(t, 7001, 8001, 100)
	info := paidDurableRelayInfo("atomic-owner-transfer", 1_750_000_004)
	info.ChannelId = 91
	info.ApiKey = "SECRET_ROUTE_KEY"
	billingContext := taskSubmissionBillingContext(info)
	_, attempt, taskErr := beginDurableTaskBilling(
		relayTaskTestContext(),
		info,
		25,
		billingContext,
	)
	require.Nil(t, taskErr)
	_, taskErr = applyDurableTaskPreconsume(nil, info, attempt)
	require.Nil(t, taskErr)

	candidate := newDurableProvisionalTask(
		info,
		constant.TaskPlatform("59"),
		25,
		billingContext,
	)
	prepared, linked, err := model.PrepareTaskSubmissionAttempt(
		candidate,
		0,
		info.BillingAttemptRequestID,
	)
	require.NoError(t, err)
	require.NotNil(t, prepared)
	require.NotNil(t, linked.TaskID)
	assert.Equal(t, prepared.ID, *linked.TaskID)
	assert.Equal(t, model.TaskBillingOwnerTask, linked.Owner)
	assert.Equal(t, model.TaskStatusSubmitting, prepared.Status)
	assert.Equal(t, "0%", prepared.Progress)
	assert.Equal(t, "SECRET_ROUTE_KEY", prepared.PrivateData.Key)
}

func TestPrepareRollbackBlocksProviderPostAndRequestOwnerRefundConverges(t *testing.T) {
	setupRelayTaskSubmissionDB(t)
	seedRelayTaskBillingSubjects(t, 7001, 8001, 100)
	require.NoError(t, model.DB.Exec(`
		CREATE TRIGGER fail_provisional_task_insert
		BEFORE INSERT ON tasks
		BEGIN
			SELECT RAISE(FAIL, 'forced provisional task insert failure');
		END
	`).Error)
	info := paidDurableRelayInfo("prepare-rollback-recovery", 1_750_000_050)
	info.ChannelId = 59
	info.ChannelType = constant.ChannelTypeSeedDance
	info.ApiKey = "SECRET_ROUTE_KEY"
	info.Action = "text2video"
	info.UpstreamModelName = "seedance-uncensored"
	adaptor := &durableSubmissionAdaptor{}

	result, taskErr := submitDurableTask(
		relayTaskTestContext(),
		info,
		adaptor,
		constant.TaskPlatform("59"),
		25,
		taskSubmissionBillingContext(info),
	)
	assert.Nil(t, result)
	require.NotNil(t, taskErr)
	assert.Equal(t, "persist_task_submission_attempt_failed", taskErr.Code)
	assert.Equal(t, 0, adaptor.postCount)

	attempt, err := model.GetTaskBillingAttemptByRequestID(info.BillingAttemptRequestID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskBillingOwnerRequest, attempt.Owner)
	assert.Nil(t, attempt.TaskID)
	assert.NotZero(t, attempt.FundingConsumedAt)
	assert.NotZero(t, attempt.TokenConsumedAt)
	assert.Zero(t, attempt.RefundStartedAt)
	var taskCount int64
	require.NoError(t, model.DB.Model(&model.Task{}).Count(&taskCount).Error)
	assert.Zero(t, taskCount)

	require.NoError(t, service.FailAndRefundTaskSubmission(
		context.Background(),
		0,
		info.BillingAttemptRequestID,
		"",
		nil,
		taskErr.Code,
		"task submission failed",
	))
	refunded, err := model.GetTaskBillingAttemptByRequestID(info.BillingAttemptRequestID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskBillingOwnerRequest, refunded.Owner)
	assert.NotZero(t, refunded.FundingRefundedAt)
	assert.NotZero(t, refunded.TokenRefundedAt)
	assert.NotZero(t, refunded.RefundCompletedAt)
	var user model.User
	require.NoError(t, model.DB.First(&user, 7001).Error)
	assert.Equal(t, 100, user.Quota)
	var token model.Token
	require.NoError(t, model.DB.First(&token, 8001).Error)
	assert.Equal(t, 100, token.RemainQuota)
	assert.Zero(t, token.UsedQuota)
}

func TestStaleRequestAttemptIsLeftForRecoverySweep(t *testing.T) {
	setupRelayTaskSubmissionDB(t)
	seedRelayTaskBillingSubjects(t, 7001, 8001, 100)
	previousFactory := service.GetTaskAdaptorFunc
	service.GetTaskAdaptorFunc = nil
	t.Cleanup(func() { service.GetTaskAdaptorFunc = previousFactory })
	require.NoError(t, model.DB.Exec(`
		CREATE TRIGGER fail_stale_request_provisional_task_insert
		BEFORE INSERT ON tasks
		BEGIN
			SELECT RAISE(FAIL, 'forced provisional task insert failure');
		END
	`).Error)
	info := paidDurableRelayInfo("stale-request-attempt-sweep", 1_750_000_055)
	info.ChannelId = 59
	info.ChannelType = constant.ChannelTypeSeedDance
	info.ApiKey = "SECRET_ROUTE_KEY"
	info.Action = "text2video"
	info.UpstreamModelName = "seedance-uncensored"
	adaptor := &durableSubmissionAdaptor{}

	result, taskErr := submitDurableTask(
		relayTaskTestContext(),
		info,
		adaptor,
		constant.TaskPlatform("59"),
		25,
		taskSubmissionBillingContext(info),
	)
	assert.Nil(t, result)
	require.NotNil(t, taskErr)
	assert.Equal(t, "persist_task_submission_attempt_failed", taskErr.Code)
	assert.Equal(t, 0, adaptor.postCount)
	assert.Zero(t, info.PersistentTaskID)

	attempt, err := model.GetTaskBillingAttemptByRequestID(info.BillingAttemptRequestID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskBillingOwnerRequest, attempt.Owner)
	assert.Nil(t, attempt.TaskID)
	assert.Zero(t, attempt.PrepareVersion)
	assert.Zero(t, attempt.OwnerTransferredAt)
	assert.NotZero(t, attempt.FundingConsumedAt)
	assert.NotZero(t, attempt.TokenConsumedAt)
	assert.NotZero(t, attempt.PreconsumeCompletedAt)
	assert.Zero(t, attempt.RefundStartedAt)
	assert.Zero(t, attempt.FundingRefundedAt)
	assert.Zero(t, attempt.TokenRefundedAt)
	assert.Zero(t, attempt.RefundCompletedAt)
	assert.Zero(t, attempt.SubmissionSettledAt)
	assert.Zero(t, attempt.SucceededAt)
	assert.False(t, model.HasTaskPollingWork())

	var taskCount int64
	require.NoError(t, model.DB.Model(&model.Task{}).Count(&taskCount).Error)
	assert.Zero(t, taskCount)
	var userBeforeSweep model.User
	require.NoError(t, model.DB.First(&userBeforeSweep, 7001).Error)
	assert.Equal(t, 75, userBeforeSweep.Quota)
	var tokenBeforeSweep model.Token
	require.NoError(t, model.DB.First(&tokenBeforeSweep, 8001).Error)
	assert.Equal(t, 75, tokenBeforeSweep.RemainQuota)
	assert.Equal(t, 25, tokenBeforeSweep.UsedQuota)

	require.NoError(t, model.DB.Model(&model.TaskBillingAttempt{}).
		Where("id = ?", attempt.ID).
		UpdateColumn("updated_at", time.Now().Add(-time.Minute).Unix()).Error)
	assert.True(t, model.HasTaskPollingWork())

	service.RunTaskPollingOnce(context.Background(), nil)

	recovered, err := model.GetTaskBillingAttemptByRequestID(info.BillingAttemptRequestID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskBillingOwnerRequest, recovered.Owner)
	assert.Nil(t, recovered.TaskID)
	assert.NotZero(t, recovered.RefundStartedAt)
	assert.NotZero(t, recovered.FundingRefundedAt)
	assert.NotZero(t, recovered.TokenRefundedAt)
	assert.NotZero(t, recovered.RefundCompletedAt)
	assert.Zero(t, recovered.SucceededAt)
	assert.False(t, model.HasTaskPollingWork())
	require.NoError(t, model.DB.Model(&model.Task{}).Count(&taskCount).Error)
	assert.Zero(t, taskCount)
	var userAfterSweep model.User
	require.NoError(t, model.DB.First(&userAfterSweep, 7001).Error)
	assert.Equal(t, 100, userAfterSweep.Quota)
	var tokenAfterSweep model.Token
	require.NoError(t, model.DB.First(&tokenAfterSweep, 8001).Error)
	assert.Equal(t, 100, tokenAfterSweep.RemainQuota)
	assert.Zero(t, tokenAfterSweep.UsedQuota)
	assert.Equal(t, 0, adaptor.postCount)

	firstRefundMarkers := [4]int64{
		recovered.RefundStartedAt,
		recovered.FundingRefundedAt,
		recovered.TokenRefundedAt,
		recovered.RefundCompletedAt,
	}
	service.RunTaskPollingOnce(context.Background(), nil)

	recoveredAgain, err := model.GetTaskBillingAttemptByRequestID(info.BillingAttemptRequestID)
	require.NoError(t, err)
	assert.Equal(t, firstRefundMarkers, [4]int64{
		recoveredAgain.RefundStartedAt,
		recoveredAgain.FundingRefundedAt,
		recoveredAgain.TokenRefundedAt,
		recoveredAgain.RefundCompletedAt,
	})
	require.NoError(t, model.DB.First(&userAfterSweep, 7001).Error)
	assert.Equal(t, 100, userAfterSweep.Quota)
	require.NoError(t, model.DB.First(&tokenAfterSweep, 8001).Error)
	assert.Equal(t, 100, tokenAfterSweep.RemainQuota)
	assert.Zero(t, tokenAfterSweep.UsedQuota)
	assert.Equal(t, 0, adaptor.postCount)
}

func TestAmbiguousPrepareAPIErrorReadsTaskOwnerAndRefundsLinkedTask(t *testing.T) {
	setupRelayTaskSubmissionDB(t)
	seedRelayTaskBillingSubjects(t, 7001, 8001, 100)
	info := paidDurableRelayInfo("prepare-committed-api-error", 1_750_000_060)
	info.ChannelId = 59
	info.ChannelType = constant.ChannelTypeSeedDance
	info.ApiKey = "SECRET_ROUTE_KEY"
	info.Action = "text2video"
	info.UpstreamModelName = "seedance-uncensored"
	adaptor := &durableSubmissionAdaptor{}
	oldPrepare := prepareTaskSubmissionAttempt
	prepareTaskSubmissionAttempt = func(
		candidate *model.Task,
		persistentTaskID int64,
		requestID string,
	) (*model.Task, *model.TaskBillingAttempt, error) {
		_, _, err := model.PrepareTaskSubmissionAttempt(
			candidate,
			persistentTaskID,
			requestID,
		)
		require.NoError(t, err)
		return nil, nil, errors.New("ambiguous prepare API error after commit")
	}
	t.Cleanup(func() { prepareTaskSubmissionAttempt = oldPrepare })

	result, taskErr := submitDurableTask(
		relayTaskTestContext(),
		info,
		adaptor,
		constant.TaskPlatform("59"),
		25,
		taskSubmissionBillingContext(info),
	)
	assert.Nil(t, result)
	require.NotNil(t, taskErr)
	assert.Equal(t, "persist_task_submission_attempt_failed", taskErr.Code)
	assert.Equal(t, 0, adaptor.postCount)
	assert.Zero(t, info.PersistentTaskID)

	attempt, err := model.GetTaskBillingAttemptByRequestID(info.BillingAttemptRequestID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskBillingOwnerTask, attempt.Owner)
	require.NotNil(t, attempt.TaskID)
	var tasks []model.Task
	require.NoError(t, model.DB.Find(&tasks).Error)
	require.Len(t, tasks, 1)
	assert.Equal(t, *attempt.TaskID, tasks[0].ID)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSubmitting), tasks[0].Status)

	require.NoError(t, service.FailAndRefundTaskSubmission(
		context.Background(),
		0,
		info.BillingAttemptRequestID,
		"",
		nil,
		taskErr.Code,
		"task submission failed",
	))
	refunded, err := model.GetTaskBillingAttemptByRequestID(info.BillingAttemptRequestID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskBillingOwnerTask, refunded.Owner)
	assert.NotZero(t, refunded.FundingRefundedAt)
	assert.NotZero(t, refunded.TokenRefundedAt)
	assert.NotZero(t, refunded.RefundCompletedAt)
	failed, err := model.GetTaskByPrimaryID(*attempt.TaskID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), failed.Status)
	assert.Equal(t, "100%", failed.Progress)
	assert.Zero(t, failed.Quota)
	var user model.User
	require.NoError(t, model.DB.First(&user, 7001).Error)
	assert.Equal(t, 100, user.Quota)
	var token model.Token
	require.NoError(t, model.DB.First(&token, 8001).Error)
	assert.Equal(t, 100, token.RemainQuota)
	assert.Zero(t, token.UsedQuota)
}

func TestSafe429RetryPreservesFirstSubmitTimeAcrossTwoSeconds(t *testing.T) {
	setupRelayTaskSubmissionDB(t)
	seedRelayTaskBillingSubjects(t, 7001, 8001, 100)
	now := int64(1_750_000_100)
	previousNow := taskSubmissionNow
	taskSubmissionNow = func() time.Time { return time.Unix(now, 0) }
	t.Cleanup(func() { taskSubmissionNow = previousNow })

	info := paidDurableRelayInfo("safe-retry-time", 0)
	billingContext := taskSubmissionBillingContext(info)
	plan, attempt, taskErr := beginDurableTaskBilling(
		relayTaskTestContext(),
		info,
		25,
		billingContext,
	)
	require.Nil(t, taskErr)
	require.NotNil(t, plan)
	firstTime := info.DurableSubmitTime
	assert.Equal(t, now, firstTime)
	_, taskErr = applyDurableTaskPreconsume(nil, info, attempt)
	require.Nil(t, taskErr)
	candidate := newDurableProvisionalTask(info, constant.TaskPlatform("59"), 25, billingContext)
	prepared, _, err := model.PrepareTaskSubmissionAttempt(candidate, 0, info.BillingAttemptRequestID)
	require.NoError(t, err)
	info.PersistentTaskID = prepared.ID

	now += 2
	info.DurableSubmitTime = 0 // simulate process-local retry metadata loss
	_, retryAttempt, taskErr := beginDurableTaskBilling(
		relayTaskTestContext(),
		info,
		25,
		billingContext,
	)
	require.Nil(t, taskErr)
	require.NotNil(t, retryAttempt)
	assert.Equal(t, firstTime, info.DurableSubmitTime)
	assert.Equal(t, firstTime, retryAttempt.SubmitTime)
	assert.Equal(t, prepared.ID, info.PersistentTaskID)
	reverified, taskErr := applyDurableTaskPreconsume(nil, info, retryAttempt)
	require.Nil(t, taskErr)
	require.NotNil(t, reverified)
	assert.Equal(t, model.TaskBillingOwnerTask, reverified.Owner)
	assert.NotZero(t, reverified.PreconsumeCompletedAt)

	retryCandidate := newDurableProvisionalTask(info, constant.TaskPlatform("59"), 25, billingContext)
	retried, _, err := model.PrepareTaskSubmissionAttempt(
		retryCandidate,
		info.PersistentTaskID,
		info.BillingAttemptRequestID,
	)
	require.NoError(t, err)
	assert.Equal(t, firstTime, retried.SubmitTime)
	assert.Equal(t, prepared.TaskID, retried.TaskID)
}

func TestSafe429RetryReprovesPrimarySubjectsBeforeSecondPost(t *testing.T) {
	setupRelayTaskSubmissionDB(t)
	seedRelayTaskBillingSubjects(t, 7001, 8001, 100)
	retryable := true
	adaptor := &durableSubmissionAdaptor{
		response: &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"code":"rate_limit"}`)),
		},
		classification: &channel.TaskSubmitFailureClassification{
			TaskError: &dto.TaskError{
				Code:       "upstream_rate_limit_error",
				Message:    "rate limited",
				StatusCode: http.StatusTooManyRequests,
				Retryable:  &retryable,
				Error:      errors.New("rate limited"),
			},
		},
	}
	adaptor.onPost = func(count int, _ *common.RelayInfo) {
		if count == 1 {
			require.NoError(t, model.DB.Unscoped().Delete(&model.Token{}, 8001).Error)
		}
	}
	info := paidDurableRelayInfo("safe-retry-primary-subject", 1_750_000_200)
	info.ChannelId = 59
	info.ChannelType = constant.ChannelTypeSeedDance
	info.ApiKey = "SECRET_ROUTE_KEY"
	info.Action = "text2video"
	info.UpstreamModelName = "seedance-uncensored"

	firstResult, firstErr := submitDurableTask(
		relayTaskTestContext(),
		info,
		adaptor,
		constant.TaskPlatform("59"),
		25,
		taskSubmissionBillingContext(info),
	)
	assert.Nil(t, firstResult)
	require.NotNil(t, firstErr)
	require.NotNil(t, firstErr.Retryable)
	assert.True(t, *firstErr.Retryable)
	assert.Equal(t, 1, adaptor.postCount)

	secondResult, secondErr := submitDurableTask(
		relayTaskTestContext(),
		info,
		adaptor,
		constant.TaskPlatform("59"),
		25,
		taskSubmissionBillingContext(info),
	)
	assert.Nil(t, secondResult)
	require.NotNil(t, secondErr)
	assert.Equal(t, "seedance_billing_primary_verify_failed", secondErr.Code)
	assert.Equal(t, 1, adaptor.postCount, "deleted Token must stop the second provider POST")

	attempt, err := model.GetTaskBillingAttemptByRequestID(info.BillingAttemptRequestID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskBillingOwnerTask, attempt.Owner)
	assert.NotZero(t, attempt.FundingConsumedAt)
	assert.NotZero(t, attempt.TokenConsumedAt)
	assert.Zero(t, attempt.RefundStartedAt)
	var user model.User
	require.NoError(t, model.DB.First(&user, 7001).Error)
	assert.Equal(t, 75, user.Quota, "relay proof failure must not perform a bare refund")
}

func runDurableSubmissionTest(
	t *testing.T,
	requestID string,
	adaptor *durableSubmissionAdaptor,
) (*TaskSubmitResult, *dto.TaskError, *common.RelayInfo) {
	t.Helper()
	setupRelayTaskSubmissionDB(t)
	seedRelayTaskBillingSubjects(t, 7001, 8001, 100)
	info := paidDurableRelayInfo(requestID, 1_750_001_000)
	info.ChannelId = 59
	info.ChannelType = constant.ChannelTypeSeedDance
	info.ApiKey = "SECRET_ROUTE_KEY"
	info.Action = "text2video"
	info.UpstreamModelName = "seedance-uncensored"
	result, taskErr := submitDurableTask(
		relayTaskTestContext(),
		info,
		adaptor,
		constant.TaskPlatform("59"),
		25,
		taskSubmissionBillingContext(info),
	)
	return result, taskErr, info
}

func TestDurableSubmissionOrderThroughPost(t *testing.T) {
	adaptor := &durableSubmissionAdaptor{
		response:     &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))},
		responseID:   "UPSTREAM_ORDER",
		responseData: []byte(`{"safe":"data"}`),
	}
	result, taskErr, info := runDurableSubmissionTest(t, "durable-order", adaptor)
	require.Nil(t, taskErr)
	require.NotNil(t, result)
	assert.Equal(t, "UPSTREAM_ORDER", result.UpstreamTaskID)
	assert.Equal(t, []string{
		"build_body",
		"post_generate",
		"do_response",
		"build_public_response",
	}, adaptor.events)
	assert.Equal(t, 1, adaptor.postCount)

	task, err := model.GetTaskByPrimaryID(info.PersistentTaskID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSubmitted), task.Status)
	assert.Equal(t, "10%", task.Progress)
	assert.Equal(t, "UPSTREAM_ORDER", task.PrivateData.UpstreamTaskID)
	attempt, err := model.GetTaskBillingAttemptByRequestID(info.BillingAttemptRequestID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskBillingOwnerTask, attempt.Owner)
	assert.Zero(t, attempt.SubmissionSettledAt)
	assert.Zero(t, attempt.SucceededAt)
}

func TestReliablePartialFromClassifierSurvivesTaskError(t *testing.T) {
	retryable := false
	const providerSentinel = "UPSTREAM_SECRET_ID PROMPT_SENTINEL MEDIA_SENTINEL"
	adaptor := &durableSubmissionAdaptor{
		response: &http.Response{
			StatusCode: http.StatusBadGateway,
			Body:       io.NopCloser(strings.NewReader(`{"redacted":"provider"}`)),
		},
		classification: &channel.TaskSubmitFailureClassification{
			TaskError: &dto.TaskError{
				Code:       "classified_failure",
				Message:    "classified failure " + providerSentinel,
				StatusCode: http.StatusBadGateway,
				Retryable:  &retryable,
				Error:      fmt.Errorf("classified failure %s", providerSentinel),
				Data:       map[string]any{"provider": providerSentinel},
			},
			UpstreamTaskID: "UPSTREAM_SECRET_ID",
			TaskData:       []byte(`{"safe":"classified"}`),
		},
	}
	result, taskErr, _ := runDurableSubmissionTest(t, "reliable-classifier", adaptor)
	require.NotNil(t, taskErr)
	require.NotNil(t, result)
	assert.Equal(t, "UPSTREAM_SECRET_ID", result.UpstreamTaskID)
	assert.Equal(t, []byte(`{"safe":"classified"}`), result.TaskData)
	assert.Nil(t, taskErr.Data)
	assert.Equal(t, "upstream task submission failed after task creation", taskErr.Message)
	require.Error(t, taskErr.Error)
	assert.Equal(t, "upstream task submission failed after task creation", taskErr.Error.Error())
	assert.NotContains(t, taskErr.Message, providerSentinel)
	assert.NotContains(t, taskErr.Error.Error(), providerSentinel)
	assert.Equal(t, 1, adaptor.postCount)
}

func TestReliablePartialFromDoResponseSurvivesTaskError(t *testing.T) {
	retryable := false
	const providerSentinel = "UPSTREAM_SECRET_ID PROMPT_SENTINEL MEDIA_SENTINEL"
	adaptor := &durableSubmissionAdaptor{
		response:     &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))},
		responseID:   "UPSTREAM_SECRET_ID",
		responseData: []byte(`{"safe":"response-error"}`),
		responseErr: &dto.TaskError{
			Code:       "response_failure",
			Message:    "response failure " + providerSentinel,
			StatusCode: http.StatusBadGateway,
			Retryable:  &retryable,
			Error:      fmt.Errorf("response failure %s", providerSentinel),
			Data:       map[string]any{"provider": providerSentinel},
		},
	}
	result, taskErr, _ := runDurableSubmissionTest(t, "reliable-response", adaptor)
	require.NotNil(t, taskErr)
	require.NotNil(t, result)
	assert.Equal(t, "UPSTREAM_SECRET_ID", result.UpstreamTaskID)
	assert.Equal(t, []byte(`{"safe":"response-error"}`), result.TaskData)
	assert.Nil(t, taskErr.Data)
	assert.Equal(t, "upstream task submission failed after task creation", taskErr.Message)
	require.Error(t, taskErr.Error)
	assert.Equal(t, "upstream task submission failed after task creation", taskErr.Error.Error())
	assert.NotContains(t, taskErr.Message, providerSentinel)
	assert.NotContains(t, taskErr.Error.Error(), providerSentinel)
	assert.Equal(t, 1, adaptor.postCount)
}

func TestReliablePartialSurvivesAttachPersistenceFailure(t *testing.T) {
	adaptor := &durableSubmissionAdaptor{
		response:     &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))},
		responseID:   "UPSTREAM_ATTACH_FAIL",
		responseData: []byte(`{"safe":"attach"}`),
	}
	setupRelayTaskSubmissionDB(t)
	require.NoError(t, model.DB.Exec(`
		CREATE TRIGGER fail_seedance_attach
		BEFORE UPDATE OF private_data ON tasks
		BEGIN
			SELECT RAISE(FAIL, 'forced attach failure');
		END
	`).Error)
	seedRelayTaskBillingSubjects(t, 7001, 8001, 100)
	info := paidDurableRelayInfo("reliable-attach", 1_750_001_001)
	info.ChannelId = 59
	info.ChannelType = constant.ChannelTypeSeedDance
	info.ApiKey = "SECRET_ROUTE_KEY"
	info.Action = "text2video"
	info.UpstreamModelName = "seedance-uncensored"
	result, taskErr := submitDurableTask(
		relayTaskTestContext(),
		info,
		adaptor,
		constant.TaskPlatform("59"),
		25,
		taskSubmissionBillingContext(info),
	)
	require.NotNil(t, taskErr)
	require.NotNil(t, result)
	assert.Equal(t, "UPSTREAM_ATTACH_FAIL", result.UpstreamTaskID)
	assert.Equal(t, 1, adaptor.postCount)
	active, err := model.GetActiveSystemTaskByActiveKey(
		model.SystemTaskTypeSeedDanceSubmitReconciliation,
		service.SeedDanceSubmitReconciliationActiveKey(info.PublicTaskID),
	)
	require.NoError(t, err)
	require.NotNil(t, active)
	var payload service.SeedDanceSubmitReconciliationPayload
	require.NoError(t, active.DecodePayload(&payload))
	assert.Equal(t, info.PublicTaskID, payload.PublicTaskID)
	assert.Equal(t, "UPSTREAM_ATTACH_FAIL", payload.UpstreamTaskID)
	assert.Equal(t, "persist_task_submit_result_failed", payload.ErrorCode)
}

func TestReliablePartialSurvivesBuildTaskSubmitResponseFailure(t *testing.T) {
	adaptor := &durableSubmissionAdaptor{
		response:          &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))},
		responseID:        "UPSTREAM_BUILD_FAIL",
		responseData:      []byte(`{"safe":"build"}`),
		publicResponseErr: fmt.Errorf("forced response build failure"),
	}
	result, taskErr, _ := runDurableSubmissionTest(t, "reliable-build", adaptor)
	require.NotNil(t, taskErr)
	require.NotNil(t, result)
	assert.Equal(t, "UPSTREAM_BUILD_FAIL", result.UpstreamTaskID)
	assert.Equal(t, 1, adaptor.postCount)
}

func TestReliablePartialSurvivesCommitPersistenceFailure(t *testing.T) {
	adaptor := &durableSubmissionAdaptor{
		response:     &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))},
		responseID:   "UPSTREAM_COMMIT_FAIL",
		responseData: []byte(`{"safe":"commit"}`),
	}
	setupRelayTaskSubmissionDB(t)
	require.NoError(t, model.DB.Exec(`
		CREATE TRIGGER fail_seedance_commit
		BEFORE UPDATE OF status ON tasks
		BEGIN
			SELECT RAISE(FAIL, 'forced commit failure');
		END
	`).Error)
	seedRelayTaskBillingSubjects(t, 7001, 8001, 100)
	info := paidDurableRelayInfo("reliable-commit", 1_750_001_002)
	info.ChannelId = 59
	info.ChannelType = constant.ChannelTypeSeedDance
	info.ApiKey = "SECRET_ROUTE_KEY"
	info.Action = "text2video"
	info.UpstreamModelName = "seedance-uncensored"
	result, taskErr := submitDurableTask(
		relayTaskTestContext(),
		info,
		adaptor,
		constant.TaskPlatform("59"),
		25,
		taskSubmissionBillingContext(info),
	)
	require.NotNil(t, taskErr)
	require.NotNil(t, result)
	assert.Equal(t, "UPSTREAM_COMMIT_FAIL", result.UpstreamTaskID)
	assert.Equal(t, 1, adaptor.postCount)
	active, err := model.GetActiveSystemTaskByActiveKey(
		model.SystemTaskTypeSeedDanceSubmitReconciliation,
		service.SeedDanceSubmitReconciliationActiveKey(info.PublicTaskID),
	)
	require.NoError(t, err)
	require.NotNil(t, active)
	var payload service.SeedDanceSubmitReconciliationPayload
	require.NoError(t, active.DecodePayload(&payload))
	assert.Equal(t, info.PublicTaskID, payload.PublicTaskID)
	assert.Equal(t, "UPSTREAM_COMMIT_FAIL", payload.UpstreamTaskID)
	assert.Equal(t, "commit_task_submission_failed", payload.ErrorCode)
}
