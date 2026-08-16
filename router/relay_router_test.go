package router

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/relay/channel/opencodego"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type relayRouterObservedBody struct {
	io.Reader
	reads int
}

func (body *relayRouterObservedBody) Read(p []byte) (int, error) {
	body.reads++
	return body.Reader.Read(p)
}

func (body *relayRouterObservedBody) Close() error {
	return nil
}

func TestRelayRouterAuthenticatesBeforeDecompression(t *testing.T) {
	setupRelayRouterTestDB(t)

	engine := gin.New()
	SetRelayRouter(engine)
	body := &relayRouterObservedBody{Reader: strings.NewReader("not-gzip")}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	request.Body = body
	request.Header.Set("Authorization", "Bearer invalid-relay-token")
	request.Header.Set("Content-Encoding", "GZip")
	request.Header.Set("Content-Type", "application/json")

	engine.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusUnauthorized, recorder.Code, recorder.Body.String())
	require.Zero(t, body.reads)
	require.NotContains(t, recorder.Body.String(), "gzip")
}

func TestListModelsSupportsOpenAIAndGeminiAuthentication(t *testing.T) {
	setupRelayRouterTestDB(t)

	user := model.User{
		Username: "models-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    100,
	}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		UserId:         user.Id,
		Key:            "modelstestkey",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}).Error)

	engine := gin.New()
	SetRelayRouter(engine)

	tests := []struct {
		name           string
		path           string
		headerName     string
		expectedObject string
		expectedField  string
	}{
		{
			name:           "OpenAI bearer token",
			path:           "/v1/models",
			headerName:     "Authorization",
			expectedObject: "list",
			expectedField:  "data",
		},
		{
			name:          "Gemini API key header",
			path:          "/v1/models",
			headerName:    "x-goog-api-key",
			expectedField: "models",
		},
		{
			name:          "Gemini API key query",
			path:          "/v1/models?key=modelstestkey",
			expectedField: "models",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.headerName != "" {
				value := "modelstestkey"
				if test.headerName == "Authorization" {
					value = "Bearer " + value
				}
				request.Header.Set(test.headerName, value)
			}

			engine.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusOK, recorder.Code)
			var payload map[string]any
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
			assert.Contains(t, payload, test.expectedField)
			assert.NotContains(t, payload, "error")
			if test.expectedObject != "" {
				assert.Equal(t, test.expectedObject, payload["object"])
			}
		})
	}
}

func TestRelayRouterValidatesClientBodiesBeforeModelAuthorizationAndDistribution(t *testing.T) {
	setupRelayRouterTestDB(t)

	user := model.User{
		Username: "relay-validation-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    100,
	}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		UserId:             user.Id,
		Key:                "relayvalidationkey",
		Status:             common.TokenStatusEnabled,
		ExpiredTime:        -1,
		UnlimitedQuota:     true,
		ModelLimitsEnabled: true,
		ModelLimits:        "allowed-model",
	}).Error)

	engine := gin.New()
	engine.Use(middleware.RequestId())
	SetRelayRouter(engine)

	tests := []struct {
		name           string
		path           string
		body           string
		rejectedDetail string
	}{
		{
			name:           "messages missing model is 400 instead of token 403",
			path:           "/v1/messages",
			body:           `{"messages":[{"role":"user","content":"hello"}]}`,
			rejectedDetail: "model is required",
		},
		{
			name:           "messages missing role is 400 instead of no-channel 503",
			path:           "/v1/messages",
			body:           `{"model":"allowed-model","messages":[{"content":"hello"}]}`,
			rejectedDetail: "messages[0].role",
		},
		{
			name:           "chat missing content is 400 instead of no-channel 503",
			path:           "/v1/chat/completions",
			body:           `{"model":"allowed-model","messages":[{"role":"user"}]}`,
			rejectedDetail: "messages[0].content",
		},
		{
			name:           "responses unknown type is 400 instead of no-channel 503",
			path:           "/v1/responses",
			body:           `{"model":"allowed-model","input":[{"type":"future_private_item"}]}`,
			rejectedDetail: "input[0].type",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, test.path, bytes.NewBufferString(test.body))
			request.Header.Set("Authorization", "Bearer relayvalidationkey")
			request.Header.Set("Content-Type", "application/json")

			engine.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
			assertFixedRelayRouterInvalidRequest(t, test.path, recorder.Body.Bytes())
			require.NotContains(t, recorder.Body.String(), test.rejectedDetail)
			require.NotContains(t, recorder.Body.String(), "rate_limit_error")
			require.NotContains(t, recorder.Body.String(), "model_not_found")
		})
	}

	var channelCount int64
	require.NoError(t, model.DB.Model(&model.Channel{}).Count(&channelCount).Error)
	require.Zero(t, channelCount)
}

func TestRelayRouterConcurrentClientErrorsHaveNoDistributionSideEffects(t *testing.T) {
	setupRelayRouterTestDB(t)

	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	user := model.User{
		Username: "relay-load-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    1000,
	}
	require.NoError(t, model.DB.Create(&user).Error)
	token := model.Token{
		UserId:         user.Id,
		Key:            "relayloadkey",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}
	require.NoError(t, model.DB.Create(&token).Error)
	baseURL := upstream.URL
	channel := model.Channel{
		Type:    constant.ChannelTypeOpenAI,
		Key:     "mock-upstream-key",
		Status:  common.ChannelStatusEnabled,
		Name:    "relay-load-mock",
		BaseURL: &baseURL,
		Models:  "allowed-model",
		Group:   "default",
	}
	require.NoError(t, model.DB.Create(&channel).Error)
	ability := model.Ability{
		Group:     "default",
		Model:     "allowed-model",
		ChannelId: channel.Id,
		Enabled:   true,
	}
	require.NoError(t, model.DB.Create(&ability).Error)

	engine := gin.New()
	engine.Use(middleware.RequestId())
	SetRelayRouter(engine)
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)

	client := &http.Client{Timeout: 10 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	invalidRequests := []struct {
		path string
		body string
	}{
		{path: "/v1/messages", body: `{"model":"allowed-model","messages":[{"content":"hello"}]}`},
		{path: "/v1/chat/completions", body: `{"model":"allowed-model","messages":[{"role":"user"}]}`},
		{path: "/v1/responses", body: `{"model":"allowed-model","input":[{"type":"unknown-production-item"}]}`},
	}

	type requestResult struct {
		status int
		body   string
		err    error
	}
	for _, concurrency := range []int{5, 16, 32, 64, 96, 128} {
		t.Run(fmt.Sprintf("concurrency_%d", concurrency), func(t *testing.T) {
			start := make(chan struct{})
			results := make(chan requestResult, concurrency)
			var waitGroup sync.WaitGroup
			waitGroup.Add(concurrency)
			for index := 0; index < concurrency; index++ {
				fixture := invalidRequests[index%len(invalidRequests)]
				go func() {
					defer waitGroup.Done()
					<-start
					request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+fixture.path, bytes.NewBufferString(fixture.body))
					if err != nil {
						results <- requestResult{err: err}
						return
					}
					request.Header.Set("Authorization", "Bearer relayloadkey")
					request.Header.Set("Content-Type", "application/json")
					response, err := client.Do(request)
					if err != nil {
						results <- requestResult{err: err}
						return
					}
					body, readErr := io.ReadAll(response.Body)
					closeErr := response.Body.Close()
					if readErr != nil {
						err = readErr
					} else if closeErr != nil {
						err = closeErr
					}
					results <- requestResult{status: response.StatusCode, body: string(body), err: err}
				}()
			}
			close(start)
			waitGroup.Wait()
			close(results)

			for result := range results {
				require.NoError(t, result.err)
				require.Equal(t, http.StatusBadRequest, result.status, result.body)
				require.Contains(t, result.body, "invalid_request_error")
				require.NotContains(t, result.body, "rate_limit_error")
				require.NotContains(t, result.body, "model_not_found")
			}
			require.Zero(t, upstreamCalls.Load())
		})
	}

	var currentUser model.User
	require.NoError(t, model.DB.First(&currentUser, user.Id).Error)
	require.Equal(t, user.Quota, currentUser.Quota)
	require.Equal(t, user.UsedQuota, currentUser.UsedQuota)
	require.Equal(t, user.RequestCount, currentUser.RequestCount)
	require.Equal(t, user.Status, currentUser.Status)

	var currentToken model.Token
	require.NoError(t, model.DB.First(&currentToken, token.Id).Error)
	require.Equal(t, token.RemainQuota, currentToken.RemainQuota)
	require.Equal(t, token.UsedQuota, currentToken.UsedQuota)
	require.Equal(t, token.Status, currentToken.Status)

	var currentChannel model.Channel
	require.NoError(t, model.DB.First(&currentChannel, channel.Id).Error)
	require.Equal(t, channel.UsedQuota, currentChannel.UsedQuota)
	require.Equal(t, channel.Status, currentChannel.Status)

	var currentAbility model.Ability
	require.NoError(t, model.DB.First(&currentAbility, "`group` = ? AND model = ? AND channel_id = ?", ability.Group, ability.Model, ability.ChannelId).Error)
	require.True(t, currentAbility.Enabled)

	var errorLogCount int64
	require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("type = ?", model.LogTypeError).Count(&errorLogCount).Error)
	require.Zero(t, errorLogCount)
	require.Zero(t, upstreamCalls.Load())
}

type relayRouterPreflightObservation struct {
	channelType int
	channelID   int
	rejection   opencodego.RequestPreflightRejection
	rejected    bool
}

type relayRouterCapturedRequest struct {
	path     string
	rawQuery string
	header   http.Header
	body     []byte
}

type relayRouterCaptureTransport struct {
	mutex    sync.Mutex
	requests []relayRouterCapturedRequest
}

func (transport *relayRouterCaptureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	if err := request.Body.Close(); err != nil {
		return nil, err
	}
	transport.mutex.Lock()
	transport.requests = append(transport.requests, relayRouterCapturedRequest{
		path:     request.URL.Path,
		rawQuery: request.URL.RawQuery,
		header:   request.Header.Clone(),
		body:     append([]byte(nil), body...),
	})
	transport.mutex.Unlock()

	var payload map[string]any
	if err := common.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	stream, _ := payload["stream"].(bool)
	contentType := "application/json"
	responseBody := `{"id":"chatcmpl-router-control","object":"chat.completion","created":1,"model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	if stream {
		contentType = "text/event-stream"
		responseBody = "data: {\"id\":\"chatcmpl-router-control\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"chatcmpl-router-control\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
			"data: [DONE]\n\n"
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(responseBody)),
		Request:    request,
	}, nil
}

func (transport *relayRouterCaptureTransport) snapshot() []relayRouterCapturedRequest {
	transport.mutex.Lock()
	defer transport.mutex.Unlock()
	result := make([]relayRouterCapturedRequest, len(transport.requests))
	copy(result, transport.requests)
	return result
}

type relayRouterPreflightSideEffects struct {
	userQuota              int
	userUsedQuota          int
	userRequestCount       int
	tokenRemainQuota       int
	tokenUsedQuota         int
	channelUsedQuota       int64
	channelStatus          int
	channelPollingIndex    int
	abilityEnabled         map[string]bool
	errorLogCount          int64
	workspaceState         string
	workspaceHealth        string
	workspaceHealthAt      int64
	workspaceCooldownUntil int64
	workspaceLastError     string
	workspaceInflight      int64
}

func TestRelayRouterOpenCodeCapabilityPreflightHasMatchedCaptureAndZeroRejectSideEffects(t *testing.T) {
	endpoints := []struct {
		name string
		path string
	}{
		{name: "messages", path: "/v1/messages"},
		{name: "chat", path: "/v1/chat/completions"},
		{name: "responses", path: "/v1/responses"},
	}
	streamStates := []struct {
		name  string
		value *bool
	}{
		{name: "absent"},
		{name: "false", value: common.GetPointer(false)},
		{name: "true", value: common.GetPointer(true)},
	}
	modelCases := []struct {
		name  string
		model string
	}{
		{name: "exact", model: "glm-5.3"},
		{name: "alias", model: "client-glm"},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		channelType := channelType
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			setupRelayRouterTestDB(t)
			user, token, channel, workspaceUID := setupRelayRouterOpenCodePreflightFixture(t, channelType)
			capture := installRelayRouterCaptureTransport(t)

			var observation relayRouterPreflightObservation
			engine := gin.New()
			engine.Use(middleware.RequestId())
			engine.Use(func(c *gin.Context) {
				c.Next()
				observation.channelType = common.GetContextKeyInt(c, constant.ContextKeyChannelType)
				observation.channelID = common.GetContextKeyInt(c, constant.ContextKeyChannelId)
				observation.rejection, observation.rejected = opencodego.GetRequestPreflightRejection(c)
			})
			SetRelayRouter(engine)

			for _, endpoint := range endpoints {
				for _, stream := range streamStates {
					for _, modelCase := range modelCases {
						name := fmt.Sprintf("%s/stream-%s/%s", endpoint.name, stream.name, modelCase.name)
						t.Run(name, func(t *testing.T) {
							before := snapshotRelayRouterPreflightSideEffects(t, user.Id, token.Id, channel.Id, workspaceUID)
							callsBefore := len(capture.snapshot())
							observation = relayRouterPreflightObservation{}

							rejectedBody := relayRouterOpenCodePreflightBody(t, endpoint.path, modelCase.model, stream.value, true)
							recorder := serveRelayRouterRequest(engine, endpoint.path, rejectedBody)

							require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
							assert.Equal(t, "application/json; charset=utf-8", recorder.Header().Get("Content-Type"))
							assert.NotContains(t, recorder.Body.String(), helper.OpenCodeGLM53ThinkingDisabledPublicMessage)
							assertFixedRelayRouterInvalidRequest(t, endpoint.path, recorder.Body.Bytes())
							assert.Equal(t, channelType, observation.channelType)
							assert.Equal(t, channel.Id, observation.channelID)
							require.True(t, observation.rejected)
							assert.Equal(t, helper.OpenCodeGLM53ThinkingDisabledRule, observation.rejection.RuleID)
							assert.Equal(t, helper.OpenCodeCapabilityStage, observation.rejection.StageID)
							assert.Len(t, capture.snapshot(), callsBefore)
							after := snapshotRelayRouterPreflightSideEffects(t, user.Id, token.Id, channel.Id, workspaceUID)
							assert.Equal(t, before, after)

							observation = relayRouterPreflightObservation{}
							controlBody := relayRouterOpenCodePreflightBody(t, endpoint.path, modelCase.model, stream.value, false)
							controlRecorder := serveRelayRouterRequest(engine, endpoint.path, controlBody)
							require.Equal(t, http.StatusOK, controlRecorder.Code, controlRecorder.Body.String())
							assert.False(t, observation.rejected)
							assert.Equal(t, channelType, observation.channelType)
							assert.Equal(t, channel.Id, observation.channelID)

							requests := capture.snapshot()
							require.Len(t, requests, callsBefore+1)
							captured := requests[len(requests)-1]
							assert.Equal(t, "/zen/go/v1/chat/completions", captured.path)
							var outbound map[string]any
							require.NoError(t, common.Unmarshal(captured.body, &outbound))
							assert.Equal(t, "glm-5.3", outbound["model"])
							_, hasThinking := outbound["thinking"]
							assert.False(t, hasThinking)
							if stream.value == nil {
								assert.NotEqual(t, true, outbound["stream"])
							} else {
								assert.Equal(t, *stream.value, outbound["stream"])
							}
							assert.Equal(t, "application/json", captured.header.Get("Content-Type"))
							assert.NotEmpty(t, captured.header.Get("Authorization"))
							if channelType == constant.ChannelTypeOpenCodeGo {
								assert.Zero(t, service.OpenCodeGoWorkspaceInFlight(channel.Id, workspaceUID))
							}
						})
					}
				}
			}
		})
	}
}

func setupRelayRouterOpenCodePreflightFixture(t *testing.T, channelType int) (model.User, model.Token, model.Channel, string) {
	t.Helper()
	previousRatio := ratio_setting.ModelRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"glm-5.2":1,"glm-5.3":0,"client-glm":0}`))
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(previousRatio)) })

	user := model.User{
		Username: "opencode-preflight-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    1000,
	}
	require.NoError(t, model.DB.Create(&user).Error)
	token := model.Token{
		UserId:         user.Id,
		Key:            "opencodepreflightkey",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}
	require.NoError(t, model.DB.Create(&token).Error)
	mapping := `{"client-glm":"glm-5.3"}`
	channelKey := "router-static-api-key"
	if channelType == constant.ChannelTypeOpenCodeGo {
		channelKey = ""
	}
	channel := model.Channel{
		Type:         channelType,
		Key:          channelKey,
		Status:       common.ChannelStatusEnabled,
		Name:         fmt.Sprintf("opencode-preflight-type-%d", channelType),
		Models:       "glm-5.2,glm-5.3,client-glm",
		Group:        "default",
		ModelMapping: &mapping,
	}
	if channelType == constant.ChannelTypeOpenCodeAPIKey {
		channel.Key = "router-static-api-key-a\nrouter-static-api-key-b"
		channel.ChannelInfo = model.ChannelInfo{
			IsMultiKey:           true,
			MultiKeySize:         2,
			MultiKeyMode:         constant.MultiKeyModePolling,
			MultiKeyPollingIndex: 0,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusEnabled,
				1: common.ChannelStatusEnabled,
			},
		}
	}
	require.NoError(t, model.DB.Create(&channel).Error)
	for _, modelName := range []string{"glm-5.2", "glm-5.3", "client-glm"} {
		require.NoError(t, model.DB.Create(&model.Ability{
			Group:     "default",
			Model:     modelName,
			ChannelId: channel.Id,
			Enabled:   true,
		}).Error)
	}

	workspaceUID := ""
	if channelType == constant.ChannelTypeOpenCodeGo {
		workspaceUID = setupRelayRouterOpenCodeWorkspace(t, channel.Id)
	}
	return user, token, channel, workspaceUID
}

func setupRelayRouterOpenCodeWorkspace(t *testing.T, channelID int) string {
	t.Helper()
	previousSecret := common.CryptoSecret
	previousExplicit := common.CryptoSecretExplicitlyConfigured
	common.CryptoSecret = "router-test-explicit-credential-secret"
	common.CryptoSecretExplicitlyConfigured = true
	t.Cleanup(func() {
		common.CryptoSecret = previousSecret
		common.CryptoSecretExplicitlyConfigured = previousExplicit
	})

	codec, err := service.NewOpenCodeGoCredentialCodec(common.CryptoSecret)
	require.NoError(t, err)
	identityUID := "identity-router-preflight"
	cookieCiphertext, err := codec.Encrypt(service.OpenCodeGoCredentialAuthCookie, channelID, identityUID, "router-test-cookie")
	require.NoError(t, err)
	identity := model.OpenCodeGoIdentity{
		UID:                   identityUID,
		ChannelID:             channelID,
		AuthCookieCiphertext:  cookieCiphertext,
		AuthCookieFingerprint: strings.Repeat("a", 64),
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, model.DB.Create(&identity).Error)

	workspaceUID := "workspace-router-preflight"
	apiKeyCiphertext, err := codec.Encrypt(service.OpenCodeGoCredentialAPIKey, channelID, workspaceUID, "router-test-api-key")
	require.NoError(t, err)
	now := time.Now().Unix()
	workspace := model.OpenCodeGoWorkspace{
		UID:                 workspaceUID,
		ChannelID:           channelID,
		IdentityID:          identity.ID,
		UpstreamWorkspaceID: "wrk_ROUTER_PREFLIGHT",
		APIKeyCiphertext:    apiKeyCiphertext,
		APIKeyFingerprint:   strings.Repeat("b", 64),
		CredentialStatus:    model.OpenCodeGoCredentialValid,
		MembershipStatus:    model.OpenCodeGoMembershipActive,
		ManualEnabled:       true,
		EffectiveState:      model.OpenCodeGoStateEligible,
		QuotaSnapshotStatus: model.OpenCodeGoQuotaSnapshotComplete,
		QuotaFetchedAt:      now,
		QuotaNextRefreshAt:  now + 3600,
		QuotaParserVersion:  service.OpenCodeGoSSRParserVersion,
	}
	require.NoError(t, model.DB.Create(&workspace).Error)
	for index, kind := range model.OpenCodeGoQuotaKinds {
		require.NoError(t, model.DB.Create(&model.OpenCodeGoQuotaWindow{
			WorkspaceID:  workspace.ID,
			Kind:         kind,
			UsedPercent:  float64(10 + index),
			ResetSeconds: int64((index + 1) * 3600),
			ResetAt:      now + int64((index+1)*3600),
			FetchedAt:    now,
		}).Error)
	}
	for _, modelName := range []string{"glm-5.2", "glm-5.3"} {
		require.NoError(t, model.DB.Create(&model.OpenCodeGoWorkspaceModel{
			WorkspaceID: workspace.ID,
			Model:       modelName,
			Discovered:  true,
			State:       model.OpenCodeGoModelAvailable,
		}).Error)
	}
	require.NoError(t, service.RebuildOpenCodeGoPoolChannel(channelID))
	return workspaceUID
}

func installRelayRouterCaptureTransport(t *testing.T) *relayRouterCaptureTransport {
	t.Helper()
	service.InitHttpClient()
	client := service.GetHttpClient()
	require.NotNil(t, client)
	previousTransport := client.Transport
	capture := &relayRouterCaptureTransport{}
	client.Transport = capture
	service.ResetProxyClientCache()
	t.Cleanup(func() {
		client.Transport = previousTransport
		service.ResetProxyClientCache()
	})
	return capture
}

func relayRouterOpenCodePreflightBody(t *testing.T, path string, modelName string, stream *bool, disabled bool) []byte {
	t.Helper()
	body := map[string]any{"model": modelName}
	switch path {
	case "/v1/messages":
		body["max_tokens"] = 16
		body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
	case "/v1/chat/completions":
		body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
	case "/v1/responses":
		body["input"] = "hello"
	default:
		t.Fatalf("unsupported relay path %q", path)
	}
	if stream != nil {
		body["stream"] = *stream
	}
	if disabled {
		body["thinking"] = map[string]any{"type": "disabled"}
	}
	encoded, err := common.Marshal(body)
	require.NoError(t, err)
	return encoded
}

func serveRelayRouterRequest(engine *gin.Engine, path string, body []byte) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer opencodepreflightkey")
	request.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(recorder, request)
	return recorder
}

func snapshotRelayRouterPreflightSideEffects(t *testing.T, userID int, tokenID int, channelID int, workspaceUID string) relayRouterPreflightSideEffects {
	t.Helper()
	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	var token model.Token
	require.NoError(t, model.DB.First(&token, tokenID).Error)
	var channel model.Channel
	require.NoError(t, model.DB.First(&channel, channelID).Error)
	var abilities []model.Ability
	require.NoError(t, model.DB.Where("channel_id = ?", channelID).Order("model ASC").Find(&abilities).Error)
	abilityEnabled := make(map[string]bool, len(abilities))
	for _, ability := range abilities {
		abilityEnabled[ability.Model] = ability.Enabled
	}
	var errorLogCount int64
	require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("type = ?", model.LogTypeError).Count(&errorLogCount).Error)
	snapshot := relayRouterPreflightSideEffects{
		userQuota:           user.Quota,
		userUsedQuota:       user.UsedQuota,
		userRequestCount:    user.RequestCount,
		tokenRemainQuota:    token.RemainQuota,
		tokenUsedQuota:      token.UsedQuota,
		channelUsedQuota:    channel.UsedQuota,
		channelStatus:       channel.Status,
		channelPollingIndex: channel.ChannelInfo.MultiKeyPollingIndex,
		abilityEnabled:      abilityEnabled,
		errorLogCount:       errorLogCount,
	}
	if workspaceUID == "" {
		return snapshot
	}
	workspace, err := model.GetOpenCodeGoWorkspace(channelID, workspaceUID)
	require.NoError(t, err)
	require.NotNil(t, workspace)
	snapshot.workspaceState = workspace.EffectiveState
	snapshot.workspaceHealth = workspace.HealthObservation
	snapshot.workspaceHealthAt = workspace.HealthObservedAt
	snapshot.workspaceCooldownUntil = workspace.CooldownUntil
	snapshot.workspaceLastError = workspace.LastError
	snapshot.workspaceInflight = service.OpenCodeGoWorkspaceInFlight(channelID, workspaceUID)
	return snapshot
}

func setupRelayRouterTestDB(t *testing.T) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	originalIsMasterNode := common.IsMasterNode
	originalRedisEnabled := common.RedisEnabled
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalSQLitePath := common.SQLitePath
	originalMainDatabaseType := common.MainDatabaseType()
	originalLogDatabaseType := common.LogDatabaseType()
	originalSQLDSN, hadSQLDSN := os.LookupEnv("SQL_DSN")

	common.IsMasterNode = false
	common.RedisEnabled = false
	common.MemoryCacheEnabled = false
	common.SQLitePath = fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	require.NoError(t, os.Setenv("SQL_DSN", "local"))
	require.NoError(t, model.InitDB())
	model.LOG_DB = model.DB
	require.NoError(t, model.DB.AutoMigrate(
		&model.User{},
		&model.Token{},
		&model.UserSubscription{},
		&model.Ability{},
		&model.Channel{},
		&model.Log{},
		&model.OpenCodeGoIdentity{},
		&model.OpenCodeGoWorkspace{},
		&model.OpenCodeGoQuotaWindow{},
		&model.OpenCodeGoWorkspaceModel{},
	))

	t.Cleanup(func() {
		perfmetrics.WaitForScheduledRelaySamples()
		if sqlDB, err := model.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
		common.IsMasterNode = originalIsMasterNode
		common.RedisEnabled = originalRedisEnabled
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		common.SQLitePath = originalSQLitePath
		common.SetDatabaseTypes(originalMainDatabaseType, originalLogDatabaseType)
		if hadSQLDSN {
			require.NoError(t, os.Setenv("SQL_DSN", originalSQLDSN))
		} else {
			require.NoError(t, os.Unsetenv("SQL_DSN"))
		}
	})
}
