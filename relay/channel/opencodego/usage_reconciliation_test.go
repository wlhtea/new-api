package opencodego

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func reconciliationFallbackVector() responseUsageVector {
	return responseUsageVector{
		input:           100,
		openAIInput:     210,
		cacheRead:       80,
		cacheWriteTotal: 30,
		cacheWrite5m:    20,
		cacheWrite1h:    10,
		output:          40,
		reasoning:       15,
	}
}

func reconciliationOutputOnlyStandardEvent(protocol Protocol) []byte {
	switch protocol {
	case ProtocolMessages:
		return []byte(`{"message":{"usage":{"output_tokens":40}}}`)
	case ProtocolResponses:
		return []byte(`{"response":{"usage":{"output_tokens":40}}}`)
	default:
		return []byte(`{"usage":{"completion_tokens":40}}`)
	}
}

func reconciliationDetailOnlyStandardEvent(protocol Protocol) []byte {
	switch protocol {
	case ProtocolMessages:
		return []byte(`{"message":{"usage":{"cache_read_input_tokens":81,"cache_creation_input_tokens":33,"cache_creation":{"ephemeral_5m_input_tokens":21,"ephemeral_1h_input_tokens":12},"prompt_tokens_details":{"cached_tokens":81,"cached_creation_tokens":33,"cache_write_tokens":32,"text_tokens":71,"audio_tokens":7,"image_tokens":3},"completion_tokens_details":{"text_tokens":20,"audio_tokens":3,"image_tokens":2,"reasoning_tokens":16}}}}`)
	case ProtocolResponses:
		return []byte(`{"response":{"usage":{"input_tokens_details":{"cached_tokens":81,"cached_creation_tokens":33,"cache_write_tokens":32,"text_tokens":71,"audio_tokens":7,"image_tokens":3},"output_tokens_details":{"text_tokens":20,"audio_tokens":3,"image_tokens":2,"reasoning_tokens":16},"claude_cache_creation_5_m_tokens":21,"claude_cache_creation_1_h_tokens":12}}}`)
	default:
		return []byte(`{"usage":{"prompt_tokens_details":{"cached_tokens":81,"cached_creation_tokens":33,"cache_write_tokens":32,"text_tokens":71,"audio_tokens":7,"image_tokens":3},"completion_tokens_details":{"text_tokens":20,"audio_tokens":3,"image_tokens":2,"reasoning_tokens":16},"claude_cache_creation_5_m_tokens":21,"claude_cache_creation_1_h_tokens":12}}`)
	}
}

func reconciliationEstimatedUsage() *dto.Usage {
	return &dto.Usage{
		PromptTokens:     999,
		CompletionTokens: 999,
		TotalTokens:      1998,
		InputTokens:      999,
		OutputTokens:     999,
	}
}

func reconciliationTransformInOrder(
	t *testing.T,
	protocol Protocol,
	standard []byte,
	standardFirst bool,
) *responseTransformState {
	t.Helper()
	state := &responseTransformState{protocol: protocol}
	fallback := costUsageEvent(t, protocol, reconciliationFallbackVector())
	transformStandard := func() {
		transformed := state.transformJSON(standard, true)
		require.NotNil(t, transformed)
	}
	transformFallback := func() {
		transformed := state.transformJSON(fallback, true)
		assert.NotContains(t, string(transformed), "private")
		assert.NotContains(t, string(transformed), "normalizedUsage")
	}
	if standardFirst {
		transformStandard()
		transformFallback()
	} else {
		transformFallback()
		transformStandard()
	}
	return state
}

func TestFinalizeResponseUsageRejectsEstimatedInputWhenStandardOnlyReportsOutput(t *testing.T) {
	for _, protocol := range []Protocol{ProtocolChat, ProtocolMessages, ProtocolResponses} {
		for _, order := range []struct {
			name          string
			standardFirst bool
		}{
			{name: "standard_before_cost", standardFirst: true},
			{name: "cost_before_standard"},
		} {
			t.Run(string(protocol)+"/"+order.name, func(t *testing.T) {
				state := reconciliationTransformInOrder(
					t,
					protocol,
					reconciliationOutputOnlyStandardEvent(protocol),
					order.standardFirst,
				)
				assert.True(t, state.sawStandardUsage)
				assert.Equal(t, responseUsageCategoryPresence{output: true}, state.standardUsageCategories)

				parsed := reconciliationEstimatedUsage()
				parsed.CompletionTokens = 40
				parsed.OutputTokens = 40
				parsed.TotalTokens = 1039
				usage := finalizeResponseUsage(parsed, state).(*dto.Usage)

				assertFinalResponseUsage(t, protocol, usage, reconciliationFallbackVector())
				assert.NotEqual(t, 999, usage.PromptTokens)
				assert.NotEqual(t, 999, usage.InputTokens)
				assert.Equal(t, 40, usage.OutputTokens)
			})
		}
	}
}

func TestFinalizeResponseUsageMarksLocalFallbackAsEstimated(t *testing.T) {
	for _, protocol := range []Protocol{ProtocolChat, ProtocolMessages, ProtocolResponses} {
		t.Run(string(protocol)+"/no_provider_usage", func(t *testing.T) {
			usage := finalizeResponseUsage(reconciliationEstimatedUsage(), &responseTransformState{protocol: protocol}).(*dto.Usage)

			require.NotNil(t, usage.BillingUsage)
			assert.True(t, usage.BillingUsage.Estimated)
			assert.Equal(t, 999, usage.PromptTokens)
			assert.Equal(t, 999, usage.CompletionTokens)
		})

		t.Run(string(protocol)+"/partial_standard_without_cost", func(t *testing.T) {
			state := &responseTransformState{protocol: protocol}
			state.transformJSON(reconciliationOutputOnlyStandardEvent(protocol), true)
			parsed := reconciliationEstimatedUsage()
			parsed.CompletionTokens = 40
			parsed.OutputTokens = 40
			parsed.TotalTokens = 1039

			usage := finalizeResponseUsage(parsed, state).(*dto.Usage)

			require.NotNil(t, usage.BillingUsage)
			assert.True(t, usage.BillingUsage.Estimated)
			assert.Equal(t, 999, usage.PromptTokens)
			assert.Equal(t, 40, usage.CompletionTokens)
		})

		t.Run(string(protocol)+"/missing_input_without_local_estimate", func(t *testing.T) {
			state := &responseTransformState{protocol: protocol}
			state.transformJSON(reconciliationOutputOnlyStandardEvent(protocol), true)
			parsed := &dto.Usage{CompletionTokens: 40, OutputTokens: 40, TotalTokens: 40}

			usage := finalizeResponseUsage(parsed, state).(*dto.Usage)

			require.NotNil(t, usage.BillingUsage)
			assert.True(t, usage.BillingUsage.Estimated)
			assert.Zero(t, usage.PromptTokens)
			assert.Equal(t, 40, usage.CompletionTokens)
		})
	}
}

func TestFinalizeResponseUsageKeepsProviderFallbackTrusted(t *testing.T) {
	for _, protocol := range []Protocol{ProtocolChat, ProtocolMessages, ProtocolResponses} {
		t.Run(string(protocol), func(t *testing.T) {
			state := &responseTransformState{protocol: protocol}
			state.transformJSON(costUsageEvent(t, protocol, reconciliationFallbackVector()), true)

			usage := finalizeResponseUsage(reconciliationEstimatedUsage(), state).(*dto.Usage)

			assertFinalResponseUsage(t, protocol, usage, reconciliationFallbackVector())
			assert.False(t, usage.BillingUsage.Estimated)
		})
	}
}

func TestFinalizeResponseUsageRequiresExplicitProviderInputForExactUsage(t *testing.T) {
	for _, protocol := range []Protocol{ProtocolChat, ProtocolMessages, ProtocolResponses} {
		t.Run(string(protocol)+"/missing_input", func(t *testing.T) {
			state := &responseTransformState{protocol: protocol}
			state.captureFallbackUsage([]byte(`{"normalizedUsage":{"outputTokens":40,"cacheReadTokens":80,"cacheWrite5mTokens":20,"cacheWrite1hTokens":10}}`))

			usage := finalizeResponseUsage(reconciliationEstimatedUsage(), state).(*dto.Usage)

			require.NotNil(t, usage.BillingUsage)
			assert.True(t, usage.BillingUsage.Estimated)
			assert.False(t, state.sawFallbackInputField)
			assert.Equal(t, 999, usage.InputTokens)
			assert.Equal(t, 80, usage.PromptTokensDetails.CachedTokens)
			assert.Equal(t, 30, usage.PromptTokensDetails.CacheCreationTokensTotal())
			if protocol == ProtocolMessages {
				assert.Equal(t, 889, usage.PromptTokens)
				require.NotNil(t, usage.BillingUsage.ClaudeUsage)
				assert.Equal(t, 889, usage.BillingUsage.ClaudeUsage.InputTokens)
			} else {
				assert.Equal(t, 999, usage.PromptTokens)
				require.NotNil(t, usage.BillingUsage.OpenAIUsage)
				assert.Equal(t, 999, usage.BillingUsage.OpenAIUsage.InputTokens)
			}
		})

		t.Run(string(protocol)+"/explicit_zero_input", func(t *testing.T) {
			state := &responseTransformState{protocol: protocol}
			state.captureFallbackUsage([]byte(`{"normalizedUsage":{"inputTokens":0,"cacheReadTokens":80}}`))

			usage := finalizeResponseUsage(nil, state).(*dto.Usage)

			require.NotNil(t, usage.BillingUsage)
			assert.False(t, usage.BillingUsage.Estimated)
			assert.True(t, state.sawFallbackInputField)
			assert.Equal(t, 80, usage.InputTokens)
		})

		t.Run(string(protocol)+"/all_zero_explicit_input", func(t *testing.T) {
			state := &responseTransformState{protocol: protocol, estimatedInputTokens: 999}
			captured := state.captureFallbackUsage([]byte(`{"normalizedUsage":{"inputTokens":0,"outputTokens":0,"reasoningTokens":0,"cacheReadTokens":0,"cacheWrite5mTokens":0,"cacheWrite1hTokens":0}}`))
			parsed := &dto.Usage{PromptTokens: 999, TotalTokens: 999, InputTokens: 999}

			usage := finalizeResponseUsage(parsed, state).(*dto.Usage)

			assert.True(t, captured)
			assert.True(t, state.sawFallbackInputField)
			assert.Equal(t, "opencode_go_inference_cost", usage.UsageSource)
			if usage.BillingUsage != nil {
				assert.False(t, usage.BillingUsage.Estimated)
			}
			assert.Zero(t, usage.PromptTokens)
			assert.Zero(t, usage.InputTokens)
			assert.Zero(t, usage.TotalTokens)
		})

		t.Run(string(protocol)+"/latest_partial_fallback_replaces_complete_input", func(t *testing.T) {
			state := &responseTransformState{protocol: protocol}
			state.captureFallbackUsage([]byte(`{"normalizedUsage":{"inputTokens":100,"outputTokens":40}}`))
			state.captureFallbackUsage([]byte(`{"normalizedUsage":{"outputTokens":41}}`))

			usage := finalizeResponseUsage(nil, state).(*dto.Usage)

			require.NotNil(t, usage.BillingUsage)
			assert.True(t, usage.BillingUsage.Estimated)
			assert.False(t, state.sawFallbackInputField)
			assert.Equal(t, 41, usage.OutputTokens)
		})

		t.Run(string(protocol)+"/invalid_fallback_does_not_replace_complete_input", func(t *testing.T) {
			state := &responseTransformState{protocol: protocol}
			state.captureFallbackUsage([]byte(`{"normalizedUsage":{"inputTokens":100,"outputTokens":40}}`))
			state.captureFallbackUsage([]byte(`{"normalizedUsage":{"inputTokens":-1,"outputTokens":41}}`))

			usage := finalizeResponseUsage(nil, state).(*dto.Usage)

			require.NotNil(t, usage.BillingUsage)
			assert.False(t, usage.BillingUsage.Estimated)
			assert.True(t, state.sawFallbackInputField)
			assert.Equal(t, 100, usage.PromptTokens)
			assert.Equal(t, 40, usage.OutputTokens)
		})
	}
}

func TestAdaptorChatStreamMissingCostInputUsesHandlerEstimate(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"id":"chat_partial_cost","object":"chat.completion.chunk","created":1710000000,"model":"vendor/chat-alias","choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":null}]}`,
		`data: {"id":"chat_partial_cost","object":"chat.completion.chunk","created":1710000000,"model":"vendor/chat-alias","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"x-opencode-type":"inference-cost","cost":"private","normalizedUsage":{"outputTokens":40,"cacheReadTokens":80,"cacheWrite5mTokens":20,"cacheWrite1hTokens":10}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	info := responseTestInfo(ProtocolChat, types.RelayFormatOpenAI, true)
	info.SetEstimatePromptTokens(999)
	c, recorder := responseTestContext(types.RelayFormatOpenAI)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	adaptor := &Adaptor{}
	adaptor.Init(info)
	adaptor.requestUpstreamStream = true

	rawUsage, apiErr := adaptor.DoResponse(c, resp, info)

	require.Nil(t, apiErr)
	usage, ok := rawUsage.(*dto.Usage)
	require.True(t, ok)
	require.NotNil(t, usage.BillingUsage)
	assert.True(t, usage.BillingUsage.Estimated)
	assert.Equal(t, 999, usage.InputTokens)
	assert.Equal(t, 999, usage.PromptTokens)
	assert.Equal(t, 40, usage.OutputTokens)
	assert.Equal(t, 80, usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, 30, usage.PromptTokensDetails.CacheCreationTokensTotal())
	assert.True(t, common.GetContextKeyBool(c, constant.ContextKeyLocalCountTokens))
	assert.NotContains(t, recorder.Body.String(), `"prompt_tokens":110`)
	assert.NotContains(t, recorder.Body.String(), "private")
	assert.NotContains(t, recorder.Body.String(), "normalizedUsage")
}

func openCodeRealChatUsageFixture(stream bool) string {
	usage := `"usage":{"prompt_tokens":210,"completion_tokens":40,"total_tokens":250,"cached_tokens":80,"prompt_tokens_details":{"cache_creation_input_tokens":30},"completion_tokens_details":{"reasoning_tokens":15}}`
	if !stream {
		return `{"id":"chat_real","object":"chat.completion","created":1710000000,"model":"vendor/chat-alias","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],` + usage + `,"cost":"0"}`
	}
	return strings.Join([]string{
		`data: {"id":"chat_real","object":"chat.completion.chunk","created":1710000000,"model":"vendor/chat-alias","choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":null}]}`,
		`data: {"id":"chat_real","object":"chat.completion.chunk","created":1710000000,"model":"vendor/chat-alias","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"id":"chat_real","object":"chat.completion.chunk","created":1710000000,"model":"vendor/chat-alias","choices":[],` + usage + `}`,
		`data: {"choices":[],"cost":"0"}`,
		`data: [DONE]`,
		``,
	}, "\n")
}

func TestAdaptorChatRealWirePreservesUsageAcrossClientFormats(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	want := responseUsageVector{
		openAIInput:     210,
		cacheRead:       80,
		cacheWriteTotal: 30,
		output:          40,
		reasoning:       15,
	}
	clients := []types.RelayFormat{
		types.RelayFormatOpenAI,
		types.RelayFormatClaude,
		types.RelayFormatOpenAIResponses,
	}
	for _, stream := range []bool{false, true} {
		for _, client := range clients {
			name := string(client) + "/json"
			contentType := "application/json"
			if stream {
				name = string(client) + "/sse"
				contentType = "text/event-stream"
			}
			t.Run(name, func(t *testing.T) {
				info := responseTestInfo(ProtocolChat, client, stream)
				info.SetEstimatePromptTokens(999)
				c, recorder := responseTestContext(client)
				resp := &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{contentType}},
					Body:       io.NopCloser(strings.NewReader(openCodeRealChatUsageFixture(stream))),
				}
				adaptor := &Adaptor{}
				adaptor.Init(info)
				adaptor.requestUpstreamStream = stream

				rawUsage, apiErr := adaptor.DoResponse(c, resp, info)

				require.Nil(t, apiErr)
				usage, ok := rawUsage.(*dto.Usage)
				require.True(t, ok)
				assertFinalResponseUsage(t, ProtocolChat, usage, want)
				require.NotNil(t, usage.BillingUsage)
				assert.False(t, usage.BillingUsage.Estimated)
				assert.False(t, common.GetContextKeyBool(c, constant.ContextKeyLocalCountTokens))
				assert.NotContains(t, recorder.Body.String(), `"cost"`)
			})
		}
	}
}

func TestTransformChatCostTrailerDoesNotRepeatSyntheticUsage(t *testing.T) {
	tagged := []byte(`{"choices":[],"x-opencode-type":"inference-cost","cost":"0","normalizedUsage":{"inputTokens":100,"outputTokens":40}}`)
	tests := []struct {
		name    string
		trailer []byte
	}{
		{name: "bare trailer", trailer: []byte(`{"choices":[],"cost":"0"}`)},
		{name: "bare trailer with normalized usage", trailer: []byte(`{"choices":[],"cost":"0","normalizedUsage":{"inputTokens":100,"outputTokens":40}}`)},
		{name: "repeated tagged cost", trailer: tagged},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &responseTransformState{protocol: ProtocolChat}
			first := state.transformJSON(tagged, true)
			second := state.transformJSON(tt.trailer, true)

			require.NotNil(t, first)
			assert.True(t, gjson.GetBytes(first, "usage").Exists())
			assert.Nil(t, second)
		})
	}
}

func TestTransformChatCostEventDisabledKeepsBillingFallbackWithoutPublicUsage(t *testing.T) {
	payload := []byte(`{"choices":[],"x-opencode-type":"inference-cost","cost":"private","normalizedUsage":{"inputTokens":100,"outputTokens":40,"reasoningTokens":15,"cacheReadTokens":80,"cacheWrite5mTokens":20,"cacheWrite1hTokens":10}}`)
	state := &responseTransformState{
		protocol:                ProtocolChat,
		usageConversionDisabled: true,
	}

	transformed := state.transformJSON(payload, true)

	assert.Nil(t, transformed)
	require.NotNil(t, state.fallbackUsage)
	assert.True(t, state.sawFallbackInputField)
	assert.False(t, state.emittedSyntheticChatUsage)
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
	require.NotNil(t, usage.BillingUsage)
	assert.False(t, usage.BillingUsage.Estimated)
}

func TestFinalizeResponseUsageKeepsExplicitZeroStandardInputOverLocalEstimate(t *testing.T) {
	standards := map[Protocol][]byte{
		ProtocolChat:      []byte(`{"usage":{"prompt_tokens":0,"completion_tokens":40}}`),
		ProtocolMessages:  []byte(`{"usage":{"input_tokens":0,"output_tokens":40}}`),
		ProtocolResponses: []byte(`{"response":{"usage":{"input_tokens":0,"output_tokens":40}}}`),
	}
	for _, protocol := range []Protocol{ProtocolChat, ProtocolMessages, ProtocolResponses} {
		t.Run(string(protocol), func(t *testing.T) {
			state := &responseTransformState{protocol: protocol, estimatedInputTokens: 999}
			state.transformJSON(standards[protocol], true)
			parsed := &dto.Usage{
				PromptTokens:     999,
				CompletionTokens: 40,
				TotalTokens:      1039,
				InputTokens:      999,
				OutputTokens:     40,
			}
			assert.True(t, state.sawStandardInputField)
			assert.False(t, state.standardUsageCategories.input)

			usage := finalizeResponseUsage(parsed, state).(*dto.Usage)

			require.NotNil(t, usage.BillingUsage)
			assert.False(t, usage.BillingUsage.Estimated)
			assert.Zero(t, usage.PromptTokens)
			assert.Zero(t, usage.InputTokens)
			assert.Equal(t, 40, usage.OutputTokens)
			assert.Equal(t, 40, usage.TotalTokens)
		})
	}
}

func missingProviderInputFixture(protocol Protocol, stream bool) string {
	if !stream {
		switch protocol {
		case ProtocolMessages:
			return `{"id":"msg_partial","type":"message","role":"assistant","model":"vendor/messages-alias","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"cache_read_input_tokens":80,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":10},"output_tokens":40},"cost":"0"}`
		case ProtocolResponses:
			return `{"id":"resp_partial","object":"response","created_at":1710000000,"model":"vendor/responses-alias","status":"completed","output":[{"id":"msg_partial","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK"}]}],"usage":{"output_tokens":40,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":30},"output_tokens_details":{"reasoning_tokens":15}},"cost":"0"}`
		}
	}

	partialCost := `data: {"type":"ping","cost":"0","normalizedUsage":{"outputTokens":40,"reasoningTokens":15,"cacheReadTokens":80,"cacheWrite5mTokens":20,"cacheWrite1hTokens":10}}`
	switch protocol {
	case ProtocolMessages:
		return strings.Join([]string{
			`data: {"type":"message_start","message":{"id":"msg_partial","type":"message","role":"assistant","model":"vendor/messages-alias","content":[],"usage":{"cache_read_input_tokens":80,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":10},"output_tokens":0}}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":40}}`,
			`data: {"type":"message_stop"}`,
			partialCost,
			`data: [DONE]`,
			``,
		}, "\n")
	case ProtocolResponses:
		return strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_partial","object":"response","model":"vendor/responses-alias","created_at":1710000000,"status":"in_progress"}}`,
			`data: {"type":"response.output_text.delta","delta":"OK"}`,
			`data: {"type":"response.completed","response":{"id":"resp_partial","object":"response","model":"vendor/responses-alias","status":"completed","usage":{"output_tokens":40,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":30},"output_tokens_details":{"reasoning_tokens":15}}}}`,
			partialCost,
			`data: [DONE]`,
			``,
		}, "\n")
	default:
		panic("unsupported missing-input fixture protocol")
	}
}

func TestAdaptorMissingProviderInputUsesRequestEstimateAcrossClientFormats(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	clients := []types.RelayFormat{
		types.RelayFormatOpenAI,
		types.RelayFormatClaude,
		types.RelayFormatOpenAIResponses,
	}
	for _, protocol := range []Protocol{ProtocolMessages, ProtocolResponses} {
		for _, stream := range []bool{false, true} {
			for _, client := range clients {
				name := string(protocol) + "/" + string(client) + "/json"
				contentType := "application/json"
				if stream {
					name = string(protocol) + "/" + string(client) + "/sse"
					contentType = "text/event-stream"
				}
				t.Run(name, func(t *testing.T) {
					info := responseTestInfo(protocol, client, stream)
					info.SetEstimatePromptTokens(999)
					c, recorder := responseTestContext(client)
					resp := &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{contentType}},
						Body:       io.NopCloser(strings.NewReader(missingProviderInputFixture(protocol, stream))),
					}
					adaptor := &Adaptor{}
					adaptor.Init(info)
					adaptor.requestUpstreamStream = stream

					rawUsage, apiErr := adaptor.DoResponse(c, resp, info)

					require.Nil(t, apiErr)
					usage, ok := rawUsage.(*dto.Usage)
					require.True(t, ok)
					want := responseUsageVector{
						input:           889,
						openAIInput:     999,
						cacheRead:       80,
						cacheWriteTotal: 30,
						output:          40,
					}
					if protocol == ProtocolMessages || stream {
						want.cacheWrite5m = 20
						want.cacheWrite1h = 10
					}
					if protocol == ProtocolResponses || stream {
						want.reasoning = 15
					}
					assertFinalResponseUsage(t, protocol, usage, want)
					require.NotNil(t, usage.BillingUsage)
					assert.True(t, usage.BillingUsage.Estimated)
					assert.NotContains(t, recorder.Body.String(), `"cost"`)
					assert.NotContains(t, recorder.Body.String(), "normalizedUsage")
				})
			}
		}
	}
}

func TestFinalizeResponseUsageRetainsDetailOnlyStandardCategories(t *testing.T) {
	standardDetails := dto.InputTokenDetails{
		CachedTokens:         81,
		CachedCreationTokens: 33,
		CacheWriteTokens:     32,
		TextTokens:           71,
		AudioTokens:          7,
		ImageTokens:          3,
	}
	standardOutputDetails := dto.OutputTokenDetails{
		TextTokens:      20,
		AudioTokens:     3,
		ImageTokens:     2,
		ReasoningTokens: 16,
	}
	want := responseUsageVector{
		input:           100,
		openAIInput:     210,
		cacheRead:       81,
		cacheWriteTotal: 33,
		cacheWrite5m:    21,
		cacheWrite1h:    12,
		output:          40,
		reasoning:       16,
		textInput:       71,
		textOutput:      20,
	}
	wantPresence := responseUsageCategoryPresence{
		cacheRead:    true,
		cacheWrite:   true,
		cacheWrite5m: true,
		cacheWrite1h: true,
		reasoning:    true,
		inputText:    true,
		inputAudio:   true,
		inputImage:   true,
		outputText:   true,
		outputAudio:  true,
		outputImage:  true,
	}

	for _, protocol := range []Protocol{ProtocolChat, ProtocolMessages, ProtocolResponses} {
		for _, order := range []struct {
			name          string
			standardFirst bool
		}{
			{name: "standard_before_cost", standardFirst: true},
			{name: "cost_before_standard"},
		} {
			t.Run(string(protocol)+"/"+order.name, func(t *testing.T) {
				state := reconciliationTransformInOrder(
					t,
					protocol,
					reconciliationDetailOnlyStandardEvent(protocol),
					order.standardFirst,
				)
				assert.True(t, state.sawPositiveStandardUsage)
				assert.Equal(t, wantPresence, state.standardUsageCategories)

				parsed := reconciliationEstimatedUsage()
				parsed.PromptTokensDetails = standardDetails
				inputDetails := standardDetails
				parsed.InputTokensDetails = &inputDetails
				parsed.CompletionTokenDetails = standardOutputDetails
				parsed.ClaudeCacheCreation5mTokens = 21
				parsed.ClaudeCacheCreation1hTokens = 12

				usage := finalizeResponseUsage(parsed, state).(*dto.Usage)

				assertFinalResponseUsage(t, protocol, usage, want)
				assert.Equal(t, 71, usage.PromptTokensDetails.TextTokens)
				assert.Equal(t, 7, usage.PromptTokensDetails.AudioTokens)
				assert.Equal(t, 3, usage.PromptTokensDetails.ImageTokens)
				assert.Equal(t, 20, usage.CompletionTokenDetails.TextTokens)
				assert.Equal(t, 3, usage.CompletionTokenDetails.AudioTokens)
				assert.Equal(t, 2, usage.CompletionTokenDetails.ImageTokens)
				assert.NotEqual(t, 999, usage.PromptTokens)
				assert.NotEqual(t, 999, usage.CompletionTokens)

				if protocol == ProtocolResponses {
					require.NotNil(t, usage.InputTokensDetails)
					assert.Equal(t, 71, usage.InputTokensDetails.TextTokens)
					assert.Equal(t, 7, usage.InputTokensDetails.AudioTokens)
					assert.Equal(t, 3, usage.InputTokensDetails.ImageTokens)
				}
				if protocol != ProtocolMessages {
					require.NotNil(t, usage.BillingUsage)
					require.NotNil(t, usage.BillingUsage.OpenAIUsage)
					billing := usage.BillingUsage.OpenAIUsage
					assert.Equal(t, 71, billing.PromptTokensDetails.TextTokens)
					assert.Equal(t, 7, billing.PromptTokensDetails.AudioTokens)
					assert.Equal(t, 3, billing.PromptTokensDetails.ImageTokens)
					assert.Equal(t, 20, billing.CompletionTokenDetails.TextTokens)
					assert.Equal(t, 3, billing.CompletionTokenDetails.AudioTokens)
					assert.Equal(t, 2, billing.CompletionTokenDetails.ImageTokens)
				}
			})
		}
	}
}

func TestTransformJSONSanitizesNonstreamCostExtensions(t *testing.T) {
	t.Run("pure_private_payload", func(t *testing.T) {
		state := &responseTransformState{protocol: ProtocolChat}
		transformed := state.transformJSON(costUsageEvent(t, ProtocolChat, reconciliationFallbackVector()), false)

		assert.Nil(t, transformed)
		require.NotNil(t, state.fallbackUsage)
		assert.Equal(t, &normalizedCostUsage{
			InputTokens:        100,
			OutputTokens:       40,
			ReasoningTokens:    15,
			CacheReadTokens:    80,
			CacheWrite5mTokens: 20,
			CacheWrite1hTokens: 10,
		}, state.fallbackUsage)
		assert.False(t, state.sawStandardUsage)
	})

	t.Run("public_response_with_private_fields", func(t *testing.T) {
		state := &responseTransformState{protocol: ProtocolChat}
		payload := []byte(`{"id":"chat_public","object":"chat.completion","model":"vendor-model","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":210,"completion_tokens":40,"total_tokens":250},"provider_extension":{"kept":true},"cost":"private","normalizedUsage":{"inputTokens":100,"outputTokens":40,"reasoningTokens":15,"cacheReadTokens":80,"cacheWrite5mTokens":20,"cacheWrite1hTokens":10},"x-opencode-type":"inference-cost"}`)

		transformed := state.transformJSON(payload, false)

		require.NotNil(t, transformed)
		assert.True(t, gjson.ValidBytes(transformed))
		assert.Equal(t, "chat_public", gjson.GetBytes(transformed, "id").String())
		assert.Equal(t, "OK", gjson.GetBytes(transformed, "choices.0.message.content").String())
		assert.Equal(t, int64(210), gjson.GetBytes(transformed, "usage.prompt_tokens").Int())
		assert.True(t, gjson.GetBytes(transformed, "provider_extension.kept").Bool())
		assert.False(t, gjson.GetBytes(transformed, "cost").Exists())
		assert.False(t, gjson.GetBytes(transformed, "normalizedUsage").Exists())
		assert.False(t, gjson.GetBytes(transformed, "x-opencode-type").Exists())
		require.NotNil(t, state.fallbackUsage)
		assert.True(t, state.sawStandardUsage)
	})
}

func TestTransformJSONStripsProviderCostFromStandardResponses(t *testing.T) {
	tests := []struct {
		name     string
		protocol Protocol
		payload  string
	}{
		{
			name:     "chat",
			protocol: ProtocolChat,
			payload:  `{"id":"chat_public","choices":[],"usage":{"prompt_tokens":7},"cost":"private"}`,
		},
		{
			name:     "messages",
			protocol: ProtocolMessages,
			payload:  `{"id":"msg_public","type":"message","content":[],"usage":{"input_tokens":7},"cost":"private"}`,
		},
		{
			name:     "responses",
			protocol: ProtocolResponses,
			payload:  `{"id":"resp_public","object":"response","output":[],"usage":{"input_tokens":7},"cost":"private"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &responseTransformState{protocol: tt.protocol}

			transformed := state.transformJSON([]byte(tt.payload), false)

			require.NotNil(t, transformed)
			assert.Equal(t, gjson.Get(tt.payload, "id").String(), gjson.GetBytes(transformed, "id").String())
			assert.False(t, gjson.GetBytes(transformed, "cost").Exists())
			assert.True(t, gjson.GetBytes(transformed, "usage").Exists())
			assert.True(t, state.sawStandardUsage)
		})
	}
}

func reconciliationResponsesSSEBody(t *testing.T, standardFirst bool) string {
	t.Helper()
	standard := `{"type":"response.completed","response":{"id":"resp_usage","object":"response","model":"vendor/responses-alias","status":"completed","output":[],"usage":{"output_tokens":40}}}`
	fallback := string(costUsageEvent(t, ProtocolResponses, reconciliationFallbackVector()))
	events := []string{fallback, standard}
	if standardFirst {
		events = []string{standard, fallback}
	}
	return strings.Join([]string{
		"data: " + events[0],
		"",
		"data: " + events[1],
		"",
		"data: [DONE]",
		"",
	}, "\n")
}

func TestAdaptorResponsesStreamReplacesHandlerPromptEstimateByCategory(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	for _, order := range []struct {
		name          string
		standardFirst bool
	}{
		{name: "standard_before_cost", standardFirst: true},
		{name: "cost_before_standard"},
	} {
		t.Run(order.name, func(t *testing.T) {
			info := responseTestInfo(ProtocolResponses, types.RelayFormatOpenAIResponses, true)
			info.SetEstimatePromptTokens(999)
			c, recorder := responseTestContext(types.RelayFormatOpenAIResponses)
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(reconciliationResponsesSSEBody(t, order.standardFirst))),
			}
			adaptor := &Adaptor{}
			adaptor.Init(info)
			adaptor.requestUpstreamStream = true

			rawUsage, apiErr := adaptor.DoResponse(c, resp, info)

			require.Nil(t, apiErr)
			usage, ok := rawUsage.(*dto.Usage)
			require.True(t, ok)
			assertFinalResponseUsage(t, ProtocolResponses, usage, reconciliationFallbackVector())
			assert.NotEqual(t, 999, usage.PromptTokens)
			assert.Equal(t, 210, usage.InputTokens)
			assert.Equal(t, 40, usage.OutputTokens)
			require.NotNil(t, info.StreamStatus)
			assert.True(t, info.StreamStatus.ProtocolTerminalObserved())

			output := recorder.Body.String()
			assert.Contains(t, output, "response.completed")
			assert.Contains(t, output, `"output_tokens":40`)
			assert.NotContains(t, output, "private")
			assert.NotContains(t, output, "normalizedUsage")
			assert.NotContains(t, output, `"cost"`)
		})
	}
}

func TestAdaptorResponsesStreamMarksHandlerFallbackAsEstimated(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	tests := []struct {
		name      string
		usageJSON string
		wantOut   int
	}{
		{name: "missing_usage"},
		{name: "output_only_usage", usageJSON: `,"usage":{"output_tokens":40}`, wantOut: 40},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := strings.Join([]string{
				`data: {"type":"response.created","response":{"id":"resp_estimated","object":"response","model":"vendor/responses-alias","status":"in_progress"}}`,
				`data: {"type":"response.output_text.delta","delta":"OK"}`,
				`data: {"type":"response.completed","response":{"id":"resp_estimated","object":"response","model":"vendor/responses-alias","status":"completed","output":[]` + tt.usageJSON + `}}`,
				`data: [DONE]`,
				``,
			}, "\n")
			info := responseTestInfo(ProtocolResponses, types.RelayFormatOpenAIResponses, true)
			info.SetEstimatePromptTokens(999)
			c, recorder := responseTestContext(types.RelayFormatOpenAIResponses)
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}
			adaptor := &Adaptor{}
			adaptor.Init(info)
			adaptor.requestUpstreamStream = true

			rawUsage, apiErr := adaptor.DoResponse(c, resp, info)

			require.Nil(t, apiErr)
			usage, ok := rawUsage.(*dto.Usage)
			require.True(t, ok)
			require.NotNil(t, usage.BillingUsage)
			assert.True(t, usage.BillingUsage.Estimated)
			assert.Equal(t, 999, usage.InputTokens)
			assert.Equal(t, 999, usage.PromptTokens)
			if tt.wantOut > 0 {
				assert.Equal(t, tt.wantOut, usage.OutputTokens)
			} else {
				assert.Positive(t, usage.OutputTokens)
			}
			assert.True(t, info.StreamStatus.ProtocolTerminalObserved())
			assert.NotContains(t, recorder.Body.String(), `"billing_usage"`)
			assert.NotContains(t, recorder.Body.String(), `"estimated"`)
		})
	}
}

func reconciliationMessagesCacheOnlyFixture(stream bool) string {
	if !stream {
		return `{"id":"msg_cache","type":"message","role":"assistant","model":"vendor/messages-alias","content":[],"stop_reason":"end_turn","usage":{"input_tokens":0,"cache_read_input_tokens":80,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":10},"output_tokens":0}}`
	}
	return strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_cache","type":"message","role":"assistant","model":"vendor/messages-alias","content":[],"usage":{"input_tokens":0,"cache_read_input_tokens":80,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":10},"output_tokens":0}}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`,
		`data: {"type":"message_stop"}`,
		`data: [DONE]`,
		``,
	}, "\n")
}

func TestAdaptorMessagesToResponsesPreservesCacheOnlyUsage(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	want := responseUsageVector{
		cacheRead:       80,
		cacheWriteTotal: 30,
		cacheWrite5m:    20,
		cacheWrite1h:    10,
	}
	for _, stream := range []bool{false, true} {
		name := "json"
		contentType := "application/json"
		if stream {
			name = "stream"
			contentType = "text/event-stream"
		}
		t.Run(name, func(t *testing.T) {
			info := responseTestInfo(ProtocolMessages, types.RelayFormatOpenAIResponses, stream)
			info.SetEstimatePromptTokens(999)
			c, recorder := responseTestContext(types.RelayFormatOpenAIResponses)
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{contentType}},
				Body:       io.NopCloser(strings.NewReader(reconciliationMessagesCacheOnlyFixture(stream))),
			}
			adaptor := &Adaptor{}
			adaptor.Init(info)
			adaptor.requestUpstreamStream = stream

			rawUsage, apiErr := adaptor.DoResponse(c, resp, info)

			require.Nil(t, apiErr)
			usage, ok := rawUsage.(*dto.Usage)
			require.True(t, ok)
			assertFinalResponseUsage(t, ProtocolMessages, usage, want)
			assert.Zero(t, usage.PromptTokens)
			assert.Zero(t, usage.CompletionTokens)
			assert.Zero(t, usage.TotalTokens)
			assert.Equal(t, 110, usage.InputTokens)
			assert.NotEqual(t, 999, usage.InputTokens)
			require.NotNil(t, usage.BillingUsage)
			assert.Equal(t, dto.BillingUsageSourceClaudeMessages, usage.BillingUsage.Source)
			require.NotNil(t, usage.BillingUsage.ClaudeUsage)
			assert.Equal(t, 80, usage.BillingUsage.ClaudeUsage.CacheReadInputTokens)
			assert.Equal(t, 30, usage.BillingUsage.ClaudeUsage.CacheCreationInputTokens)
			assert.Equal(t, 20, usage.BillingUsage.ClaudeUsage.ClaudeCacheCreation5mTokens)
			assert.Equal(t, 10, usage.BillingUsage.ClaudeUsage.ClaudeCacheCreation1hTokens)
			assert.NotEmpty(t, recorder.Body.String())

			if stream {
				require.NotNil(t, info.StreamStatus)
				assert.True(t, info.StreamStatus.ProtocolTerminalObserved())
			}
		})
	}
}

func TestAdaptorMessagesToChatStreamPreservesCacheOnlyUsage(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	info := responseTestInfo(ProtocolMessages, types.RelayFormatOpenAI, true)
	info.SetEstimatePromptTokens(999)
	c, recorder := responseTestContext(types.RelayFormatOpenAI)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(reconciliationMessagesCacheOnlyFixture(true))),
	}
	adaptor := &Adaptor{}
	adaptor.Init(info)
	adaptor.requestUpstreamStream = true

	rawUsage, apiErr := adaptor.DoResponse(c, resp, info)

	require.Nil(t, apiErr)
	usage, ok := rawUsage.(*dto.Usage)
	require.True(t, ok)
	assert.Zero(t, usage.PromptTokens)
	assert.Zero(t, usage.CompletionTokens)
	assert.Zero(t, usage.TotalTokens)
	assert.Equal(t, 110, usage.InputTokens)
	assert.False(t, common.GetContextKeyBool(c, constant.ContextKeyLocalCountTokens))

	var publicUsage *dto.Usage
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var chunk dto.ChatCompletionsStreamResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err == nil && chunk.Usage != nil {
			publicUsage = chunk.Usage
		}
	}
	require.NotNil(t, publicUsage)
	assert.Equal(t, 110, publicUsage.PromptTokens)
	assert.Zero(t, publicUsage.CompletionTokens)
	assert.Equal(t, 110, publicUsage.TotalTokens)
	assert.Equal(t, 110, publicUsage.InputTokens)
	assert.Equal(t, 80, publicUsage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, 30, publicUsage.PromptTokensDetails.CacheCreationTokensTotal())
}
