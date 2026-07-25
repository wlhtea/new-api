package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestSeedDanceReconciliationPayloadWhitelist(t *testing.T) {
	payloadType := reflect.TypeOf(SeedDanceSubmitReconciliationPayload{})
	require.Equal(t, 7, payloadType.NumField())
	wantFields := map[string]string{
		"PublicTaskID":     "public_task_id",
		"UpstreamTaskID":   "upstream_task_id",
		"PersistentTaskID": "persistent_task_id",
		"ChannelID":        "channel_id",
		"NodeName":         "node_name",
		"ErrorCode":        "error_code",
		"ObservedAt":       "observed_at",
	}
	for index := 0; index < payloadType.NumField(); index++ {
		field := payloadType.Field(index)
		assert.Equal(t, wantFields[field.Name], field.Tag.Get("json"))
	}

	encoded, err := json.Marshal(SeedDanceSubmitReconciliationPayload{
		PublicTaskID:     "task_public",
		UpstreamTaskID:   "UPSTREAM_INTERNAL",
		PersistentTaskID: 11,
		ChannelID:        59,
		NodeName:         "node-a",
		ErrorCode:        "persist_failed",
		ObservedAt:       1_750_003_000,
	})
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(encoded, &fields))
	assert.Len(t, fields, 7)
	for _, forbidden := range []string{
		"user", "platform", "key", "token", "prompt", "image",
		"base64", "task_data", "response",
	} {
		assert.NotContains(t, strings.ToLower(string(encoded)), forbidden)
	}
}

func TestSeedDanceReconciliationPayloadRejectsAdditionalJSONFields(t *testing.T) {
	systemTask := &model.SystemTask{Payload: `{
		"public_task_id":"task_public",
		"upstream_task_id":"UPSTREAM_INTERNAL",
		"persistent_task_id":11,
		"channel_id":59,
		"node_name":"node-a",
		"error_code":"persist_failed",
		"observed_at":1750003000,
		"key":"MUST_BE_REJECTED"
	}`}
	_, err := DecodeSeedDanceSubmitReconciliationPayload(systemTask)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown field")
}

func TestSeedDanceReconciliationActiveKeyFormula(t *testing.T) {
	publicTaskID := "task_active_key_formula"
	sum := sha256.Sum256([]byte(publicTaskID))
	want := fmt.Sprintf("sd-submit:%x", sum[:16])
	assert.Equal(t, want, SeedDanceSubmitReconciliationActiveKey(publicTaskID))
}

func TestSeedDanceReconciliationCreationConvergesByActiveKey(t *testing.T) {
	truncate(t)
	payload := SeedDanceSubmitReconciliationPayload{
		PublicTaskID:     "task_create_reconciliation",
		UpstreamTaskID:   "UPSTREAM_CREATE",
		PersistentTaskID: 101,
		ChannelID:        59,
		NodeName:         "node-a",
		ErrorCode:        "persist_failed",
		ObservedAt:       1_750_003_001,
	}
	first, err := CreateSeedDanceSubmitReconciliation(payload)
	require.NoError(t, err)
	second, err := CreateSeedDanceSubmitReconciliation(payload)
	require.NoError(t, err)
	assert.Equal(t, first.TaskID, second.TaskID)
	require.NotNil(t, first.ActiveKey)
	assert.Equal(t, SeedDanceSubmitReconciliationActiveKey(payload.PublicTaskID), *first.ActiveKey)

	var decoded SeedDanceSubmitReconciliationPayload
	require.NoError(t, first.DecodePayload(&decoded))
	assert.Equal(t, payload, decoded)

	conflict := payload
	conflict.UpstreamTaskID = "UPSTREAM_DIFFERENT"
	existing, err := CreateSeedDanceSubmitReconciliation(conflict)
	require.ErrorIs(t, err, model.ErrTaskUpstreamIDConflict)
	require.NotNil(t, existing)
	assert.Equal(t, first.TaskID, existing.TaskID)
	reloaded, readErr := model.GetSystemTaskByTaskID(first.TaskID)
	require.NoError(t, readErr)
	require.NotNil(t, reloaded)
	require.NoError(t, reloaded.DecodePayload(&decoded))
	assert.Equal(t, payload.UpstreamTaskID, decoded.UpstreamTaskID)
}

func TestFailAndRefundUsesDurableRequestOwner(t *testing.T) {
	truncate(t)
	attempt := seedDurableBillingAttempt(t, "fail-request-owner", 8201, 9201, 120)

	err := FailAndRefundTaskSubmission(
		context.Background(),
		0,
		attempt.RequestID,
		"",
		nil,
		"submit_failed",
		"submission failed",
	)
	require.NoError(t, err)

	refunded, err := model.GetTaskBillingAttemptByRequestID(attempt.RequestID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskBillingOwnerRequest, refunded.Owner)
	assert.NotZero(t, refunded.FundingRefundedAt)
	assert.NotZero(t, refunded.TokenRefundedAt)
	assert.NotZero(t, refunded.RefundCompletedAt)
	assert.Equal(t, 10_000, getUserQuota(t, attempt.UserID))
	assert.Equal(t, 10_000, getTokenRemainQuota(t, attempt.TokenID))
}

func TestFailAndRefundUsesDurableTaskOwner(t *testing.T) {
	truncate(t)
	attempt := seedDurableBillingAttempt(t, "fail-task-owner", 8202, 9202, 130)
	task := linkDurableBillingAttempt(t, attempt, attempt.RequestID)

	err := FailAndRefundTaskSubmission(
		context.Background(),
		task.ID,
		attempt.RequestID,
		"UPSTREAM_TASK_OWNER",
		[]byte(`{"safe":"task-data"}`),
		"submit_failed",
		"submission failed",
	)
	require.NoError(t, err)

	failed, err := model.GetTaskByPrimaryID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), failed.Status)
	assert.Equal(t, "100%", failed.Progress)
	assert.Equal(t, "UPSTREAM_TASK_OWNER", failed.PrivateData.UpstreamTaskID)
	assert.Zero(t, failed.Quota)

	refunded, err := model.GetTaskBillingAttemptByRequestID(attempt.RequestID)
	require.NoError(t, err)
	assert.NotZero(t, refunded.FundingRefundedAt)
	assert.NotZero(t, refunded.TokenRefundedAt)
	assert.NotZero(t, refunded.RefundCompletedAt)
	assert.Zero(t, refunded.SucceededAt)
}

func TestFailAndRefundPreservesConcurrentAttachedID(t *testing.T) {
	truncate(t)
	attempt := seedDurableBillingAttempt(t, "fail-preserve-attached", 8203, 9203, 140)
	task := linkDurableBillingAttempt(t, attempt, attempt.RequestID)
	_, err := model.AttachTaskUpstreamResult(
		task.ID,
		task.TaskID,
		"UPSTREAM_CONCURRENT",
		[]byte(`{"safe":"concurrent"}`),
	)
	require.NoError(t, err)

	err = FailAndRefundTaskSubmission(
		context.Background(),
		task.ID,
		attempt.RequestID,
		"",
		nil,
		"submit_failed",
		"submission failed",
	)
	require.NoError(t, err)

	failed, err := model.GetTaskByPrimaryID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, "UPSTREAM_CONCURRENT", failed.PrivateData.UpstreamTaskID)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), failed.Status)
	assert.Equal(t, "100%", failed.Progress)
}

func TestFailAndRefundTreatsTransitionConvergenceAsSuccess(t *testing.T) {
	truncate(t)
	attempt := seedDurableBillingAttempt(t, "fail-transition-converges", 8205, 9205, 160)
	task := linkDurableBillingAttempt(t, attempt, attempt.RequestID)
	require.NoError(t, model.DB.Exec(`
		CREATE TRIGGER fail_strict_attach_only
		BEFORE UPDATE OF private_data ON tasks
		WHEN NEW.status != 'FAILURE'
		BEGIN
			SELECT RAISE(FAIL, 'forced strict attach failure');
		END
	`).Error)

	err := FailAndRefundTaskSubmission(
		context.Background(),
		task.ID,
		attempt.RequestID,
		"UPSTREAM_TRANSITION_CONVERGED",
		[]byte(`{"safe":"data"}`),
		"submit_failed",
		"submission failed",
	)
	require.NoError(t, err)

	failed, err := model.GetTaskByPrimaryID(task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), failed.Status)
	assert.Equal(t, "UPSTREAM_TRANSITION_CONVERGED", failed.PrivateData.UpstreamTaskID)
	assert.Zero(t, failed.Quota)
}

func TestOwnerLookupDBErrorDoesNotRefund(t *testing.T) {
	truncate(t)
	attempt := seedDurableBillingAttempt(t, "fail-owner-db-error", 8204, 9204, 150)
	walletAfterPreconsume := getUserQuota(t, attempt.UserID)
	tokenAfterPreconsume := getTokenRemainQuota(t, attempt.TokenID)

	primaryDB := model.DB
	closedDB, err := gorm.Open(
		sqlite.Open(fmt.Sprintf("file:closed-owner-%s?mode=memory&cache=shared", attempt.RequestID)),
		&gorm.Config{},
	)
	require.NoError(t, err)
	sqlDB, err := closedDB.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	model.DB = closedDB
	err = FailAndRefundTaskSubmission(
		context.Background(),
		0,
		attempt.RequestID,
		"",
		nil,
		"submit_failed",
		"submission failed",
	)
	model.DB = primaryDB

	require.Error(t, err)
	assert.False(t, errors.Is(err, gorm.ErrRecordNotFound))
	assert.Equal(t, walletAfterPreconsume, getUserQuota(t, attempt.UserID))
	assert.Equal(t, tokenAfterPreconsume, getTokenRemainQuota(t, attempt.TokenID))
	reloaded, readErr := model.GetTaskBillingAttemptByRequestID(attempt.RequestID)
	require.NoError(t, readErr)
	assert.Zero(t, reloaded.RefundStartedAt)
	assert.Zero(t, reloaded.RefundCompletedAt)
}
