package service

import (
	"context"
	"io"
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
	DeleteTask(ctx context.Context, baseURL, key string, task *model.Task, proxy string) (*http.Response, error)
}

const taskProviderDeleteOperationTimeout = 30 * time.Second

type taskContextPollingAdaptor interface {
	FetchTaskContext(ctx context.Context, baseURL, key string, task *model.Task, proxy string) (*http.Response, error)
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
	operationCtx, cancel := context.WithTimeout(c.Request.Context(), taskProviderDeleteOperationTimeout)
	defer cancel()
	if !terminal {
		// Local state can lag a completed provider task. Reconcile through the
		// same authenticated plugin query machinery as the poller before any
		// cancellation, rather than trusting that stale snapshot.
		preflightTask := *task
		preflightTask.PrivateData.UpstreamTaskID = providerTaskID
		var preflight *http.Response
		var fetchErr error
		if contextual, supported := adaptor.(taskContextPollingAdaptor); supported {
			preflight, fetchErr = contextual.FetchTaskContext(operationCtx, baseURL, key, &preflightTask, channel.GetSetting().Proxy)
		} else {
			preflight, fetchErr = adaptor.FetchTask(baseURL, key, &preflightTask, channel.GetSetting().Proxy)
		}
		if fetchErr != nil || preflight == nil {
			return &dto.TaskError{Code: "upstream_error", Message: "Upstream task state check failed", StatusCode: http.StatusBadGateway}
		}
		body, readErr := io.ReadAll(preflight.Body)
		closeErr := preflight.Body.Close()
		if readErr != nil || closeErr != nil || preflight.StatusCode < 200 || preflight.StatusCode >= 300 {
			return &dto.TaskError{Code: "upstream_error", Message: "Upstream task state check failed", StatusCode: http.StatusBadGateway}
		}
		providerState, parseErr := adaptor.ParseTaskResult(&preflightTask, preflight, body)
		if parseErr != nil || providerState == nil || model.TaskStatus(providerState.Status) == model.TaskStatusUnknown || providerState.Status == "" {
			return &dto.TaskError{Code: "upstream_error", Message: "Upstream task state could not be verified", StatusCode: http.StatusBadGateway}
		}
		switch model.TaskStatus(providerState.Status) {
		case model.TaskStatusQueued:
			// Only provider-confirmed queued tasks enter queued cancellation.
		case model.TaskStatusInProgress:
			return &dto.TaskError{Code: "invalid_request", Message: "Running tasks cannot be deleted", StatusCode: http.StatusBadRequest}
		default:
			return &dto.TaskError{Code: "task_state_conflict", Message: "Task state changed; wait for synchronization and retry", StatusCode: http.StatusConflict}
		}
	}
	response, err := deletion.DeleteTask(operationCtx, baseURL, key, task, channel.GetSetting().Proxy)
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

	// Ark's empty 2xx DELETE body does not itself prove cancellation. The
	// preflight and local CAS close the stale-local and poller-settlement bugs;
	// a provider transition in the network window between those upstream calls
	// remains an unavoidable distributed race, so no success state is inferred
	// from this response.
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
	} else {
		// The poller settled first and retains sole billing ownership, but the
		// upstream DELETE still removed the provider record. Hide that record
		// without changing the poller's terminal state or refunding again.
		if err := task.MarkProviderDeleted(time.Now().Unix()); err != nil {
			return &dto.TaskError{Code: "server_error", Message: "Task deletion could not be persisted; retry the operation", StatusCode: http.StatusInternalServerError}
		}
	}
	return nil
}
