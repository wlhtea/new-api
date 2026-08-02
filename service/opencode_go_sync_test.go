package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type fakeOpenCodeGoConsole struct {
	mutex                  sync.Mutex
	discovered             []OpenCodeGoWorkspacePageResult
	discoveredByCookie     map[string][]OpenCodeGoWorkspacePageResult
	discoverErr            error
	discoverErrorsByCookie map[string]error
	discoverDelay          time.Duration
	discoverCalls          atomic.Int32
	activeDiscoveries      atomic.Int32
	maxActiveDiscoveries   atomic.Int32
	keys                   map[string]string
	keyErrors              map[string]error
	models                 map[string][]string
	modelErrors            map[string]error
}

func (fake *fakeOpenCodeGoConsole) DiscoverWorkspacePages(
	_ context.Context,
	authCookie string,
	_ string,
) ([]OpenCodeGoWorkspacePageResult, error) {
	fake.discoverCalls.Add(1)
	fake.mutex.Lock()
	discovered := fake.discovered
	if value, exists := fake.discoveredByCookie[authCookie]; exists {
		discovered = value
	}
	discoverErr := fake.discoverErr
	if value, exists := fake.discoverErrorsByCookie[authCookie]; exists {
		discoverErr = value
	}
	delay := fake.discoverDelay
	fake.mutex.Unlock()
	active := fake.activeDiscoveries.Add(1)
	for {
		maximum := fake.maxActiveDiscoveries.Load()
		if active <= maximum || fake.maxActiveDiscoveries.CompareAndSwap(maximum, active) {
			break
		}
	}
	defer fake.activeDiscoveries.Add(-1)
	if delay > 0 {
		time.Sleep(delay)
	}
	return append([]OpenCodeGoWorkspacePageResult(nil), discovered...), discoverErr
}

func (fake *fakeOpenCodeGoConsole) FetchAPIKey(_ context.Context, _ string, workspaceID string) (string, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return fake.keys[workspaceID], fake.keyErrors[workspaceID]
}

func (fake *fakeOpenCodeGoConsole) FetchModels(_ context.Context, apiKey string) ([]string, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return append([]string(nil), fake.models[apiKey]...), fake.modelErrors[apiKey]
}

func setupOpenCodeGoPoolTestDB(t *testing.T) (*gorm.DB, *model.Channel, *OpenCodeGoCredentialCodec) {
	t.Helper()
	previousDB := model.DB
	previousMemoryCache := common.MemoryCacheEnabled
	previousSecret := common.CryptoSecret
	previousConfigured := common.CryptoSecretExplicitlyConfigured
	dsn := fmt.Sprintf("file:opencode-go-%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(
		&model.Channel{},
		&model.Ability{},
		&model.OpenCodeGoIdentity{},
		&model.OpenCodeGoWorkspace{},
		&model.OpenCodeGoQuotaWindow{},
		&model.OpenCodeGoWorkspaceModel{},
		&model.OpenCodeGoOperation{},
	))
	model.DB = db
	common.MemoryCacheEnabled = false
	common.CryptoSecret = "test-only-explicit-pool-secret"
	common.CryptoSecretExplicitlyConfigured = true
	openCodeGoPoolChannels = sync.Map{}
	openCodeGoIdentityOperationLocks = sync.Map{}
	t.Cleanup(func() {
		openCodeGoPoolChannels = sync.Map{}
		openCodeGoIdentityOperationLocks = sync.Map{}
		model.DB = previousDB
		common.MemoryCacheEnabled = previousMemoryCache
		common.CryptoSecret = previousSecret
		common.CryptoSecretExplicitlyConfigured = previousConfigured
		_ = sqlDB.Close()
	})

	channel := &model.Channel{
		Type:   constant.ChannelTypeOpenCodeGo,
		Key:    "",
		Name:   "OpenCode Go test pool",
		Status: common.ChannelStatusEnabled,
		Models: "model-a,model-b",
		Group:  "default",
	}
	require.NoError(t, db.Create(channel).Error)
	codec, err := NewOpenCodeGoCredentialCodec(common.CryptoSecret)
	require.NoError(t, err)
	return db, channel, codec
}

func completeOpenCodeGoConsolePage(workspaceID string, usedPercent float64, fetchedAt int64) *OpenCodeGoConsolePage {
	windows := []OpenCodeGoAuthoritativeQuotaWindow{
		{Kind: model.OpenCodeGoQuotaRolling, UsedPercent: usedPercent, ResetSeconds: 3600, ResetAt: fetchedAt + 3600, FetchedAt: fetchedAt},
		{Kind: model.OpenCodeGoQuotaWeekly, UsedPercent: usedPercent + 1, ResetSeconds: 7200, ResetAt: fetchedAt + 7200, FetchedAt: fetchedAt},
		{Kind: model.OpenCodeGoQuotaMonthly, UsedPercent: usedPercent + 2, ResetSeconds: 10800, ResetAt: fetchedAt + 10800, FetchedAt: fetchedAt},
	}
	return &OpenCodeGoConsolePage{
		WorkspaceID:      workspaceID,
		WorkspaceName:    "Synthetic workspace",
		Email:            "operator@example.test",
		MembershipStatus: model.OpenCodeGoMembershipActive,
		Quota: &OpenCodeGoAuthoritativeQuotaSnapshot{
			Windows:       windows,
			FetchedAt:     fetchedAt,
			NextRefreshAt: fetchedAt + 3600,
		},
	}
}

func TestOpenCodeGoImportEncryptsAndRejectsDuplicateCookie(t *testing.T) {
	_, channel, codec := setupOpenCodeGoPoolTestDB(t)
	fetchedAt := int64(1_900_000_000)
	fake := &fakeOpenCodeGoConsole{
		discovered: []OpenCodeGoWorkspacePageResult{{
			Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_ALPHA1", Name: "Synthetic workspace"},
			Page:      completeOpenCodeGoConsolePage("wrk_ALPHA1", 10, fetchedAt),
		}},
		keys:        map[string]string{"wrk_ALPHA1": "sk-synthetic-one"},
		keyErrors:   map[string]error{},
		models:      map[string][]string{"sk-synthetic-one": {"model-a", "model-b"}},
		modelErrors: map[string]error{},
	}
	poolService := newOpenCodeGoAccountPoolService(fake, codec)
	poolService.now = func() time.Time { return time.Unix(fetchedAt, 0) }
	poolService.rebuild = nil

	results, err := poolService.ImportAuthCookies(context.Background(), channel.Id, "Primary", "synthetic-cookie")
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "imported", results[0].Status)
	require.NotEmpty(t, results[0].IdentityUID)
	require.Equal(t, 1, results[0].WorkspaceCount)

	identities, err := model.ListOpenCodeGoIdentities(channel.Id)
	require.NoError(t, err)
	require.Len(t, identities, 1)
	require.NotEqual(t, "synthetic-cookie", identities[0].AuthCookieCiphertext)
	require.NotContains(t, identities[0].AuthCookieCiphertext, "synthetic-cookie")
	require.Len(t, identities[0].Workspaces, 1)
	require.NotEqual(t, "sk-synthetic-one", identities[0].Workspaces[0].APIKeyCiphertext)
	require.Len(t, identities[0].Workspaces[0].QuotaWindows, 3)
	require.Equal(t, model.OpenCodeGoStateEligible, identities[0].Workspaces[0].EffectiveState)

	results, err = poolService.ImportAuthCookies(context.Background(), channel.Id, "Duplicate", "synthetic-cookie")
	require.NoError(t, err)
	require.Equal(t, "error", results[0].Status)
	require.Contains(t, results[0].Error, "already imported")
}

func TestOpenCodeGoBatchImportCreatesTwoIndependentIdentities(t *testing.T) {
	_, channel, codec := setupOpenCodeGoPoolTestDB(t)
	fetchedAt := int64(1_900_000_000)
	fake := &fakeOpenCodeGoConsole{
		discoveredByCookie: map[string][]OpenCodeGoWorkspacePageResult{
			"cookie-one": {{
				Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_ALPHA1"},
				Page:      completeOpenCodeGoConsolePage("wrk_ALPHA1", 10, fetchedAt),
			}},
			"cookie-two": {{
				Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_BETA2"},
				Page:      completeOpenCodeGoConsolePage("wrk_BETA2", 20, fetchedAt),
			}},
		},
		keys: map[string]string{
			"wrk_ALPHA1": "sk-synthetic-one",
			"wrk_BETA2":  "sk-synthetic-two",
		},
		keyErrors: map[string]error{},
		models: map[string][]string{
			"sk-synthetic-one": {"model-a"},
			"sk-synthetic-two": {"model-a"},
		},
		modelErrors: map[string]error{},
	}
	poolService := newOpenCodeGoAccountPoolService(fake, codec)
	poolService.now = func() time.Time { return time.Unix(fetchedAt, 0) }
	poolService.rebuild = nil

	results, err := poolService.ImportAuthCookies(
		context.Background(),
		channel.Id,
		"Imported batch",
		"auth=cookie-one\nauth=cookie-two",
	)
	require.NoError(t, err)
	require.Len(t, results, 2)
	require.Equal(t, "imported", results[0].Status)
	require.Equal(t, "imported", results[1].Status)
	require.NotEqual(t, results[0].IdentityUID, results[1].IdentityUID)

	identities, err := model.ListOpenCodeGoIdentities(channel.Id)
	require.NoError(t, err)
	require.Len(t, identities, 2)
	for _, identity := range identities {
		require.Len(t, identity.Workspaces, 1)
		require.Equal(t, model.OpenCodeGoStateEligible, identity.Workspaces[0].EffectiveState)
	}
}

func TestOpenCodeGoIncompleteRefreshPreservesCompleteQuotaRows(t *testing.T) {
	_, channel, codec := setupOpenCodeGoPoolTestDB(t)
	fetchedAt := int64(1_900_000_000)
	fake := &fakeOpenCodeGoConsole{
		discovered: []OpenCodeGoWorkspacePageResult{{
			Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_ALPHA1"},
			Page:      completeOpenCodeGoConsolePage("wrk_ALPHA1", 10, fetchedAt),
		}},
		keys:        map[string]string{"wrk_ALPHA1": "sk-synthetic-one"},
		keyErrors:   map[string]error{},
		models:      map[string][]string{"sk-synthetic-one": {"model-a"}},
		modelErrors: map[string]error{},
	}
	poolService := newOpenCodeGoAccountPoolService(fake, codec)
	poolService.now = func() time.Time { return time.Unix(fetchedAt, 0) }
	poolService.rebuild = nil
	results, err := poolService.ImportAuthCookies(context.Background(), channel.Id, "", "synthetic-cookie")
	require.NoError(t, err)
	require.Equal(t, "imported", results[0].Status)

	before, err := model.GetOpenCodeGoIdentityPool(channel.Id, results[0].IdentityUID)
	require.NoError(t, err)
	require.Len(t, before.Workspaces[0].QuotaWindows, 3)
	beforeWindows := append([]model.OpenCodeGoQuotaWindow(nil), before.Workspaces[0].QuotaWindows...)
	beforeFetchedAt := before.Workspaces[0].QuotaFetchedAt

	incomplete := completeOpenCodeGoConsolePage("wrk_ALPHA1", 80, fetchedAt+600)
	incomplete.Quota = nil
	incomplete.QuotaParseError = "weekly quota resetInSec is invalid"
	fake.mutex.Lock()
	fake.discovered = []OpenCodeGoWorkspacePageResult{{Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_ALPHA1"}, Page: incomplete}}
	fake.mutex.Unlock()
	poolService.now = func() time.Time { return time.Unix(fetchedAt+600, 0) }

	after, err := poolService.RefreshIdentity(context.Background(), channel.Id, results[0].IdentityUID)
	require.NoError(t, err)
	require.Equal(t, model.OpenCodeGoQuotaSnapshotError, after.Workspaces[0].QuotaSnapshotStatus)
	require.Equal(t, model.OpenCodeGoStateStale, after.Workspaces[0].EffectiveState)
	require.Equal(t, beforeFetchedAt, after.Workspaces[0].QuotaFetchedAt)
	require.Equal(t, beforeWindows, after.Workspaces[0].QuotaWindows)
}

func TestOpenCodeGoAuthenticationFailureMarksIdentityAndWorkspaces(t *testing.T) {
	_, channel, codec := setupOpenCodeGoPoolTestDB(t)
	fetchedAt := int64(1_900_000_000)
	fake := &fakeOpenCodeGoConsole{
		discovered: []OpenCodeGoWorkspacePageResult{{
			Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_ALPHA1"},
			Page:      completeOpenCodeGoConsolePage("wrk_ALPHA1", 10, fetchedAt),
		}},
		keys:        map[string]string{"wrk_ALPHA1": "sk-synthetic-one"},
		keyErrors:   map[string]error{},
		models:      map[string][]string{"sk-synthetic-one": {"model-a"}},
		modelErrors: map[string]error{},
	}
	poolService := newOpenCodeGoAccountPoolService(fake, codec)
	poolService.now = func() time.Time { return time.Unix(fetchedAt, 0) }
	poolService.rebuild = nil
	results, err := poolService.ImportAuthCookies(context.Background(), channel.Id, "", "synthetic-cookie")
	require.NoError(t, err)

	fake.mutex.Lock()
	fake.discoverErr = ErrOpenCodeGoAuthenticationInvalid
	fake.mutex.Unlock()
	_, err = poolService.RefreshIdentity(context.Background(), channel.Id, results[0].IdentityUID)
	require.ErrorIs(t, err, ErrOpenCodeGoAuthenticationInvalid)

	identity, err := model.GetOpenCodeGoIdentityPool(channel.Id, results[0].IdentityUID)
	require.NoError(t, err)
	require.Equal(t, model.OpenCodeGoIdentityStatusAuthError, identity.Status)
	require.Len(t, identity.Workspaces, 1)
	require.Equal(t, model.OpenCodeGoStateAuthError, identity.Workspaces[0].EffectiveState)
	require.Equal(t, model.OpenCodeGoQuotaSnapshotStale, identity.Workspaces[0].QuotaSnapshotStatus)
	require.Len(t, identity.Workspaces[0].QuotaWindows, 3)
}

func TestReplaceOpenCodeGoIdentityCookieCommitsOnlyAfterSuccessfulDiscovery(t *testing.T) {
	_, channel, codec := setupOpenCodeGoPoolTestDB(t)
	fetchedAt := int64(1_900_000_000)
	fake := &fakeOpenCodeGoConsole{
		discovered: []OpenCodeGoWorkspacePageResult{{
			Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_ALPHA1"},
			Page:      completeOpenCodeGoConsolePage("wrk_ALPHA1", 10, fetchedAt),
		}},
		discoveredByCookie: map[string][]OpenCodeGoWorkspacePageResult{
			"replacement-cookie": {{
				Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_ALPHA1"},
				Page:      completeOpenCodeGoConsolePage("wrk_ALPHA1", 20, fetchedAt+60),
			}},
		},
		discoverErrorsByCookie: map[string]error{
			"invalid-replacement": ErrOpenCodeGoAuthenticationInvalid,
		},
		keys:        map[string]string{"wrk_ALPHA1": "sk-synthetic-one"},
		keyErrors:   map[string]error{},
		models:      map[string][]string{"sk-synthetic-one": {"model-a"}},
		modelErrors: map[string]error{},
	}
	poolService := newOpenCodeGoAccountPoolService(fake, codec)
	poolService.now = func() time.Time { return time.Unix(fetchedAt, 0) }
	poolService.rebuild = nil
	results, err := poolService.ImportAuthCookies(context.Background(), channel.Id, "", "original-cookie")
	require.NoError(t, err)
	identityUID := results[0].IdentityUID

	before, err := model.GetOpenCodeGoIdentityPool(channel.Id, identityUID)
	require.NoError(t, err)
	_, err = poolService.ReplaceIdentityAuthCookie(
		context.Background(),
		channel.Id,
		identityUID,
		"invalid-replacement",
	)
	require.ErrorIs(t, err, ErrOpenCodeGoAuthenticationInvalid)
	afterFailure, err := model.GetOpenCodeGoIdentityPool(channel.Id, identityUID)
	require.NoError(t, err)
	require.Equal(t, before.AuthCookieCiphertext, afterFailure.AuthCookieCiphertext)
	require.Equal(t, before.AuthCookieFingerprint, afterFailure.AuthCookieFingerprint)
	require.Equal(t, model.OpenCodeGoIdentityStatusActive, afterFailure.Status)
	require.Equal(t, model.OpenCodeGoStateEligible, afterFailure.Workspaces[0].EffectiveState)

	poolService.now = func() time.Time { return time.Unix(fetchedAt+60, 0) }
	afterSuccess, err := poolService.ReplaceIdentityAuthCookie(
		context.Background(),
		channel.Id,
		identityUID,
		"auth=replacement-cookie",
	)
	require.NoError(t, err)
	plaintext, err := codec.Decrypt(
		OpenCodeGoCredentialAuthCookie,
		channel.Id,
		identityUID,
		afterSuccess.AuthCookieCiphertext,
	)
	require.NoError(t, err)
	require.Equal(t, "replacement-cookie", plaintext)
	require.NotEqual(t, before.AuthCookieFingerprint, afterSuccess.AuthCookieFingerprint)
	require.Equal(t, fetchedAt+60, afterSuccess.Workspaces[0].QuotaFetchedAt)
}

func TestOpenCodeGoRefreshTargetsUsesBoundedConcurrentWorkers(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	fetchedAt := int64(1_900_000_000)
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "two", "workspace-two", "wrk_BETA2", []string{"model-a"})
	fake := &fakeOpenCodeGoConsole{
		discoveredByCookie: map[string][]OpenCodeGoWorkspacePageResult{
			"cookie-one": {{
				Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_ALPHA1"},
				Page:      completeOpenCodeGoConsolePage("wrk_ALPHA1", 11, fetchedAt+60),
			}},
			"cookie-two": {{
				Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_BETA2"},
				Page:      completeOpenCodeGoConsolePage("wrk_BETA2", 12, fetchedAt+60),
			}},
		},
		discoverDelay: 25 * time.Millisecond,
		keys: map[string]string{
			"wrk_ALPHA1": "sk-synthetic-one",
			"wrk_BETA2":  "sk-synthetic-two",
		},
		keyErrors: map[string]error{},
		models: map[string][]string{
			"sk-synthetic-one": {"model-a"},
			"sk-synthetic-two": {"model-a"},
		},
		modelErrors: map[string]error{},
	}
	poolService := newOpenCodeGoAccountPoolService(fake, codec)
	poolService.now = func() time.Time { return time.Unix(fetchedAt+60, 0) }
	poolService.rebuild = nil
	progress := make([][2]int, 0)
	summary, err := poolService.RefreshIdentityTargets(
		context.Background(),
		[]model.OpenCodeGoRefreshTarget{
			{ChannelID: channel.Id, IdentityUID: "identity-one"},
			{ChannelID: channel.Id, IdentityUID: "identity-two"},
			{ChannelID: channel.Id, IdentityUID: "identity-one"},
		},
		2,
		func(processed, total int) { progress = append(progress, [2]int{processed, total}) },
	)
	require.NoError(t, err)
	require.Equal(t, 2, summary.Total)
	require.Equal(t, 2, summary.Succeeded)
	require.Zero(t, summary.Failed)
	require.Equal(t, int32(2), fake.maxActiveDiscoveries.Load())
	require.Equal(t, [2]int{0, 2}, progress[0])
	require.Equal(t, [2]int{2, 2}, progress[len(progress)-1])
	require.Equal(t, "identity-one", summary.Results[0].IdentityUID)
	require.Equal(t, "identity-two", summary.Results[1].IdentityUID)
}

func TestRefreshOpenCodeGoWorkspaceRefreshesItsOwningIdentityOnce(t *testing.T) {
	_, channel, codec := setupOpenCodeGoPoolTestDB(t)
	fetchedAt := int64(1_900_000_000)
	fake := &fakeOpenCodeGoConsole{
		discovered: []OpenCodeGoWorkspacePageResult{
			{
				Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_ALPHA1"},
				Page:      completeOpenCodeGoConsolePage("wrk_ALPHA1", 10, fetchedAt),
			},
			{
				Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_BETA2"},
				Page:      completeOpenCodeGoConsolePage("wrk_BETA2", 20, fetchedAt),
			},
		},
		keys: map[string]string{
			"wrk_ALPHA1": "sk-synthetic-one",
			"wrk_BETA2":  "sk-synthetic-two",
		},
		keyErrors: map[string]error{},
		models: map[string][]string{
			"sk-synthetic-one": {"model-a"},
			"sk-synthetic-two": {"model-a"},
		},
		modelErrors: map[string]error{},
	}
	poolService := newOpenCodeGoAccountPoolService(fake, codec)
	poolService.now = func() time.Time { return time.Unix(fetchedAt, 0) }
	poolService.rebuild = nil
	results, err := poolService.ImportAuthCookies(context.Background(), channel.Id, "", "synthetic-cookie")
	require.NoError(t, err)
	identity, err := model.GetOpenCodeGoIdentityPool(channel.Id, results[0].IdentityUID)
	require.NoError(t, err)
	require.Len(t, identity.Workspaces, 2)
	fake.discoverCalls.Store(0)

	fake.mutex.Lock()
	fake.discovered = []OpenCodeGoWorkspacePageResult{
		{
			Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_ALPHA1"},
			Page:      completeOpenCodeGoConsolePage("wrk_ALPHA1", 40, fetchedAt+60),
		},
		{
			Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_BETA2"},
			Page:      completeOpenCodeGoConsolePage("wrk_BETA2", 50, fetchedAt+60),
		},
	}
	fake.mutex.Unlock()
	poolService.now = func() time.Time { return time.Unix(fetchedAt+60, 0) }

	refreshed, err := poolService.RefreshWorkspace(context.Background(), channel.Id, identity.Workspaces[0].UID)
	require.NoError(t, err)
	require.Equal(t, int32(1), fake.discoverCalls.Load())
	require.Equal(t, fetchedAt+60, refreshed.QuotaFetchedAt)

	after, err := model.GetOpenCodeGoIdentityPool(channel.Id, identity.UID)
	require.NoError(t, err)
	require.Len(t, after.Workspaces, 2)
	for _, workspace := range after.Workspaces {
		require.Equal(t, fetchedAt+60, workspace.QuotaFetchedAt)
	}
}

func TestListOpenCodeGoDueRefreshTargetsUsesResetNodesAndStaleness(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	now := int64(1_900_010_000)
	first := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})
	second := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "two", "workspace-two", "wrk_BETA2", []string{"model-a"})
	require.NoError(t, db.Model(&model.OpenCodeGoWorkspace{}).Where("id = ?", first.ID).Updates(map[string]interface{}{
		"quota_next_refresh_at": now - 1,
		"last_synced_at":        now,
	}).Error)
	require.NoError(t, db.Model(&model.OpenCodeGoWorkspace{}).Where("id = ?", second.ID).Updates(map[string]interface{}{
		"quota_next_refresh_at": now + 3600,
		"last_synced_at":        now,
	}).Error)

	targets, err := model.ListOpenCodeGoDueRefreshTargets(now, now-900, 100)
	require.NoError(t, err)
	require.Equal(t, []model.OpenCodeGoRefreshTarget{{
		ChannelID:   channel.Id,
		IdentityUID: "identity-one",
	}}, targets)

	require.NoError(t, db.Model(&model.OpenCodeGoWorkspace{}).Where("id = ?", second.ID).Update("last_synced_at", now-901).Error)
	targets, err = model.ListOpenCodeGoDueRefreshTargets(now, now-900, 100)
	require.NoError(t, err)
	require.Len(t, targets, 2)
}

func TestOpenCodeGoQuotaReplacementRollsBackAtomically(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	fetchedAt := int64(1_900_000_000)
	fake := &fakeOpenCodeGoConsole{
		discovered: []OpenCodeGoWorkspacePageResult{{
			Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_ALPHA1"},
			Page:      completeOpenCodeGoConsolePage("wrk_ALPHA1", 10, fetchedAt),
		}},
		keys:        map[string]string{"wrk_ALPHA1": "sk-synthetic-one"},
		keyErrors:   map[string]error{},
		models:      map[string][]string{"sk-synthetic-one": {"model-a"}},
		modelErrors: map[string]error{},
	}
	poolService := newOpenCodeGoAccountPoolService(fake, codec)
	poolService.now = func() time.Time { return time.Unix(fetchedAt, 0) }
	poolService.rebuild = nil
	results, err := poolService.ImportAuthCookies(context.Background(), channel.Id, "", "synthetic-cookie")
	require.NoError(t, err)
	identityUID := results[0].IdentityUID
	before, err := model.GetOpenCodeGoIdentityPool(channel.Id, identityUID)
	require.NoError(t, err)
	beforeWindows := append([]model.OpenCodeGoQuotaWindow(nil), before.Workspaces[0].QuotaWindows...)

	callbackName := "opencode_go_test_fail_quota_create"
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "open_code_go_quota_windows" {
			tx.AddError(errors.New("injected quota insert failure"))
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Create().Remove(callbackName) })
	fake.mutex.Lock()
	fake.discovered = []OpenCodeGoWorkspacePageResult{{
		Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_ALPHA1"},
		Page:      completeOpenCodeGoConsolePage("wrk_ALPHA1", 70, fetchedAt+600),
	}}
	fake.mutex.Unlock()
	poolService.now = func() time.Time { return time.Unix(fetchedAt+600, 0) }

	_, err = poolService.RefreshIdentity(context.Background(), channel.Id, identityUID)
	require.ErrorContains(t, err, "injected quota insert failure")
	after, err := model.GetOpenCodeGoIdentityPool(channel.Id, identityUID)
	require.NoError(t, err)
	require.Equal(t, before.Workspaces[0].QuotaFetchedAt, after.Workspaces[0].QuotaFetchedAt)
	require.Equal(t, beforeWindows, after.Workspaces[0].QuotaWindows)
}

func createEligibleOpenCodeGoWorkspace(
	t *testing.T,
	db *gorm.DB,
	codec *OpenCodeGoCredentialCodec,
	channelID int,
	identitySuffix string,
	workspaceUID string,
	upstreamWorkspaceID string,
	models []string,
) model.OpenCodeGoWorkspace {
	t.Helper()
	identityUID := "identity-" + identitySuffix
	cookieCiphertext, err := codec.Encrypt(OpenCodeGoCredentialAuthCookie, channelID, identityUID, "cookie-"+identitySuffix)
	require.NoError(t, err)
	identity := model.OpenCodeGoIdentity{
		UID:                   identityUID,
		ChannelID:             channelID,
		AuthCookieCiphertext:  cookieCiphertext,
		AuthCookieFingerprint: fmt.Sprintf("%064s", identitySuffix),
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, db.Create(&identity).Error)
	apiKey := "sk-synthetic-" + identitySuffix
	apiKeyCiphertext, err := codec.Encrypt(OpenCodeGoCredentialAPIKey, channelID, workspaceUID, apiKey)
	require.NoError(t, err)
	workspace := model.OpenCodeGoWorkspace{
		UID:                 workspaceUID,
		ChannelID:           channelID,
		IdentityID:          identity.ID,
		UpstreamWorkspaceID: upstreamWorkspaceID,
		APIKeyCiphertext:    apiKeyCiphertext,
		APIKeyFingerprint:   fmt.Sprintf("%064s", "key-"+identitySuffix),
		CredentialStatus:    model.OpenCodeGoCredentialValid,
		MembershipStatus:    model.OpenCodeGoMembershipActive,
		ManualEnabled:       true,
		EffectiveState:      model.OpenCodeGoStateEligible,
		QuotaSnapshotStatus: model.OpenCodeGoQuotaSnapshotComplete,
		QuotaFetchedAt:      1_900_000_000,
		QuotaNextRefreshAt:  1_900_003_600,
		QuotaParserVersion:  OpenCodeGoSSRParserVersion,
	}
	require.NoError(t, db.Create(&workspace).Error)
	for index, kind := range model.OpenCodeGoQuotaKinds {
		require.NoError(t, db.Create(&model.OpenCodeGoQuotaWindow{
			WorkspaceID:  workspace.ID,
			Kind:         kind,
			UsedPercent:  float64(10 + index),
			ResetSeconds: int64((index + 1) * 3600),
			ResetAt:      1_900_000_000 + int64((index+1)*3600),
			FetchedAt:    1_900_000_000,
		}).Error)
	}
	for _, modelID := range models {
		require.NoError(t, db.Create(&model.OpenCodeGoWorkspaceModel{
			WorkspaceID: workspace.ID,
			Model:       modelID,
			Discovered:  true,
			State:       model.OpenCodeGoModelAvailable,
		}).Error)
	}
	return workspace
}

func TestOpenCodeGoPoolSelectionFairnessFilteringAndZeroQueries(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	first := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a", "model-b"})
	second := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "two", "workspace-two", "wrk_BETA2", []string{"model-a"})
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))
	stateValue, found := openCodeGoPoolChannels.Load(channel.Id)
	require.True(t, found)
	snapshot := stateValue.(*openCodeGoPoolChannelState).snapshot.Load()
	require.NotNil(t, snapshot)
	require.NotContains(t, snapshot.byModel["model-a"][0].apiKeyCiphertext, "sk-synthetic-")

	selected := make([]string, 0, 6)
	for index := 0; index < 6; index++ {
		selection, err := SelectOpenCodeGoWorkspace(channel.Id, "model-a")
		require.NoError(t, err)
		selected = append(selected, selection.WorkspaceUID)
	}
	require.Equal(t, []string{
		first.UID, second.UID,
		first.UID, second.UID,
		first.UID, second.UID,
	}, selected)

	selection, err := SelectOpenCodeGoWorkspace(channel.Id, "model-b")
	require.NoError(t, err)
	require.Equal(t, first.UID, selection.WorkspaceUID)
	require.Equal(t, "sk-synthetic-one", selection.APIKey)
	_, err = SelectOpenCodeGoWorkspace(channel.Id, "model-missing")
	require.ErrorIs(t, err, ErrOpenCodeGoNoEligibleWorkspace)

	var queryCount atomic.Int64
	callbackName := "opencode_go_test_count_selector_queries"
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callbackName, func(_ *gorm.DB) {
		queryCount.Add(1)
	}))
	for index := 0; index < 50; index++ {
		_, err := SelectOpenCodeGoWorkspace(channel.Id, "model-a")
		require.NoError(t, err)
	}
	require.Zero(t, queryCount.Load())
	require.NoError(t, db.Callback().Query().Remove(callbackName))
}

func TestInitOpenCodeGoPoolsReconcilesAnEmptyLegacyChannel(t *testing.T) {
	db, channel, _ := setupOpenCodeGoPoolTestDB(t)
	require.NoError(t, channel.AddAbilities(nil))

	InitOpenCodeGoPools()

	reloaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	require.Empty(t, reloaded.Models)
	require.Equal(t, common.ChannelStatusAutoDisabled, reloaded.Status)
	require.Equal(t, openCodeGoNoEligibleWorkspaceReason, reloaded.GetOtherInfo()["status_reason"])
	var abilityCount int64
	require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", channel.Id).Count(&abilityCount).Error)
	require.Zero(t, abilityCount)
	_, err = SelectOpenCodeGoWorkspace(channel.Id, "model-a")
	require.ErrorIs(t, err, ErrOpenCodeGoNoEligibleWorkspace)
}

func TestOpenCodeGoPoolConcurrentCursorIsBalanced(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	first := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})
	second := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "two", "workspace-two", "wrk_BETA2", []string{"model-a"})
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))

	counts := sync.Map{}
	errorsFound := make(chan error, 100)
	var wait sync.WaitGroup
	for index := 0; index < 100; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			selection, err := SelectOpenCodeGoWorkspace(channel.Id, "model-a")
			if err != nil {
				errorsFound <- err
				return
			}
			value, _ := counts.LoadOrStore(selection.WorkspaceUID, &atomic.Int64{})
			value.(*atomic.Int64).Add(1)
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		require.NoError(t, err)
	}
	firstCount, _ := counts.Load(first.UID)
	secondCount, _ := counts.Load(second.UID)
	require.Equal(t, int64(50), firstCount.(*atomic.Int64).Load())
	require.Equal(t, int64(50), secondCount.(*atomic.Int64).Load())
}

func TestOpenCodeGoWorkspaceEligibilityRejectsEverySchedulingBlocker(t *testing.T) {
	now := int64(1_900_000_000)
	base := model.OpenCodeGoWorkspace{
		APIKeyCiphertext:    "ciphertext",
		CredentialStatus:    model.OpenCodeGoCredentialValid,
		MembershipStatus:    model.OpenCodeGoMembershipActive,
		ManualEnabled:       true,
		EffectiveState:      model.OpenCodeGoStateEligible,
		QuotaSnapshotStatus: model.OpenCodeGoQuotaSnapshotComplete,
		QuotaFetchedAt:      now,
		QuotaWindows: []model.OpenCodeGoQuotaWindow{
			{Kind: model.OpenCodeGoQuotaRolling, UsedPercent: 10, FetchedAt: now},
			{Kind: model.OpenCodeGoQuotaWeekly, UsedPercent: 20, FetchedAt: now},
			{Kind: model.OpenCodeGoQuotaMonthly, UsedPercent: 30, FetchedAt: now},
		},
	}
	require.True(t, isOpenCodeGoWorkspaceEligibleForSnapshot(base, now))

	tests := []struct {
		name   string
		mutate func(*model.OpenCodeGoWorkspace)
	}{
		{name: "manual disabled", mutate: func(value *model.OpenCodeGoWorkspace) { value.ManualEnabled = false }},
		{name: "membership inactive", mutate: func(value *model.OpenCodeGoWorkspace) { value.MembershipStatus = model.OpenCodeGoMembershipInactive }},
		{name: "subscription expired", mutate: func(value *model.OpenCodeGoWorkspace) { value.SubscriptionEndsAt = now }},
		{name: "credential invalid", mutate: func(value *model.OpenCodeGoWorkspace) { value.CredentialStatus = model.OpenCodeGoCredentialError }},
		{name: "effective state stale", mutate: func(value *model.OpenCodeGoWorkspace) { value.EffectiveState = model.OpenCodeGoStateStale }},
		{name: "effective state quota exhausted", mutate: func(value *model.OpenCodeGoWorkspace) { value.EffectiveState = model.OpenCodeGoStateQuotaExhausted }},
		{name: "effective state risk blocked", mutate: func(value *model.OpenCodeGoWorkspace) { value.EffectiveState = model.OpenCodeGoStateRiskBlocked }},
		{name: "effective state cooldown", mutate: func(value *model.OpenCodeGoWorkspace) { value.EffectiveState = model.OpenCodeGoStateCooldown }},
		{name: "quota stale", mutate: func(value *model.OpenCodeGoWorkspace) { value.QuotaSnapshotStatus = model.OpenCodeGoQuotaSnapshotStale }},
		{name: "quota never fetched", mutate: func(value *model.OpenCodeGoWorkspace) { value.QuotaFetchedAt = 0 }},
		{name: "cooldown active", mutate: func(value *model.OpenCodeGoWorkspace) { value.CooldownUntil = now + 60 }},
		{name: "credential absent", mutate: func(value *model.OpenCodeGoWorkspace) { value.APIKeyCiphertext = "" }},
		{name: "quota exhausted", mutate: func(value *model.OpenCodeGoWorkspace) { value.QuotaWindows[0].UsedPercent = 100 }},
		{name: "quota negative", mutate: func(value *model.OpenCodeGoWorkspace) { value.QuotaWindows[0].UsedPercent = -1 }},
		{name: "quota from another snapshot", mutate: func(value *model.OpenCodeGoWorkspace) { value.QuotaWindows[0].FetchedAt = now - 1 }},
		{name: "quota kind missing", mutate: func(value *model.OpenCodeGoWorkspace) { value.QuotaWindows = value.QuotaWindows[:2] }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := base
			value.QuotaWindows = append([]model.OpenCodeGoQuotaWindow(nil), base.QuotaWindows...)
			test.mutate(&value)
			require.False(t, isOpenCodeGoWorkspaceEligibleForSnapshot(value, now))
		})
	}
}

func TestSyncOpenCodeGoChannelModelsRemovesUnavailableModelsAndKeepsAvailableAliases(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	mapping := `{"public-a":"model-a","public-missing":"model-missing"}`
	channel.ModelMapping = &mapping
	channel.Models = "model-a,model-b,old-model,public-a,public-missing"
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channel.Id).Updates(map[string]interface{}{
		"models":        channel.Models,
		"model_mapping": mapping,
	}).Error)
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})

	require.NoError(t, syncOpenCodeGoChannelModels(channel.Id))
	reloaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	require.Equal(t, "model-a,public-a", reloaded.Models)

	var abilities []model.Ability
	require.NoError(t, db.Where("channel_id = ?", channel.Id).Order("model asc").Find(&abilities).Error)
	require.Len(t, abilities, 2)
	require.Equal(t, "model-a", abilities[0].Model)
	require.Equal(t, "public-a", abilities[1].Model)
}
