package router

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/opencodego"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type relayRouterProtocolCapture struct {
	mutex    sync.Mutex
	requests []relayRouterCapturedRequest
}

func (capture *relayRouterProtocolCapture) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	if err := request.Body.Close(); err != nil {
		return nil, err
	}
	capture.mutex.Lock()
	capture.requests = append(capture.requests, relayRouterCapturedRequest{
		path:     request.URL.Path,
		rawQuery: request.URL.RawQuery,
		header:   request.Header.Clone(),
		body:     append([]byte(nil), body...),
	})
	capture.mutex.Unlock()

	var outbound map[string]any
	if err := common.Unmarshal(body, &outbound); err != nil {
		return nil, err
	}
	stream, _ := outbound["stream"].(bool)
	protocol, err := relayRouterProtocolForPath(request.URL.Path)
	if err != nil {
		return nil, err
	}
	contentType := "application/json"
	if stream {
		contentType = "text/event-stream"
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(relayRouterProtocolResponse(protocol, stream))),
		Request:    request,
	}, nil
}

func (capture *relayRouterProtocolCapture) snapshot() []relayRouterCapturedRequest {
	capture.mutex.Lock()
	defer capture.mutex.Unlock()
	result := make([]relayRouterCapturedRequest, len(capture.requests))
	copy(result, capture.requests)
	return result
}

type relayRouterProtocolObservation struct {
	channelType   int
	channelID     int
	plan          opencodego.RequestPreflightPlan
	planFound     bool
	planLookupErr error
}

// This is a deterministic authenticated E1/E2 matrix. It proves gateway
// routing and the exact physical wire request. A mock 2xx cannot establish E3
// upstream acceptance or E4 model behavior/support.
func TestRelayRouterOpenCodeSuccessfulProtocolCaptureMatrix(t *testing.T) {
	clients := []struct {
		name     string
		path     string
		terminal string
	}{
		{name: "messages", path: "/v1/messages", terminal: `"type":"message_stop"`},
		{name: "chat", path: "/v1/chat/completions", terminal: "[DONE]"},
		{name: "responses", path: "/v1/responses", terminal: `"type":"response.completed"`},
	}
	protocols := []struct {
		protocol opencodego.Protocol
		path     string
	}{
		{protocol: opencodego.ProtocolChat, path: "/zen/go/v1/chat/completions"},
		{protocol: opencodego.ProtocolMessages, path: "/zen/go/v1/messages"},
		{protocol: opencodego.ProtocolResponses, path: "/zen/go/v1/responses"},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		channelType := channelType
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			setupRelayRouterTestDB(t)
			user, _, channel, workspaceUID := setupRelayRouterOpenCodePreflightFixture(t, channelType)
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", user.Id).Update("quota", 1_000_000_000).Error)
			capture := installRelayRouterProtocolCapture(t)
			var observation relayRouterProtocolObservation
			engine := gin.New()
			engine.Use(middleware.RequestId())
			engine.Use(func(c *gin.Context) {
				c.Next()
				observation.channelType = common.GetContextKeyInt(c, constant.ContextKeyChannelType)
				observation.channelID = common.GetContextKeyInt(c, constant.ContextKeyChannelId)
				observation.plan, observation.planFound, observation.planLookupErr = opencodego.GetRequestPreflightPlan(c)
			})
			SetRelayRouter(engine)

			for _, final := range protocols {
				channel.SetOtherSettings(dto.ChannelOtherSettings{OpenCodeGo: &dto.OpenCodeGoConfig{
					ModelProtocols: map[string]string{"glm-5.2": string(final.protocol)},
				}})
				require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).
					Update("settings", channel.OtherSettings).Error)

				for _, client := range clients {
					for _, stream := range []bool{false, true} {
						name := fmt.Sprintf("%s-to-%s/stream-%t", client.name, final.protocol, stream)
						t.Run(name, func(t *testing.T) {
							callsBefore := len(capture.snapshot())
							observation = relayRouterProtocolObservation{}
							body := relayRouterProtocolMatrixRequest(t, client.path, stream)
							recorder := serveRelayRouterRequest(engine, client.path, body)

							require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
							assert.Equal(t, channelType, observation.channelType)
							assert.Equal(t, channel.Id, observation.channelID)
							require.NoError(t, observation.planLookupErr)
							require.True(t, observation.planFound)
							assert.Equal(t, final.protocol, observation.plan.FinalProtocol)
							assert.Equal(t, opencodego.ProtocolSourceExactOverride, observation.plan.ProtocolSource)

							requests := capture.snapshot()
							require.Len(t, requests, callsBefore+1)
							physical := requests[len(requests)-1]
							assert.Equal(t, final.path, physical.path)
							assert.Equal(t, "application/json", physical.header.Get("Content-Type"))
							assert.Empty(t, physical.header.Get("Content-Encoding"))
							if stream {
								assert.Equal(t, "text/event-stream", physical.header.Get("Accept"))
								assert.Contains(t, recorder.Body.String(), client.terminal)
							} else {
								assert.Equal(t, "application/json", physical.header.Get("Accept"))
								assert.Contains(t, recorder.Body.String(), "OK")
							}
							if final.protocol == opencodego.ProtocolMessages {
								assert.NotEmpty(t, physical.header.Get("x-api-key"))
								assert.Empty(t, physical.header.Get("Authorization"))
							} else {
								assert.NotEmpty(t, physical.header.Get("Authorization"))
								assert.Empty(t, physical.header.Get("x-api-key"))
							}
							assert.NotEmpty(t, physical.header.Get("X-OpenCode-Session"))

							var outbound map[string]any
							require.NoError(t, common.Unmarshal(physical.body, &outbound))
							assert.Equal(t, "glm-5.2", outbound["model"])
							assert.Equal(t, stream, outbound["stream"])
							switch final.protocol {
							case opencodego.ProtocolChat, opencodego.ProtocolMessages:
								assert.NotNil(t, outbound["messages"])
							case opencodego.ProtocolResponses:
								assert.NotNil(t, outbound["input"])
							}
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

func installRelayRouterProtocolCapture(t *testing.T) *relayRouterProtocolCapture {
	t.Helper()
	service.InitHttpClient()
	client := service.GetHttpClient()
	require.NotNil(t, client)
	previousTransport := client.Transport
	capture := &relayRouterProtocolCapture{}
	client.Transport = capture
	service.ResetProxyClientCache()
	t.Cleanup(func() {
		client.Transport = previousTransport
		service.ResetProxyClientCache()
	})
	return capture
}

func relayRouterProtocolMatrixRequest(t *testing.T, path string, stream bool) []byte {
	t.Helper()
	body := map[string]any{"model": "glm-5.2", "stream": stream}
	switch path {
	case "/v1/messages":
		body["max_tokens"] = 16
		body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
	case "/v1/chat/completions":
		body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
	case "/v1/responses":
		body["input"] = "hello"
	default:
		t.Fatalf("unsupported protocol matrix path %q", path)
	}
	encoded, err := common.Marshal(body)
	require.NoError(t, err)
	return encoded
}

func relayRouterProtocolForPath(path string) (opencodego.Protocol, error) {
	switch path {
	case "/zen/go/v1/chat/completions":
		return opencodego.ProtocolChat, nil
	case "/zen/go/v1/messages":
		return opencodego.ProtocolMessages, nil
	case "/zen/go/v1/responses":
		return opencodego.ProtocolResponses, nil
	default:
		return "", fmt.Errorf("unexpected OpenCode protocol capture path %q", path)
	}
}

func relayRouterProtocolResponse(protocol opencodego.Protocol, stream bool) string {
	if !stream {
		switch protocol {
		case opencodego.ProtocolChat:
			return `{"id":"chat_matrix","object":"chat.completion","created":1,"model":"glm-5.2","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
		case opencodego.ProtocolMessages:
			return `{"id":"msg_matrix","type":"message","role":"assistant","model":"glm-5.2","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		case opencodego.ProtocolResponses:
			return `{"id":"resp_matrix","object":"response","created_at":1,"model":"glm-5.2","status":"completed","output":[{"id":"msg_matrix","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
		}
	}

	var events []string
	switch protocol {
	case opencodego.ProtocolChat:
		events = []string{
			`data: {"id":"chat_matrix","object":"chat.completion.chunk","created":1,"model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","content":"OK"},"finish_reason":null}]}`,
			`data: {"id":"chat_matrix","object":"chat.completion.chunk","created":1,"model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			`data: [DONE]`,
		}
	case opencodego.ProtocolMessages:
		events = []string{
			`data: {"type":"message_start","message":{"id":"msg_matrix","type":"message","role":"assistant","model":"glm-5.2","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
			`data: {"type":"message_stop"}`,
			`data: [DONE]`,
		}
	case opencodego.ProtocolResponses:
		events = []string{
			`data: {"type":"response.created","response":{"id":"resp_matrix","object":"response","model":"glm-5.2","created_at":1,"status":"in_progress"}}`,
			`data: {"type":"response.output_text.delta","delta":"OK"}`,
			`data: {"type":"response.completed","response":{"id":"resp_matrix","object":"response","model":"glm-5.2","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
			`data: [DONE]`,
		}
	}
	return strings.Join(events, "\n\n") + "\n\n"
}

var _ http.RoundTripper = (*relayRouterProtocolCapture)(nil)
