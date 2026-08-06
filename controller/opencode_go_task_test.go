package controller

import (
	"context"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCodeGoRefreshHandlerConfigurationIsBoundedAndRequiresStableCrypto(t *testing.T) {
	previousConfigured := common.CryptoSecretExplicitlyConfigured
	t.Cleanup(func() { common.CryptoSecretExplicitlyConfigured = previousConfigured })
	t.Setenv("OPENCODE_GO_REFRESH_TASK_ENABLED", "true")
	t.Setenv("OPENCODE_GO_REFRESH_TASK_INTERVAL_MINUTES", "0")
	t.Setenv("OPENCODE_GO_REFRESH_CONCURRENCY", "999")

	handler := openCodeGoRefreshHandler{}
	common.CryptoSecretExplicitlyConfigured = false
	assert.False(t, handler.Enabled())
	common.CryptoSecretExplicitlyConfigured = true
	assert.True(t, handler.Enabled())
	assert.Equal(t, time.Minute, handler.Interval())
	assert.Equal(t, service.OpenCodeGoMaxRefreshConcurrency, configuredOpenCodeGoRefreshConcurrency())
	payload, ok := handler.NewPayload().(openCodeGoRefreshTaskPayload)
	require.True(t, ok)
	assert.True(t, payload.Scheduled)
	assert.Equal(t, service.OpenCodeGoMaxRefreshConcurrency, payload.Concurrency)
}

func TestOpenCodeGoRiskRecheckConfigurationIsBounded(t *testing.T) {
	t.Setenv("OPENCODE_GO_RISK_RECHECK_CONCURRENCY", "999")
	t.Setenv("OPENCODE_GO_RISK_RECHECK_BATCH_SIZE", "999999")
	assert.Equal(t, service.OpenCodeGoMaxRiskRecheckConcurrency, configuredOpenCodeGoRiskRecheckConcurrency())
	assert.Equal(t, 5000, configuredOpenCodeGoRiskRecheckBatchSize())
	assert.Equal(t, model.SystemTaskTypeOpenCodeGoRiskRecheck, openCodeGoRiskRecheckHandler{}.Type())
}

func TestOpenCodeGoRefreshHandlerPersistsZeroTargetProgressAndResult(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(
		&model.SystemTask{},
		&model.SystemTaskLock{},
		&model.OpenCodeGoIdentity{},
		&model.OpenCodeGoWorkspace{},
		&model.OpenCodeGoQuotaWindow{},
		&model.OpenCodeGoWorkspaceModel{},
		&model.OpenCodeGoOperation{},
	))
	previousSecret := common.CryptoSecret
	previousConfigured := common.CryptoSecretExplicitlyConfigured
	common.CryptoSecret = "controller-opencode-go-task-test-secret"
	common.CryptoSecretExplicitlyConfigured = true
	t.Cleanup(func() {
		common.CryptoSecret = previousSecret
		common.CryptoSecretExplicitlyConfigured = previousConfigured
	})

	channel := &model.Channel{
		Type:   constant.ChannelTypeOpenCodeGo,
		Name:   "OpenCode Go empty refresh pool",
		Group:  "default",
		Status: common.ChannelStatusAutoDisabled,
	}
	require.NoError(t, db.Create(channel).Error)
	task, err := model.CreateSystemTaskWithActiveKey(
		model.SystemTaskTypeOpenCodeGoRefresh,
		"opencode_go_refresh:test-empty",
		openCodeGoRefreshTaskPayload{ChannelID: channel.Id, Concurrency: 2},
		nil,
	)
	require.NoError(t, err)
	const runnerID = "opencode-go-test-runner"
	claimed, acquired, err := model.ClaimSystemTask(
		task.ID,
		task.Type,
		runnerID,
		common.GetTimestamp()+60,
	)
	require.NoError(t, err)
	require.True(t, acquired)

	openCodeGoRefreshHandler{}.Run(context.Background(), claimed, runnerID)

	finished, err := model.GetSystemTaskByTaskID(task.TaskID)
	require.NoError(t, err)
	require.NotNil(t, finished)
	assert.Equal(t, model.SystemTaskStatusSucceeded, finished.Status)
	var progress service.SystemTaskProgress
	require.NoError(t, finished.DecodeState(&progress))
	assert.Equal(t, 100, progress.Progress)
	assert.Zero(t, progress.Total)
	assert.Zero(t, progress.Processed)
	var summary service.OpenCodeGoRefreshSummary
	require.NoError(t, common.UnmarshalJsonStr(finished.Result, &summary))
	assert.Zero(t, summary.Total)
	assert.Zero(t, summary.Failed)
}

func TestOpenCodeGoRiskRecheckHandlerPersistsZeroTargetProgressAndResult(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(
		&model.SystemTask{},
		&model.SystemTaskLock{},
		&model.OpenCodeGoIdentity{},
		&model.OpenCodeGoWorkspace{},
		&model.OpenCodeGoQuotaWindow{},
		&model.OpenCodeGoWorkspaceModel{},
		&model.OpenCodeGoOperation{},
	))
	previousSecret := common.CryptoSecret
	previousConfigured := common.CryptoSecretExplicitlyConfigured
	common.CryptoSecret = "controller-opencode-go-risk-task-test-secret"
	common.CryptoSecretExplicitlyConfigured = true
	t.Cleanup(func() {
		common.CryptoSecret = previousSecret
		common.CryptoSecretExplicitlyConfigured = previousConfigured
	})

	channel := &model.Channel{
		Type:   constant.ChannelTypeOpenCodeGo,
		Name:   "OpenCode Go empty risk pool",
		Group:  "default",
		Status: common.ChannelStatusAutoDisabled,
	}
	require.NoError(t, db.Create(channel).Error)
	task, err := model.CreateSystemTaskWithActiveKey(
		model.SystemTaskTypeOpenCodeGoRiskRecheck,
		"opencode_go_risk_recheck:test-empty",
		openCodeGoRiskRecheckTaskPayload{ChannelID: channel.Id, Concurrency: 2},
		nil,
	)
	require.NoError(t, err)
	const runnerID = "opencode-go-risk-test-runner"
	claimed, acquired, err := model.ClaimSystemTask(
		task.ID,
		task.Type,
		runnerID,
		common.GetTimestamp()+60,
	)
	require.NoError(t, err)
	require.True(t, acquired)

	openCodeGoRiskRecheckHandler{}.Run(context.Background(), claimed, runnerID)

	finished, err := model.GetSystemTaskByTaskID(task.TaskID)
	require.NoError(t, err)
	require.NotNil(t, finished)
	assert.Equal(t, model.SystemTaskStatusSucceeded, finished.Status)
	var progress service.SystemTaskProgress
	require.NoError(t, finished.DecodeState(&progress))
	assert.Equal(t, 100, progress.Progress)
	assert.Zero(t, progress.Total)
	var summary service.OpenCodeGoRiskRecheckSummary
	require.NoError(t, common.UnmarshalJsonStr(finished.Result, &summary))
	assert.Zero(t, summary.Total)
	assert.Zero(t, summary.Failed)
}
