package opencodego

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFinalizeMessagesToChatPreservesRawFieldSemantics(t *testing.T) {
	tests := []struct {
		name             string
		extraFields      string
		wantThinking     string
		wantStop         string
		wantThinkingSeen bool
		wantStopSeen     bool
	}{
		{
			name:             "exact thinking",
			extraFields:      `,"thinking": { "type":"enabled", "budget_tokens":9007199254740993, "scale":1e+09 }`,
			wantThinking:     `{ "type":"enabled", "budget_tokens":9007199254740993, "scale":1e+09 }`,
			wantThinkingSeen: true,
		},
		{
			name:         "stop sequences translate by presence",
			extraFields:  `,"stop_sequences": ["END", "DONE"]`,
			wantStop:     `["END", "DONE"]`,
			wantStopSeen: true,
		},
		{
			name:         "raw stop fallback",
			extraFields:  `,"stop": { "provider_extension":9007199254740993 }`,
			wantStop:     `{ "provider_extension":9007199254740993 }`,
			wantStopSeen: true,
		},
		{
			name:         "empty stop sequences remain present",
			extraFields:  `,"stop_sequences":[]`,
			wantStop:     `[]`,
			wantStopSeen: true,
		},
		{
			name:             "explicit nulls remain present",
			extraFields:      `,"thinking":null,"stop_sequences":null`,
			wantThinking:     `null`,
			wantStop:         `null`,
			wantThinkingSeen: true,
			wantStopSeen:     true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, info := newMessagesFinalizerFixture(t, test.extraFields)
			converted := &dto.GeneralOpenAIRequest{
				Model:    "glm-5.2",
				Messages: []dto.Message{{Role: "user", Content: "hello"}},
			}

			result, err := finalizeOutboundRequest(c, info, converted)
			require.NoError(t, err)
			object := decodeFinalizerTestObject(t, result)
			_, sourcePresent := object["stop_sequences"]
			assert.False(t, sourcePresent)

			thinking, thinkingPresent := object["thinking"]
			assert.Equal(t, test.wantThinkingSeen, thinkingPresent)
			if test.wantThinkingSeen {
				assert.True(t, bytes.Equal([]byte(test.wantThinking), thinking), "thinking raw value changed: %s", thinking)
			}
			stop, stopPresent := object["stop"]
			assert.Equal(t, test.wantStopSeen, stopPresent)
			if test.wantStopSeen {
				assert.True(t, bytes.Equal([]byte(test.wantStop), stop), "stop raw value changed: %s", stop)
			}
		})
	}
}

func TestFinalizeMessagesToChatRejectsStopSourceCollision(t *testing.T) {
	c, info := newMessagesFinalizerFixture(t, `,"stop_sequences":null,"stop":null`)
	converted := &dto.GeneralOpenAIRequest{
		Model:    "glm-5.2",
		Messages: []dto.Message{{Role: "user", Content: "hello"}},
	}

	_, err := finalizeOutboundRequest(c, info, converted)
	require.Error(t, err)
	validationErr, ok := helper.AsClientRequestValidationError(err)
	require.True(t, ok)
	assert.Equal(t, http.StatusBadRequest, validationErr.StatusCode)
	assert.Equal(t, MessagesStopSourceCollisionRule, validationErr.RuleID)
	assert.Equal(t, RequestContractPreflightStage, validationErr.StageID)
}

func TestFinalizeMessagesToChatPreservesExplicitFalseStreamPresence(t *testing.T) {
	tests := []struct {
		name          string
		extraFields   string
		streamPresent bool
		streamValue   bool
	}{
		{name: "absent remains absent"},
		{name: "explicit false remains present", extraFields: `,"stream":false`, streamPresent: true},
		{name: "explicit true remains present", extraFields: `,"stream":true`, streamPresent: true, streamValue: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, info := newMessagesFinalizerFixture(t, test.extraFields)
			converted := &dto.GeneralOpenAIRequest{
				Model:    "glm-5.2",
				Messages: []dto.Message{{Role: "user", Content: "hello"}},
			}

			result, err := finalizeOutboundRequest(c, info, converted)
			require.NoError(t, err)
			object := decodeFinalizerTestObject(t, result)
			stream, present := object["stream"]
			assert.Equal(t, test.streamPresent, present)
			if test.streamPresent {
				assert.JSONEq(t, fmt.Sprintf("%t", test.streamValue), string(stream))
			}
		})
	}
}

func TestFinalizeMessagesToChatOrdersMutationsAndPreservesRawSubtrees(t *testing.T) {
	c, info := newMessagesFinalizerFixture(
		t,
		`,"thinking": { "type":"enabled", "budget_tokens":9007199254740993, "scale":1e+09 },"stop_sequences":["raw"]`,
	)
	stream := true
	converted := &dto.GeneralOpenAIRequest{
		Model: "glm-5.2",
		Messages: []dto.Message{
			{Role: "system", Content: "gateway prompt\nclient prompt"},
			{Role: "user", Content: "hello"},
		},
		Stream:        &stream,
		StreamOptions: &dto.StreamOptions{IncludeUsage: true},
	}
	info.ParamOverride = map[string]interface{}{
		"temperature":       0.25,
		"model":             "override-model",
		"stream":            false,
		"stop":              "override-stop",
		"service_tier":      "priority",
		"safety_identifier": "private-user",
		"store":             true,
		"stream_options": map[string]interface{}{
			"include_usage":       false,
			"include_obfuscation": true,
		},
	}
	info.ChannelOtherSettings.DisableStore = true

	result, err := finalizeOutboundRequest(c, info, converted)
	require.NoError(t, err)
	object := decodeFinalizerTestObject(t, result)

	assert.JSONEq(t, `"glm-5.2"`, string(object["model"]))
	assert.JSONEq(t, `true`, string(object["stream"]))
	assert.JSONEq(t, `0.25`, string(object["temperature"]))
	assert.JSONEq(t, `"override-stop"`, string(object["stop"]))
	assert.True(
		t,
		bytes.Equal(
			[]byte(`{ "type":"enabled", "budget_tokens":9007199254740993, "scale":1e+09 }`),
			object["thinking"],
		),
		"unrelated override normalized the raw thinking subtree: %s",
		object["thinking"],
	)

	for _, protected := range []string{"service_tier", "safety_identifier", "store"} {
		_, present := object[protected]
		assert.False(t, present, "protected field %s survived final fence", protected)
	}
	streamOptions := decodeFinalizerTestObject(t, object["stream_options"])
	_, obfuscationPresent := streamOptions["include_obfuscation"]
	assert.False(t, obfuscationPresent)
	assert.JSONEq(t, `false`, string(streamOptions["include_usage"]))

	var messages []dto.Message
	require.NoError(t, common.Unmarshal(object["messages"], &messages))
	require.Len(t, messages, 2)
	assert.Equal(t, "system", messages[0].Role)
	assert.Equal(t, "gateway prompt\nclient prompt", messages[0].StringContent())
}

func TestFinalizeMessagesToChatTargetedThinkingOverrideWins(t *testing.T) {
	c, info := newMessagesFinalizerFixture(t, `,"thinking":{"type":"raw","value":9007199254740993}`)
	info.ParamOverride = map[string]interface{}{
		"thinking": map[string]interface{}{"type": "override", "value": int64(42)},
	}
	converted := &dto.GeneralOpenAIRequest{
		Model:    "glm-5.2",
		Messages: []dto.Message{{Role: "user", Content: "hello"}},
	}

	result, err := finalizeOutboundRequest(c, info, converted)
	require.NoError(t, err)
	object := decodeFinalizerTestObject(t, result)
	assert.JSONEq(t, `{"type":"override","value":42}`, string(object["thinking"]))
}

func TestFinalizeSameProtocolPreservesRawPresenceAndNestedExtensions(t *testing.T) {
	tests := []struct {
		name              string
		format            types.RelayFormat
		path              string
		body              string
		presenceField     string
		emptyField        string
		nestedTopLevel    string
		nestedRawFragment string
	}{
		{
			name:   "chat",
			format: types.RelayFormatOpenAI,
			path:   "/v1/chat/completions",
			body: `{
				"model":"glm-5.2",
				"messages":[{"role":"user","content":"hello","provider_extension":{"big":9007199254740993}}],
				"temperature":null,
				"modalities":[],
				"metadata":{"scale":1e+09}
			}`,
			presenceField:     "temperature",
			emptyField:        "modalities",
			nestedTopLevel:    "messages",
			nestedRawFragment: `"provider_extension":{"big":9007199254740993}`,
		},
		{
			name:   "messages",
			format: types.RelayFormatClaude,
			path:   "/v1/messages",
			body: `{
				"model":"qwen3.8-max",
				"max_tokens":16,
				"messages":[{"role":"user","content":"hello","provider_extension":{"big":9007199254740993}}],
				"temperature":null,
				"stop_sequences":[],
				"metadata":{"scale":1e+09}
			}`,
			presenceField:     "temperature",
			emptyField:        "stop_sequences",
			nestedTopLevel:    "messages",
			nestedRawFragment: `"provider_extension":{"big":9007199254740993}`,
		},
		{
			name:   "responses",
			format: types.RelayFormatOpenAIResponses,
			path:   "/v1/responses",
			body: `{
				"model":"gpt-5.6-luna",
				"input":[{"role":"user","content":[{"type":"input_text","text":"hello","provider_extension":{"big":9007199254740993}}]}],
				"temperature":null,
				"include":[],
				"metadata":{"scale":1e+09}
			}`,
			presenceField:     "temperature",
			emptyField:        "include",
			nestedTopLevel:    "input",
			nestedRawFragment: `"provider_extension":{"big":9007199254740993}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, info, request := newSameProtocolFinalizerFixture(t, test.format, test.path, []byte(test.body))

			result, err := finalizeOutboundRequest(c, info, request)
			require.NoError(t, err)
			object := decodeFinalizerTestObject(t, result)

			presence, found := object[test.presenceField]
			require.True(t, found, "explicit null disappeared")
			assert.Equal(t, "null", string(presence))
			empty, found := object[test.emptyField]
			require.True(t, found, "explicit empty collection disappeared")
			assert.Equal(t, "[]", string(empty))
			nested, found := object[test.nestedTopLevel]
			require.True(t, found)
			assert.Contains(t, string(nested), test.nestedRawFragment)
			assert.Contains(t, string(object["metadata"]), "1e+09")
		})
	}
}

func TestFinalizePreservesClientTopLevelExtensionsAcrossProtocolMatrix(t *testing.T) {
	extensionValue := json.RawMessage(`{
		"null_value":null,
		"false_value":false,
		"zero_value":0,
		"empty_array":[],
		"empty_object":{},
		"large_integer":9007199254740993,
		"unicode":"\u4f60\u597d"
	}`)
	tests := []struct {
		name          string
		endpoint      requestPreflightEndpoint
		finalProtocol Protocol
	}{
		{name: "messages to messages", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolMessages},
		{name: "chat to chat", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolChat},
		{name: "responses to responses", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolResponses},
		{name: "messages to chat", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolChat},
		{name: "messages to responses", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolResponses},
		{name: "chat to messages", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolMessages},
		{name: "chat to responses", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolResponses},
		{name: "responses to chat", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat},
		{name: "responses to messages", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolMessages},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("type-%d/%s", channelType, test.name), func(t *testing.T) {
				const extensionField = "provider.extension"
				c, info := newRequestContractFixture(
					t,
					channelType,
					test.endpoint,
					test.finalProtocol,
					map[string]any{extensionField: extensionValue},
				)
				plan, err := BuildRequestPreflightPlan(c, info)
				require.NoError(t, err)
				assert.Equal(t, test.finalProtocol, plan.FinalProtocol)

				envelope, found, err := helper.GetValidatedRequestEnvelope(c, test.endpoint.format)
				require.NoError(t, err)
				require.True(t, found)
				expected, present, err := envelope.RawTopLevelField(extensionField)
				require.NoError(t, err)
				require.True(t, present)

				info.InitChannelMeta(c)
				info.UpstreamModelName = "glm-5.2"
				info.FinalRequestRelayFormat = test.finalProtocol.RelayFormat()
				result, err := finalizeOutboundRequest(c, info, minimalFinalizerConvertedRequest(test.finalProtocol))
				require.NoError(t, err)
				actual, found := decodeFinalizerTestObject(t, result)[extensionField]
				require.True(t, found)
				assert.True(t, bytes.Equal(expected, actual), "client extension changed: %s", actual)
			})
		}
	}
}

func TestFinalizeRejectsMutatedClientTopLevelExtension(t *testing.T) {
	const extensionField = "provider_extension"
	c, info := newRequestContractFixture(
		t,
		constant.ChannelTypeOpenCodeAPIKey,
		requestPreflightEndpoints[1],
		ProtocolResponses,
		map[string]any{extensionField: map[string]any{"value": 1}},
	)
	info.InitChannelMeta(c)
	info.UpstreamModelName = "glm-5.2"
	info.FinalRequestRelayFormat = types.RelayFormatOpenAIResponses
	info.ParamOverride = map[string]interface{}{extensionField: map[string]any{"value": 2}}

	_, err := finalizeOutboundRequest(c, info, minimalFinalizerConvertedRequest(ProtocolResponses))
	require.EqualError(t, err, RequestContractFinalizedMessage)
	assert.NotContains(t, err.Error(), extensionField)
}

func TestFinalizeRejectsLexicallyMutatedClientTopLevelExtension(t *testing.T) {
	body := []byte(`{
		"model":"glm-5.2",
		"messages":[{"role":"user","content":"hello"}],
		"provider_extension":1e+09
	}`)
	c, info, request := newSameProtocolFinalizerFixture(
		t,
		types.RelayFormatOpenAI,
		"/v1/chat/completions",
		body,
	)
	info.ParamOverride = map[string]interface{}{"provider_extension": int64(1000000000)}

	_, err := finalizeOutboundRequest(c, info, request)
	require.EqualError(t, err, RequestContractFinalizedMessage)
	assert.NotContains(t, err.Error(), "provider_extension")
}

func TestFinalizeResponsesToChatPreservesExplicitFalseIncludeUsage(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			c, info := newRequestContractFixture(
				t,
				channelType,
				requestPreflightEndpoints[2],
				ProtocolChat,
				map[string]any{
					"stream": false,
					"stream_options": map[string]any{
						"include_usage":       false,
						"include_obfuscation": false,
					},
				},
			)
			_, err := BuildRequestPreflightPlan(c, info)
			require.NoError(t, err)
			info.InitChannelMeta(c)
			info.UpstreamModelName = "glm-5.2"
			info.FinalRequestRelayFormat = types.RelayFormatOpenAI

			result, err := finalizeOutboundRequest(c, info, minimalFinalizerConvertedRequest(ProtocolChat))
			require.NoError(t, err)
			object := decodeFinalizerTestObject(t, result)
			streamOptions := decodeFinalizerTestObject(t, object["stream_options"])
			includeUsage, present := streamOptions["include_usage"]
			require.True(t, present)
			assert.Equal(t, "false", string(includeUsage))
			_, obfuscationPresent := streamOptions["include_obfuscation"]
			assert.False(t, obfuscationPresent)
		})
	}
}

func TestFinalizePreservesTranslatedNullAndEmptySameNameFields(t *testing.T) {
	tests := []struct {
		name          string
		endpoint      requestPreflightEndpoint
		finalProtocol Protocol
	}{
		{name: "messages to chat", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolChat},
		{name: "messages to responses", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolResponses},
		{name: "chat to messages", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolMessages},
		{name: "chat to responses", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolResponses},
		{name: "responses to chat", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat},
		{name: "responses to messages", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolMessages},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("type-%d/%s", channelType, test.name), func(t *testing.T) {
				c, info := newRequestContractFixture(
					t,
					channelType,
					test.endpoint,
					test.finalProtocol,
					map[string]any{
						"temperature": nil,
						"tools":       []any{},
					},
				)
				plan, err := BuildRequestPreflightPlan(c, info)
				require.NoError(t, err)
				assert.Equal(t, test.finalProtocol, plan.FinalProtocol)

				info.InitChannelMeta(c)
				info.UpstreamModelName = "glm-5.2"
				info.FinalRequestRelayFormat = test.finalProtocol.RelayFormat()
				result, err := finalizeOutboundRequest(c, info, minimalFinalizerConvertedRequest(test.finalProtocol))
				require.NoError(t, err)
				object := decodeFinalizerTestObject(t, result)
				assert.Equal(t, "null", string(object["temperature"]))
				assert.Equal(t, "[]", string(object["tools"]))
			})
		}
	}
}

func TestFinalizePreservesAcceptedNestedFamiliesThroughPhysicalConversion(t *testing.T) {
	const (
		marker       = "private_marker"
		largeInteger = "9007199254740993"
	)
	tests := []struct {
		name          string
		endpoint      requestPreflightEndpoint
		finalProtocol Protocol
		extraFields   map[string]any
		wantFragments []string
	}{
		{
			name: "chat media", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolResponses,
			extraFields: map[string]any{"messages": json.RawMessage(`[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/private_marker/9007199254740993"}},{"type":"input_audio","input_audio":{"data":"private_marker-9007199254740993","format":"wav"}},{"type":"file","file":{"filename":"private_marker","file_data":"9007199254740993"}},{"type":"video_url","video_url":"https://example.invalid/private_marker/9007199254740993"}]}]`)},
		},
		{
			name: "chat assistant tool call", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolResponses,
			extraFields: map[string]any{"messages": json.RawMessage(`[{"role":"assistant","content":"working","tool_calls":[{"id":"call_1","type":"function","function":{"name":"private_marker","arguments":"{\"big\":9007199254740993}"}}]}]`)},
		},
		{
			name: "chat tool result content", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolResponses,
			extraFields: map[string]any{"messages": json.RawMessage(`[{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"done","provider_extension":{"marker":"private_marker","big":9007199254740993}}]}]`)},
		},
		{
			name: "chat function tool schema", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolResponses,
			extraFields: map[string]any{"tools": json.RawMessage(`[{"type":"function","function":{"name":"lookup","description":"private_marker","parameters":{"type":"object","provider_schema_extension":{"big":9007199254740993}}}}]`)},
		},
		{
			name: "chat custom tool", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolResponses,
			extraFields: map[string]any{"tools": json.RawMessage(`[{"type":"custom","custom":{"provider_extension":{"marker":"private_marker","big":9007199254740993}}}]`)},
		},
		{
			name: "chat non-function tool choice", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolResponses,
			extraFields: map[string]any{"tool_choice": json.RawMessage(`{"type":"computer","provider_extension":{"marker":"private_marker","big":9007199254740993}}`)},
		},
		{
			name: "messages system array", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolChat,
			extraFields: map[string]any{"system": json.RawMessage(`[{"type":"text","text":"private_marker 9007199254740993"}]`)},
		},
		{
			name: "messages tool input", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolChat,
			extraFields: map[string]any{"messages": json.RawMessage(`[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{"marker":"private_marker","big":9007199254740993}}]}]`)},
		},
		{
			name: "messages tool schema", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolChat,
			extraFields: map[string]any{"tools": json.RawMessage(`[{"name":"lookup","description":"private_marker","input_schema":{"type":"object","provider_schema_extension":{"big":9007199254740993}}}]`)},
		},
		{
			name: "responses function call", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			extraFields: map[string]any{"input": json.RawMessage(`[{"type":"function_call","call_id":"call_1","name":"private_marker","arguments":"{\"big\":9007199254740993}"}]`)},
		},
		{
			name: "responses function output", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			extraFields: map[string]any{"input": json.RawMessage(`[{"type":"function_call_output","call_id":"call_1","output":{"marker":"private_marker","big":9007199254740993}}]`)},
		},
		{
			name: "responses function tool", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			extraFields: map[string]any{"tools": json.RawMessage(`[{"type":"function","name":"lookup","description":"private_marker","parameters":{"type":"object","provider_schema_extension":{"big":9007199254740993}}}]`)},
		},
		{
			name: "responses custom call", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			extraFields: map[string]any{"input": json.RawMessage(`[{"type":"custom_tool_call","call_id":"call_1","name":"apply_patch","input":{"marker":"private_marker","big":9007199254740993}}]`)},
		},
		{
			name: "responses custom output", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			extraFields: map[string]any{"input": json.RawMessage(`[{"type":"custom_tool_call_output","call_id":"call_1","output":{"marker":"private_marker","big":9007199254740993}}]`)},
		},
		{
			name: "responses custom tool", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			extraFields:   map[string]any{"tools": json.RawMessage(`[{"type":"custom","name":"private_marker","description":"9007199254740993"}]`)},
			wantFragments: []string{`"type":"function"`},
		},
		{
			name: "responses JSON schema", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			extraFields: map[string]any{"text": json.RawMessage(`{"format":{"type":"json_schema","name":"private_marker","schema":{"type":"object","provider_schema_extension":{"big":9007199254740993}}}}`)},
		},
		{
			name: "responses namespace declaration and choice", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			extraFields: map[string]any{
				"tools":       json.RawMessage(`[{"type":"namespace","name":"fs","tools":[{"type":"function","name":"read","description":"private_marker","parameters":{"type":"object","provider_schema_extension":{"big":9007199254740993}}}]}]`),
				"tool_choice": json.RawMessage(`{"type":"namespace","namespace":"fs","function":{"name":"read"}}`),
			},
			wantFragments: []string{`"name":"fs__read"`},
		},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("type-%d/%s", channelType, test.name), func(t *testing.T) {
				c, info := newRequestContractFixture(
					t,
					channelType,
					test.endpoint,
					test.finalProtocol,
					test.extraFields,
				)

				result := convertAndFinalizeRequestForPresenceTest(t, c, info)
				assert.Contains(t, string(result), marker)
				assert.Contains(t, string(result), largeInteger)
				assert.NotContains(t, string(result), "9007199254740992")
				for _, fragment := range test.wantFragments {
					assert.Contains(t, string(result), fragment)
				}
			})
		}
	}
}

func TestFinalizeResponsesToChatDoesNotCopyEmptyDifferentlyTranslatedFields(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			c, info := newRequestContractFixture(
				t,
				channelType,
				requestPreflightEndpoints[2],
				ProtocolChat,
				map[string]any{
					"input":     "",
					"reasoning": map[string]any{},
				},
			)
			plan, err := BuildRequestPreflightPlan(c, info)
			require.NoError(t, err)
			assert.Equal(t, ProtocolChat, plan.FinalProtocol)

			info.InitChannelMeta(c)
			info.UpstreamModelName = "glm-5.2"
			info.FinalRequestRelayFormat = types.RelayFormatOpenAI
			result, err := finalizeOutboundRequest(c, info, minimalFinalizerConvertedRequest(ProtocolChat))
			require.NoError(t, err)
			object := decodeFinalizerTestObject(t, result)
			_, inputPresent := object["input"]
			_, reasoningPresent := object["reasoning"]
			assert.False(t, inputPresent)
			assert.False(t, reasoningPresent)
		})
	}
}

func TestFinalizeSameProtocolRejectsRawExtensionWhenGatewayMutatesItsOwner(t *testing.T) {
	body := []byte(`{
		"model":"glm-5.2",
		"messages":[{"role":"user","content":"hello","provider_extension":{"private":"value"}}]
	}`)
	c, info, request := newSameProtocolFinalizerFixture(
		t,
		types.RelayFormatOpenAI,
		"/v1/chat/completions",
		body,
	)
	converted, err := common.DeepCopy(request.(*dto.GeneralOpenAIRequest))
	require.NoError(t, err)
	converted.Messages = append([]dto.Message{{Role: "system", Content: "gateway prompt"}}, converted.Messages...)

	_, err = finalizeOutboundRequest(c, info, converted)
	require.Error(t, err)
	validationErr, ok := helper.AsClientRequestValidationError(err)
	require.True(t, ok)
	assert.Equal(t, RequestContractPreserveConflictRule, validationErr.RuleID)
	assert.NotContains(t, err.Error(), "provider_extension")
	assert.NotContains(t, err.Error(), "private")
}

func TestFinalizeRejectsNonQwenThinkingBudgetInsteadOfBypassingMarshalPolicy(t *testing.T) {
	body := []byte(`{
		"model":"glm-5.2",
		"messages":[{"role":"user","content":"hello"}],
		"thinking_budget":128
	}`)
	c, info, request := newSameProtocolFinalizerFixture(
		t,
		types.RelayFormatOpenAI,
		"/v1/chat/completions",
		body,
	)

	_, err := finalizeOutboundRequest(c, info, request)
	require.Error(t, err)
	validationErr, ok := helper.AsClientRequestValidationError(err)
	require.True(t, ok)
	assert.Equal(t, RequestContractThinkingBudgetRule, validationErr.RuleID)
}

func TestFinalizeRejectsUnclassifiedOperatorOverrideInProductionPath(t *testing.T) {
	body := []byte(`{
		"model":"glm-5.2",
		"messages":[{"role":"user","content":"hello"}]
	}`)
	c, info, request := newSameProtocolFinalizerFixture(
		t,
		types.RelayFormatOpenAI,
		"/v1/chat/completions",
		body,
	)
	info.ParamOverride = map[string]interface{}{"future_output_copies": 64}

	_, err := finalizeOutboundRequest(c, info, request)
	require.EqualError(t, err, RequestContractFinalizedMessage)
	assert.NotContains(t, err.Error(), "future_output_copies")
}

func newSameProtocolFinalizerFixture(
	t *testing.T,
	format types.RelayFormat,
	path string,
	body []byte,
) (*gin.Context, *relaycommon.RelayInfo, dto.Request) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	storage, err := common.CreateBodyStorage(body)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })
	c.Set(common.KeyBodyStorage, storage)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeAPIKey)
	common.SetContextKey(c, constant.ContextKeyChannelId, 6301)
	common.SetContextKey(c, constant.ContextKeyChannelModelMapping, "")
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{})
	var modelField struct {
		Model string `json:"model"`
	}
	require.NoError(t, common.Unmarshal(body, &modelField))
	require.NotEmpty(t, modelField.Model)
	common.SetContextKey(c, constant.ContextKeyOriginalModel, modelField.Model)

	request, err := helper.GetAndValidateRequest(c, format)
	require.NoError(t, err)
	info, err := relaycommon.GenRelayInfo(c, format, request, nil)
	require.NoError(t, err)
	info.InitChannelMeta(c)
	info.UpstreamModelName = modelField.Model
	info.FinalRequestRelayFormat = format
	return c, info, request
}

func minimalFinalizerConvertedRequest(finalProtocol Protocol) any {
	switch finalProtocol {
	case ProtocolChat:
		return &dto.GeneralOpenAIRequest{
			Model:    "glm-5.2",
			Messages: []dto.Message{{Role: "user", Content: "hello"}},
		}
	case ProtocolMessages:
		maxTokens := uint(16)
		return &dto.ClaudeRequest{
			Model:     "glm-5.2",
			MaxTokens: &maxTokens,
			Messages:  []dto.ClaudeMessage{{Role: "user", Content: "hello"}},
		}
	case ProtocolResponses:
		return &dto.OpenAIResponsesRequest{
			Model: "glm-5.2",
			Input: json.RawMessage(`"hello"`),
		}
	default:
		return nil
	}
}

func newMessagesFinalizerFixture(t *testing.T, extraFields string) (*gin.Context, *relaycommon.RelayInfo) {
	t.Helper()
	body := []byte(`{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"hello"}]` + extraFields + `}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	storage, err := common.CreateBodyStorage(body)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })
	c.Set(common.KeyBodyStorage, storage)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeAPIKey)
	common.SetContextKey(c, constant.ContextKeyChannelId, 6301)
	common.SetContextKey(c, constant.ContextKeyChannelModelMapping, "")
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{})
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "glm-5.2")

	request, err := helper.GetAndValidateRequest(c, types.RelayFormatClaude)
	require.NoError(t, err)
	info, err := relaycommon.GenRelayInfo(c, types.RelayFormatClaude, request, nil)
	require.NoError(t, err)
	info.InitChannelMeta(c)
	info.UpstreamModelName = "glm-5.2"
	info.FinalRequestRelayFormat = types.RelayFormatOpenAI
	return c, info
}

func decodeFinalizerTestObject(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	require.NoError(t, common.Unmarshal(data, &object))
	require.NotNil(t, object)
	return object
}
