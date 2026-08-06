package opencodego

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const responseTestPublicModel = "public-model"

func responseFixture(protocol Protocol, stream bool) string {
	if !stream {
		switch protocol {
		case ProtocolChat:
			return `{"id":"chat_1","object":"chat.completion","created":1710000000,"model":"vendor/chat-alias","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`
		case ProtocolMessages:
			return `{"id":"msg_1","type":"message","role":"assistant","model":"vendor/messages-alias","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`
		case ProtocolResponses:
			return `{"id":"resp_1","object":"response","created_at":1710000000,"model":"vendor/responses-alias","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`
		}
	}

	switch protocol {
	case ProtocolChat:
		return strings.Join([]string{
			`data: {"id":"chat_1","object":"chat.completion.chunk","created":1710000000,"model":"vendor/chat-alias","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`data: {"id":"chat_1","object":"chat.completion.chunk","created":1710000000,"model":"vendor/chat-alias","choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":null}]}`,
			`data: {"id":"chat_1","object":"chat.completion.chunk","created":1710000000,"model":"vendor/chat-alias","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: {"id":"chat_1","object":"chat.completion.chunk","created":1710000000,"model":"vendor/chat-alias","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`,
			`data: {"choices":[],"x-opencode-type":"inference-cost","cost":"0","normalizedUsage":{"inputTokens":2,"outputTokens":1}}`,
			`data: [DONE]`,
			``,
		}, "\n")
	case ProtocolMessages:
		return strings.Join([]string{
			`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"vendor/messages-alias","content":[],"usage":{"input_tokens":2,"output_tokens":0}}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
			`data: {"type":"message_stop"}`,
			`data: {"type":"ping","cost":"0","normalizedUsage":{"inputTokens":2,"outputTokens":1}}`,
			`data: [DONE]`,
			``,
		}, "\n")
	case ProtocolResponses:
		return strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_1","object":"response","model":"vendor/responses-alias","created_at":1710000000,"status":"in_progress"}}`,
			`data: {"type":"response.output_text.delta","delta":"OK"}`,
			`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"vendor/responses-alias","status":"completed","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`,
			`data: {"type":"ping","cost":"0","normalizedUsage":{"inputTokens":2,"outputTokens":1}}`,
			`data: [DONE]`,
			``,
		}, "\n")
	default:
		panic("unsupported response fixture protocol")
	}
}

func responseTestInfo(protocol Protocol, client types.RelayFormat, stream bool) *relaycommon.RelayInfo {
	model := map[Protocol]string{
		ProtocolChat:      "glm-5.2",
		ProtocolMessages:  "minimax-m3",
		ProtocolResponses: "gpt-5.6-luna",
	}[protocol]
	relayMode := relayconstant.RelayModeChatCompletions
	if client == types.RelayFormatOpenAIResponses {
		relayMode = relayconstant.RelayModeResponses
	}
	info := &relaycommon.RelayInfo{
		OriginModelName:    responseTestPublicModel,
		IsStream:           stream,
		RelayMode:          relayMode,
		RelayFormat:        client,
		ShouldIncludeUsage: true,
		DisablePing:        true,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenCodeGo,
			ApiType:           constant.APITypeOpenCodeGo,
			UpstreamModelName: model,
		},
	}
	if client == types.RelayFormatClaude {
		info.EnsureClaudeConvertInfo()
	}
	return info
}

func responseTestContext(client types.RelayFormat) (*gin.Context, *httptest.ResponseRecorder) {
	path := "/v1/chat/completions"
	if client == types.RelayFormatClaude {
		path = "/v1/messages"
	} else if client == types.RelayFormatOpenAIResponses {
		path = "/v1/responses"
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	c.Set(common.RequestIdKey, "opencode-go-response-test")
	return c, recorder
}

type panicFlushResponseWriter struct {
	gin.ResponseWriter
}

func (w *panicFlushResponseWriter) Flush() {
	panic("local response flush failed")
}

type failAfterDoneResponseWriter struct {
	gin.ResponseWriter
	recorder *httptest.ResponseRecorder
}

func (w *failAfterDoneResponseWriter) Flush() {
	if w.recorder != nil && strings.Contains(w.recorder.Body.String(), "data: [DONE]") {
		panic("local post-terminal flush failed")
	}
	w.ResponseWriter.Flush()
}

func TestAdaptorResponseCompatibilityMatrix(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	protocols := []Protocol{ProtocolChat, ProtocolMessages, ProtocolResponses}
	clients := []types.RelayFormat{
		types.RelayFormatOpenAI,
		types.RelayFormatClaude,
		types.RelayFormatOpenAIResponses,
	}
	for _, stream := range []bool{false, true} {
		for _, protocol := range protocols {
			for _, client := range clients {
				name := string(protocol) + "_to_" + string(client)
				if stream {
					name += "_stream"
				} else {
					name += "_json"
				}
				t.Run(name, func(t *testing.T) {
					info := responseTestInfo(protocol, client, stream)
					upstreamModel := info.UpstreamModelName
					c, recorder := responseTestContext(client)
					contentType := "application/json"
					if stream {
						contentType = "text/event-stream"
					}
					resp := &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{contentType}},
						Body:       io.NopCloser(strings.NewReader(responseFixture(protocol, stream))),
					}
					adaptor := &Adaptor{}
					adaptor.Init(info)

					rawUsage, apiErr := adaptor.DoResponse(c, resp, info)

					require.Nil(t, apiErr)
					usage, ok := rawUsage.(*dto.Usage)
					require.True(t, ok)
					assert.Greater(t, usage.PromptTokens, 0)
					assert.Greater(t, usage.CompletionTokens, 0)
					assert.Equal(t, upstreamModel, info.UpstreamModelName)
					assert.Equal(t, protocol.RelayFormat(), info.FinalRequestRelayFormat)
					output := recorder.Body.String()
					assert.NotEmpty(t, output)
					assert.Contains(t, output, responseTestPublicModel)
					assert.NotContains(t, output, "vendor/")
					assert.NotContains(t, output, "inference-cost")
					assert.NotContains(t, output, `"cost":"0"`)
					if stream {
						require.NotNil(t, info.StreamStatus)
						if protocol == ProtocolChat {
							assert.True(t, info.StreamStatus.DoneSentinelObserved())
						} else {
							assert.True(t, info.StreamStatus.ProtocolTerminalObserved())
						}
					}
				})
			}
		}
	}
}

func TestAdaptorStreamTerminalEventsWithoutDoneSentinel(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	for _, test := range []struct {
		protocol Protocol
		client   types.RelayFormat
		want     bool
	}{
		{protocol: ProtocolChat, client: types.RelayFormatOpenAI, want: false},
		{protocol: ProtocolMessages, client: types.RelayFormatClaude, want: true},
		{protocol: ProtocolResponses, client: types.RelayFormatOpenAIResponses, want: true},
	} {
		t.Run(string(test.protocol), func(t *testing.T) {
			info := responseTestInfo(test.protocol, test.client, true)
			c, _ := responseTestContext(test.client)
			body := strings.Replace(responseFixture(test.protocol, true), "data: [DONE]\n", "", 1)
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}
			adaptor := &Adaptor{}
			adaptor.Init(info)
			_, apiErr := adaptor.DoResponse(c, resp, info)
			require.Nil(t, apiErr)
			require.NotNil(t, info.StreamStatus)
			assert.Equal(t, test.want, info.StreamStatus.ProtocolTerminalObserved())
		})
	}
}

func TestAdaptorRecordsIncompleteStreamAndSuccessSeparately(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	originalFailureObserver := observeOpenCodeGoFailoverFailure
	originalSuccessObserver := observeOpenCodeGoFailoverSuccess
	originalNow := openCodeGoHealthNow
	t.Cleanup(func() {
		observeOpenCodeGoFailoverFailure = originalFailureObserver
		observeOpenCodeGoFailoverSuccess = originalSuccessObserver
		openCodeGoHealthNow = originalNow
	})
	fixedNow := time.Unix(1_900_000_000, 0)
	openCodeGoHealthNow = func() time.Time { return fixedNow }
	failures := 0
	successes := 0
	observeOpenCodeGoFailoverFailure = func(_ *service.OpenCodeGoFailoverAttempt, observedAt time.Time) (service.OpenCodeGoFailoverObservation, error) {
		failures++
		assert.Equal(t, fixedNow, observedAt)
		return service.OpenCodeGoFailoverObservation{Action: service.OpenCodeGoFailoverActionSuspect}, nil
	}
	observeOpenCodeGoFailoverSuccess = func(_ *service.OpenCodeGoFailoverAttempt, observedAt time.Time) (service.OpenCodeGoFailoverObservation, error) {
		successes++
		assert.Equal(t, fixedNow, observedAt)
		return service.OpenCodeGoFailoverObservation{Action: service.OpenCodeGoFailoverActionCleared}, nil
	}

	run := func(body string) {
		info := responseTestInfo(ProtocolChat, types.RelayFormatOpenAI, true)
		c, _ := responseTestContext(types.RelayFormatOpenAI)
		adaptor := &Adaptor{}
		adaptor.Init(info)
		adaptor.requestUpstreamStream = true
		adaptor.failoverAttempt = &service.OpenCodeGoFailoverAttempt{}
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}
		_, apiErr := adaptor.DoResponse(c, resp, info)
		require.Nil(t, apiErr)
	}

	run(strings.Replace(responseFixture(ProtocolChat, true), "data: [DONE]\n", "", 1))
	assert.Equal(t, 1, failures)
	assert.Equal(t, 0, successes)
	run(responseFixture(ProtocolChat, true))
	assert.Equal(t, 1, failures)
	assert.Equal(t, 1, successes)
}

func TestAdaptorDoneSentinelDoesNotCompleteMessagesOrResponses(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	originalFailureObserver := observeOpenCodeGoFailoverFailure
	originalSuccessObserver := observeOpenCodeGoFailoverSuccess
	t.Cleanup(func() {
		observeOpenCodeGoFailoverFailure = originalFailureObserver
		observeOpenCodeGoFailoverSuccess = originalSuccessObserver
	})

	failures := 0
	successes := 0
	observeOpenCodeGoFailoverFailure = func(_ *service.OpenCodeGoFailoverAttempt, _ time.Time) (service.OpenCodeGoFailoverObservation, error) {
		failures++
		return service.OpenCodeGoFailoverObservation{Action: service.OpenCodeGoFailoverActionSuspect}, nil
	}
	observeOpenCodeGoFailoverSuccess = func(_ *service.OpenCodeGoFailoverAttempt, _ time.Time) (service.OpenCodeGoFailoverObservation, error) {
		successes++
		return service.OpenCodeGoFailoverObservation{Action: service.OpenCodeGoFailoverActionCleared}, nil
	}

	tests := []struct {
		protocol Protocol
		client   types.RelayFormat
		event    string
	}{
		{protocol: ProtocolMessages, client: types.RelayFormatClaude, event: `"type":"message_stop"`},
		{protocol: ProtocolResponses, client: types.RelayFormatOpenAIResponses, event: `"type":"response.completed"`},
	}
	for _, test := range tests {
		t.Run(string(test.protocol), func(t *testing.T) {
			lines := strings.Split(responseFixture(test.protocol, true), "\n")
			filtered := lines[:0]
			for _, line := range lines {
				if !strings.Contains(line, test.event) {
					filtered = append(filtered, line)
				}
			}
			info := responseTestInfo(test.protocol, test.client, true)
			c, _ := responseTestContext(test.client)
			adaptor := &Adaptor{}
			adaptor.Init(info)
			adaptor.requestUpstreamStream = true
			adaptor.failoverAttempt = &service.OpenCodeGoFailoverAttempt{}
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(strings.Join(filtered, "\n"))),
			}

			_, apiErr := adaptor.DoResponse(c, resp, info)

			require.Nil(t, apiErr)
			require.NotNil(t, info.StreamStatus)
			assert.True(t, info.StreamStatus.DoneSentinelObserved())
			assert.False(t, info.StreamStatus.ProtocolTerminalObserved())
		})
	}
	assert.Equal(t, len(tests), failures)
	assert.Zero(t, successes)
}

func TestAdaptorTruncatedStreamJSONRecordsOneUpstreamFailure(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	originalFailureObserver := observeOpenCodeGoFailoverFailure
	originalSuccessObserver := observeOpenCodeGoFailoverSuccess
	t.Cleanup(func() {
		observeOpenCodeGoFailoverFailure = originalFailureObserver
		observeOpenCodeGoFailoverSuccess = originalSuccessObserver
	})

	failures := 0
	successes := 0
	observeOpenCodeGoFailoverFailure = func(_ *service.OpenCodeGoFailoverAttempt, _ time.Time) (service.OpenCodeGoFailoverObservation, error) {
		failures++
		return service.OpenCodeGoFailoverObservation{Action: service.OpenCodeGoFailoverActionSuspect}, nil
	}
	observeOpenCodeGoFailoverSuccess = func(_ *service.OpenCodeGoFailoverAttempt, _ time.Time) (service.OpenCodeGoFailoverObservation, error) {
		successes++
		return service.OpenCodeGoFailoverObservation{Action: service.OpenCodeGoFailoverActionCleared}, nil
	}

	tests := []struct {
		name     string
		protocol Protocol
		client   types.RelayFormat
		body     string
	}{
		{name: "chat to responses", protocol: ProtocolChat, client: types.RelayFormatOpenAIResponses, body: `data: {"id":"chat_truncated"` + "\n"},
		{name: "messages to responses", protocol: ProtocolMessages, client: types.RelayFormatOpenAIResponses, body: `data: {"type":"message_start"` + "\n"},
		{name: "native responses", protocol: ProtocolResponses, client: types.RelayFormatOpenAIResponses, body: `data: {"type":"response.output_text.delta"` + "\n"},
		{name: "responses to chat", protocol: ProtocolResponses, client: types.RelayFormatOpenAI, body: `data: {"type":"response.output_text.delta"` + "\n"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info := responseTestInfo(test.protocol, test.client, true)
			c, _ := responseTestContext(test.client)
			adaptor := &Adaptor{}
			adaptor.Init(info)
			adaptor.requestUpstreamStream = true
			adaptor.failoverAttempt = &service.OpenCodeGoFailoverAttempt{}
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(test.body)),
			}

			_, apiErr := adaptor.DoResponse(c, resp, info)

			require.NotNil(t, apiErr)
			require.NotNil(t, info.StreamStatus)
			assert.True(t, info.StreamStatus.UpstreamFailureObserved())
			assert.Equal(t, index+1, failures)
			assert.Zero(t, successes)
		})
	}
}

func TestAdaptorLocalStreamWriteFailureDoesNotTriggerFailover(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	originalFailureObserver := observeOpenCodeGoFailoverFailure
	originalSuccessObserver := observeOpenCodeGoFailoverSuccess
	t.Cleanup(func() {
		observeOpenCodeGoFailoverFailure = originalFailureObserver
		observeOpenCodeGoFailoverSuccess = originalSuccessObserver
	})

	failures := 0
	successes := 0
	observeOpenCodeGoFailoverFailure = func(_ *service.OpenCodeGoFailoverAttempt, _ time.Time) (service.OpenCodeGoFailoverObservation, error) {
		failures++
		return service.OpenCodeGoFailoverObservation{}, nil
	}
	observeOpenCodeGoFailoverSuccess = func(_ *service.OpenCodeGoFailoverAttempt, _ time.Time) (service.OpenCodeGoFailoverObservation, error) {
		successes++
		return service.OpenCodeGoFailoverObservation{}, nil
	}

	info := responseTestInfo(ProtocolResponses, types.RelayFormatOpenAI, true)
	c, _ := responseTestContext(types.RelayFormatOpenAI)
	c.Writer = &panicFlushResponseWriter{ResponseWriter: c.Writer}
	adaptor := &Adaptor{}
	adaptor.Init(info)
	adaptor.requestUpstreamStream = true
	adaptor.failoverAttempt = &service.OpenCodeGoFailoverAttempt{}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(responseFixture(ProtocolResponses, true))),
	}

	_, apiErr := adaptor.DoResponse(c, resp, info)

	require.NotNil(t, apiErr)
	require.NotNil(t, info.StreamStatus)
	assert.False(t, info.StreamStatus.UpstreamFailureObserved())
	assert.Zero(t, failures)
	assert.Zero(t, successes)
}

func TestAdaptorPostTerminalDoneWriteFailureSuppressesSuccess(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	originalFailureObserver := observeOpenCodeGoFailoverFailure
	originalSuccessObserver := observeOpenCodeGoFailoverSuccess
	t.Cleanup(func() {
		observeOpenCodeGoFailoverFailure = originalFailureObserver
		observeOpenCodeGoFailoverSuccess = originalSuccessObserver
	})
	failures := 0
	successes := 0
	observeOpenCodeGoFailoverFailure = func(_ *service.OpenCodeGoFailoverAttempt, _ time.Time) (service.OpenCodeGoFailoverObservation, error) {
		failures++
		return service.OpenCodeGoFailoverObservation{}, nil
	}
	observeOpenCodeGoFailoverSuccess = func(_ *service.OpenCodeGoFailoverAttempt, _ time.Time) (service.OpenCodeGoFailoverObservation, error) {
		successes++
		return service.OpenCodeGoFailoverObservation{}, nil
	}

	info := responseTestInfo(ProtocolResponses, types.RelayFormatOpenAI, true)
	c, recorder := responseTestContext(types.RelayFormatOpenAI)
	c.Writer = &failAfterDoneResponseWriter{ResponseWriter: c.Writer, recorder: recorder}
	adaptor := &Adaptor{}
	adaptor.Init(info)
	adaptor.requestUpstreamStream = true
	adaptor.failoverAttempt = &service.OpenCodeGoFailoverAttempt{}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(responseFixture(ProtocolResponses, true))),
	}

	_, apiErr := adaptor.DoResponse(c, resp, info)

	require.NotNil(t, apiErr)
	require.NotNil(t, info.StreamStatus)
	assert.True(t, info.StreamStatus.ProtocolTerminalObserved())
	assert.True(t, info.StreamStatus.LocalFailureObserved())
	assert.Zero(t, failures)
	assert.Zero(t, successes)
}

func TestAdaptorChatStructuredErrorProvenance(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	originalFailureObserver := observeOpenCodeGoFailoverFailure
	originalSuccessObserver := observeOpenCodeGoFailoverSuccess
	t.Cleanup(func() {
		observeOpenCodeGoFailoverFailure = originalFailureObserver
		observeOpenCodeGoFailoverSuccess = originalSuccessObserver
	})

	for _, test := range []struct {
		name       string
		errorType  string
		message    string
		status     int
		wantHealth bool
	}{
		{name: "deterministic request error", errorType: "invalid_request_error", message: "invalid tool schema", status: http.StatusOK, wantHealth: false},
		{name: "deterministic request wrapped in 5xx", errorType: "invalid_request_error", message: "invalid tool schema", status: http.StatusInternalServerError, wantHealth: false},
		{name: "transient overload error", errorType: "overloaded_error", message: "try again later", status: http.StatusOK, wantHealth: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			failures := 0
			successes := 0
			observeOpenCodeGoFailoverFailure = func(_ *service.OpenCodeGoFailoverAttempt, _ time.Time) (service.OpenCodeGoFailoverObservation, error) {
				failures++
				return service.OpenCodeGoFailoverObservation{Action: service.OpenCodeGoFailoverActionSuspect}, nil
			}
			observeOpenCodeGoFailoverSuccess = func(_ *service.OpenCodeGoFailoverAttempt, _ time.Time) (service.OpenCodeGoFailoverObservation, error) {
				successes++
				return service.OpenCodeGoFailoverObservation{}, nil
			}

			info := responseTestInfo(ProtocolChat, types.RelayFormatOpenAI, true)
			c, _ := responseTestContext(types.RelayFormatOpenAI)
			adaptor := &Adaptor{}
			adaptor.Init(info)
			adaptor.requestUpstreamStream = true
			adaptor.failoverAttempt = &service.OpenCodeGoFailoverAttempt{}
			body := fmt.Sprintf("data: {\"error\":{\"type\":%q,\"message\":%q}}\ndata: [DONE]\n", test.errorType, test.message)
			resp := &http.Response{
				StatusCode: test.status,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}

			_, apiErr := adaptor.DoResponse(c, resp, info)
			require.NotNil(t, apiErr)
			if test.wantHealth {
				assert.Equal(t, 1, failures)
			} else {
				assert.Zero(t, failures)
			}
			assert.Zero(t, successes)
		})
	}
}

func TestAdaptorMessagesStructuredErrorUsesUpstreamStatus(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	originalFailureObserver := observeOpenCodeGoFailoverFailure
	originalSuccessObserver := observeOpenCodeGoFailoverSuccess
	t.Cleanup(func() {
		observeOpenCodeGoFailoverFailure = originalFailureObserver
		observeOpenCodeGoFailoverSuccess = originalSuccessObserver
	})

	failures := 0
	successes := 0
	observeOpenCodeGoFailoverFailure = func(_ *service.OpenCodeGoFailoverAttempt, _ time.Time) (service.OpenCodeGoFailoverObservation, error) {
		failures++
		return service.OpenCodeGoFailoverObservation{Action: service.OpenCodeGoFailoverActionSuspect}, nil
	}
	observeOpenCodeGoFailoverSuccess = func(_ *service.OpenCodeGoFailoverAttempt, _ time.Time) (service.OpenCodeGoFailoverObservation, error) {
		successes++
		return service.OpenCodeGoFailoverObservation{}, nil
	}

	info := responseTestInfo(ProtocolMessages, types.RelayFormatOpenAIResponses, true)
	c, _ := responseTestContext(types.RelayFormatOpenAIResponses)
	adaptor := &Adaptor{}
	adaptor.Init(info)
	adaptor.requestUpstreamStream = true
	adaptor.failoverAttempt = &service.OpenCodeGoFailoverAttempt{}
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"error","error":{"type":"upstream_error","message":"opaque provider failure"}}`,
			`data: [DONE]`,
			``,
		}, "\n"))),
	}

	_, apiErr := adaptor.DoResponse(c, resp, info)
	require.NotNil(t, apiErr)
	assert.Equal(t, 1, failures, "the actual upstream status must participate in stream-error classification")
	assert.Zero(t, successes)
}

func TestChatCostExtensionBecomesUsageFallback(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"chat_1","object":"chat.completion.chunk","created":1,"model":"vendor/chat-alias","choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"x-opencode-type":"inference-cost","cost":"0","normalizedUsage":{"inputTokens":12,"outputTokens":3,"reasoningTokens":2,"cacheReadTokens":88}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	state := &responseTransformState{model: responseTestPublicModel, protocol: ProtocolChat}
	resp := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}
	require.NoError(t, prepareResponseForRelay(resp, state, true))
	transformed, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	output := string(transformed)
	assert.NotContains(t, output, "inference-cost")
	assert.Contains(t, output, `"prompt_tokens":100`)
	assert.Contains(t, output, `"cached_tokens":88`)
	usage := finalizeResponseUsage(nil, state).(*dto.Usage)
	assert.Equal(t, 100, usage.PromptTokens)
	assert.Equal(t, 3, usage.CompletionTokens)
	assert.Equal(t, 88, usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, 2, usage.CompletionTokenDetails.ReasoningTokens)
}
