package service

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
)

// BuildTaskPluginView converts a persisted task into the deliberately narrow
// public shape permitted at JavaScript plugin boundaries.
func BuildTaskPluginView(task *model.Task) (dto.TaskView, error) {
	return buildTaskPluginView(task, task.TaskID)
}

// BuildTaskPluginNativeView applies the public identifier contract of one
// native plugin route without changing generic or OpenAI task presentations.
func BuildTaskPluginNativeView(task *model.Task, pluginKey string) (dto.TaskView, error) {
	publicTaskID := task.TaskID
	if pluginKey == "doubao_ark_official" && task.ProviderTaskID != "" {
		publicTaskID = task.ProviderTaskID
	}
	view, err := buildTaskPluginView(task, publicTaskID)
	if err == nil && pluginKey == "doubao_ark_official" {
		view.Data = sanitizeDoubaoArkOfficialData(view.Data)
	}
	return view, err
}

func sanitizeDoubaoArkOfficialData(value any) any {
	data, ok := value.(map[string]any)
	if !ok {
		return value
	}
	sanitized := make(map[string]any)
	for _, key := range []string{"status", "model", "content", "usage", "error", "created_at", "updated_at", "service_tier"} {
		if field, present := data[key]; present {
			sanitized[key] = field
		}
	}
	return sanitized
}

func buildTaskPluginView(task *model.Task, publicTaskID string) (dto.TaskView, error) {
	if publicTaskID == "" {
		publicTaskID = task.TaskID
	}
	createdAt := task.CreatedAt
	if createdAt == 0 {
		createdAt = task.SubmitTime
	}
	view := dto.TaskView{
		TaskID:     publicTaskID,
		Platform:   string(task.Platform),
		Status:     string(task.Status),
		Progress:   task.Progress,
		FailReason: task.FailReason,
		CreatedAt:  createdAt,
		UpdatedAt:  task.UpdatedAt,
		FinishedAt: task.FinishTime,
	}
	if len(task.Data) > 0 {
		if err := common.Unmarshal(task.Data, &view.Data); err != nil {
			return dto.TaskView{}, err
		}
		view.Data = replacePrivateTaskID(view.Data, task.PrivateData.UpstreamTaskID, publicTaskID)
		if publicTaskID != task.TaskID {
			view.Data = replacePrivateTaskID(view.Data, task.TaskID, publicTaskID)
		}
	}
	return view, nil
}

// replacePrivateTaskID rewrites exact private IDs only in known task-ID fields.
// Map keys and opaque strings, including URLs containing the ID, are preserved.
func replacePrivateTaskID(value any, privateTaskID, publicTaskID string) any {
	if privateTaskID == "" || privateTaskID == publicTaskID {
		return value
	}
	switch typed := value.(type) {
	case []any:
		replaced := make([]any, len(typed))
		for index, item := range typed {
			replaced[index] = replacePrivateTaskID(item, privateTaskID, publicTaskID)
		}
		return replaced
	case map[string]any:
		replaced := make(map[string]any, len(typed))
		for key, item := range typed {
			if (key == "id" || key == "task_id" || key == "taskId") && item == privateTaskID {
				replaced[key] = publicTaskID
				continue
			}
			replaced[key] = replacePrivateTaskID(item, privateTaskID, publicTaskID)
		}
		return replaced
	default:
		return value
	}
}
