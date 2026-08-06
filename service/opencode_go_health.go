package service

import (
	"errors"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/model"
)

type OpenCodeGoHealthObservationKind string

const (
	OpenCodeGoObservationConsoleSnapshot       OpenCodeGoHealthObservationKind = "console_snapshot"
	OpenCodeGoObservationStaleSnapshot         OpenCodeGoHealthObservationKind = "stale_snapshot"
	OpenCodeGoObservationAuthenticationFailure OpenCodeGoHealthObservationKind = "authentication_failure"
	OpenCodeGoObservationCredentialFailure     OpenCodeGoHealthObservationKind = "credential_failure"
	OpenCodeGoObservationQuotaExhausted        OpenCodeGoHealthObservationKind = "quota_exhausted"
	OpenCodeGoObservationRiskBlocked           OpenCodeGoHealthObservationKind = "risk_blocked"
	OpenCodeGoObservationRiskProbeSucceeded    OpenCodeGoHealthObservationKind = "risk_probe_succeeded"
	OpenCodeGoObservationRiskProbeFailed       OpenCodeGoHealthObservationKind = "risk_probe_failed"
	OpenCodeGoObservationManualDisabled        OpenCodeGoHealthObservationKind = "manual_disabled"
	OpenCodeGoObservationManualEnabled         OpenCodeGoHealthObservationKind = "manual_enabled"
	OpenCodeGoObservationRegionBlocked         OpenCodeGoHealthObservationKind = "region_blocked"
	OpenCodeGoObservationModelBlocked          OpenCodeGoHealthObservationKind = "model_blocked"
	OpenCodeGoObservationRPMThrottled          OpenCodeGoHealthObservationKind = "rpm_throttled"
	OpenCodeGoObservationTransientFailure      OpenCodeGoHealthObservationKind = "transient_failure"
	OpenCodeGoObservationModelProbeSucceeded   OpenCodeGoHealthObservationKind = "model_probe_succeeded"
	OpenCodeGoObservationCooldownExpired       OpenCodeGoHealthObservationKind = "cooldown_expired"
	// OpenCodeGoObservationBulkFailure marks a workspace auto-disabled by
	// repeated persistent provider failures (for example bulk 401/403),
	// awaiting manual verification before it can be re-enabled.
	OpenCodeGoObservationBulkFailure           OpenCodeGoHealthObservationKind = "bulk_failure"
)

type OpenCodeGoHealthObservation struct {
	Kind            OpenCodeGoHealthObservationKind
	ObservedAt      time.Time
	Reason          string
	ErrorCode       string
	QuotaKind       string
	Deadline        time.Time
	HasUsableModels bool
}

func ReduceOpenCodeGoWorkspaceHealth(
	current model.OpenCodeGoWorkspace,
	candidate model.OpenCodeGoWorkspace,
	windows []model.OpenCodeGoQuotaWindow,
	observation OpenCodeGoHealthObservation,
) (model.OpenCodeGoWorkspace, bool, error) {
	if observation.ObservedAt.IsZero() {
		return current, false, errors.New("OpenCode Go health observation time is required")
	}
	if !canApplyOpenCodeGoWorkspaceObservation(current, observation) {
		return current, false, nil
	}
	observedAt := observation.ObservedAt.UnixNano()

	reason := sanitizeOpenCodeGoHealthReason(observation.Reason)
	switch observation.Kind {
	case OpenCodeGoObservationManualDisabled:
		candidate = current
		candidate.ManualEnabled = false
		candidate.EffectiveState = model.OpenCodeGoStateManualDisabled
		candidate.StateReason = firstNonEmptyOpenCodeGoMessage(reason, "workspace is manually disabled")
		candidate.CooldownUntil = 0
	case OpenCodeGoObservationManualEnabled:
		if current.ManualEnabled &&
			current.EffectiveState != model.OpenCodeGoStateManualDisabled &&
			current.EffectiveState != model.OpenCodeGoStateBulkDisabled {
			return current, false, nil
		}
		candidate = current
		candidate.ManualEnabled = true
		// A human re-enabled this workspace after verification; clear any
		// auto-disable evidence so a later refresh can restore eligibility.
		candidate.BulkFailureDetectedAt = 0
		candidate.EffectiveState, candidate.StateReason, candidate.QuotaRecoveryAt = deriveOpenCodeGoWorkspaceHealth(
			candidate,
			windows,
			observation.HasUsableModels,
		)
	case OpenCodeGoObservationConsoleSnapshot:
		if current.EffectiveState == model.OpenCodeGoStateManualDisabled || !current.ManualEnabled {
			candidate.ManualEnabled = false
			candidate.EffectiveState = model.OpenCodeGoStateManualDisabled
			candidate.StateReason = firstNonEmptyOpenCodeGoMessage(current.StateReason, "workspace is manually disabled")
			candidate.QuotaRecoveryAt = current.QuotaRecoveryAt
			break
		}
		if current.EffectiveState == model.OpenCodeGoStateRiskBlocked {
			candidate.EffectiveState = current.EffectiveState
			candidate.StateReason = current.StateReason
			candidate.RiskDetectedAt = current.RiskDetectedAt
			candidate.QuotaRecoveryAt = current.QuotaRecoveryAt
			return candidate, true, nil
		}
		candidate.EffectiveState, candidate.StateReason, candidate.QuotaRecoveryAt = deriveOpenCodeGoWorkspaceHealth(
			candidate,
			windows,
			observation.HasUsableModels,
		)
		if current.EffectiveState == model.OpenCodeGoStateQuotaExhausted &&
			candidate.EffectiveState == model.OpenCodeGoStateEligible &&
			current.QuotaRecoveryAt > 0 && candidate.QuotaFetchedAt < current.QuotaRecoveryAt {
			candidate.EffectiveState = model.OpenCodeGoStateQuotaExhausted
			candidate.StateReason = firstNonEmptyOpenCodeGoMessage(current.StateReason, "quota recovery awaits the authoritative reset node")
			candidate.QuotaRecoveryAt = current.QuotaRecoveryAt
		}
	case OpenCodeGoObservationStaleSnapshot:
		if openCodeGoWorkspaceStatePriority(current.EffectiveState) > openCodeGoWorkspaceStatePriority(model.OpenCodeGoStateStale) {
			candidate.EffectiveState = current.EffectiveState
			candidate.StateReason = current.StateReason
			candidate.RiskDetectedAt = current.RiskDetectedAt
			candidate.RiskLastCheckedAt = current.RiskLastCheckedAt
			candidate.QuotaRecoveryAt = current.QuotaRecoveryAt
			candidate.CooldownUntil = current.CooldownUntil
			break
		}
		candidate.EffectiveState = model.OpenCodeGoStateStale
		candidate.StateReason = firstNonEmptyOpenCodeGoMessage(reason, "authoritative console snapshot is stale")
	case OpenCodeGoObservationAuthenticationFailure:
		candidate = current
		if current.EffectiveState == model.OpenCodeGoStateManualDisabled || current.EffectiveState == model.OpenCodeGoStateRiskBlocked {
			return current, false, nil
		}
		candidate.EffectiveState = model.OpenCodeGoStateAuthError
		candidate.StateReason = firstNonEmptyOpenCodeGoMessage(reason, "OpenCode Go console authentication failed")
	case OpenCodeGoObservationCredentialFailure:
		candidate = current
		if openCodeGoWorkspaceStatePriority(current.EffectiveState) > openCodeGoWorkspaceStatePriority(model.OpenCodeGoStateKeyError) {
			return current, false, nil
		}
		candidate.EffectiveState = model.OpenCodeGoStateKeyError
		candidate.StateReason = firstNonEmptyOpenCodeGoMessage(reason, "OpenCode Go workspace API key is invalid")
	case OpenCodeGoObservationQuotaExhausted:
		candidate = current
		if openCodeGoWorkspaceStatePriority(current.EffectiveState) > openCodeGoWorkspaceStatePriority(model.OpenCodeGoStateQuotaExhausted) {
			return current, false, nil
		}
		candidate.EffectiveState = model.OpenCodeGoStateQuotaExhausted
		candidate.StateReason = firstNonEmptyOpenCodeGoMessage(reason, "OpenCode Go quota is exhausted")
		candidate.QuotaRecoveryAt = openCodeGoQuotaRecoveryAt(windows)
		if candidate.QuotaRecoveryAt == 0 && observation.QuotaKind != "" {
			candidate.QuotaRecoveryAt = openCodeGoQuotaWindowResetAt(windows, observation.QuotaKind)
		}
		if !observation.Deadline.IsZero() && observation.Deadline.Unix() > candidate.QuotaRecoveryAt {
			candidate.QuotaRecoveryAt = observation.Deadline.Unix()
		}
		candidate.QuotaNextRefreshAt = observation.ObservedAt.Unix()
	case OpenCodeGoObservationRiskBlocked:
		candidate = current
		if current.EffectiveState == model.OpenCodeGoStateManualDisabled {
			return current, false, nil
		}
		candidate.EffectiveState = model.OpenCodeGoStateRiskBlocked
		candidate.StateReason = firstNonEmptyOpenCodeGoMessage(reason, "request blocked by upstream provider")
		if candidate.RiskDetectedAt == 0 {
			candidate.RiskDetectedAt = observation.ObservedAt.Unix()
		}
		candidate.RiskLastCheckedAt = observation.ObservedAt.Unix()
	case OpenCodeGoObservationRiskProbeSucceeded:
		if current.EffectiveState != model.OpenCodeGoStateRiskBlocked {
			return current, false, nil
		}
		candidate = current
		candidate.RiskLastCheckedAt = observation.ObservedAt.Unix()
		candidate.RiskDetectedAt = 0
		candidate.EffectiveState, candidate.StateReason, candidate.QuotaRecoveryAt = deriveOpenCodeGoWorkspaceHealth(
			candidate,
			windows,
			observation.HasUsableModels,
		)
	case OpenCodeGoObservationRiskProbeFailed:
		if current.EffectiveState != model.OpenCodeGoStateRiskBlocked {
			return current, false, nil
		}
		candidate = current
		candidate.RiskLastCheckedAt = observation.ObservedAt.Unix()
	case OpenCodeGoObservationBulkFailure:
		if current.EffectiveState == model.OpenCodeGoStateManualDisabled {
			return current, false, nil
		}
		candidate = current
		candidate.BulkFailureDetectedAt = observation.ObservedAt.Unix()
		candidate.EffectiveState = model.OpenCodeGoStateBulkDisabled
		candidate.StateReason = firstNonEmptyOpenCodeGoMessage(reason, "auto-disabled awaiting manual verification after repeated provider failures")
	default:
		return current, false, errors.New("unsupported workspace-scoped OpenCode Go health observation")
	}

	candidate.HealthObservation = string(observation.Kind)
	candidate.HealthObservedAt = observedAt
	if candidate.EffectiveState != model.OpenCodeGoStateCooldown {
		candidate.CooldownUntil = 0
	}
	return candidate, true, nil
}

func canApplyOpenCodeGoWorkspaceObservation(
	current model.OpenCodeGoWorkspace,
	observation OpenCodeGoHealthObservation,
) bool {
	observedAt := observation.ObservedAt.UnixNano()
	if current.HealthObservedAt != observedAt {
		return current.HealthObservedAt < observedAt
	}
	return openCodeGoWorkspaceObservationPriority(current.HealthObservation) <
		openCodeGoWorkspaceObservationPriority(string(observation.Kind))
}

func ReduceOpenCodeGoModelHealth(
	current model.OpenCodeGoWorkspaceModel,
	observation OpenCodeGoHealthObservation,
) (model.OpenCodeGoWorkspaceModel, bool, error) {
	if observation.ObservedAt.IsZero() {
		return current, false, errors.New("OpenCode Go model health observation time is required")
	}
	observedAt := observation.ObservedAt.UnixNano()
	if current.HealthObservedAt > observedAt ||
		(current.HealthObservedAt == observedAt &&
			openCodeGoModelObservationPriority(current.HealthObservation) >= openCodeGoModelObservationPriority(string(observation.Kind))) {
		return current, false, nil
	}

	next := current
	reason := sanitizeOpenCodeGoHealthReason(observation.Reason)
	switch observation.Kind {
	case OpenCodeGoObservationRegionBlocked:
		next.State = model.OpenCodeGoModelRegionBlocked
		next.DisabledUntil = observation.Deadline.Unix()
	case OpenCodeGoObservationModelBlocked:
		next.State = model.OpenCodeGoModelDisabled
		next.DisabledUntil = observation.Deadline.Unix()
	case OpenCodeGoObservationRPMThrottled:
		next.State = model.OpenCodeGoModelRPMCooldown
		next.DisabledUntil = observation.Deadline.Unix()
	case OpenCodeGoObservationTransientFailure:
		next.State = model.OpenCodeGoModelTransient
		next.DisabledUntil = observation.Deadline.Unix()
	case OpenCodeGoObservationModelProbeSucceeded:
		next.State = model.OpenCodeGoModelAvailable
		next.DisabledUntil = 0
		next.LastErrorCode = ""
		next.LastError = ""
	case OpenCodeGoObservationCooldownExpired:
		if current.DisabledUntil <= 0 || observation.ObservedAt.Unix() < current.DisabledUntil {
			return current, false, nil
		}
		next.State = model.OpenCodeGoModelAvailable
		next.DisabledUntil = 0
		next.LastErrorCode = ""
		next.LastError = ""
	default:
		return current, false, errors.New("unsupported model-scoped OpenCode Go health observation")
	}
	if observation.Kind != OpenCodeGoObservationModelProbeSucceeded && observation.Kind != OpenCodeGoObservationCooldownExpired {
		if next.DisabledUntil <= observation.ObservedAt.Unix() {
			next.DisabledUntil = observation.ObservedAt.Add(time.Minute).Unix()
		}
		next.LastErrorCode = sanitizeOpenCodeGoErrorIdentifier(observation.ErrorCode)
		next.LastError = reason
	}
	next.HealthObservation = string(observation.Kind)
	next.HealthObservedAt = observedAt
	return next, true, nil
}

func deriveOpenCodeGoWorkspaceHealth(
	workspace model.OpenCodeGoWorkspace,
	windows []model.OpenCodeGoQuotaWindow,
	hasUsableModels bool,
) (string, string, int64) {
	if !workspace.ManualEnabled {
		return model.OpenCodeGoStateManualDisabled, "workspace is manually disabled", workspace.QuotaRecoveryAt
	}
	if workspace.RiskDetectedAt > 0 {
		return model.OpenCodeGoStateRiskBlocked, firstNonEmptyOpenCodeGoMessage(workspace.LastError, "request blocked by upstream provider"), workspace.QuotaRecoveryAt
	}
	if workspace.BulkFailureDetectedAt > 0 {
		return model.OpenCodeGoStateBulkDisabled, firstNonEmptyOpenCodeGoMessage(workspace.LastError, "auto-disabled awaiting manual verification after repeated provider failures"), workspace.QuotaRecoveryAt
	}
	if workspace.MembershipStatus == model.OpenCodeGoMembershipInactive {
		return model.OpenCodeGoStateMembershipExpired, "OpenCode Go membership is inactive", workspace.QuotaRecoveryAt
	}
	if workspace.MembershipStatus != model.OpenCodeGoMembershipActive {
		return model.OpenCodeGoStateStale, "OpenCode Go membership status is not authoritative", workspace.QuotaRecoveryAt
	}
	if workspace.QuotaSnapshotStatus != model.OpenCodeGoQuotaSnapshotComplete || len(windows) != len(model.OpenCodeGoQuotaKinds) {
		return model.OpenCodeGoStateStale, firstNonEmptyOpenCodeGoMessage(workspace.QuotaError, "authoritative quota snapshot is incomplete"), workspace.QuotaRecoveryAt
	}
	if workspace.CredentialStatus != model.OpenCodeGoCredentialValid || workspace.APIKeyCiphertext == "" {
		return model.OpenCodeGoStateKeyError, "OpenCode Go workspace API key is unavailable", workspace.QuotaRecoveryAt
	}
	if !hasUsableModels {
		return model.OpenCodeGoStateStale, "OpenCode Go model discovery is unavailable", workspace.QuotaRecoveryAt
	}
	if recoveryAt := openCodeGoQuotaRecoveryAt(windows); recoveryAt > 0 {
		return model.OpenCodeGoStateQuotaExhausted, "OpenCode Go quota is exhausted", recoveryAt
	}
	return model.OpenCodeGoStateEligible, "", 0
}

func openCodeGoQuotaRecoveryAt(windows []model.OpenCodeGoQuotaWindow) int64 {
	recoveryAt := int64(0)
	for _, window := range windows {
		if window.UsedPercent < 100 {
			continue
		}
		if window.ResetAt > recoveryAt {
			recoveryAt = window.ResetAt
		}
	}
	return recoveryAt
}

func openCodeGoQuotaWindowResetAt(windows []model.OpenCodeGoQuotaWindow, kind string) int64 {
	for _, window := range windows {
		if window.Kind == kind {
			return window.ResetAt
		}
	}
	return 0
}

func openCodeGoWorkspaceStatePriority(state string) int {
	switch state {
	case model.OpenCodeGoStateManualDisabled:
		return 100
	case model.OpenCodeGoStateRiskBlocked:
		return 90
	case model.OpenCodeGoStateBulkDisabled:
		return 85
	case model.OpenCodeGoStateAuthError:
		return 80
	case model.OpenCodeGoStateKeyError:
		return 75
	case model.OpenCodeGoStateMembershipExpired:
		return 70
	case model.OpenCodeGoStateStale:
		return 60
	case model.OpenCodeGoStateQuotaExhausted:
		return 50
	case model.OpenCodeGoStateCooldown:
		return 40
	case model.OpenCodeGoStateEligible:
		return 10
	default:
		return 0
	}
}

func openCodeGoWorkspaceObservationPriority(kind string) int {
	switch OpenCodeGoHealthObservationKind(kind) {
	case OpenCodeGoObservationManualDisabled:
		return 100
	case OpenCodeGoObservationRiskBlocked:
		return 90
	case OpenCodeGoObservationBulkFailure:
		return 85
	case OpenCodeGoObservationAuthenticationFailure:
		return 80
	case OpenCodeGoObservationCredentialFailure:
		return 75
	case OpenCodeGoObservationStaleSnapshot:
		return 60
	case OpenCodeGoObservationQuotaExhausted:
		return 50
	case OpenCodeGoObservationConsoleSnapshot:
		return 30
	case OpenCodeGoObservationManualEnabled:
		return 20
	case OpenCodeGoObservationRiskProbeFailed:
		return 15
	case OpenCodeGoObservationRiskProbeSucceeded:
		return 10
	default:
		return 0
	}
}

func openCodeGoModelObservationPriority(kind string) int {
	switch OpenCodeGoHealthObservationKind(kind) {
	case OpenCodeGoObservationRegionBlocked:
		return 80
	case OpenCodeGoObservationModelBlocked:
		return 70
	case OpenCodeGoObservationRPMThrottled:
		return 60
	case OpenCodeGoObservationTransientFailure:
		return 50
	case OpenCodeGoObservationModelProbeSucceeded:
		return 20
	case OpenCodeGoObservationCooldownExpired:
		return 10
	default:
		return 0
	}
}

func sanitizeOpenCodeGoHealthReason(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return ""
	}
	return sanitizeOpenCodeGoError(errors.New(reason))
}

func sanitizeOpenCodeGoErrorIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 96 {
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
