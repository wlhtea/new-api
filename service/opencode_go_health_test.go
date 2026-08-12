package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openCodeGoHealthyReducerFixture(fetchedAt int64) (model.OpenCodeGoWorkspace, []model.OpenCodeGoQuotaWindow) {
	workspace := model.OpenCodeGoWorkspace{
		UID:                 "workspace-health-test",
		ManualEnabled:       true,
		CredentialStatus:    model.OpenCodeGoCredentialValid,
		MembershipStatus:    model.OpenCodeGoMembershipActive,
		APIKeyCiphertext:    "encrypted-test-value",
		EffectiveState:      model.OpenCodeGoStateEligible,
		QuotaSnapshotStatus: model.OpenCodeGoQuotaSnapshotComplete,
		QuotaFetchedAt:      fetchedAt,
	}
	windows := make([]model.OpenCodeGoQuotaWindow, 0, len(model.OpenCodeGoQuotaKinds))
	for index, kind := range model.OpenCodeGoQuotaKinds {
		windows = append(windows, model.OpenCodeGoQuotaWindow{
			Kind:        kind,
			UsedPercent: float64(10 + index),
			ResetAt:     fetchedAt + int64((index+1)*3600),
			FetchedAt:   fetchedAt,
		})
	}
	return workspace, windows
}

func TestReduceOpenCodeGoWorkspaceHealthPrecedenceAndRecoveryEvidence(t *testing.T) {
	baseTime := time.Unix(1_900_000_000, 0)
	workspace, windows := openCodeGoHealthyReducerFixture(baseTime.Unix())

	risk, applied, err := ReduceOpenCodeGoWorkspaceHealth(
		workspace,
		workspace,
		windows,
		OpenCodeGoHealthObservation{
			Kind:       OpenCodeGoObservationRiskBlocked,
			ObservedAt: baseTime.Add(time.Minute),
			Reason:     "Request blocked by upstream provider.",
		},
	)
	require.NoError(t, err)
	require.True(t, applied)
	require.Equal(t, model.OpenCodeGoStateRiskBlocked, risk.EffectiveState)

	consoleCandidate := risk
	consoleCandidate.MembershipStatus = model.OpenCodeGoMembershipActive
	consoleCandidate.QuotaFetchedAt = baseTime.Add(2 * time.Minute).Unix()
	preserved, applied, err := ReduceOpenCodeGoWorkspaceHealth(
		risk,
		consoleCandidate,
		windows,
		OpenCodeGoHealthObservation{
			Kind:            OpenCodeGoObservationConsoleSnapshot,
			ObservedAt:      baseTime.Add(2 * time.Minute),
			HasUsableModels: true,
		},
	)
	require.NoError(t, err)
	require.True(t, applied)
	assert.Equal(t, model.OpenCodeGoStateRiskBlocked, preserved.EffectiveState)

	recovered, applied, err := ReduceOpenCodeGoWorkspaceHealth(
		preserved,
		preserved,
		windows,
		OpenCodeGoHealthObservation{
			Kind:            OpenCodeGoObservationRiskProbeSucceeded,
			ObservedAt:      baseTime.Add(3 * time.Minute),
			HasUsableModels: true,
		},
	)
	require.NoError(t, err)
	require.True(t, applied)
	assert.Equal(t, model.OpenCodeGoStateEligible, recovered.EffectiveState)
	assert.Zero(t, recovered.RiskDetectedAt)
}

func TestReduceOpenCodeGoWorkspaceHealthQuotaWaitsForLatestExhaustedReset(t *testing.T) {
	baseTime := time.Unix(1_900_000_000, 0)
	workspace, windows := openCodeGoHealthyReducerFixture(baseTime.Unix())
	windows[0].UsedPercent = 100
	windows[0].ResetAt = baseTime.Add(time.Hour).Unix()
	windows[2].UsedPercent = 101
	windows[2].ResetAt = baseTime.Add(3 * time.Hour).Unix()

	exhausted, applied, err := ReduceOpenCodeGoWorkspaceHealth(
		workspace,
		workspace,
		windows,
		OpenCodeGoHealthObservation{
			Kind:            OpenCodeGoObservationConsoleSnapshot,
			ObservedAt:      baseTime,
			HasUsableModels: true,
		},
	)
	require.NoError(t, err)
	require.True(t, applied)
	require.Equal(t, model.OpenCodeGoStateQuotaExhausted, exhausted.EffectiveState)
	require.Equal(t, baseTime.Add(3*time.Hour).Unix(), exhausted.QuotaRecoveryAt)

	availableWindows := append([]model.OpenCodeGoQuotaWindow(nil), windows...)
	for index := range availableWindows {
		availableWindows[index].UsedPercent = 20
	}
	early := exhausted
	early.QuotaFetchedAt = baseTime.Add(2 * time.Hour).Unix()
	stillExhausted, applied, err := ReduceOpenCodeGoWorkspaceHealth(
		exhausted,
		early,
		availableWindows,
		OpenCodeGoHealthObservation{
			Kind:            OpenCodeGoObservationConsoleSnapshot,
			ObservedAt:      baseTime.Add(2 * time.Hour),
			HasUsableModels: true,
		},
	)
	require.NoError(t, err)
	require.True(t, applied)
	assert.Equal(t, model.OpenCodeGoStateQuotaExhausted, stillExhausted.EffectiveState)

	afterReset := stillExhausted
	afterReset.QuotaFetchedAt = baseTime.Add(3*time.Hour + time.Second).Unix()
	recovered, applied, err := ReduceOpenCodeGoWorkspaceHealth(
		stillExhausted,
		afterReset,
		availableWindows,
		OpenCodeGoHealthObservation{
			Kind:            OpenCodeGoObservationConsoleSnapshot,
			ObservedAt:      baseTime.Add(3*time.Hour + time.Second),
			HasUsableModels: true,
		},
	)
	require.NoError(t, err)
	require.True(t, applied)
	assert.Equal(t, model.OpenCodeGoStateEligible, recovered.EffectiveState)
	assert.Zero(t, recovered.QuotaRecoveryAt)
}

func TestReduceOpenCodeGoWorkspaceHealthRejectsOutOfOrderSuccess(t *testing.T) {
	baseTime := time.Unix(1_900_000_000, 0)
	workspace, windows := openCodeGoHealthyReducerFixture(baseTime.Unix())
	workspace.EffectiveState = model.OpenCodeGoStateRiskBlocked
	workspace.HealthObservedAt = baseTime.Add(2 * time.Minute).UnixNano()
	workspace.RiskDetectedAt = baseTime.Unix()

	reduced, applied, err := ReduceOpenCodeGoWorkspaceHealth(
		workspace,
		workspace,
		windows,
		OpenCodeGoHealthObservation{
			Kind:            OpenCodeGoObservationRiskProbeSucceeded,
			ObservedAt:      baseTime.Add(time.Minute),
			HasUsableModels: true,
		},
	)
	require.NoError(t, err)
	assert.False(t, applied)
	assert.Equal(t, workspace, reduced)
}

func TestReduceOpenCodeGoModelHealthIsScopedAndDeadlineBound(t *testing.T) {
	baseTime := time.Unix(1_900_000_000, 0)
	entry := model.OpenCodeGoWorkspaceModel{Model: "glm-5.2", Discovered: true, State: model.OpenCodeGoModelAvailable}

	cooling, applied, err := ReduceOpenCodeGoModelHealth(entry, OpenCodeGoHealthObservation{
		Kind:       OpenCodeGoObservationRPMThrottled,
		ObservedAt: baseTime,
		Deadline:   baseTime.Add(90 * time.Second),
		ErrorCode:  "RateLimitError",
		Reason:     "rate limited",
	})
	require.NoError(t, err)
	require.True(t, applied)
	require.Equal(t, model.OpenCodeGoModelRPMCooldown, cooling.State)
	require.Equal(t, baseTime.Add(90*time.Second).Unix(), cooling.DisabledUntil)

	unchanged, applied, err := ReduceOpenCodeGoModelHealth(cooling, OpenCodeGoHealthObservation{
		Kind:       OpenCodeGoObservationCooldownExpired,
		ObservedAt: baseTime.Add(time.Minute),
	})
	require.NoError(t, err)
	assert.False(t, applied)
	assert.Equal(t, cooling, unchanged)

	recovered, applied, err := ReduceOpenCodeGoModelHealth(cooling, OpenCodeGoHealthObservation{
		Kind:       OpenCodeGoObservationCooldownExpired,
		ObservedAt: baseTime.Add(90 * time.Second),
	})
	require.NoError(t, err)
	require.True(t, applied)
	assert.Equal(t, model.OpenCodeGoModelAvailable, recovered.State)
	assert.Zero(t, recovered.DisabledUntil)
}

func TestReduceOpenCodeGoWorkspaceHealthManualDisableOutranksRisk(t *testing.T) {
	baseTime := time.Unix(1_900_000_000, 0)
	workspace, windows := openCodeGoHealthyReducerFixture(baseTime.Unix())
	const blockedReason = "This account has found to be committing fraud or is in breach of terms of services and has been blocked."
	disabled, applied, err := ReduceOpenCodeGoWorkspaceHealth(
		workspace,
		workspace,
		windows,
		OpenCodeGoHealthObservation{Kind: OpenCodeGoObservationManualDisabled, ObservedAt: baseTime},
	)
	require.NoError(t, err)
	require.True(t, applied)

	blocked, applied, err := ReduceOpenCodeGoWorkspaceHealth(
		disabled,
		disabled,
		windows,
		OpenCodeGoHealthObservation{
			Kind:       OpenCodeGoObservationRiskBlocked,
			ObservedAt: baseTime.Add(time.Minute),
			Reason:     blockedReason,
		},
	)
	require.NoError(t, err)
	require.True(t, applied)
	assert.Equal(t, model.OpenCodeGoStateManualDisabled, blocked.EffectiveState)
	assert.Equal(t, baseTime.Add(time.Minute).Unix(), blocked.RiskDetectedAt)
	assert.Equal(t, baseTime.Add(time.Minute).Unix(), blocked.RiskLastCheckedAt)
	assert.Equal(t, blockedReason, blocked.LastError)

	consoleCandidate := blocked
	consoleCandidate.LastError = ""
	consoleCandidate.QuotaFetchedAt = baseTime.Add(2 * time.Minute).Unix()
	preserved, applied, err := ReduceOpenCodeGoWorkspaceHealth(
		blocked,
		consoleCandidate,
		windows,
		OpenCodeGoHealthObservation{
			Kind:            OpenCodeGoObservationConsoleSnapshot,
			ObservedAt:      baseTime.Add(2 * time.Minute),
			HasUsableModels: true,
		},
	)
	require.NoError(t, err)
	require.True(t, applied)
	assert.Equal(t, model.OpenCodeGoStateManualDisabled, preserved.EffectiveState)
	assert.Equal(t, blocked.RiskDetectedAt, preserved.RiskDetectedAt)
	assert.Equal(t, blockedReason, preserved.LastError)

	reenabled, applied, err := ReduceOpenCodeGoWorkspaceHealth(
		preserved,
		preserved,
		windows,
		OpenCodeGoHealthObservation{
			Kind:            OpenCodeGoObservationManualEnabled,
			ObservedAt:      baseTime.Add(3 * time.Minute),
			HasUsableModels: true,
		},
	)
	require.NoError(t, err)
	require.True(t, applied)
	assert.True(t, reenabled.ManualEnabled)
	assert.Equal(t, model.OpenCodeGoStateRiskBlocked, reenabled.EffectiveState)
	assert.Equal(t, blocked.RiskDetectedAt, reenabled.RiskDetectedAt)
	assert.Equal(t, blockedReason, reenabled.StateReason)
}

func TestReduceOpenCodeGoWorkspaceHealthRiskOutranksBulkFailure(t *testing.T) {
	baseTime := time.Unix(1_900_000_000, 0)
	workspace, windows := openCodeGoHealthyReducerFixture(baseTime.Unix())
	workspace.EffectiveState = model.OpenCodeGoStateRiskBlocked
	workspace.StateReason = "authoritative fraud block"
	workspace.LastError = workspace.StateReason
	workspace.RiskDetectedAt = baseTime.Unix()
	workspace.RiskLastCheckedAt = baseTime.Unix()
	workspace.HealthObservation = string(OpenCodeGoObservationRiskBlocked)
	workspace.HealthObservedAt = baseTime.UnixNano()

	reduced, applied, err := ReduceOpenCodeGoWorkspaceHealth(
		workspace,
		workspace,
		windows,
		OpenCodeGoHealthObservation{
			Kind:       OpenCodeGoObservationBulkFailure,
			ObservedAt: baseTime.Add(time.Minute),
			Reason:     "repeated provider failures",
		},
	)
	require.NoError(t, err)
	assert.False(t, applied)
	assert.Equal(t, workspace, reduced)
}

func TestReduceOpenCodeGoWorkspaceHealthPreservesStaleCandidateSnapshotFields(t *testing.T) {
	baseTime := time.Unix(1_900_000_000, 0)
	workspace, windows := openCodeGoHealthyReducerFixture(baseTime.Unix())
	candidate := workspace
	candidate.QuotaSnapshotStatus = model.OpenCodeGoQuotaSnapshotError
	candidate.QuotaError = "quota parser failed"
	candidate.LastError = candidate.QuotaError

	reduced, applied, err := ReduceOpenCodeGoWorkspaceHealth(
		workspace,
		candidate,
		windows,
		OpenCodeGoHealthObservation{
			Kind:       OpenCodeGoObservationStaleSnapshot,
			ObservedAt: baseTime,
			Reason:     candidate.QuotaError,
		},
	)
	require.NoError(t, err)
	require.True(t, applied)
	assert.Equal(t, model.OpenCodeGoQuotaSnapshotError, reduced.QuotaSnapshotStatus)
	assert.Equal(t, "quota parser failed", reduced.QuotaError)
	assert.Equal(t, model.OpenCodeGoStateStale, reduced.EffectiveState)
}

func TestReduceOpenCodeGoWorkspaceHealthStaleSnapshotPreservesRiskReason(t *testing.T) {
	baseTime := time.Unix(1_900_000_000, 0)
	workspace, windows := openCodeGoHealthyReducerFixture(baseTime.Unix())
	workspace.EffectiveState = model.OpenCodeGoStateRiskBlocked
	workspace.StateReason = "authoritative fraud block"
	workspace.LastError = workspace.StateReason
	workspace.RiskDetectedAt = baseTime.Unix()
	workspace.RiskLastCheckedAt = baseTime.Unix()
	workspace.HealthObservation = string(OpenCodeGoObservationRiskBlocked)
	workspace.HealthObservedAt = baseTime.UnixNano()
	candidate := workspace
	candidate.LastError = "temporary console timeout"
	candidate.QuotaError = candidate.LastError

	reduced, applied, err := ReduceOpenCodeGoWorkspaceHealth(
		workspace,
		candidate,
		windows,
		OpenCodeGoHealthObservation{
			Kind:       OpenCodeGoObservationStaleSnapshot,
			ObservedAt: baseTime.Add(time.Minute),
			Reason:     candidate.LastError,
		},
	)
	require.NoError(t, err)
	require.True(t, applied)
	assert.Equal(t, model.OpenCodeGoStateRiskBlocked, reduced.EffectiveState)
	assert.Equal(t, workspace.RiskDetectedAt, reduced.RiskDetectedAt)
	assert.Equal(t, workspace.LastError, reduced.LastError)
}

func TestReduceOpenCodeGoHealthFailureWinsSameTimestampRecovery(t *testing.T) {
	baseTime := time.Unix(1_900_000_000, 123)
	workspace, windows := openCodeGoHealthyReducerFixture(baseTime.Unix())
	risk, applied, err := ReduceOpenCodeGoWorkspaceHealth(
		workspace,
		workspace,
		windows,
		OpenCodeGoHealthObservation{Kind: OpenCodeGoObservationRiskBlocked, ObservedAt: baseTime},
	)
	require.NoError(t, err)
	require.True(t, applied)

	unchanged, applied, err := ReduceOpenCodeGoWorkspaceHealth(
		risk,
		risk,
		windows,
		OpenCodeGoHealthObservation{
			Kind:            OpenCodeGoObservationRiskProbeSucceeded,
			ObservedAt:      baseTime,
			HasUsableModels: true,
		},
	)
	require.NoError(t, err)
	assert.False(t, applied)
	assert.Equal(t, model.OpenCodeGoStateRiskBlocked, unchanged.EffectiveState)

	entry := model.OpenCodeGoWorkspaceModel{Model: "glm-5.2", Discovered: true, State: model.OpenCodeGoModelAvailable}
	cooling, applied, err := ReduceOpenCodeGoModelHealth(entry, OpenCodeGoHealthObservation{
		Kind:       OpenCodeGoObservationRPMThrottled,
		ObservedAt: baseTime,
		Deadline:   baseTime.Add(time.Minute),
	})
	require.NoError(t, err)
	require.True(t, applied)
	assert.Equal(t, string(OpenCodeGoObservationRPMThrottled), cooling.HealthObservation)

	unchangedModel, applied, err := ReduceOpenCodeGoModelHealth(cooling, OpenCodeGoHealthObservation{
		Kind:       OpenCodeGoObservationModelProbeSucceeded,
		ObservedAt: baseTime,
	})
	require.NoError(t, err)
	assert.False(t, applied)
	assert.Equal(t, model.OpenCodeGoModelRPMCooldown, unchangedModel.State)
}

func TestReduceOpenCodeGoQuotaObservationUsesNamedWindowReset(t *testing.T) {
	baseTime := time.Unix(1_900_000_000, 0)
	workspace, windows := openCodeGoHealthyReducerFixture(baseTime.Unix())

	reduced, applied, err := ReduceOpenCodeGoWorkspaceHealth(
		workspace,
		workspace,
		windows,
		OpenCodeGoHealthObservation{
			Kind:       OpenCodeGoObservationQuotaExhausted,
			ObservedAt: baseTime,
			QuotaKind:  model.OpenCodeGoQuotaWeekly,
		},
	)
	require.NoError(t, err)
	require.True(t, applied)
	assert.Equal(t, model.OpenCodeGoStateQuotaExhausted, reduced.EffectiveState)
	assert.Equal(t, windows[1].ResetAt, reduced.QuotaRecoveryAt)
}

func TestReduceOpenCodeGoRiskProbeFailureRecordsCheckWithoutRecovery(t *testing.T) {
	baseTime := time.Unix(1_900_000_000, 0)
	workspace, windows := openCodeGoHealthyReducerFixture(baseTime.Unix())
	workspace.EffectiveState = model.OpenCodeGoStateRiskBlocked
	workspace.HealthObservation = string(OpenCodeGoObservationRiskBlocked)
	workspace.HealthObservedAt = baseTime.UnixNano()
	workspace.RiskDetectedAt = baseTime.Unix()

	checkedAt := baseTime.Add(time.Minute)
	reduced, applied, err := ReduceOpenCodeGoWorkspaceHealth(
		workspace,
		workspace,
		windows,
		OpenCodeGoHealthObservation{
			Kind:       OpenCodeGoObservationRiskProbeFailed,
			ObservedAt: checkedAt,
			Reason:     "probe returned a non-success response",
		},
	)
	require.NoError(t, err)
	require.True(t, applied)
	assert.Equal(t, model.OpenCodeGoStateRiskBlocked, reduced.EffectiveState)
	assert.Equal(t, checkedAt.Unix(), reduced.RiskLastCheckedAt)
	assert.Equal(t, string(OpenCodeGoObservationRiskProbeFailed), reduced.HealthObservation)
}
