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
	openCodeGoImportBodyLimit = int64(2 * 1024 * 1024)
	openCodeGoAdminBodyLimit  = int64(64 * 1024)
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
