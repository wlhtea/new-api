package model

import (
	"errors"
	"reflect"
	"sync"

	"gorm.io/gorm"
)

var (
	ErrTaskSubmissionStateConflict = errors.New("task submission state conflict")
	ErrTaskUpstreamIDConflict      = errors.New("task upstream id conflict")
)

type taskSubmissionFailpointFunc func(operation, point string) error

var taskSubmissionFailpointState struct {
	sync.RWMutex
	fn taskSubmissionFailpointFunc
}

func setTaskSubmissionFailpointForTest(fn taskSubmissionFailpointFunc) func() {
	taskSubmissionFailpointState.Lock()
	previous := taskSubmissionFailpointState.fn
	taskSubmissionFailpointState.fn = fn
	taskSubmissionFailpointState.Unlock()
	return func() {
		taskSubmissionFailpointState.Lock()
		taskSubmissionFailpointState.fn = previous
		taskSubmissionFailpointState.Unlock()
	}
}

func runTaskSubmissionFailpoint(operation, point string) error {
	taskSubmissionFailpointState.RLock()
	fn := taskSubmissionFailpointState.fn
	taskSubmissionFailpointState.RUnlock()
	if fn == nil {
		return nil
	}
	return fn(operation, point)
}

func validateTaskCandidateAgainstAttempt(candidate *Task, attempt *TaskBillingAttempt) error {
	if candidate == nil || attempt == nil {
		return ErrTaskSubmissionStateConflict
	}
	if err := validateTaskBillingAttempt(attempt); err != nil {
		return err
	}
	if candidate.TaskID != attempt.PublicTaskID ||
		candidate.UserId != attempt.UserID ||
		candidate.Quota != attempt.FundingAmount ||
		candidate.PrivateData.BillingSource != attempt.FundingSource ||
		candidate.PrivateData.SubscriptionId != attempt.SubscriptionID ||
		candidate.PrivateData.TokenId != attempt.TokenID ||
		candidate.SubmitTime != attempt.SubmitTime {
		return ErrTaskBillingIdentityDrift
	}
	if candidate.Status != TaskStatusSubmitting || candidate.Progress != "0%" ||
		candidate.PrivateData.UpstreamTaskID != "" {
		return ErrTaskSubmissionStateConflict
	}
	return nil
}

func validateTaskRetryIdentity(current, candidate *Task, attempt *TaskBillingAttempt) error {
	if current == nil || candidate == nil || attempt == nil {
		return ErrTaskSubmissionStateConflict
	}
	if current.ID != candidate.ID && candidate.ID != 0 {
		return ErrTaskBillingIdentityDrift
	}
	if current.ID != *attempt.TaskID ||
		current.TaskID != candidate.TaskID ||
		current.UserId != candidate.UserId ||
		current.Platform != candidate.Platform ||
		current.Action != candidate.Action ||
		current.SubmitTime != candidate.SubmitTime ||
		current.Group != candidate.Group ||
		current.Quota != candidate.Quota ||
		current.PrivateData.BillingSource != candidate.PrivateData.BillingSource ||
		current.PrivateData.SubscriptionId != candidate.PrivateData.SubscriptionId ||
		current.PrivateData.TokenId != candidate.PrivateData.TokenId ||
		current.PrivateData.NodeName != candidate.PrivateData.NodeName ||
		current.Properties.OriginModelName != candidate.Properties.OriginModelName ||
		!reflect.DeepEqual(current.PrivateData.BillingContext, candidate.PrivateData.BillingContext) {
		return ErrTaskBillingIdentityDrift
	}
	if err := validateTaskCandidateAgainstAttempt(candidate, attempt); err != nil {
		return err
	}
	if current.Status != TaskStatusSubmitting || current.Progress != "0%" ||
		current.PrivateData.UpstreamTaskID != "" {
		return ErrTaskSubmissionStateConflict
	}
	return nil
}

func readPreparedTaskSubmission(requestID string) (*Task, *TaskBillingAttempt, error) {
	attempt, err := GetTaskBillingAttemptByRequestID(requestID)
	if err != nil {
		return nil, nil, err
	}
	if attempt.Owner != TaskBillingOwnerTask || attempt.TaskID == nil {
		return nil, attempt, ErrTaskSubmissionStateConflict
	}
	var task Task
	if err := DB.Where("id = ? AND task_id = ?", *attempt.TaskID, attempt.PublicTaskID).
		First(&task).Error; err != nil {
		return nil, attempt, err
	}
	return &task, attempt, nil
}

func PrepareTaskSubmissionAttempt(
	candidate *Task,
	persistentID int64,
	requestID string,
) (*Task, *TaskBillingAttempt, error) {
	if candidate == nil || persistentID < 0 || requestID == "" {
		return nil, nil, ErrTaskSubmissionStateConflict
	}

	var prepared Task
	var attempt TaskBillingAttempt
	transactionMayHaveCommitted := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where("request_id = ?", requestID).First(&attempt).Error; err != nil {
			return err
		}
		if err := validateTaskBillingAttempt(&attempt); err != nil {
			return err
		}
		if attempt.FundingConsumedAt == 0 || attempt.TokenConsumedAt == 0 ||
			attempt.PreconsumeCompletedAt == 0 ||
			attempt.RefundStartedAt != 0 || attempt.RefundCompletedAt != 0 ||
			attempt.SucceededAt != 0 {
			return ErrTaskBillingAttemptState
		}

		if persistentID == 0 {
			if attempt.Owner != TaskBillingOwnerRequest || attempt.TaskID != nil {
				return ErrTaskSubmissionStateConflict
			}
			if err := validateTaskCandidateAgainstAttempt(candidate, &attempt); err != nil {
				return err
			}
			prepared = *candidate
			prepared.ID = 0
			now := taskBillingTimestamp()
			if prepared.CreatedAt == 0 {
				prepared.CreatedAt = now
			}
			prepared.UpdatedAt = now
			if err := runTaskSubmissionFailpoint("prepare", "before_task_insert"); err != nil {
				return err
			}
			if err := tx.Create(&prepared).Error; err != nil {
				return err
			}
			if err := runTaskSubmissionFailpoint("prepare", "after_task_insert"); err != nil {
				return err
			}
			attempt.TaskID = &prepared.ID
			attempt.Owner = TaskBillingOwnerTask
			attempt.OwnerTransferredAt = now
			attempt.UpdatedAt = now
			update := tx.Model(&TaskBillingAttempt{}).
				Where("id = ? AND owner = ? AND task_id IS NULL", attempt.ID, TaskBillingOwnerRequest).
				Updates(map[string]any{
					"task_id":              prepared.ID,
					"owner":                TaskBillingOwnerTask,
					"owner_transferred_at": now,
					"updated_at":           now,
				})
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected != 1 {
				return ErrTaskSubmissionStateConflict
			}
			if err := runTaskSubmissionFailpoint("prepare", "after_owner_transfer"); err != nil {
				return err
			}
		} else {
			if attempt.Owner != TaskBillingOwnerTask || attempt.TaskID == nil ||
				*attempt.TaskID != persistentID {
				return ErrTaskSubmissionStateConflict
			}
			if err := lockForUpdate(tx).
				Where("id = ? AND task_id = ?", persistentID, attempt.PublicTaskID).
				First(&prepared).Error; err != nil {
				return err
			}
			if err := validateTaskRetryIdentity(&prepared, candidate, &attempt); err != nil {
				return err
			}

			latestPrivate := prepared.PrivateData
			latestPrivate.Key = candidate.PrivateData.Key
			latestProperties := prepared.Properties
			latestProperties.UpstreamModelName = candidate.Properties.UpstreamModelName
			now := taskBillingTimestamp()
			update := tx.Model(&Task{}).
				Where("id = ? AND task_id = ?", prepared.ID, prepared.TaskID).
				Updates(map[string]any{
					"channel_id":   candidate.ChannelId,
					"private_data": latestPrivate,
					"properties":   latestProperties,
					"updated_at":   now,
				})
			if update.Error != nil {
				return update.Error
			}
			// The row was locked and its identity was validated above. MySQL
			// reports zero changed rows for an idempotent retry whose route
			// fields (including second-granularity updated_at) are unchanged.
			prepared.ChannelId = candidate.ChannelId
			prepared.PrivateData = latestPrivate
			prepared.Properties = latestProperties
			prepared.UpdatedAt = now
		}
		if err := runTaskSubmissionFailpoint("prepare", "before_commit"); err != nil {
			return err
		}
		transactionMayHaveCommitted = true
		return nil
	})
	if err == nil {
		transactionMayHaveCommitted = true
		if hookErr := runTaskSubmissionFailpoint("prepare", "after_commit"); hookErr != nil {
			err = hookErr
		}
	}
	if err != nil {
		if !transactionMayHaveCommitted {
			return nil, nil, err
		}
		readTask, readAttempt, readErr := readPreparedTaskSubmission(requestID)
		if readErr != nil {
			return nil, nil, err
		}
		if persistentID != 0 && readTask.ID != persistentID {
			return nil, nil, err
		}
		if validationErr := validateTaskCandidateAgainstAttempt(readTask, readAttempt); validationErr != nil {
			return nil, nil, err
		}
		if persistentID != 0 &&
			(readTask.ChannelId != candidate.ChannelId ||
				readTask.PrivateData.Key != candidate.PrivateData.Key ||
				readTask.Properties.UpstreamModelName != candidate.Properties.UpstreamModelName) {
			return nil, nil, err
		}
		return readTask, readAttempt, nil
	}
	return &prepared, &attempt, nil
}

func GetTaskByPrimaryID(id int64) (*Task, error) {
	if id <= 0 {
		return nil, gorm.ErrRecordNotFound
	}
	var task Task
	if err := DB.Where("id = ?", id).First(&task).Error; err != nil {
		return nil, err
	}
	return &task, nil
}

func AttachTaskUpstreamResult(
	id int64,
	publicTaskID string,
	upstreamTaskID string,
	taskData []byte,
) (*Task, error) {
	if id <= 0 || publicTaskID == "" || upstreamTaskID == "" {
		return nil, ErrTaskSubmissionStateConflict
	}
	var result Task
	transactionMayHaveCommitted := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).
			Where("id = ? AND task_id = ?", id, publicTaskID).
			First(&result).Error; err != nil {
			return err
		}
		currentID := result.PrivateData.UpstreamTaskID
		if currentID != "" {
			if currentID != upstreamTaskID {
				return ErrTaskUpstreamIDConflict
			}
			transactionMayHaveCommitted = true
			return nil
		}
		if result.Status != TaskStatusSubmitting || result.Progress != "0%" {
			return ErrTaskSubmissionStateConflict
		}
		result.PrivateData.UpstreamTaskID = upstreamTaskID
		result.Data = append([]byte(nil), taskData...)
		result.UpdatedAt = taskBillingTimestamp()
		update := tx.Model(&Task{}).
			Where("id = ? AND task_id = ?", result.ID, result.TaskID).
			Updates(map[string]any{
				"private_data": result.PrivateData,
				"data":         result.Data,
				"updated_at":   result.UpdatedAt,
			})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrTaskSubmissionStateConflict
		}
		if err := runTaskSubmissionFailpoint("attach", "before_commit"); err != nil {
			return err
		}
		transactionMayHaveCommitted = true
		return nil
	})
	if err == nil {
		transactionMayHaveCommitted = true
		if hookErr := runTaskSubmissionFailpoint("attach", "after_commit"); hookErr != nil {
			err = hookErr
		}
	}
	if err != nil {
		if transactionMayHaveCommitted {
			primary, readErr := GetTaskByPrimaryID(id)
			if readErr == nil && primary.TaskID == publicTaskID &&
				primary.PrivateData.UpstreamTaskID == upstreamTaskID {
				return primary, nil
			}
		}
		return nil, err
	}
	return &result, nil
}

func AttachTaskUpstreamResultForReconciliation(
	id int64,
	publicTaskID string,
	channelID int,
	upstreamTaskID string,
) (*Task, error) {
	if id <= 0 || publicTaskID == "" || channelID <= 0 || upstreamTaskID == "" {
		return nil, ErrTaskSubmissionStateConflict
	}
	var result Task
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).
			Where("id = ? AND task_id = ? AND channel_id = ?", id, publicTaskID, channelID).
			First(&result).Error; err != nil {
			return err
		}
		currentID := result.PrivateData.UpstreamTaskID
		if currentID != "" {
			if currentID != upstreamTaskID {
				return ErrTaskUpstreamIDConflict
			}
			return nil
		}
		validState := (result.Status == TaskStatusSubmitting && result.Progress == "0%") ||
			(result.Status == TaskStatusFailure && result.Progress == "100%")
		if !validState {
			return ErrTaskSubmissionStateConflict
		}
		result.PrivateData.UpstreamTaskID = upstreamTaskID
		result.UpdatedAt = taskBillingTimestamp()
		update := tx.Model(&Task{}).
			Where("id = ? AND task_id = ? AND channel_id = ?", id, publicTaskID, channelID).
			Updates(map[string]any{
				"private_data": result.PrivateData,
				"updated_at":   result.UpdatedAt,
			})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrTaskSubmissionStateConflict
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func CommitTaskSubmission(id int64, publicTaskID string) (*Task, error) {
	if id <= 0 || publicTaskID == "" {
		return nil, ErrTaskSubmissionStateConflict
	}
	var result Task
	transactionMayHaveCommitted := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).
			Where("id = ? AND task_id = ?", id, publicTaskID).
			First(&result).Error; err != nil {
			return err
		}
		if result.PrivateData.UpstreamTaskID == "" {
			return ErrTaskSubmissionStateConflict
		}
		if result.Status == TaskStatusSubmitted && result.Progress == "10%" {
			transactionMayHaveCommitted = true
			return nil
		}
		if result.Status != TaskStatusSubmitting || result.Progress != "0%" {
			return ErrTaskSubmissionStateConflict
		}
		result.Status = TaskStatusSubmitted
		result.Progress = "10%"
		result.UpdatedAt = taskBillingTimestamp()
		update := tx.Model(&Task{}).
			Where("id = ? AND task_id = ?", result.ID, result.TaskID).
			Updates(map[string]any{
				"status":     result.Status,
				"progress":   result.Progress,
				"updated_at": result.UpdatedAt,
			})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrTaskSubmissionStateConflict
		}
		if err := runTaskSubmissionFailpoint("commit", "before_commit"); err != nil {
			return err
		}
		transactionMayHaveCommitted = true
		return nil
	})
	if err == nil {
		transactionMayHaveCommitted = true
		if hookErr := runTaskSubmissionFailpoint("commit", "after_commit"); hookErr != nil {
			err = hookErr
		}
	}
	if err != nil {
		if transactionMayHaveCommitted {
			primary, readErr := GetTaskByPrimaryID(id)
			if readErr == nil && primary.TaskID == publicTaskID &&
				primary.Status == TaskStatusSubmitted && primary.Progress == "10%" &&
				primary.PrivateData.UpstreamTaskID != "" {
				return primary, nil
			}
		}
		return nil, err
	}
	return &result, nil
}

func TransitionTaskSubmissionToFailure(
	id int64,
	publicTaskID string,
	upstreamTaskID string,
	code string,
	message string,
) (*Task, error) {
	if id <= 0 || publicTaskID == "" {
		return nil, ErrTaskSubmissionStateConflict
	}
	var result Task
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).
			Where("id = ? AND task_id = ?", id, publicTaskID).
			First(&result).Error; err != nil {
			return err
		}
		switch result.Status {
		case TaskStatusSubmitting, TaskStatusSubmitted:
		case TaskStatusFailure:
			if result.Progress != "100%" {
				return ErrTaskSubmissionStateConflict
			}
		default:
			return ErrTaskSubmissionStateConflict
		}

		privateChanged := false
		if upstreamTaskID != "" {
			switch storedID := result.PrivateData.UpstreamTaskID; {
			case storedID == "":
				result.PrivateData.UpstreamTaskID = upstreamTaskID
				privateChanged = true
			case storedID != upstreamTaskID:
				return ErrTaskUpstreamIDConflict
			}
		}

		now := taskBillingTimestamp()
		if result.Status != TaskStatusFailure {
			result.Status = TaskStatusFailure
			result.Progress = "100%"
			result.FinishTime = now
			if message != "" {
				result.FailReason = message
			} else {
				result.FailReason = code
			}
			result.UpdatedAt = now
			updates := map[string]any{
				"status":      result.Status,
				"progress":    result.Progress,
				"finish_time": result.FinishTime,
				"fail_reason": result.FailReason,
				"updated_at":  result.UpdatedAt,
			}
			if privateChanged {
				updates["private_data"] = result.PrivateData
			}
			update := tx.Model(&Task{}).
				Where("id = ? AND task_id = ?", result.ID, result.TaskID).
				Updates(updates)
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected != 1 {
				return ErrTaskSubmissionStateConflict
			}
			return nil
		}
		if !privateChanged {
			return nil
		}
		result.UpdatedAt = now
		update := tx.Model(&Task{}).
			Where("id = ? AND task_id = ?", result.ID, result.TaskID).
			Updates(map[string]any{
				"private_data": result.PrivateData,
				"updated_at":   result.UpdatedAt,
			})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrTaskSubmissionStateConflict
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func markTaskBillingAttemptTerminal(requestID string, finalSuccess bool) error {
	if requestID == "" {
		return ErrTaskBillingAttemptState
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		var attempt TaskBillingAttempt
		if err := lockForUpdate(tx).Where("request_id = ?", requestID).First(&attempt).Error; err != nil {
			return err
		}
		if err := validateTaskBillingAttempt(&attempt); err != nil {
			return err
		}
		if attempt.Owner != TaskBillingOwnerTask || attempt.TaskID == nil ||
			attempt.FundingConsumedAt == 0 || attempt.TokenConsumedAt == 0 ||
			attempt.PreconsumeCompletedAt == 0 ||
			attempt.RefundStartedAt != 0 || attempt.RefundCompletedAt != 0 {
			return ErrTaskBillingAttemptState
		}
		var task Task
		if err := lockForUpdate(tx).Where("id = ? AND task_id = ?", *attempt.TaskID, attempt.PublicTaskID).
			First(&task).Error; err != nil {
			return err
		}
		now := taskBillingTimestamp()
		column := "submission_settled_at"
		if finalSuccess {
			if task.Status != TaskStatusSuccess || task.Progress != "100%" {
				return ErrTaskBillingAttemptState
			}
			if attempt.SucceededAt != 0 {
				return nil
			}
			column = "succeeded_at"
			attempt.SucceededAt = now
		} else {
			validForwardState := false
			switch task.Status {
			case TaskStatusSubmitted:
				validForwardState = task.Progress == "10%"
			case TaskStatusQueued, TaskStatusInProgress:
				validForwardState = true
			case TaskStatusSuccess:
				validForwardState = task.Progress == "100%"
			}
			if !validForwardState {
				return ErrTaskBillingAttemptState
			}
			if attempt.SubmissionSettledAt != 0 {
				return nil
			}
			attempt.SubmissionSettledAt = now
		}
		return tx.Model(&TaskBillingAttempt{}).Where("id = ?", attempt.ID).
			Updates(map[string]any{column: now, "updated_at": now}).Error
	})
}

// MarkTaskBillingAttemptSubmissionSettled records the zero-delta submission
// settlement. It is intentionally not a terminal success marker.
func MarkTaskBillingAttemptSubmissionSettled(requestID string) error {
	return markTaskBillingAttemptTerminal(requestID, false)
}

// MarkTaskBillingAttemptSucceeded records final asynchronous Task SUCCESS.
func MarkTaskBillingAttemptSucceeded(requestID string) error {
	return markTaskBillingAttemptTerminal(requestID, true)
}
