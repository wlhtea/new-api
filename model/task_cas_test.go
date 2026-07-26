package model

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestMain(m *testing.M) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		panic("failed to open test db: " + err.Error())
	}
	DB = db
	LOG_DB = db

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	common.BatchUpdateEnabled = false
	common.LogConsumeEnabled = true
	initCol()

	sqlDB, err := db.DB()
	if err != nil {
		panic("failed to get sql.DB: " + err.Error())
	}
	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(
		&Task{},
		&TaskBillingAttempt{},
		&User{},
		&UserSession{},
		&AuthFlow{},
		&ExternalIdentityClaim{},
		&Token{},
		&PasskeyCredential{},
		&TwoFA{},
		&TwoFABackupCode{},
		&Log{},
		&Channel{},
		&QuotaData{},
		&Ability{},
		&TopUp{},
		&SubscriptionPlan{},
		&SubscriptionOrder{},
		&UserSubscription{},
		&SubscriptionPreConsumeRecord{},
		&UserOAuthBinding{},
		&PerfMetric{},
		&SystemInstance{},
		&SystemTask{},
		&SystemTaskLock{},
	); err != nil {
		panic("failed to migrate: " + err.Error())
	}

	os.Exit(m.Run())
}

func truncateTables(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		DB.Exec("DELETE FROM task_billing_attempts")
		DB.Exec("DELETE FROM subscription_pre_consume_records")
		DB.Exec("DELETE FROM tasks")
		DB.Exec("DELETE FROM auth_flows")
		DB.Exec("DELETE FROM external_identity_claims")
		DB.Exec("DELETE FROM user_sessions")
		DB.Exec("DELETE FROM passkey_credentials")
		DB.Exec("DELETE FROM two_fa_backup_codes")
		DB.Exec("DELETE FROM two_fas")
		DB.Exec("DELETE FROM tokens")
		DB.Exec("DELETE FROM user_oauth_bindings")
		DB.Exec("DELETE FROM users")
		DB.Exec("DELETE FROM logs")
		DB.Exec("DELETE FROM channels")
		DB.Exec("DELETE FROM quota_data")
		DB.Exec("DELETE FROM abilities")
		DB.Exec("DELETE FROM top_ups")
		DB.Exec("DELETE FROM subscription_orders")
		DB.Exec("DELETE FROM subscription_plans")
		DB.Exec("DELETE FROM user_subscriptions")
		DB.Exec("DELETE FROM perf_metrics")
		DB.Exec("DELETE FROM system_instances")
		DB.Exec("DELETE FROM system_task_locks")
		DB.Exec("DELETE FROM system_tasks")
	})
}

func insertTask(t *testing.T, task *Task) {
	t.Helper()
	task.CreatedAt = time.Now().Unix()
	task.UpdatedAt = time.Now().Unix()
	require.NoError(t, DB.Create(task).Error)
}

// ---------------------------------------------------------------------------
// Snapshot / Equal — pure logic tests (no DB)
// ---------------------------------------------------------------------------

func TestSnapshotEqual_Same(t *testing.T) {
	s := taskSnapshot{
		Status:     TaskStatusInProgress,
		Progress:   "50%",
		StartTime:  1000,
		FinishTime: 0,
		FailReason: "",
		ResultURL:  "",
		Data:       json.RawMessage(`{"key":"value"}`),
	}
	assert.True(t, s.Equal(s))
}

func TestSnapshotEqual_DifferentStatus(t *testing.T) {
	a := taskSnapshot{Status: TaskStatusInProgress, Data: json.RawMessage(`{}`)}
	b := taskSnapshot{Status: TaskStatusSuccess, Data: json.RawMessage(`{}`)}
	assert.False(t, a.Equal(b))
}

func TestSnapshotEqual_DifferentProgress(t *testing.T) {
	a := taskSnapshot{Status: TaskStatusInProgress, Progress: "30%", Data: json.RawMessage(`{}`)}
	b := taskSnapshot{Status: TaskStatusInProgress, Progress: "60%", Data: json.RawMessage(`{}`)}
	assert.False(t, a.Equal(b))
}

func TestSnapshotEqual_DifferentData(t *testing.T) {
	a := taskSnapshot{Status: TaskStatusInProgress, Data: json.RawMessage(`{"a":1}`)}
	b := taskSnapshot{Status: TaskStatusInProgress, Data: json.RawMessage(`{"a":2}`)}
	assert.False(t, a.Equal(b))
}

func TestSnapshotEqual_NilVsEmpty(t *testing.T) {
	a := taskSnapshot{Status: TaskStatusInProgress, Data: nil}
	b := taskSnapshot{Status: TaskStatusInProgress, Data: json.RawMessage{}}
	// bytes.Equal(nil, []byte{}) == true
	assert.True(t, a.Equal(b))
}

func TestSnapshot_Roundtrip(t *testing.T) {
	task := &Task{
		Status:     TaskStatusInProgress,
		Progress:   "42%",
		StartTime:  1234,
		FinishTime: 5678,
		FailReason: "timeout",
		PrivateData: TaskPrivateData{
			ResultURL: "https://example.com/result.mp4",
		},
		Data: json.RawMessage(`{"model":"test-model"}`),
	}
	snap := task.Snapshot()
	assert.Equal(t, task.Status, snap.Status)
	assert.Equal(t, task.Progress, snap.Progress)
	assert.Equal(t, task.StartTime, snap.StartTime)
	assert.Equal(t, task.FinishTime, snap.FinishTime)
	assert.Equal(t, task.FailReason, snap.FailReason)
	assert.Equal(t, task.PrivateData.ResultURL, snap.ResultURL)
	assert.JSONEq(t, string(task.Data), string(snap.Data))
}

// ---------------------------------------------------------------------------
// UpdateWithStatus CAS — DB integration tests
// ---------------------------------------------------------------------------

func TestUpdateWithStatus_Win(t *testing.T) {
	truncateTables(t)

	task := &Task{
		TaskID:   "task_cas_win",
		Status:   TaskStatusInProgress,
		Progress: "50%",
		Data:     json.RawMessage(`{}`),
	}
	insertTask(t, task)

	task.Status = TaskStatusSuccess
	task.Progress = "100%"
	won, err := task.UpdateWithStatus(TaskStatusInProgress)
	require.NoError(t, err)
	assert.True(t, won)

	var reloaded Task
	require.NoError(t, DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, TaskStatusSuccess, reloaded.Status)
	assert.Equal(t, "100%", reloaded.Progress)
}

func TestUpdateWithStatus_Lose(t *testing.T) {
	truncateTables(t)

	task := &Task{
		TaskID: "task_cas_lose",
		Status: TaskStatusFailure,
		Data:   json.RawMessage(`{}`),
	}
	insertTask(t, task)

	task.Status = TaskStatusSuccess
	won, err := task.UpdateWithStatus(TaskStatusInProgress) // wrong fromStatus
	require.NoError(t, err)
	assert.False(t, won)

	var reloaded Task
	require.NoError(t, DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, TaskStatusFailure, reloaded.Status) // unchanged
}

func TestUpdateWithStatus_ConcurrentWinner(t *testing.T) {
	truncateTables(t)

	task := &Task{
		TaskID: "task_cas_race",
		Status: TaskStatusInProgress,
		Quota:  1000,
		Data:   json.RawMessage(`{}`),
	}
	insertTask(t, task)

	const goroutines = 5
	wins := make([]bool, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			t := &Task{}
			*t = Task{
				ID:       task.ID,
				TaskID:   task.TaskID,
				Status:   TaskStatusSuccess,
				Progress: "100%",
				Quota:    task.Quota,
				Data:     json.RawMessage(`{}`),
			}
			t.CreatedAt = task.CreatedAt
			t.UpdatedAt = time.Now().Unix()
			won, err := t.UpdateWithStatus(TaskStatusInProgress)
			if err == nil {
				wins[idx] = won
			}
		}(i)
	}
	wg.Wait()

	winCount := 0
	for _, w := range wins {
		if w {
			winCount++
		}
	}
	assert.Equal(t, 1, winCount, "exactly one goroutine should win the CAS")
}

func TestTransitionTaskToSuccessLocksAttemptAndUpdatesNarrowColumns(t *testing.T) {
	truncateTables(t)
	candidate, _ := preparedSubmissionFixture(t, "poll-final-success", 100)
	prepared, _, err := PrepareTaskSubmissionAttempt(candidate, 0, "poll-final-success")
	require.NoError(t, err)
	_, err = AttachTaskUpstreamResult(
		prepared.ID,
		prepared.TaskID,
		"UPSTREAM_FINAL_SUCCESS",
		json.RawMessage(`{"status":"accepted"}`),
	)
	require.NoError(t, err)
	prepared, err = CommitTaskSubmission(prepared.ID, prepared.TaskID)
	require.NoError(t, err)
	require.NoError(t, MarkTaskBillingAttemptSubmissionSettled("poll-final-success"))
	require.NoError(t, DB.Model(&Task{}).Where("id = ?", prepared.ID).
		Updates(map[string]any{
			"status":   TaskStatusInProgress,
			"progress": "30%",
			"private_data": TaskPrivateData{
				Key:            "LATEST_STORED_KEY",
				UpstreamTaskID: "UPSTREAM_FINAL_SUCCESS",
				BillingSource:  prepared.PrivateData.BillingSource,
				SubscriptionId: prepared.PrivateData.SubscriptionId,
				TokenId:        prepared.PrivateData.TokenId,
				NodeName:       "latest-node",
				BillingContext: prepared.PrivateData.BillingContext,
			},
		}).Error)

	data := json.RawMessage(`{"status":"completed"}`)
	transitioned, attempt, err := TransitionTaskToSuccess(
		prepared.ID,
		prepared.TaskID,
		TaskSuccessTransition{
			ExpectedStatus:   TaskStatusInProgress,
			ExpectedProgress: "30%",
			ResultURL:        "/v1/videos/" + prepared.TaskID + "/content",
			Data:             &data,
		},
	)
	require.NoError(t, err)
	require.NotNil(t, attempt)
	assert.Equal(t, "poll-final-success", attempt.RequestID)
	assert.Equal(t, TaskStatus(TaskStatusSuccess), transitioned.Status)
	assert.Equal(t, "100%", transitioned.Progress)
	assert.Equal(t, "LATEST_STORED_KEY", transitioned.PrivateData.Key)
	assert.Equal(t, "latest-node", transitioned.PrivateData.NodeName)
	assert.Equal(
		t,
		"/v1/videos/"+prepared.TaskID+"/content",
		transitioned.PrivateData.ResultURL,
	)
	assert.JSONEq(t, string(data), string(transitioned.Data))

	reloadedAttempt := loadBillingAttempt(t, "poll-final-success")
	assert.NotZero(t, reloadedAttempt.SubmissionSettledAt)
	assert.Zero(t, reloadedAttempt.SucceededAt)
}

func TestTransitionTaskToSuccessRejectsLostStateAndRefundConflict(t *testing.T) {
	tests := []struct {
		name         string
		mutateTask   map[string]any
		mutateLedger map[string]any
	}{
		{
			name:       "lost expected state",
			mutateTask: map[string]any{"progress": "40%"},
		},
		{
			name: "refund started",
			mutateLedger: map[string]any{
				"refund_started_at": time.Now().Unix(),
			},
		},
		{
			name: "refund completed",
			mutateLedger: map[string]any{
				"refund_started_at":   time.Now().Unix(),
				"refund_completed_at": time.Now().Unix(),
			},
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			truncateTables(t)
			requestID := "poll-success-conflict-" + string(rune('a'+index))
			candidate, _ := preparedSubmissionFixture(t, requestID, 0)
			prepared, attempt, err := PrepareTaskSubmissionAttempt(candidate, 0, requestID)
			require.NoError(t, err)
			_, err = AttachTaskUpstreamResult(
				prepared.ID,
				prepared.TaskID,
				"UPSTREAM_CONFLICT",
				nil,
			)
			require.NoError(t, err)
			prepared, err = CommitTaskSubmission(prepared.ID, prepared.TaskID)
			require.NoError(t, err)
			require.NoError(t, MarkTaskBillingAttemptSubmissionSettled(requestID))
			require.NoError(t, DB.Model(&Task{}).Where("id = ?", prepared.ID).
				Updates(map[string]any{
					"status":   TaskStatusInProgress,
					"progress": "30%",
				}).Error)
			if len(test.mutateTask) > 0 {
				require.NoError(t, DB.Model(&Task{}).Where("id = ?", prepared.ID).
					Updates(test.mutateTask).Error)
			}
			if len(test.mutateLedger) > 0 {
				require.NoError(t, DB.Model(&TaskBillingAttempt{}).
					Where("id = ?", attempt.ID).
					Updates(test.mutateLedger).Error)
			}

			_, _, err = TransitionTaskToSuccess(
				prepared.ID,
				prepared.TaskID,
				TaskSuccessTransition{
					ExpectedStatus:   TaskStatusInProgress,
					ExpectedProgress: "30%",
					ResultURL:        "/v1/videos/" + prepared.TaskID + "/content",
				},
			)
			require.Error(t, err)

			var reloaded Task
			require.NoError(t, DB.First(&reloaded, prepared.ID).Error)
			assert.NotEqual(t, TaskStatus(TaskStatusSuccess), reloaded.Status)
			reloadedAttempt := loadBillingAttempt(t, requestID)
			assert.Zero(t, reloadedAttempt.SucceededAt)
		})
	}
}

func TestTransitionTaskPollingStateUsesLatestRowAndNarrowColumns(t *testing.T) {
	truncateTables(t)
	candidate, _ := preparedSubmissionFixture(t, "poll-nonterminal-narrow", 100)
	prepared, _, err := PrepareTaskSubmissionAttempt(
		candidate,
		0,
		"poll-nonterminal-narrow",
	)
	require.NoError(t, err)
	_, err = AttachTaskUpstreamResult(
		prepared.ID,
		prepared.TaskID,
		"UPSTREAM_NONTERMINAL",
		json.RawMessage(`{"status":"accepted"}`),
	)
	require.NoError(t, err)
	prepared, err = CommitTaskSubmission(prepared.ID, prepared.TaskID)
	require.NoError(t, err)
	require.NoError(t, MarkTaskBillingAttemptSubmissionSettled("poll-nonterminal-narrow"))

	var stale Task
	require.NoError(t, DB.First(&stale, prepared.ID).Error)
	latestPrivate := stale.PrivateData
	latestPrivate.Key = "LATEST_STORED_KEY"
	latestPrivate.NodeName = "latest-node"
	latestProperties := Properties{
		Input:             "latest-input",
		UpstreamModelName: "latest-route",
		OriginModelName:   stale.Properties.OriginModelName,
	}
	require.NoError(t, DB.Model(&Task{}).Where("id = ?", stale.ID).
		Updates(map[string]any{
			"private_data": latestPrivate,
			"properties":   latestProperties,
		}).Error)

	data := json.RawMessage(`{"status":"processing","requestId":"REQUEST_ID"}`)
	transitioned, err := TransitionTaskPollingState(
		stale.ID,
		stale.TaskID,
		TaskPollingTransition{
			ExpectedStatus:   TaskStatusSubmitted,
			ExpectedProgress: "10%",
			Status:           TaskStatusInProgress,
			Progress:         "30%",
			Data:             &data,
		},
	)
	require.NoError(t, err)
	assert.Equal(t, TaskStatus(TaskStatusInProgress), transitioned.Status)
	assert.Equal(t, "30%", transitioned.Progress)
	assert.NotZero(t, transitioned.StartTime)
	assert.Equal(t, "LATEST_STORED_KEY", transitioned.PrivateData.Key)
	assert.Equal(t, "latest-node", transitioned.PrivateData.NodeName)
	assert.Equal(t, latestProperties, transitioned.Properties)
	assert.Equal(t, stale.Quota, transitioned.Quota)
	assert.JSONEq(t, string(data), string(transitioned.Data))
}

func TestTransitionTaskPollingStateIsIdempotentAndRejectsLostState(t *testing.T) {
	t.Run("same state and data is an idempotent no-op", func(t *testing.T) {
		truncateTables(t)
		candidate, _ := preparedSubmissionFixture(t, "poll-nonterminal-idempotent", 100)
		prepared, _, err := PrepareTaskSubmissionAttempt(
			candidate,
			0,
			"poll-nonterminal-idempotent",
		)
		require.NoError(t, err)
		_, err = AttachTaskUpstreamResult(
			prepared.ID,
			prepared.TaskID,
			"UPSTREAM_NONTERMINAL_IDEMPOTENT",
			nil,
		)
		require.NoError(t, err)
		prepared, err = CommitTaskSubmission(prepared.ID, prepared.TaskID)
		require.NoError(t, err)
		require.NoError(
			t,
			MarkTaskBillingAttemptSubmissionSettled("poll-nonterminal-idempotent"),
		)

		data := json.RawMessage(`{"status":"processing"}`)
		first, err := TransitionTaskPollingState(
			prepared.ID,
			prepared.TaskID,
			TaskPollingTransition{
				ExpectedStatus:   TaskStatusSubmitted,
				ExpectedProgress: "10%",
				Status:           TaskStatusInProgress,
				Progress:         "30%",
				Data:             &data,
			},
		)
		require.NoError(t, err)
		firstUpdatedAt := first.UpdatedAt
		firstStartTime := first.StartTime

		second, err := TransitionTaskPollingState(
			prepared.ID,
			prepared.TaskID,
			TaskPollingTransition{
				ExpectedStatus:   TaskStatusInProgress,
				ExpectedProgress: "30%",
				Status:           TaskStatusInProgress,
				Progress:         "30%",
				Data:             &data,
			},
		)
		require.NoError(t, err)
		assert.Equal(t, firstUpdatedAt, second.UpdatedAt)
		assert.Equal(t, firstStartTime, second.StartTime)
		assert.JSONEq(t, string(data), string(second.Data))
	})

	t.Run("lost expected progress leaves the row unchanged", func(t *testing.T) {
		truncateTables(t)
		candidate, _ := preparedSubmissionFixture(t, "poll-nonterminal-lost", 100)
		prepared, _, err := PrepareTaskSubmissionAttempt(
			candidate,
			0,
			"poll-nonterminal-lost",
		)
		require.NoError(t, err)
		_, err = AttachTaskUpstreamResult(
			prepared.ID,
			prepared.TaskID,
			"UPSTREAM_NONTERMINAL_LOST",
			nil,
		)
		require.NoError(t, err)
		prepared, err = CommitTaskSubmission(prepared.ID, prepared.TaskID)
		require.NoError(t, err)
		require.NoError(
			t,
			MarkTaskBillingAttemptSubmissionSettled("poll-nonterminal-lost"),
		)
		require.NoError(t, DB.Model(&Task{}).Where("id = ?", prepared.ID).
			Update("progress", "40%").Error)

		data := json.RawMessage(`{"status":"processing"}`)
		_, err = TransitionTaskPollingState(
			prepared.ID,
			prepared.TaskID,
			TaskPollingTransition{
				ExpectedStatus:   TaskStatusSubmitted,
				ExpectedProgress: "10%",
				Status:           TaskStatusInProgress,
				Progress:         "30%",
				Data:             &data,
			},
		)
		require.ErrorIs(t, err, ErrTaskSubmissionStateConflict)

		var reloaded Task
		require.NoError(t, DB.First(&reloaded, prepared.ID).Error)
		assert.Equal(t, TaskStatus(TaskStatusSubmitted), reloaded.Status)
		assert.Equal(t, "40%", reloaded.Progress)
		assert.NotEqual(t, string(data), string(reloaded.Data))
	})
}

func TestTransitionDurableTaskToFailureRejectsCompleteFinancialIdentityDrift(
	t *testing.T,
) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, task *Task)
	}{
		{
			name: "user id",
			mutate: func(t *testing.T, task *Task) {
				require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
					Update("user_id", task.UserId+1).Error)
			},
		},
		{
			name: "submit time",
			mutate: func(t *testing.T, task *Task) {
				require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
					Update("submit_time", task.SubmitTime+1).Error)
			},
		},
		{
			name: "quota and funding amount",
			mutate: func(t *testing.T, task *Task) {
				require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
					Update("quota", task.Quota+1).Error)
			},
		},
		{
			name: "billing source",
			mutate: func(t *testing.T, task *Task) {
				var current Task
				require.NoError(t, DB.First(&current, task.ID).Error)
				current.PrivateData.BillingSource = "drift-source"
				require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
					Update("private_data", current.PrivateData).Error)
			},
		},
		{
			name: "subscription id",
			mutate: func(t *testing.T, task *Task) {
				var current Task
				require.NoError(t, DB.First(&current, task.ID).Error)
				current.PrivateData.SubscriptionId++
				require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
					Update("private_data", current.PrivateData).Error)
			},
		},
		{
			name: "token id",
			mutate: func(t *testing.T, task *Task) {
				var current Task
				require.NoError(t, DB.First(&current, task.ID).Error)
				current.PrivateData.TokenId++
				require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
					Update("private_data", current.PrivateData).Error)
			},
		},
		{
			name: "billing context digest",
			mutate: func(t *testing.T, task *Task) {
				var current Task
				require.NoError(t, DB.First(&current, task.ID).Error)
				require.NotNil(t, current.PrivateData.BillingContext)
				current.PrivateData.BillingContext.GroupRatio++
				require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
					Update("private_data", current.PrivateData).Error)
			},
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			truncateTables(t)
			requestID := "poll-failure-identity-" + string(rune('a'+index))
			candidate, _ := preparedSubmissionFixture(t, requestID, 100)
			prepared, attempt, err := PrepareTaskSubmissionAttempt(
				candidate,
				0,
				requestID,
			)
			require.NoError(t, err)
			_, err = AttachTaskUpstreamResult(
				prepared.ID,
				prepared.TaskID,
				"UPSTREAM_FAILURE_IDENTITY",
				nil,
			)
			require.NoError(t, err)
			prepared, err = CommitTaskSubmission(prepared.ID, prepared.TaskID)
			require.NoError(t, err)
			require.NoError(
				t,
				MarkTaskBillingAttemptSubmissionSettled(requestID),
			)
			test.mutate(t, prepared)

			_, err = TransitionDurableTaskToFailure(
				prepared.ID,
				prepared.TaskID,
				TaskFailureTransition{
					ExpectedStatus:   TaskStatusSubmitted,
					ExpectedProgress: "10%",
					Code:             "provider_failure",
					Message:          "safe provider failure",
				},
			)
			require.ErrorIs(t, err, ErrTaskBillingIdentityDrift)

			var reloaded Task
			require.NoError(t, DB.First(&reloaded, prepared.ID).Error)
			assert.Equal(t, TaskStatus(TaskStatusSubmitted), reloaded.Status)
			assert.Equal(t, "10%", reloaded.Progress)
			reloadedAttempt := loadBillingAttempt(t, requestID)
			assert.Equal(t, attempt.ID, reloadedAttempt.ID)
			assert.Zero(t, reloadedAttempt.SucceededAt)
			assert.Zero(t, reloadedAttempt.FundingRefundedAt)
			assert.Zero(t, reloadedAttempt.TokenRefundedAt)
			assert.Zero(t, reloadedAttempt.RefundStartedAt)
			assert.Zero(t, reloadedAttempt.RefundCompletedAt)
		})
	}
}

func prepareFinalSuccessMarkerFixture(
	t *testing.T,
	requestID string,
	submissionSettled bool,
) (*Task, *TaskBillingAttempt) {
	t.Helper()
	candidate, _ := preparedSubmissionFixture(t, requestID, 100)
	prepared, attempt, err := PrepareTaskSubmissionAttempt(candidate, 0, requestID)
	require.NoError(t, err)
	_, err = AttachTaskUpstreamResult(
		prepared.ID,
		prepared.TaskID,
		"UPSTREAM_FINAL_MARKER",
		json.RawMessage(`{"status":"accepted"}`),
	)
	require.NoError(t, err)
	prepared, err = CommitTaskSubmission(prepared.ID, prepared.TaskID)
	require.NoError(t, err)
	if submissionSettled {
		require.NoError(t, MarkTaskBillingAttemptSubmissionSettled(requestID))
	}
	require.NoError(t, DB.Model(&Task{}).
		Where("id = ? AND task_id = ?", prepared.ID, prepared.TaskID).
		Updates(map[string]any{
			"status":   TaskStatusSuccess,
			"progress": "100%",
		}).Error)
	return prepared, attempt
}

func TestMarkTaskBillingAttemptSucceededRequiresSubmissionSettlement(t *testing.T) {
	truncateTables(t)
	_, _ = prepareFinalSuccessMarkerFixture(
		t,
		"final-marker-requires-submission-settlement",
		false,
	)

	err := MarkTaskBillingAttemptSucceeded(
		"final-marker-requires-submission-settlement",
	)
	require.Error(t, err)
	assert.Zero(
		t,
		loadBillingAttempt(t, "final-marker-requires-submission-settlement").SucceededAt,
	)
}

func TestMarkTaskBillingAttemptSucceededRejectsCompleteFinancialIdentityDrift(
	t *testing.T,
) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, task *Task, attempt *TaskBillingAttempt)
	}{
		{
			name: "task public id",
			mutate: func(t *testing.T, task *Task, _ *TaskBillingAttempt) {
				require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
					Update("task_id", task.TaskID+"-drift").Error)
			},
		},
		{
			name: "attempt task link",
			mutate: func(t *testing.T, task *Task, attempt *TaskBillingAttempt) {
				require.NoError(t, DB.Model(&TaskBillingAttempt{}).
					Where("id = ?", attempt.ID).
					Update("task_id", task.ID+1000).Error)
			},
		},
		{
			name: "user id",
			mutate: func(t *testing.T, task *Task, _ *TaskBillingAttempt) {
				require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
					Update("user_id", task.UserId+1).Error)
			},
		},
		{
			name: "submit time",
			mutate: func(t *testing.T, task *Task, _ *TaskBillingAttempt) {
				require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
					Update("submit_time", task.SubmitTime+1).Error)
			},
		},
		{
			name: "quota and funding amount",
			mutate: func(t *testing.T, task *Task, _ *TaskBillingAttempt) {
				require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
					Update("quota", task.Quota+1).Error)
			},
		},
		{
			name: "billing source",
			mutate: func(t *testing.T, task *Task, _ *TaskBillingAttempt) {
				var current Task
				require.NoError(t, DB.First(&current, task.ID).Error)
				current.PrivateData.BillingSource = "drift-source"
				require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
					Update("private_data", current.PrivateData).Error)
			},
		},
		{
			name: "subscription id",
			mutate: func(t *testing.T, task *Task, _ *TaskBillingAttempt) {
				var current Task
				require.NoError(t, DB.First(&current, task.ID).Error)
				current.PrivateData.SubscriptionId++
				require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
					Update("private_data", current.PrivateData).Error)
			},
		},
		{
			name: "token id",
			mutate: func(t *testing.T, task *Task, _ *TaskBillingAttempt) {
				var current Task
				require.NoError(t, DB.First(&current, task.ID).Error)
				current.PrivateData.TokenId++
				require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
					Update("private_data", current.PrivateData).Error)
			},
		},
		{
			name: "billing context digest",
			mutate: func(t *testing.T, task *Task, _ *TaskBillingAttempt) {
				var current Task
				require.NoError(t, DB.First(&current, task.ID).Error)
				require.NotNil(t, current.PrivateData.BillingContext)
				current.PrivateData.BillingContext.GroupRatio++
				require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
					Update("private_data", current.PrivateData).Error)
			},
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			truncateTables(t)
			requestID := "final-marker-identity-" + string(rune('a'+index))
			task, attempt := prepareFinalSuccessMarkerFixture(
				t,
				requestID,
				true,
			)
			test.mutate(t, task, attempt)

			err := MarkTaskBillingAttemptSucceeded(requestID)
			require.Error(t, err)
			assert.Zero(t, loadBillingAttempt(t, requestID).SucceededAt)
		})
	}
}

func TestMarkTaskBillingAttemptSucceededRejectsEveryRefundMarker(t *testing.T) {
	now := time.Now().Unix()
	tests := []struct {
		name    string
		updates map[string]any
	}{
		{
			name:    "funding component only",
			updates: map[string]any{"funding_refunded_at": now},
		},
		{
			name:    "token component only",
			updates: map[string]any{"token_refunded_at": now},
		},
		{
			name:    "refund started",
			updates: map[string]any{"refund_started_at": now},
		},
		{
			name:    "refund completed aggregate only",
			updates: map[string]any{"refund_completed_at": now},
		},
		{
			name: "coherent completed refund",
			updates: map[string]any{
				"funding_refunded_at": now,
				"token_refunded_at":   now,
				"refund_started_at":   now,
				"refund_completed_at": now,
			},
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			truncateTables(t)
			requestID := "final-marker-refund-" + string(rune('a'+index))
			_, attempt := prepareFinalSuccessMarkerFixture(t, requestID, true)
			require.NoError(t, DB.Model(&TaskBillingAttempt{}).
				Where("id = ?", attempt.ID).
				Updates(test.updates).Error)

			err := MarkTaskBillingAttemptSucceeded(requestID)
			require.Error(t, err)
			assert.Zero(t, loadBillingAttempt(t, requestID).SucceededAt)
		})
	}
}

func TestMarkTaskBillingAttemptSucceededGuardedUpdateRejectsLostState(
	t *testing.T,
) {
	truncateTables(t)
	requestID := "final-marker-guarded-update"
	_, attempt := prepareFinalSuccessMarkerFixture(t, requestID, true)

	const callbackName = "test:final_marker_guarded_update_conflict"
	callbackCalls := 0
	require.NoError(t, DB.Callback().Update().
		Before("gorm:update").
		Register(callbackName, func(tx *gorm.DB) {
			if tx.Statement.Schema == nil ||
				tx.Statement.Schema.Table != "task_billing_attempts" {
				return
			}
			callbackCalls++
			_, err := tx.Statement.ConnPool.ExecContext(
				tx.Statement.Context,
				"UPDATE task_billing_attempts SET funding_refunded_at = ? WHERE id = ?",
				time.Now().Unix(),
				attempt.ID,
			)
			if err != nil {
				tx.AddError(err)
			}
		}))
	t.Cleanup(func() {
		DB.Callback().Update().Remove(callbackName)
	})

	beforeReadbackCalls := 0
	restore := setTaskSubmissionFailpointForTest(
		func(operation, point string) error {
			if operation == "mark_success" && point == "before_readback" {
				beforeReadbackCalls++
				return errors.New("read-back unavailable")
			}
			return nil
		},
	)
	t.Cleanup(restore)

	err := MarkTaskBillingAttemptSucceeded(requestID)
	require.Error(t, err)
	assert.Equal(t, 1, callbackCalls)
	assert.Equal(t, 1, beforeReadbackCalls)
	reloaded := loadBillingAttempt(t, requestID)
	assert.Zero(t, reloaded.FundingRefundedAt)
	assert.Zero(t, reloaded.SucceededAt)
}

func TestMarkTaskBillingAttemptSucceededReadsBackAfterCommitError(t *testing.T) {
	truncateTables(t)
	requestID := "final-marker-after-commit"
	_, _ = prepareFinalSuccessMarkerFixture(t, requestID, true)

	afterCommitCalls := 0
	restore := setTaskSubmissionFailpointForTest(
		func(operation, point string) error {
			if operation == "mark_success" && point == "after_commit" {
				afterCommitCalls++
				return errors.New("ambiguous marker commit")
			}
			return nil
		},
	)
	t.Cleanup(restore)

	require.NoError(t, MarkTaskBillingAttemptSucceeded(requestID))
	assert.Equal(t, 1, afterCommitCalls)
	assert.NotZero(t, loadBillingAttempt(t, requestID).SucceededAt)
}

func TestMarkTaskBillingAttemptSucceededIdempotenceRevalidatesFullState(
	t *testing.T,
) {
	truncateTables(t)
	requestID := "final-marker-idempotent-revalidation"
	task, _ := prepareFinalSuccessMarkerFixture(t, requestID, true)

	require.NoError(t, MarkTaskBillingAttemptSucceeded(requestID))
	firstSucceededAt := loadBillingAttempt(t, requestID).SucceededAt
	require.NotZero(t, firstSucceededAt)
	require.NoError(t, MarkTaskBillingAttemptSucceeded(requestID))
	assert.Equal(t, firstSucceededAt, loadBillingAttempt(t, requestID).SucceededAt)

	require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
		Update("quota", task.Quota+1).Error)
	err := MarkTaskBillingAttemptSucceeded(requestID)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTaskBillingIdentityDrift)
	assert.Equal(t, firstSucceededAt, loadBillingAttempt(t, requestID).SucceededAt)
}

func TestRecoverableFinalSuccessQueryMatchesTerminalGuards(t *testing.T) {
	t.Run("active final success keeps the scheduler alive until marked", func(t *testing.T) {
		truncateTables(t)
		requestID := "recoverable-final-success-active"
		task, _ := prepareFinalSuccessMarkerFixture(t, requestID, true)
		require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
			Update("platform", constant.TaskPlatform("59")).Error)

		tasks, err := ListRecoverableFinalSuccessTasks(10)
		require.NoError(t, err)
		require.Len(t, tasks, 1)
		assert.Equal(t, task.ID, tasks[0].ID)
		hasWork, err := HasRecoverableFinalSuccessTasks()
		require.NoError(t, err)
		assert.True(t, hasWork)
		assert.True(t, HasTaskPollingWork())

		require.NoError(t, MarkTaskBillingAttemptSucceeded(requestID))
		tasks, err = ListRecoverableFinalSuccessTasks(10)
		require.NoError(t, err)
		assert.Empty(t, tasks)
		hasWork, err = HasRecoverableFinalSuccessTasks()
		require.NoError(t, err)
		assert.False(t, hasWork)
		assert.False(t, HasTaskPollingWork())
	})

	refundMarkers := []string{
		"funding_refunded_at",
		"token_refunded_at",
		"refund_started_at",
		"refund_completed_at",
	}
	for index, marker := range refundMarkers {
		t.Run("excludes "+marker, func(t *testing.T) {
			truncateTables(t)
			requestID := "recoverable-final-success-refund-" +
				string(rune('a'+index))
			task, attempt := prepareFinalSuccessMarkerFixture(
				t,
				requestID,
				true,
			)
			require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
				Update("platform", constant.TaskPlatform("59")).Error)
			require.NoError(t, DB.Model(&TaskBillingAttempt{}).
				Where("id = ?", attempt.ID).
				Update(marker, time.Now().Unix()).Error)

			tasks, err := ListRecoverableFinalSuccessTasks(10)
			require.NoError(t, err)
			assert.Empty(t, tasks)
			hasWork, err := HasRecoverableFinalSuccessTasks()
			require.NoError(t, err)
			assert.False(t, hasWork)
		})
	}

	t.Run("requires submission settlement", func(t *testing.T) {
		truncateTables(t)
		task, _ := prepareFinalSuccessMarkerFixture(
			t,
			"recoverable-final-success-unsettled",
			false,
		)
		require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
			Update("platform", constant.TaskPlatform("59")).Error)

		tasks, err := ListRecoverableFinalSuccessTasks(10)
		require.NoError(t, err)
		assert.Empty(t, tasks)
		hasWork, err := HasRecoverableFinalSuccessTasks()
		require.NoError(t, err)
		assert.False(t, hasWork)
	})
}

func TestClaimQuotaForRefund_OnlyOneClaimSucceeds(t *testing.T) {
	truncateTables(t)

	task := &Task{
		TaskID: "task_refund_claim",
		Status: TaskStatusFailure,
		Quota:  1000,
		Data:   json.RawMessage(`{}`),
	}
	insertTask(t, task)

	claimed, err := ClaimQuotaForRefund(task.ID, task.Quota)
	require.NoError(t, err)
	assert.True(t, claimed)

	claimed, err = ClaimQuotaForRefund(task.ID, task.Quota)
	require.NoError(t, err)
	assert.False(t, claimed)

	var reloaded Task
	require.NoError(t, DB.First(&reloaded, task.ID).Error)
	assert.Zero(t, reloaded.Quota)
}

func TestGetUnrefundedFailedTasks_FiltersAndLimits(t *testing.T) {
	truncateTables(t)

	tasks := []*Task{
		{TaskID: "failed_refundable_1", Status: TaskStatusFailure, Quota: 100, SubmitTime: TaskRefundLegacyCutoff, Data: json.RawMessage(`{}`)},
		{TaskID: "failed_refundable_2", Status: TaskStatusFailure, Quota: 200, SubmitTime: TaskRefundLegacyCutoff + 1, Data: json.RawMessage(`{}`)},
		{TaskID: "legacy_failed", Status: TaskStatusFailure, Quota: 400, SubmitTime: TaskRefundLegacyCutoff - 1, Data: json.RawMessage(`{}`)},
		{TaskID: "failed_without_quota", Status: TaskStatusFailure, Quota: 0, Data: json.RawMessage(`{}`)},
		{TaskID: "successful_with_quota", Status: TaskStatusSuccess, Quota: 300, Data: json.RawMessage(`{}`)},
	}
	for _, task := range tasks {
		insertTask(t, task)
	}

	updatedBefore := time.Now().Unix() + 1
	found := GetUnrefundedFailedTasks(updatedBefore, 1)
	require.Len(t, found, 1)
	assert.Equal(t, tasks[0].ID, found[0].ID)

	found = GetUnrefundedFailedTasks(updatedBefore, 10)
	require.Len(t, found, 2)
	assert.Equal(t, []int64{tasks[0].ID, tasks[1].ID}, []int64{found[0].ID, found[1].ID})

	assert.Empty(t, GetUnrefundedFailedTasks(updatedBefore, 0))
}

func TestRestoreQuotaAfterFailedRefund_OnlyRestoresClaimedMarker(t *testing.T) {
	truncateTables(t)

	task := &Task{
		TaskID: "task_refund_restore",
		Status: TaskStatusFailure,
		Quota:  750,
		Data:   json.RawMessage(`{}`),
	}
	insertTask(t, task)

	claimed, err := ClaimQuotaForRefund(task.ID, task.Quota)
	require.NoError(t, err)
	require.True(t, claimed)

	restored, err := RestoreQuotaAfterFailedRefund(task.ID, task.Quota)
	require.NoError(t, err)
	assert.True(t, restored)

	restored, err = RestoreQuotaAfterFailedRefund(task.ID, task.Quota)
	require.NoError(t, err)
	assert.False(t, restored)

	var reloaded Task
	require.NoError(t, DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, task.Quota, reloaded.Quota)
}

func TestHasTaskPollingWork_IncludesOnlyRefundableFailedTasks(t *testing.T) {
	truncateTables(t)
	assert.False(t, HasTaskPollingWork())

	legacy := &Task{
		TaskID:     "legacy_failed_work",
		Status:     TaskStatusFailure,
		Progress:   "100%",
		Quota:      500,
		SubmitTime: TaskRefundLegacyCutoff - 1,
		Data:       json.RawMessage(`{}`),
	}
	insertTask(t, legacy)
	assert.False(t, HasTaskPollingWork())

	refundable := &Task{
		TaskID:     "refundable_failed_work",
		Status:     TaskStatusFailure,
		Progress:   "100%",
		Quota:      500,
		SubmitTime: TaskRefundLegacyCutoff,
		Data:       json.RawMessage(`{}`),
	}
	insertTask(t, refundable)
	assert.True(t, HasTaskPollingWork())
}

func TestHasTaskPollingWorkIncludesAttemptMarkerRecovery(t *testing.T) {
	truncateTables(t)
	snapshot := TaskBillingAttemptSnapshot{
		RequestID:     "polling-attempt-recovery",
		PublicTaskID:  "task_polling_attempt_recovery",
		SubmitTime:    time.Now().Add(-time.Minute).Unix(),
		UserID:        981,
		FundingSource: "wallet",
		FundingAmount: 0,
		TokenID:       982,
		TokenAmount:   0,
	}
	require.NoError(t, DB.Create(&User{
		Id:       snapshot.UserID,
		Username: "polling-attempt-user",
		AffCode:  "polling-attempt-aff",
	}).Error)
	require.NoError(t, DB.Create(&Token{
		Id:          snapshot.TokenID,
		UserId:      snapshot.UserID,
		Key:         "polling-attempt-token",
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
	}).Error)
	attempt, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	_, err = ApplyTaskTokenPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	assert.False(t, HasTaskPollingWork(), "fresh request-owned attempts wait for the recovery grace period")

	require.NoError(t, DB.Model(&TaskBillingAttempt{}).Where("id = ?", attempt.ID).
		Update("updated_at", time.Now().Add(-time.Minute).Unix()).Error)
	assert.True(t, HasTaskPollingWork())
}
