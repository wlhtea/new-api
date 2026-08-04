package controller

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

const (
	openCodeGoImportBodyLimit           = int64(2 * 1024 * 1024)
	openCodeGoAdminBodyLimit            = int64(64 * 1024)
	openCodeGoCancelRenewalConfirmation = "CANCEL RENEWAL"
)

type openCodeGoImportRequest struct {
	Label       string `json:"label"`
	AuthCookies string `json:"auth_cookies"`
}

type openCodeGoIdentityUpdateRequest struct {
	Label string `json:"label"`
}

type openCodeGoCookieReplaceRequest struct {
	AuthCookie string `json:"auth_cookie"`
}

type openCodeGoWorkspaceEnabledRequest struct {
	Enabled *bool `json:"enabled"`
}

type openCodeGoLifecyclePolicyUpdateRequest struct {
	AutoEnableChinaModels         bool `json:"auto_enable_china_models"`
	AutoApplyReferralRewards      bool `json:"auto_apply_referral_rewards"`
	ReferralRewardsMaxPerRun      int  `json:"referral_rewards_max_per_run"`
	AutoCancelSubscriptionRenewal bool `json:"auto_cancel_subscription_renewal"`
}

type openCodeGoCancelRenewalRequest struct {
	Confirmation string `json:"confirmation"`
}

func GetOpenCodeGoPool(c *gin.Context) {
	channelID, ok := openCodeGoChannelID(c)
	if !ok {
		return
	}
	view, err := service.GetOpenCodeGoPoolView(channelID)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, view)
}

func GetOpenCodeGoLifecyclePolicy(c *gin.Context) {
	channelID, ok := openCodeGoChannelID(c)
	if !ok {
		return
	}
	policy, err := service.GetOpenCodeGoLifecyclePolicy(channelID)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, policy)
}

func UpdateOpenCodeGoLifecyclePolicy(c *gin.Context) {
	channelID, ok := openCodeGoChannelID(c)
	if !ok {
		return
	}
	request := openCodeGoLifecyclePolicyUpdateRequest{}
	if err := decodeOpenCodeGoAdminJSON(c, &request, openCodeGoAdminBodyLimit); err != nil {
		common.ApiError(c, errors.New("invalid OpenCode Go lifecycle policy request"))
		return
	}
	policy, err := service.UpdateOpenCodeGoLifecyclePolicy(channelID, service.OpenCodeGoLifecyclePolicy{
		AutoEnableChinaModels:         request.AutoEnableChinaModels,
		AutoApplyReferralRewards:      request.AutoApplyReferralRewards,
		ReferralRewardsMaxPerRun:      request.ReferralRewardsMaxPerRun,
		AutoCancelSubscriptionRenewal: request.AutoCancelSubscriptionRenewal,
	})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAudit(c, "channel.opencode_go_lifecycle_policy", map[string]interface{}{
		"id":                               channelID,
		"auto_enable_china_models":         policy.AutoEnableChinaModels,
		"auto_apply_referral_rewards":      policy.AutoApplyReferralRewards,
		"referral_rewards_max_per_run":     policy.ReferralRewardsMaxPerRun,
		"auto_cancel_subscription_renewal": policy.AutoCancelSubscriptionRenewal,
	})
	common.ApiSuccess(c, policy)
}

func EnableOpenCodeGoChinaModels(c *gin.Context) {
	channelID, workspaceUID, ok := openCodeGoWorkspaceParams(c)
	if !ok {
		return
	}
	lifecycle, err := service.NewConfiguredOpenCodeGoLifecycleService()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	operation, err := lifecycle.EnableChinaModels(c.Request.Context(), channelID, workspaceUID, "manual")
	if err != nil {
		common.ApiError(c, err)
		return
	}
	view, err := service.GetOpenCodeGoPoolView(channelID)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAudit(c, "channel.opencode_go_china_models_enable", map[string]interface{}{
		"id":            channelID,
		"workspace_uid": workspaceUID,
		"operation_uid": operation.UID,
		"status":        operation.Status,
	})
	common.ApiSuccess(c, gin.H{"operation": operation, "pool": view})
}

func ApplyOpenCodeGoReferralReward(c *gin.Context) {
	channelID, workspaceUID, ok := openCodeGoWorkspaceParams(c)
	if !ok {
		return
	}
	lifecycle, err := service.NewConfiguredOpenCodeGoLifecycleService()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	summary, err := lifecycle.ApplyReferralRewards(c.Request.Context(), channelID, workspaceUID, "manual", 1)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	view, err := service.GetOpenCodeGoPoolView(channelID)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAudit(c, "channel.opencode_go_referral_apply", map[string]interface{}{
		"id":            channelID,
		"workspace_uid": workspaceUID,
		"attempted":     summary.Attempted,
		"applied":       summary.Applied,
	})
	common.ApiSuccess(c, gin.H{"summary": summary, "pool": view})
}

func CancelOpenCodeGoSubscriptionRenewal(c *gin.Context) {
	channelID, workspaceUID, ok := openCodeGoWorkspaceParams(c)
	if !ok {
		return
	}
	request := openCodeGoCancelRenewalRequest{}
	if err := decodeOpenCodeGoAdminJSON(c, &request, openCodeGoAdminBodyLimit); err != nil || strings.TrimSpace(request.Confirmation) != openCodeGoCancelRenewalConfirmation {
		common.ApiError(c, fmt.Errorf("confirmation must be %q", openCodeGoCancelRenewalConfirmation))
		return
	}
	lifecycle, err := service.NewConfiguredOpenCodeGoLifecycleService()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	operation, result, err := lifecycle.CancelSubscriptionRenewal(c.Request.Context(), channelID, workspaceUID, "manual")
	if err != nil {
		common.ApiError(c, err)
		return
	}
	view, err := service.GetOpenCodeGoPoolView(channelID)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAudit(c, "channel.opencode_go_renewal_cancel", map[string]interface{}{
		"id":                 channelID,
		"workspace_uid":      workspaceUID,
		"operation_uid":      operation.UID,
		"already_cancelled":  result.AlreadyCancelled,
		"current_period_end": result.CurrentPeriodEnd,
	})
	common.ApiSuccess(c, gin.H{"operation": operation, "cancellation": result, "pool": view})
}

func ImportOpenCodeGoIdentities(c *gin.Context) {
	channelID, ok := openCodeGoChannelID(c)
	if !ok {
		return
	}
	request := openCodeGoImportRequest{}
	if err := decodeOpenCodeGoAdminJSON(c, &request, openCodeGoImportBodyLimit); err != nil {
		common.ApiError(c, errors.New("invalid OpenCode Go Cookie import request"))
		return
	}
	poolService, err := service.NewConfiguredOpenCodeGoAccountPoolService()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	results, err := poolService.ImportAuthCookies(c.Request.Context(), channelID, request.Label, request.AuthCookies)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	view, err := service.GetOpenCodeGoPoolView(channelID)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	imported := 0
	for _, result := range results {
		if result.Status == "imported" {
			imported++
		}
	}
	recordManageAudit(c, "channel.opencode_go_import", map[string]interface{}{
		"id":       channelID,
		"imported": imported,
		"total":    len(results),
	})
	common.ApiSuccess(c, gin.H{"results": results, "pool": view})
}

func UpdateOpenCodeGoIdentity(c *gin.Context) {
	channelID, identityUID, ok := openCodeGoIdentityParams(c)
	if !ok {
		return
	}
	request := openCodeGoIdentityUpdateRequest{}
	if err := decodeOpenCodeGoAdminJSON(c, &request, openCodeGoAdminBodyLimit); err != nil {
		common.ApiError(c, errors.New("invalid OpenCode Go identity update request"))
		return
	}
	poolService := service.NewOpenCodeGoAccountPoolAdminService()
	if err := poolService.UpdateIdentityLabel(channelID, identityUID, request.Label); err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAudit(c, "channel.opencode_go_identity_update", map[string]interface{}{
		"id":           channelID,
		"identity_uid": identityUID,
	})
	writeOpenCodeGoPoolView(c, channelID)
}

func SetOpenCodeGoIdentityEnabled(c *gin.Context) {
	channelID, identityUID, ok := openCodeGoIdentityParams(c)
	if !ok {
		return
	}
	request := openCodeGoWorkspaceEnabledRequest{}
	if err := decodeOpenCodeGoAdminJSON(c, &request, openCodeGoAdminBodyLimit); err != nil || request.Enabled == nil {
		common.ApiError(c, errors.New("OpenCode Go identity enabled state is required"))
		return
	}
	poolService := service.NewOpenCodeGoAccountPoolAdminService()
	if err := poolService.SetIdentityEnabled(channelID, identityUID, *request.Enabled); err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAudit(c, "channel.opencode_go_identity_status", map[string]interface{}{
		"id":           channelID,
		"identity_uid": identityUID,
		"enabled":      *request.Enabled,
	})
	writeOpenCodeGoPoolView(c, channelID)
}

func ReplaceOpenCodeGoIdentityCookie(c *gin.Context) {
	channelID, identityUID, ok := openCodeGoIdentityParams(c)
	if !ok {
		return
	}
	request := openCodeGoCookieReplaceRequest{}
	if err := decodeOpenCodeGoAdminJSON(c, &request, openCodeGoAdminBodyLimit); err != nil {
		common.ApiError(c, errors.New("invalid OpenCode Go Cookie replacement request"))
		return
	}
	poolService, err := service.NewConfiguredOpenCodeGoAccountPoolService()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if _, err := poolService.ReplaceIdentityAuthCookie(c.Request.Context(), channelID, identityUID, request.AuthCookie); err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAudit(c, "channel.opencode_go_cookie_replace", map[string]interface{}{
		"id":           channelID,
		"identity_uid": identityUID,
	})
	writeOpenCodeGoPoolView(c, channelID)
}

func RefreshOpenCodeGoIdentity(c *gin.Context) {
	channelID, identityUID, ok := openCodeGoIdentityParams(c)
	if !ok {
		return
	}
	poolService, err := service.NewConfiguredOpenCodeGoAccountPoolService()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if _, err := poolService.RefreshIdentity(c.Request.Context(), channelID, identityUID); err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAudit(c, "channel.opencode_go_identity_refresh", map[string]interface{}{
		"id":           channelID,
		"identity_uid": identityUID,
	})
	writeOpenCodeGoPoolView(c, channelID)
}

func DeleteOpenCodeGoIdentity(c *gin.Context) {
	channelID, identityUID, ok := openCodeGoIdentityParams(c)
	if !ok {
		return
	}
	poolService := service.NewOpenCodeGoAccountPoolAdminService()
	if err := poolService.DeleteIdentity(channelID, identityUID); err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAudit(c, "channel.opencode_go_identity_delete", map[string]interface{}{
		"id":           channelID,
		"identity_uid": identityUID,
	})
	writeOpenCodeGoPoolView(c, channelID)
}

func SetOpenCodeGoWorkspaceEnabled(c *gin.Context) {
	channelID, workspaceUID, ok := openCodeGoWorkspaceParams(c)
	if !ok {
		return
	}
	request := openCodeGoWorkspaceEnabledRequest{}
	if err := decodeOpenCodeGoAdminJSON(c, &request, openCodeGoAdminBodyLimit); err != nil || request.Enabled == nil {
		common.ApiError(c, errors.New("OpenCode Go workspace enabled state is required"))
		return
	}
	poolService := service.NewOpenCodeGoAccountPoolAdminService()
	if err := poolService.SetWorkspaceEnabled(channelID, workspaceUID, *request.Enabled); err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAudit(c, "channel.opencode_go_workspace_status", map[string]interface{}{
		"id":            channelID,
		"workspace_uid": workspaceUID,
		"enabled":       *request.Enabled,
	})
	writeOpenCodeGoPoolView(c, channelID)
}

func RefreshOpenCodeGoWorkspace(c *gin.Context) {
	channelID, workspaceUID, ok := openCodeGoWorkspaceParams(c)
	if !ok {
		return
	}
	poolService, err := service.NewConfiguredOpenCodeGoAccountPoolService()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if _, err := poolService.RefreshWorkspace(c.Request.Context(), channelID, workspaceUID); err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAudit(c, "channel.opencode_go_workspace_refresh", map[string]interface{}{
		"id":            channelID,
		"workspace_uid": workspaceUID,
	})
	writeOpenCodeGoPoolView(c, channelID)
}

func RecheckOpenCodeGoWorkspaceRisk(c *gin.Context) {
	channelID, workspaceUID, ok := openCodeGoWorkspaceParams(c)
	if !ok {
		return
	}
	riskService, err := service.NewConfiguredOpenCodeGoRiskRecheckService()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	result, err := riskService.RecheckWorkspace(c.Request.Context(), channelID, workspaceUID, "manual")
	if err != nil {
		common.ApiError(c, err)
		return
	}
	view, err := service.GetOpenCodeGoPoolView(channelID)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAudit(c, "channel.opencode_go_risk_recheck", map[string]interface{}{
		"id":              channelID,
		"workspace_uid":   workspaceUID,
		"status":          result.Status,
		"upstream_status": result.UpstreamStatus,
	})
	common.ApiSuccess(c, gin.H{"result": result, "pool": view})
}

func DeleteOpenCodeGoWorkspace(c *gin.Context) {
	channelID, workspaceUID, ok := openCodeGoWorkspaceParams(c)
	if !ok {
		return
	}
	poolService := service.NewOpenCodeGoAccountPoolAdminService()
	if err := poolService.DeleteWorkspace(channelID, workspaceUID); err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAudit(c, "channel.opencode_go_workspace_delete", map[string]interface{}{
		"id":            channelID,
		"workspace_uid": workspaceUID,
	})
	writeOpenCodeGoPoolView(c, channelID)
}

func DeleteOpenCodeGoNonMemberWorkspaces(c *gin.Context) {
	channelID, ok := openCodeGoChannelID(c)
	if !ok {
		return
	}
	poolService := service.NewOpenCodeGoAccountPoolAdminService()
	deleted, err := poolService.DeleteNonMemberWorkspaces(channelID)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	view, err := service.GetOpenCodeGoPoolView(channelID)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAudit(c, "channel.opencode_go_non_member_delete", map[string]interface{}{
		"id":      channelID,
		"deleted": deleted,
	})
	common.ApiSuccess(c, gin.H{"deleted_count": deleted, "pool": view})
}

func RefreshAllOpenCodeGoIdentities(c *gin.Context) {
	channelID, ok := openCodeGoChannelID(c)
	if !ok {
		return
	}
	if _, err := service.GetOpenCodeGoPoolView(channelID); err != nil {
		common.ApiError(c, err)
		return
	}
	if _, err := service.NewConfiguredOpenCodeGoAccountPoolService(); err != nil {
		common.ApiError(c, err)
		return
	}
	payload := openCodeGoRefreshTaskPayload{
		ChannelID:   channelID,
		Concurrency: configuredOpenCodeGoRefreshConcurrency(),
	}
	activeKey := fmt.Sprintf("%s:%d", model.SystemTaskTypeOpenCodeGoRefresh, channelID)
	task, created, err := service.EnqueueSystemTaskWithActiveKey(
		model.SystemTaskTypeOpenCodeGoRefresh,
		activeKey,
		payload,
	)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAudit(c, "channel.opencode_go_refresh_all", map[string]interface{}{
		"id":      channelID,
		"task_id": task.TaskID,
		"created": created,
	})
	common.ApiSuccess(c, gin.H{"task": task.ToResponse(), "created": created})
}

func RecheckAllOpenCodeGoWorkspaceRisks(c *gin.Context) {
	channelID, ok := openCodeGoChannelID(c)
	if !ok {
		return
	}
	if _, err := service.GetOpenCodeGoPoolView(channelID); err != nil {
		common.ApiError(c, err)
		return
	}
	if _, err := service.NewConfiguredOpenCodeGoRiskRecheckService(); err != nil {
		common.ApiError(c, err)
		return
	}
	payload := openCodeGoRiskRecheckTaskPayload{
		ChannelID:   channelID,
		Concurrency: configuredOpenCodeGoRiskRecheckConcurrency(),
		Limit:       configuredOpenCodeGoRiskRecheckBatchSize(),
	}
	activeKey := fmt.Sprintf("%s:%d", model.SystemTaskTypeOpenCodeGoRiskRecheck, channelID)
	task, created, err := service.EnqueueSystemTaskWithActiveKey(
		model.SystemTaskTypeOpenCodeGoRiskRecheck,
		activeKey,
		payload,
	)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAudit(c, "channel.opencode_go_risk_recheck_all", map[string]interface{}{
		"id":      channelID,
		"task_id": task.TaskID,
		"created": created,
	})
	common.ApiSuccess(c, gin.H{"task": task.ToResponse(), "created": created})
}

func GetOpenCodeGoRefreshTask(c *gin.Context) {
	channelID, ok := openCodeGoChannelID(c)
	if !ok {
		return
	}
	taskID := strings.TrimSpace(c.Param("task_id"))
	if taskID == "" || len(taskID) > 64 {
		common.ApiError(c, errors.New("invalid OpenCode Go refresh task ID"))
		return
	}
	task, err := model.GetSystemTaskByTaskID(taskID)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if task == nil || task.Type != model.SystemTaskTypeOpenCodeGoRefresh {
		common.ApiError(c, errors.New("OpenCode Go refresh task not found"))
		return
	}
	payload := openCodeGoRefreshTaskPayload{}
	if err := task.DecodePayload(&payload); err != nil || payload.ChannelID != channelID {
		common.ApiError(c, errors.New("OpenCode Go refresh task not found"))
		return
	}
	common.ApiSuccess(c, task.ToResponse())
}

func GetOpenCodeGoRiskRecheckTask(c *gin.Context) {
	channelID, ok := openCodeGoChannelID(c)
	if !ok {
		return
	}
	taskID := strings.TrimSpace(c.Param("task_id"))
	if taskID == "" || len(taskID) > 64 {
		common.ApiError(c, errors.New("invalid OpenCode Go risk recheck task ID"))
		return
	}
	task, err := model.GetSystemTaskByTaskID(taskID)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if task == nil || task.Type != model.SystemTaskTypeOpenCodeGoRiskRecheck {
		common.ApiError(c, errors.New("OpenCode Go risk recheck task not found"))
		return
	}
	payload := openCodeGoRiskRecheckTaskPayload{}
	if err := task.DecodePayload(&payload); err != nil || payload.ChannelID != channelID {
		common.ApiError(c, errors.New("OpenCode Go risk recheck task not found"))
		return
	}
	common.ApiSuccess(c, task.ToResponse())
}

func openCodeGoChannelID(c *gin.Context) (int, bool) {
	channelID, err := strconv.Atoi(c.Param("id"))
	if err != nil || channelID <= 0 {
		common.ApiError(c, errors.New("invalid OpenCode Go channel ID"))
		return 0, false
	}
	return channelID, true
}

func openCodeGoIdentityParams(c *gin.Context) (int, string, bool) {
	channelID, ok := openCodeGoChannelID(c)
	if !ok {
		return 0, "", false
	}
	identityUID := strings.TrimSpace(c.Param("identity_uid"))
	if identityUID == "" || len(identityUID) > 64 {
		common.ApiError(c, errors.New("invalid OpenCode Go identity UID"))
		return 0, "", false
	}
	return channelID, identityUID, true
}

func openCodeGoWorkspaceParams(c *gin.Context) (int, string, bool) {
	channelID, ok := openCodeGoChannelID(c)
	if !ok {
		return 0, "", false
	}
	workspaceUID := strings.TrimSpace(c.Param("workspace_uid"))
	if workspaceUID == "" || len(workspaceUID) > 64 {
		common.ApiError(c, errors.New("invalid OpenCode Go workspace UID"))
		return 0, "", false
	}
	return channelID, workspaceUID, true
}

func decodeOpenCodeGoAdminJSON(c *gin.Context, target any, maxBytes int64) error {
	if c.Request.Body == nil {
		return errors.New("request body is required")
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
	return common.DecodeJson(c.Request.Body, target)
}

func writeOpenCodeGoPoolView(c *gin.Context, channelID int) {
	view, err := service.GetOpenCodeGoPoolView(channelID)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, view)
}
