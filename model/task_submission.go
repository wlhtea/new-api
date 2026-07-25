package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
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
	billingContextDigest, err := DigestTaskBillingContext(candidate.PrivateData.BillingContext)
	if err != nil {
		return err
	}
	if billingContextDigest != attempt.BillingContextDigest {
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
		current.Properties.OriginModelName != candidate.Properties.OriginModelName {
		return ErrTaskBillingIdentityDrift
	}
	currentBillingContextDigest, err := DigestTaskBillingContext(current.PrivateData.BillingContext)
	if err != nil {
		return err
	}
	if currentBillingContextDigest != attempt.BillingContextDigest {
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
	var attempt TaskBillingAttempt
	var task Task
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).
			Where("request_id = ?", requestID).
			First(&attempt).Error; err != nil {
			return err
		}
		if err := validateTaskBillingAttempt(&attempt); err != nil {
			return err
		}
		if attempt.Owner != TaskBillingOwnerTask || attempt.TaskID == nil {
			return ErrTaskSubmissionStateConflict
		}
		return lockForUpdate(tx).
			Where("id = ? AND task_id = ?", *attempt.TaskID, attempt.PublicTaskID).
			First(&task).Error
	})
	if err != nil {
		return nil, &attempt, err
	}
	return &task, &attempt, nil
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
	expectedPrepareVersion := int64(0)
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
			attempt.PrepareVersion = 1
			attempt.UpdatedAt = now
			expectedPrepareVersion = attempt.PrepareVersion
			update := tx.Model(&TaskBillingAttempt{}).
				Where(
					"id = ? AND owner = ? AND task_id IS NULL AND prepare_version = ?",
					attempt.ID,
					TaskBillingOwnerRequest,
					0,
				).
				Updates(map[string]any{
					"task_id":              prepared.ID,
					"owner":                TaskBillingOwnerTask,
					"owner_transferred_at": now,
					"prepare_version":      expectedPrepareVersion,
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
			if attempt.PrepareVersion == math.MaxInt64 {
				return ErrTaskSubmissionStateConflict
			}
			previousPrepareVersion := attempt.PrepareVersion
			expectedPrepareVersion = previousPrepareVersion + 1

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
			versionUpdate := tx.Model(&TaskBillingAttempt{}).
				Where(
					"id = ? AND owner = ? AND task_id = ? AND prepare_version = ?",
					attempt.ID,
					TaskBillingOwnerTask,
					prepared.ID,
					previousPrepareVersion,
				).
				Updates(map[string]any{
					"prepare_version": expectedPrepareVersion,
					"updated_at":      now,
				})
			if versionUpdate.Error != nil {
				return versionUpdate.Error
			}
			if versionUpdate.RowsAffected != 1 {
				return ErrTaskSubmissionStateConflict
			}
			attempt.PrepareVersion = expectedPrepareVersion
			attempt.UpdatedAt = now
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
		if readbackErr := runTaskSubmissionFailpoint("prepare", "before_readback"); readbackErr != nil {
			return nil, nil, readbackErr
		}
		readTask, readAttempt, readErr := readPreparedTaskSubmission(requestID)
		if readErr != nil {
			return nil, nil, err
		}
		if readAttempt.PrepareVersion != expectedPrepareVersion {
			return nil, nil, ErrTaskSubmissionStateConflict
		}
		if persistentID != 0 && readTask.ID != persistentID {
			return nil, nil, ErrTaskSubmissionStateConflict
		}
		if validationErr := validateTaskCandidateAgainstAttempt(readTask, readAttempt); validationErr != nil {
			return nil, nil, err
		}
		if readTask.ChannelId != candidate.ChannelId ||
			readTask.PrivateData.Key != candidate.PrivateData.Key ||
			readTask.Properties.UpstreamModelName != candidate.Properties.UpstreamModelName {
			return nil, nil, ErrTaskSubmissionStateConflict
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
		case TaskStatusSubmitting:
			if result.Progress != "0%" {
				return ErrTaskSubmissionStateConflict
			}
		case TaskStatusSubmitted:
			if result.Progress != "10%" {
				return ErrTaskSubmissionStateConflict
			}
		case TaskStatusFailure:
			if result.Progress != "100%" {
				return ErrTaskSubmissionStateConflict
			}
		default:
			return ErrTaskSubmissionStateConflict
		}
		return transitionTaskToFailureLocked(tx, &result, TaskFailureTransition{
			ExpectedStatus:   result.Status,
			ExpectedProgress: result.Progress,
			UpstreamTaskID:   upstreamTaskID,
			Code:             code,
			Message:          message,
		})
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// TaskFailureTransition names the exact durable state observed by a caller.
// Data and ResultURL are opt-in so a terminal failure never rewrites mutable
// polling columns from a stale in-memory Task by accident.
type TaskFailureTransition struct {
	ExpectedStatus   TaskStatus
	ExpectedProgress string
	UpstreamTaskID   string
	Code             string
	Message          string
	Data             *json.RawMessage
	ResultURL        *string
}

func validateTaskFailureExpectedState(transition TaskFailureTransition) error {
	switch transition.ExpectedStatus {
	case TaskStatusSubmitting:
		if transition.ExpectedProgress != "0%" {
			return ErrTaskSubmissionStateConflict
		}
	case TaskStatusSubmitted:
		if transition.ExpectedProgress != "10%" {
			return ErrTaskSubmissionStateConflict
		}
	case TaskStatusQueued, TaskStatusInProgress:
		if transition.ExpectedProgress == "" {
			return ErrTaskSubmissionStateConflict
		}
	case TaskStatusFailure:
		if transition.ExpectedProgress != "100%" {
			return ErrTaskSubmissionStateConflict
		}
	default:
		return ErrTaskSubmissionStateConflict
	}
	return nil
}

// TransitionTaskToFailure locks and reloads the current Task before applying a
// terminal failure. The expected status and progress are both exact guards;
// callers that discovered stale polling state lose without modifying the row.
func TransitionTaskToFailure(
	id int64,
	publicTaskID string,
	transition TaskFailureTransition,
) (*Task, error) {
	if id <= 0 || publicTaskID == "" {
		return nil, ErrTaskSubmissionStateConflict
	}
	if err := validateTaskFailureExpectedState(transition); err != nil {
		return nil, err
	}
	var result Task
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).
			Where("id = ? AND task_id = ?", id, publicTaskID).
			First(&result).Error; err != nil {
			return err
		}
		if result.Status != transition.ExpectedStatus ||
			result.Progress != transition.ExpectedProgress {
			return ErrTaskSubmissionStateConflict
		}
		return transitionTaskToFailureLocked(tx, &result, transition)
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func transitionTaskToFailureLocked(
	tx *gorm.DB,
	result *Task,
	transition TaskFailureTransition,
) error {
	if tx == nil || result == nil {
		return ErrTaskSubmissionStateConflict
	}
	privateChanged := false
	if transition.UpstreamTaskID != "" {
		switch storedID := result.PrivateData.UpstreamTaskID; {
		case storedID == "":
			result.PrivateData.UpstreamTaskID = transition.UpstreamTaskID
			privateChanged = true
		case storedID != transition.UpstreamTaskID:
			return ErrTaskUpstreamIDConflict
		}
	}
	if transition.ResultURL != nil &&
		result.PrivateData.ResultURL != *transition.ResultURL {
		result.PrivateData.ResultURL = *transition.ResultURL
		privateChanged = true
	}

	dataChanged := transition.Data != nil && !bytes.Equal(result.Data, *transition.Data)
	wasFailure := result.Status == TaskStatusFailure
	if wasFailure && !privateChanged && !dataChanged {
		return nil
	}

	now := taskBillingTimestamp()
	updates := make(map[string]any, 7)
	if !wasFailure {
		result.Status = TaskStatusFailure
		result.Progress = "100%"
		result.FinishTime = now
		if transition.Message != "" {
			result.FailReason = transition.Message
		} else {
			result.FailReason = transition.Code
		}
		updates["status"] = result.Status
		updates["progress"] = result.Progress
		updates["finish_time"] = result.FinishTime
		updates["fail_reason"] = result.FailReason
	}
	if privateChanged {
		updates["private_data"] = result.PrivateData
	}
	if transition.Data != nil {
		result.Data = append(json.RawMessage(nil), (*transition.Data)...)
		updates["data"] = result.Data
	}
	result.UpdatedAt = now
	updates["updated_at"] = result.UpdatedAt

	update := tx.Model(&Task{}).
		Where(
			"id = ? AND task_id = ? AND status = ? AND progress = ?",
			result.ID,
			result.TaskID,
			transition.ExpectedStatus,
			transition.ExpectedProgress,
		).
		Updates(updates)
	if update.Error != nil {
		return update.Error
	}
	if update.RowsAffected != 1 {
		return ErrTaskSubmissionStateConflict
	}
	return nil
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
