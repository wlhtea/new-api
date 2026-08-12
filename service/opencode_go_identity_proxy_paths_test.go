package service

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const (
	openCodeGoProductionProxyMarker      = "identity-proxy-production-path"
	openCodeGoProductionProxyTemplateSID = "271828"
)

var openCodeGoProductionProxyPaths = []string{
	"/account/provisional",
	"/account/persisted",
	"/risk/probe",
	"/lifecycle/route-asset",
	"/lifecycle/stripe",
}

type openCodeGoProductionPathTraffic struct {
	mutex       sync.Mutex
	hits        map[string]int
	proxiedHits map[string]int
}

func newOpenCodeGoProductionPathTarget(t *testing.T) (*httptest.Server, *openCodeGoProductionPathTraffic) {
	t.Helper()
	traffic := &openCodeGoProductionPathTraffic{
		hits:        make(map[string]int),
		proxiedHits: make(map[string]int),
	}
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		traffic.mutex.Lock()
		traffic.hits[request.URL.Path]++
		if request.Header.Get("X-Test-Proxy-Hop") == openCodeGoProductionProxyMarker {
			traffic.proxiedHits[request.URL.Path]++
		}
		traffic.mutex.Unlock()
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)
	return target, traffic
}

func (traffic *openCodeGoProductionPathTraffic) assertEachPath(t *testing.T, expectProxy bool) {
	t.Helper()
	traffic.mutex.Lock()
	defer traffic.mutex.Unlock()
	for _, path := range openCodeGoProductionProxyPaths {
		if traffic.hits[path] != 1 {
			t.Fatal("a production OpenCode Go client path did not reach the local target exactly once")
		}
		proxied := traffic.proxiedHits[path] == 1
		if proxied != expectProxy {
			t.Fatal("a production OpenCode Go client path used the wrong direct/proxy route")
		}
	}
}

type openCodeGoProductionMarkedProxy struct {
	server *httptest.Server

	mutex           sync.Mutex
	usernames       []string
	authParseFailed bool
	forwardFailed   bool
}

func newOpenCodeGoProductionMarkedProxy(t *testing.T) *openCodeGoProductionMarkedProxy {
	t.Helper()
	proxy := &openCodeGoProductionMarkedProxy{}
	forwardTransport := http.DefaultTransport.(*http.Transport).Clone()
	forwardTransport.Proxy = nil
	proxy.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		username, ok := openCodeGoProductionProxyUsername(request.Header.Get("Proxy-Authorization"))
		proxy.mutex.Lock()
		if ok {
			proxy.usernames = append(proxy.usernames, username)
		} else {
			proxy.authParseFailed = true
		}
		proxy.mutex.Unlock()

		forwardRequest := request.Clone(request.Context())
		forwardRequest.RequestURI = ""
		forwardRequest.Header = request.Header.Clone()
		forwardRequest.Header.Del("Proxy-Authorization")
		forwardRequest.Header.Set("X-Test-Proxy-Hop", openCodeGoProductionProxyMarker)
		response, err := forwardTransport.RoundTrip(forwardRequest)
		if err != nil {
			proxy.mutex.Lock()
			proxy.forwardFailed = true
			proxy.mutex.Unlock()
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for name, values := range response.Header {
			for _, value := range values {
				writer.Header().Add(name, value)
			}
		}
		writer.WriteHeader(response.StatusCode)
		_, _ = io.Copy(writer, response.Body)
	}))
	t.Cleanup(func() {
		proxy.server.Close()
		forwardTransport.CloseIdleConnections()
	})
	return proxy
}

func openCodeGoProductionProxyUsername(proxyAuthorization string) (string, bool) {
	scheme, encoded, found := strings.Cut(strings.TrimSpace(proxyAuthorization), " ")
	if !found || !strings.EqualFold(scheme, "Basic") {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", false
	}
	username, _, found := strings.Cut(string(decoded), ":")
	return username, found && username != ""
}

func (proxy *openCodeGoProductionMarkedProxy) credentialedTemplateURL(t *testing.T) string {
	t.Helper()
	parsed, err := url.Parse(proxy.server.URL)
	require.NoError(t, err)
	parsed.User = url.UserPassword(
		"fixture_custom_zone_US_sid_"+openCodeGoProductionProxyTemplateSID+"_time_10",
		"fixture-proxy-password",
	)
	return parsed.String()
}

func (proxy *openCodeGoProductionMarkedProxy) assertRequestsUsedUsername(t *testing.T, expected string) {
	t.Helper()
	proxy.mutex.Lock()
	defer proxy.mutex.Unlock()
	if proxy.authParseFailed || proxy.forwardFailed {
		t.Fatal("the local marked proxy could not authenticate or forward a production request")
	}
	if len(proxy.usernames) != len(openCodeGoProductionProxyPaths) {
		t.Fatal("the local marked proxy did not receive every production request")
	}
	for _, username := range proxy.usernames {
		if username != expected {
			t.Fatal("a production request did not use the expected identity-derived proxy username")
		}
	}
}

type openCodeGoProductionIdentityClients struct {
	provisional *OpenCodeGoConsoleClient
	persisted   *OpenCodeGoConsoleClient
	risk        *openCodeGoHTTPRiskProbeClient
	lifecycle   *OpenCodeGoLifecycleService
}

func configuredOpenCodeGoProductionIdentityClients(
	t *testing.T,
	channelID int,
	identityUID string,
) openCodeGoProductionIdentityClients {
	t.Helper()
	pool, err := NewConfiguredOpenCodeGoAccountPoolService()
	require.NoError(t, err)
	provisionalReader, err := pool.provisionalConsoleFactory(channelID, identityUID)
	require.NoError(t, err)
	provisional, ok := provisionalReader.(*OpenCodeGoConsoleClient)
	require.True(t, ok)
	persistedReader, err := pool.consoleFactory(channelID, identityUID)
	require.NoError(t, err)
	persisted, ok := persistedReader.(*OpenCodeGoConsoleClient)
	require.True(t, ok)

	riskService, err := NewConfiguredOpenCodeGoRiskRecheckService()
	require.NoError(t, err)
	riskProbe, err := riskService.probeFactory(channelID, identityUID)
	require.NoError(t, err)
	risk, ok := riskProbe.(*openCodeGoHTTPRiskProbeClient)
	require.True(t, ok)

	lifecycleService, err := NewConfiguredOpenCodeGoLifecycleService()
	require.NoError(t, err)
	lifecycle, err := lifecycleService.scopedFactory(channelID, identityUID)
	require.NoError(t, err)
	require.NotNil(t, lifecycle)
	return openCodeGoProductionIdentityClients{
		provisional: provisional,
		persisted:   persisted,
		risk:        risk,
		lifecycle:   lifecycle,
	}
}

func (clients openCodeGoProductionIdentityClients) httpClients(t *testing.T) []*http.Client {
	t.Helper()
	lifecycleConsole, ok := clients.lifecycle.reader.(*OpenCodeGoConsoleClient)
	require.True(t, ok)
	lifecycleMutator, ok := clients.lifecycle.mutator.(*OpenCodeGoLifecycleClient)
	require.True(t, ok)
	return []*http.Client{
		clients.provisional.followClient,
		clients.persisted.followClient,
		clients.risk.client,
		lifecycleConsole.followClient,
		lifecycleMutator.billingClient,
	}
}

func (clients openCodeGoProductionIdentityClients) exercise(t *testing.T, targetURL string) {
	t.Helper()
	requests := clients.httpClients(t)
	riskEndpoint, err := url.Parse(targetURL + openCodeGoProductionProxyPaths[2])
	require.NoError(t, err)
	clients.risk.endpoint = riskEndpoint

	openCodeGoProductionGET(t, requests[0], targetURL+openCodeGoProductionProxyPaths[0])
	openCodeGoProductionGET(t, requests[1], targetURL+openCodeGoProductionProxyPaths[1])
	response, err := clients.risk.Probe(context.Background(), "fixture-risk-key", openCodeGoDefaultRiskProbeModel)
	require.NoError(t, err)
	if response.StatusCode != http.StatusNoContent {
		t.Fatal("the production risk client returned an unexpected local response")
	}
	openCodeGoProductionGET(t, requests[3], targetURL+openCodeGoProductionProxyPaths[3])
	openCodeGoProductionGET(t, requests[4], targetURL+openCodeGoProductionProxyPaths[4])
}

func openCodeGoProductionGET(t *testing.T, client *http.Client, endpoint string) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, endpoint, nil)
	require.NoError(t, err)
	response, err := client.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	if response.StatusCode != http.StatusNoContent {
		t.Fatal("a production OpenCode Go client returned an unexpected local response")
	}
}

func persistOpenCodeGoProductionIdentity(t *testing.T, channelID int) string {
	t.Helper()
	identityUID := uuid.NewString()
	require.NoError(t, model.DB.Create(&model.OpenCodeGoIdentity{
		UID:                   identityUID,
		ChannelID:             channelID,
		AuthCookieCiphertext:  "opaque-test-ciphertext",
		AuthCookieFingerprint: strings.Repeat("f", 64),
		Status:                model.OpenCodeGoIdentityStatusActive,
	}).Error)
	return identityUID
}

func setOpenCodeGoProductionIdentityProxyPolicy(
	t *testing.T,
	channelID int,
	proxyURL string,
	config *dto.OpenCodeGoConfig,
) {
	t.Helper()
	setOpenCodeGoChannelHTTPSettings(t, channelID, dto.ChannelSettings{Proxy: proxyURL})
	otherSettings, err := common.Marshal(dto.ChannelOtherSettings{OpenCodeGo: config})
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&model.Channel{}).
		Where("id = ?", channelID).
		Update("settings", string(otherSettings)).Error)
}

func useIsolatedOpenCodeGoProductionIdentityProxyCache(t *testing.T) {
	t.Helper()
	previous := openCodeGoIdentityProxyClients
	openCodeGoIdentityProxyClients = newOpenCodeGoIdentityProxyClientCache(16, time.Hour)
	t.Cleanup(func() {
		openCodeGoIdentityProxyClients.reset()
		openCodeGoIdentityProxyClients = previous
	})
}

func assertOpenCodeGoProductionIdentityProxyClients(
	t *testing.T,
	clients []*http.Client,
	expectedProxyHost string,
	expectedCountry string,
) string {
	t.Helper()
	derivedUsername := ""
	for _, client := range clients {
		proxyRequest := identityProxyURLFromClient(t, client)
		if proxyRequest.URL.Host != expectedProxyHost {
			t.Fatal("a production factory resolved the wrong identity proxy endpoint")
		}
		username := proxyRequest.URL.User.Username()
		country, sid, ok := openCodeGoProductionProxyBindingParts(username)
		if !ok || country != expectedCountry || sid == openCodeGoProductionProxyTemplateSID {
			t.Fatal("a production factory did not resolve the configured identity proxy policy")
		}
		if len(sid) != 12 || strings.Trim(sid, "0123456789") != "" {
			t.Fatal("a production factory did not use a derived numeric proxy session")
		}
		if derivedUsername == "" {
			derivedUsername = username
		} else if username != derivedUsername {
			t.Fatal("production factories did not share one proxy binding for the same identity")
		}
	}
	return derivedUsername
}

func openCodeGoProductionProxyBindingParts(username string) (country string, sid string, ok bool) {
	parts := strings.Split(username, "_")
	for index := 0; index+1 < len(parts); index++ {
		switch strings.ToLower(parts[index]) {
		case "zone":
			if country != "" {
				return "", "", false
			}
			country = strings.ToUpper(parts[index+1])
		case "sid":
			if sid != "" {
				return "", "", false
			}
			sid = parts[index+1]
		}
	}
	return country, sid, country != "" && sid != ""
}

func TestConfiguredOpenCodeGoProductionFactoriesUseIdentityProxy(t *testing.T) {
	_, channel, _ := setupOpenCodeGoPoolTestDB(t)
	useIsolatedOpenCodeGoProductionIdentityProxyCache(t)
	identityUID := persistOpenCodeGoProductionIdentity(t, channel.Id)
	target, traffic := newOpenCodeGoProductionPathTarget(t)
	proxy := newOpenCodeGoProductionMarkedProxy(t)
	setOpenCodeGoProductionIdentityProxyPolicy(
		t,
		channel.Id,
		proxy.credentialedTemplateURL(t),
		&dto.OpenCodeGoConfig{
			IdentityProxyEnabled:       true,
			IdentityProxyCountry:       "CA",
			IdentityProxyRotateMinutes: 180,
		},
	)

	clients := configuredOpenCodeGoProductionIdentityClients(t, channel.Id, identityUID)
	derivedUsername := assertOpenCodeGoProductionIdentityProxyClients(
		t,
		clients.httpClients(t),
		strings.TrimPrefix(proxy.server.URL, "http://"),
		"CA",
	)
	clients.exercise(t, target.URL)

	traffic.assertEachPath(t, true)
	proxy.assertRequestsUsedUsername(t, derivedUsername)
}

func TestConfiguredOpenCodeGoProductionFactoriesUseDirectNetworkWithoutProxyConfiguration(t *testing.T) {
	_, channel, _ := setupOpenCodeGoPoolTestDB(t)
	initDefaultHTTPClientFixture(t)
	useIsolatedOpenCodeGoProductionIdentityProxyCache(t)
	identityUID := persistOpenCodeGoProductionIdentity(t, channel.Id)
	setOpenCodeGoProductionIdentityProxyPolicy(t, channel.Id, "", nil)
	target, traffic := newOpenCodeGoProductionPathTarget(t)

	clients := configuredOpenCodeGoProductionIdentityClients(t, channel.Id, identityUID)
	for _, client := range clients.httpClients(t) {
		if _, proxied := client.Transport.(*openCodeGoChannelRoundTripper); proxied {
			t.Fatal("an unconfigured production identity client unexpectedly used a proxy wrapper")
		}
	}
	clients.exercise(t, target.URL)

	traffic.assertEachPath(t, false)
}
