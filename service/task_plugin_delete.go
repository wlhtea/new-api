package service

import (
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
)

// TaskDeletionAdaptor is deliberately optional: existing task plugins retain
// their submit/query-only behavior unless they declare buildDeleteRequest.
type TaskDeletionAdaptor interface {
	DeleteTask(baseURL, key string, task *model.Task, proxy string) (*http.Response, error)
}

// DeleteDoubaoArkOfficialTask authorizes against the local owner/plugin scope
// before any upstream mutation. Queued cancellation joins the same terminal
// billing path as polling; the local row is never physically deleted.
func DeleteDoubaoArkOfficialTask(c *gin.Context, pluginKey, providerTaskID string) (result *dto.TaskError) {
	userID := common.GetContextKeyInt(c, constant.ContextKeyUserId)
	action := "task_provider_delete"
	statusCode := http.StatusOK
	defer func() {
		if result != nil {
			statusCode = result.StatusCode
		}
		content, _ := common.Marshal(map[string]any{
			"provider_task_id": providerTaskID,
			"result":           statusCode,
		})
		model.RecordAuditLog(c, model.AuditLog{
			UserId: userID, ActorRole: c.GetInt("role"), Category: model.AuditCategoryOperation,
			Action: action, Content: string(content), Status: statusCode, Success: result == nil,
		})
	}()

	tasks, err := model.GetByProviderTaskIdsForPlatforms(userID, []constant.TaskPlatform{constant.TaskPlatform(pluginKey)}, []string{providerTaskID})
	if err != nil {
		return &dto.TaskError{Code: "server_error", Message: "Task lookup failed", StatusCode: http.StatusInternalServerError}
	}
	if len(tasks) != 1 {
		return &dto.TaskError{Code: "task_not_found", Message: "Task not found", StatusCode: http.StatusNotFound}
	}
	task := tasks[0]
	if task.Status == model.TaskStatusInProgress {
		return &dto.TaskError{Code: "invalid_request", Message: "Running tasks cannot be deleted", StatusCode: http.StatusBadRequest}
	}
	terminal := task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure
	if !terminal {
		action = "task_provider_cancel"
	}

	if GetTaskAdaptorFunc == nil {
		return &dto.TaskError{Code: "server_error", Message: "Task adaptor is unavailable", StatusCode: http.StatusInternalServerError}
	}
	adaptor := GetTaskAdaptorFunc(task.Platform)
	deletion, ok := adaptor.(TaskDeletionAdaptor)
	if !ok || deletion == nil {
		return &dto.TaskError{Code: "server_error", Message: "Task deletion is unavailable", StatusCode: http.StatusInternalServerError}
	}
	channel, err := model.CacheGetChannel(task.ChannelId)
	if err != nil {
		return &dto.TaskError{Code: "server_error", Message: "Task channel is unavailable", StatusCode: http.StatusInternalServerError}
	}
	baseURL := constant.GetChannelBaseURL(channel.Type)
	if channel.GetBaseURL() != "" {
		baseURL = channel.GetBaseURL()
	}
	key := channel.Key
	if task.PrivateData.Key != "" {
		key = task.PrivateData.Key
	}
	adaptor.Init(&relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType: channel.Type, ChannelId: channel.Id, ChannelBaseUrl: baseURL,
			ChannelSetting: channel.GetSetting(), HeadersOverride: channel.GetHeaderOverride(), ApiKey: key,
		},
	})
	response, err := deletion.DeleteTask(baseURL, key, task, channel.GetSetting().Proxy)
	if err != nil {
		return &dto.TaskError{Code: "upstream_error", Message: "Upstream task mutation failed", StatusCode: http.StatusBadGateway}
	}
	if response == nil {
		return &dto.TaskError{Code: "upstream_error", Message: "Upstream task mutation failed", StatusCode: http.StatusBadGateway}
	}
	defer response.Body.Close()
	upstreamSuccess := response.StatusCode >= 200 && response.StatusCode < 300
	if terminal && (upstreamSuccess || response.StatusCode == http.StatusNotFound) {
		if err := task.MarkProviderDeleted(time.Now().Unix()); err != nil {
			return &dto.TaskError{Code: "server_error", Message: "Task deletion could not be persisted; retry the operation", StatusCode: http.StatusInternalServerError}
		}
		return nil
	}

	if !upstreamSuccess {
		return &dto.TaskError{Code: "upstream_error", Message: "Upstream task mutation failed", StatusCode: http.StatusBadGateway}
	}

	data := make(map[string]any)
	if len(task.Data) > 0 {
		_ = common.Unmarshal(task.Data, &data)
	}
	data["id"] = task.ProviderTaskID
	data["status"] = "cancelled"
	delete(data, "error")
	encoded, err := common.Marshal(data)
	if err != nil {
		return &dto.TaskError{Code: "server_error", Message: "Task cancellation could not be persisted", StatusCode: http.StatusInternalServerError}
	}
	won, err := task.CancelNonTerminal(encoded, time.Now().Unix())
	if err != nil {
		return &dto.TaskError{Code: "server_error", Message: "Task cancellation could not be persisted", StatusCode: http.StatusInternalServerError}
	}
	if won {
		finalizeTerminalTask(c.Request.Context(), adaptor, task, &relaycommon.TaskInfo{Status: string(model.TaskStatusFailure), Progress: "100%"})
	}
	return nil
}
