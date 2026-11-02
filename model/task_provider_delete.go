package model

import "encoding/json"

// CancelNonTerminal atomically settles a queued/running task as an internal
// failure while preserving the official cancelled status in Data. Only the CAS
// winner may enter terminal billing; a poller that already settled keeps the
// sole billing/refund ownership.
func (task *Task) CancelNonTerminal(data json.RawMessage, now int64) (bool, error) {
	updates := map[string]any{
		"status":      TaskStatusFailure,
		"progress":    "100%",
		"finish_time": now,
		"updated_at":  now,
		"data":        data,
		"fail_reason": "",
	}
	result := DB.Model(&Task{}).
		Where("id = ?", task.ID).
		Where("status NOT IN ?", []TaskStatus{TaskStatusSuccess, TaskStatusFailure}).
		Updates(updates)
	if result.Error != nil || result.RowsAffected == 0 {
		return false, result.Error
	}
	task.Status = TaskStatusFailure
	task.Progress = "100%"
	task.FinishTime = now
	task.UpdatedAt = now
	task.Data = data
	task.FailReason = ""
	return true, nil
}

// MarkProviderDeleted hides a terminal provider record from public protocol
// queries while retaining the task and its billing/audit provenance.
func (task *Task) MarkProviderDeleted(now int64) error {
	result := DB.Model(&Task{}).
		Where("id = ? AND provider_deleted = ?", task.ID, false).
		Updates(map[string]any{"provider_deleted": true, "updated_at": now})
	if result.Error == nil {
		task.ProviderDeleted = true
		task.UpdatedAt = now
	}
	return result.Error
}
