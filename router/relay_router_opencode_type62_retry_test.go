package router

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type relayRouterType62RetryTransport struct {
	mutex    sync.Mutex
	statuses []int
	requests []relayRouterCapturedRequest
}

func (transport *relayRouterType62RetryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	if err := request.Body.Close(); err != nil {
		return nil, err
	}

	transport.mutex.Lock()
	callIndex := len(transport.requests)
	transport.requests = append(transport.requests, relayRouterCapturedRequest{
		path:     request.URL.Path,
		rawQuery: request.URL.RawQuery,
		header:   request.Header.Clone(),
		body:     append([]byte(nil), body...),
	})
	status := http.StatusOK
	if callIndex < len(transport.statuses) {
		status = transport.statuses[callIndex]
	}
	transport.mutex.Unlock()

	responseBody := `{"id":"chatcmpl-type62-retry","object":"chat.completion","created":1,"model":"glm-5.2","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	if status != http.StatusOK {
		responseBody = `{"error":{"message":"private upstream failure","type":"server_error","code":"server_error"}}`
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(responseBody)),
		Request:    request,
	}, nil
}

func (transport *relayRouterType62RetryTransport) snapshot() []relayRouterCapturedRequest {
	transport.mutex.Lock()
	defer transport.mutex.Unlock()
	result := make([]relayRouterCapturedRequest, len(transport.requests))
	copy(result, transport.requests)
	return result
}

func installRelayRouterType62RetryTransport(t *testing.T, statuses ...int) *relayRouterType62RetryTransport {
	t.Helper()
	service.InitHttpClient()
	client := service.GetHttpClient()
	require.NotNil(t, client)
	previousTransport := client.Transport
	transport := &relayRouterType62RetryTransport{statuses: append([]int(nil), statuses...)}
	client.Transport = transport
	service.ResetProxyClientCache()
	t.Cleanup(func() {
		client.Transport = previousTransport
		service.ResetProxyClientCache()
	})
	return transport
}

func TestRelayRouterOpenCodeGoImmediateRetryReplaysExactFinalizedBody(t *testing.T) {
	tests := []struct {
		name             string
		statuses         []int
		wantPublicStatus int
		wantCalls        int
	}{
		{
			name:             "503 then success",
			statuses:         []int{http.StatusServiceUnavailable, http.StatusOK},
			wantPublicStatus: http.StatusOK,
			wantCalls:        2,
		},
		{
			name:             "503 then 503",
			statuses:         []int{http.StatusServiceUnavailable, http.StatusServiceUnavailable},
			wantPublicStatus: http.StatusTooManyRequests,
			wantCalls:        2,
		},
		{
			name:             "400 is not retried",
			statuses:         []int{http.StatusBadRequest},
			wantPublicStatus: http.StatusBadRequest,
			wantCalls:        1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setupRelayRouterTestDB(t)
			user, _, channel, workspaceUID := setupRelayRouterOpenCodePreflightFixture(t, constant.ChannelTypeOpenCodeGo)
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", user.Id).Update("quota", 1_000_000_000).Error)
			transport := installRelayRouterType62RetryTransport(t, test.statuses...)
			engine := gin.New()
			engine.Use(middleware.RequestId())
			SetRelayRouter(engine)

			stream := false
			body := relayRouterOpenCodePreflightBody(t, "/v1/chat/completions", "glm-5.2", &stream, false)
			recorder := serveRelayRouterRequest(engine, "/v1/chat/completions", body)

			require.Equal(t, test.wantPublicStatus, recorder.Code, recorder.Body.String())
			requests := transport.snapshot()
			require.Len(t, requests, test.wantCalls)
			assert.Zero(t, service.OpenCodeGoWorkspaceInFlight(channel.Id, workspaceUID))
			if test.wantCalls != 2 {
				return
			}

			first, second := requests[0], requests[1]
			assert.Equal(t, "/zen/go/v1/chat/completions", first.path)
			assert.Equal(t, first.path, second.path)
			require.NotEmpty(t, first.header.Get("Authorization"))
			require.NotEmpty(t, first.header.Get("X-OpenCode-Session"))
			assert.Equal(t, first.header.Get("Authorization"), second.header.Get("Authorization"))
			assert.Equal(t, first.header.Get("X-OpenCode-Session"), second.header.Get("X-OpenCode-Session"))
			require.NotEmpty(t, first.body)
			assert.True(t, bytes.Equal(first.body, second.body), "physical retry changed finalized body")
			assert.JSONEq(t, string(first.body), string(second.body))
		})
	}
}
