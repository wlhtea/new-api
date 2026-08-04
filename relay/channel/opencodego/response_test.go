package opencodego

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
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
				})
			}
		}
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
