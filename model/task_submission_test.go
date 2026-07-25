package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func preparedSubmissionFixture(t *testing.T, requestID string, quota int) (*Task, *TaskBillingAttempt) {
	t.Helper()
	snapshot := billingSnapshot(requestID, quota)
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
			BillingContext: &TaskBillingContext{
				GroupRatio:      1,
				OriginModelName: "fixture-model",
				OtherRatios:     map[string]float64{"seconds": 1},
			},
		},
		Data: json.RawMessage(`{"latest":"initial"}`),
	}
	return candidate, attempt
}

func TestPrepareTaskSubmissionLinksAttemptAndTransfersOwnerAtomically(t *testing.T) {
	truncateTables(t)
	candidate, _ := preparedSubmissionFixture(t, "prepare-link", 100)

	prepared, attempt, err := PrepareTaskSubmissionAttempt(candidate, 0, "prepare-link")
	require.NoError(t, err)
	assert.NotZero(t, prepared.ID)
	require.NotNil(t, attempt.TaskID)
	assert.Equal(t, prepared.ID, *attempt.TaskID)
	assert.Equal(t, TaskBillingOwnerTask, attempt.Owner)
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
