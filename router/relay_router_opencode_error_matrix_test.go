package router

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type relayRouterRawErrorTransport struct {
	mutex    sync.Mutex
	status   int
	requests []relayRouterCapturedRequest
}

func (transport *relayRouterRawErrorTransport) setStatus(status int) {
	transport.mutex.Lock()
	defer transport.mutex.Unlock()
	transport.status = status
}

func (transport *relayRouterRawErrorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	if err := request.Body.Close(); err != nil {
		return nil, err
	}

	transport.mutex.Lock()
	status := transport.status
	transport.requests = append(transport.requests, relayRouterCapturedRequest{
		path:   request.URL.Path,
		header: request.Header.Clone(),
		body:   append([]byte(nil), body...),
	})
	transport.mutex.Unlock()

	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d upstream", status),
		Header: http.Header{
			"Content-Type":          []string{"application/json"},
			"Location":              []string{"http://upstream-private.invalid/resource"},
			"Retry-After":           []string{"workspace=upstream-private"},
			"Server":                []string{"upstream-private-provider"},
			"Set-Cookie":            []string{"session=upstream-private"},
			"Www-Authenticate":      []string{"Bearer upstream-private"},
			"X-Upstream-Request-Id": []string{"upstream-private-request-id"},
		},
		Body:    io.NopCloser(strings.NewReader(`{"error":{"message":"credential=upstream-private workspace=upstream-private endpoint=http://upstream-private.invalid","type":"upstream-private-type","code":"upstream-private-code","param":"upstream-private-param"}}`)),
		Request: request,
	}, nil
}

func (transport *relayRouterRawErrorTransport) callCount() int {
	transport.mutex.Lock()
	defer transport.mutex.Unlock()
	return len(transport.requests)
}

func (transport *relayRouterRawErrorTransport) snapshot() []relayRouterCapturedRequest {
	transport.mutex.Lock()
	defer transport.mutex.Unlock()
	requests := make([]relayRouterCapturedRequest, len(transport.requests))
	copy(requests, transport.requests)
	return requests
}

// This is an authenticated E1/E2 matrix. It proves routing, physical call
// count, and the public error boundary; it is not real-provider capability
// evidence.
func TestRelayRouterOpenCodeRawClientErrorMatrix(t *testing.T) {
	oldRetryRanges := append([]operation_setting.StatusCodeRange(nil), operation_setting.AutomaticRetryStatusCodeRanges...)
	oldDisableRanges := append([]operation_setting.StatusCodeRange(nil), operation_setting.AutomaticDisableStatusCodeRanges...)
	oldDisableEnabled := common.AutomaticDisableChannelEnabled
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 400, End: 422}}
	operation_setting.AutomaticDisableStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 400, End: 422}}
	common.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() {
		operation_setting.AutomaticRetryStatusCodeRanges = oldRetryRanges
		operation_setting.AutomaticDisableStatusCodeRanges = oldDisableRanges
		common.AutomaticDisableChannelEnabled = oldDisableEnabled
	})

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

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		channelType := channelType
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			setupRelayRouterTestDB(t)
			user, token, channel, workspaceUID := setupRelayRouterOpenCodePreflightFixture(t, channelType)
			statusMapping := `{"400":503,"422":503}`
			require.NoError(t, model.DB.Model(&model.Channel{}).
				Where("id = ?", channel.Id).
				Update("status_code_mapping", statusMapping).Error)

			transport := installRelayRouterRawErrorTransport(t)
			var observation relayRouterPreflightObservation
			engine := gin.New()
			engine.Use(middleware.RequestId())
			engine.Use(func(c *gin.Context) {
				c.Next()
				observation.channelType = common.GetContextKeyInt(c, constant.ContextKeyChannelType)
				observation.channelID = common.GetContextKeyInt(c, constant.ContextKeyChannelId)
			})
			SetRelayRouter(engine)

			for _, endpoint := range endpoints {
				for _, stream := range streamStates {
					for _, rawStatus := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
						name := fmt.Sprintf("%s/stream-%s/raw-%d", endpoint.name, stream.name, rawStatus)
						t.Run(name, func(t *testing.T) {
							transport.setStatus(rawStatus)
							before := snapshotRelayRouterPreflightSideEffects(t, user.Id, token.Id, channel.Id, workspaceUID)
							callsBefore := transport.callCount()
							observation = relayRouterPreflightObservation{}

							body := relayRouterRawErrorRequestBody(t, endpoint.path, stream.value)
							recorder := serveRelayRouterRequest(engine, endpoint.path, body)

							require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
							assert.Equal(t, callsBefore+1, transport.callCount())
							assert.Equal(t, channelType, observation.channelType)
							assert.Equal(t, channel.Id, observation.channelID)
							assert.NotEmpty(t, recorder.Header().Get(common.RequestIdKey))
							assert.NotContains(t, recorder.Body.String(), recorder.Header().Get(common.RequestIdKey))
							for _, header := range []string{
								"Location", "Retry-After", "Server", "Set-Cookie",
								"WWW-Authenticate", "X-Upstream-Request-Id",
							} {
								assert.Empty(t, recorder.Header().Values(header), header)
							}
							assert.NotContains(t, strings.ToLower(recorder.Body.String()), "upstream-private")
							assertFixedRelayRouterInvalidRequest(t, endpoint.path, recorder.Body.Bytes())

							requests := transport.snapshot()
							require.Len(t, requests, callsBefore+1)
							captured := requests[len(requests)-1]
							assert.Equal(t, "/zen/go/v1/chat/completions", captured.path)
							assert.Equal(t, "application/json", captured.header.Get("Content-Type"))
							assert.NotEmpty(t, captured.header.Get("Authorization"))
							assert.NotEmpty(t, captured.header.Get("X-OpenCode-Session"))
							if stream.value != nil && *stream.value {
								assert.Equal(t, "text/event-stream", captured.header.Get("Accept"))
							} else {
								assert.Equal(t, "application/json", captured.header.Get("Accept"))
							}
							var outbound map[string]any
							require.NoError(t, common.Unmarshal(captured.body, &outbound))
							assert.Equal(t, "glm-5.2", outbound["model"])
							if stream.value == nil {
								assert.NotEqual(t, true, outbound["stream"])
							} else {
								assert.Equal(t, *stream.value, outbound["stream"])
							}

							require.Eventually(t, func() bool {
								after := snapshotRelayRouterPreflightSideEffects(t, user.Id, token.Id, channel.Id, workspaceUID)
								return after.userQuota == before.userQuota &&
									after.userUsedQuota == before.userUsedQuota &&
									after.tokenRemainQuota == before.tokenRemainQuota &&
									after.tokenUsedQuota == before.tokenUsedQuota
							}, 2*time.Second, 10*time.Millisecond, "pre-consumed quota was not fully refunded")

							after := snapshotRelayRouterPreflightSideEffects(t, user.Id, token.Id, channel.Id, workspaceUID)
							assert.Equal(t, before.userQuota, after.userQuota)
							assert.Equal(t, before.userUsedQuota, after.userUsedQuota)
							assert.Equal(t, before.tokenRemainQuota, after.tokenRemainQuota)
							assert.Equal(t, before.tokenUsedQuota, after.tokenUsedQuota)
							assert.Equal(t, before.channelUsedQuota, after.channelUsedQuota)
							assert.Equal(t, before.channelStatus, after.channelStatus)
							assert.Equal(t, before.abilityEnabled, after.abilityEnabled)
							assert.Equal(t, before.workspaceState, after.workspaceState)
							assert.Equal(t, before.workspaceHealth, after.workspaceHealth)
							assert.Equal(t, before.workspaceHealthAt, after.workspaceHealthAt)
							assert.Equal(t, before.workspaceCooldownUntil, after.workspaceCooldownUntil)
							assert.Equal(t, before.workspaceLastError, after.workspaceLastError)
							assert.Zero(t, after.workspaceInflight)
						})
					}
				}
			}
		})
	}
}

func installRelayRouterRawErrorTransport(t *testing.T) *relayRouterRawErrorTransport {
	t.Helper()
	service.InitHttpClient()
	client := service.GetHttpClient()
	require.NotNil(t, client)
	previousTransport := client.Transport
	transport := &relayRouterRawErrorTransport{}
	client.Transport = transport
	service.ResetProxyClientCache()
	t.Cleanup(func() {
		client.Transport = previousTransport
		service.ResetProxyClientCache()
	})
	return transport
}

func relayRouterRawErrorRequestBody(t *testing.T, path string, stream *bool) []byte {
	t.Helper()
	body := map[string]any{"model": "glm-5.2"}
	switch path {
	case "/v1/messages":
		body["max_tokens"] = 16
		body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
	case "/v1/chat/completions":
		body["max_tokens"] = 16
		body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
	case "/v1/responses":
		body["max_output_tokens"] = 16
		body["input"] = "hello"
	default:
		t.Fatalf("unsupported relay path %q", path)
	}
	if stream != nil {
		body["stream"] = *stream
	}
	encoded, err := common.Marshal(body)
	require.NoError(t, err)
	return encoded
}

func assertFixedRelayRouterInvalidRequest(t *testing.T, path string, body []byte) {
	t.Helper()
	var payload map[string]any
	require.NoError(t, common.Unmarshal(body, &payload))
	if path == "/v1/messages" {
		assert.Equal(t, map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    constant.OpenCodeGoPublicInvalidRequestCode,
				"message": constant.OpenCodeGoPublicInvalidRequestMessage,
			},
		}, payload)
		return
	}
	assert.Equal(t, map[string]any{
		"error": map[string]any{
			"message": constant.OpenCodeGoPublicInvalidRequestMessage,
			"type":    constant.OpenCodeGoPublicInvalidRequestCode,
			"code":    constant.OpenCodeGoPublicInvalidRequestCode,
		},
	}, payload)
}

var _ http.RoundTripper = (*relayRouterRawErrorTransport)(nil)
