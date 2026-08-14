package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

func ObserveOpenCodeGoProviderFailure(
	channelID int,
	workspaceUID string,
	upstreamModel string,
	failure OpenCodeGoProviderFailure,
	observedAt time.Time,
) (bool, error) {
	classified, ok := ClassifyOpenCodeGoProviderFailure(failure, observedAt)
	if !ok {
		return false, nil
	}
	return applyOpenCodeGoClassifiedFailure(
		channelID,
		workspaceUID,
		upstreamModel,
		classified,
		ReconcileOpenCodeGoPoolChannel,
	)
}

func ObserveOpenCodeGoTransportFailure(
	channelID int,
	workspaceUID string,
	upstreamModel string,
	reason string,
	observedAt time.Time,
) (bool, error) {
	if observedAt.IsZero() {
		return false, errors.New("OpenCode Go transport observation time is required")
	}
	return applyOpenCodeGoClassifiedFailure(
		channelID,
		workspaceUID,
		upstreamModel,
		ClassifyOpenCodeGoTransportFailure(reason, observedAt),
		ReconcileOpenCodeGoPoolChannel,
	)
}

func applyOpenCodeGoClassifiedFailure(
	channelID int,
	workspaceUID string,
	upstreamModel string,
	classified OpenCodeGoClassifiedFailure,
	rebuild func(int) error,
) (bool, error) {
	return applyOpenCodeGoClassifiedFailureWithMutation(
		channelID,
		workspaceUID,
		upstreamModel,
		classified,
		rebuild,
		false,
	)
}

func applyOpenCodeGoClassifiedFailureWithMutation(
	channelID int,
	workspaceUID string,
	upstreamModel string,
	classified OpenCodeGoClassifiedFailure,
	rebuild func(int) error,
	mutationAlreadyHeld bool,
) (bool, error) {
	workspaceUID = strings.TrimSpace(workspaceUID)
	upstreamModel = strings.TrimSpace(upstreamModel)
	if channelID <= 0 || workspaceUID == "" || len(workspaceUID) > 64 {
		return false, errors.New("OpenCode Go health observation target is invalid")
	}
	if classified.Observation.ObservedAt.IsZero() {
		return false, errors.New("OpenCode Go health observation time is required")
	}
	if classified.Observation.Kind == "" {
		return false, nil
	}
	switch classified.Scope {
	case OpenCodeGoHealthScopeWorkspace:
	case OpenCodeGoHealthScopeModel:
		if upstreamModel == "" || len(upstreamModel) > 191 {
			return false, errors.New("OpenCode Go model health observation target is invalid")
		}
	default:
		return false, errors.New("unsupported OpenCode Go health observation scope")
	}
	restrictive := isRestrictiveOpenCodeGoHealthObservation(classified.Observation.Kind)
	releaseMutation := func() {}
	if restrictive && !mutationAlreadyHeld {
		releaseMutation = BeginOpenCodeGoPoolMutation(channelID)
	}
	defer releaseMutation()

	applied, err := persistOpenCodeGoHealthWithRetry(
		channelID,
		workspaceUID,
		upstreamModel,
		classified,
	)
	if err != nil || !applied {
		return false, err
	}
	if restrictive && !mutationAlreadyHeld {
		InvalidateOpenCodeGoIdentityProxyChannel(channelID)
	}
	if rebuild == nil {
		return true, nil
	}
	if err := rebuild(channelID); err != nil {
		return true, err
	}
	return true, nil
}

func persistOpenCodeGoHealthWithRetry(
	channelID int,
	workspaceUID string,
	upstreamModel string,
	classified OpenCodeGoClassifiedFailure,
) (bool, error) {
	applied := false
	err := runOpenCodeGoContentionTransaction(
		context.Background(),
		"health_persistence",
		func(tx *gorm.DB) error {
			applied = false
			return applyOpenCodeGoHealthTransaction(
				tx,
				channelID,
				workspaceUID,
				upstreamModel,
				classified,
				&applied,
			)
		},
	)
	if err != nil {
		return false, err
	}
	return applied, nil
}

func applyOpenCodeGoHealthTransaction(
	tx *gorm.DB,
	channelID int,
	workspaceUID string,
	upstreamModel string,
	classified OpenCodeGoClassifiedFailure,
	applied *bool,
) error {
	var workspace model.OpenCodeGoWorkspace
	query := model.LockForUpdate(tx).
		Where("channel_id = ? AND uid = ?", channelID, workspaceUID).
		Preload("QuotaWindows").
		Preload("Models")
	workspaceResult := query.Limit(1).Find(&workspace)
	if workspaceResult.Error != nil {
		return workspaceResult.Error
	}
	if workspaceResult.RowsAffected == 0 {
		return nil
	}

	switch classified.Scope {
	case OpenCodeGoHealthScopeWorkspace:
		candidate := workspace
		if classified.Observation.Kind == OpenCodeGoObservationCredentialFailure {
			candidate.CredentialStatus = model.OpenCodeGoCredentialError
		}
		next, changed, err := ReduceOpenCodeGoWorkspaceHealth(
			workspace,
			candidate,
			workspace.QuotaWindows,
			classified.Observation,
		)
		if err != nil || !changed {
			return err
		}
		if classified.Observation.Kind == OpenCodeGoObservationRiskBlocked && next.EffectiveState == model.OpenCodeGoStateManualDisabled {
			next.LastError = firstNonEmptyOpenCodeGoMessage(next.LastError, next.StateReason)
		} else {
			next.LastError = firstNonEmptyOpenCodeGoMessage(next.StateReason, next.LastError)
		}
		result := tx.Model(&model.OpenCodeGoWorkspace{}).
			Where("id = ? AND COALESCE(health_observed_at, 0) <= ?", workspace.ID, classified.Observation.ObservedAt.UnixNano()).
			Updates(openCodeGoWorkspaceHealthUpdates(next))
		if result.Error != nil {
			return result.Error
		}
		*applied = result.RowsAffected == 1
		return nil
	case OpenCodeGoHealthScopeModel:
		var entry model.OpenCodeGoWorkspaceModel
		modelResult := model.LockForUpdate(tx).
			Where("workspace_id = ? AND model = ?", workspace.ID, upstreamModel).
			Limit(1).
			Find(&entry)
		if modelResult.Error != nil {
			return modelResult.Error
		}
		if modelResult.RowsAffected == 0 {
			return nil
		}
		next, changed, err := ReduceOpenCodeGoModelHealth(entry, classified.Observation)
		if err != nil || !changed {
			return err
		}
		result := tx.Model(&model.OpenCodeGoWorkspaceModel{}).
			Where("id = ? AND COALESCE(health_observed_at, 0) <= ?", entry.ID, classified.Observation.ObservedAt.UnixNano()).
			Updates(map[string]interface{}{
				"state":              next.State,
				"disabled_until":     next.DisabledUntil,
				"last_error_code":    next.LastErrorCode,
				"last_error":         next.LastError,
				"health_observation": next.HealthObservation,
				"health_observed_at": next.HealthObservedAt,
				"updated_at":         common.GetTimestamp(),
			})
		if result.Error != nil {
			return result.Error
		}
		*applied = result.RowsAffected == 1
		return nil
	default:
		return errors.New("unsupported OpenCode Go health observation scope")
	}
}

func isRestrictiveOpenCodeGoHealthObservation(kind OpenCodeGoHealthObservationKind) bool {
	switch kind {
	case OpenCodeGoObservationCredentialFailure,
		OpenCodeGoObservationQuotaExhausted,
		OpenCodeGoObservationRiskBlocked,
		OpenCodeGoObservationRegionBlocked,
		OpenCodeGoObservationModelBlocked,
		OpenCodeGoObservationRPMThrottled,
		OpenCodeGoObservationTransientFailure,
		OpenCodeGoObservationBulkFailure:
		return true
	default:
		return false
	}
}

func openCodeGoWorkspaceHealthUpdates(workspace model.OpenCodeGoWorkspace) map[string]interface{} {
	return map[string]interface{}{
		"credential_status":        workspace.CredentialStatus,
		"effective_state":          workspace.EffectiveState,
		"state_reason":             workspace.StateReason,
		"health_observation":       workspace.HealthObservation,
		"health_observed_at":       workspace.HealthObservedAt,
		"cooldown_until":           workspace.CooldownUntil,
		"quota_next_refresh_at":    workspace.QuotaNextRefreshAt,
		"quota_recovery_at":        workspace.QuotaRecoveryAt,
		"risk_detected_at":         workspace.RiskDetectedAt,
		"risk_last_checked_at":     workspace.RiskLastCheckedAt,
		"bulk_failure_detected_at": workspace.BulkFailureDetectedAt,
		"last_error":               workspace.LastError,
		"updated_at":               common.GetTimestamp(),
	}
}
