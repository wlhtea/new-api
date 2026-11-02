package service

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// RecordTaskPluginOperationLog records a zero-quota, user-visible operation
// without changing task state or any billing counters. Only public request and
// owned-task metadata is included; upstream credentials and payloads are not.
func RecordTaskPluginOperationLog(c *gin.Context, operation, providerTaskID string, statusCode int, task *model.Task) {
	if c == nil || model.DB == nil || model.LOG_DB == nil {
		return
	}
	other := model.NewLogOther()
	other.SetPublic("provider_task_id", providerTaskID)
	other.SetPublic("operation", operation)
	other.SetPublic("http_status", statusCode)
	other.SetPublic("success", statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices)
	if c.Request != nil && c.Request.URL != nil {
		other.SetPublic("request_path", c.Request.URL.Path)
	}
	params := model.RecordConsumeLogParams{
		Quota: 0,
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
