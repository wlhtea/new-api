package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// SeedDanceSubmitReconciliationPayload is intentionally a seven-field
// administrator-only identity envelope. Credentials, request media, task data,
// and provider responses are not accepted by this type.
type SeedDanceSubmitReconciliationPayload struct {
	PublicTaskID     string `json:"public_task_id"`
	UpstreamTaskID   string `json:"upstream_task_id"`
	PersistentTaskID int64  `json:"persistent_task_id"`
	ChannelID        int    `json:"channel_id"`
	NodeName         string `json:"node_name"`
	ErrorCode        string `json:"error_code"`
	ObservedAt       int64  `json:"observed_at"`
}

func DecodeSeedDanceSubmitReconciliationPayload(
	systemTask *model.SystemTask,
) (SeedDanceSubmitReconciliationPayload, error) {
	var payload SeedDanceSubmitReconciliationPayload
	if systemTask == nil {
		return payload, model.ErrTaskBillingIdentityDrift
	}
	decoder := json.NewDecoder(strings.NewReader(systemTask.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return payload, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return payload, errors.New("multiple reconciliation payload values")
		}
		return payload, err
	}
	return payload, nil
}

func SeedDanceSubmitReconciliationActiveKey(publicTaskID string) string {
	sum := sha256.Sum256([]byte(publicTaskID))
	return fmt.Sprintf("sd-submit:%x", sum[:16])
}

func validateSeedDanceSubmitReconciliationPayload(
	payload SeedDanceSubmitReconciliationPayload,
) error {
	if payload.PublicTaskID == "" ||
		strings.TrimSpace(payload.PublicTaskID) != payload.PublicTaskID ||
		payload.UpstreamTaskID == "" ||
		strings.TrimSpace(payload.UpstreamTaskID) != payload.UpstreamTaskID ||
		payload.PersistentTaskID <= 0 ||
		payload.ChannelID <= 0 ||
		payload.ErrorCode == "" {
		return model.ErrTaskBillingIdentityDrift
	}
	return nil
}

func CreateSeedDanceSubmitReconciliation(
	payload SeedDanceSubmitReconciliationPayload,
) (*model.SystemTask, error) {
	if payload.ObservedAt == 0 {
		payload.ObservedAt = common.GetTimestamp()
	}
	if err := validateSeedDanceSubmitReconciliationPayload(payload); err != nil {
		return nil, err
	}
	activeKey := SeedDanceSubmitReconciliationActiveKey(payload.PublicTaskID)
	active, err := model.GetActiveSystemTaskByActiveKey(
		model.SystemTaskTypeSeedDanceSubmitReconciliation,
		activeKey,
	)
	if err != nil {
		return nil, err
	}
	if active != nil {
		return active, validateActiveSeedDanceReconciliation(active, payload)
	}
	created, createErr := model.CreateSystemTaskWithActiveKey(
		model.SystemTaskTypeSeedDanceSubmitReconciliation,
		activeKey,
		payload,
		nil,
	)
	if createErr == nil {
		return created, nil
	}
	active, readErr := model.GetActiveSystemTaskByActiveKey(
		model.SystemTaskTypeSeedDanceSubmitReconciliation,
		activeKey,
	)
	if readErr == nil && active != nil {
		return active, validateActiveSeedDanceReconciliation(active, payload)
	}
	return nil, errors.Join(createErr, readErr)
}

func validateActiveSeedDanceReconciliation(
	active *model.SystemTask,
	payload SeedDanceSubmitReconciliationPayload,
) error {
	if active == nil {
		return model.ErrTaskBillingIdentityDrift
	}
	var existing SeedDanceSubmitReconciliationPayload
	if err := active.DecodePayload(&existing); err != nil {
		return err
	}
	if existing.PublicTaskID != payload.PublicTaskID ||
		existing.PersistentTaskID != payload.PersistentTaskID ||
		existing.ChannelID != payload.ChannelID {
		return model.ErrTaskBillingIdentityDrift
	}
	if existing.UpstreamTaskID != payload.UpstreamTaskID {
		return model.ErrTaskUpstreamIDConflict
	}
	return nil
}

// FailAndRefundTaskSubmission resolves the durable refund owner exclusively
// from the RequestID ledger. Request-owned attempts converge their component
// markers directly. Task-owned attempts first use the locked narrow failure
// transition, then refund the same attempt components.
func FailAndRefundTaskSubmission(
	ctx context.Context,
	taskID int64,
	requestID string,
	upstreamTaskID string,
	taskData []byte,
	code string,
	message string,
) error {
	if requestID == "" {
		return model.ErrTaskBillingIdentityDrift
	}
	attempt, err := model.GetTaskBillingAttemptByRequestID(requestID)
	if err != nil {
		return err
	}
	switch attempt.Owner {
	case model.TaskBillingOwnerRequest:
		if attempt.TaskID != nil {
			return model.ErrTaskBillingIdentityDrift
		}
		_, err = RefundTaskBillingAttempt(ctx, attempt.RequestID, code)
		return err

	case model.TaskBillingOwnerTask:
		if attempt.TaskID == nil || *attempt.TaskID <= 0 {
			return model.ErrTaskBillingIdentityDrift
		}
		linkedTaskID := *attempt.TaskID
		if taskID != 0 && taskID != linkedTaskID {
			return model.ErrTaskBillingIdentityDrift
		}

		var attachErr error
		if upstreamTaskID != "" {
			_, attachErr = model.AttachTaskUpstreamResult(
				linkedTaskID,
				attempt.PublicTaskID,
				upstreamTaskID,
				taskData,
			)
		}
		_, transitionErr := model.TransitionTaskSubmissionToFailure(
			linkedTaskID,
			attempt.PublicTaskID,
			upstreamTaskID,
			code,
			message,
		)
		if transitionErr != nil {
			return errors.Join(attachErr, transitionErr)
		}
		failed, readErr := model.GetTaskByPrimaryID(linkedTaskID)
		if readErr != nil {
			return errors.Join(attachErr, readErr)
		}
		if failed.TaskID != attempt.PublicTaskID ||
			failed.Status != model.TaskStatusFailure ||
			failed.Progress != "100%" {
			return errors.Join(attachErr, model.ErrTaskSubmissionStateConflict)
		}
		if upstreamTaskID != "" &&
			failed.PrivateData.UpstreamTaskID != upstreamTaskID {
			return errors.Join(attachErr, model.ErrTaskUpstreamIDConflict)
		}
		_, refundErr := RefundTaskBillingAttempt(ctx, attempt.RequestID, code)
		return refundErr

	default:
		return model.ErrTaskBillingIdentityDrift
	}
}
