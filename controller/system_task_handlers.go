package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

// RegisterScheduledSystemTasks wires the periodic channel test, upstream model
// update, and async task polling (Midjourney / Suno / video) jobs into the
// system task framework so a DB lease dedups execution across multiple master
// instances and each run is recorded as one task row. Call this before
// service.StartSystemTaskRunner.
func RegisterScheduledSystemTasks() {
	service.RegisterSystemTaskHandler(channelTestHandler{})
	service.RegisterSystemTaskHandler(modelUpdateHandler{})
	service.RegisterSystemTaskHandler(midjourneyPollHandler{})
	service.RegisterSystemTaskHandler(asyncTaskPollHandler{})
	service.RegisterSystemTaskHandler(seedDanceSubmitReconciliationHandler{})
	service.RegisterSystemTaskHandler(openCodeGoRefreshHandler{})
	service.RegisterSystemTaskHandler(openCodeGoRiskRecheckHandler{})
}

const (
	openCodeGoRefreshTaskDefaultIntervalMinutes = 1
	openCodeGoRefreshTaskDefaultStaleMinutes    = 15
	openCodeGoRefreshTaskDefaultBatchSize       = 500
)

type openCodeGoRefreshTaskPayload struct {
	ChannelID   int  `json:"channel_id,omitempty"`
	Concurrency int  `json:"concurrency,omitempty"`
	Scheduled   bool `json:"scheduled,omitempty"`
}

type openCodeGoRefreshHandler struct{}

func (openCodeGoRefreshHandler) Type() string { return model.SystemTaskTypeOpenCodeGoRefresh }

func (openCodeGoRefreshHandler) Enabled() bool {
	return common.CryptoSecretExplicitlyConfigured &&
		common.GetEnvOrDefaultBool("OPENCODE_GO_REFRESH_TASK_ENABLED", true)
}

type openCodeGoRiskRecheckTaskPayload struct {
	ChannelID   int `json:"channel_id"`
	Concurrency int `json:"concurrency,omitempty"`
	Limit       int `json:"limit,omitempty"`
}

type openCodeGoRiskRecheckHandler struct{}

func (openCodeGoRiskRecheckHandler) Type() string {
	return model.SystemTaskTypeOpenCodeGoRiskRecheck
}

func (openCodeGoRiskRecheckHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	payload := openCodeGoRiskRecheckTaskPayload{}
	if err := task.DecodePayload(&payload); err != nil {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, err)
		return
	}
	if payload.ChannelID <= 0 {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, errors.New("OpenCode Go risk recheck channel is required"))
		return
	}
	if payload.Concurrency <= 0 {
		payload.Concurrency = configuredOpenCodeGoRiskRecheckConcurrency()
	}
	if payload.Limit <= 0 {
		payload.Limit = configuredOpenCodeGoRiskRecheckBatchSize()
	}
	riskService, err := service.NewConfiguredOpenCodeGoRiskRecheckService()
	if err != nil {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, err)
		return
	}
	summary, err := riskService.RecheckRiskWorkspaces(
		ctx,
		payload.ChannelID,
		payload.Concurrency,
		payload.Limit,
		"task",
		service.NewSystemTaskProgressReporter(task, runnerID),
	)
	if err != nil {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, summary, err)
		return
	}
	finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusSucceeded, summary, nil)
}

func (openCodeGoRefreshHandler) Interval() time.Duration {
	minutes := common.GetEnvOrDefault(
		"OPENCODE_GO_REFRESH_TASK_INTERVAL_MINUTES",
		openCodeGoRefreshTaskDefaultIntervalMinutes,
	)
	if minutes < 1 {
		minutes = openCodeGoRefreshTaskDefaultIntervalMinutes
	}
	return time.Duration(minutes) * time.Minute
}

func (openCodeGoRefreshHandler) NewPayload() any {
	return openCodeGoRefreshTaskPayload{
		Concurrency: configuredOpenCodeGoRefreshConcurrency(),
		Scheduled:   true,
	}
}

func (openCodeGoRefreshHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	payload := openCodeGoRefreshTaskPayload{}
	if err := task.DecodePayload(&payload); err != nil {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, err)
		return
	}
	poolService, err := service.NewConfiguredOpenCodeGoAccountPoolService()
	if err != nil {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, err)
		return
	}
	if payload.Concurrency <= 0 {
		payload.Concurrency = configuredOpenCodeGoRefreshConcurrency()
	}
	batchSize := common.GetEnvOrDefault(
		"OPENCODE_GO_REFRESH_TASK_BATCH_SIZE",
		openCodeGoRefreshTaskDefaultBatchSize,
	)
	modelRecovery, err := service.RecoverOpenCodeGoModelCooldowns(
		payload.ChannelID,
		time.Now(),
		batchSize,
	)
	if err != nil {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, err)
		return
	}
	reportProgress := service.NewSystemTaskProgressReporter(task, runnerID)
	var summary service.OpenCodeGoRefreshSummary
	if payload.ChannelID > 0 {
		summary, err = poolService.RefreshAllIdentities(
			ctx,
			payload.ChannelID,
			payload.Concurrency,
			reportProgress,
		)
	} else {
		now := common.GetTimestamp()
		staleMinutes := common.GetEnvOrDefault(
			"OPENCODE_GO_REFRESH_STALE_AFTER_MINUTES",
			openCodeGoRefreshTaskDefaultStaleMinutes,
		)
		if staleMinutes < 1 {
			staleMinutes = openCodeGoRefreshTaskDefaultStaleMinutes
		}
		targets, queryErr := model.ListOpenCodeGoDueRefreshTargets(
			now,
			now-int64(staleMinutes*60),
			batchSize,
		)
		if queryErr != nil {
			finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, queryErr)
			return
		}
		summary, err = poolService.RefreshIdentityTargets(
			ctx,
			targets,
			payload.Concurrency,
			reportProgress,
		)
	}
	if err != nil {
		summary.ModelRecovery = modelRecovery
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, summary, err)
		return
	}
	summary.ModelRecovery = modelRecovery
	if summary.Succeeded > 0 {
		lifecycle, lifecycleErr := service.NewConfiguredOpenCodeGoLifecycleService()
		if lifecycleErr != nil {
			finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, summary, lifecycleErr)
			return
		}
		summary.Lifecycle, lifecycleErr = lifecycle.RunRefreshAutomations(ctx, summary.Results, "refresh_task")
		if lifecycleErr != nil {
			finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, summary, lifecycleErr)
			return
		}
	}
	if summary.Failed > 0 {
		finishSystemTaskHandler(
			task,
			runnerID,
			model.SystemTaskStatusFailed,
			summary,
			fmt.Errorf("OpenCode Go refresh completed with %d failed item(s)", summary.Failed),
		)
		return
	}
	finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusSucceeded, summary, nil)
}

func configuredOpenCodeGoRefreshConcurrency() int {
	concurrency := common.GetEnvOrDefault(
		"OPENCODE_GO_REFRESH_CONCURRENCY",
		service.OpenCodeGoDefaultRefreshConcurrency,
	)
	if concurrency < 1 {
		return service.OpenCodeGoDefaultRefreshConcurrency
	}
	if concurrency > service.OpenCodeGoMaxRefreshConcurrency {
		return service.OpenCodeGoMaxRefreshConcurrency
	}
	return concurrency
}

func configuredOpenCodeGoRiskRecheckConcurrency() int {
	concurrency := common.GetEnvOrDefault(
		"OPENCODE_GO_RISK_RECHECK_CONCURRENCY",
		service.OpenCodeGoDefaultRiskRecheckConcurrency,
	)
	if concurrency < 1 {
		return service.OpenCodeGoDefaultRiskRecheckConcurrency
	}
	if concurrency > service.OpenCodeGoMaxRiskRecheckConcurrency {
		return service.OpenCodeGoMaxRiskRecheckConcurrency
	}
	return concurrency
}

func configuredOpenCodeGoRiskRecheckBatchSize() int {
	batchSize := common.GetEnvOrDefault("OPENCODE_GO_RISK_RECHECK_BATCH_SIZE", 500)
	if batchSize < 1 {
		return 500
	}
	if batchSize > 5000 {
		return 5000
	}
	return batchSize
}

// channelTestHandler runs the scheduled "test all channels" job. Enablement and
// cadence still come from the monitor settings; only the execution path moved
// into the system task runner.
type channelTestHandler struct{}

func (channelTestHandler) Type() string { return model.SystemTaskTypeChannelTest }

func (channelTestHandler) Enabled() bool {
	return operation_setting.GetMonitorSetting().AutoTestChannelEnabled
}

func (channelTestHandler) Interval() time.Duration {
	minutes := operation_setting.GetMonitorSetting().AutoTestChannelMinutes
	if minutes <= 0 {
		minutes = 10
	}
	return time.Duration(minutes * float64(time.Minute))
}

func (channelTestHandler) NewPayload() any { return nil }

// channelTestTaskPayload controls one channel_test run. A nil/empty payload is a
// scheduled run, which uses the configured monitor ChannelTestMode and does not
// notify. A manual "test all channels" trigger sets Mode=scheduled_all and
// Notify=true to reproduce the legacy manual behavior (test every channel and
// notify root on completion).
type channelTestTaskPayload struct {
	Mode   string `json:"mode,omitempty"`
	Notify bool   `json:"notify,omitempty"`
}

func (channelTestHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	payload := channelTestTaskPayload{}
	if err := task.DecodePayload(&payload); err != nil {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, err)
		return
	}
	summary, err := runChannelTestTask(ctx, payload.Mode, payload.Notify, service.NewSystemTaskProgressReporter(task, runnerID))
	if err != nil {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, err)
		return
	}
	finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusSucceeded, summary, nil)
}

// modelUpdateHandler runs the scheduled upstream model update detection job.
type modelUpdateHandler struct{}

func (modelUpdateHandler) Type() string { return model.SystemTaskTypeModelUpdate }

func (modelUpdateHandler) Enabled() bool {
	return common.GetEnvOrDefaultBool("CHANNEL_UPSTREAM_MODEL_UPDATE_TASK_ENABLED", true)
}

func (modelUpdateHandler) Interval() time.Duration {
	intervalMinutes := common.GetEnvOrDefault(
		"CHANNEL_UPSTREAM_MODEL_UPDATE_TASK_INTERVAL_MINUTES",
		channelUpstreamModelUpdateTaskDefaultIntervalMinutes,
	)
	if intervalMinutes < 1 {
		intervalMinutes = channelUpstreamModelUpdateTaskDefaultIntervalMinutes
	}
	return time.Duration(intervalMinutes) * time.Minute
}

func (modelUpdateHandler) NewPayload() any { return nil }

// modelUpdateTaskPayload controls one model_update run. A scheduled run
// (Manual=false) respects the per-channel minimum check interval and may
// auto-apply detected models when a channel has auto-sync enabled. A manual
// "detect all" trigger sets Manual=true to reproduce the legacy detect-all
// semantics: force a re-check regardless of the interval and never auto-apply,
// so the admin reviews and applies changes explicitly.
type modelUpdateTaskPayload struct {
	Manual bool `json:"manual,omitempty"`
}

func (modelUpdateHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	payload := modelUpdateTaskPayload{}
	if err := task.DecodePayload(&payload); err != nil {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, err)
		return
	}
	summary := runChannelUpstreamModelUpdateTaskOnce(ctx, payload.Manual, !payload.Manual, service.NewSystemTaskProgressReporter(task, runnerID))
	finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusSucceeded, summary, nil)
}

// midjourneyPollHandler runs one Midjourney polling pass per scheduled run.
// Enabled() folds the "are there unfinished tasks?" check into enablement so the
// scheduler creates no row when the system is idle; only when at least one
// Midjourney task is in progress does a row get scheduled.
type midjourneyPollHandler struct{}

func (midjourneyPollHandler) Type() string { return model.SystemTaskTypeMidjourneyPoll }

func (midjourneyPollHandler) Enabled() bool {
	return constant.UpdateTask && model.HasUnfinishedMidjourneyTasks()
}

func (midjourneyPollHandler) Interval() time.Duration { return 15 * time.Second }

func (midjourneyPollHandler) NewPayload() any { return nil }

func (midjourneyPollHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	summary := runMidjourneyTaskUpdateOnce(ctx, service.NewSystemTaskProgressReporter(task, runnerID))
	finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusSucceeded, summary, nil)
}

// asyncTaskPollHandler runs one async-task (Suno/video) polling pass per
// scheduled run. Like midjourneyPollHandler, Enabled() folds in the unfinished
// task existence check so an idle system schedules no rows.
type asyncTaskPollHandler struct{}

func (asyncTaskPollHandler) Type() string { return model.SystemTaskTypeAsyncTaskPoll }

func (asyncTaskPollHandler) Enabled() bool {
	return constant.UpdateTask && model.HasUnfinishedSyncTasks()
}

func (asyncTaskPollHandler) Interval() time.Duration { return 15 * time.Second }

func (asyncTaskPollHandler) NewPayload() any { return nil }

func (asyncTaskPollHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	summary := service.RunTaskPollingOnce(ctx, service.NewSystemTaskProgressReporter(task, runnerID))
	finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusSucceeded, summary, nil)
}

type seedDanceSubmitReconciliationHandler struct{}

func (seedDanceSubmitReconciliationHandler) Type() string {
	return model.SystemTaskTypeSeedDanceSubmitReconciliation
}

func failSeedDanceSubmitReconciliation(
	task *model.SystemTask,
	runnerID string,
	err error,
) {
	if err == nil {
		err = model.ErrTaskSubmissionStateConflict
	}
	finishSystemTaskHandler(
		task,
		runnerID,
		model.SystemTaskStatusFailed,
		nil,
		err,
	)
}

func validateSeedDanceReconciliationIdentity(
	task *model.Task,
	attempt *model.TaskBillingAttempt,
	payload service.SeedDanceSubmitReconciliationPayload,
) error {
	if task == nil || attempt == nil ||
		task.ID != payload.PersistentTaskID ||
		task.TaskID != payload.PublicTaskID ||
		task.ChannelId != payload.ChannelID ||
		task.Platform != constant.TaskPlatform("59") ||
		attempt.Owner != model.TaskBillingOwnerTask ||
		attempt.TaskID == nil || *attempt.TaskID != task.ID ||
		attempt.RequestID == "" ||
		attempt.PublicTaskID != task.TaskID ||
		attempt.UserID != task.UserId ||
		attempt.FundingSource != task.PrivateData.BillingSource ||
		attempt.SubscriptionID != task.PrivateData.SubscriptionId ||
		attempt.TokenID != task.PrivateData.TokenId ||
		attempt.SubmitTime != task.SubmitTime {
		return model.ErrTaskBillingIdentityDrift
	}
	if attempt.FundingAmount < 0 || attempt.TokenAmount < 0 ||
		attempt.FundingAmount > common.MaxQuota ||
		attempt.TokenAmount > common.MaxQuota ||
		attempt.FundingConsumedAt == 0 ||
		attempt.TokenConsumedAt == 0 ||
		attempt.PreconsumeCompletedAt == 0 ||
		attempt.SucceededAt != 0 {
		return model.ErrTaskBillingIdentityDrift
	}
	if attempt.IsFree {
		if attempt.UserID < 0 || attempt.FundingSource != "" ||
			attempt.SubscriptionID != 0 || attempt.FundingAmount != 0 ||
			attempt.TokenAmount != 0 {
			return model.ErrTaskBillingIdentityDrift
		}
	} else {
		if attempt.UserID <= 0 {
			return model.ErrTaskBillingIdentityDrift
		}
		switch attempt.FundingSource {
		case service.BillingSourceWallet:
			if attempt.SubscriptionID != 0 {
				return model.ErrTaskBillingIdentityDrift
			}
		case service.BillingSourceSubscription:
			if attempt.SubscriptionID <= 0 {
				return model.ErrTaskBillingIdentityDrift
			}
		default:
			return model.ErrTaskBillingIdentityDrift
		}
		if attempt.TokenAmount > 0 && attempt.TokenID <= 0 {
			return model.ErrTaskBillingIdentityDrift
		}
	}
	refundComplete := attempt.FundingRefundedAt != 0 &&
		attempt.TokenRefundedAt != 0
	if (attempt.RefundCompletedAt != 0) != refundComplete {
		return model.ErrTaskBillingIdentityDrift
	}
	if attempt.RefundCompletedAt == 0 &&
		attempt.FundingAmount != task.Quota {
		return model.ErrTaskBillingIdentityDrift
	}
	digest, err := model.DigestTaskBillingContext(task.PrivateData.BillingContext)
	if err != nil {
		return err
	}
	if digest != attempt.BillingContextDigest {
		return model.ErrTaskBillingIdentityDrift
	}
	return nil
}

func (seedDanceSubmitReconciliationHandler) Run(
	ctx context.Context,
	systemTask *model.SystemTask,
	runnerID string,
) {
	if err := ctx.Err(); err != nil {
		failSeedDanceSubmitReconciliation(systemTask, runnerID, err)
		return
	}
	payload, err := service.DecodeSeedDanceSubmitReconciliationPayload(systemTask)
	if err != nil {
		failSeedDanceSubmitReconciliation(systemTask, runnerID, err)
		return
	}
	if payload.PublicTaskID == "" || payload.UpstreamTaskID == "" ||
		payload.PersistentTaskID <= 0 ||
		payload.ChannelID <= 0 ||
		payload.ErrorCode == "" || payload.ObservedAt <= 0 {
		failSeedDanceSubmitReconciliation(
			systemTask,
			runnerID,
			model.ErrTaskBillingIdentityDrift,
		)
		return
	}

	task, err := model.GetTaskByPrimaryID(payload.PersistentTaskID)
	if err != nil {
		failSeedDanceSubmitReconciliation(systemTask, runnerID, err)
		return
	}
	channelModel, err := model.GetChannelById(payload.ChannelID, true)
	if err != nil {
		failSeedDanceSubmitReconciliation(systemTask, runnerID, err)
		return
	}
	if channelModel.Type != constant.ChannelTypeSeedDance {
		failSeedDanceSubmitReconciliation(
			systemTask,
			runnerID,
			model.ErrTaskBillingIdentityDrift,
		)
		return
	}
	attempt, err := model.GetTaskBillingAttemptByTaskID(task.ID)
	if err != nil {
		failSeedDanceSubmitReconciliation(systemTask, runnerID, err)
		return
	}
	if err := validateSeedDanceReconciliationIdentity(task, attempt, payload); err != nil {
		failSeedDanceSubmitReconciliation(systemTask, runnerID, err)
		return
	}

	if _, err := model.AttachTaskUpstreamResultForReconciliation(
		task.ID,
		task.TaskID,
		task.ChannelId,
		payload.UpstreamTaskID,
	); err != nil {
		failSeedDanceSubmitReconciliation(systemTask, runnerID, err)
		return
	}
	if _, err := model.TransitionTaskSubmissionToFailure(
		task.ID,
		task.TaskID,
		payload.UpstreamTaskID,
		payload.ErrorCode,
		"task submission persistence failed",
	); err != nil {
		failSeedDanceSubmitReconciliation(systemTask, runnerID, err)
		return
	}
	if _, err := service.RefundTaskBillingAttempt(
		ctx,
		attempt.RequestID,
		payload.ErrorCode,
	); err != nil {
		failSeedDanceSubmitReconciliation(systemTask, runnerID, err)
		return
	}

	reloadedTask, err := model.GetTaskByPrimaryID(task.ID)
	if err != nil {
		failSeedDanceSubmitReconciliation(systemTask, runnerID, err)
		return
	}
	reloadedAttempt, err := model.GetTaskBillingAttemptByTaskID(task.ID)
	if err != nil {
		failSeedDanceSubmitReconciliation(systemTask, runnerID, err)
		return
	}
	if err := validateSeedDanceReconciliationIdentity(
		reloadedTask,
		reloadedAttempt,
		payload,
	); err != nil {
		failSeedDanceSubmitReconciliation(systemTask, runnerID, err)
		return
	}
	if reloadedTask.Status != model.TaskStatusFailure ||
		reloadedTask.Progress != "100%" ||
		reloadedTask.PrivateData.UpstreamTaskID != payload.UpstreamTaskID ||
		reloadedTask.Quota != 0 ||
		reloadedAttempt.FundingRefundedAt == 0 ||
		reloadedAttempt.TokenRefundedAt == 0 ||
		reloadedAttempt.RefundCompletedAt == 0 ||
		reloadedAttempt.SucceededAt != 0 {
		failSeedDanceSubmitReconciliation(
			systemTask,
			runnerID,
			model.ErrTaskBillingAttemptState,
		)
		return
	}
	finishSystemTaskHandler(
		systemTask,
		runnerID,
		model.SystemTaskStatusSucceeded,
		nil,
		nil,
	)
}

func finishSystemTaskHandler(task *model.SystemTask, runnerID string, status model.SystemTaskStatus, result any, runErr error) {
	errorMessage := ""
	if runErr != nil {
		errorMessage = runErr.Error()
	}
	if err := model.FinishSystemTask(task.TaskID, runnerID, status, result, errorMessage); err != nil {
		common.SysLog(fmt.Sprintf("system task %s failed to persist result: %v", task.TaskID, err))
	}
}
