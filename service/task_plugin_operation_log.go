package service

import (
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

const (
	ContextKeyTaskPluginOriginalRequestPath = "task_plugin_original_request_path"
	maxTaskPluginOperationProviderIDBytes   = 191
	maxTaskPluginOperationRequestPathBytes  = 256
)

func sanitizeTaskPluginOperationLogValue(value string, maxBytes int) string {
	var sanitized strings.Builder
	sanitized.Grow(min(len(value), maxBytes))
	for _, r := range value {
		if unicode.IsControl(r) {
			r = '?'
		}
		if sanitized.Len()+utf8.RuneLen(r) > maxBytes {
			break
		}
		sanitized.WriteRune(r)
	}
	return sanitized.String()
}

// RecordTaskPluginOperationLog records a zero-quota, user-visible operation
// without changing task state or any billing counters. Only public request and
// owned-task metadata is included; upstream credentials and payloads are not.
func RecordTaskPluginOperationLog(c *gin.Context, operation, providerTaskID string, statusCode int, task *model.Task) {
	if c == nil || model.DB == nil || model.LOG_DB == nil {
		return
	}
	providerTaskID = sanitizeTaskPluginOperationLogValue(providerTaskID, maxTaskPluginOperationProviderIDBytes)
	other := model.NewLogOther()
	other.SetPublic("provider_task_id", providerTaskID)
	other.SetPublic("operation", operation)
	other.SetPublic("http_status", statusCode)
	other.SetPublic("success", statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices)
	if c.Request != nil && c.Request.URL != nil {
		requestPath := c.Request.URL.Path
		if originalPath := c.GetString(ContextKeyTaskPluginOriginalRequestPath); originalPath != "" {
			requestPath = originalPath
		}
		other.SetPublic("request_path", sanitizeTaskPluginOperationLogValue(requestPath, maxTaskPluginOperationRequestPathBytes))
	}
	params := model.RecordConsumeLogParams{
		Quota:          0,
		SkipDataExport: true,
		Content: []*common.Message{common.NewMessage(
			"Official task {{operation}} {{provider_task_id}} returned HTTP {{http_status}}",
			map[string]any{"operation": operation, "provider_task_id": providerTaskID, "http_status": statusCode},
		)},
		TokenName: c.GetString("token_name"),
		TokenId:   common.GetContextKeyInt(c, constant.ContextKeyTokenId),
		Group:     common.GetContextKeyString(c, constant.ContextKeyUsingGroup),
		Other:     other,
	}
	if task != nil {
		params.ChannelId = task.ChannelId
		params.ModelName = taskModelName(task)
		if params.ModelName == "" {
			params.ModelName = task.OfficialModel
		}
		if params.ModelName == "" {
			params.ModelName = task.Properties.UpstreamModelName
		}
	}
	model.RecordConsumeLog(c, common.GetContextKeyInt(c, constant.ContextKeyUserId), params)
}
