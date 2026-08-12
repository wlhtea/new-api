package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type openCodeGoIdentityProxyCloseTracker struct {
	closed atomic.Int32
}

type openCodeGoIdentityProxyTrackedTransport struct {
	base   *http.Transport
	closed atomic.Int32
}

func (transport *openCodeGoIdentityProxyTrackedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport.base.RoundTrip(request)
}

func (transport *openCodeGoIdentityProxyTrackedTransport) CloseIdleConnections() {
	transport.closed.Add(1)
	transport.base.CloseIdleConnections()
}

func (tracker *openCodeGoIdentityProxyCloseTracker) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("ok")),
		Header:     make(http.Header),
	}, nil
}

func (tracker *openCodeGoIdentityProxyCloseTracker) CloseIdleConnections() {
	tracker.closed.Add(1)
}

func withOpenCodeGoIdentityProxySecret(t *testing.T) {
	t.Helper()
	previousSecret := common.CryptoSecret
	previousConfigured := common.CryptoSecretExplicitlyConfigured
	common.CryptoSecret = "test-only-identity-proxy-secret"
	common.CryptoSecretExplicitlyConfigured = true
	t.Cleanup(func() {
		common.CryptoSecret = previousSecret
		common.CryptoSecretExplicitlyConfigured = previousConfigured
	})
}

func TestDeriveOpenCodeGoIdentityProxySIDIsStableAndSeparated(t *testing.T) {
	const secret = "test-only-secret"
	first := deriveOpenCodeGoIdentityProxySID(secret, 11, "identity-a", 100)
	assert.Equal(t, first, deriveOpenCodeGoIdentityProxySID(secret, 11, "identity-a", 100))
	assert.NotEqual(t, first, deriveOpenCodeGoIdentityProxySID(secret, 11, "identity-b", 100))
	assert.NotEqual(t, first, deriveOpenCodeGoIdentityProxySID(secret, 12, "identity-a", 100))
	assert.NotEqual(t, first, deriveOpenCodeGoIdentityProxySID(secret, 11, "identity-a", 101))
	assert.NotContains(t, first, "identity")
	assert.Len(t, first, 12)
	for _, char := range first {
		assert.GreaterOrEqual(t, char, '0')
		assert.LessOrEqual(t, char, '9')
	}
}

func TestResolveOpenCodeGoIdentityHTTPClientScopesAndRotates(t *testing.T) {
	withOpenCodeGoIdentityProxySecret(t)
	previousCache := openCodeGoIdentityProxyClients
	openCodeGoIdentityProxyClients = newOpenCodeGoIdentityProxyClientCache(8, time.Hour)
	t.Cleanup(func() {
		openCodeGoIdentityProxyClients.reset()
		openCodeGoIdentityProxyClients = previousCache
	})

	settings := dto.ChannelSettings{
		Proxy: "http://test_custom_zone_US_sid_1_time_10:secret@proxy.example:8080",
	}
	config := &dto.OpenCodeGoConfig{
		IdentityProxyEnabled:       true,
		IdentityProxyCountry:       "GB",
		IdentityProxyRotateMinutes: 10,
	}
	firstTime := time.Unix(60*10*100+1, 0)
	first, err := resolveOpenCodeGoIdentityHTTPClient(41, "identity-a", settings, config, firstTime)
	require.NoError(t, err)
	same, err := resolveOpenCodeGoIdentityHTTPClient(41, "identity-a", settings, config, firstTime.Add(time.Minute))
	require.NoError(t, err)
	otherIdentity, err := resolveOpenCodeGoIdentityHTTPClient(41, "identity-b", settings, config, firstTime)
	require.NoError(t, err)
	nextBucket, err := resolveOpenCodeGoIdentityHTTPClient(41, "identity-a", settings, config, firstTime.Add(10*time.Minute))
	require.NoError(t, err)

	assert.Same(t, first, same)
	assert.NotSame(t, first, otherIdentity)
	assert.NotSame(t, first, nextBucket)
	assert.Equal(t, "GB", identityProxyCountryFromClient(t, nextBucket))
	assert.NotEqual(t, identityProxyUsernameFromClient(t, first), identityProxyUsernameFromClient(t, otherIdentity))
	assert.NotEqual(t, identityProxyUsernameFromClient(t, first), identityProxyUsernameFromClient(t, nextBucket))
}

func TestResolveOpenCodeGoIdentityHTTPClientPreservesTransportPolicy(t *testing.T) {
	withOpenCodeGoIdentityProxySecret(t)
	previousCache := openCodeGoIdentityProxyClients
	openCodeGoIdentityProxyClients = newOpenCodeGoIdentityProxyClientCache(8, time.Hour)
	t.Cleanup(func() {
		openCodeGoIdentityProxyClients.reset()
		openCodeGoIdentityProxyClients = previousCache
	})
	config := &dto.OpenCodeGoConfig{
		IdentityProxyEnabled:       true,
		IdentityProxyCountry:       "US",
		IdentityProxyRotateMinutes: 10,
	}
	const proxyTemplate = "http://template_zone_US_sid_1_time_10:secret@proxy.example:8080"
	now := time.Unix(60*10*100+1, 0)

	http1Client, err := resolveOpenCodeGoIdentityHTTPClient(61, "identity-http1", dto.ChannelSettings{
		Proxy:        proxyTemplate,
		HTTPProtocol: dto.HTTPProtocolHTTP1,
	}, config, now)
	require.NoError(t, err)
	http1Wrapper, ok := http1Client.Transport.(*openCodeGoChannelRoundTripper)
	require.True(t, ok)
	http1Transport, ok := http1Wrapper.base.(*http.Transport)
	require.True(t, ok)
	assert.False(t, http1Transport.ForceAttemptHTTP2)
	assert.NotNil(t, http1Transport.TLSNextProto)
	assert.Empty(t, http1Transport.TLSNextProto)

	shardedClient, err := resolveOpenCodeGoIdentityHTTPClient(61, "identity-sharded", dto.ChannelSettings{
		Proxy:                 proxyTemplate,
		HTTP2ConnectionShards: 3,
	}, config, now)
	require.NoError(t, err)
	shardedWrapper, ok := shardedClient.Transport.(*openCodeGoChannelRoundTripper)
	require.True(t, ok)
	shardedTransport, ok := shardedWrapper.base.(*shardedRoundTripper)
	require.True(t, ok)
	assert.Equal(t, uint32(3), shardedTransport.n)
	assert.Equal(t, HTTPTransportPolicy{Protocol: dto.HTTPProtocolAuto, Shards: 3}, shardedTransport.policy)
	assert.Len(t, shardedTransport.shards, 3)
}

func TestResolveOpenCodeGoIdentityHTTPClientEmptyProxyUsesDirectClient(t *testing.T) {
	previousClient := httpClient
	directHits := atomic.Int32{}
	directClient := &http.Client{Transport: openCodeGoRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		directHits.Add(1)
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    request,
		}, nil
	})}
	httpClient = directClient
	t.Cleanup(func() { httpClient = previousClient })

	client, err := resolveOpenCodeGoIdentityHTTPClient(51, "identity-direct", dto.ChannelSettings{}, nil, time.Now())
	require.NoError(t, err)
	assert.Same(t, directClient, client)

	request, err := http.NewRequest(http.MethodGet, "http://upstream.invalid/direct", nil)
	require.NoError(t, err)
	response, err := client.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, int32(1), directHits.Load())
}

func TestResolveOpenCodeGoIdentityHTTPClientUnreachableProxyFailsClosed(t *testing.T) {
	withOpenCodeGoIdentityProxySecret(t)
	previousCache := openCodeGoIdentityProxyClients
	openCodeGoIdentityProxyClients = newOpenCodeGoIdentityProxyClientCache(4, time.Hour)
	t.Cleanup(func() {
		openCodeGoIdentityProxyClients.reset()
		openCodeGoIdentityProxyClients = previousCache
	})

	settings := dto.ChannelSettings{
		Proxy: "http://template_custom_zone_US_sid_1_time_10:secret@proxy-unreachable.invalid:18080",
	}
	config := &dto.OpenCodeGoConfig{
		IdentityProxyEnabled:       true,
		IdentityProxyCountry:       "US",
		IdentityProxyRotateMinutes: 10,
	}
	client, err := resolveOpenCodeGoIdentityHTTPClient(52, "identity-fail-closed", settings, config, time.Now())
	require.NoError(t, err)
	wrapper, ok := client.Transport.(*openCodeGoChannelRoundTripper)
	require.True(t, ok)
	transport, ok := wrapper.base.(*http.Transport)
	require.True(t, ok)
	dialedAddress := ""
	transport.DialContext = func(_ context.Context, _, address string) (net.Conn, error) {
		dialedAddress = address
		return nil, errors.New("proxy dial failed")
	}

	request, err := http.NewRequest(http.MethodGet, "http://direct-target.invalid:28080/must-not-be-direct", nil)
	require.NoError(t, err)
	_, err = client.Do(request)
	require.Error(t, err)
	assert.Equal(t, "proxy-unreachable.invalid:18080", dialedAddress)
	assert.NotEqual(t, "direct-target.invalid:28080", dialedAddress)
	for _, forbidden := range []string{
		"template_custom_zone_US_sid_1_time_10",
		"identity-fail-closed",
		"proxy-unreachable.invalid",
	} {
		assert.NotContains(t, err.Error(), forbidden)
	}
}

func TestOpenCodeGoIdentityProxyLocalNetworkFallbackAndFailClosed(t *testing.T) {
	withOpenCodeGoIdentityProxySecret(t)
	previousCache := openCodeGoIdentityProxyClients
	openCodeGoIdentityProxyClients = newOpenCodeGoIdentityProxyClientCache(4, time.Hour)
	t.Cleanup(func() {
		openCodeGoIdentityProxyClients.reset()
		openCodeGoIdentityProxyClients = previousCache
	})

	var directHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		directHits.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)

	directClient, err := resolveOpenCodeGoIdentityHTTPClient(
		53,
		"identity-local-direct",
		dto.ChannelSettings{},
		nil,
		time.Now(),
	)
	require.NoError(t, err)
	response, err := directClient.Get(target.URL)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, int32(1), directHits.Load())

	blockedProxy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = blockedProxy.Close() })
	proxyURL := &url.URL{
		Scheme: "http",
		Host:   blockedProxy.Addr().String(),
		User:   url.UserPassword("template_zone_US_sid_1_time_10", "secret"),
	}
	proxyClient, err := resolveOpenCodeGoIdentityHTTPClient(
		53,
		"identity-local-fail-closed",
		dto.ChannelSettings{Proxy: proxyURL.String()},
		&dto.OpenCodeGoConfig{
			IdentityProxyEnabled:       true,
			IdentityProxyCountry:       "US",
			IdentityProxyRotateMinutes: 10,
		},
		time.Now(),
	)
	require.NoError(t, err)
	boundedClient := *proxyClient
	boundedClient.Timeout = 500 * time.Millisecond
	_, err = boundedClient.Get(target.URL)
	require.Error(t, err)
	assert.Equal(t, int32(1), directHits.Load(), "an unavailable enabled proxy must not fall back to the local target")
}

func TestOpenCodeGoIdentityProxyLiveSmoke(t *testing.T) {
	liveProxyURL := strings.TrimSpace(os.Getenv("OPENCODE_GO_LIVE_PROXY_URL"))
	if liveProxyURL == "" {
		t.Skip("OPENCODE_GO_LIVE_PROXY_URL is not configured")
	}
	withOpenCodeGoIdentityProxySecret(t)
	previousCache := openCodeGoIdentityProxyClients
	openCodeGoIdentityProxyClients = newOpenCodeGoIdentityProxyClientCache(8, time.Hour)
	t.Cleanup(func() {
		openCodeGoIdentityProxyClients.reset()
		openCodeGoIdentityProxyClients = previousCache
	})

	type liveObservation struct {
		IP          string `json:"ip"`
		Country     string `json:"country"`
		CountryISO  string `json:"country_iso"`
		CountryCode string `json:"country_code"`
	}
	observationURL := strings.TrimSpace(os.Getenv("OPENCODE_GO_LIVE_OBSERVATION_URL"))
	if observationURL == "" {
		observationURL = "https://ifconfig.co/json"
	}
	exitHash := func(ip string) string {
		digest := sha256.Sum256([]byte(ip))
		return fmt.Sprintf("%x", digest[:6])
	}
	fetchObservation := func(label string, client *http.Client) liveObservation {
		requestContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(requestContext, http.MethodGet, observationURL, nil)
		require.NoError(t, err)
		request.Header.Set("Accept", "application/json")
		request.Header.Set("User-Agent", "new-api-opencode-go-live-smoke")
		response, err := client.Do(request)
		require.NoError(t, err)
		defer response.Body.Close()
		require.Equal(t, http.StatusOK, response.StatusCode)
		observation := liveObservation{}
		require.NoError(t, common.DecodeJson(io.LimitReader(response.Body, 1<<20), &observation))
		require.NotEmpty(t, strings.TrimSpace(observation.IP))
		if observation.CountryISO != "" {
			observation.Country = observation.CountryISO
		} else if observation.CountryCode != "" {
			observation.Country = observation.CountryCode
		}
		observation.Country = strings.ToUpper(strings.TrimSpace(observation.Country))
		t.Logf("%s country=%s exit_hash=%s", label, observation.Country, exitHash(observation.IP))
		return observation
	}

	settings := dto.ChannelSettings{Proxy: liveProxyURL}
	templateClient, err := getOpenCodeGoHTTPClientForSettings(settings)
	require.NoError(t, err)
	_ = fetchObservation("template_static", templateClient)

	config := &dto.OpenCodeGoConfig{
		IdentityProxyEnabled:       true,
		IdentityProxyCountry:       "US",
		IdentityProxyRotateMinutes: 10,
	}
	nowUnix := time.Now().UTC().Unix()
	firstBucketTime := time.Unix(nowUnix-nowUnix%600+1, 0)
	firstClient, err := resolveOpenCodeGoIdentityHTTPClient(91_001, "live-identity-a", settings, config, firstBucketTime)
	require.NoError(t, err)
	sameClient, err := resolveOpenCodeGoIdentityHTTPClient(91_001, "live-identity-a", settings, config, firstBucketTime.Add(30*time.Second))
	require.NoError(t, err)
	otherClient, err := resolveOpenCodeGoIdentityHTTPClient(91_001, "live-identity-b", settings, config, firstBucketTime)
	require.NoError(t, err)
	nextBucketClient, err := resolveOpenCodeGoIdentityHTTPClient(91_001, "live-identity-a", settings, config, firstBucketTime.Add(10*time.Minute))
	require.NoError(t, err)
	if firstClient != sameClient {
		t.Fatal("same identity and rotation bucket did not reuse its client")
	}
	if identityProxyUsernameFromClient(t, firstClient) == identityProxyUsernameFromClient(t, otherClient) {
		t.Fatal("different identities derived the same proxy session")
	}
	if identityProxyUsernameFromClient(t, firstClient) == identityProxyUsernameFromClient(t, nextBucketClient) {
		t.Fatal("the next rotation bucket retained the previous proxy session")
	}

	first := fetchObservation("identity_a_first", firstClient)
	repeated := fetchObservation("identity_a_repeat", sameClient)
	other := fetchObservation("identity_b", otherClient)
	next := fetchObservation("identity_a_next_bucket", nextBucketClient)
	for label, observation := range map[string]liveObservation{
		"identity_a_first":       first,
		"identity_a_repeat":      repeated,
		"identity_b":             other,
		"identity_a_next_bucket": next,
	} {
		if observation.Country != "US" {
			t.Fatalf("%s returned country=%s, expected US", label, observation.Country)
		}
	}
	if first.IP != repeated.IP {
		t.Fatalf(
			"same identity was not sticky (first_hash=%s repeat_hash=%s)",
			exitHash(first.IP),
			exitHash(repeated.IP),
		)
	}
	if first.IP == other.IP {
		t.Log("different identity sessions shared an exit; finite provider-pool collisions are allowed")
	}
	if first.IP == next.IP {
		t.Log("the next bucket shared the previous exit; finite provider-pool collisions are allowed")
	}

	directClient, err := resolveOpenCodeGoIdentityHTTPClient(91_001, "live-direct", dto.ChannelSettings{}, nil, firstBucketTime)
	require.NoError(t, err)
	_ = fetchObservation("direct", directClient)
}

func identityProxyURLFromClient(t *testing.T, client *http.Client) *http.Request {
	t.Helper()
	wrapper, ok := client.Transport.(*openCodeGoChannelRoundTripper)
	require.True(t, ok)
	transport, ok := wrapper.base.(*http.Transport)
	require.True(t, ok)
	request, err := http.NewRequest(http.MethodGet, "http://upstream.example", nil)
	require.NoError(t, err)
	proxyURL, err := transport.Proxy(request)
	require.NoError(t, err)
	require.NotNil(t, proxyURL)
	proxyRequest, err := http.NewRequest(http.MethodGet, proxyURL.String(), nil)
	require.NoError(t, err)
	return proxyRequest
}

func identityProxyUsernameFromClient(t *testing.T, client *http.Client) string {
	t.Helper()
	return identityProxyURLFromClient(t, client).URL.User.Username()
}

func identityProxyCountryFromClient(t *testing.T, client *http.Client) string {
	t.Helper()
	parts := strings.Split(identityProxyUsernameFromClient(t, client), "_")
	for index, part := range parts {
		if strings.EqualFold(part, "zone") && index+1 < len(parts) {
			return parts[index+1]
		}
	}
	return ""
}

func TestOpenCodeGoIdentityProxyCacheIsConcurrentBoundedAndInvalidatable(t *testing.T) {
	cache := newOpenCodeGoIdentityProxyClientCache(2, time.Hour)
	now := time.Unix(1_000, 0)
	var created atomic.Int32
	tracker := &openCodeGoIdentityProxyCloseTracker{}
	factory := func() (*http.Client, error) {
		created.Add(1)
		return &http.Client{Transport: tracker}, nil
	}

	var wait sync.WaitGroup
	clients := make(chan *http.Client, 16)
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			client, err := cache.getOrCreate(now, "key-a", 1, "identity-ref-a", 1, nil, factory)
			require.NoError(t, err)
			clients <- client
		}()
	}
	wait.Wait()
	close(clients)
	var first *http.Client
	for client := range clients {
		if first == nil {
			first = client
		}
		assert.Same(t, first, client)
	}
	assert.Equal(t, int32(1), created.Load())

	secondTracker := &openCodeGoIdentityProxyCloseTracker{}
	_, err := cache.getOrCreate(now, "key-b", 1, "identity-ref-b", 1, nil, func() (*http.Client, error) {
		return &http.Client{Transport: secondTracker}, nil
	})
	require.NoError(t, err)
	thirdTracker := &openCodeGoIdentityProxyCloseTracker{}
	_, err = cache.getOrCreate(now, "key-c", 2, "identity-ref-c", 1, nil, func() (*http.Client, error) {
		return &http.Client{Transport: thirdTracker}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), tracker.closed.Load(), "capacity eviction must close idle connections")

	cache.invalidateChannel(1)
	assert.Equal(t, int32(1), secondTracker.closed.Load())
	cache.reset()
	assert.Equal(t, int32(1), thirdTracker.closed.Load())
}

func TestOpenCodeGoIdentityProxyCacheRejectsStaleGenerationBeforeFactory(t *testing.T) {
	cache := newOpenCodeGoIdentityProxyClientCache(2, time.Hour)
	generation := cache.captureGeneration(17)
	cache.invalidateIdentity(17, "identity-ref")

	var factoryCalls atomic.Int32
	client, err := cache.getOrCreate(
		time.Unix(1_000, 0),
		"stale-key",
		17,
		"identity-ref",
		1,
		&generation,
		func() (*http.Client, error) {
			factoryCalls.Add(1)
			return &http.Client{}, nil
		},
	)

	require.ErrorIs(t, err, ErrOpenCodeGoIdentityProxySelectionStale)
	assert.Nil(t, client)
	assert.Zero(t, factoryCalls.Load())
	assert.Empty(t, cache.entries)
}

func TestOpenCodeGoIdentityProxyGenerationIsBoundToChannel(t *testing.T) {
	cache := newOpenCodeGoIdentityProxyClientCache(2, time.Hour)
	generation := cache.captureGeneration(17)
	var factoryCalls atomic.Int32

	client, err := cache.getOrCreate(
		time.Unix(1_000, 0),
		"wrong-channel-key",
		18,
		"identity-ref",
		1,
		&generation,
		func() (*http.Client, error) {
			factoryCalls.Add(1)
			return &http.Client{}, nil
		},
	)

	require.ErrorIs(t, err, ErrOpenCodeGoIdentityProxySelectionStale)
	assert.Nil(t, client)
	assert.Zero(t, factoryCalls.Load())
}

func TestOpenCodeGoIdentityProxyInvalidationWaitsForFactoryAndRetiresCreatedClient(t *testing.T) {
	cache := newOpenCodeGoIdentityProxyClientCache(2, time.Hour)
	generation := cache.captureGeneration(18)
	factoryStarted := make(chan struct{})
	allowFactory := make(chan struct{})
	lookupDone := make(chan error, 1)
	tracker := &openCodeGoIdentityProxyCloseTracker{}
	go func() {
		_, err := cache.getOrCreate(
			time.Unix(1_000, 0),
			"created-before-invalidation",
			18,
			"identity-ref",
			1,
			&generation,
			func() (*http.Client, error) {
				close(factoryStarted)
				<-allowFactory
				return &http.Client{Transport: tracker}, nil
			},
		)
		lookupDone <- err
	}()
	<-factoryStarted

	invalidationAdvanced := make(chan struct{})
	invalidationDone := make(chan struct{})
	go func() {
		cache.generationMutex.Lock()
		cache.channelGeneration[18]++
		cache.invalidatingChannels[18]++
		cache.generationMutex.Unlock()
		close(invalidationAdvanced)
		cache.mutex.Lock()
		removed := make([]*http.Client, 0)
		for _, entry := range cache.entries {
			if entry.channelID == 18 && entry.identityRef == "identity-ref" {
				removed = append(removed, cache.removeEntryLocked(entry))
			}
		}
		cache.mutex.Unlock()
		cache.generationMutex.Lock()
		cache.invalidatingChannels[18]--
		cache.generationMutex.Unlock()
		closeOpenCodeGoIdentityProxyClients(removed)
		close(invalidationDone)
	}()
	<-invalidationAdvanced
	select {
	case <-invalidationDone:
		t.Fatal("invalidation completed while client creation held the cache mutex")
	default:
	}
	close(allowFactory)
	require.ErrorIs(t, <-lookupDone, ErrOpenCodeGoIdentityProxySelectionStale)
	<-invalidationDone
	assert.Equal(t, int32(1), tracker.closed.Load())
	assert.Empty(t, cache.entries)
}

func TestOpenCodeGoIdentityProxyInvalidationReplacesObsoleteFactoryError(t *testing.T) {
	cache := newOpenCodeGoIdentityProxyClientCache(2, time.Hour)
	generation := cache.captureGeneration(19)
	factoryStarted := make(chan struct{})
	allowFactory := make(chan struct{})
	lookupDone := make(chan error, 1)
	go func() {
		_, err := cache.getOrCreate(
			time.Unix(1_000, 0),
			"failed-before-invalidation",
			19,
			"identity-ref",
			1,
			&generation,
			func() (*http.Client, error) {
				close(factoryStarted)
				<-allowFactory
				return nil, errors.New("obsolete proxy factory failure")
			},
		)
		lookupDone <- err
	}()
	<-factoryStarted

	cache.generationMutex.Lock()
	cache.channelGeneration[19]++
	cache.invalidatingChannels[19]++
	cache.generationMutex.Unlock()
	close(allowFactory)

	err := <-lookupDone
	require.ErrorIs(t, err, ErrOpenCodeGoIdentityProxySelectionStale)
	assert.NotContains(t, err.Error(), "obsolete proxy factory failure")
	cache.generationMutex.Lock()
	cache.invalidatingChannels[19]--
	cache.generationMutex.Unlock()
	assert.Empty(t, cache.entries)
}

func TestOpenCodeGoIdentityProxyConcurrentResetAndInvalidationKeepEpochsAndMarkers(t *testing.T) {
	cache := newOpenCodeGoIdentityProxyClientCache(2, time.Hour)
	oldGeneration := cache.captureGeneration(20)
	cache.mutex.Lock()

	resetDone := make(chan struct{})
	go func() {
		cache.reset()
		close(resetDone)
	}()
	invalidateDone := make(chan struct{})
	go func() {
		cache.invalidateChannel(20)
		close(invalidateDone)
	}()

	require.Eventually(t, func() bool {
		cache.generationMutex.Lock()
		defer cache.generationMutex.Unlock()
		return cache.resetCount == 1 && cache.invalidatingChannels[20] == 1
	}, time.Second, time.Millisecond)
	cache.mutex.Unlock()
	<-resetDone
	<-invalidateDone

	cache.generationMutex.Lock()
	assert.Zero(t, cache.resetCount)
	assert.Zero(t, cache.invalidatingChannels[20])
	assert.Equal(t, uint64(1), cache.globalGeneration)
	assert.Equal(t, uint64(1), cache.channelGeneration[20])
	assert.False(t, cache.generationMatchesLocked(20, &oldGeneration))
	cache.generationMutex.Unlock()
	assert.Empty(t, cache.entries)
	assert.Zero(t, cache.lru.Len())

	newGeneration := cache.captureGeneration(20)
	client, err := cache.getOrCreate(time.Unix(2_000, 0), "fresh-key", 20, "identity-ref", 2, &newGeneration, func() (*http.Client, error) {
		return &http.Client{}, nil
	})
	require.NoError(t, err)
	require.NotNil(t, client)
}

func TestOpenCodeGoIdentityProxyCacheEvictsAtMaxAgeBoundary(t *testing.T) {
	cache := newOpenCodeGoIdentityProxyClientCache(2, time.Hour)
	tracker := &openCodeGoIdentityProxyCloseTracker{}
	createdAt := time.Unix(1_000, 0)
	_, err := cache.getOrCreate(createdAt, "expired-key", 21, "expired-ref", 1, nil, func() (*http.Client, error) {
		return &http.Client{Transport: tracker}, nil
	})
	require.NoError(t, err)

	_, err = cache.getOrCreate(createdAt.Add(time.Hour), "current-key", 22, "current-ref", 1, nil, func() (*http.Client, error) {
		return &http.Client{}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), tracker.closed.Load())
	assert.NotContains(t, cache.entries, "expired-key")
}

func TestOpenCodeGoIdentityProxyCacheRetiresPreviousBucketOnly(t *testing.T) {
	cache := newOpenCodeGoIdentityProxyClientCache(4, time.Hour)
	now := time.Unix(1_000, 0)
	firstChunkWritten := make(chan struct{})
	finishResponse := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "first-")
		writer.(http.Flusher).Flush()
		close(firstChunkWritten)
		<-finishResponse
		_, _ = io.WriteString(writer, "second")
	}))
	t.Cleanup(server.Close)
	firstTracker := &openCodeGoIdentityProxyTrackedTransport{base: http.DefaultTransport.(*http.Transport).Clone()}
	first, err := cache.getOrCreate(now, "bucket-one", 7, "identity-ref", 1, nil, func() (*http.Client, error) {
		return &http.Client{Transport: firstTracker}, nil
	})
	require.NoError(t, err)
	response, err := first.Get(server.URL)
	require.NoError(t, err)
	<-firstChunkWritten

	_, err = cache.getOrCreate(now.Add(time.Minute), "bucket-two", 7, "identity-ref", 2, nil, func() (*http.Client, error) {
		return &http.Client{Transport: &openCodeGoIdentityProxyCloseTracker{}}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), firstTracker.closed.Load())
	close(finishResponse)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, "first-second", string(body), "retiring idle connections must not cancel an active response body")
}

func TestOpenCodeGoImportResultDoesNotSerializeIdentityUID(t *testing.T) {
	payload, err := json.Marshal(OpenCodeGoImportResult{
		Index:          1,
		Status:         "created",
		IdentityUID:    "identity-must-remain-internal",
		WorkspaceCount: 2,
	})
	require.NoError(t, err)
	assert.NotContains(t, string(payload), "identity-must-remain-internal")
	assert.NotContains(t, string(payload), "identity_uid")
}

func TestOpenCodeGoIdentityProxyCacheLateOldBucketCannotReplaceNewBucket(t *testing.T) {
	cache := newOpenCodeGoIdentityProxyClientCache(2, time.Hour)
	now := time.Unix(1_000, 0)
	newTracker := &openCodeGoIdentityProxyCloseTracker{}
	newClient, err := cache.getOrCreate(now, "new-bucket", 7, "identity-ref", 2, nil, func() (*http.Client, error) {
		return &http.Client{Transport: newTracker}, nil
	})
	require.NoError(t, err)

	oldTracker := &openCodeGoIdentityProxyCloseTracker{}
	oldClient, err := cache.getOrCreate(now, "old-bucket", 7, "identity-ref", 1, nil, func() (*http.Client, error) {
		return &http.Client{Transport: oldTracker}, nil
	})
	require.NoError(t, err)
	assert.NotSame(t, newClient, oldClient)
	assert.Zero(t, newTracker.closed.Load(), "a delayed old request must not retire the current bucket")

	currentAgain, err := cache.getOrCreate(now, "new-bucket", 7, "identity-ref", 2, nil, func() (*http.Client, error) {
		t.Fatal("the current bucket client should remain cached")
		return nil, nil
	})
	require.NoError(t, err)
	assert.Same(t, newClient, currentAgain)
	assert.Equal(t, int32(1), oldTracker.closed.Load(), "the next current lookup retires the delayed old bucket")
}

func TestWrapOpenCodeGoProxyHTTPClientRedactsDerivedCredentials(t *testing.T) {
	proxyURL, err := url.Parse("http://derived_user_sid_12345:derived-password@proxy.example:8080")
	require.NoError(t, err)
	client := wrapOpenCodeGoProxyHTTPClient(&http.Client{Transport: openCodeGoRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, errors.New("proxy credentials derived_user_sid_12345 derived-password rejected for identity-raw api-key-raw")
	})}, "http://template_sid_1:template-password@proxy.example:8080", proxyURL, "identity-raw", "12345")
	request, err := http.NewRequest(http.MethodGet, "http://upstream.example", nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer api-key-raw")
	_, err = client.Do(request)
	require.Error(t, err)
	for _, forbidden := range []string{
		"derived_user_sid_12345",
		"derived-password",
		"identity-raw",
		"12345",
		"api-key-raw",
	} {
		assert.NotContains(t, err.Error(), forbidden)
	}
}
