package opencodego

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
			return `{"id":"chat_1","object":"chat.completion","created":1710000000,"model":"vendor/chat-alias","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":210,"completion_tokens":40,"total_tokens":250,"prompt_tokens_details":{"cached_tokens":80,"cached_creation_tokens":30,"cache_write_tokens":30},"completion_tokens_details":{"reasoning_tokens":15}}}`
		case ProtocolMessages:
			return `{"id":"msg_1","type":"message","role":"assistant","model":"vendor/messages-alias","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":100,"cache_read_input_tokens":80,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":10},"output_tokens":40}}`
		case ProtocolResponses:
			return `{"id":"resp_1","object":"response","created_at":1710000000,"model":"vendor/responses-alias","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":210,"output_tokens":40,"total_tokens":250,"input_tokens_details":{"cached_tokens":80,"cached_creation_tokens":30,"cache_write_tokens":30},"output_tokens_details":{"reasoning_tokens":15}}}`
		}
	}

	switch protocol {
	case ProtocolChat:
		return strings.Join([]string{
			`data: {"id":"chat_1","object":"chat.completion.chunk","created":1710000000,"model":"vendor/chat-alias","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`data: {"id":"chat_1","object":"chat.completion.chunk","created":1710000000,"model":"vendor/chat-alias","choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":null}]}`,
			`data: {"id":"chat_1","object":"chat.completion.chunk","created":1710000000,"model":"vendor/chat-alias","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: {"id":"chat_1","object":"chat.completion.chunk","created":1710000000,"model":"vendor/chat-alias","choices":[],"usage":{"prompt_tokens":210,"completion_tokens":40,"total_tokens":250,"prompt_tokens_details":{"cached_tokens":80,"cached_creation_tokens":30,"cache_write_tokens":30},"completion_tokens_details":{"reasoning_tokens":15}}}`,
			`data: {"choices":[],"x-opencode-type":"inference-cost","cost":"0","normalizedUsage":{"inputTokens":100,"outputTokens":40,"reasoningTokens":15,"cacheReadTokens":80,"cacheWrite5mTokens":20,"cacheWrite1hTokens":10}}`,
			`data: [DONE]`,
			``,
		}, "\n")
	case ProtocolMessages:
		return strings.Join([]string{
			`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"vendor/messages-alias","content":[],"usage":{"input_tokens":100,"cache_read_input_tokens":80,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":10},"output_tokens":0}}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":40}}`,
			`data: {"type":"message_stop"}`,
			`data: {"type":"ping","cost":"0","normalizedUsage":{"inputTokens":100,"outputTokens":40,"reasoningTokens":15,"cacheReadTokens":80,"cacheWrite5mTokens":20,"cacheWrite1hTokens":10}}`,
			`data: [DONE]`,
			``,
		}, "\n")
	case ProtocolResponses:
		return strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_1","object":"response","model":"vendor/responses-alias","created_at":1710000000,"status":"in_progress"}}`,
			`data: {"type":"response.output_text.delta","delta":"OK"}`,
			`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"vendor/responses-alias","status":"completed","usage":{"input_tokens":210,"output_tokens":40,"total_tokens":250,"input_tokens_details":{"cached_tokens":80,"cached_creation_tokens":30,"cache_write_tokens":30},"output_tokens_details":{"reasoning_tokens":15}}}}`,
			`data: {"type":"ping","cost":"0","normalizedUsage":{"inputTokens":100,"outputTokens":40,"reasoningTokens":15,"cacheReadTokens":80,"cacheWrite5mTokens":20,"cacheWrite1hTokens":10}}`,
			`data: [DONE]`,
			``,
		}, "\n")
	default:
		panic("unsupported response fixture protocol")
	}
}

func privateErrorResponseFixture(protocol Protocol, stream bool) string {
	privateError := `{"metadata":{"workspace":"wrk_private"},"detail":"Error from provider (Console Go): Endpoint is unavailable."}`
	var payload string
	switch protocol {
	case ProtocolChat:
		payload = `{"error":` + privateError + `}`
	case ProtocolMessages:
		payload = `{"type":"error","error":` + privateError + `}`
	case ProtocolResponses:
		if stream {
			payload = `{"type":"response.failed","response":{"status":"failed","error":` + privateError + `}}`
		} else {
			payload = `{"error":` + privateError + `}`
		}
	default:
		panic("unsupported private error fixture protocol")
	}
	if stream {
		return "data: " + payload + "\n\n"
	}
	return payload
}

func topLevelPrivateErrorResponseFixture(stream bool) string {
	payload := `{"type":"error","message":"Error from provider (Console Go): Endpoint is unavailable.","code":"work\u0073pace_unavailable","metadata":{"work\u0073pace":"wrk\u005fprivate"}}`
	if stream {
		return "data: " + payload + "\n\n"
	}
	return payload
}

func responsesRootPrivateErrorFixture(stream bool) string {
	payload := `{"type":"response.failed","message":"Error from provider (Console Go): Endpoint is unavailable.","code":"work\u0073pace_unavailable","metadata":{"work\u0073pace":"wrk\u005fprivate"}}`
	if stream {
		return "data: " + payload + "\n\n"
	}
	return payload
}

func unknownUpstreamErrorResponseFixture(protocol Protocol, stream bool) string {
	upstreamError := `{"type":"upstream_error","code":"shard_failure","message":"internal shard zen-primary failed"}`
	var payload string
	switch protocol {
	case ProtocolChat:
		payload = `{"error":` + upstreamError + `}`
	case ProtocolMessages:
		payload = `{"type":"error","error":` + upstreamError + `}`
	case ProtocolResponses:
		if stream {
			payload = `{"type":"response.failed","response":{"status":"failed","error":` + upstreamError + `}}`
		} else {
			payload = `{"error":` + upstreamError + `}`
		}
	default:
		panic("unsupported unknown upstream error fixture protocol")
	}
	if stream {
		return "data: " + payload + "\n\n"
	}
	return payload
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

type closeBlockingResponseBody struct {
	closed chan struct{}
	once   sync.Once
}

func newCloseBlockingResponseBody() *closeBlockingResponseBody {
	return &closeBlockingResponseBody{closed: make(chan struct{})}
}

func (b *closeBlockingResponseBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.EOF
}

func (b *closeBlockingResponseBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
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
					want := responseUsageVector{
						input:           100,
						openAIInput:     210,
						cacheRead:       80,
						cacheWriteTotal: 30,
						output:          40,
					}
					if protocol == ProtocolMessages || stream {
						want.cacheWrite5m = 20
						want.cacheWrite1h = 10
					}
					if protocol != ProtocolMessages || stream {
						want.reasoning = 15
					}
					assertFinalResponseUsage(t, protocol, usage, want)
					require.NotNil(t, usage.BillingUsage)
					assert.False(t, usage.BillingUsage.Estimated)
					assert.Equal(t, upstreamModel, info.UpstreamModelName)
					assert.Equal(t, protocol.RelayFormat(), info.FinalRequestRelayFormat)
					output := recorder.Body.String()
					assert.NotEmpty(t, output)
					assert.Contains(t, output, responseTestPublicModel)
					assert.NotContains(t, output, "vendor/")
					assert.NotContains(t, output, "inference-cost")
					assert.NotContains(t, output, `"cost":"0"`)
					assert.NotContains(t, output, "normalizedUsage")
					assert.NotContains(t, output, `"billing_usage"`)
					assert.NotContains(t, output, `"usage_semantic"`)
					assert.NotContains(t, output, `"usage_source"`)
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

func TestAdaptorDoesNotExposePrivateErrorsAcrossProtocolMatrix(t *testing.T) {
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
					c, recorder := responseTestContext(client)
					contentType := "application/json"
					if stream {
						contentType = "text/event-stream"
					}
					resp := &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{contentType}},
						Body:       io.NopCloser(strings.NewReader(privateErrorResponseFixture(protocol, stream))),
					}
					adaptor := &Adaptor{}
					adaptor.Init(info)

					rawUsage, apiErr := adaptor.DoResponse(c, resp, info)

					require.Nil(t, rawUsage)
					require.NotNil(t, apiErr)
					assert.Contains(t, apiErr.Error(), "Console Go")
					assert.Contains(t, apiErr.Error(), "workspace")
					assert.Empty(t, recorder.Body.String())
					publicErr := service.PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, apiErr)
					require.NotSame(t, apiErr, publicErr)
					assert.Equal(t, http.StatusTooManyRequests, publicErr.StatusCode)
					assert.Equal(t, service.OpenCodeGoPublicOverloadMessage, publicErr.Error())
				})
			}
		}
	}
}

func TestAdaptorMarksUnknownUpstreamErrorsAcrossProtocolMatrix(t *testing.T) {
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
					c, recorder := responseTestContext(client)
					contentType := "application/json"
					if stream {
						contentType = "text/event-stream"
					}
					body := unknownUpstreamErrorResponseFixture(protocol, stream)
					if stream {
						body = " \t" + body
					}
					resp := &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{contentType}},
						Body:       io.NopCloser(strings.NewReader(body)),
					}
					adaptor := &Adaptor{}
					adaptor.Init(info)

					rawUsage, apiErr := adaptor.DoResponse(c, resp, info)

					require.Nil(t, rawUsage)
					require.NotNil(t, apiErr)
					assert.Contains(t, apiErr.Error(), "internal shard zen-primary failed")
					assert.True(t, service.IsOpenCodeGoUpstreamRelayError(apiErr))
					assert.Empty(t, recorder.Body.String())
					publicErr := service.PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, apiErr)
					require.NotSame(t, apiErr, publicErr)
					assert.Equal(t, http.StatusTooManyRequests, publicErr.StatusCode)
					assert.Equal(t, service.OpenCodeGoPublicOverloadMessage, publicErr.Error())
				})
			}
		}
	}
}

func TestResponseTransformStateDoesNotTreatAssistantTextAsUpstreamError(t *testing.T) {
	payload := []byte(`{"id":"chat_1","choices":[{"message":{"role":"assistant","content":"internal shard zen-primary failed"}}]}`)
	state := &responseTransformState{protocol: ProtocolChat}

	transformed := state.transformJSON(payload, false)

	assert.JSONEq(t, string(payload), string(transformed))
	assert.False(t, state.sawUpstreamError)
}

func TestResponseTransformStateIgnoresEmptyErrorFields(t *testing.T) {
	for _, protocol := range []Protocol{ProtocolChat, ProtocolMessages, ProtocolResponses} {
		for _, emptyValue := range []string{"null", `{}`, `[]`, `""`, `"   "`} {
			t.Run(string(protocol)+"_"+emptyValue, func(t *testing.T) {
				payload := []byte(`{"id":"response_1","error":` + emptyValue + `,"choices":[]}`)
				state := &responseTransformState{protocol: protocol}

				transformed := state.transformJSON(payload, false)

				assert.JSONEq(t, string(payload), string(transformed))
				assert.False(t, state.sawUpstreamError)
			})
		}
	}
}

func TestResponseTransformStateFailsClosedForExplicitEmptyErrorEvent(t *testing.T) {
	for _, test := range []struct {
		name     string
		protocol Protocol
		payload  string
	}{
		{name: "chat", protocol: ProtocolChat, payload: `{"type":"error","error":{}}`},
		{name: "messages", protocol: ProtocolMessages, payload: `{"type":"error","error":null}`},
		{name: "responses", protocol: ProtocolResponses, payload: `{"type":"response.failed","response":{"error":{}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := &responseTransformState{protocol: test.protocol}

			transformed := state.transformJSON([]byte(test.payload), false)

			assert.True(t, state.sawUpstreamError)
			assert.Contains(t, string(transformed), "upstream_error")
		})
	}
}

func TestAdaptorDoesNotExposeTopLevelPrivateErrorsAcrossProtocolMatrix(t *testing.T) {
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
					c, recorder := responseTestContext(client)
					contentType := "application/json"
					if stream {
						contentType = "text/event-stream"
					}
					resp := &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{contentType}},
						Body:       io.NopCloser(strings.NewReader(topLevelPrivateErrorResponseFixture(stream))),
					}
					adaptor := &Adaptor{}
					adaptor.Init(info)

					rawUsage, apiErr := adaptor.DoResponse(c, resp, info)

					require.Nil(t, rawUsage)
					require.NotNil(t, apiErr)
					assert.Contains(t, apiErr.Error(), "Console Go")
					assert.Contains(t, apiErr.Error(), "workspace")
					assert.Empty(t, recorder.Body.String())
					publicErr := service.PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, apiErr)
					require.NotSame(t, apiErr, publicErr)
					assert.Equal(t, http.StatusTooManyRequests, publicErr.StatusCode)
					assert.Equal(t, service.OpenCodeGoPublicOverloadMessage, publicErr.Error())
				})
			}
		}
	}
}

func TestAdaptorDoesNotExposeResponsesRootPrivateErrorAcrossClientMatrix(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	clients := []types.RelayFormat{
		types.RelayFormatOpenAI,
		types.RelayFormatClaude,
		types.RelayFormatOpenAIResponses,
	}
	for _, stream := range []bool{false, true} {
		for _, client := range clients {
			name := string(client)
			if stream {
				name += "_stream"
			} else {
				name += "_json"
			}
			t.Run(name, func(t *testing.T) {
				info := responseTestInfo(ProtocolResponses, client, stream)
				c, recorder := responseTestContext(client)
				contentType := "application/json"
				if stream {
					contentType = "text/event-stream"
				}
				resp := &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{contentType}},
					Body:       io.NopCloser(strings.NewReader(responsesRootPrivateErrorFixture(stream))),
				}
				adaptor := &Adaptor{}
				adaptor.Init(info)

				rawUsage, apiErr := adaptor.DoResponse(c, resp, info)

				require.Nil(t, rawUsage)
				require.NotNil(t, apiErr)
				assert.Contains(t, apiErr.Error(), "Console Go")
				assert.Contains(t, apiErr.Error(), "workspace")
				assert.Empty(t, recorder.Body.String())
				publicErr := service.PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, apiErr)
				require.NotSame(t, apiErr, publicErr)
				assert.Equal(t, http.StatusTooManyRequests, publicErr.StatusCode)
				assert.Equal(t, service.OpenCodeGoPublicOverloadMessage, publicErr.Error())
			})
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

	run := func(body string, wantIncomplete bool) {
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
		usage, apiErr := adaptor.DoResponse(c, resp, info)
		if wantIncomplete {
			require.Nil(t, usage)
			require.NotNil(t, apiErr)
			assert.True(t, types.IsSkipRetryError(apiErr))
			return
		}
		require.Nil(t, apiErr)
		require.NotNil(t, usage)
	}

	run(strings.Replace(responseFixture(ProtocolChat, true), "data: [DONE]\n", "", 1), true)
	assert.Equal(t, 1, failures)
	assert.Equal(t, 0, successes)
	run(responseFixture(ProtocolChat, true), false)
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

			usage, apiErr := adaptor.DoResponse(c, resp, info)

			require.Nil(t, usage)
			require.NotNil(t, apiErr)
			assert.True(t, types.IsSkipRetryError(apiErr))
			require.NotNil(t, info.StreamStatus)
			assert.True(t, info.StreamStatus.DoneSentinelObserved())
			assert.False(t, info.StreamStatus.ProtocolTerminalObserved())
		})
	}
	assert.Equal(t, len(tests), failures)
	assert.Zero(t, successes)
}

func TestAdaptorRejectsIncompleteUpstreamStreamWithoutFailoverAttempt(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	tests := []struct {
		protocol Protocol
		client   types.RelayFormat
		terminal string
	}{
		{protocol: ProtocolChat, client: types.RelayFormatOpenAI, terminal: "data: [DONE]"},
		{protocol: ProtocolMessages, client: types.RelayFormatClaude, terminal: `"type":"message_stop"`},
		{protocol: ProtocolResponses, client: types.RelayFormatOpenAIResponses, terminal: `"type":"response.completed"`},
	}
	for _, test := range tests {
		t.Run(string(test.protocol), func(t *testing.T) {
			lines := strings.Split(responseFixture(test.protocol, true), "\n")
			filtered := lines[:0]
			for _, line := range lines {
				if !strings.Contains(line, test.terminal) {
					filtered = append(filtered, line)
				}
			}
			info := responseTestInfo(test.protocol, test.client, true)
			c, _ := responseTestContext(test.client)
			adaptor := &Adaptor{}
			adaptor.Init(info)
			adaptor.requestUpstreamStream = true
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(strings.Join(filtered, "\n"))),
			}

			usage, apiErr := adaptor.DoResponse(c, resp, info)

			require.Nil(t, usage)
			require.NotNil(t, apiErr)
			assert.True(t, types.IsSkipRetryError(apiErr))
			assert.Equal(t, types.ErrorCodeBadResponseBody, apiErr.GetErrorCode())
			assert.Equal(t, http.StatusBadGateway, apiErr.StatusCode)
			require.NotNil(t, info.StreamStatus)
			if test.protocol == ProtocolChat {
				assert.False(t, info.StreamStatus.DoneSentinelObserved())
			} else {
				assert.False(t, info.StreamStatus.ProtocolTerminalObserved())
			}
		})
	}
}

func TestAdaptorBufferedNonstreamUpstreamIsNotRejectedAsIncomplete(t *testing.T) {
	info := responseTestInfo(ProtocolChat, types.RelayFormatClaude, true)
	c, _ := responseTestContext(types.RelayFormatClaude)
	adaptor := &Adaptor{}
	adaptor.Init(info)
	adaptor.bufferClaudeToolCall = true
	adaptor.requestUpstreamStream = false
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(responseFixture(ProtocolChat, false))),
	}

	usage, apiErr := adaptor.DoResponse(c, resp, info)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	require.NotNil(t, info.StreamStatus)
	assert.True(t, info.StreamStatus.ProtocolTerminalObserved())
}

func TestAdaptorRejectsCallerCancelledStreamWithoutFailoverObservation(t *testing.T) {
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

	for _, test := range []struct {
		protocol      Protocol
		client        types.RelayFormat
		wantSkipRetry bool
	}{
		{protocol: ProtocolChat, client: types.RelayFormatOpenAI},
		{protocol: ProtocolMessages, client: types.RelayFormatClaude, wantSkipRetry: true},
		{protocol: ProtocolResponses, client: types.RelayFormatOpenAIResponses, wantSkipRetry: true},
	} {
		t.Run(string(test.protocol), func(t *testing.T) {
			info := responseTestInfo(test.protocol, test.client, true)
			c, _ := responseTestContext(test.client)
			requestContext, cancel := context.WithCancel(c.Request.Context())
			c.Request = c.Request.WithContext(requestContext)
			cancel()
			adaptor := &Adaptor{}
			adaptor.Init(info)
			adaptor.requestUpstreamStream = true
			adaptor.failoverAttempt = &service.OpenCodeGoFailoverAttempt{}
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       newCloseBlockingResponseBody(),
			}

			usage, apiErr := adaptor.DoResponse(c, resp, info)

			require.Nil(t, usage)
			require.NotNil(t, apiErr)
			if test.wantSkipRetry {
				assert.True(t, types.IsSkipRetryError(apiErr))
			}
			require.NotNil(t, info.StreamStatus)
			assert.Equal(t, relaycommon.StreamEndReasonClientGone, info.StreamStatus.EndReason)
		})
	}
	assert.Zero(t, failures)
	assert.Zero(t, successes)
}

func TestAdaptorLocalStreamFailureAfterTerminalReturnsSettlementError(t *testing.T) {
	info := responseTestInfo(ProtocolResponses, types.RelayFormatOpenAIResponses, true)
	info.StreamStatus = relaycommon.NewStreamStatus()
	info.StreamStatus.MarkProtocolTerminal()
	info.StreamStatus.MarkLocalFailure()
	info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonPingFail, errors.New("local ping write failed"))
	adaptor := &Adaptor{protocol: ProtocolResponses, requestUpstreamStream: true}
	c, _ := responseTestContext(types.RelayFormatOpenAIResponses)

	apiErr := adaptor.openCodeGoStreamSettlementError(c, info)

	require.NotNil(t, apiErr)
	assert.True(t, types.IsSkipRetryError(apiErr))
	assert.Equal(t, types.ErrorCodeBadResponse, apiErr.GetErrorCode())
	assert.Equal(t, http.StatusInternalServerError, apiErr.StatusCode)
	assert.False(t, adaptor.openCodeGoStreamIncomplete(c, info))
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

type responseUsageVector struct {
	input           int
	cacheRead       int
	cacheWriteTotal int
	cacheWrite5m    int
	cacheWrite1h    int
	output          int
	reasoning       int
	textInput       int
	textOutput      int
	openAIInput     int
	zeroOpenAIInput bool
}

func (v responseUsageVector) cacheWrite() int {
	cacheWrite := v.cacheWrite5m + v.cacheWrite1h
	if v.cacheWriteTotal > cacheWrite {
		return v.cacheWriteTotal
	}
	return cacheWrite
}

func (v responseUsageVector) normalizedInput() int {
	return v.input + v.cacheRead + v.cacheWrite()
}

func (v responseUsageVector) nativeOpenAIInput() int {
	if v.zeroOpenAIInput {
		return 0
	}
	if v.openAIInput > 0 {
		return v.openAIInput
	}
	return v.normalizedInput()
}

func nativeResponseUsage(protocol Protocol, v responseUsageVector) *dto.Usage {
	usage := &dto.Usage{
		CompletionTokens:            v.output,
		OutputTokens:                v.output,
		ClaudeCacheCreation5mTokens: v.cacheWrite5m,
		ClaudeCacheCreation1hTokens: v.cacheWrite1h,
	}
	usage.PromptTokensDetails.CachedTokens = v.cacheRead
	usage.PromptTokensDetails.CachedCreationTokens = v.cacheWrite()
	usage.PromptTokensDetails.CacheWriteTokens = v.cacheWrite()
	usage.PromptTokensDetails.TextTokens = v.textInput
	usage.CompletionTokenDetails.ReasoningTokens = v.reasoning
	usage.CompletionTokenDetails.TextTokens = v.textOutput

	switch protocol {
	case ProtocolMessages:
		usage.PromptTokens = v.input
		usage.InputTokens = v.normalizedInput()
		usage.TotalTokens = v.input + v.output
		usage.UsageSemantic = dto.BillingUsageSemanticAnthropic
		usage.UsageSource = "anthropic"
		usage.BillingUsage = dto.NewClaudeMessagesBillingUsage(&dto.ClaudeUsage{
			InputTokens:                 v.input,
			CacheReadInputTokens:        v.cacheRead,
			CacheCreationInputTokens:    v.cacheWrite(),
			OutputTokens:                v.output,
			ClaudeCacheCreation5mTokens: v.cacheWrite5m,
			ClaudeCacheCreation1hTokens: v.cacheWrite1h,
			CacheCreation: &dto.ClaudeCacheCreationUsage{
				Ephemeral5mInputTokens: v.cacheWrite5m,
				Ephemeral1hInputTokens: v.cacheWrite1h,
			},
		})
	case ProtocolResponses:
		usage.PromptTokens = v.nativeOpenAIInput()
		usage.InputTokens = v.nativeOpenAIInput()
		usage.TotalTokens = v.nativeOpenAIInput() + v.output
		usage.UsageSemantic = dto.BillingUsageSemanticOpenAI
		usage.UsageSource = "openai"
		inputDetails := usage.PromptTokensDetails
		usage.InputTokensDetails = &inputDetails
		usage.BillingUsage = dto.NewOpenAIResponsesBillingUsage(usage)
	default:
		usage.PromptTokens = v.nativeOpenAIInput()
		usage.InputTokens = v.nativeOpenAIInput()
		usage.TotalTokens = v.nativeOpenAIInput() + v.output
		usage.UsageSemantic = dto.BillingUsageSemanticOpenAI
		usage.UsageSource = "openai"
		usage.BillingUsage = dto.NewOpenAIChatBillingUsage(usage)
	}
	return usage
}

func standardUsageEvent(t *testing.T, protocol Protocol, v responseUsageVector) []byte {
	t.Helper()
	var payload any
	switch protocol {
	case ProtocolMessages:
		payload = map[string]any{
			"message": map[string]any{
				"usage": map[string]any{
					"input_tokens":                v.input,
					"cache_read_input_tokens":     v.cacheRead,
					"cache_creation_input_tokens": v.cacheWrite(),
					"output_tokens":               v.output,
					"cache_creation": map[string]any{
						"ephemeral_5m_input_tokens": v.cacheWrite5m,
						"ephemeral_1h_input_tokens": v.cacheWrite1h,
					},
				},
			},
		}
	case ProtocolResponses:
		payload = map[string]any{
			"response": map[string]any{
				"usage": map[string]any{
					"input_tokens":  v.nativeOpenAIInput(),
					"output_tokens": v.output,
					"total_tokens":  v.nativeOpenAIInput() + v.output,
					"input_tokens_details": map[string]any{
						"cached_tokens":          v.cacheRead,
						"cached_creation_tokens": v.cacheWrite(),
						"cache_write_tokens":     v.cacheWrite(),
					},
					"output_tokens_details": map[string]any{
						"reasoning_tokens": v.reasoning,
					},
				},
			},
		}
	default:
		payload = map[string]any{
			"usage": map[string]any{
				"prompt_tokens":     v.nativeOpenAIInput(),
				"completion_tokens": v.output,
				"total_tokens":      v.nativeOpenAIInput() + v.output,
				"prompt_tokens_details": map[string]any{
					"cached_tokens":          v.cacheRead,
					"cached_creation_tokens": v.cacheWrite(),
					"cache_write_tokens":     v.cacheWrite(),
				},
				"completion_tokens_details": map[string]any{
					"reasoning_tokens": v.reasoning,
				},
			},
		}
	}
	encoded, err := common.Marshal(payload)
	require.NoError(t, err)
	return encoded
}

func costUsageEvent(t *testing.T, protocol Protocol, v responseUsageVector) []byte {
	t.Helper()
	payload := map[string]any{
		"type": "ping",
		"cost": "private",
		"normalizedUsage": map[string]any{
			"inputTokens":        v.input,
			"outputTokens":       v.output,
			"reasoningTokens":    v.reasoning,
			"cacheReadTokens":    v.cacheRead,
			"cacheWrite5mTokens": v.cacheWrite5m,
			"cacheWrite1hTokens": v.cacheWrite1h,
		},
	}
	if protocol == ProtocolChat {
		payload["x-opencode-type"] = "inference-cost"
	}
	encoded, err := common.Marshal(payload)
	require.NoError(t, err)
	return encoded
}

func assertFinalResponseUsage(t *testing.T, protocol Protocol, usage *dto.Usage, v responseUsageVector) {
	t.Helper()
	require.NotNil(t, usage)
	wantPrompt := v.nativeOpenAIInput()
	wantTotal := wantPrompt + v.output
	if protocol == ProtocolMessages {
		wantPrompt = v.input
		wantTotal = v.input + v.output
	}
	assert.Equal(t, wantPrompt, usage.PromptTokens)
	assert.Equal(t, v.output, usage.CompletionTokens)
	assert.Equal(t, wantTotal, usage.TotalTokens)
	wantNormalizedInput := v.nativeOpenAIInput()
	if protocol == ProtocolMessages {
		wantNormalizedInput = v.normalizedInput()
	}
	assert.Equal(t, wantNormalizedInput, usage.InputTokens)
	assert.Equal(t, v.output, usage.OutputTokens)
	assert.Equal(t, v.cacheRead, usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, v.cacheWrite(), usage.PromptTokensDetails.CacheCreationTokensTotal())
	assert.Equal(t, v.cacheWrite5m, usage.ClaudeCacheCreation5mTokens)
	assert.Equal(t, v.cacheWrite1h, usage.ClaudeCacheCreation1hTokens)
	assert.Equal(t, v.reasoning, usage.CompletionTokenDetails.ReasoningTokens)
	assert.LessOrEqual(t, usage.CompletionTokenDetails.ReasoningTokens, usage.OutputTokens)

	require.NotNil(t, usage.BillingUsage)
	switch protocol {
	case ProtocolMessages:
		assert.Equal(t, dto.BillingUsageSourceClaudeMessages, usage.BillingUsage.Source)
		assert.Equal(t, dto.BillingUsageSemanticAnthropic, usage.BillingUsage.Semantic)
		require.NotNil(t, usage.BillingUsage.ClaudeUsage)
		assert.Equal(t, v.input, usage.BillingUsage.ClaudeUsage.InputTokens)
		assert.Equal(t, v.cacheRead, usage.BillingUsage.ClaudeUsage.CacheReadInputTokens)
		assert.Equal(t, v.cacheWrite(), usage.BillingUsage.ClaudeUsage.CacheCreationInputTokens)
		assert.Equal(t, v.output, usage.BillingUsage.ClaudeUsage.OutputTokens)
		assert.Equal(t, v.cacheWrite5m, usage.BillingUsage.ClaudeUsage.ClaudeCacheCreation5mTokens)
		assert.Equal(t, v.cacheWrite1h, usage.BillingUsage.ClaudeUsage.ClaudeCacheCreation1hTokens)
	case ProtocolResponses:
		assert.Equal(t, dto.BillingUsageSourceOAIResponses, usage.BillingUsage.Source)
		assert.Equal(t, dto.BillingUsageSemanticOpenAI, usage.BillingUsage.Semantic)
		require.NotNil(t, usage.BillingUsage.OpenAIUsage)
		billing := usage.BillingUsage.OpenAIUsage
		assert.Equal(t, v.nativeOpenAIInput(), billing.InputTokens)
		assert.Equal(t, v.output, billing.OutputTokens)
		assert.Equal(t, v.nativeOpenAIInput()+v.output, billing.TotalTokens)
		assert.Equal(t, v.cacheRead, billing.PromptTokensDetails.CachedTokens)
		assert.Equal(t, v.cacheWrite(), billing.PromptTokensDetails.CacheCreationTokensTotal())
		assert.Equal(t, v.reasoning, billing.CompletionTokenDetails.ReasoningTokens)
		require.NotNil(t, billing.InputTokensDetails)
		assert.Equal(t, v.cacheRead, billing.InputTokensDetails.CachedTokens)
		assert.Equal(t, v.cacheWrite(), billing.InputTokensDetails.CacheCreationTokensTotal())
	default:
		assert.Equal(t, dto.BillingUsageSourceOAIChat, usage.BillingUsage.Source)
		assert.Equal(t, dto.BillingUsageSemanticOpenAI, usage.BillingUsage.Semantic)
		require.NotNil(t, usage.BillingUsage.OpenAIUsage)
		billing := usage.BillingUsage.OpenAIUsage
		assert.Equal(t, v.nativeOpenAIInput(), billing.PromptTokens)
		assert.Equal(t, v.output, billing.CompletionTokens)
		assert.Equal(t, v.nativeOpenAIInput()+v.output, billing.TotalTokens)
		assert.Equal(t, v.cacheRead, billing.PromptTokensDetails.CachedTokens)
		assert.Equal(t, v.cacheWrite(), billing.PromptTokensDetails.CacheCreationTokensTotal())
		assert.Equal(t, v.reasoning, billing.CompletionTokenDetails.ReasoningTokens)
	}
}

func TestFinalizeResponseUsageDoesNotDoubleCountOpenAICache(t *testing.T) {
	state := &responseTransformState{protocol: ProtocolChat}
	state.transformJSON([]byte(`{"usage":{"prompt_tokens":7002,"completion_tokens":16,"total_tokens":7018,"prompt_tokens_details":{"cached_tokens":6912}}}`), false)
	parsed := &dto.Usage{PromptTokens: 7002, CompletionTokens: 16, TotalTokens: 7018}
	parsed.PromptTokensDetails.CachedTokens = 6912

	usage := finalizeResponseUsage(parsed, state).(*dto.Usage)

	assert.Equal(t, 7002, usage.InputTokens)
	assert.Equal(t, 7018, usage.TotalTokens)
	assert.Equal(t, 6912, usage.PromptTokensDetails.CachedTokens)
	require.NotNil(t, usage.BillingUsage)
	assert.Equal(t, 7002, usage.BillingUsage.OpenAIUsage.InputTokens)
}

func TestFinalizeResponseUsagePreservesLegacyPromptCacheHitTokens(t *testing.T) {
	state := &responseTransformState{protocol: ProtocolChat}
	state.transformJSON([]byte(`{"usage":{"prompt_tokens":210,"completion_tokens":40,"total_tokens":250,"prompt_cache_hit_tokens":80}}`), false)
	parsed := &dto.Usage{
		PromptTokens:         210,
		CompletionTokens:     40,
		TotalTokens:          250,
		PromptCacheHitTokens: 80,
	}

	usage := finalizeResponseUsage(parsed, state).(*dto.Usage)

	assert.Equal(t, 210, usage.InputTokens)
	assert.Equal(t, 80, usage.PromptCacheHitTokens)
	assert.Equal(t, 80, usage.PromptTokensDetails.CachedTokens)
	require.NotNil(t, usage.BillingUsage)
	require.NotNil(t, usage.BillingUsage.OpenAIUsage)
	assert.Equal(t, 80, usage.BillingUsage.OpenAIUsage.PromptTokensDetails.CachedTokens)
}

func TestFinalizeResponseUsageFillsZeroNativeInputFromPositiveStandard(t *testing.T) {
	vector := responseUsageVector{input: 100, openAIInput: 210, cacheRead: 80, cacheWrite5m: 20, cacheWrite1h: 10, output: 40, reasoning: 15}
	for _, protocol := range []Protocol{ProtocolChat, ProtocolMessages, ProtocolResponses} {
		t.Run(string(protocol), func(t *testing.T) {
			state := &responseTransformState{protocol: protocol}
			state.transformJSON(standardUsageEvent(t, protocol, vector), false)
			outer := nativeResponseUsage(protocol, vector)
			require.NotNil(t, outer.BillingUsage)
			switch protocol {
			case ProtocolMessages:
				outer.BillingUsage.ClaudeUsage.InputTokens = 0
				outer.PromptTokens = vector.normalizedInput()
				outer.InputTokens = vector.normalizedInput()
				outer.UsageSemantic = dto.BillingUsageSemanticOpenAI
			case ProtocolResponses:
				outer.BillingUsage.OpenAIUsage.InputTokens = 0
				outer.BillingUsage.OpenAIUsage.PromptTokens = 0
			case ProtocolChat:
				outer.BillingUsage.OpenAIUsage.PromptTokens = 0
				outer.BillingUsage.OpenAIUsage.InputTokens = 0
			}

			usage := finalizeResponseUsage(outer, state).(*dto.Usage)

			assertFinalResponseUsage(t, protocol, usage, vector)
		})
	}
}

func TestFinalizeResponseUsagePreservesMessagesCacheOnlyZeroInput(t *testing.T) {
	state := &responseTransformState{protocol: ProtocolMessages}
	state.transformJSON([]byte(`{"usage":{"input_tokens":0,"cache_read_input_tokens":80,"cache_creation_input_tokens":30,"output_tokens":0}}`), false)
	parsed := &dto.Usage{
		PromptTokens: 110,
		InputTokens:  110,
		BillingUsage: dto.NewClaudeMessagesBillingUsage(&dto.ClaudeUsage{
			CacheReadInputTokens:     80,
			CacheCreationInputTokens: 30,
		}),
	}
	parsed.PromptTokensDetails.CachedTokens = 80
	parsed.PromptTokensDetails.CachedCreationTokens = 30

	usage := finalizeResponseUsage(parsed, state).(*dto.Usage)

	assert.Zero(t, usage.PromptTokens)
	assert.Equal(t, 110, usage.InputTokens)
	require.NotNil(t, usage.BillingUsage)
	require.NotNil(t, usage.BillingUsage.ClaudeUsage)
	assert.Zero(t, usage.BillingUsage.ClaudeUsage.InputTokens)
	assert.Equal(t, 80, usage.BillingUsage.ClaudeUsage.CacheReadInputTokens)
	assert.Equal(t, 30, usage.BillingUsage.ClaudeUsage.CacheCreationInputTokens)
}

func TestCaptureFallbackUsageRejectsLaterNegativeSnapshot(t *testing.T) {
	state := &responseTransformState{protocol: ProtocolChat}
	state.captureFallbackUsage([]byte(`{"normalizedUsage":{"inputTokens":100,"outputTokens":40,"reasoningTokens":15,"cacheReadTokens":80,"cacheWrite5mTokens":20,"cacheWrite1hTokens":10}}`))
	state.captureFallbackUsage([]byte(`{"normalizedUsage":{"inputTokens":-1}}`))

	usage := finalizeResponseUsage(nil, state).(*dto.Usage)

	assertFinalResponseUsage(t, ProtocolChat, usage, responseUsageVector{
		input:        100,
		openAIInput:  210,
		cacheRead:    80,
		cacheWrite5m: 20,
		cacheWrite1h: 10,
		output:       40,
		reasoning:    15,
	})
}

func TestFinalizeResponseUsageTotalOnlyStandardDoesNotBlockCostFallback(t *testing.T) {
	state := &responseTransformState{protocol: ProtocolChat}
	state.transformJSON([]byte(`{"usage":{"total_tokens":999}}`), true)
	transformed := state.transformJSON([]byte(`{"x-opencode-type":"inference-cost","cost":"private","normalizedUsage":{"inputTokens":100,"outputTokens":40,"reasoningTokens":15,"cacheReadTokens":80,"cacheWrite5mTokens":20,"cacheWrite1hTokens":10}}`), true)
	assert.True(t, state.sawStandardUsage)
	assert.False(t, state.sawPositiveStandardUsage)
	assert.NotContains(t, string(transformed), "private")

	usage := finalizeResponseUsage(&dto.Usage{PromptTokens: 999, TotalTokens: 999}, state).(*dto.Usage)

	assertFinalResponseUsage(t, ProtocolChat, usage, responseUsageVector{
		input:        100,
		openAIInput:  210,
		cacheRead:    80,
		cacheWrite5m: 20,
		cacheWrite1h: 10,
		output:       40,
		reasoning:    15,
	})
}

func TestFinalizeResponseUsageUsesNativeBillingUsageAcrossClientConversions(t *testing.T) {
	vector := responseUsageVector{input: 100, openAIInput: 210, cacheRead: 80, cacheWrite5m: 20, cacheWrite1h: 10, output: 40, reasoning: 15}
	for _, protocol := range []Protocol{ProtocolChat, ProtocolMessages, ProtocolResponses} {
		t.Run(string(protocol), func(t *testing.T) {
			state := &responseTransformState{protocol: protocol}
			state.transformJSON(standardUsageEvent(t, protocol, vector), false)
			native := nativeResponseUsage(protocol, vector)
			converted := *native
			converted.BillingUsage = dto.CloneBillingUsage(native.BillingUsage)
			switch protocol {
			case ProtocolMessages:
				converted.PromptTokens = vector.nativeOpenAIInput()
				converted.TotalTokens = vector.nativeOpenAIInput() + vector.output
			case ProtocolChat:
				converted.PromptTokens = vector.input
				converted.InputTokens = vector.input
				converted.TotalTokens = vector.input + vector.output
			case ProtocolResponses:
				converted.InputTokens = 0
				converted.PromptTokens = vector.input
				converted.TotalTokens = vector.input + vector.output
			}

			usage := finalizeResponseUsage(&converted, state).(*dto.Usage)

			assertFinalResponseUsage(t, protocol, usage, vector)
		})
	}
}

func TestFinalizeResponseUsageFillsPartialNativeBillingFromOuterDetails(t *testing.T) {
	full := responseUsageVector{input: 100, openAIInput: 210, cacheRead: 80, cacheWrite5m: 20, cacheWrite1h: 10, output: 40, reasoning: 15}
	partial := responseUsageVector{input: 100, openAIInput: 210}
	for _, protocol := range []Protocol{ProtocolChat, ProtocolMessages, ProtocolResponses} {
		t.Run(string(protocol), func(t *testing.T) {
			state := &responseTransformState{protocol: protocol}
			state.transformJSON(standardUsageEvent(t, protocol, full), false)
			outer := nativeResponseUsage(protocol, full)
			outer.BillingUsage = dto.CloneBillingUsage(nativeResponseUsage(protocol, partial).BillingUsage)
			switch protocol {
			case ProtocolMessages:
				outer.PromptTokens = full.nativeOpenAIInput()
				outer.InputTokens = full.nativeOpenAIInput()
			case ProtocolChat:
				outer.PromptTokens = full.input
				outer.InputTokens = full.input
			case ProtocolResponses:
				outer.PromptTokens = full.input
				outer.InputTokens = full.input
			}

			usage := finalizeResponseUsage(outer, state).(*dto.Usage)

			assertFinalResponseUsage(t, protocol, usage, full)
		})
	}
}

func TestFinalizeResponseUsageReconcilesStandardAndCostByCategory(t *testing.T) {
	fallback := responseUsageVector{input: 100, openAIInput: 210, cacheRead: 80, cacheWrite5m: 20, cacheWrite1h: 10, output: 40, reasoning: 15}
	conflict := responseUsageVector{input: 110, openAIInput: 220, cacheRead: 81, cacheWrite5m: 21, cacheWrite1h: 11, output: 41, reasoning: 16}
	partial := responseUsageVector{input: 110, openAIInput: 110, reasoning: 17}
	zero := responseUsageVector{}

	tests := []struct {
		name          string
		standard      *responseUsageVector
		parsed        *dto.Usage
		cost          bool
		standardFirst bool
		want          responseUsageVector
	}{
		{name: "standard only", standard: &fallback, parsed: nativeResponseUsage(ProtocolChat, fallback), want: fallback},
		{name: "cost only", cost: true, want: fallback},
		{name: "standard before cost", standard: &fallback, parsed: nativeResponseUsage(ProtocolChat, fallback), cost: true, standardFirst: true, want: fallback},
		{name: "cost before standard", standard: &fallback, parsed: nativeResponseUsage(ProtocolChat, fallback), cost: true, want: fallback},
		{name: "positive cost fills zero standard input", standard: &zero, parsed: &dto.Usage{PromptTokens: 999, CompletionTokens: 999, TotalTokens: 1998}, cost: true, standardFirst: true, want: fallback},
		{name: "partial standard is completed by cost", standard: &partial, parsed: nativeResponseUsage(ProtocolChat, partial), cost: true, standardFirst: true, want: responseUsageVector{input: 110, openAIInput: 110, cacheRead: 80, cacheWrite5m: 20, cacheWrite1h: 10, output: 40, reasoning: 17}},
		{name: "positive standard fields win conflicts", standard: &conflict, parsed: nativeResponseUsage(ProtocolChat, conflict), cost: true, standardFirst: true, want: conflict},
	}

	for _, protocol := range []Protocol{ProtocolChat, ProtocolMessages, ProtocolResponses} {
		for _, test := range tests {
			t.Run(string(protocol)+"/"+test.name, func(t *testing.T) {
				state := &responseTransformState{protocol: protocol}
				var parsed *dto.Usage
				if test.standard != nil {
					parsed = nativeResponseUsage(protocol, *test.standard)
					if test.name == "positive cost fills zero standard input" {
						parsed = test.parsed
					}
				}
				standardEvent := func() {
					if test.standard != nil {
						state.transformJSON(standardUsageEvent(t, protocol, *test.standard), true)
					}
				}
				costEvent := func() {
					if !test.cost {
						return
					}
					transformed := state.transformJSON(costUsageEvent(t, protocol, fallback), true)
					assert.NotContains(t, string(transformed), "private")
					assert.NotContains(t, string(transformed), "normalizedUsage")
				}
				if test.standardFirst {
					standardEvent()
					costEvent()
				} else {
					costEvent()
					standardEvent()
				}

				usage := finalizeResponseUsage(parsed, state).(*dto.Usage)

				want := test.want
				if test.cost && protocol != ProtocolMessages {
					// Native Chat/Responses usage does not contain Claude's 5m/1h
					// split fields, so those missing categories come from cost.
					want.cacheWriteTotal = want.cacheWrite()
					want.cacheWrite5m = fallback.cacheWrite5m
					want.cacheWrite1h = fallback.cacheWrite1h
				}
				if test.cost && protocol == ProtocolMessages {
					// Native Messages usage has no reasoning-token detail.
					want.reasoning = fallback.reasoning
				}
				assertFinalResponseUsage(t, protocol, usage, want)
			})
		}
	}
}

func TestFinalizeResponseUsageClampsInvalidTokenDetails(t *testing.T) {
	want := responseUsageVector{
		input:        10,
		openAIInput:  10,
		cacheWrite5m: 5,
		cacheWrite1h: 6,
		output:       5,
		reasoning:    5,
	}
	for _, protocol := range []Protocol{ProtocolChat, ProtocolMessages, ProtocolResponses} {
		t.Run(string(protocol), func(t *testing.T) {
			parsed := &dto.Usage{
				PromptTokens:                10,
				InputTokens:                 10,
				CompletionTokens:            5,
				OutputTokens:                5,
				ClaudeCacheCreation5mTokens: 5,
				ClaudeCacheCreation1hTokens: 6,
			}
			parsed.PromptTokensDetails.CachedTokens = -7
			parsed.PromptTokensDetails.CachedCreationTokens = 4
			parsed.PromptTokensDetails.CacheWriteTokens = 3
			parsed.PromptTokensDetails.TextTokens = -2
			parsed.CompletionTokenDetails.ReasoningTokens = 9
			parsed.CompletionTokenDetails.AudioTokens = -1
			state := &responseTransformState{protocol: protocol, sawPositiveStandardUsage: true}

			usage := finalizeResponseUsage(parsed, state).(*dto.Usage)

			assertFinalResponseUsage(t, protocol, usage, want)
			assert.Zero(t, usage.PromptTokensDetails.TextTokens)
			assert.Zero(t, usage.CompletionTokenDetails.AudioTokens)
		})
	}
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
