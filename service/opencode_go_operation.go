package service

import (
	"errors"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/google/uuid"
)

const (
	OpenCodeGoOperationStatusRunning   = "running"
	OpenCodeGoOperationStatusSucceeded = "succeeded"
	OpenCodeGoOperationStatusFailed    = "failed"

	OpenCodeGoOperationRiskRecheck        = "risk_recheck"
	OpenCodeGoOperationEnableChinaModels  = "enable_china_models"
	OpenCodeGoOperationApplyReferral      = "apply_referral_reward"
	OpenCodeGoOperationCancelSubscription = "cancel_subscription_renewal"
)

func startOpenCodeGoOperation(
	channelID int,
	workspace model.OpenCodeGoWorkspace,
	action string,
	source string,
	now time.Time,
) (*model.OpenCodeGoOperation, error) {
	if channelID <= 0 || workspace.ID <= 0 || workspace.UID == "" || action == "" || now.IsZero() {
		return nil, errors.New("OpenCode Go operation target is invalid")
	}
	operation := &model.OpenCodeGoOperation{
		UID:          uuid.NewString(),
		ChannelID:    channelID,
		WorkspaceID:  workspace.ID,
		WorkspaceUID: workspace.UID,
		Action:       sanitizeOpenCodeGoOperationValue(action, 64),
		Source:       sanitizeOpenCodeGoOperationValue(source, 32),
		Status:       OpenCodeGoOperationStatusRunning,
		StartedAt:    now.Unix(),
	}
	if operation.Source == "" {
		operation.Source = "system"
	}
	if err := model.DB.Create(operation).Error; err != nil {
		return nil, err
	}
	return operation, nil
}

func finishOpenCodeGoOperation(
	operation *model.OpenCodeGoOperation,
	status string,
	result string,
	err error,
	now time.Time,
) error {
	if operation == nil || operation.ID <= 0 {
		return errors.New("OpenCode Go operation is missing")
	}
	if now.IsZero() {
		now = time.Now()
	}
	errorMessage := ""
	if err != nil {
		errorMessage = sanitizeOpenCodeGoError(err)
	}
	resultValue := sanitizeOpenCodeGoOperationResult(result)
	finishedAt := now.Unix()
	updateErr := model.DB.Model(&model.OpenCodeGoOperation{}).
		Where("id = ?", operation.ID).
		Updates(map[string]interface{}{
			"status":      sanitizeOpenCodeGoOperationValue(status, 32),
			"finished_at": finishedAt,
			"result":      resultValue,
			"error":       errorMessage,
			"updated_at":  common.GetTimestamp(),
		}).Error
	if updateErr != nil {
		return updateErr
	}
	operation.Status = sanitizeOpenCodeGoOperationValue(status, 32)
	operation.FinishedAt = finishedAt
	operation.Result = resultValue
	operation.Error = errorMessage
	return nil
}

func sanitizeOpenCodeGoOperationValue(value string, limit int) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > limit {
		return ""
	}
	for index := 0; index < len(value); index++ {
		char := value[index]
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.' || char == ':' {
			continue
		}
		return ""
	}
	return value
}

func sanitizeOpenCodeGoOperationResult(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return sanitizeOpenCodeGoError(errors.New(value))
}
