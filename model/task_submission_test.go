package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func preparedSubmissionFixture(t *testing.T, requestID string, quota int) (*Task, *TaskBillingAttempt) {
	t.Helper()
	billingContext := &TaskBillingContext{
		GroupRatio:      1,
		OriginModelName: "fixture-model",
		OtherRatios:     map[string]float64{"seconds": 1},
	}
	snapshot := billingSnapshot(requestID, quota)
	snapshot.BillingContext = billingContext
	if quota == 0 {
		snapshot.IsFree = true
		snapshot.UserID = 0
		snapshot.FundingSource = ""
		snapshot.TokenAmount = 0
	} else {
		seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 10_000, 10_000)
	}
	attempt, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	if !snapshot.IsFree {
		_, err = ApplyTaskFundingPreconsume(requestID)
		require.NoError(t, err)
		_, err = ApplyTaskTokenPreconsume(requestID)
		require.NoError(t, err)
	}
	candidate := &Task{
		TaskID:     snapshot.PublicTaskID,
		Platform:   constant.TaskPlatform("fixture"),
		UserId:     snapshot.UserID,
		Group:      "default",
		ChannelId:  91,
		Quota:      snapshot.FundingAmount,
		Action:     "generate",
		Status:     TaskStatusSubmitting,
		SubmitTime: snapshot.SubmitTime,
		Progress:   "0%",
		Properties: Properties{UpstreamModelName: "route-a", OriginModelName: "fixture-model"},
		PrivateData: TaskPrivateData{
			Key:            "KEY_A",
			BillingSource:  snapshot.FundingSource,
			SubscriptionId: snapshot.SubscriptionID,
			TokenId:        snapshot.TokenID,
			NodeName:       "node-a",
			BillingContext: billingContext,
		},
		Data: json.RawMessage(`{"latest":"initial"}`),
	}
	return candidate, attempt
}

func TestPrepareTaskSubmissionLinksAttemptAndTransfersOwnerAtomically(t *testing.T) {
	truncateTables(t)
	candidate, requestAttempt := preparedSubmissionFixture(t, "prepare-link", 100)
	assert.Zero(t, requestAttempt.PrepareVersion)

	prepared, attempt, err := PrepareTaskSubmissionAttempt(candidate, 0, "prepare-link")
	require.NoError(t, err)
	assert.NotZero(t, prepared.ID)
	require.NotNil(t, attempt.TaskID)
	assert.Equal(t, prepared.ID, *attempt.TaskID)
	assert.Equal(t, TaskBillingOwnerTask, attempt.Owner)
	assert.Equal(t, int64(1), attempt.PrepareVersion)
	assert.NotZero(t, attempt.OwnerTransferredAt)
	assert.Equal(t, TaskStatusSubmitting, prepared.Status)
	assert.Equal(t, "0%", prepared.Progress)
}

func TestPrepareTaskSubmissionCommitAmbiguityUsesDurableOwner(t *testing.T) {
	truncateTables(t)
	candidate, _ := preparedSubmissionFixture(t, "prepare-ambiguous", 0)

	restore := setTaskSubmissionFailpointForTest(func(operation, point string) error {
		if operation == "prepare" && point == "after_commit" {
			return errors.New("ambiguous prepare commit")
		}
		return nil
	})
	prepared, attempt, err := PrepareTaskSubmissionAttempt(candidate, 0, "prepare-ambiguous")
	restore()
	require.NoError(t, err)
	require.NotNil(t, attempt.TaskID)
	assert.Equal(t, prepared.ID, *attempt.TaskID)
	assert.Equal(t, TaskBillingOwnerTask, attempt.Owner)
	assert.Equal(t, int64(1), attempt.PrepareVersion)
}

func TestPrepareTaskSubmissionDBUnreadableDoesNotBareRefund(t *testing.T) {
	truncateTables(t)
	candidate, snapshotAttempt := preparedSubmissionFixture(t, "prepare-db-unreadable", 100)
	beforeUser := loadBillingUserQuota(t, snapshotAttempt.UserID)
	beforeToken := loadBillingToken(t, snapshotAttempt.TokenID)

	restore := setTaskSubmissionFailpointForTest(func(operation, point string) error {
		if operation == "prepare" && point == "before_commit" {
			return errors.New("database unavailable")
		}
		return nil
	})
	_, _, err := PrepareTaskSubmissionAttempt(candidate, 0, "prepare-db-unreadable")
	restore()
	require.Error(t, err)
	assert.Equal(t, beforeUser, loadBillingUserQuota(t, snapshotAttempt.UserID))
	assert.Equal(t, beforeToken.RemainQuota, loadBillingToken(t, snapshotAttempt.TokenID).RemainQuota)
	attempt := loadBillingAttempt(t, "prepare-db-unreadable")
	assert.Equal(t, TaskBillingOwnerRequest, attempt.Owner)
	assert.Nil(t, attempt.TaskID)
	assert.Zero(t, attempt.RefundStartedAt)
}

func TestPrepareTaskSubmissionAmbiguousReadBackUnavailableLeavesTaskForRecovery(t *testing.T) {
	truncateTables(t)
	candidate, snapshotAttempt := preparedSubmissionFixture(t, "prepare-readback-unavailable", 100)
	preRefundUserQuota := loadBillingUserQuota(t, snapshotAttempt.UserID)
	preRefundToken := loadBillingToken(t, snapshotAttempt.TokenID)
	ambiguousErr := errors.New("ambiguous prepare commit")
	readbackErr := errors.New("primary database unavailable during prepare readback")

	restore := setTaskSubmissionFailpointForTest(func(operation, point string) error {
		if operation != "prepare" {
			return nil
		}
		switch point {
		case "after_commit":
			return ambiguousErr
		case "before_readback":
			return readbackErr
		default:
			return nil
		}
	})
	_, _, err := PrepareTaskSubmissionAttempt(candidate, 0, candidate.TaskID[len("task_"):])
	restore()
	require.ErrorIs(t, err, readbackErr)

	attempt := loadBillingAttempt(t, "prepare-readback-unavailable")
	require.Equal(t, TaskBillingOwnerTask, attempt.Owner)
	require.NotNil(t, attempt.TaskID)
	assert.Equal(t, int64(1), attempt.PrepareVersion)
	assert.Zero(t, attempt.RefundStartedAt)
	assert.Zero(t, attempt.RefundCompletedAt)
	assert.Equal(t, preRefundUserQuota, loadBillingUserQuota(t, snapshotAttempt.UserID))
	assert.Equal(t, preRefundToken.RemainQuota, loadBillingToken(t, snapshotAttempt.TokenID).RemainQuota)

	var linked Task
	require.NoError(t, DB.First(&linked, *attempt.TaskID).Error)
	assert.Equal(t, TaskStatusSubmitting, linked.Status)
	assert.Equal(t, "0%", linked.Progress)
	assert.Equal(t, snapshotAttempt.FundingAmount, linked.Quota)

	recoverable, err := ListRecoverableTaskBillingAttempts(
		taskBillingTimestamp(),
		linked.SubmitTime,
		10,
	)
	require.NoError(t, err)
	require.Len(t, recoverable, 1)
	assert.Equal(t, attempt.ID, recoverable[0].ID)

	failed, err := TransitionTaskSubmissionToFailure(
		linked.ID,
		linked.TaskID,
		"",
		"prepare_readback_unavailable",
		"prepare owner readback unavailable",
	)
	require.NoError(t, err)
	assert.Equal(t, TaskStatus(TaskStatusFailure), failed.Status)
	_, err = ApplyTaskFundingRefund(attempt.RequestID)
	require.NoError(t, err)
	_, err = ApplyTaskTokenRefund(attempt.RequestID)
	require.NoError(t, err)

	recovered := loadBillingAttempt(t, attempt.RequestID)
	assert.NotZero(t, recovered.FundingRefundedAt)
	assert.NotZero(t, recovered.TokenRefundedAt)
	assert.NotZero(t, recovered.RefundCompletedAt)
	assert.Equal(t, preRefundUserQuota+snapshotAttempt.FundingAmount, loadBillingUserQuota(t, snapshotAttempt.UserID))
	assert.Equal(
		t,
		preRefundToken.RemainQuota+snapshotAttempt.TokenAmount,
		loadBillingToken(t, snapshotAttempt.TokenID).RemainQuota,
	)
	require.NoError(t, DB.First(&linked, linked.ID).Error)
	assert.Zero(t, linked.Quota)
}

func TestPrepareTaskSubmissionRejectsFinancialDrift(t *testing.T) {
	truncateTables(t)
	candidate, _ := preparedSubmissionFixture(t, "prepare-financial-drift", 100)
	candidate.Quota++

	_, _, err := PrepareTaskSubmissionAttempt(candidate, 0, "prepare-financial-drift")
	require.Error(t, err)
	var count int64
	require.NoError(t, DB.Model(&Task{}).Where("task_id = ?", candidate.TaskID).Count(&count).Error)
	assert.Zero(t, count)
	attempt := loadBillingAttempt(t, "prepare-financial-drift")
	assert.Equal(t, TaskBillingOwnerRequest, attempt.Owner)
	assert.Nil(t, attempt.TaskID)
}

func TestPrepareTaskSubmissionRejectsBillingContextFieldDrift(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(*Task)
	}{
		{name: "nil versus present", mutate: func(candidate *Task) {
			candidate.PrivateData.BillingContext = nil
		}},
		{name: "model price", mutate: func(candidate *Task) {
			candidate.PrivateData.BillingContext.ModelPrice = 0.015
		}},
		{name: "group ratio", mutate: func(candidate *Task) {
			candidate.PrivateData.BillingContext.GroupRatio = 1.25
		}},
		{name: "model ratio", mutate: func(candidate *Task) {
			candidate.PrivateData.BillingContext.ModelRatio = 2
		}},
		{name: "other ratios", mutate: func(candidate *Task) {
			candidate.PrivateData.BillingContext.OtherRatios["resolution"] = 2.25
		}},
		{name: "origin model name", mutate: func(candidate *Task) {
			candidate.PrivateData.BillingContext.OriginModelName = "fixture-model-v2"
		}},
		{name: "per call billing", mutate: func(candidate *Task) {
			candidate.PrivateData.BillingContext.PerCallBilling = true
		}},
	}
	for index, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			truncateTables(t)
			requestID := fmt.Sprintf("prepare-billing-context-drift-%d", index)
			candidate, _ := preparedSubmissionFixture(t, requestID, 0)
			candidate.PrivateData.BillingContext =
				cloneTaskBillingContextForTest(candidate.PrivateData.BillingContext)
			testCase.mutate(candidate)

			_, _, err := PrepareTaskSubmissionAttempt(candidate, 0, requestID)
			assert.ErrorIs(t, err, ErrTaskBillingIdentityDrift)
			var count int64
			require.NoError(t, DB.Model(&Task{}).
				Where("task_id = ?", candidate.TaskID).
				Count(&count).Error)
			assert.Zero(t, count)
			attempt := loadBillingAttempt(t, requestID)
			assert.Equal(t, TaskBillingOwnerRequest, attempt.Owner)
			assert.Nil(t, attempt.TaskID)
			assert.Zero(t, attempt.PrepareVersion)
		})
	}
}

func TestPrepareTaskSubmissionRetryValidatesBillingContextAgainstAttempt(t *testing.T) {
	truncateTables(t)
	const requestID = "prepare-retry-billing-context-drift"
	candidate, _ := preparedSubmissionFixture(t, requestID, 0)
	prepared, attempt, err := PrepareTaskSubmissionAttempt(candidate, 0, requestID)
	require.NoError(t, err)
	require.Equal(t, int64(1), attempt.PrepareVersion)

	tamperedPrivate := prepared.PrivateData
	tamperedPrivate.BillingContext = cloneTaskBillingContextForTest(prepared.PrivateData.BillingContext)
	tamperedPrivate.BillingContext.GroupRatio = 3
	require.NoError(t, DB.Model(&Task{}).
		Where("id = ?", prepared.ID).
		Update("private_data", tamperedPrivate).Error)

	retry := *prepared
	retry.ChannelId = 92
	retry.PrivateData = tamperedPrivate
	retry.PrivateData.Key = "KEY_B"
	retry.Properties = prepared.Properties
	retry.Properties.UpstreamModelName = "route-b"
	_, _, err = PrepareTaskSubmissionAttempt(&retry, prepared.ID, requestID)
	assert.ErrorIs(t, err, ErrTaskBillingIdentityDrift)

	reloadedAttempt := loadBillingAttempt(t, requestID)
	assert.Equal(t, int64(1), reloadedAttempt.PrepareVersion)
	var reloaded Task
	require.NoError(t, DB.First(&reloaded, prepared.ID).Error)
	assert.Equal(t, prepared.ChannelId, reloaded.ChannelId)
	assert.Equal(t, prepared.PrivateData.Key, reloaded.PrivateData.Key)
	assert.Equal(t, prepared.Properties.UpstreamModelName, reloaded.Properties.UpstreamModelName)
}

func TestPrepareTaskSubmissionAmbiguousReadBackValidatesBillingContextAgainstAttempt(t *testing.T) {
	truncateTables(t)
	const requestID = "prepare-ambiguous-billing-context-drift"
	candidate, _ := preparedSubmissionFixture(t, requestID, 0)
	ambiguousErr := errors.New("ambiguous prepare commit")

	restore := setTaskSubmissionFailpointForTest(func(operation, point string) error {
		if operation != "prepare" || point != "after_commit" {
			return nil
		}
		attempt := loadBillingAttempt(t, requestID)
		if attempt.TaskID == nil {
			return errors.New("linked task is missing")
		}
		var linked Task
		if err := DB.First(&linked, *attempt.TaskID).Error; err != nil {
			return err
		}
		privateData := linked.PrivateData
		privateData.BillingContext = cloneTaskBillingContextForTest(linked.PrivateData.BillingContext)
		privateData.BillingContext.PerCallBilling = !privateData.BillingContext.PerCallBilling
		if err := DB.Model(&Task{}).
			Where("id = ?", linked.ID).
			Update("private_data", privateData).Error; err != nil {
			return err
		}
		return ambiguousErr
	})
	_, _, err := PrepareTaskSubmissionAttempt(candidate, 0, requestID)
	restore()
	require.Error(t, err)

	attempt := loadBillingAttempt(t, requestID)
	assert.Equal(t, TaskBillingOwnerTask, attempt.Owner)
	assert.Equal(t, int64(1), attempt.PrepareVersion)
	assert.Zero(t, attempt.RefundStartedAt)
}

func TestPrepareTaskSubmissionPreservesFirstSubmitTime(t *testing.T) {
	truncateTables(t)
	candidate, _ := preparedSubmissionFixture(t, "prepare-time", 0)
	first, _, err := PrepareTaskSubmissionAttempt(candidate, 0, "prepare-time")
	require.NoError(t, err)

	retry := *candidate
	retry.SubmitTime += 2
	retry.ChannelId = 92
	_, _, err = PrepareTaskSubmissionAttempt(&retry, first.ID, "prepare-time")
	require.Error(t, err)

	retry.SubmitTime = first.SubmitTime
	retry.PrivateData = first.PrivateData
	retry.PrivateData.Key = "KEY_B"
	retry.Properties = first.Properties
	retry.Properties.UpstreamModelName = "route-b"
	refreshed, _, err := PrepareTaskSubmissionAttempt(&retry, first.ID, "prepare-time")
	require.NoError(t, err)
	assert.Equal(t, first.SubmitTime, refreshed.SubmitTime)
	assert.Equal(t, 92, refreshed.ChannelId)
	assert.Equal(t, "KEY_B", refreshed.PrivateData.Key)
	assert.Equal(t, "route-b", refreshed.Properties.UpstreamModelName)
	assert.Equal(t, int64(2), loadBillingAttempt(t, "prepare-time").PrepareVersion)
}

func TestPrepareTaskSubmissionRetryAcceptsNoChangedRouteRow(t *testing.T) {
	truncateTables(t)
	candidate, _ := preparedSubmissionFixture(t, "prepare-retry-no-change", 0)
	prepared, _, err := PrepareTaskSubmissionAttempt(candidate, 0, "prepare-retry-no-change")
	require.NoError(t, err)

	retry := *prepared
	const callbackName = "task_submission_test_no_changed_route_row"
	emulateChangedRows := true
	require.NoError(t, DB.Callback().Update().After("gorm:update").Register(
		callbackName,
		func(tx *gorm.DB) {
			if emulateChangedRows && tx.Statement != nil && tx.Statement.Table == "tasks" {
				tx.RowsAffected = 0
			}
		},
	))
	t.Cleanup(func() {
		emulateChangedRows = false
	})

	refreshed, attempt, err := PrepareTaskSubmissionAttempt(
		&retry,
		prepared.ID,
		"prepare-retry-no-change",
	)
	require.NoError(t, err)
	require.NotNil(t, refreshed)
	require.NotNil(t, attempt)
	assert.Equal(t, prepared.ID, refreshed.ID)
	assert.Equal(t, TaskBillingOwnerTask, attempt.Owner)
	assert.Equal(t, int64(2), attempt.PrepareVersion)
}

func TestPrepareTaskSubmissionRetryRejectsPrepareVersionOverflow(t *testing.T) {
	truncateTables(t)
	const requestID = "prepare-version-overflow"
	candidate, _ := preparedSubmissionFixture(t, requestID, 0)
	prepared, _, err := PrepareTaskSubmissionAttempt(candidate, 0, requestID)
	require.NoError(t, err)
	require.NoError(t, DB.Model(&TaskBillingAttempt{}).
		Where("request_id = ?", requestID).
		Update("prepare_version", int64(math.MaxInt64)).Error)

	retry := *prepared
	retry.ChannelId = 92
	retry.PrivateData = prepared.PrivateData
	retry.PrivateData.Key = "KEY_OVERFLOW"
	retry.Properties = prepared.Properties
	retry.Properties.UpstreamModelName = "route-overflow"
	_, _, err = PrepareTaskSubmissionAttempt(&retry, prepared.ID, requestID)
	assert.ErrorIs(t, err, ErrTaskSubmissionStateConflict)

	attempt := loadBillingAttempt(t, requestID)
	assert.Equal(t, int64(math.MaxInt64), attempt.PrepareVersion)
	var reloaded Task
	require.NoError(t, DB.First(&reloaded, prepared.ID).Error)
	assert.Equal(t, prepared.ChannelId, reloaded.ChannelId)
	assert.Equal(t, prepared.PrivateData.Key, reloaded.PrivateData.Key)
	assert.Equal(t, prepared.Properties.UpstreamModelName, reloaded.Properties.UpstreamModelName)
}

func TestPrepareTaskSubmissionAmbiguousFirstCommitRejectsConcurrentRouteRefresh(t *testing.T) {
	truncateTables(t)
	const requestID = "prepare-ambiguous-concurrent-route"
	candidate, _ := preparedSubmissionFixture(t, requestID, 0)
	ambiguousErr := errors.New("ambiguous first prepare commit")
	firstCommitted := make(chan struct{})
	releaseFirstReadBack := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() {
			close(releaseFirstReadBack)
		})
	})

	var afterCommitCalls atomic.Int32
	restore := setTaskSubmissionFailpointForTest(func(operation, point string) error {
		if operation != "prepare" || point != "after_commit" {
			return nil
		}
		if afterCommitCalls.Add(1) != 1 {
			return nil
		}
		close(firstCommitted)
		<-releaseFirstReadBack
		return ambiguousErr
	})
	t.Cleanup(restore)

	type prepareResult struct {
		task    *Task
		attempt *TaskBillingAttempt
		err     error
	}
	firstResult := make(chan prepareResult, 1)
	go func() {
		task, attempt, err := PrepareTaskSubmissionAttempt(candidate, 0, requestID)
		firstResult <- prepareResult{task: task, attempt: attempt, err: err}
	}()

	select {
	case <-firstCommitted:
	case <-time.After(5 * time.Second):
		t.Fatal("first Prepare did not reach the deterministic after-commit barrier")
	}

	linkedAttempt := loadBillingAttempt(t, requestID)
	require.NotNil(t, linkedAttempt.TaskID)
	assert.Equal(t, int64(1), linkedAttempt.PrepareVersion)
	retry := *candidate
	retry.ChannelId = 92
	retry.PrivateData = candidate.PrivateData
	retry.PrivateData.Key = "KEY_B"
	retry.Properties = candidate.Properties
	retry.Properties.UpstreamModelName = "route-b"
	refreshed, refreshedAttempt, err := PrepareTaskSubmissionAttempt(
		&retry,
		*linkedAttempt.TaskID,
		requestID,
	)
	require.NoError(t, err)
	require.NotNil(t, refreshed)
	require.NotNil(t, refreshedAttempt)
	assert.Equal(t, int64(2), refreshedAttempt.PrepareVersion)

	releaseOnce.Do(func() {
		close(releaseFirstReadBack)
	})
	var ambiguousResult prepareResult
	select {
	case ambiguousResult = <-firstResult:
	case <-time.After(5 * time.Second):
		t.Fatal("ambiguous first Prepare did not finish after releasing read-back")
	}
	assert.ErrorIs(t, ambiguousResult.err, ErrTaskSubmissionStateConflict)
	assert.Nil(t, ambiguousResult.task)
	assert.Nil(t, ambiguousResult.attempt)

	finalAttempt := loadBillingAttempt(t, requestID)
	assert.Equal(t, TaskBillingOwnerTask, finalAttempt.Owner)
	assert.Equal(t, int64(2), finalAttempt.PrepareVersion)
	assert.Zero(t, finalAttempt.RefundStartedAt)
	var finalTask Task
	require.NoError(t, DB.First(&finalTask, *finalAttempt.TaskID).Error)
	assert.Equal(t, TaskStatusSubmitting, finalTask.Status)
	assert.Equal(t, "0%", finalTask.Progress)
	assert.Equal(t, 92, finalTask.ChannelId)
	assert.Equal(t, "KEY_B", finalTask.PrivateData.Key)
	assert.Equal(t, "route-b", finalTask.Properties.UpstreamModelName)
	var taskCount int64
	require.NoError(t, DB.Model(&Task{}).Where("task_id = ?", candidate.TaskID).Count(&taskCount).Error)
	assert.Equal(t, int64(1), taskCount)
}

func TestAttachTaskUpstreamResultStateMatrix(t *testing.T) {
	truncateTables(t)
	candidate, _ := preparedSubmissionFixture(t, "attach-matrix", 0)
	prepared, _, err := PrepareTaskSubmissionAttempt(candidate, 0, "attach-matrix")
	require.NoError(t, err)

	attached, err := AttachTaskUpstreamResult(prepared.ID, prepared.TaskID, "UPSTREAM_A", []byte(`{"safe":"a"}`))
	require.NoError(t, err)
	assert.Equal(t, "UPSTREAM_A", attached.PrivateData.UpstreamTaskID)
	assert.JSONEq(t, `{"safe":"a"}`, string(attached.Data))

	same, err := AttachTaskUpstreamResult(prepared.ID, prepared.TaskID, "UPSTREAM_A", []byte(`{"safe":"changed"}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"safe":"a"}`, string(same.Data))

	_, err = AttachTaskUpstreamResult(prepared.ID, prepared.TaskID, "UPSTREAM_B", nil)
	assert.ErrorIs(t, err, ErrTaskUpstreamIDConflict)
	_, err = AttachTaskUpstreamResult(prepared.ID, prepared.TaskID, "", nil)
	assert.ErrorIs(t, err, ErrTaskSubmissionStateConflict)
}

func TestCommitTaskSubmissionStateMatrix(t *testing.T) {
	truncateTables(t)
	candidate, _ := preparedSubmissionFixture(t, "commit-matrix", 0)
	prepared, _, err := PrepareTaskSubmissionAttempt(candidate, 0, "commit-matrix")
	require.NoError(t, err)

	_, err = CommitTaskSubmission(prepared.ID, prepared.TaskID)
	assert.ErrorIs(t, err, ErrTaskSubmissionStateConflict)
	_, err = AttachTaskUpstreamResult(prepared.ID, prepared.TaskID, "UPSTREAM_A", nil)
	require.NoError(t, err)
	committed, err := CommitTaskSubmission(prepared.ID, prepared.TaskID)
	require.NoError(t, err)
	assert.Equal(t, TaskStatus(TaskStatusSubmitted), committed.Status)
	assert.Equal(t, "10%", committed.Progress)
	idempotent, err := CommitTaskSubmission(prepared.ID, prepared.TaskID)
	require.NoError(t, err)
	assert.Equal(t, TaskStatus(TaskStatusSubmitted), idempotent.Status)

	require.NoError(t, DB.Model(&Task{}).Where("id = ?", prepared.ID).
		Updates(map[string]any{"status": TaskStatusFailure, "progress": "100%"}).Error)
	_, err = CommitTaskSubmission(prepared.ID, prepared.TaskID)
	assert.ErrorIs(t, err, ErrTaskSubmissionStateConflict)
}

func TestTransitionTaskSubmissionToFailureUsesNarrowUpdate(t *testing.T) {
	truncateTables(t)
	candidate, _ := preparedSubmissionFixture(t, "transition-narrow", 0)
	prepared, _, err := PrepareTaskSubmissionAttempt(candidate, 0, "transition-narrow")
	require.NoError(t, err)
	require.NoError(t, DB.Model(&Task{}).Where("id = ?", prepared.ID).Updates(map[string]any{
		"private_data": TaskPrivateData{
			Key:            "LATEST_KEY",
			ResultURL:      "LATEST_RESULT",
			BillingSource:  "",
			TokenId:        prepared.PrivateData.TokenId,
			NodeName:       "LATEST_NODE",
			BillingContext: prepared.PrivateData.BillingContext,
		},
		"data":  json.RawMessage(`{"latest":"data"}`),
		"quota": 17,
	}).Error)

	failed, err := TransitionTaskSubmissionToFailure(
		prepared.ID, prepared.TaskID, "", "timeout", "fixture timed out",
	)
	require.NoError(t, err)
	assert.Equal(t, TaskStatus(TaskStatusFailure), failed.Status)
	assert.Equal(t, "100%", failed.Progress)
	assert.Equal(t, "fixture timed out", failed.FailReason)
	assert.Equal(t, "LATEST_KEY", failed.PrivateData.Key)
	assert.Equal(t, "LATEST_RESULT", failed.PrivateData.ResultURL)
	assert.Equal(t, "LATEST_NODE", failed.PrivateData.NodeName)
	assert.JSONEq(t, `{"latest":"data"}`, string(failed.Data))
	assert.Equal(t, 17, failed.Quota)
}

func TestTransitionTaskSubmissionToFailureRejectsInvalidProgressMatrix(t *testing.T) {
	testCases := []struct {
		name     string
		status   TaskStatus
		progress string
	}{
		{name: "submitting", status: TaskStatusSubmitting, progress: "1%"},
		{name: "submitted", status: TaskStatusSubmitted, progress: "11%"},
		{name: "failure", status: TaskStatusFailure, progress: "99%"},
	}
	for index, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			truncateTables(t)
			requestID := fmt.Sprintf("failure-progress-matrix-%d", index)
			candidate, _ := preparedSubmissionFixture(t, requestID, 0)
			prepared, _, err := PrepareTaskSubmissionAttempt(candidate, 0, requestID)
			require.NoError(t, err)
			require.NoError(t, DB.Model(&Task{}).Where("id = ?", prepared.ID).
				Updates(map[string]any{
					"status":   testCase.status,
					"progress": testCase.progress,
				}).Error)

			_, err = TransitionTaskSubmissionToFailure(
				prepared.ID,
				prepared.TaskID,
				"",
				"invalid_progress",
				"must fail closed",
			)
			assert.ErrorIs(t, err, ErrTaskSubmissionStateConflict)

			var reloaded Task
			require.NoError(t, DB.First(&reloaded, prepared.ID).Error)
			assert.Equal(t, testCase.status, reloaded.Status)
			assert.Equal(t, testCase.progress, reloaded.Progress)
		})
	}
}

func TestTransitionTaskToFailureUsesExactExpectedStateAndLatestRow(t *testing.T) {
	testCases := []struct {
		name     string
		status   TaskStatus
		progress string
	}{
		{name: "queued", status: TaskStatusQueued, progress: "20%"},
		{name: "in progress", status: TaskStatusInProgress, progress: "55%"},
	}
	for index, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			truncateTables(t)
			requestID := fmt.Sprintf("polled-failure-latest-%d", index)
			candidate, _ := preparedSubmissionFixture(t, requestID, 0)
			prepared, _, err := PrepareTaskSubmissionAttempt(candidate, 0, requestID)
			require.NoError(t, err)

			latestPrivate := TaskPrivateData{
				Key:            "LATEST_KEY",
				UpstreamTaskID: "LATEST_UPSTREAM",
				ResultURL:      "LATEST_RESULT",
				BillingSource:  prepared.PrivateData.BillingSource,
				SubscriptionId: prepared.PrivateData.SubscriptionId,
				TokenId:        prepared.PrivateData.TokenId,
				NodeName:       "LATEST_NODE",
				BillingContext: &TaskBillingContext{
					ModelPrice:      9,
					GroupRatio:      8,
					ModelRatio:      7,
					OtherRatios:     map[string]float64{"latest": 6},
					OriginModelName: "LATEST_MODEL",
					PerCallBilling:  true,
				},
			}
			latestProperties := Properties{
				Input:             "LATEST_INPUT",
				UpstreamModelName: "LATEST_UPSTREAM_MODEL",
				OriginModelName:   "LATEST_ORIGIN_MODEL",
			}
			require.NoError(t, DB.Model(&Task{}).Where("id = ?", prepared.ID).
				Updates(map[string]any{
					"status":       testCase.status,
					"progress":     testCase.progress,
					"private_data": latestPrivate,
					"properties":   latestProperties,
					"data":         json.RawMessage(`{"latest":"before-poll"}`),
				}).Error)

			polledData := json.RawMessage(`{"sanitized":"failure"}`)
			failed, err := TransitionTaskToFailure(
				prepared.ID,
				prepared.TaskID,
				TaskFailureTransition{
					ExpectedStatus:   testCase.status,
					ExpectedProgress: testCase.progress,
					Code:             "provider_failure",
					Message:          "provider failed",
					Data:             &polledData,
				},
			)
			require.NoError(t, err)
			assert.Equal(t, TaskStatus(TaskStatusFailure), failed.Status)
			assert.Equal(t, "100%", failed.Progress)
			assert.Equal(t, "LATEST_KEY", failed.PrivateData.Key)
			assert.Equal(t, "LATEST_UPSTREAM", failed.PrivateData.UpstreamTaskID)
			assert.Equal(t, "LATEST_RESULT", failed.PrivateData.ResultURL)
			assert.Equal(t, "LATEST_NODE", failed.PrivateData.NodeName)
			require.NotNil(t, failed.PrivateData.BillingContext)
			assert.Equal(t, "LATEST_MODEL", failed.PrivateData.BillingContext.OriginModelName)
			assert.Equal(t, latestProperties, failed.Properties)
			assert.JSONEq(t, `{"sanitized":"failure"}`, string(failed.Data))
		})
	}
}

func TestTransitionTaskToFailureRejectsExpectedProgressDrift(t *testing.T) {
	truncateTables(t)
	candidate, _ := preparedSubmissionFixture(t, "polled-failure-progress-drift", 0)
	prepared, _, err := PrepareTaskSubmissionAttempt(candidate, 0, "polled-failure-progress-drift")
	require.NoError(t, err)
	require.NoError(t, DB.Model(&Task{}).Where("id = ?", prepared.ID).
		Updates(map[string]any{
			"status":   TaskStatusQueued,
			"progress": "25%",
		}).Error)

	_, err = TransitionTaskToFailure(
		prepared.ID,
		prepared.TaskID,
		TaskFailureTransition{
			ExpectedStatus:   TaskStatusQueued,
			ExpectedProgress: "20%",
			Code:             "stale_poll",
			Message:          "must not win",
		},
	)
	assert.ErrorIs(t, err, ErrTaskSubmissionStateConflict)

	var reloaded Task
	require.NoError(t, DB.First(&reloaded, prepared.ID).Error)
	assert.Equal(t, TaskStatus(TaskStatusQueued), reloaded.Status)
	assert.Equal(t, "25%", reloaded.Progress)
}

func TestFailureTransitionPreservesConcurrentAttachedID(t *testing.T) {
	truncateTables(t)
	candidate, _ := preparedSubmissionFixture(t, "transition-race", 0)
	prepared, _, err := PrepareTaskSubmissionAttempt(candidate, 0, "transition-race")
	require.NoError(t, err)

	var staleDiscovery Task
	require.NoError(t, DB.First(&staleDiscovery, prepared.ID).Error)
	assert.Empty(t, staleDiscovery.PrivateData.UpstreamTaskID)
	_, err = AttachTaskUpstreamResult(prepared.ID, prepared.TaskID, "UPSTREAM_AFTER_DISCOVERY", []byte(`{"new":"data"}`))
	require.NoError(t, err)
	require.NoError(t, DB.Model(&Task{}).Where("id = ?", prepared.ID).
		Update("private_data", TaskPrivateData{
			Key:            "KEY_AFTER_ATTACH",
			UpstreamTaskID: "UPSTREAM_AFTER_DISCOVERY",
			ResultURL:      "RESULT_AFTER_ATTACH",
			TokenId:        prepared.PrivateData.TokenId,
			NodeName:       "NODE_AFTER_ATTACH",
			BillingContext: prepared.PrivateData.BillingContext,
		}).Error)

	failed, err := TransitionTaskSubmissionToFailure(
		staleDiscovery.ID, staleDiscovery.TaskID, staleDiscovery.PrivateData.UpstreamTaskID,
		"timeout", "timed out",
	)
	require.NoError(t, err)
	assert.Equal(t, TaskStatus(TaskStatusFailure), failed.Status)
	assert.Equal(t, "UPSTREAM_AFTER_DISCOVERY", failed.PrivateData.UpstreamTaskID)
	assert.Equal(t, "KEY_AFTER_ATTACH", failed.PrivateData.Key)
	assert.Equal(t, "RESULT_AFTER_ATTACH", failed.PrivateData.ResultURL)
	assert.Equal(t, "NODE_AFTER_ATTACH", failed.PrivateData.NodeName)
	assert.JSONEq(t, `{"new":"data"}`, string(failed.Data))
}

func TestReconciliationAttachCanFillFailureWithoutRevival(t *testing.T) {
	truncateTables(t)
	candidate, _ := preparedSubmissionFixture(t, "reconcile-attach", 0)
	prepared, _, err := PrepareTaskSubmissionAttempt(candidate, 0, "reconcile-attach")
	require.NoError(t, err)
	require.NoError(t, DB.Model(&Task{}).Where("id = ?", prepared.ID).Updates(map[string]any{
		"status":      TaskStatusFailure,
		"progress":    "100%",
		"finish_time": int64(1_750_000_123),
		"fail_reason": "persist failed",
		"quota":       77,
	}).Error)

	reconciled, err := AttachTaskUpstreamResultForReconciliation(
		prepared.ID, prepared.TaskID, prepared.ChannelId, "UPSTREAM_RECONCILED",
	)
	require.NoError(t, err)
	assert.Equal(t, "UPSTREAM_RECONCILED", reconciled.PrivateData.UpstreamTaskID)
	assert.Equal(t, TaskStatus(TaskStatusFailure), reconciled.Status)
	assert.Equal(t, "100%", reconciled.Progress)
	assert.Equal(t, int64(1_750_000_123), reconciled.FinishTime)
	assert.Equal(t, "persist failed", reconciled.FailReason)
	assert.Equal(t, 77, reconciled.Quota)
}

func TestAttachAndCommitAmbiguityUsePrimaryReadBack(t *testing.T) {
	truncateTables(t)
	candidate, _ := preparedSubmissionFixture(t, "attach-commit-ambiguous", 0)
	prepared, _, err := PrepareTaskSubmissionAttempt(candidate, 0, "attach-commit-ambiguous")
	require.NoError(t, err)

	var attachHookCalls atomic.Int32
	restore := setTaskSubmissionFailpointForTest(func(operation, point string) error {
		if operation == "attach" && point == "after_commit" {
			attachHookCalls.Add(1)
			return errors.New("ambiguous attach commit")
		}
		return nil
	})
	attached, err := AttachTaskUpstreamResult(
		prepared.ID,
		prepared.TaskID,
		"UPSTREAM_AMBIGUOUS",
		[]byte(`{"safe":"attached"}`),
	)
	restore()
	require.NoError(t, err)
	assert.Equal(t, int32(1), attachHookCalls.Load())
	assert.Equal(t, "UPSTREAM_AMBIGUOUS", attached.PrivateData.UpstreamTaskID)

	var commitHookCalls atomic.Int32
	restore = setTaskSubmissionFailpointForTest(func(operation, point string) error {
		if operation == "commit" && point == "after_commit" {
			commitHookCalls.Add(1)
			return errors.New("ambiguous submission commit")
		}
		return nil
	})
	committed, err := CommitTaskSubmission(prepared.ID, prepared.TaskID)
	restore()
	require.NoError(t, err)
	assert.Equal(t, int32(1), commitHookCalls.Load())
	assert.Equal(t, TaskStatus(TaskStatusSubmitted), committed.Status)
	assert.Equal(t, "10%", committed.Progress)
}

func TestSubmissionSettledAttemptCanLaterRefund(t *testing.T) {
	truncateTables(t)
	candidate, _ := preparedSubmissionFixture(t, "submission-settled-refund", 100)
	prepared, _, err := PrepareTaskSubmissionAttempt(candidate, 0, "submission-settled-refund")
	require.NoError(t, err)
	_, err = AttachTaskUpstreamResult(
		prepared.ID, prepared.TaskID, "UPSTREAM_SETTLED_REFUND", nil,
	)
	require.NoError(t, err)
	_, err = CommitTaskSubmission(prepared.ID, prepared.TaskID)
	require.NoError(t, err)
	require.NoError(t, MarkTaskBillingAttemptSubmissionSettled("submission-settled-refund"))

	failed, err := TransitionTaskSubmissionToFailure(
		prepared.ID,
		prepared.TaskID,
		"UPSTREAM_SETTLED_REFUND",
		"provider_failed",
		"provider failed asynchronously",
	)
	require.NoError(t, err)
	_, err = ApplyTaskFundingRefund("submission-settled-refund")
	require.NoError(t, err)
	_, err = ApplyTaskTokenRefund("submission-settled-refund")
	require.NoError(t, err)

	attempt := loadBillingAttempt(t, "submission-settled-refund")
	assert.NotZero(t, attempt.SubmissionSettledAt)
	assert.Zero(t, attempt.SucceededAt)
	assert.NotZero(t, attempt.RefundCompletedAt)
	require.NoError(t, DB.First(failed, failed.ID).Error)
	assert.Zero(t, failed.Quota)
}

func TestMarkTaskBillingAttemptSubmissionSettledAcceptsPolledForwardState(t *testing.T) {
	testCases := []struct {
		name     string
		status   TaskStatus
		progress string
	}{
		{name: "queued", status: TaskStatusQueued, progress: "20%"},
		{name: "in progress", status: TaskStatusInProgress, progress: "50%"},
		{name: "success", status: TaskStatusSuccess, progress: "100%"},
	}
	for index, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			truncateTables(t)
			requestID := fmt.Sprintf("submission-settled-forward-%d", index)
			candidate, _ := preparedSubmissionFixture(t, requestID, 0)
			prepared, _, err := PrepareTaskSubmissionAttempt(candidate, 0, requestID)
			require.NoError(t, err)
			_, err = AttachTaskUpstreamResult(
				prepared.ID,
				prepared.TaskID,
				fmt.Sprintf("UPSTREAM_FORWARD_%d", index),
				nil,
			)
			require.NoError(t, err)
			_, err = CommitTaskSubmission(prepared.ID, prepared.TaskID)
			require.NoError(t, err)
			require.NoError(t, DB.Model(&Task{}).Where("id = ?", prepared.ID).
				Updates(map[string]any{
					"status":   testCase.status,
					"progress": testCase.progress,
				}).Error)

			require.NoError(t, MarkTaskBillingAttemptSubmissionSettled(requestID))
			attempt := loadBillingAttempt(t, requestID)
			assert.NotZero(t, attempt.SubmissionSettledAt)
			assert.Zero(t, attempt.SucceededAt)
		})
	}
}

func TestMarkTaskBillingAttemptSucceededRequiresFinalTaskSuccess(t *testing.T) {
	truncateTables(t)
	candidate, _ := preparedSubmissionFixture(t, "final-task-success", 100)
	prepared, _, err := PrepareTaskSubmissionAttempt(candidate, 0, "final-task-success")
	require.NoError(t, err)
	_, err = AttachTaskUpstreamResult(
		prepared.ID, prepared.TaskID, "UPSTREAM_FINAL_SUCCESS", nil,
	)
	require.NoError(t, err)
	_, err = CommitTaskSubmission(prepared.ID, prepared.TaskID)
	require.NoError(t, err)
	require.NoError(t, MarkTaskBillingAttemptSubmissionSettled("final-task-success"))

	err = MarkTaskBillingAttemptSucceeded("final-task-success")
	assert.ErrorIs(t, err, ErrTaskBillingAttemptState)
	require.NoError(t, DB.Model(&Task{}).Where("id = ?", prepared.ID).
		Updates(map[string]any{
			"status":   TaskStatusSuccess,
			"progress": "100%",
		}).Error)
	require.NoError(t, MarkTaskBillingAttemptSucceeded("final-task-success"))
	require.NoError(t, MarkTaskBillingAttemptSucceeded("final-task-success"))

	attempt := loadBillingAttempt(t, "final-task-success")
	assert.NotZero(t, attempt.SucceededAt)
	assert.Zero(t, attempt.RefundStartedAt)
	_, err = ApplyTaskFundingRefund("final-task-success")
	assert.ErrorIs(t, err, ErrTaskBillingAttemptState)
}
