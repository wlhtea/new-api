package service

import (
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func seedOpenCodeGoHealthWorkspace(
	t *testing.T,
	channelID int,
	identityID int64,
	uid string,
	models ...string,
) model.OpenCodeGoWorkspace {
	t.Helper()
	now := time.Unix(1_900_000_000, 0).Unix()
	workspace := model.OpenCodeGoWorkspace{
		UID:                 uid,
		ChannelID:           channelID,
		IdentityID:          identityID,
		UpstreamWorkspaceID: "wrk_SYNTHETIC_" + uid,
		APIKeyCiphertext:    "encrypted-fixture",
		CredentialStatus:    model.OpenCodeGoCredentialValid,
		MembershipStatus:    model.OpenCodeGoMembershipActive,
		ManualEnabled:       true,
		EffectiveState:      model.OpenCodeGoStateEligible,
		QuotaSnapshotStatus: model.OpenCodeGoQuotaSnapshotComplete,
		QuotaFetchedAt:      now,
	}
	require.NoError(t, model.DB.Create(&workspace).Error)
	for index, kind := range model.OpenCodeGoQuotaKinds {
		require.NoError(t, model.DB.Create(&model.OpenCodeGoQuotaWindow{
			WorkspaceID: workspace.ID,
			Kind:        kind,
			UsedPercent: 10,
			ResetAt:     now + int64((index+1)*3600),
			FetchedAt:   now,
		}).Error)
	}
	for _, modelID := range models {
		require.NoError(t, model.DB.Create(&model.OpenCodeGoWorkspaceModel{
			WorkspaceID: workspace.ID,
			Model:       modelID,
			Discovered:  true,
			State:       model.OpenCodeGoModelAvailable,
		}).Error)
	}
	return workspace
}

func TestObserveOpenCodeGoProviderFailureDoesNotCoolSelectedWorkspaceModel(t *testing.T) {
	_, channel, _ := setupOpenCodeGoPoolTestDB(t)
	identity := model.OpenCodeGoIdentity{
		UID:                   "identity-health-a",
		ChannelID:             channel.Id,
		AuthCookieCiphertext:  "encrypted-cookie-a",
		AuthCookieFingerprint: "fingerprint-health-a",
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, model.DB.Create(&identity).Error)
	selected := seedOpenCodeGoHealthWorkspace(t, channel.Id, identity.ID, "workspace-health-a", "glm-5.2", "kimi-k2.5")
	unrelated := seedOpenCodeGoHealthWorkspace(t, channel.Id, identity.ID, "workspace-health-b", "glm-5.2")

	now := time.Unix(1_900_000_100, 0)
	applied, err := ObserveOpenCodeGoProviderFailure(channel.Id, selected.UID, "glm-5.2", OpenCodeGoProviderFailure{
		StatusCode: 429,
		ErrorType:  "RateLimitError",
		ErrorCode:  "rate_limit",
		Message:    "rate limited",
		RetryAfter: "90",
	}, now)
	require.NoError(t, err)
	assert.False(t, applied)

	selectedAfter, err := model.GetOpenCodeGoWorkspace(channel.Id, selected.UID)
	require.NoError(t, err)
	require.Equal(t, model.OpenCodeGoStateEligible, selectedAfter.EffectiveState)
	states := map[string]string{}
	for _, entry := range selectedAfter.Models {
		states[entry.Model] = entry.State
	}
	assert.Equal(t, model.OpenCodeGoModelAvailable, states["glm-5.2"])
	assert.Equal(t, model.OpenCodeGoModelAvailable, states["kimi-k2.5"])

	unrelatedAfter, err := model.GetOpenCodeGoWorkspace(channel.Id, unrelated.UID)
	require.NoError(t, err)
	require.Len(t, unrelatedAfter.Models, 1)
	assert.Equal(t, model.OpenCodeGoModelAvailable, unrelatedAfter.Models[0].State)
}

func TestObserveOpenCodeGoProviderFailureDoesNotChangeWorkspaceForNonAuthSignals(t *testing.T) {
	_, channel, _ := setupOpenCodeGoPoolTestDB(t)
	identity := model.OpenCodeGoIdentity{
		UID:                   "identity-health-non-auth",
		ChannelID:             channel.Id,
		AuthCookieCiphertext:  "encrypted-cookie-non-auth",
		AuthCookieFingerprint: "fingerprint-health-non-auth",
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, model.DB.Create(&identity).Error)
	workspace := seedOpenCodeGoHealthWorkspace(t, channel.Id, identity.ID, "workspace-health-non-auth", "glm-5.2")
	now := time.Unix(1_900_000_100, 0)
	for _, failure := range []OpenCodeGoProviderFailure{
		{StatusCode: http.StatusBadRequest, ErrorType: "AuthError", Message: "invalid request"},
		{StatusCode: http.StatusUnauthorized, ErrorType: "ModelError", Message: "model unavailable"},
		{StatusCode: http.StatusForbidden, ErrorType: "RegionError", Message: "region unavailable"},
		{StatusCode: http.StatusRequestTimeout, ErrorType: "upstream_error", Message: "request timeout"},
		{StatusCode: http.StatusTooEarly, ErrorType: "upstream_error", Message: "too early"},
		{StatusCode: http.StatusTooManyRequests, ErrorType: "GoUsageLimitError", LimitName: "weekly"},
		{StatusCode: http.StatusInternalServerError, ErrorType: "AuthError", Message: "credential rejected"},
	} {
		applied, err := ObserveOpenCodeGoProviderFailure(channel.Id, workspace.UID, "glm-5.2", failure, now)
		require.NoError(t, err)
		assert.False(t, applied)
		now = now.Add(time.Second)
	}
	transportApplied, err := ObserveOpenCodeGoTransportFailure(channel.Id, workspace.UID, "glm-5.2", "connection reset", now)
	require.NoError(t, err)
	assert.False(t, transportApplied)

	after, err := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, model.OpenCodeGoStateEligible, after.EffectiveState)
	assert.Equal(t, model.OpenCodeGoCredentialValid, after.CredentialStatus)
	require.Len(t, after.Models, 1)
	assert.Equal(t, model.OpenCodeGoModelAvailable, after.Models[0].State)
}

func TestClearLegacyOpenCodeGoModelCooldowns(t *testing.T) {
	_, channel, _ := setupOpenCodeGoPoolTestDB(t)
	identity := model.OpenCodeGoIdentity{
		UID:                   "identity-health-legacy-cooldowns",
		ChannelID:             channel.Id,
		AuthCookieCiphertext:  "encrypted-cookie-legacy-cooldowns",
		AuthCookieFingerprint: "fingerprint-health-legacy-cooldowns",
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, model.DB.Create(&identity).Error)
	workspace := seedOpenCodeGoHealthWorkspace(
		t,
		channel.Id,
		identity.ID,
		"workspace-health-legacy-cooldowns",
		"region-model",
		"model-model",
		"rpm-model",
		"transport-model",
		"undiscovered-model",
	)
	legacy := map[string]struct {
		state       string
		observation OpenCodeGoHealthObservationKind
	}{
		"region-model":    {state: model.OpenCodeGoModelRegionBlocked, observation: OpenCodeGoObservationRegionBlocked},
		"model-model":     {state: model.OpenCodeGoModelDisabled, observation: OpenCodeGoObservationModelBlocked},
		"rpm-model":       {state: model.OpenCodeGoModelRPMCooldown, observation: OpenCodeGoObservationRPMThrottled},
		"transport-model": {state: model.OpenCodeGoModelTransient, observation: OpenCodeGoObservationTransientFailure},
	}
	for modelID, legacyState := range legacy {
		observation := string(legacyState.observation)
		if modelID == "region-model" {
			// Older rows may predate persisted observation metadata.
			observation = ""
		}
		require.NoError(t, model.DB.Model(&model.OpenCodeGoWorkspaceModel{}).
			Where("workspace_id = ? AND model = ?", workspace.ID, modelID).
			Updates(map[string]interface{}{
				"state":              legacyState.state,
				"disabled_until":     int64(2_000_000_000),
				"last_error_code":    "legacy_error",
				"last_error":         "legacy relay failure",
				"health_observation": observation,
				"health_observed_at": int64(1_900_000_000),
			}).Error)
	}
	require.NoError(t, model.DB.Model(&model.OpenCodeGoWorkspaceModel{}).
		Where("workspace_id = ? AND model = ?", workspace.ID, "undiscovered-model").
		Updates(map[string]interface{}{
			"discovered":         false,
			"state":              model.OpenCodeGoModelDisabled,
			"disabled_until":     int64(2_000_000_000),
			"last_error":         "not an error cooldown",
			"health_observation": "",
		}).Error)

	require.NoError(t, clearLegacyOpenCodeGoModelCooldowns())
	after, err := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
	require.NoError(t, err)
	require.NotNil(t, after)
	states := make(map[string]model.OpenCodeGoWorkspaceModel, len(after.Models))
	for _, entry := range after.Models {
		states[entry.Model] = entry
	}
	for modelID := range legacy {
		entry := states[modelID]
		assert.Equal(t, model.OpenCodeGoModelAvailable, entry.State)
		assert.Zero(t, entry.DisabledUntil)
		assert.Empty(t, entry.LastErrorCode)
		assert.Empty(t, entry.LastError)
		assert.Empty(t, entry.HealthObservation)
		assert.Zero(t, entry.HealthObservedAt)
	}
	assert.Equal(t, model.OpenCodeGoModelDisabled, states["undiscovered-model"].State)
	assert.False(t, states["undiscovered-model"].Discovered)
}

func TestApplyOpenCodeGoClassifiedFailureRejectsOlderWorkspaceObservation(t *testing.T) {
	_, channel, _ := setupOpenCodeGoPoolTestDB(t)
	identity := model.OpenCodeGoIdentity{
		UID:                   "identity-health-risk",
		ChannelID:             channel.Id,
		AuthCookieCiphertext:  "encrypted-cookie-risk",
		AuthCookieFingerprint: "fingerprint-health-risk",
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, model.DB.Create(&identity).Error)
	workspace := seedOpenCodeGoHealthWorkspace(t, channel.Id, identity.ID, "workspace-health-risk", "glm-5.2")

	later := time.Unix(1_900_000_200, 0)
	risk, ok := ClassifyOpenCodeGoProviderFailure(OpenCodeGoProviderFailure{
		StatusCode: 401,
		ErrorType:  "AuthError",
		Message:    "This account has found to be committing fraud or is in breach of terms of services and has been blocked.",
	}, later)
	require.True(t, ok)
	rebuilds := 0
	applied, err := applyOpenCodeGoClassifiedFailure(channel.Id, workspace.UID, "glm-5.2", risk, func(int) error {
		rebuilds++
		return nil
	})
	require.NoError(t, err)
	require.True(t, applied)

	older, ok := ClassifyOpenCodeGoProviderFailure(OpenCodeGoProviderFailure{
		StatusCode: 401,
		ErrorType:  "AuthError",
		Message:    "Invalid API key",
	}, later.Add(-time.Second))
	require.True(t, ok)
	applied, err = applyOpenCodeGoClassifiedFailure(channel.Id, workspace.UID, "glm-5.2", older, func(int) error {
		rebuilds++
		return nil
	})
	require.NoError(t, err)
	assert.False(t, applied)
	assert.Equal(t, 1, rebuilds)

	after, err := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
	require.NoError(t, err)
	assert.Equal(t, model.OpenCodeGoStateRiskBlocked, after.EffectiveState)
	assert.Equal(t, model.OpenCodeGoCredentialValid, after.CredentialStatus)
}

func TestApplyOpenCodeGoClassifiedFailureRetriesSQLiteReadWriteContention(t *testing.T) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(1000)",
		filepath.ToSlash(filepath.Join(t.TempDir(), "health-contention.db")),
	)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	require.NoError(t, db.AutoMigrate(
		&model.OpenCodeGoWorkspace{},
		&model.OpenCodeGoQuotaWindow{},
		&model.OpenCodeGoWorkspaceModel{},
	))

	previousDB := model.DB
	previousDatabaseType := common.MainDatabaseType()
	model.DB = db
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	t.Cleanup(func() {
		model.DB = previousDB
		common.SetMainDatabaseType(previousDatabaseType)
		require.NoError(t, sqlDB.Close())
	})

	workspace := model.OpenCodeGoWorkspace{
		UID:                 "workspace-health-contention",
		ChannelID:           771,
		IdentityID:          1,
		UpstreamWorkspaceID: "synthetic-health-contention",
		APIKeyCiphertext:    "encrypted-fixture",
		CredentialStatus:    model.OpenCodeGoCredentialValid,
		MembershipStatus:    model.OpenCodeGoMembershipActive,
		ManualEnabled:       true,
		EffectiveState:      model.OpenCodeGoStateEligible,
		QuotaSnapshotStatus: model.OpenCodeGoQuotaSnapshotComplete,
		QuotaFetchedAt:      time.Unix(1_900_000_000, 0).Unix(),
	}
	require.NoError(t, db.Create(&workspace).Error)
	require.NoError(t, db.Create(&model.OpenCodeGoWorkspaceModel{
		WorkspaceID: workspace.ID,
		Model:       "glm-5.2",
		Discovered:  true,
		State:       model.OpenCodeGoModelAvailable,
	}).Error)

	locker := db.Begin()
	require.NoError(t, locker.Error)
	require.NoError(t, locker.Exec(
		"UPDATE open_code_go_workspaces SET updated_at = updated_at WHERE id = ?",
		workspace.ID,
	).Error)

	releaseLock := make(chan struct{})
	var releaseLockOnce sync.Once
	release := func() { releaseLockOnce.Do(func() { close(releaseLock) }) }
	lockReleased := make(chan struct{})
	go func() {
		<-releaseLock
		_ = locker.Rollback().Error
		close(lockReleased)
	}()
	t.Cleanup(func() {
		select {
		case <-lockReleased:
		default:
			release()
			<-lockReleased
		}
	})
	time.AfterFunc(25*time.Millisecond, release)

	observedAt := time.Unix(1_900_000_100, 0)
	classified, ok := ClassifyOpenCodeGoProviderFailure(OpenCodeGoProviderFailure{
		StatusCode: http.StatusUnauthorized,
		ErrorType:  "AuthError",
		Message:    "This account has found to be committing fraud or is in breach of terms of services and has been blocked.",
	}, observedAt)
	require.True(t, ok)
	rebuilds := 0
	applied, err := applyOpenCodeGoClassifiedFailure(
		workspace.ChannelID,
		workspace.UID,
		"glm-5.2",
		classified,
		func(int) error {
			rebuilds++
			return nil
		},
	)
	require.NoError(t, err)
	require.True(t, applied)
	require.Equal(t, 1, rebuilds)

	var after model.OpenCodeGoWorkspace
	require.NoError(t, db.First(&after, workspace.ID).Error)
	assert.Equal(t, model.OpenCodeGoStateRiskBlocked, after.EffectiveState)
	assert.Equal(t, string(OpenCodeGoObservationRiskBlocked), after.HealthObservation)
	assert.Equal(t, observedAt.UnixNano(), after.HealthObservedAt)

	older, ok := ClassifyOpenCodeGoProviderFailure(OpenCodeGoProviderFailure{
		StatusCode: http.StatusUnauthorized,
		ErrorType:  "AuthError",
		Message:    "Invalid API key",
	}, observedAt.Add(-time.Second))
	require.True(t, ok)
	applied, err = applyOpenCodeGoClassifiedFailure(
		workspace.ChannelID,
		workspace.UID,
		"glm-5.2",
		older,
		func(int) error {
			rebuilds++
			return nil
		},
	)
	require.NoError(t, err)
	assert.False(t, applied)
	assert.Equal(t, 1, rebuilds)
	require.NoError(t, db.First(&after, workspace.ID).Error)
	assert.Equal(t, model.OpenCodeGoStateRiskBlocked, after.EffectiveState)
	assert.Equal(t, observedAt.UnixNano(), after.HealthObservedAt)
}

func TestApplyOpenCodeGoClassifiedFailureRetryRejectsConcurrentNewerObservation(t *testing.T) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(1000)",
		filepath.ToSlash(filepath.Join(t.TempDir(), "health-cas-retry.db")),
	)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	require.NoError(t, db.AutoMigrate(
		&model.OpenCodeGoWorkspace{},
		&model.OpenCodeGoQuotaWindow{},
		&model.OpenCodeGoWorkspaceModel{},
	))
	var journalMode string
	require.NoError(t, db.Raw("PRAGMA journal_mode=WAL").Scan(&journalMode).Error)
	require.Equal(t, "wal", journalMode)

	previousDB := model.DB
	previousDatabaseType := common.MainDatabaseType()
	model.DB = db
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	t.Cleanup(func() {
		model.DB = previousDB
		common.SetMainDatabaseType(previousDatabaseType)
		require.NoError(t, sqlDB.Close())
	})

	workspace := model.OpenCodeGoWorkspace{
		UID:                 "workspace-health-cas-retry",
		ChannelID:           772,
		IdentityID:          1,
		UpstreamWorkspaceID: "synthetic-health-cas-retry",
		APIKeyCiphertext:    "encrypted-fixture",
		CredentialStatus:    model.OpenCodeGoCredentialValid,
		MembershipStatus:    model.OpenCodeGoMembershipActive,
		ManualEnabled:       true,
		EffectiveState:      model.OpenCodeGoStateEligible,
		QuotaSnapshotStatus: model.OpenCodeGoQuotaSnapshotComplete,
		QuotaFetchedAt:      time.Unix(1_900_000_000, 0).Unix(),
	}
	require.NoError(t, db.Create(&workspace).Error)
	require.NoError(t, db.Create(&model.OpenCodeGoWorkspaceModel{
		WorkspaceID: workspace.ID,
		Model:       "glm-5.2",
		Discovered:  true,
		State:       model.OpenCodeGoModelAvailable,
	}).Error)

	firstWorkspaceRead := make(chan struct{})
	continueFirstTransaction := make(chan struct{})
	var continueFirstTransactionOnce sync.Once
	continueTransaction := func() {
		continueFirstTransactionOnce.Do(func() { close(continueFirstTransaction) })
	}
	t.Cleanup(continueTransaction)
	var workspaceReads atomic.Int32
	const callbackName = "test:observe-opencode-go-health-workspace-read"
	require.NoError(t, db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Error != nil || tx.Statement.Table != "open_code_go_workspaces" {
			return
		}
		if workspaceReads.Add(1) == 1 {
			close(firstWorkspaceRead)
			<-continueFirstTransaction
		}
	}))

	olderAt := time.Unix(1_900_000_100, 0)
	classified, ok := ClassifyOpenCodeGoProviderFailure(OpenCodeGoProviderFailure{
		StatusCode: http.StatusUnauthorized,
		ErrorType:  "AuthError",
		Message:    "Invalid API key",
	}, olderAt)
	require.True(t, ok)

	type applyResult struct {
		applied bool
		err     error
	}
	result := make(chan applyResult, 1)
	var rebuilds atomic.Int32
	go func() {
		applied, applyErr := applyOpenCodeGoClassifiedFailure(
			workspace.ChannelID,
			workspace.UID,
			"glm-5.2",
			classified,
			func(int) error {
				rebuilds.Add(1)
				return nil
			},
		)
		result <- applyResult{applied: applied, err: applyErr}
	}()

	select {
	case <-firstWorkspaceRead:
	case <-time.After(time.Second):
		t.Fatal("health transaction did not read the workspace")
	}
	newerAt := olderAt.Add(time.Second)
	require.NoError(t, db.Model(&model.OpenCodeGoWorkspace{}).
		Where("id = ?", workspace.ID).
		Updates(map[string]interface{}{
			"effective_state":      model.OpenCodeGoStateRiskBlocked,
			"state_reason":         "newer concurrent risk observation",
			"health_observation":   string(OpenCodeGoObservationRiskBlocked),
			"health_observed_at":   newerAt.UnixNano(),
			"risk_detected_at":     newerAt.Unix(),
			"risk_last_checked_at": newerAt.Unix(),
			"last_error":           "newer concurrent risk observation",
		}).Error)
	continueTransaction()

	var outcome applyResult
	select {
	case outcome = <-result:
	case <-time.After(time.Second):
		t.Fatal("health transaction retry did not finish")
	}
	require.NoError(t, outcome.err)
	assert.False(t, outcome.applied)
	assert.Equal(t, int32(2), workspaceReads.Load())
	assert.Zero(t, rebuilds.Load())

	var after model.OpenCodeGoWorkspace
	require.NoError(t, db.First(&after, workspace.ID).Error)
	assert.Equal(t, model.OpenCodeGoStateRiskBlocked, after.EffectiveState)
	assert.Equal(t, string(OpenCodeGoObservationRiskBlocked), after.HealthObservation)
	assert.Equal(t, newerAt.UnixNano(), after.HealthObservedAt)
}

func TestRestrictiveOpenCodeGoHealthWriteKeepsRelayBlockedUntilRebuildCompletes(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	workspace := createEligibleOpenCodeGoWorkspace(
		t,
		db,
		codec,
		channel.Id,
		"health-barrier",
		"workspace-health-barrier",
		"wrk_HEALTHBARRIER",
		[]string{"model-a"},
	)
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))
	classified, ok := ClassifyOpenCodeGoProviderFailure(OpenCodeGoProviderFailure{
		StatusCode: http.StatusUnauthorized,
		ErrorType:  "AuthError",
		Message:    "Invalid API key",
	}, time.Unix(1_900_000_100, 0))
	require.True(t, ok)

	rebuildStarted := make(chan struct{})
	allowRebuild := make(chan struct{})
	applyResult := make(chan error, 1)
	go func() {
		_, applyErr := applyOpenCodeGoClassifiedFailure(
			channel.Id,
			workspace.UID,
			"model-a",
			classified,
			func(channelID int) error {
				close(rebuildStarted)
				<-allowRebuild
				return RebuildOpenCodeGoPoolChannel(channelID)
			},
		)
		applyResult <- applyErr
	}()
	<-rebuildStarted

	relayAcquired := make(chan struct{}, 1)
	go func() {
		release := openCodeGoPoolMutations.beginRelay(channel.Id)
		release()
		relayAcquired <- struct{}{}
	}()
	select {
	case <-relayAcquired:
		t.Fatal("relay crossed the health commit-to-rebuild window")
	case <-time.After(50 * time.Millisecond):
	}

	close(allowRebuild)
	require.NoError(t, <-applyResult)
	select {
	case <-relayAcquired:
	case <-time.After(time.Second):
		t.Fatal("relay lease did not resume after health rebuild")
	}
}

func TestRestorativeOpenCodeGoHealthWriteDoesNotAcquireRestrictiveMutationBarrier(t *testing.T) {
	_, channel, _ := setupOpenCodeGoPoolTestDB(t)
	identity := model.OpenCodeGoIdentity{
		UID:                   "identity-health-recovery-no-barrier",
		ChannelID:             channel.Id,
		AuthCookieCiphertext:  "encrypted-cookie-health-recovery-no-barrier",
		AuthCookieFingerprint: "fingerprint-health-recovery-no-barrier",
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, model.DB.Create(&identity).Error)
	workspace := seedOpenCodeGoHealthWorkspace(t, channel.Id, identity.ID, "workspace-health-recovery-no-barrier", "glm-5.2")
	riskAt := time.Unix(1_900_000_100, 0)
	require.NoError(t, model.DB.Model(&model.OpenCodeGoWorkspace{}).
		Where("id = ?", workspace.ID).
		Updates(map[string]interface{}{
			"effective_state":    model.OpenCodeGoStateRiskBlocked,
			"risk_detected_at":   riskAt.Unix(),
			"health_observation": string(OpenCodeGoObservationRiskBlocked),
			"health_observed_at": riskAt.UnixNano(),
		}).Error)

	relayRelease := openCodeGoPoolMutations.beginRelay(channel.Id)
	defer relayRelease()
	result := make(chan error, 1)
	go func() {
		_, applyErr := applyOpenCodeGoClassifiedFailure(
			channel.Id,
			workspace.UID,
			"glm-5.2",
			OpenCodeGoClassifiedFailure{
				Scope: OpenCodeGoHealthScopeWorkspace,
				Observation: OpenCodeGoHealthObservation{
					Kind:            OpenCodeGoObservationRiskProbeSucceeded,
					ObservedAt:      riskAt.Add(time.Minute),
					HasUsableModels: true,
				},
			},
			func(int) error { return nil },
		)
		result <- applyErr
	}()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("restorative health observation waited on a relay lease")
	}
}
