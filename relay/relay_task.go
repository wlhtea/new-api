package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type TaskSubmitResult struct {
	UpstreamTaskID string
	TaskData       []byte
	Platform       constant.TaskPlatform
	Quota          int
	HTTPResponse   *channel.TaskSubmitHTTPResponse
	//PerCallPrice   types.PriceData
}

var taskSubmissionNow = time.Now
var getTaskSubmitAdaptor = GetTaskAdaptor
var prepareTaskSubmissionAttempt = model.PrepareTaskSubmissionAttempt

const taskSubmissionEventObserverKey = "task_submission_event_observer"
const taskSubmissionNowKey = "task_submission_now"

// SetTaskSubmissionNow installs a request-local clock for deterministic
// submission retries. Production requests fall back to taskSubmissionNow.
func SetTaskSubmissionNow(c *gin.Context, now func() time.Time) {
	if c == nil {
		return
	}
	c.Set(taskSubmissionNowKey, now)
}

func currentTaskSubmissionTime(c *gin.Context) time.Time {
	if c != nil {
		if value, exists := c.Get(taskSubmissionNowKey); exists {
			if now, ok := value.(func() time.Time); ok && now != nil {
				return now()
			}
		}
	}
	return taskSubmissionNow()
}

// SetTaskSubmissionEventObserver installs a request-local event observer used
// by orchestration tests. Normal requests have no observer and incur no output
// or global state.
func SetTaskSubmissionEventObserver(c *gin.Context, observer func(string)) {
	if c == nil {
		return
	}
	c.Set(taskSubmissionEventObserverKey, observer)
}

// RecordTaskSubmissionEvent records one successful durable-submission
// boundary when a request-local observer is installed.
func RecordTaskSubmissionEvent(c *gin.Context, event string) {
	if c == nil || event == "" {
		return
	}
	value, exists := c.Get(taskSubmissionEventObserverKey)
	if !exists {
		return
	}
	observer, ok := value.(func(string))
	if ok && observer != nil {
		observer(event)
	}
}

func fullPrepaidTaskBillingError() *dto.TaskError {
	retryable := false
	err := errors.New("full-prepaid task billing invariant failed")
	return &dto.TaskError{
		Code:       "seedance_billing_invariant_failed",
		Message:    err.Error(),
		StatusCode: http.StatusInternalServerError,
		Retryable:  &retryable,
		LocalError: true,
		Error:      err,
	}
}

// ValidateFullPrepaidTaskBilling proves that a full-prepaid task is either an
// exact free shape or a paid session whose immutable preconsume amount equals
// the quota about to cross a durable boundary.
func ValidateFullPrepaidTaskBilling(
	info *relaycommon.RelayInfo,
	quota int,
) *dto.TaskError {
	if info == nil {
		return fullPrepaidTaskBillingError()
	}
	if info.PriceData.FreeModel {
		if quota != 0 || info.Billing != nil {
			return fullPrepaidTaskBillingError()
		}
		return nil
	}
	if !info.ForcePreConsume || info.Billing == nil ||
		quota != info.Billing.GetPreConsumedQuota() {
		return fullPrepaidTaskBillingError()
	}
	return nil
}

func taskSubmissionBillingContext(info *relaycommon.RelayInfo) *model.TaskBillingContext {
	if info == nil {
		return nil
	}
	otherRatios := info.PriceData.OtherRatios()
	if otherRatios == nil {
		otherRatios = map[string]float64{}
	}
	return &model.TaskBillingContext{
		ModelPrice:      info.PriceData.ModelPrice,
		GroupRatio:      info.PriceData.GroupRatioInfo.GroupRatio,
		ModelRatio:      info.PriceData.ModelRatio,
		OtherRatios:     otherRatios,
		OriginModelName: info.OriginModelName,
		PerCallBilling: common.StringsContains(
			constant.TaskPricePatches,
			info.OriginModelName,
		) || info.PriceData.UsePrice,
	}
}

func taskBillingAttemptSnapshot(
	info *relaycommon.RelayInfo,
	plan *service.DurableTaskBillingPlan,
	billingContext *model.TaskBillingContext,
) model.TaskBillingAttemptSnapshot {
	if info == nil || plan == nil {
		return model.TaskBillingAttemptSnapshot{}
	}
	requestID := info.RequestId
	publicTaskID := ""
	submitTime := int64(0)
	if info.TaskRelayInfo != nil {
		if info.TaskRelayInfo.BillingAttemptRequestID != "" {
			requestID = info.TaskRelayInfo.BillingAttemptRequestID
		}
		publicTaskID = info.TaskRelayInfo.PublicTaskID
		submitTime = info.TaskRelayInfo.DurableSubmitTime
	}
	return model.TaskBillingAttemptSnapshot{
		RequestID:      requestID,
		PublicTaskID:   publicTaskID,
		SubmitTime:     submitTime,
		IsFree:         plan.IsFree,
		UserID:         info.UserId,
		FundingSource:  plan.FundingSource,
		SubscriptionID: plan.SubscriptionID,
		FundingAmount:  plan.FundingAmount,
		TokenID:        plan.TokenID,
		TokenAmount:    plan.TokenAmount,
		BillingContext: billingContext,
	}
}

func durableTaskLocalError(code string, err error) *dto.TaskError {
	retryable := false
	if err == nil {
		err = errors.New("durable task submission failed")
	}
	return &dto.TaskError{
		Code:       code,
		Message:    err.Error(),
		StatusCode: http.StatusInternalServerError,
		Retryable:  &retryable,
		LocalError: true,
		Error:      err,
	}
}

func beginDurableTaskBilling(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	quota int,
	billingContext *model.TaskBillingContext,
) (*service.DurableTaskBillingPlan, *model.TaskBillingAttempt, *dto.TaskError) {
	if info == nil || info.TaskRelayInfo == nil || info.RequestId == "" ||
		info.PublicTaskID == "" {
		return nil, nil, durableTaskLocalError(
			"seedance_billing_attempt_begin_failed",
			model.ErrTaskBillingIdentityDrift,
		)
	}
	if info.BillingAttemptRequestID == "" {
		info.BillingAttemptRequestID = info.RequestId
	}
	if info.BillingAttemptRequestID != info.RequestId {
		return nil, nil, durableTaskLocalError(
			"seedance_billing_attempt_begin_failed",
			model.ErrTaskBillingIdentityDrift,
		)
	}

	var plan *service.DurableTaskBillingPlan
	existing, lookupErr := model.GetTaskBillingAttemptByRequestID(info.BillingAttemptRequestID)
	switch {
	case lookupErr == nil:
		if existing.PublicTaskID != info.PublicTaskID {
			return nil, nil, durableTaskLocalError(
				"seedance_billing_attempt_begin_failed",
				model.ErrTaskBillingIdentityDrift,
			)
		}
		if info.DurableSubmitTime == 0 {
			info.DurableSubmitTime = existing.SubmitTime
		}
		if info.DurableSubmitTime != existing.SubmitTime {
			return nil, nil, durableTaskLocalError(
				"seedance_billing_attempt_begin_failed",
				model.ErrTaskBillingIdentityDrift,
			)
		}
		if existing.TaskID != nil {
			info.PersistentTaskID = *existing.TaskID
		}
		plan = &service.DurableTaskBillingPlan{
			IsFree:         existing.IsFree,
			FundingSource:  existing.FundingSource,
			SubscriptionID: existing.SubscriptionID,
			FundingAmount:  existing.FundingAmount,
			TokenID:        existing.TokenID,
			TokenAmount:    existing.TokenAmount,
		}
		info.BillingSource = plan.FundingSource
		info.SubscriptionId = plan.SubscriptionID
		if !plan.IsFree {
			info.ForcePreConsume = true
		}
	case errors.Is(lookupErr, gorm.ErrRecordNotFound):
		if info.DurableSubmitTime == 0 {
			info.DurableSubmitTime = currentTaskSubmissionTime(c).Unix()
		}
		session, planned, apiErr := service.NewDurableTaskBillingSession(c, info, quota)
		if apiErr != nil {
			return nil, nil, service.TaskErrorFromAPIError(apiErr)
		}
		plan = planned
		if plan == nil {
			return nil, nil, durableTaskLocalError(
				"seedance_billing_attempt_begin_failed",
				model.ErrTaskBillingIdentityDrift,
			)
		}
		if !plan.IsFree {
			info.ForcePreConsume = true
			info.Billing = session
		}
		info.BillingSource = plan.FundingSource
		info.SubscriptionId = plan.SubscriptionID
	default:
		return nil, nil, durableTaskLocalError(
			"seedance_billing_attempt_owner_unreadable",
			lookupErr,
		)
	}

	if taskErr := ValidateFullPrepaidTaskBilling(info, quota); taskErr != nil {
		return nil, nil, taskErr
	}
	RecordTaskSubmissionEvent(c, "validate_full_prepaid_shape")
	snapshot := taskBillingAttemptSnapshot(info, plan, billingContext)
	attempt, err := model.BeginTaskBillingAttempt(snapshot)
	if err != nil {
		return nil, nil, durableTaskLocalError(
			"seedance_billing_attempt_begin_failed",
			err,
		)
	}
	RecordTaskSubmissionEvent(c, "begin_billing_attempt_owner_request")
	return plan, attempt, nil
}

func applyDurableTaskPreconsume(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	attempt *model.TaskBillingAttempt,
) (*model.TaskBillingAttempt, *dto.TaskError) {
	if info == nil || attempt == nil || info.BillingAttemptRequestID == "" ||
		attempt.RequestID != info.BillingAttemptRequestID {
		return nil, durableTaskLocalError(
			"seedance_billing_preconsume_failed",
			model.ErrTaskBillingIdentityDrift,
		)
	}
	if _, err := model.ApplyTaskFundingPreconsume(attempt.RequestID); err != nil {
		return nil, durableTaskLocalError("seedance_billing_preconsume_failed", err)
	}
	RecordTaskSubmissionEvent(c, "sync_funding_preconsume_and_marker")
	if _, err := model.ApplyTaskTokenPreconsume(attempt.RequestID); err != nil {
		return nil, durableTaskLocalError("seedance_billing_preconsume_failed", err)
	}
	RecordTaskSubmissionEvent(c, "sync_token_preconsume_and_marker")
	verified, err := model.VerifyTaskBillingAttemptPreconsumedForSubmit(attempt.RequestID)
	if err != nil {
		return nil, durableTaskLocalError("seedance_billing_primary_verify_failed", err)
	}
	RecordTaskSubmissionEvent(c, "primary_db_verify_preconsume")
	if !verified.IsFree {
		verifier, ok := info.Billing.(interface {
			VerifyDurableTaskBillingAttempt(string) error
		})
		if !ok {
			return nil, durableTaskLocalError(
				"seedance_billing_primary_verify_failed",
				model.ErrTaskBillingIdentityDrift,
			)
		}
		if err := verifier.VerifyDurableTaskBillingAttempt(attempt.RequestID); err != nil {
			return nil, durableTaskLocalError("seedance_billing_primary_verify_failed", err)
		}
	}
	return verified, nil
}

func newDurableProvisionalTask(
	info *relaycommon.RelayInfo,
	platform constant.TaskPlatform,
	quota int,
	billingContext *model.TaskBillingContext,
) *model.Task {
	if info == nil {
		return nil
	}
	task := model.InitTask(platform, info)
	task.Platform = platform
	task.SubmitTime = info.DurableSubmitTime
	task.Status = model.TaskStatusSubmitting
	task.Progress = "0%"
	task.Quota = quota
	task.Action = info.Action
	task.PrivateData.Key = info.ApiKey
	task.PrivateData.BillingSource = info.BillingSource
	task.PrivateData.SubscriptionId = info.SubscriptionId
	task.PrivateData.TokenId = info.TokenId
	task.PrivateData.NodeName = common.NodeName
	task.PrivateData.BillingContext = billingContext
	task.Properties.OriginModelName = info.OriginModelName
	task.Properties.UpstreamModelName = info.UpstreamModelName
	return task
}

func taskSubmissionContext(c *gin.Context) context.Context {
	if c != nil && c.Request != nil {
		return c.Request.Context()
	}
	return context.Background()
}

func waitTaskSubmissionRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func attachTaskUpstreamResultWithRetry(
	ctx context.Context,
	info *relaycommon.RelayInfo,
	upstreamTaskID string,
	taskData []byte,
) (*model.Task, error) {
	if info == nil || info.PersistentTaskID <= 0 || info.PublicTaskID == "" ||
		upstreamTaskID == "" {
		return nil, model.ErrTaskSubmissionStateConflict
	}
	delays := []time.Duration{0, 25 * time.Millisecond, 100 * time.Millisecond}
	var lastErr error
	for _, delay := range delays {
		if err := waitTaskSubmissionRetry(ctx, delay); err != nil {
			return nil, err
		}
		attached, err := model.AttachTaskUpstreamResult(
			info.PersistentTaskID,
			info.PublicTaskID,
			upstreamTaskID,
			taskData,
		)
		if err == nil {
			return attached, nil
		}
		lastErr = err
		persisted, readErr := model.GetTaskByPrimaryID(info.PersistentTaskID)
		if readErr == nil {
			if persisted.TaskID != info.PublicTaskID {
				return nil, model.ErrTaskSubmissionStateConflict
			}
			if persisted.PrivateData.UpstreamTaskID == upstreamTaskID {
				return persisted, nil
			}
			if persisted.PrivateData.UpstreamTaskID != "" {
				return nil, model.ErrTaskUpstreamIDConflict
			}
		}
		if errors.Is(err, model.ErrTaskUpstreamIDConflict) ||
			errors.Is(err, model.ErrTaskSubmissionStateConflict) {
			return nil, err
		}
	}
	return nil, lastErr
}

func commitTaskSubmissionWithRetry(
	ctx context.Context,
	info *relaycommon.RelayInfo,
) (*model.Task, error) {
	if info == nil || info.PersistentTaskID <= 0 || info.PublicTaskID == "" {
		return nil, model.ErrTaskSubmissionStateConflict
	}
	delays := []time.Duration{0, 25 * time.Millisecond, 100 * time.Millisecond}
	var lastErr error
	for _, delay := range delays {
		if err := waitTaskSubmissionRetry(ctx, delay); err != nil {
			return nil, err
		}
		committed, err := model.CommitTaskSubmission(
			info.PersistentTaskID,
			info.PublicTaskID,
		)
		if err == nil {
			return committed, nil
		}
		lastErr = err
		persisted, readErr := model.GetTaskByPrimaryID(info.PersistentTaskID)
		if readErr == nil {
			if persisted.TaskID != info.PublicTaskID {
				return nil, model.ErrTaskSubmissionStateConflict
			}
			if persisted.Status == model.TaskStatusSubmitted &&
				persisted.Progress == "10%" &&
				persisted.PrivateData.UpstreamTaskID != "" {
				return persisted, nil
			}
			if persisted.Status == model.TaskStatusFailure ||
				persisted.Status == model.TaskStatusSuccess {
				return nil, errors.Join(lastErr, model.ErrTaskSubmissionStateConflict)
			}
		}
		if errors.Is(err, model.ErrTaskSubmissionStateConflict) {
			return nil, err
		}
	}
	return nil, lastErr
}

func nonRetryableTaskPersistenceError(err error) *dto.TaskError {
	retryable := false
	return &dto.TaskError{
		Code:       "persist_task_submit_result_failed",
		Message:    "failed to persist upstream task result",
		StatusCode: http.StatusInternalServerError,
		Retryable:  &retryable,
		LocalError: true,
		Error:      err,
	}
}

const reliableTaskSubmitErrorMessage = "upstream task submission failed after task creation"

// sanitizeReliableTaskSubmitError retains only routing/retry metadata after a
// reliable provider identity exists. Provider-controlled messages, wrapped
// errors, and Data may contain the internal task ID or echoed request media and
// therefore must not cross the relay boundary into logs or client JSON.
func sanitizeReliableTaskSubmitError(taskErr *dto.TaskError) *dto.TaskError {
	if taskErr == nil {
		return nil
	}
	var retryable *bool
	if taskErr.Retryable != nil {
		value := *taskErr.Retryable
		retryable = &value
	}
	return &dto.TaskError{
		Code:       taskErr.Code,
		Message:    reliableTaskSubmitErrorMessage,
		StatusCode: taskErr.StatusCode,
		Retryable:  retryable,
		LocalError: taskErr.LocalError,
		Error:      errors.New(reliableTaskSubmitErrorMessage),
		Data:       nil,
	}
}

func recordSeedDanceSubmitReconciliation(
	info *relaycommon.RelayInfo,
	upstreamTaskID string,
	errorCode string,
) error {
	if info == nil || info.TaskRelayInfo == nil {
		return model.ErrTaskBillingIdentityDrift
	}
	_, err := service.CreateSeedDanceSubmitReconciliation(
		service.SeedDanceSubmitReconciliationPayload{
			PublicTaskID:     info.PublicTaskID,
			UpstreamTaskID:   upstreamTaskID,
			PersistentTaskID: info.PersistentTaskID,
			ChannelID:        info.ChannelId,
			NodeName:         common.NodeName,
			ErrorCode:        errorCode,
		},
	)
	return err
}

func submitDurableTask(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	adaptor channel.TaskAdaptor,
	platform constant.TaskPlatform,
	quota int,
	billingContext *model.TaskBillingContext,
) (*TaskSubmitResult, *dto.TaskError) {
	if info == nil || adaptor == nil {
		return nil, durableTaskLocalError(
			"seedance_durable_submit_failed",
			model.ErrTaskSubmissionStateConflict,
		)
	}
	_, attempt, taskErr := beginDurableTaskBilling(c, info, quota, billingContext)
	if taskErr != nil {
		return nil, taskErr
	}
	if _, taskErr = applyDurableTaskPreconsume(c, info, attempt); taskErr != nil {
		return nil, taskErr
	}
	if taskErr = ValidateFullPrepaidTaskBilling(info, quota); taskErr != nil {
		return nil, taskErr
	}
	RecordTaskSubmissionEvent(c, "validate_full_prepaid_before_build")

	requestBody, err := adaptor.BuildRequestBody(c, info)
	if err != nil {
		return nil, service.TaskErrorWrapper(
			err,
			"build_request_failed",
			http.StatusInternalServerError,
		)
	}
	RecordTaskSubmissionEvent(c, "build_body")
	candidate := newDurableProvisionalTask(info, platform, quota, billingContext)
	prepared, _, err := prepareTaskSubmissionAttempt(
		candidate,
		info.PersistentTaskID,
		info.BillingAttemptRequestID,
	)
	if err != nil {
		return nil, durableTaskLocalError("persist_task_submission_attempt_failed", err)
	}
	info.PersistentTaskID = prepared.ID
	info.DurableSubmitTime = prepared.SubmitTime
	RecordTaskSubmissionEvent(c, "insert_provisional_link_attempt_transfer_owner")
	if _, err := model.VerifyTaskBillingAttemptPreconsumedForSubmit(
		info.BillingAttemptRequestID,
	); err != nil {
		return nil, durableTaskLocalError("seedance_billing_primary_verify_failed", err)
	}

	resp, requestErr := adaptor.DoRequest(c, info, requestBody)
	RecordTaskSubmissionEvent(c, "post_generate")
	if requestErr == nil && resp == nil {
		requestErr = errors.New("upstream returned a nil response")
	}

	var upstreamTaskID string
	var taskData []byte
	needsClassification := requestErr != nil ||
		(resp != nil && resp.StatusCode != http.StatusOK)
	if needsClassification {
		if classifier, ok := adaptor.(channel.TaskSubmitFailureClassifier); ok {
			classified := classifier.ClassifyTaskSubmitFailure(resp, requestErr)
			if classified != nil && classified.TaskError != nil {
				upstreamTaskID = classified.UpstreamTaskID
				taskData = append([]byte(nil), classified.TaskData...)
				taskErr = classified.TaskError
			}
		}
		if taskErr == nil {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if requestErr != nil {
				return nil, service.TaskErrorWrapper(
					requestErr,
					"do_request_failed",
					http.StatusInternalServerError,
				)
			}
			status := http.StatusBadGateway
			if resp != nil {
				status = resp.StatusCode
			}
			return nil, service.TaskErrorWrapper(
				errors.New("upstream task submission failed"),
				"fail_to_fetch_task",
				status,
			)
		}
	} else {
		upstreamTaskID, taskData, taskErr = adaptor.DoResponse(c, resp, info)
	}

	var partial *TaskSubmitResult
	if upstreamTaskID != "" {
		partial = &TaskSubmitResult{
			UpstreamTaskID: upstreamTaskID,
			TaskData:       append([]byte(nil), taskData...),
			Platform:       platform,
			Quota:          quota,
		}
		taskErr = sanitizeReliableTaskSubmitError(taskErr)
		if _, err := attachTaskUpstreamResultWithRetry(
			taskSubmissionContext(c),
			info,
			upstreamTaskID,
			taskData,
		); err != nil {
			if recordErr := recordSeedDanceSubmitReconciliation(
				info,
				upstreamTaskID,
				"persist_task_submit_result_failed",
			); recordErr != nil {
				common.SysError("create Seed Dance submit reconciliation record")
			}
			return partial, nonRetryableTaskPersistenceError(err)
		}
		RecordTaskSubmissionEvent(c, "attach_upstream_id")
	}
	if taskErr != nil {
		return partial, taskErr
	}
	if partial == nil {
		return nil, durableTaskLocalError(
			"seedance_submit_outcome_unknown",
			errors.New("upstream response has no task identity"),
		)
	}

	finalQuota := quota
	if adjustedRatios := adaptor.AdjustBillingOnSubmit(info, taskData); len(adjustedRatios) > 0 {
		if adjustedQuota, ok := recalcQuotaFromRatios(info, adjustedRatios); ok {
			finalQuota = adjustedQuota
			info.PriceData.ReplaceOtherRatios(adjustedRatios)
			info.PriceData.Quota = finalQuota
		}
	}
	partial.Quota = finalQuota
	if taskErr = ValidateFullPrepaidTaskBilling(info, finalQuota); taskErr != nil {
		return partial, taskErr
	}
	responder, ok := adaptor.(channel.DeferredTaskSubmitResponder)
	if !ok {
		return partial, durableTaskLocalError(
			"build_task_submit_response_failed",
			errors.New("durable task adaptor has no deferred responder"),
		)
	}
	httpResponse, err := responder.BuildTaskSubmitResponse(info, taskData)
	if err != nil || httpResponse == nil {
		if err == nil {
			err = errors.New("durable task response is nil")
		}
		return partial, durableTaskLocalError("build_task_submit_response_failed", err)
	}
	partial.HTTPResponse = httpResponse
	RecordTaskSubmissionEvent(c, "build_public_response")
	if _, err := commitTaskSubmissionWithRetry(taskSubmissionContext(c), info); err != nil {
		if recordErr := recordSeedDanceSubmitReconciliation(
			info,
			upstreamTaskID,
			"commit_task_submission_failed",
		); recordErr != nil {
			common.SysError("create Seed Dance submit reconciliation record")
		}
		return partial, nonRetryableTaskPersistenceError(err)
	}
	RecordTaskSubmissionEvent(c, "mark_submitted")
	return partial, nil
}

func isDurableFullPrepaidTaskAdaptor(adaptor channel.TaskAdaptor) bool {
	if adaptor == nil {
		return false
	}
	fullPrepaid, fullPrepaidOK := adaptor.(channel.FullPrepaidTaskSubmitter)
	durable, durableOK := adaptor.(channel.DurableTaskSubmitter)
	_, responderOK := adaptor.(channel.DeferredTaskSubmitResponder)
	return fullPrepaidOK && durableOK && responderOK &&
		fullPrepaid.RequiresFullPrepaidBilling() &&
		durable.RequiresDurableTaskBeforeSubmit()
}

// ResolveOriginTask 处理基于已有任务的提交（remix / continuation）：
// 查找原始任务、从中提取模型名称、将渠道锁定到原始任务的渠道
// （通过 info.LockedChannel，重试时复用同一渠道并轮换 key），
// 以及提取 OtherRatios（时长、分辨率）。
// 该函数在控制器的重试循环之前调用一次，其结果通过 info 字段和上下文持久化。
func ResolveOriginTask(c *gin.Context, info *relaycommon.RelayInfo) *dto.TaskError {
	// 检测 remix action
	path := c.Request.URL.Path
	if strings.Contains(path, "/v1/videos/") && strings.HasSuffix(path, "/remix") {
		info.Action = constant.TaskActionRemix
	}

	// 提取 remix 任务的 video_id
	if info.Action == constant.TaskActionRemix {
		videoID := c.Param("video_id")
		if strings.TrimSpace(videoID) == "" {
			return service.TaskErrorWrapperLocal(fmt.Errorf("video_id is required"), "invalid_request", http.StatusBadRequest)
		}
		info.OriginTaskID = videoID
	}

	if info.OriginTaskID == "" {
		return nil
	}

	// 查找原始任务
	originTask, exist, err := model.GetByTaskId(info.UserId, info.OriginTaskID)
	if err != nil {
		return service.TaskErrorWrapper(err, "get_origin_task_failed", http.StatusInternalServerError)
	}
	if !exist {
		return service.TaskErrorWrapperLocal(errors.New("task_origin_not_exist"), "task_not_exist", http.StatusBadRequest)
	}

	// 从原始任务推导模型名称
	if info.OriginModelName == "" {
		if originTask.Properties.OriginModelName != "" {
			info.OriginModelName = originTask.Properties.OriginModelName
		} else if originTask.Properties.UpstreamModelName != "" {
			info.OriginModelName = originTask.Properties.UpstreamModelName
		} else {
			var taskData map[string]interface{}
			_ = common.Unmarshal(originTask.Data, &taskData)
			if m, ok := taskData["model"].(string); ok && m != "" {
				info.OriginModelName = m
			}
		}
	}

	// 锁定到原始任务的渠道（重试时复用同一渠道，轮换 key）
	ch, err := model.GetChannelById(originTask.ChannelId, true)
	if err != nil {
		return service.TaskErrorWrapperLocal(err, "channel_not_found", http.StatusBadRequest)
	}
	if ch.Status != common.ChannelStatusEnabled {
		return service.TaskErrorWrapperLocal(errors.New("the channel of the origin task is disabled"), "task_channel_disable", http.StatusBadRequest)
	}
	info.LockedChannel = ch

	if originTask.ChannelId != info.ChannelId {
		key, _, newAPIError := ch.GetNextEnabledKey()
		if newAPIError != nil {
			return service.TaskErrorWrapper(newAPIError, "channel_no_available_key", newAPIError.StatusCode)
		}
		common.SetContextKey(c, constant.ContextKeyChannelKey, key)
		common.SetContextKey(c, constant.ContextKeyChannelType, ch.Type)
		common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, ch.GetBaseURL())
		common.SetContextKey(c, constant.ContextKeyChannelId, originTask.ChannelId)

		info.ChannelBaseUrl = ch.GetBaseURL()
		info.ChannelId = originTask.ChannelId
		info.ChannelType = ch.Type
		info.ApiKey = key
	}

	// 提取 remix 参数（时长、分辨率 → OtherRatios）
	if info.Action == constant.TaskActionRemix {
		if originTask.PrivateData.BillingContext != nil {
			// 新的 remix 逻辑：直接从原始任务的 BillingContext 中提取 OtherRatios（如果存在）
			for s, f := range originTask.PrivateData.BillingContext.OtherRatios {
				info.PriceData.AddOtherRatio(s, f)
			}
		} else {
			// 旧的 remix 逻辑：直接从 task data 解析 seconds 和 size（如果存在）
			var taskData map[string]interface{}
			_ = common.Unmarshal(originTask.Data, &taskData)
			secondsStr, _ := taskData["seconds"].(string)
			seconds, _ := strconv.Atoi(secondsStr)
			if seconds <= 0 {
				seconds = 4
			}
			// 历史任务数据可能包含未经校验的时长，作为计费乘数前必须钳制
			if seconds > relaycommon.MaxTaskDurationSeconds {
				seconds = relaycommon.MaxTaskDurationSeconds
			}
			sizeStr, _ := taskData["size"].(string)
			info.PriceData.AddOtherRatio("seconds", float64(seconds))
			info.PriceData.AddOtherRatio("size", 1)
			if sizeStr == "1792x1024" || sizeStr == "1024x1792" {
				info.PriceData.AddOtherRatio("size", 1.666667)
			}
		}
	}

	return nil
}

// RelayTaskSubmit 完成 task 提交的全部流程（每次尝试调用一次）：
// 刷新渠道元数据 → 确定 platform/adaptor → 验证请求 →
// 估算计费(EstimateBilling) → 计算价格 → 预扣费（仅首次）→
// 构建/发送/解析上游请求 → 提交后计费调整(AdjustBillingOnSubmit)。
// 控制器负责 defer Refund 和成功后 Settle。
func RelayTaskSubmit(c *gin.Context, info *relaycommon.RelayInfo) (*TaskSubmitResult, *dto.TaskError) {
	info.InitChannelMeta(c)

	// 1. 确定 platform → 创建适配器 → 验证请求
	platform := constant.TaskPlatform(c.GetString("platform"))
	if platform == "" {
		platform = GetTaskPlatform(c)
	}
	adaptor := getTaskSubmitAdaptor(platform)
	if adaptor == nil {
		return nil, service.TaskErrorWrapperLocal(fmt.Errorf("invalid api platform: %s", platform), "invalid_api_platform", http.StatusBadRequest)
	}
	adaptor.Init(info)
	if taskErr := adaptor.ValidateRequestAndSetAction(c, info); taskErr != nil {
		return nil, taskErr
	}

	// 2. 确定模型名称
	modelName := info.OriginModelName
	if modelName == "" {
		modelName = service.CoverTaskActionToModelName(platform, info.Action)
	}

	// 2.5 应用渠道的模型映射（与同步任务对齐）
	info.OriginModelName = modelName
	info.UpstreamModelName = modelName
	if err := helper.ModelMappedHelper(c, info, nil); err != nil {
		return nil, service.TaskErrorWrapperLocal(err, "model_mapping_failed", http.StatusBadRequest)
	}

	// 3. 预生成公开 task ID（仅首次）
	if info.PublicTaskID == "" {
		info.PublicTaskID = model.GenerateTaskID()
	}

	// 4. 价格计算：基础模型价格
	info.OriginModelName = modelName
	priceData, err := helper.ModelPriceHelperPerCall(c, info)
	if err != nil {
		return nil, service.TaskErrorWrapper(err, "model_price_error", http.StatusBadRequest)
	}
	info.PriceData = priceData

	// 5. 计费估算：让适配器根据用户请求提供 OtherRatios（时长、分辨率等）
	//    必须在 ModelPriceHelperPerCall 之后调用（它会重建 PriceData）。
	//    ResolveOriginTask 可能已在 remix 路径中预设了 OtherRatios，此处合并。
	if estimatedRatios := adaptor.EstimateBilling(c, info); len(estimatedRatios) > 0 {
		for k, v := range estimatedRatios {
			info.PriceData.AddOtherRatio(k, v)
		}
	}

	// 6. 将 OtherRatios 应用到基础额度（饱和转换，防止溢出成负数）
	if !common.StringsContains(constant.TaskPricePatches, modelName) {
		quotaWithRatios := info.PriceData.ApplyOtherRatiosToFloat(float64(info.PriceData.Quota))
		quota, clamp := common.QuotaFromFloatChecked(quotaWithRatios)
		info.PriceData.Quota = quota
		noteTaskQuotaClamp(info, clamp)
	}

	if isDurableFullPrepaidTaskAdaptor(adaptor) {
		return submitDurableTask(
			c,
			info,
			adaptor,
			platform,
			info.PriceData.Quota,
			taskSubmissionBillingContext(info),
		)
	}

	// 7. 预扣费（仅首次 — 重试时 info.Billing 已存在，跳过）
	if info.Billing == nil && !info.PriceData.FreeModel {
		info.ForcePreConsume = true
		if apiErr := service.PreConsumeBilling(c, info.PriceData.Quota, info); apiErr != nil {
			return nil, service.TaskErrorFromAPIError(apiErr)
		}
	}

	// 8. 构建请求体
	requestBody, err := adaptor.BuildRequestBody(c, info)
	if err != nil {
		return nil, service.TaskErrorWrapper(err, "build_request_failed", http.StatusInternalServerError)
	}

	// 9. 发送请求
	resp, err := adaptor.DoRequest(c, info, requestBody)
	if err != nil {
		return nil, service.TaskErrorWrapper(err, "do_request_failed", http.StatusInternalServerError)
	}
	if resp != nil && resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		return nil, service.TaskErrorWrapper(fmt.Errorf("%s", string(responseBody)), "fail_to_fetch_task", resp.StatusCode)
	}

	// 10. 返回 OtherRatios 给下游（header 必须在 DoResponse 写 body 之前设置）
	otherRatios := info.PriceData.OtherRatios()
	if otherRatios == nil {
		otherRatios = map[string]float64{}
	}
	ratiosJSON, _ := common.Marshal(otherRatios)
	c.Header("X-New-Api-Other-Ratios", string(ratiosJSON))

	// 11. 解析响应
	upstreamTaskID, taskData, taskErr := adaptor.DoResponse(c, resp, info)
	if taskErr != nil {
		return nil, taskErr
	}

	// 11. 提交后计费调整：让适配器根据上游实际返回调整 OtherRatios
	finalQuota := info.PriceData.Quota
	if adjustedRatios := adaptor.AdjustBillingOnSubmit(info, taskData); len(adjustedRatios) > 0 {
		if adjustedQuota, ok := recalcQuotaFromRatios(info, adjustedRatios); ok {
			// 基于调整后的 ratios 重新计算 quota
			finalQuota = adjustedQuota
			info.PriceData.ReplaceOtherRatios(adjustedRatios)
			info.PriceData.Quota = finalQuota
		}
	}

	return &TaskSubmitResult{
		UpstreamTaskID: upstreamTaskID,
		TaskData:       taskData,
		Platform:       platform,
		Quota:          finalQuota,
	}, nil
}

// recalcQuotaFromRatios 根据 adjustedRatios 重新计算 quota。
// 公式: baseQuota × ∏(ratio) — 其中 baseQuota 是不含 OtherRatios 的基础额度。
func recalcQuotaFromRatios(info *relaycommon.RelayInfo, ratios map[string]float64) (int, bool) {
	// 从 PriceData 获取不含 OtherRatios 的基础价格
	baseQuota := info.PriceData.RemoveOtherRatiosFromFloat(float64(info.PriceData.Quota))
	priceData := info.PriceData
	if !priceData.ReplaceOtherRatios(ratios) {
		return 0, false
	}
	// 应用新的 ratios
	result := priceData.ApplyOtherRatiosToFloat(baseQuota)
	quota, clamp := common.QuotaFromFloatChecked(result)
	noteTaskQuotaClamp(info, clamp)
	return quota, true
}

// noteTaskQuotaClamp records the first quota saturation event onto the task's
// RelayInfo so LogTaskConsumption can surface it on the submit log's
// admin_info. First non-nil clamp wins.
func noteTaskQuotaClamp(info *relaycommon.RelayInfo, clamp *common.QuotaClamp) {
	if clamp == nil || info == nil {
		return
	}
	if info.QuotaClamp == nil {
		info.QuotaClamp = clamp
	}
}

var fetchRespBuilders = map[int]func(c *gin.Context) (respBody []byte, taskResp *dto.TaskError){
	relayconstant.RelayModeSunoFetchByID:  sunoFetchByIDRespBodyBuilder,
	relayconstant.RelayModeSunoFetch:      sunoFetchRespBodyBuilder,
	relayconstant.RelayModeVideoFetchByID: videoFetchByIDRespBodyBuilder,
}

func RelayTaskFetch(c *gin.Context, relayMode int) (taskResp *dto.TaskError) {
	respBuilder, ok := fetchRespBuilders[relayMode]
	if !ok {
		taskResp = service.TaskErrorWrapperLocal(errors.New("invalid_relay_mode"), "invalid_relay_mode", http.StatusBadRequest)
	}

	respBody, taskErr := respBuilder(c)
	if taskErr != nil {
		return taskErr
	}
	if len(respBody) == 0 {
		respBody = []byte("{\"code\":\"success\",\"data\":null}")
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	_, err := io.Copy(c.Writer, bytes.NewBuffer(respBody))
	if err != nil {
		taskResp = service.TaskErrorWrapper(err, "copy_response_body_failed", http.StatusInternalServerError)
		return
	}
	return
}

func sunoFetchRespBodyBuilder(c *gin.Context) (respBody []byte, taskResp *dto.TaskError) {
	userId := c.GetInt("id")
	var condition = struct {
		IDs    []any  `json:"ids"`
		Action string `json:"action"`
	}{}
	err := c.BindJSON(&condition)
	if err != nil {
		taskResp = service.TaskErrorWrapper(err, "invalid_request", http.StatusBadRequest)
		return
	}
	var tasks []any
	if len(condition.IDs) > 0 {
		taskModels, err := model.GetByTaskIds(userId, condition.IDs)
		if err != nil {
			taskResp = service.TaskErrorWrapper(err, "get_tasks_failed", http.StatusInternalServerError)
			return
		}
		for _, task := range taskModels {
			tasks = append(tasks, TaskModel2Dto(task))
		}
	} else {
		tasks = make([]any, 0)
	}
	respBody, err = common.Marshal(dto.TaskResponse[[]any]{
		Code: "success",
		Data: tasks,
	})
	return
}

func sunoFetchByIDRespBodyBuilder(c *gin.Context) (respBody []byte, taskResp *dto.TaskError) {
	taskId := c.Param("id")
	userId := c.GetInt("id")

	originTask, exist, err := model.GetByTaskId(userId, taskId)
	if err != nil {
		taskResp = service.TaskErrorWrapper(err, "get_task_failed", http.StatusInternalServerError)
		return
	}
	if !exist {
		taskResp = service.TaskErrorWrapperLocal(errors.New("task_not_exist"), "task_not_exist", http.StatusBadRequest)
		return
	}

	respBody, err = common.Marshal(dto.TaskResponse[any]{
		Code: "success",
		Data: TaskModel2Dto(originTask),
	})
	return
}

func videoFetchByIDRespBodyBuilder(c *gin.Context) (respBody []byte, taskResp *dto.TaskError) {
	taskId := c.Param("task_id")
	if taskId == "" {
		taskId = c.GetString("task_id")
	}
	userId := c.GetInt("id")

	originTask, exist, err := model.GetByTaskId(userId, taskId)
	if err != nil {
		taskResp = service.TaskErrorWrapper(err, "get_task_failed", http.StatusInternalServerError)
		return
	}
	if !exist {
		taskResp = service.TaskErrorWrapperLocal(errors.New("task_not_exist"), "task_not_exist", http.StatusBadRequest)
		return
	}

	isOpenAIVideoAPI := strings.HasPrefix(c.Request.RequestURI, "/v1/videos/")

	// Gemini/Vertex 支持实时查询：用户 fetch 时直接从上游拉取最新状态
	if realtimeResp := tryRealtimeFetch(originTask, isOpenAIVideoAPI); len(realtimeResp) > 0 {
		respBody = realtimeResp
		return
	}

	// OpenAI Video API 格式: 走各 adaptor 的 ConvertToOpenAIVideo
	if isOpenAIVideoAPI {
		adaptor := GetTaskAdaptor(originTask.Platform)
		if adaptor == nil {
			taskResp = service.TaskErrorWrapperLocal(fmt.Errorf("invalid channel id: %d", originTask.ChannelId), "invalid_channel_id", http.StatusBadRequest)
			return
		}
		if converter, ok := adaptor.(channel.OpenAIVideoConverter); ok {
			openAIVideoData, err := converter.ConvertToOpenAIVideo(originTask)
			if err != nil {
				taskResp = service.TaskErrorWrapper(err, "convert_to_openai_video_failed", http.StatusInternalServerError)
				return
			}
			respBody = openAIVideoData
			return
		}
		taskResp = service.TaskErrorWrapperLocal(fmt.Errorf("not_implemented:%s", originTask.Platform), "not_implemented", http.StatusNotImplemented)
		return
	}

	// 通用 TaskDto 格式
	respBody, err = common.Marshal(dto.TaskResponse[any]{
		Code: "success",
		Data: TaskModel2Dto(originTask),
	})
	if err != nil {
		taskResp = service.TaskErrorWrapper(err, "marshal_response_failed", http.StatusInternalServerError)
	}
	return
}

// tryRealtimeFetch 尝试从上游实时拉取 Gemini/Vertex 任务状态。
// 仅当渠道类型为 Gemini 或 Vertex 时触发；其他渠道或出错时返回 nil。
// 当非 OpenAI Video API 时，还会构建自定义格式的响应体。
func tryRealtimeFetch(task *model.Task, isOpenAIVideoAPI bool) []byte {
	channelModel, err := model.GetChannelById(task.ChannelId, true)
	if err != nil {
		return nil
	}
	if channelModel.Type != constant.ChannelTypeVertexAi && channelModel.Type != constant.ChannelTypeGemini {
		return nil
	}

	baseURL := constant.ChannelBaseURLs[channelModel.Type]
	if channelModel.GetBaseURL() != "" {
		baseURL = channelModel.GetBaseURL()
	}
	proxy := channelModel.GetSetting().Proxy
	adaptor := GetTaskAdaptor(constant.TaskPlatform(strconv.Itoa(channelModel.Type)))
	if adaptor == nil {
		return nil
	}

	resp, err := adaptor.FetchTask(baseURL, channelModel.Key, map[string]any{
		"task_id": task.GetUpstreamTaskID(),
		"action":  task.Action,
	}, proxy)
	if err != nil || resp == nil {
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	ti, err := adaptor.ParseTaskResult(body)
	if err != nil || ti == nil {
		return nil
	}

	snap := task.Snapshot()

	// 将上游最新状态更新到 task
	if ti.Status != "" {
		task.Status = model.TaskStatus(ti.Status)
	}
	if ti.Progress != "" {
		task.Progress = ti.Progress
	}
	if strings.HasPrefix(ti.Url, "data:") {
		// data: URI — kept in Data, not ResultURL
	} else if ti.Url != "" {
		task.PrivateData.ResultURL = ti.Url
	} else if task.Status == model.TaskStatusSuccess {
		// No URL from adaptor — construct proxy URL using public task ID
		task.PrivateData.ResultURL = taskcommon.BuildProxyURL(task.TaskID)
	}

	if !snap.Equal(task.Snapshot()) {
		_, _ = task.UpdateWithStatus(snap.Status)
	}

	// OpenAI Video API 由调用者的 ConvertToOpenAIVideo 分支处理
	if isOpenAIVideoAPI {
		return nil
	}

	// 非 OpenAI Video API: 构建自定义格式响应
	format := detectVideoFormat(body)
	out := map[string]any{
		"error":    nil,
		"format":   format,
		"metadata": nil,
		"status":   mapTaskStatusToSimple(task.Status),
		"task_id":  task.TaskID,
		"url":      task.GetResultURL(),
	}
	respBody, _ := common.Marshal(dto.TaskResponse[any]{
		Code: "success",
		Data: out,
	})
	return respBody
}

// detectVideoFormat 从 Gemini/Vertex 原始响应中探测视频格式
func detectVideoFormat(rawBody []byte) string {
	var raw map[string]any
	if err := common.Unmarshal(rawBody, &raw); err != nil {
		return "mp4"
	}
	respObj, ok := raw["response"].(map[string]any)
	if !ok {
		return "mp4"
	}
	vids, ok := respObj["videos"].([]any)
	if !ok || len(vids) == 0 {
		return "mp4"
	}
	v0, ok := vids[0].(map[string]any)
	if !ok {
		return "mp4"
	}
	mt, ok := v0["mimeType"].(string)
	if !ok || mt == "" || strings.Contains(mt, "mp4") {
		return "mp4"
	}
	return mt
}

// mapTaskStatusToSimple 将内部 TaskStatus 映射为简化状态字符串
func mapTaskStatusToSimple(status model.TaskStatus) string {
	switch status {
	case model.TaskStatusSuccess:
		return "succeeded"
	case model.TaskStatusFailure:
		return "failed"
	case model.TaskStatusQueued, model.TaskStatusSubmitted:
		return "queued"
	default:
		return "processing"
	}
}

func TaskModel2Dto(task *model.Task) *dto.TaskDto {
	return &dto.TaskDto{
		ID:         task.ID,
		CreatedAt:  task.CreatedAt,
		UpdatedAt:  task.UpdatedAt,
		TaskID:     task.TaskID,
		Platform:   string(task.Platform),
		UserId:     task.UserId,
		Group:      task.Group,
		ChannelId:  task.ChannelId,
		Quota:      task.Quota,
		Action:     task.Action,
		Status:     string(task.Status),
		FailReason: task.FailReason,
		ResultURL:  task.GetResultURL(),
		SubmitTime: task.SubmitTime,
		StartTime:  task.StartTime,
		FinishTime: task.FinishTime,
		Progress:   task.Progress,
		Properties: task.Properties,
		Username:   task.Username,
		Data:       task.Data,
	}
}
