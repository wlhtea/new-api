package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type openCodeGoRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip openCodeGoRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func setOpenCodeGoChannelHTTPSettings(
	t *testing.T,
	channelID int,
	settings dto.ChannelSettings,
) {
	t.Helper()
	encoded, err := common.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&model.Channel{}).
		Where("id = ?", channelID).
		Update("setting", string(encoded)).Error)
}

func newOpenCodeGoMarkedProxy(t *testing.T, marker string) *httptest.Server {
	t.Helper()
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.NotEmpty(t, request.Header.Get("Proxy-Authorization"))
		writer.Header().Set("X-Test-Proxy", marker)
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(marker))
	}))
	t.Cleanup(proxy.Close)
	return proxy
}

func openCodeGoProxyURLWithCredentials(t *testing.T, rawURL string, suffix string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	require.NoError(t, err)
	parsed.User = url.UserPassword("test-proxy-user-"+suffix, "test-proxy-password-"+suffix)
	return parsed.String()
}

func TestOpenCodeGoChannelHTTPClientRoutesEachChannelThroughItsProxy(t *testing.T) {
	_, firstChannel, _ := setupOpenCodeGoPoolTestDB(t)
	secondChannel := &model.Channel{
		Type:   constant.ChannelTypeOpenCodeGo,
		Key:    "",
		Name:   "OpenCode Go second proxy",
		Status: common.ChannelStatusEnabled,
		Models: "model-a",
		Group:  "default",
	}
	require.NoError(t, model.DB.Create(secondChannel).Error)

	firstProxy := newOpenCodeGoMarkedProxy(t, "first")
	secondProxy := newOpenCodeGoMarkedProxy(t, "second")
	setOpenCodeGoChannelHTTPSettings(t, firstChannel.Id, dto.ChannelSettings{
		Proxy: openCodeGoProxyURLWithCredentials(t, firstProxy.URL, "first"),
	})
	setOpenCodeGoChannelHTTPSettings(t, secondChannel.Id, dto.ChannelSettings{
		Proxy: openCodeGoProxyURLWithCredentials(t, secondProxy.URL, "second"),
	})

	for _, testCase := range []struct {
		channelID int
		marker    string
	}{
		{channelID: firstChannel.Id, marker: "first"},
		{channelID: secondChannel.Id, marker: "second"},
	} {
		client, err := getOpenCodeGoChannelHTTPClient(testCase.channelID)
		require.NoError(t, err)
		response, err := client.Get("http://opencode-go-upstream.invalid/proxy-check")
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		assert.Equal(t, testCase.marker, response.Header.Get("X-Test-Proxy"))
		assert.Equal(t, testCase.marker, string(body))
	}
}

func TestOpenCodeGoChannelHTTPClientEmptyProxyKeepsDefaultClient(t *testing.T) {
	_, channel, _ := setupOpenCodeGoPoolTestDB(t)
	defaultClient := initDefaultHTTPClientFixture(t)
	setOpenCodeGoChannelHTTPSettings(t, channel.Id, dto.ChannelSettings{})

	client, err := getOpenCodeGoChannelHTTPClient(channel.Id)
	require.NoError(t, err)
	assert.Same(t, defaultClient, client)
}

func TestOpenCodeGoChannelHTTPClientFailsClosedWithoutExposingProxy(t *testing.T) {
	_, channel, _ := setupOpenCodeGoPoolTestDB(t)
	var directHits atomic.Int32
	direct := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		directHits.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	defer direct.Close()

	closedProxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedProxyURL := openCodeGoProxyURLWithCredentials(t, closedProxy.URL, "closed")
	closedProxy.Close()
	setOpenCodeGoChannelHTTPSettings(t, channel.Id, dto.ChannelSettings{Proxy: closedProxyURL})

	client, err := getOpenCodeGoChannelHTTPClient(channel.Id)
	require.NoError(t, err)
	_, err = client.Get(direct.URL)
	require.Error(t, err)
	assert.Zero(t, directHits.Load(), "a failed channel proxy must not fall back to a direct request")
	assert.NotContains(t, err.Error(), "test-proxy-user-closed")
	assert.NotContains(t, err.Error(), "test-proxy-password-closed")
	assert.NotContains(t, err.Error(), strings.TrimPrefix(closedProxy.URL, "http://"))
}

func TestOpenCodeGoChannelHTTPClientRedactsProxyHostnameFromDNSError(t *testing.T) {
	_, channel, _ := setupOpenCodeGoPoolTestDB(t)
	const proxyHostname = "test-secret-proxy-host.invalid"
	setOpenCodeGoChannelHTTPSettings(t, channel.Id, dto.ChannelSettings{
		Proxy: "http://test-proxy-user-dns:test-proxy-password-dns@" + proxyHostname + ":43123",
	})

	client, err := getOpenCodeGoChannelHTTPClient(channel.Id)
	require.NoError(t, err)
	transport, ok := client.Transport.(*openCodeGoChannelRoundTripper)
	require.True(t, ok)
	transport.base = openCodeGoRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("proxyconnect tcp: dial tcp: lookup %s: no such host", proxyHostname)
	})
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://opencode-go-upstream.invalid", nil)
	require.NoError(t, err)

	_, err = client.Do(request)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), proxyHostname)
	assert.Contains(t, err.Error(), "[redacted]")
}

func TestOpenCodeGoChannelHTTPClientRejectsInvalidChannelSettings(t *testing.T) {
	_, channel, _ := setupOpenCodeGoPoolTestDB(t)
	require.NoError(t, model.DB.Model(&model.Channel{}).
		Where("id = ?", channel.Id).
		Update("setting", "{").Error)

	_, err := getOpenCodeGoChannelHTTPClient(channel.Id)
	require.EqualError(t, err, "OpenCode Go channel HTTP settings are invalid")

	invalidProxy := "http://test-proxy-user-invalid:test-proxy-password-invalid@"
	setOpenCodeGoChannelHTTPSettings(t, channel.Id, dto.ChannelSettings{Proxy: invalidProxy})
	_, err = getOpenCodeGoChannelHTTPClient(channel.Id)
	require.EqualError(t, err, "OpenCode Go channel HTTP client could not be configured")
	assert.NotContains(t, err.Error(), "test-proxy-user-invalid")
	assert.NotContains(t, err.Error(), "test-proxy-password-invalid")
}

func TestOpenCodeGoChannelHTTPClientRejectsOtherChannelTypes(t *testing.T) {
	_, _, _ = setupOpenCodeGoPoolTestDB(t)
	channel := &model.Channel{
		Type:   constant.ChannelTypeOpenAI,
		Key:    "test-key",
		Name:   "non OpenCode Go channel",
		Status: common.ChannelStatusEnabled,
		Models: "model-a",
		Group:  "default",
	}
	require.NoError(t, model.DB.Create(channel).Error)

	_, err := getOpenCodeGoChannelHTTPClient(channel.Id)
	require.EqualError(t, err, "channel is not an OpenCode Go channel")
}

func createOpenCodeGoRelayFenceFixture(
	t *testing.T,
) (*model.Channel, *OpenCodeGoCredentialCodec, model.OpenCodeGoIdentity, model.OpenCodeGoWorkspace, string, OpenCodeGoIdentityProxyGeneration) {
	t.Helper()
	db, channel, codec := setupOpenCodeGoPoolTestDB(t)
	now := time.Now().Unix()
	identityUID := "identity-relay-fence"
	workspaceUID := "workspace-relay-fence"
	apiKey := "api-key-relay-fence"
	cookieCiphertext, err := codec.Encrypt(OpenCodeGoCredentialAuthCookie, channel.Id, identityUID, "cookie-relay-fence")
	require.NoError(t, err)
	identity := model.OpenCodeGoIdentity{
		UID:                   identityUID,
		ChannelID:             channel.Id,
		AuthCookieCiphertext:  cookieCiphertext,
		AuthCookieFingerprint: fmt.Sprintf("%064s", "relay-fence-cookie"),
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, db.Create(&identity).Error)
	apiKeyCiphertext, err := codec.Encrypt(OpenCodeGoCredentialAPIKey, channel.Id, workspaceUID, apiKey)
	require.NoError(t, err)
	workspace := model.OpenCodeGoWorkspace{
		UID:                 workspaceUID,
		ChannelID:           channel.Id,
		IdentityID:          identity.ID,
		UpstreamWorkspaceID: "upstream-relay-fence",
		APIKeyCiphertext:    apiKeyCiphertext,
		APIKeyFingerprint:   fmt.Sprintf("%064s", "relay-fence-key"),
		CredentialStatus:    model.OpenCodeGoCredentialValid,
		MembershipStatus:    model.OpenCodeGoMembershipActive,
		ManualEnabled:       true,
		EffectiveState:      model.OpenCodeGoStateEligible,
		QuotaSnapshotStatus: model.OpenCodeGoQuotaSnapshotComplete,
		QuotaFetchedAt:      now,
		QuotaNextRefreshAt:  now + 3600,
		QuotaParserVersion:  OpenCodeGoSSRParserVersion,
	}
	require.NoError(t, db.Create(&workspace).Error)
	for index, kind := range model.OpenCodeGoQuotaKinds {
		require.NoError(t, db.Create(&model.OpenCodeGoQuotaWindow{
			WorkspaceID:  workspace.ID,
			Kind:         kind,
			UsedPercent:  float64(10 + index),
			ResetSeconds: int64((index + 1) * 3600),
			ResetAt:      now + int64((index+1)*3600),
			FetchedAt:    now,
		}).Error)
	}
	require.NoError(t, db.Create(&model.OpenCodeGoWorkspaceModel{
		WorkspaceID: workspace.ID,
		Model:       "model-a",
		Discovered:  true,
		State:       model.OpenCodeGoModelAvailable,
	}).Error)
	return channel, codec, identity, workspace, apiKey, openCodeGoIdentityProxyClients.captureGeneration(channel.Id)
}

func TestAcquireOpenCodeGoRelayHTTPClientAcceptsCurrentDirectSelection(t *testing.T) {
	channel, _, identity, workspace, apiKey, generation := createOpenCodeGoRelayFenceFixture(t)
	client, release, err := AcquireOpenCodeGoRelayHTTPClient(
		channel.Id,
		identity.UID,
		workspace.UID,
		apiKey,
		"model-a",
		generation,
	)
	require.NoError(t, err)
	require.NotNil(t, client)
	require.NotNil(t, release)
	release()
	release()
}

func TestAcquireOpenCodeGoRelayHTTPClientRejectsStaleGenerationBeforeQueries(t *testing.T) {
	channel, _, identity, workspace, apiKey, generation := createOpenCodeGoRelayFenceFixture(t)
	openCodeGoIdentityProxyClients.advanceSelectionGeneration(channel.Id)

	var queryCount atomic.Int32
	const callbackName = "test:opencode-go-stale-generation-query-count"
	require.NoError(t, model.DB.Callback().Query().After("gorm:query").Register(callbackName, func(*gorm.DB) {
		queryCount.Add(1)
	}))
	t.Cleanup(func() { _ = model.DB.Callback().Query().Remove(callbackName) })

	client, release, err := AcquireOpenCodeGoRelayHTTPClient(
		channel.Id,
		identity.UID,
		workspace.UID,
		apiKey,
		"model-a",
		generation,
	)
	require.ErrorIs(t, err, ErrOpenCodeGoIdentityProxySelectionStale)
	assert.Nil(t, client)
	assert.Nil(t, release)
	assert.Zero(t, queryCount.Load())
}

func TestAcquireOpenCodeGoRelayHTTPClientRejectsDeletedOrDisabledOwnership(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*testing.T, int, model.OpenCodeGoIdentity, model.OpenCodeGoWorkspace)
	}{
		{
			name: "disabled identity",
			mutate: func(t *testing.T, channelID int, identity model.OpenCodeGoIdentity, _ model.OpenCodeGoWorkspace) {
				require.NoError(t, model.DB.Model(&model.OpenCodeGoIdentity{}).
					Where("id = ? AND channel_id = ?", identity.ID, channelID).
					Update("status", model.OpenCodeGoIdentityStatusManualDisabled).Error)
			},
		},
		{
			name: "deleted identity",
			mutate: func(t *testing.T, channelID int, identity model.OpenCodeGoIdentity, _ model.OpenCodeGoWorkspace) {
				require.NoError(t, model.DeleteOpenCodeGoIdentity(channelID, identity.UID))
			},
		},
		{
			name: "disabled workspace",
			mutate: func(t *testing.T, channelID int, _ model.OpenCodeGoIdentity, workspace model.OpenCodeGoWorkspace) {
				require.NoError(t, model.DB.Model(&model.OpenCodeGoWorkspace{}).
					Where("id = ? AND channel_id = ?", workspace.ID, channelID).
					Update("manual_enabled", false).Error)
			},
		},
		{
			name: "deleted workspace",
			mutate: func(t *testing.T, channelID int, _ model.OpenCodeGoIdentity, workspace model.OpenCodeGoWorkspace) {
				require.NoError(t, model.DeleteOpenCodeGoWorkspace(channelID, workspace.UID))
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			channel, _, identity, workspace, apiKey, generation := createOpenCodeGoRelayFenceFixture(t)
			testCase.mutate(t, channel.Id, identity, workspace)

			client, release, err := AcquireOpenCodeGoRelayHTTPClient(
				channel.Id,
				identity.UID,
				workspace.UID,
				apiKey,
				"model-a",
				generation,
			)
			require.ErrorIs(t, err, ErrOpenCodeGoIdentityProxySelectionStale)
			assert.Nil(t, client)
			assert.Nil(t, release)
			assert.NotContains(t, err.Error(), identity.UID)
			assert.NotContains(t, err.Error(), apiKey)
		})
	}
}

func TestAcquireOpenCodeGoRelayHTTPClientRejectsDisabledOrDeletedChannel(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*testing.T, *model.Channel)
	}{
		{
			name: "disabled channel",
			mutate: func(t *testing.T, channel *model.Channel) {
				require.NoError(t, model.DB.Model(&model.Channel{}).
					Where("id = ?", channel.Id).
					Update("status", common.ChannelStatusManuallyDisabled).Error)
			},
		},
		{
			name: "deleted channel",
			mutate: func(t *testing.T, channel *model.Channel) {
				require.NoError(t, channel.Delete())
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			channel, _, identity, workspace, apiKey, generation := createOpenCodeGoRelayFenceFixture(t)
			testCase.mutate(t, channel)
			client, release, err := AcquireOpenCodeGoRelayHTTPClient(
				channel.Id,
				identity.UID,
				workspace.UID,
				apiKey,
				"model-a",
				generation,
			)
			require.ErrorIs(t, err, ErrOpenCodeGoIdentityProxySelectionStale)
			assert.Nil(t, client)
			assert.Nil(t, release)
			assert.NotContains(t, err.Error(), identity.UID)
			assert.NotContains(t, err.Error(), apiKey)
		})
	}
}

func TestAcquireOpenCodeGoRelayHTTPClientRejectsChangedAPIKey(t *testing.T) {
	channel, codec, identity, workspace, oldAPIKey, generation := createOpenCodeGoRelayFenceFixture(t)
	newCiphertext, err := codec.Encrypt(OpenCodeGoCredentialAPIKey, channel.Id, workspace.UID, "api-key-replaced")
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&model.OpenCodeGoWorkspace{}).
		Where("id = ?", workspace.ID).
		Update("api_key_ciphertext", newCiphertext).Error)

	client, release, err := AcquireOpenCodeGoRelayHTTPClient(
		channel.Id,
		identity.UID,
		workspace.UID,
		oldAPIKey,
		"model-a",
		generation,
	)
	require.ErrorIs(t, err, ErrOpenCodeGoIdentityProxySelectionStale)
	assert.Nil(t, client)
	assert.Nil(t, release)
	assert.NotContains(t, err.Error(), oldAPIKey)
	assert.NotContains(t, err.Error(), identity.UID)
}

func TestAcquireOpenCodeGoRelayHTTPClientLoadsCurrentChannelProxyPolicy(t *testing.T) {
	channel, _, identity, workspace, apiKey, generation := createOpenCodeGoRelayFenceFixture(t)
	previousCache := openCodeGoIdentityProxyClients
	openCodeGoIdentityProxyClients = newOpenCodeGoIdentityProxyClientCache(4, time.Hour)
	t.Cleanup(func() {
		openCodeGoIdentityProxyClients.reset()
		openCodeGoIdentityProxyClients = previousCache
	})
	generation = openCodeGoIdentityProxyClients.captureGeneration(channel.Id)
	proxyTemplate := "http://current_zone_US_sid_1_time_10:current-password@current-proxy.invalid:18080"
	setOpenCodeGoChannelHTTPSettings(t, channel.Id, dto.ChannelSettings{Proxy: proxyTemplate})
	otherSettings := dto.ChannelOtherSettings{OpenCodeGo: &dto.OpenCodeGoConfig{
		IdentityProxyEnabled:       true,
		IdentityProxyCountry:       "CA",
		IdentityProxyRotateMinutes: 10,
	}}
	encodedOtherSettings, err := common.Marshal(otherSettings)
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&model.Channel{}).
		Where("id = ?", channel.Id).
		Update("settings", string(encodedOtherSettings)).Error)

	client, release, err := AcquireOpenCodeGoRelayHTTPClient(
		channel.Id,
		identity.UID,
		workspace.UID,
		apiKey,
		"model-a",
		generation,
	)
	require.NoError(t, err)
	require.NotNil(t, release)
	defer release()
	request, err := http.NewRequest(http.MethodGet, "http://upstream.invalid", nil)
	require.NoError(t, err)
	wrapper, ok := client.Transport.(*openCodeGoChannelRoundTripper)
	require.True(t, ok)
	transport, ok := wrapper.base.(*http.Transport)
	require.True(t, ok)
	derivedProxy, err := transport.Proxy(request)
	require.NoError(t, err)
	require.NotNil(t, derivedProxy)
	assert.Equal(t, "current-proxy.invalid:18080", derivedProxy.Host)
	assert.Contains(t, derivedProxy.User.Username(), "zone_CA")
	assert.NotEqual(t, "current_zone_US_sid_1_time_10", derivedProxy.User.Username())
}

func TestOpenCodeGoChannelLifecycleRuntimeSharesOneTransport(t *testing.T) {
	_, channel, codec := setupOpenCodeGoPoolTestDB(t)
	proxy := newOpenCodeGoMarkedProxy(t, "lifecycle")
	setOpenCodeGoChannelHTTPSettings(t, channel.Id, dto.ChannelSettings{
		Proxy: openCodeGoProxyURLWithCredentials(t, proxy.URL, "lifecycle"),
	})

	runtime, err := newOpenCodeGoChannelLifecycleService(channel.Id, codec)
	require.NoError(t, err)
	console, ok := runtime.reader.(*OpenCodeGoConsoleClient)
	require.True(t, ok)
	mutator, ok := runtime.mutator.(*OpenCodeGoLifecycleClient)
	require.True(t, ok)
	poolConsole, ok := runtime.pool.console.(*OpenCodeGoConsoleClient)
	require.True(t, ok)

	assert.Same(t, console, poolConsole)
	assert.Same(t, console.followClient.Transport, console.manualClient.Transport)
	assert.Same(t, console.followClient.Transport, mutator.billingClient.Transport)
}

func TestSanitizeOpenCodeGoErrorRedactsProxyUserinfoAndAuthorization(t *testing.T) {
	err := errors.New(
		"proxy http://proxy-user:proxy-password@proxy.example:8080 failed; " +
			"fallback socks5h://socks-user:socks-password@127.0.0.1:1080\n" +
			"Authorization: Bearer opaque-token\n" +
			"Authorization: Basic basic-credential\n" +
			"Proxy-Authorization: Custom proxy-secret-part-one;proxy-secret-part-two",
	)
	sanitized := sanitizeOpenCodeGoError(err)

	for _, forbidden := range []string{
		"proxy-user",
		"proxy-password",
		"socks-user",
		"socks-password",
		"opaque-token",
		"basic-credential",
		"proxy-secret-part-one",
		"proxy-secret-part-two",
	} {
		assert.NotContains(t, sanitized, forbidden, fmt.Sprintf("sanitized error exposed %s", forbidden))
	}
	assert.NotContains(t, sanitized, "proxy.example:8080")
	assert.NotContains(t, sanitized, "127.0.0.1:1080")
	assert.Contains(t, sanitized, "[redacted-proxy]")
	assert.Contains(t, sanitized, "Authorization: [redacted]")
	assert.Contains(t, sanitized, "Proxy-Authorization: [redacted]")
}
