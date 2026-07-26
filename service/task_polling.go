package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/samber/lo"
	"gorm.io/gorm"
)

// TaskPollingAdaptor 定义轮询所需的最小适配器接口，避免 service -> relay 的循环依赖
type TaskPollingAdaptor interface {
	Init(info *relaycommon.RelayInfo)
	FetchTask(baseURL string, key string, body map[string]any, proxy string) (*http.Response, error)
	ParseTaskResult(body []byte) (*relaycommon.TaskInfo, error)
	// AdjustBillingOnComplete 在任务到达终态（成功/失败）时由轮询循环调用。
	// 返回正数触发差额结算（补扣/退还），返回 0 保持预扣费金额不变。
	AdjustBillingOnComplete(task *model.Task, taskResult *relaycommon.TaskInfo) int
}

type TaskFetcherWithContext interface {
	FetchTaskWithContext(
		ctx context.Context,
		baseURL string,
		key string,
		body map[string]any,
		proxy string,
	) (*http.Response, error)
}

// GetTaskAdaptorFunc 由 main 包注入，用于获取指定平台的任务适配器。
// 打破 service -> relay -> relay/channel -> service 的循环依赖。
var GetTaskAdaptorFunc func(platform constant.TaskPlatform) TaskPollingAdaptor

const (
	refundReconciliationLimit       = 100
	refundReconciliationGracePeriod = 30 * time.Second
)

var seedDanceTaskPlatform = constant.TaskPlatform(
	fmt.Sprintf("%d", constant.ChannelTypeSeedDance),
)

type safeTaskPollingError struct {
	message string
	cause   error
}

func (e *safeTaskPollingError) Error() string {
	return e.message
}

func (e *safeTaskPollingError) Unwrap() error {
	return e.cause
}

func newSafeTaskPollingError(message string, cause error) error {
	return &safeTaskPollingError{message: message, cause: cause}
}

func isSeedDancePlatform(platform constant.TaskPlatform) bool {
	return platform == seedDanceTaskPlatform
}

func seedDancePollingIdentity(
	ch *model.Channel,
	task *model.Task,
) (bool, error) {
	if task == nil {
		return false, model.ErrTaskSubmissionStateConflict
	}
	taskIsSeedDance := isSeedDancePlatform(task.Platform)
	channelIsSeedDance := ch != nil && ch.Type == constant.ChannelTypeSeedDance
	if taskIsSeedDance != channelIsSeedDance {
		return true, fmt.Errorf(
			"Seed Dance polling identity mismatch for task %s",
			task.TaskID,
		)
	}
	return taskIsSeedDance, nil
}

func transitionDurablePollingFailure(
	task *model.Task,
	expectedStatus model.TaskStatus,
	expectedProgress string,
	code string,
	message string,
	data *json.RawMessage,
	requireDurable bool,
) (*model.Task, bool, error) {
	if task == nil {
		return nil, false, model.ErrTaskSubmissionStateConflict
	}
	requireDurable = requireDurable || isSeedDancePlatform(task.Platform)
	_, err := model.GetTaskBillingAttemptByTaskID(task.ID)
	switch {
	case err == nil:
		failed, transitionErr := model.TransitionDurableTaskToFailure(
			task.ID,
			task.TaskID,
			model.TaskFailureTransition{
				ExpectedStatus:   expectedStatus,
				ExpectedProgress: expectedProgress,
				Code:             code,
				Message:          message,
				Data:             data,
			},
		)
		return failed, true, transitionErr
	case errors.Is(err, gorm.ErrRecordNotFound):
		if requireDurable {
			return nil, true, fmt.Errorf(
				"durable billing attempt unavailable for task %s",
				task.TaskID,
			)
		}
		return nil, false, nil
	default:
		return nil, true, err
	}
}

// sweepTimedOutTasks 在主轮询之前独立清理超时任务。
// 每次最多处理 100 条，剩余的下个周期继续处理。
// 使用 per-task CAS (UpdateWithStatus) 防止覆盖被正常轮询已推进的任务。
func sweepTimedOutTasks(ctx context.Context) {
	if constant.TaskTimeoutMinutes <= 0 {
		return
	}
	cutoff := time.Now().Unix() - int64(constant.TaskTimeoutMinutes)*60
	tasks := model.GetTimedOutUnfinishedTasks(cutoff, 100)
	if len(tasks) == 0 {
		return
	}

	reason := fmt.Sprintf("任务超时（%d分钟）", constant.TaskTimeoutMinutes)
	legacyReason := "任务超时（旧系统遗留任务，不进行退款，请联系管理员）"
	now := time.Now().Unix()
	timedOutCount := 0

	for _, task := range tasks {
		failed, durable, transitionErr := transitionDurablePollingFailure(
			task,
			task.Status,
			task.Progress,
			"task_timeout",
			reason,
			nil,
			false,
		)
		if durable {
			if transitionErr != nil {
				logger.LogError(ctx, fmt.Sprintf(
					"sweepTimedOutTasks durable transition error for task %s: %v",
					task.TaskID,
					transitionErr,
				))
				continue
			}
			timedOutCount++
			RefundTaskQuota(ctx, failed, reason)
			continue
		}

		isLegacy := task.SubmitTime > 0 && task.SubmitTime < model.TaskRefundLegacyCutoff

		oldStatus := task.Status
		task.Status = model.TaskStatusFailure
		task.Progress = "100%"
		task.FinishTime = now
		if isLegacy {
			task.FailReason = legacyReason
			// 旧系统任务明确不退款，随终态 CAS 一并清掉 quota，避免被后续对账误判。
			task.Quota = 0
		} else {
			task.FailReason = reason
		}

		won, err := task.UpdateWithStatus(oldStatus)
		if err != nil {
			logger.LogError(ctx, fmt.Sprintf("sweepTimedOutTasks CAS update error for task %s: %v", task.TaskID, err))
			continue
		}
		if !won {
			logger.LogInfo(ctx, fmt.Sprintf("sweepTimedOutTasks: task %s already transitioned, skip", task.TaskID))
			continue
		}
		timedOutCount++
		if !isLegacy {
			RefundTaskQuota(ctx, task, reason)
		}
	}

	if timedOutCount > 0 {
		logger.LogInfo(ctx, fmt.Sprintf("sweepTimedOutTasks: timed out %d tasks", timedOutCount))
	}
}

// sweepUnrefundedFailedTasks 重试已落 FAILURE 终态但仍保留 quota 的欠退款任务。
// 先等待一个短暂宽限期，让终态 CAS 的胜出者完成主路径即时退款，避免正常
// 轮询与对账同时处理刚失败的任务。
func sweepUnrefundedFailedTasks(ctx context.Context) {
	now := time.Now()
	submittingBefore := int64(0)
	if constant.TaskTimeoutMinutes > 0 {
		submittingBefore = now.Unix() - int64(constant.TaskTimeoutMinutes)*60
	}
	attempts, err := model.ListRecoverableTaskBillingAttempts(
		now.Add(-refundReconciliationGracePeriod).Unix(),
		submittingBefore,
		refundReconciliationLimit,
	)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("sweepUnrefundedFailedTasks attempt query error: %v", err))
		return
	}
	for _, attempt := range attempts {
		if ctx.Err() != nil {
			return
		}
		if attempt.Owner == model.TaskBillingOwnerTask {
			if attempt.TaskID == nil {
				logger.LogError(ctx, fmt.Sprintf(
					"sweepUnrefundedFailedTasks invalid task-owned attempt requestId=%s",
					attempt.RequestID,
				))
				continue
			}
			task, taskErr := model.GetTaskByPrimaryID(*attempt.TaskID)
			if taskErr != nil {
				logger.LogError(ctx, fmt.Sprintf(
					"sweepUnrefundedFailedTasks linked task query error requestId=%s: %v",
					attempt.RequestID,
					taskErr,
				))
				continue
			}
			if task.Status == model.TaskStatusSubmitting {
				task, taskErr = model.TransitionTaskSubmissionToFailure(
					task.ID,
					task.TaskID,
					task.PrivateData.UpstreamTaskID,
					"task_timeout",
					fmt.Sprintf("任务超时（%d分钟）", constant.TaskTimeoutMinutes),
				)
				if taskErr != nil {
					logger.LogError(ctx, fmt.Sprintf(
						"sweepUnrefundedFailedTasks transition error requestId=%s: %v",
						attempt.RequestID,
						taskErr,
					))
					continue
				}
			}
		}
		if _, refundErr := RefundTaskBillingAttempt(ctx, attempt.RequestID, "billing recovery sweep"); refundErr != nil {
			logger.LogError(ctx, fmt.Sprintf(
				"sweepUnrefundedFailedTasks refund error requestId=%s: %v",
				attempt.RequestID,
				refundErr,
			))
		}
	}

	updatedBefore := now.Add(-refundReconciliationGracePeriod).Unix()
	tasks := model.GetUnrefundedFailedTasks(updatedBefore, refundReconciliationLimit)
	for _, task := range tasks {
		if ctx.Err() != nil {
			return
		}
		RefundTaskQuota(ctx, task, task.FailReason)
	}
}

func finalizeDurableFinalSuccess(
	ctx context.Context,
	adaptor TaskPollingAdaptor,
	task *model.Task,
	taskResult *relaycommon.TaskInfo,
	requestID string,
) error {
	if task == nil || taskResult == nil || adaptor == nil || requestID == "" {
		return errors.New("durable final success state is unavailable")
	}
	if err := settleTaskBillingOnCompleteForPolling(
		ctx,
		adaptor,
		task,
		taskResult,
	); err != nil {
		return fmt.Errorf("final settlement failed for task %s: %w", task.TaskID, err)
	}
	if err := model.MarkTaskBillingAttemptSucceeded(requestID); err != nil {
		return fmt.Errorf(
			"mark durable task success failed for task %s: %w",
			task.TaskID,
			err,
		)
	}
	return nil
}

// replayDurableFinalSuccessTasks repairs the crash window after the durable
// SUCCESS/100% Task transition but before its zero-delta verification and
// SucceededAt marker. It is deliberately local-only: no channel lookup,
// credential resolution, or provider status request occurs here.
func replayDurableFinalSuccessTasks(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if GetTaskAdaptorFunc == nil {
		return nil
	}
	tasks, err := model.ListRecoverableFinalSuccessTasks(refundReconciliationLimit)
	if err != nil {
		return newSafeTaskPollingError(
			"query recoverable final success tasks failed",
			err,
		)
	}

	var replayErrors []error
	for _, task := range tasks {
		if task == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			replayErrors = append(replayErrors, err)
			break
		}
		if !isSeedDancePlatform(task.Platform) {
			replayErrors = append(replayErrors, fmt.Errorf(
				"recoverable final success platform mismatch for task %s",
				task.TaskID,
			))
			continue
		}
		attempt, attemptErr := model.GetTaskBillingAttemptByTaskID(task.ID)
		if attemptErr != nil {
			replayErrors = append(replayErrors, newSafeTaskPollingError(
				fmt.Sprintf(
					"load recoverable billing attempt failed for task %s",
					task.TaskID,
				),
				attemptErr,
			))
			continue
		}
		if attempt.TaskID == nil ||
			*attempt.TaskID != task.ID ||
			attempt.PublicTaskID != task.TaskID ||
			attempt.Owner != model.TaskBillingOwnerTask ||
			task.Status != model.TaskStatusSuccess ||
			task.Progress != taskcommon.ProgressComplete {
			replayErrors = append(replayErrors, fmt.Errorf(
				"recoverable final success identity mismatch for task %s",
				task.TaskID,
			))
			continue
		}
		if attempt.SucceededAt != 0 {
			continue
		}
		if attempt.FundingRefundedAt != 0 ||
			attempt.TokenRefundedAt != 0 ||
			attempt.RefundStartedAt != 0 ||
			attempt.RefundCompletedAt != 0 {
			replayErrors = append(replayErrors, fmt.Errorf(
				"recoverable final success refund conflict for task %s",
				task.TaskID,
			))
			continue
		}

		adaptor := GetTaskAdaptorFunc(task.Platform)
		if adaptor == nil {
			replayErrors = append(replayErrors, fmt.Errorf(
				"recoverable final success adaptor unavailable for task %s",
				task.TaskID,
			))
			continue
		}
		adaptor.Init(&relaycommon.RelayInfo{
			ChannelMeta: &relaycommon.ChannelMeta{},
		})
		taskResult := &relaycommon.TaskInfo{
			Status:   string(model.TaskStatusSuccess),
			Progress: taskcommon.ProgressComplete,
		}
		if finalizeErr := finalizeDurableFinalSuccess(
			ctx,
			adaptor,
			task,
			taskResult,
			attempt.RequestID,
		); finalizeErr != nil {
			replayErrors = append(replayErrors, finalizeErr)
		}
	}
	return errors.Join(replayErrors...)
}

// TaskPollSummary is the result recorded on an async_task_poll system task row,
// summarizing one polling pass.
type TaskPollSummary struct {
	UnfinishedTasks  int `json:"unfinished_tasks"`
	PlatformsScanned int `json:"platforms_scanned"`
	NullTasksFailed  int `json:"null_tasks_failed"`
}

// RunTaskPollingOnce performs one async-task (Suno/video) polling pass
// synchronously. It honors ctx cancellation (the system-task runner cancels it
// when the lease is lost) and, when report is non-nil, reports progress as
// (processedPlatforms, totalPlatforms). It returns immediately if the task
// adaptor factory has not been wired yet, to avoid a nil call during startup.
func RunTaskPollingOnce(ctx context.Context, report func(processed, total int)) TaskPollSummary {
	summary := TaskPollSummary{}
	if ctx == nil {
		ctx = context.Background()
	}

	common.SysLog("任务进度轮询开始")
	sweepTimedOutTasks(ctx)
	sweepUnrefundedFailedTasks(ctx)
	if GetTaskAdaptorFunc == nil {
		return summary
	}
	if err := replayDurableFinalSuccessTasks(ctx); err != nil {
		logger.LogError(ctx, fmt.Sprintf(
			"replay durable final success tasks failed: %v",
			err,
		))
	}
	allTasks := model.GetAllUnFinishSyncTasks(constant.TaskQueryLimit)
	summary.UnfinishedTasks = len(allTasks)
	platformTask := make(map[constant.TaskPlatform][]*model.Task)
	for _, t := range allTasks {
		if t == nil || t.Status == model.TaskStatusSubmitting {
			continue
		}
		platformTask[t.Platform] = append(platformTask[t.Platform], t)
	}

	totalPlatforms := len(platformTask)
	processedPlatforms := 0
	for platform, tasks := range platformTask {
		if ctx.Err() != nil {
			break
		}
		if report != nil {
			report(processedPlatforms, totalPlatforms)
		}
		processedPlatforms++
		if len(tasks) == 0 {
			continue
		}
		summary.PlatformsScanned++
		taskChannelM := make(map[int][]string)
		taskM := make(map[string]*model.Task)
		nullTaskIds := make([]int64, 0)
		for _, task := range tasks {
			upstreamID := task.GetUpstreamTaskID()
			if isSeedDancePlatform(task.Platform) {
				upstreamID = task.PrivateData.UpstreamTaskID
				if upstreamID == "" {
					logger.LogError(ctx, fmt.Sprintf(
						"durable upstream task id unavailable for task %s",
						task.TaskID,
					))
					continue
				}
			}
			if upstreamID == "" {
				// 统计失败的未完成任务
				nullTaskIds = append(nullTaskIds, task.ID)
				continue
			}
			taskM[upstreamID] = task
			taskChannelM[task.ChannelId] = append(taskChannelM[task.ChannelId], upstreamID)
		}
		if len(nullTaskIds) > 0 {
			summary.NullTasksFailed += len(nullTaskIds)
			err := model.TaskBulkUpdateByID(nullTaskIds, map[string]any{
				"status":   "FAILURE",
				"progress": "100%",
			})
			if err != nil {
				logger.LogError(ctx, fmt.Sprintf("Fix null task_id task error: %v", err))
			} else {
				logger.LogInfo(ctx, fmt.Sprintf("Fix null task_id task success: %v", nullTaskIds))
			}
		}
		if len(taskChannelM) == 0 {
			continue
		}

		DispatchPlatformUpdate(ctx, platform, taskChannelM, taskM)
	}
	if report != nil && ctx.Err() == nil {
		report(totalPlatforms, totalPlatforms)
	}
	common.SysLog("任务进度轮询完成")
	return summary
}

// DispatchPlatformUpdate 按平台分发轮询更新
func DispatchPlatformUpdate(ctx context.Context, platform constant.TaskPlatform, taskChannelM map[int][]string, taskM map[string]*model.Task) {
	if ctx == nil {
		ctx = context.Background()
	}
	switch platform {
	case constant.TaskPlatformMidjourney:
		// MJ 轮询由其自身处理，这里预留入口
	case constant.TaskPlatformSuno:
		_ = UpdateSunoTasks(ctx, taskChannelM, taskM)
	default:
		if err := UpdateVideoTasks(ctx, platform, taskChannelM, taskM); err != nil {
			common.SysLog(fmt.Sprintf("UpdateVideoTasks fail: %s", err))
		}
	}
}

// UpdateSunoTasks 按渠道更新所有 Suno 任务
func UpdateSunoTasks(ctx context.Context, taskChannelM map[int][]string, taskM map[string]*model.Task) error {
	for channelId, taskIds := range taskChannelM {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := updateSunoTasks(ctx, channelId, taskIds, taskM)
		if err != nil {
			logger.LogError(ctx, fmt.Sprintf("渠道 #%d 更新异步任务失败: %s", channelId, err.Error()))
		}
	}
	return nil
}

func updateSunoTasks(ctx context.Context, channelId int, taskIds []string, taskM map[string]*model.Task) error {
	logger.LogInfo(ctx, fmt.Sprintf("渠道 #%d 未完成的任务有: %d", channelId, len(taskIds)))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(taskIds) == 0 {
		return nil
	}
	ch, err := model.CacheGetChannel(channelId)
	if err != nil {
		common.SysLog(fmt.Sprintf("CacheGetChannel: %v", err))
		channelErr := err
		// Durable tasks must transition through the locked narrow primitive so
		// their component-ledger refund remains recoverable. Keep the historical
		// bulk behavior only for tasks with no durable attempt.
		var failedIDs []int64
		for _, upstreamID := range taskIds {
			if t, ok := taskM[upstreamID]; ok {
				reason := fmt.Sprintf("获取渠道信息失败，请联系管理员，渠道ID：%d", channelId)
				failed, durable, transitionErr := transitionDurablePollingFailure(
					t,
					t.Status,
					t.Progress,
					"task_channel_unavailable",
					reason,
					nil,
					false,
				)
				if durable {
					if transitionErr != nil {
						logger.LogError(ctx, fmt.Sprintf(
							"UpdateSunoTask durable transition error task %s: %v",
							t.TaskID,
							transitionErr,
						))
					} else {
						RefundTaskQuota(ctx, failed, reason)
					}
					continue
				}
				failedIDs = append(failedIDs, t.ID)
			}
		}
		updateErr := model.TaskBulkUpdateByID(failedIDs, map[string]any{
			"fail_reason": fmt.Sprintf("获取渠道信息失败，请联系管理员，渠道ID：%d", channelId),
			"status":      "FAILURE",
			"progress":    "100%",
		})
		if updateErr != nil {
			common.SysLog(fmt.Sprintf("UpdateSunoTask error: %v", updateErr))
			return updateErr
		}
		return channelErr
	}
	adaptor := GetTaskAdaptorFunc(constant.TaskPlatformSuno)
	if adaptor == nil {
		return errors.New("adaptor not found")
	}
	proxy := ch.GetSetting().Proxy
	resp, err := adaptor.FetchTask(*ch.BaseURL, ch.Key, map[string]any{
		"ids": taskIds,
	}, proxy)
	if err != nil {
		common.SysLog(fmt.Sprintf("Get Task Do req error: %v", err))
		return err
	}
	if resp.StatusCode != http.StatusOK {
		logger.LogError(ctx, fmt.Sprintf("Get Task status code: %d", resp.StatusCode))
		return fmt.Errorf("Get Task status code: %d", resp.StatusCode)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		common.SysLog(fmt.Sprintf("Get Suno Task parse body error: %v", err))
		return err
	}
	var responseItems dto.TaskResponse[[]dto.SunoDataResponse]
	err = common.Unmarshal(responseBody, &responseItems)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("Get Suno Task parse body error2: %v, body: %s", err, string(responseBody)))
		return err
	}
	if !responseItems.IsSuccess() {
		common.SysLog(fmt.Sprintf("渠道 #%d 未完成的任务有: %d, 成功获取到任务数: %s", channelId, len(taskIds), string(responseBody)))
		return err
	}

	for _, responseItem := range responseItems.Data {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		task := taskM[responseItem.TaskID]
		if task == nil {
			logger.LogWarn(ctx, fmt.Sprintf("Suno task response ignored: unknown task_id=%s", responseItem.TaskID))
			continue
		}
		if !taskNeedsUpdate(task, responseItem) {
			continue
		}

		prevStatus := task.Status
		prevProgress := task.Progress
		task.Status = lo.If(model.TaskStatus(responseItem.Status) != "", model.TaskStatus(responseItem.Status)).Else(task.Status)
		task.FailReason = lo.If(responseItem.FailReason != "", responseItem.FailReason).Else(task.FailReason)
		task.SubmitTime = lo.If(responseItem.SubmitTime != 0, responseItem.SubmitTime).Else(task.SubmitTime)
		task.StartTime = lo.If(responseItem.StartTime != 0, responseItem.StartTime).Else(task.StartTime)
		task.FinishTime = lo.If(responseItem.FinishTime != 0, responseItem.FinishTime).Else(task.FinishTime)
		isFailure := responseItem.FailReason != "" || task.Status == model.TaskStatusFailure
		if isFailure {
			logger.LogInfo(ctx, task.TaskID+" 构建失败，"+task.FailReason)
			task.Status = model.TaskStatusFailure
			task.Progress = "100%"
		}
		if responseItem.Status == model.TaskStatusSuccess {
			task.Progress = "100%"
		}
		task.Data = responseItem.Data

		if isFailure {
			failed, durable, transitionErr := transitionDurablePollingFailure(
				task,
				prevStatus,
				prevProgress,
				"provider_failure",
				task.FailReason,
				&task.Data,
				false,
			)
			if durable {
				if transitionErr != nil {
					logger.LogError(ctx, fmt.Sprintf(
						"UpdateSunoTask durable transition error task %s: %v",
						task.TaskID,
						transitionErr,
					))
					continue
				}
				*task = *failed
				RefundTaskQuota(ctx, failed, failed.FailReason)
				continue
			}
		}

		// 持久化走 CAS，防止重叠轮询/sweep/多实例/持久化失败重试导致重复退款或覆盖终态。
		won, err := task.UpdateWithStatus(prevStatus)
		if err != nil {
			logger.LogError(ctx, fmt.Sprintf("UpdateSunoTask task %s error: %v", task.TaskID, err))
		} else if !won {
			logger.LogWarn(ctx, fmt.Sprintf("Task %s CAS lost or no-op update, skip billing", task.TaskID))
		} else if isFailure && prevStatus != model.TaskStatusFailure {
			RefundTaskQuota(ctx, task, task.FailReason)
		}
	}
	return nil
}

// taskNeedsUpdate 检查 Suno 任务是否需要更新
func taskNeedsUpdate(oldTask *model.Task, newTask dto.SunoDataResponse) bool {
	if oldTask.SubmitTime != newTask.SubmitTime {
		return true
	}
	if oldTask.StartTime != newTask.StartTime {
		return true
	}
	if oldTask.FinishTime != newTask.FinishTime {
		return true
	}
	if string(oldTask.Status) != newTask.Status {
		return true
	}
	if oldTask.FailReason != newTask.FailReason {
		return true
	}

	if (oldTask.Status == model.TaskStatusFailure || oldTask.Status == model.TaskStatusSuccess) && oldTask.Progress != "100%" {
		return true
	}

	oldData, _ := common.Marshal(oldTask.Data)
	newData, _ := common.Marshal(newTask.Data)

	sort.Slice(oldData, func(i, j int) bool {
		return oldData[i] < oldData[j]
	})
	sort.Slice(newData, func(i, j int) bool {
		return newData[i] < newData[j]
	})

	if string(oldData) != string(newData) {
		return true
	}
	return false
}

// UpdateVideoTasks 按渠道更新所有视频任务
func UpdateVideoTasks(ctx context.Context, platform constant.TaskPlatform, taskChannelM map[int][]string, taskM map[string]*model.Task) error {
	channelIDs := make([]int, 0, len(taskChannelM))
	for channelID := range taskChannelM {
		channelIDs = append(channelIDs, channelID)
	}
	sort.Ints(channelIDs)

	var wg sync.WaitGroup
	for _, channelId := range channelIDs {
		taskIds := taskChannelM[channelId]
		if len(taskIds) == 0 {
			continue
		}
		taskIds = append([]string(nil), taskIds...)

		wg.Add(1)
		gopool.Go(func() {
			defer wg.Done()
			if err := updateVideoTasks(ctx, platform, channelId, taskIds, taskM); err != nil {
				logger.LogError(ctx, fmt.Sprintf("Channel #%d failed to update video async tasks: %s", channelId, err.Error()))
			}
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func updateVideoTasks(ctx context.Context, platform constant.TaskPlatform, channelId int, taskIds []string, taskM map[string]*model.Task) error {
	logger.LogInfo(ctx, fmt.Sprintf("Channel #%d pending video tasks: %d", channelId, len(taskIds)))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(taskIds) == 0 {
		return nil
	}
	cacheGetChannel, err := model.CacheGetChannel(channelId)
	if err != nil {
		reason := fmt.Sprintf("Failed to get channel info, channel ID: %d", channelId)
		// Preserve the legacy bulk path only for tasks without a durable
		// billing owner. Ledger-backed tasks need the locked narrow transition
		// and component refund.
		var failedIDs []int64
		for _, upstreamID := range taskIds {
			if t, ok := taskM[upstreamID]; ok {
				failed, durable, transitionErr := transitionDurablePollingFailure(
					t,
					t.Status,
					t.Progress,
					"task_channel_unavailable",
					reason,
					nil,
					isSeedDancePlatform(platform),
				)
				if durable {
					if transitionErr != nil {
						logger.LogError(ctx, fmt.Sprintf(
							"UpdateVideoTask durable transition error task %s: %v",
							t.TaskID,
							transitionErr,
						))
					} else {
						RefundTaskQuota(ctx, failed, reason)
					}
					continue
				}
				failedIDs = append(failedIDs, t.ID)
			}
		}
		errUpdate := model.TaskBulkUpdateByID(failedIDs, map[string]any{
			"fail_reason": reason,
			"status":      "FAILURE",
			"progress":    "100%",
		})
		if errUpdate != nil {
			common.SysLog(fmt.Sprintf("UpdateVideoTask error: %v", errUpdate))
		}
		return fmt.Errorf("CacheGetChannel failed: %w", err)
	}
	adaptor := GetTaskAdaptorFunc(platform)
	if adaptor == nil {
		return fmt.Errorf("video adaptor not found")
	}
	info := &relaycommon.RelayInfo{}
	info.ChannelMeta = &relaycommon.ChannelMeta{
		ChannelBaseUrl: cacheGetChannel.GetBaseURL(),
	}
	info.ApiKey = cacheGetChannel.Key
	adaptor.Init(info)
	disablePollingSleep := cacheGetChannel.GetOtherSettings().DisableTaskPollingSleep
	for i, taskId := range taskIds {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := updateVideoSingleTask(ctx, adaptor, cacheGetChannel, taskId, taskM); err != nil {
			publicTaskID := ""
			if task := taskM[taskId]; task != nil {
				publicTaskID = task.TaskID
			}
			logger.LogError(ctx, fmt.Sprintf(
				"Failed to update video task %s: %s",
				publicTaskID,
				err.Error(),
			))
		}
		if disablePollingSleep || i == len(taskIds)-1 {
			continue
		}

		// sleep 1 second between tasks for this channel only.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
	return nil
}

func updateVideoSingleTask(ctx context.Context, adaptor TaskPollingAdaptor, ch *model.Channel, taskId string, taskM map[string]*model.Task) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if ch == nil {
		return errors.New("video polling channel is unavailable")
	}
	baseURL := constant.ChannelBaseURLs[ch.Type]
	if ch.GetBaseURL() != "" {
		baseURL = ch.GetBaseURL()
	}
	proxy := ch.GetSetting().Proxy

	task := taskM[taskId]
	if task == nil {
		logger.LogError(ctx, "video task not found in polling map")
		return errors.New("video task not found in polling map")
	}
	if task.Status == model.TaskStatusSubmitting {
		return nil
	}
	isSeedDance, identityErr := seedDancePollingIdentity(ch, task)
	if identityErr != nil {
		return identityErr
	}
	upstreamTaskID := task.GetUpstreamTaskID()
	if isSeedDance {
		upstreamTaskID = task.PrivateData.UpstreamTaskID
		if upstreamTaskID == "" {
			return fmt.Errorf(
				"durable upstream task id unavailable for task %s",
				task.TaskID,
			)
		}
		if _, attemptErr := model.GetTaskBillingAttemptByTaskID(task.ID); attemptErr != nil {
			return newSafeTaskPollingError(
				fmt.Sprintf(
					"durable billing attempt unavailable for task %s",
					task.TaskID,
				),
				attemptErr,
			)
		}
	}
	key, err := ResolveStoredTaskKey(ch, task.PrivateData.Key)
	if err != nil {
		return fmt.Errorf("resolve stored task credential: %w", err)
	}
	request := map[string]any{
		"task_id": upstreamTaskID,
		"action":  task.Action,
	}
	var resp *http.Response
	if withContext, ok := adaptor.(TaskFetcherWithContext); ok {
		resp, err = withContext.FetchTaskWithContext(
			ctx,
			baseURL,
			key,
			request,
			proxy,
		)
	} else {
		resp, err = adaptor.FetchTask(baseURL, key, request, proxy)
	}
	if err != nil {
		return newSafeTaskPollingError(
			fmt.Sprintf("fetch task failed for task %s", task.TaskID),
			err,
		)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return newSafeTaskPollingError(
			fmt.Sprintf("read polling response failed for task %s", task.TaskID),
			err,
		)
	}

	snap := task.Snapshot()

	taskResult := &relaycommon.TaskInfo{}
	// try parse as New API response format
	var responseItems dto.TaskResponse[model.Task]
	if err = common.Unmarshal(responseBody, &responseItems); err == nil && responseItems.IsSuccess() {
		logger.LogDebug(
			ctx,
			"updateVideoSingleTask parsed new api response for task %s",
			task.TaskID,
		)
		t := responseItems.Data
		taskResult.TaskID = t.TaskID
		taskResult.Status = string(t.Status)
		taskResult.Url = t.GetResultURL()
		taskResult.Progress = t.Progress
		taskResult.Reason = t.FailReason
		task.Data = t.Data
	} else if taskResult, err = adaptor.ParseTaskResult(responseBody); err != nil {
		return newSafeTaskPollingError(
			fmt.Sprintf("parse polling response failed for task %s", task.TaskID),
			err,
		)
	}

	task.Data = redactVideoResponseBody(responseBody)

	now := time.Now().Unix()
	if taskResult.Status == "" {
		//taskResult = relaycommon.FailTaskInfo("upstream returned empty status")
		errorResult := &dto.GeneralErrorResponse{}
		if err = common.Unmarshal(responseBody, &errorResult); err == nil {
			openaiError := errorResult.TryToOpenAIError()
			if openaiError != nil {
				// 返回规范的 OpenAI 错误格式，提取错误信息，判断错误是否为任务失败
				if openaiError.Code == "429" {
					// 429 错误通常表示请求过多或速率限制，暂时不认为是任务失败，保持原状态等待下一轮轮询
					return nil
				}

				// 其他错误认为是任务失败，记录错误信息并更新任务状态
				taskResult = relaycommon.FailTaskInfo("upstream returned error")
			} else {
				// unknown error format, log original response
				logger.LogError(ctx, fmt.Sprintf(
					"Task %s returned empty status with unrecognized error format",
					task.TaskID,
				))
				taskResult = relaycommon.FailTaskInfo("upstream returned unrecognized message")
			}
		}
	}

	shouldRefund := false
	shouldSettle := false

	task.Status = model.TaskStatus(taskResult.Status)
	switch taskResult.Status {
	case model.TaskStatusSubmitted:
		task.Progress = taskcommon.ProgressSubmitted
	case model.TaskStatusQueued:
		task.Progress = taskcommon.ProgressQueued
	case model.TaskStatusInProgress:
		task.Progress = taskcommon.ProgressInProgress
		if task.StartTime == 0 {
			task.StartTime = now
		}
	case model.TaskStatusSuccess:
		task.Progress = taskcommon.ProgressComplete
		if task.FinishTime == 0 {
			task.FinishTime = now
		}
		if isSeedDance {
			task.PrivateData.ResultURL = fmt.Sprintf(
				"/v1/videos/%s/content",
				task.TaskID,
			)
		} else if strings.HasPrefix(taskResult.Url, "data:") {
			// data: URI (e.g. Vertex base64 encoded video) — keep in Data, not in ResultURL
			task.PrivateData.ResultURL = taskcommon.BuildProxyURL(task.TaskID)
		} else if taskResult.Url != "" {
			// Direct upstream URL (e.g. Kling, Ali, Doubao, etc.)
			task.PrivateData.ResultURL = taskResult.Url
		} else {
			// No URL from adaptor — construct proxy URL using public task ID
			task.PrivateData.ResultURL = taskcommon.BuildProxyURL(task.TaskID)
		}
		shouldSettle = true
	case model.TaskStatusFailure:
		task.Status = model.TaskStatusFailure
		task.Progress = taskcommon.ProgressComplete
		if task.FinishTime == 0 {
			task.FinishTime = now
		}
		task.FailReason = taskResult.Reason
		logger.LogInfo(ctx, fmt.Sprintf("Task %s failed", task.TaskID))
		taskResult.Progress = taskcommon.ProgressComplete
		shouldRefund = true
	default:
		return fmt.Errorf("unknown task status for task %s", task.TaskID)
	}
	if taskResult.Progress != "" {
		task.Progress = taskResult.Progress
	}

	if shouldRefund {
		failed, durable, transitionErr := transitionDurablePollingFailure(
			task,
			snap.Status,
			snap.Progress,
			"provider_failure",
			task.FailReason,
			&task.Data,
			isSeedDance,
		)
		if durable {
			if transitionErr != nil {
				return fmt.Errorf(
					"durable failure transition failed for task %s: %w",
					task.TaskID,
					transitionErr,
				)
			}
			task = failed
			RefundTaskQuota(ctx, failed, failed.FailReason)
			return nil
		}
	}

	if shouldSettle && isSeedDance {
		transitioned, lockedAttempt, transitionErr := model.TransitionTaskToSuccess(
			task.ID,
			task.TaskID,
			model.TaskSuccessTransition{
				ExpectedStatus:   snap.Status,
				ExpectedProgress: snap.Progress,
				ResultURL:        task.PrivateData.ResultURL,
				Data:             &task.Data,
			},
		)
		if transitionErr != nil {
			return fmt.Errorf(
				"durable success transition failed for task %s: %w",
				task.TaskID,
				transitionErr,
			)
		}
		if finalizeErr := finalizeDurableFinalSuccess(
			ctx,
			adaptor,
			transitioned,
			taskResult,
			lockedAttempt.RequestID,
		); finalizeErr != nil {
			return finalizeErr
		}
		*task = *transitioned
		return nil
	}

	if isSeedDance {
		transitioned, transitionErr := model.TransitionTaskPollingState(
			task.ID,
			task.TaskID,
			model.TaskPollingTransition{
				ExpectedStatus:   snap.Status,
				ExpectedProgress: snap.Progress,
				Status:           task.Status,
				Progress:         task.Progress,
				Data:             &task.Data,
			},
		)
		if transitionErr != nil {
			return fmt.Errorf(
				"durable polling transition failed for task %s: %w",
				task.TaskID,
				transitionErr,
			)
		}
		*task = *transitioned
		return nil
	}

	isDone := task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure
	if isDone && snap.Status != task.Status {
		won, err := task.UpdateWithStatus(snap.Status)
		if err != nil {
			logger.LogError(ctx, fmt.Sprintf("UpdateWithStatus failed for task %s: %s", task.TaskID, err.Error()))
			shouldRefund = false
			shouldSettle = false
		} else if !won {
			logger.LogWarn(ctx, fmt.Sprintf("Task %s CAS lost or no-op update, skip billing", task.TaskID))
			shouldRefund = false
			shouldSettle = false
		}
	} else if !snap.Equal(task.Snapshot()) {
		if _, err := task.UpdateWithStatus(snap.Status); err != nil {
			logger.LogError(ctx, fmt.Sprintf("Failed to update task %s: %s", task.TaskID, err.Error()))
		}
	} else {
		// No changes, skip update
		logger.LogDebug(ctx, "No update needed for task %s", task.TaskID)
	}

	if shouldSettle {
		settleTaskBillingOnComplete(ctx, adaptor, task, taskResult)
	}
	if shouldRefund {
		RefundTaskQuota(ctx, task, task.FailReason)
	}

	return nil
}

var settleTaskBillingOnCompleteForPolling = func(
	_ context.Context,
	adaptor TaskPollingAdaptor,
	task *model.Task,
	taskResult *relaycommon.TaskInfo,
) error {
	if task == nil || task.PrivateData.BillingContext == nil || taskResult == nil {
		return errors.New("durable task final billing context is unavailable")
	}
	if actualQuota := adaptor.AdjustBillingOnComplete(task, taskResult); actualQuota > 0 {
		return fmt.Errorf(
			"durable task final billing delta is not zero: actual=%d prepaid=%d",
			actualQuota,
			task.Quota,
		)
	}
	if taskResult.TotalTokens > 0 {
		return errors.New("durable task final token billing delta is not zero")
	}
	return nil
}

func redactVideoResponseBody(body []byte) []byte {
	var m map[string]any
	if err := common.Unmarshal(body, &m); err != nil {
		return body
	}
	resp, _ := m["response"].(map[string]any)
	if resp != nil {
		delete(resp, "bytesBase64Encoded")
		if v, ok := resp["video"].(string); ok {
			resp["video"] = truncateBase64(v)
		}
		if vs, ok := resp["videos"].([]any); ok {
			for i := range vs {
				if vm, ok := vs[i].(map[string]any); ok {
					delete(vm, "bytesBase64Encoded")
				}
			}
		}
	}
	b, err := common.Marshal(m)
	if err != nil {
		return body
	}
	return b
}

func truncateBase64(s string) string {
	const maxKeep = 256
	if len(s) <= maxKeep {
		return s
	}
	return s[:maxKeep] + "..."
}

// settleTaskBillingOnComplete 任务完成时的统一计费调整。
// 优先级：1. adaptor.AdjustBillingOnComplete 返回正数 → 使用 adaptor 计算的额度
//
//  2. taskResult.TotalTokens > 0 → 按 token 重算
//  3. 都不满足 → 保持预扣额度不变
func settleTaskBillingOnComplete(ctx context.Context, adaptor TaskPollingAdaptor, task *model.Task, taskResult *relaycommon.TaskInfo) {
	// 0. 按次计费的任务不做差额结算
	if bc := task.PrivateData.BillingContext; bc != nil && bc.PerCallBilling {
		logger.LogInfo(ctx, fmt.Sprintf("任务 %s 按次计费，跳过差额结算", task.TaskID))
		return
	}
	// 1. 优先让 adaptor 决定最终额度
	if actualQuota := adaptor.AdjustBillingOnComplete(task, taskResult); actualQuota > 0 {
		RecalculateTaskQuota(ctx, task, actualQuota, "adaptor计费调整")
		return
	}
	// 2. 回退到 token 重算
	if taskResult.TotalTokens > 0 {
		RecalculateTaskQuotaByTokens(ctx, task, taskResult.TotalTokens)
		return
	}
	// 3. 无调整，保持预扣额度
}
