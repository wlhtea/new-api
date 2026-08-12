package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
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
	discoverStarted        chan<- struct{}
	discoverRelease        <-chan struct{}
	keys                   map[string]string
	keyErrors              map[string]error
	models                 map[string][]string
	modelErrors            map[string]error
}

func (fake *fakeOpenCodeGoConsole) DiscoverWorkspacePages(
	ctx context.Context,
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
	discoverStarted := fake.discoverStarted
	discoverRelease := fake.discoverRelease
	fake.mutex.Unlock()
	active := fake.activeDiscoveries.Add(1)
	for {
		maximum := fake.maxActiveDiscoveries.Load()
		if active <= maximum || fake.maxActiveDiscoveries.CompareAndSwap(maximum, active) {
			break
		}
	}
	defer fake.activeDiscoveries.Add(-1)
	if discoverStarted != nil {
		select {
		case discoverStarted <- struct{}{}:
		default:
		}
	}
	if discoverRelease != nil {
		select {
		case <-discoverRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
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

func TestOpenCodeGoImportAndCookieReplacementKeepIdentityProxyBinding(t *testing.T) {
	_, channel, codec := setupOpenCodeGoPoolTestDB(t)
	previousCache := openCodeGoIdentityProxyClients
	openCodeGoIdentityProxyClients = newOpenCodeGoIdentityProxyClientCache(8, time.Hour)
	t.Cleanup(func() {
		openCodeGoIdentityProxyClients.reset()
		openCodeGoIdentityProxyClients = previousCache
	})
	fetchedAt := int64(1_900_000_000)
	fake := &fakeOpenCodeGoConsole{
		discoveredByCookie: map[string][]OpenCodeGoWorkspacePageResult{
			"original-cookie": {{
				Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_PROXY1"},
				Page:      completeOpenCodeGoConsolePage("wrk_PROXY1", 10, fetchedAt),
			}},
			"replacement-cookie": {{
				Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_PROXY1"},
				Page:      completeOpenCodeGoConsolePage("wrk_PROXY1", 20, fetchedAt+60),
			}},
		},
		keys:        map[string]string{"wrk_PROXY1": "sk-synthetic-proxy"},
		keyErrors:   map[string]error{},
		models:      map[string][]string{"sk-synthetic-proxy": {"model-a"}},
		modelErrors: map[string]error{},
	}
	var factoryMutex sync.Mutex
	identityUIDs := make([]string, 0, 2)
	poolService := &OpenCodeGoAccountPoolService{
		consoleFactory: func(gotChannelID int, identityUID string) (openCodeGoConsoleReader, error) {
			assert.Equal(t, channel.Id, gotChannelID)
			assert.NotEmpty(t, identityUID, "import must allocate the identity before its first upstream request")
			factoryMutex.Lock()
			identityUIDs = append(identityUIDs, identityUID)
			factoryMutex.Unlock()
			return fake, nil
		},
		codec:   codec,
		now:     func() time.Time { return time.Unix(fetchedAt, 0) },
		rebuild: RebuildOpenCodeGoPoolChannel,
	}

	results, err := poolService.ImportAuthCookies(context.Background(), channel.Id, "", "original-cookie")
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "imported", results[0].Status)
	identityUID := results[0].IdentityUID
	require.NotEmpty(t, identityUID)
	selectionBefore, err := SelectOpenCodeGoWorkspace(channel.Id, "model-a")
	require.NoError(t, err)

	proxySettings := dto.ChannelSettings{
		Proxy: "http://test_custom_zone_US_sid_1_time_10:secret@proxy.example:8080",
	}
	proxyConfig := &dto.OpenCodeGoConfig{
		IdentityProxyEnabled:       true,
		IdentityProxyCountry:       "US",
		IdentityProxyRotateMinutes: 10,
	}
	proxyTime := time.Unix(fetchedAt+1, 0)
	identityClientBefore, err := resolveOpenCodeGoIdentityHTTPClient(
		channel.Id,
		identityUID,
		proxySettings,
		proxyConfig,
		proxyTime,
	)
	require.NoError(t, err)
	unrelatedClientBefore, err := resolveOpenCodeGoIdentityHTTPClient(
		channel.Id,
		"unrelated-identity",
		proxySettings,
		proxyConfig,
		proxyTime,
	)
	require.NoError(t, err)

	poolService.now = func() time.Time { return time.Unix(fetchedAt+60, 0) }
	_, err = poolService.ReplaceIdentityAuthCookie(
		context.Background(),
		channel.Id,
		identityUID,
		"replacement-cookie",
	)
	require.NoError(t, err)
	require.Equal(t, []string{identityUID, identityUID}, identityUIDs)
	selectionAfter, err := SelectOpenCodeGoWorkspace(channel.Id, "model-a")
	require.NoError(t, err)
	assert.NotEqual(t, selectionBefore.IdentityProxyGeneration, selectionAfter.IdentityProxyGeneration)
	_, err = resolveOpenCodeGoIdentityHTTPClientWithGeneration(
		channel.Id,
		identityUID,
		proxySettings,
		proxyConfig,
		proxyTime,
		&selectionBefore.IdentityProxyGeneration,
	)
	require.ErrorIs(t, err, ErrOpenCodeGoIdentityProxySelectionStale)

	identityClientAfter, err := resolveOpenCodeGoIdentityHTTPClientWithGeneration(
		channel.Id,
		identityUID,
		proxySettings,
		proxyConfig,
		proxyTime,
		&selectionAfter.IdentityProxyGeneration,
	)
	require.NoError(t, err)
	unrelatedClientAfter, err := resolveOpenCodeGoIdentityHTTPClientWithGeneration(
		channel.Id,
		"unrelated-identity",
		proxySettings,
		proxyConfig,
		proxyTime,
		&selectionAfter.IdentityProxyGeneration,
	)
	require.NoError(t, err)
	assert.Same(t, identityClientBefore, identityClientAfter)
	assert.Same(t, unrelatedClientBefore, unrelatedClientAfter)

	const bucket = int64(1234)
	assert.Equal(
		t,
		deriveOpenCodeGoIdentityProxySID(common.CryptoSecret, channel.Id, identityUIDs[0], bucket),
		deriveOpenCodeGoIdentityProxySID(common.CryptoSecret, channel.Id, identityUIDs[1], bucket),
		"replacing a Cookie must retain the current-bucket proxy SID",
	)
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

func TestOpenCodeGoRefreshKeepsRelayBlockedUntilSnapshotRebuildCompletes(t *testing.T) {
	_, channel, codec := setupOpenCodeGoPoolTestDB(t)
	fetchedAt := int64(1_900_000_000)
	fake := &fakeOpenCodeGoConsole{
		discovered: []OpenCodeGoWorkspacePageResult{{
			Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_BARRIER"},
			Page:      completeOpenCodeGoConsolePage("wrk_BARRIER", 10, fetchedAt),
		}},
		keys:        map[string]string{"wrk_BARRIER": "sk-barrier-before"},
		keyErrors:   map[string]error{},
		models:      map[string][]string{"sk-barrier-before": {"model-a"}, "sk-barrier-after": {"model-a"}},
		modelErrors: map[string]error{},
	}
	poolService := newOpenCodeGoAccountPoolService(fake, codec)
	poolService.now = func() time.Time { return time.Unix(fetchedAt, 0) }
	poolService.rebuild = nil
	results, err := poolService.ImportAuthCookies(context.Background(), channel.Id, "", "barrier-cookie")
	require.NoError(t, err)
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))
	_, err = SelectOpenCodeGoWorkspace(channel.Id, "model-a")
	require.NoError(t, err)

	fake.mutex.Lock()
	fake.keys["wrk_BARRIER"] = "sk-barrier-after"
	fake.discovered[0].Page = completeOpenCodeGoConsolePage("wrk_BARRIER", 20, fetchedAt+60)
	fake.mutex.Unlock()
	poolService.now = func() time.Time { return time.Unix(fetchedAt+60, 0) }
	rebuildStarted := make(chan struct{})
	allowRebuild := make(chan struct{})
	poolService.rebuild = func(channelID int) error {
		close(rebuildStarted)
		<-allowRebuild
		return RebuildOpenCodeGoPoolChannel(channelID)
	}

	refreshResult := make(chan error, 1)
	go func() {
		_, refreshErr := poolService.RefreshIdentity(context.Background(), channel.Id, results[0].IdentityUID)
		refreshResult <- refreshErr
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
		t.Fatal("relay crossed the refresh commit-to-rebuild window")
	case <-time.After(50 * time.Millisecond):
	}

	close(allowRebuild)
	require.NoError(t, <-refreshResult)
	select {
	case <-relayAcquired:
	case <-time.After(time.Second):
		t.Fatal("relay lease did not resume after refresh rebuild")
	}
}

func TestOpenCodeGoAuthenticationFailureKeepsRelayBlockedUntilRebuildCompletes(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	createEligibleOpenCodeGoWorkspace(
		t,
		db,
		codec,
		channel.Id,
		"auth-barrier",
		"workspace-auth-barrier",
		"wrk_AUTHBARRIER",
		[]string{"model-a"},
	)
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))
	selection, err := SelectOpenCodeGoWorkspace(channel.Id, "model-a")
	require.NoError(t, err)
	identity, err := model.GetOpenCodeGoIdentityPool(channel.Id, selection.IdentityUID)
	require.NoError(t, err)

	rebuildStarted := make(chan struct{})
	allowRebuild := make(chan struct{})
	poolService := NewOpenCodeGoAccountPoolAdminService()
	poolService.now = func() time.Time { return time.Unix(1_900_000_100, 0) }
	poolService.rebuild = func(channelID int) error {
		close(rebuildStarted)
		<-allowRebuild
		return RebuildOpenCodeGoPoolChannel(channelID)
	}

	refreshResult := make(chan error, 1)
	go func() {
		refreshResult <- poolService.markIdentityRefreshFailure(
			channel.Id,
			identity,
			model.OpenCodeGoIdentityStatusAuthError,
			ErrOpenCodeGoAuthenticationInvalid,
		)
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
		t.Fatal("relay crossed the authentication-failure commit-to-rebuild window")
	case <-time.After(50 * time.Millisecond):
	}

	close(allowRebuild)
	require.NoError(t, <-refreshResult)
	select {
	case <-relayAcquired:
	case <-time.After(time.Second):
		t.Fatal("relay lease did not resume after authentication-failure rebuild")
	}
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

func TestOpenCodeGoRefreshTargetsScopesConsoleByChannelAndIdentity(t *testing.T) {
	db, firstChannel, codec := setupOpenCodeGoPoolTestDB(t)
	secondChannel := &model.Channel{
		Type:   constant.ChannelTypeOpenCodeGo,
		Key:    "",
		Name:   "OpenCode Go second refresh channel",
		Status: common.ChannelStatusEnabled,
		Models: "model-a",
		Group:  "default",
	}
	require.NoError(t, db.Create(secondChannel).Error)
	fetchedAt := int64(1_900_000_000)
	createEligibleOpenCodeGoWorkspace(t, db, codec, firstChannel.Id, "first-channel", "workspace-first-channel", "wrk_FIRST1", []string{"model-a"})
	createEligibleOpenCodeGoWorkspace(t, db, codec, secondChannel.Id, "second-channel", "workspace-second-channel", "wrk_SECOND2", []string{"model-a"})

	newChannelConsole := func(cookie string, workspaceID string, apiKey string) *fakeOpenCodeGoConsole {
		return &fakeOpenCodeGoConsole{
			discoveredByCookie: map[string][]OpenCodeGoWorkspacePageResult{
				cookie: {{
					Workspace: OpenCodeGoDiscoveredWorkspace{ID: workspaceID},
					Page:      completeOpenCodeGoConsolePage(workspaceID, 25, fetchedAt+60),
				}},
			},
			discoverErr:            errors.New("console received an identity from another channel"),
			discoverErrorsByCookie: map[string]error{cookie: nil},
			keys:                   map[string]string{workspaceID: apiKey},
			keyErrors:              map[string]error{},
			models:                 map[string][]string{apiKey: {"model-a"}},
			modelErrors:            map[string]error{},
		}
	}
	consoles := map[int]openCodeGoConsoleReader{
		firstChannel.Id:  newChannelConsole("cookie-first-channel", "wrk_FIRST1", "sk-synthetic-first-channel"),
		secondChannel.Id: newChannelConsole("cookie-second-channel", "wrk_SECOND2", "sk-synthetic-second-channel"),
	}
	var factoryMutex sync.Mutex
	factoryCalls := make(map[string]int)
	poolService := &OpenCodeGoAccountPoolService{
		consoleFactory: func(channelID int, identityUID string) (openCodeGoConsoleReader, error) {
			factoryMutex.Lock()
			defer factoryMutex.Unlock()
			factoryCalls[fmt.Sprintf("%d:%s", channelID, identityUID)]++
			console, ok := consoles[channelID]
			if !ok {
				return nil, errors.New("unexpected OpenCode Go channel")
			}
			return console, nil
		},
		codec:   codec,
		now:     func() time.Time { return time.Unix(fetchedAt+60, 0) },
		rebuild: nil,
	}

	summary, err := poolService.RefreshIdentityTargets(
		context.Background(),
		[]model.OpenCodeGoRefreshTarget{
			{ChannelID: firstChannel.Id, IdentityUID: "identity-first-channel"},
			{ChannelID: secondChannel.Id, IdentityUID: "identity-second-channel"},
		},
		2,
		nil,
	)
	require.NoError(t, err)
	require.Equal(t, 2, summary.Succeeded)
	require.Zero(t, summary.Failed)
	assert.Equal(t, map[string]int{
		fmt.Sprintf("%d:%s", firstChannel.Id, "identity-first-channel"):   1,
		fmt.Sprintf("%d:%s", secondChannel.Id, "identity-second-channel"): 1,
	}, factoryCalls)
}

func TestOpenCodeGoImportAutomationContextLivesUntilRunnerCompletes(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	completed := make(chan struct{})
	var gotChannelID int
	var gotIdentityUIDs []string
	runner := func(ctx context.Context, channelID int, identityUIDs []string) error {
		gotChannelID = channelID
		gotIdentityUIDs = append([]string(nil), identityUIDs...)
		close(started)
		select {
		case <-release:
			assert.NoError(t, ctx.Err())
		case <-ctx.Done():
			return ctx.Err()
		}
		close(completed)
		return nil
	}

	runOpenCodeGoImportAutomationsWithRunner(context.Background(), 62, []string{"identity-import-one"}, runner)
	<-started
	assert.Equal(t, 62, gotChannelID)
	assert.Equal(t, []string{"identity-import-one"}, gotIdentityUIDs)
	close(release)
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("OpenCode Go import automation did not complete")
	}
}

func TestOpenCodeGoOlderConsoleCommitCannotOverwriteNewerRiskObservation(t *testing.T) {
	_, channel, _ := setupOpenCodeGoPoolTestDB(t)
	identity := model.OpenCodeGoIdentity{
		UID:                   "identity-console-race",
		ChannelID:             channel.Id,
		AuthCookieCiphertext:  "encrypted-cookie-console-race",
		AuthCookieFingerprint: "fingerprint-console-race",
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, model.DB.Create(&identity).Error)
	seeded := seedOpenCodeGoHealthWorkspace(t, channel.Id, identity.ID, "workspace-console-race", "glm-5.2")
	current, err := model.GetOpenCodeGoWorkspace(channel.Id, seeded.UID)
	require.NoError(t, err)
	require.NotNil(t, current)

	consoleTime := time.Unix(1_900_000_100, 0)
	prepared := openCodeGoPreparedWorkspace{
		record:      *current,
		models:      []string{"glm-5.2"},
		modelsFresh: true,
		healthObservation: OpenCodeGoHealthObservation{
			Kind:            OpenCodeGoObservationConsoleSnapshot,
			ObservedAt:      consoleTime,
			HasUsableModels: true,
		},
	}
	prepared.record.QuotaFetchedAt = consoleTime.Unix()
	prepared.record.QuotaNextRefreshAt = consoleTime.Add(time.Hour).Unix()
	prepared.record.LastSyncedAt = consoleTime.Unix()
	prepared.windows = make([]model.OpenCodeGoQuotaWindow, 0, len(model.OpenCodeGoQuotaKinds))
	for index, kind := range model.OpenCodeGoQuotaKinds {
		prepared.windows = append(prepared.windows, model.OpenCodeGoQuotaWindow{
			Kind:        kind,
			UsedPercent: 20 + float64(index),
			ResetAt:     consoleTime.Add(time.Duration(index+1) * time.Hour).Unix(),
			FetchedAt:   consoleTime.Unix(),
		})
	}

	riskTime := consoleTime.Add(time.Second)
	risk, ok := ClassifyOpenCodeGoProviderFailure(OpenCodeGoProviderFailure{
		StatusCode: 401,
		ErrorType:  "AuthError",
		Message:    "This account has found to be committing fraud or is in breach of terms of services and has been blocked.",
	}, riskTime)
	require.True(t, ok)
	applied, err := applyOpenCodeGoClassifiedFailure(channel.Id, seeded.UID, "glm-5.2", risk, nil)
	require.NoError(t, err)
	require.True(t, applied)

	require.NoError(t, model.DB.Transaction(func(tx *gorm.DB) error {
		return updateOpenCodeGoPreparedWorkspaceTx(tx, &prepared, riskTime.Add(time.Second).Unix())
	}))
	after, err := model.GetOpenCodeGoWorkspace(channel.Id, seeded.UID)
	require.NoError(t, err)
	assert.Equal(t, model.OpenCodeGoStateRiskBlocked, after.EffectiveState)
	assert.Equal(t, string(OpenCodeGoObservationRiskBlocked), after.HealthObservation)
	assert.Equal(t, riskTime.UnixNano(), after.HealthObservedAt)
	assert.Equal(t, consoleTime.Unix(), after.QuotaFetchedAt)
	require.Len(t, after.QuotaWindows, len(model.OpenCodeGoQuotaKinds))
	percentByKind := make(map[string]float64, len(after.QuotaWindows))
	for _, window := range after.QuotaWindows {
		percentByKind[window.Kind] = window.UsedPercent
	}
	assert.Equal(t, float64(20), percentByKind[model.OpenCodeGoQuotaRolling])
}

func TestOpenCodeGoOlderConsoleCommitCannotOverwriteNewerConsoleFields(t *testing.T) {
	_, channel, _ := setupOpenCodeGoPoolTestDB(t)
	identity := model.OpenCodeGoIdentity{
		UID:                   "identity-console-order",
		ChannelID:             channel.Id,
		AuthCookieCiphertext:  "encrypted-cookie-console-order",
		AuthCookieFingerprint: "fingerprint-console-order",
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, model.DB.Create(&identity).Error)
	seeded := seedOpenCodeGoHealthWorkspace(t, channel.Id, identity.ID, "workspace-console-order", "newer-model")

	newer := time.Unix(1_900_000_200, 0)
	require.NoError(t, model.DB.Model(&model.OpenCodeGoWorkspace{}).
		Where("id = ?", seeded.ID).
		Updates(map[string]interface{}{
			"name":                       "newer workspace",
			"email":                      "newer@example.test",
			"membership_status":          model.OpenCodeGoMembershipActive,
			"subscription_reference":     "sub_NEWER",
			"china_models_enabled":       true,
			"referral_code":              "NEWER",
			"available_referral_rewards": 3,
			"used_referral_rewards":      2,
			"last_synced_at":             newer.Unix(),
			"health_observation":         string(OpenCodeGoObservationConsoleSnapshot),
			"health_observed_at":         newer.UnixNano(),
		}).Error)
	current, err := model.GetOpenCodeGoWorkspace(channel.Id, seeded.UID)
	require.NoError(t, err)
	require.NotNil(t, current)

	older := newer.Add(-time.Minute)
	chinaModelsDisabled := false
	prepared := openCodeGoPreparedWorkspace{
		record:      *current,
		models:      []string{"older-model"},
		modelsFresh: true,
		healthObservation: OpenCodeGoHealthObservation{
			Kind:            OpenCodeGoObservationConsoleSnapshot,
			ObservedAt:      older,
			HasUsableModels: true,
		},
	}
	prepared.record.Name = "older workspace"
	prepared.record.Email = "older@example.test"
	prepared.record.MembershipStatus = model.OpenCodeGoMembershipInactive
	prepared.record.SubscriptionReference = "sub_OLDER"
	prepared.record.ChinaModelsEnabled = &chinaModelsDisabled
	prepared.record.ReferralCode = "OLDER"
	prepared.record.AvailableReferralRewards = 0
	prepared.record.UsedReferralRewards = 0
	prepared.record.LastSyncedAt = older.Unix()

	require.NoError(t, model.DB.Transaction(func(tx *gorm.DB) error {
		return updateOpenCodeGoPreparedWorkspaceTx(tx, &prepared, newer.Add(time.Minute).Unix())
	}))
	after, err := model.GetOpenCodeGoWorkspace(channel.Id, seeded.UID)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, "newer workspace", after.Name)
	assert.Equal(t, "newer@example.test", after.Email)
	assert.Equal(t, model.OpenCodeGoMembershipActive, after.MembershipStatus)
	assert.Equal(t, "sub_NEWER", after.SubscriptionReference)
	require.NotNil(t, after.ChinaModelsEnabled)
	assert.True(t, *after.ChinaModelsEnabled)
	assert.Equal(t, "NEWER", after.ReferralCode)
	assert.Equal(t, 3, after.AvailableReferralRewards)
	assert.Equal(t, 2, after.UsedReferralRewards)
	assert.Equal(t, newer.Unix(), after.LastSyncedAt)
	require.Len(t, after.Models, 1)
	assert.Equal(t, "newer-model", after.Models[0].Model)
}

func TestOpenCodeGoRefreshCommitPreservesConcurrentManualDisable(t *testing.T) {
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
	identity, err := model.GetOpenCodeGoIdentityPool(channel.Id, results[0].IdentityUID)
	require.NoError(t, err)
	require.Len(t, identity.Workspaces, 1)

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	fake.mutex.Lock()
	fake.discovered = []OpenCodeGoWorkspacePageResult{{
		Workspace: OpenCodeGoDiscoveredWorkspace{ID: "wrk_ALPHA1"},
		Page:      completeOpenCodeGoConsolePage("wrk_ALPHA1", 20, fetchedAt+60),
	}}
	fake.discoverStarted = started
	fake.discoverRelease = release
	fake.mutex.Unlock()
	poolService.now = func() time.Time { return time.Unix(fetchedAt+60, 0) }

	refreshResult := make(chan error, 1)
	go func() {
		_, refreshErr := poolService.RefreshIdentity(context.Background(), channel.Id, identity.UID)
		refreshResult <- refreshErr
	}()
	<-started
	require.NoError(t, poolService.SetIdentityEnabled(channel.Id, identity.UID, false))
	require.NoError(t, poolService.SetWorkspaceEnabled(channel.Id, identity.Workspaces[0].UID, false))
	close(release)
	require.NoError(t, <-refreshResult)

	after, err := model.GetOpenCodeGoIdentityPool(channel.Id, identity.UID)
	require.NoError(t, err)
	assert.Equal(t, model.OpenCodeGoIdentityStatusManualDisabled, after.Status)
	require.Len(t, after.Workspaces, 1)
	assert.False(t, after.Workspaces[0].ManualEnabled)
	assert.Equal(t, model.OpenCodeGoStateManualDisabled, after.Workspaces[0].EffectiveState)
}

func TestOpenCodeGoOlderRefreshFailureCannotInvalidateNewerSnapshot(t *testing.T) {
	_, channel, _ := setupOpenCodeGoPoolTestDB(t)
	identity := model.OpenCodeGoIdentity{
		UID:                   "identity-old-failure",
		ChannelID:             channel.Id,
		AuthCookieCiphertext:  "encrypted-cookie-old-failure",
		AuthCookieFingerprint: "fingerprint-old-failure",
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, model.DB.Create(&identity).Error)
	workspace := seedOpenCodeGoHealthWorkspace(t, channel.Id, identity.ID, "workspace-old-failure", "glm-5.2")
	staleIdentity, err := model.GetOpenCodeGoIdentityPool(channel.Id, identity.UID)
	require.NoError(t, err)

	newer := time.Unix(1_900_000_200, 0)
	require.NoError(t, model.DB.Model(&model.OpenCodeGoIdentity{}).
		Where("id = ?", identity.ID).
		Updates(map[string]interface{}{"last_synced_at": newer.Unix(), "last_error": ""}).Error)
	require.NoError(t, model.DB.Model(&model.OpenCodeGoWorkspace{}).
		Where("id = ?", workspace.ID).
		Updates(map[string]interface{}{
			"health_observation":    string(OpenCodeGoObservationConsoleSnapshot),
			"health_observed_at":    newer.UnixNano(),
			"last_synced_at":        newer.Unix(),
			"quota_snapshot_status": model.OpenCodeGoQuotaSnapshotComplete,
			"quota_error":           "",
		}).Error)

	poolService := NewOpenCodeGoAccountPoolAdminService()
	poolService.now = func() time.Time { return newer.Add(-time.Second) }
	poolService.rebuild = nil
	require.NoError(t, poolService.markIdentityRefreshFailure(
		channel.Id,
		staleIdentity,
		model.OpenCodeGoIdentityStatusAuthError,
		ErrOpenCodeGoAuthenticationInvalid,
	))

	after, err := model.GetOpenCodeGoIdentityPool(channel.Id, identity.UID)
	require.NoError(t, err)
	assert.Equal(t, model.OpenCodeGoIdentityStatusActive, after.Status)
	assert.Equal(t, newer.Unix(), after.LastSyncedAt)
	require.Len(t, after.Workspaces, 1)
	assert.Equal(t, model.OpenCodeGoQuotaSnapshotComplete, after.Workspaces[0].QuotaSnapshotStatus)
	assert.Equal(t, string(OpenCodeGoObservationConsoleSnapshot), after.Workspaces[0].HealthObservation)
	assert.Equal(t, newer.UnixNano(), after.Workspaces[0].HealthObservedAt)
}

func TestOpenCodeGoTransientRefreshFailurePreservesCompleteSnapshotEligibility(t *testing.T) {
	_, channel, _ := setupOpenCodeGoPoolTestDB(t)
	identity := model.OpenCodeGoIdentity{
		UID:                   "identity-transient-refresh",
		ChannelID:             channel.Id,
		AuthCookieCiphertext:  "encrypted-cookie-transient-refresh",
		AuthCookieFingerprint: "fingerprint-transient-refresh",
		Status:                model.OpenCodeGoIdentityStatusActive,
		LastSyncedAt:          1_900_000_000,
	}
	require.NoError(t, model.DB.Create(&identity).Error)
	workspace := seedOpenCodeGoHealthWorkspace(t, channel.Id, identity.ID, "workspace-transient-refresh", "glm-5.2")
	require.NoError(t, model.DB.Model(&model.OpenCodeGoWorkspace{}).
		Where("id = ?", workspace.ID).
		Updates(map[string]interface{}{
			"last_synced_at":     int64(1_900_000_000),
			"health_observation": string(OpenCodeGoObservationConsoleSnapshot),
			"health_observed_at": time.Unix(1_900_000_000, 0).UnixNano(),
		}).Error)

	poolService := NewOpenCodeGoAccountPoolAdminService()
	poolService.now = func() time.Time { return time.Unix(1_900_000_060, 0) }
	poolService.rebuild = nil
	require.NoError(t, poolService.markIdentityRefreshFailure(
		channel.Id,
		&identity,
		model.OpenCodeGoIdentityStatusStale,
		errors.New("OpenCode Go workspace request returned status 500"),
	))

	after, err := model.GetOpenCodeGoIdentityPool(channel.Id, identity.UID)
	require.NoError(t, err)
	assert.Equal(t, model.OpenCodeGoIdentityStatusActive, after.Status)
	assert.Equal(t, int64(1_900_000_060), after.LastSyncedAt)
	require.Len(t, after.Workspaces, 1)
	assert.Equal(t, model.OpenCodeGoStateEligible, after.Workspaces[0].EffectiveState)
	assert.Equal(t, model.OpenCodeGoQuotaSnapshotComplete, after.Workspaces[0].QuotaSnapshotStatus)
	assert.Equal(t, string(OpenCodeGoObservationConsoleSnapshot), after.Workspaces[0].HealthObservation)
	assert.Equal(t, int64(1_900_000_060), after.Workspaces[0].LastSyncedAt)
	assert.Contains(t, after.Workspaces[0].LastError, "status 500")
	assert.Contains(t, after.Workspaces[0].QuotaError, "status 500")
}

func TestSanitizeOpenCodeGoErrorTruncatesAtUTF8Boundary(t *testing.T) {
	message := strings.Repeat("\u754c", 200) + string([]byte{0xff})
	sanitized := sanitizeOpenCodeGoError(errors.New(message))

	assert.True(t, utf8.ValidString(sanitized))
	assert.LessOrEqual(t, len(sanitized), 512)
	assert.NotContains(t, sanitized, string([]byte{0xff}))
}

func TestSanitizeOpenCodeGoErrorRedactsIdentityUID(t *testing.T) {
	const identityUID = "123e4567-e89b-42d3-a456-426614174000"

	sanitized := sanitizeOpenCodeGoError(fmt.Errorf("identity %s proxy request failed", identityUID))

	assert.NotContains(t, sanitized, identityUID)
	assert.Contains(t, sanitized, "[identity]")
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
	// Channel models are admin-managed and must survive a pool reconcile even
	// when the pool has no eligible workspaces.
	require.Equal(t, "model-a,model-b", reloaded.Models)
	require.Equal(t, common.ChannelStatusAutoDisabled, reloaded.Status)
	require.Equal(t, openCodeGoNoEligibleWorkspaceReason, reloaded.GetOtherInfo()["status_reason"])
	var abilityCount int64
	require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", channel.Id).Count(&abilityCount).Error)
	require.NotZero(t, abilityCount)
	_, err = SelectOpenCodeGoWorkspace(channel.Id, "model-a")
	require.ErrorIs(t, err, ErrOpenCodeGoNoEligibleWorkspace)
}

func TestOpenCodeGoReconcileDoesNotRestoreAdminRemovedModels(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)

	// The pool supports both models, but the channel exposes only model-a.
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a", "model-b"})
	channel.Models = "model-a"
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("models", channel.Models).Error)

	require.NoError(t, ReconcileOpenCodeGoPoolChannel(channel.Id))

	reloaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	// model-b stays removed even though the pool still supports it.
	require.Equal(t, "model-a", reloaded.Models)

	// Kept models still resolve through the pool snapshot.
	_, err = SelectOpenCodeGoWorkspace(channel.Id, "model-a")
	require.NoError(t, err)
}

func TestOpenCodeGoReconcilePreservesManualModelsOutsidePool(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})
	channel.Models = "model-a,manual-model"
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("models", channel.Models).Error)

	require.NoError(t, ReconcileOpenCodeGoPoolChannel(channel.Id))

	reloaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	// Manual models the pool does not cover are preserved; reconcile neither
	// prunes nor appends to the admin-managed model list.
	require.Equal(t, "model-a,manual-model", reloaded.Models)
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

func TestOpenCodeGoPoolSessionAffinityIsStableAndDistributed(t *testing.T) {
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	first := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "one", "workspace-one", "wrk_ALPHA1", []string{"model-a"})
	second := createEligibleOpenCodeGoWorkspace(t, db, codec, channel.Id, "two", "workspace-two", "wrk_BETA2", []string{"model-a"})
	require.NoError(t, RebuildOpenCodeGoPoolChannel(channel.Id))

	stable, err := SelectOpenCodeGoWorkspaceWithAffinity(channel.Id, "model-a", "session-stable")
	require.NoError(t, err)
	for index := 0; index < 20; index++ {
		selection, selectErr := SelectOpenCodeGoWorkspaceWithAffinity(channel.Id, "model-a", "session-stable")
		require.NoError(t, selectErr)
		assert.Equal(t, stable.WorkspaceUID, selection.WorkspaceUID)
	}

	counts := map[string]int{first.UID: 0, second.UID: 0}
	for index := 0; index < 200; index++ {
		selection, selectErr := SelectOpenCodeGoWorkspaceWithAffinity(channel.Id, "model-a", fmt.Sprintf("session-%d", index))
		require.NoError(t, selectErr)
		counts[selection.WorkspaceUID]++
	}
	assert.Greater(t, counts[first.UID], 60)
	assert.Greater(t, counts[second.UID], 60)
	assert.Less(t, counts[first.UID], 140)
	assert.Less(t, counts[second.UID], 140)
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
