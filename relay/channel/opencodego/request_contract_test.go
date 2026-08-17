package opencodego

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
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

func TestRequestPathContractRegistryClassifiesEveryDTOField(t *testing.T) {
	dtoTypes := map[types.RelayFormat]reflect.Type{
		types.RelayFormatClaude:          reflect.TypeOf(dto.ClaudeRequest{}),
		types.RelayFormatOpenAI:          reflect.TypeOf(dto.GeneralOpenAIRequest{}),
		types.RelayFormatOpenAIResponses: reflect.TypeOf(dto.OpenAIResponsesRequest{}),
	}
	expectedRows := 3 // Messages raw `stop`, one row per final protocol.
	for clientFormat, dtoType := range dtoTypes {
		dtoFields := requestContractJSONFields(dtoType)
		listedFields := append([]string(nil), typedRequestTopLevelFields[clientFormat]...)
		sort.Strings(dtoFields)
		sort.Strings(listedFields)
		assert.Equal(t, dtoFields, listedFields, "typed field list drifted for %s", clientFormat)
		expectedRows += len(dtoFields) * len(requestContractProtocols)
		expectedRows += len(effortAliasTopLevelFields[clientFormat]) * len(requestContractProtocols)

		for _, field := range dtoFields {
			for _, finalProtocol := range requestContractProtocols {
				contract, found := LookupRequestPathContract(clientFormat, finalProtocol, field)
				require.Truef(t, found, "missing row for %s -> %s field %q", clientFormat, finalProtocol, field)
				assert.Equal(t, []string{field}, contract.SourcePath)
				assert.NotEmpty(t, contract.RuleID)
				assert.True(t, validRequestPathWireAction(contract.WireAction))
				assert.Zero(t, contract.LocalObligations&^requestPathObligationAll)
			}
		}
	}
	for clientFormat, fields := range effortAliasTopLevelFields {
		for _, field := range fields {
			for _, finalProtocol := range requestContractProtocols {
				contract, found := LookupRequestPathContract(clientFormat, finalProtocol, field)
				require.Truef(t, found, "missing effort alias row for %s -> %s field %q", clientFormat, finalProtocol, field)
				assert.Equal(t, []string{field}, contract.SourcePath)
			}
		}
	}
	assert.Len(t, requestPathContracts, expectedRows)
}

func TestRequestPathContractRegistryMatchesImplementedCrossProtocolActions(t *testing.T) {
	tests := []struct {
		name           string
		clientFormat   types.RelayFormat
		finalProtocol  Protocol
		field          string
		wantAction     RequestPathWireAction
		wantObligation RequestPathLocalObligation
	}{
		{
			name:         "Messages thinking uses raw Chat finalizer",
			clientFormat: types.RelayFormatClaude, finalProtocol: ProtocolChat,
			field: "thinking", wantAction: RequestPathWireForwardRaw,
			wantObligation: RequestPathObligationBilling | RequestPathObligationResponse,
		},
		{
			name:         "Messages stop_sequences translates to Chat stop",
			clientFormat: types.RelayFormatClaude, finalProtocol: ProtocolChat,
			field: "stop_sequences", wantAction: RequestPathWireTranslate,
			wantObligation: RequestPathObligationBilling | RequestPathObligationResponse,
		},
		{
			name:         "Messages raw stop is implemented only for Chat",
			clientFormat: types.RelayFormatClaude, finalProtocol: ProtocolChat,
			field: "stop", wantAction: RequestPathWireForwardRaw,
			wantObligation: RequestPathObligationBilling | RequestPathObligationResponse,
		},
		{
			name:         "Messages metadata translates to Chat metadata",
			clientFormat: types.RelayFormatClaude, finalProtocol: ProtocolChat,
			field: "metadata", wantAction: RequestPathWireTranslate,
			wantObligation: RequestPathObligationAffinity,
		},
		{
			name:         "Messages output config is strictly translated to Chat",
			clientFormat: types.RelayFormatClaude, finalProtocol: ProtocolChat,
			field: "output_config", wantAction: RequestPathWireTranslate,
			wantObligation: RequestPathObligationBilling | RequestPathObligationResponse,
		},
		{
			name:         "Messages no-op context is consumed locally for Chat",
			clientFormat: types.RelayFormatClaude, finalProtocol: ProtocolChat,
			field: "context_management", wantAction: RequestPathWireConsumeLocal,
			wantObligation: RequestPathObligationResponse,
		},
		{
			name:         "Chat thinking has no Responses mapping",
			clientFormat: types.RelayFormatOpenAI, finalProtocol: ProtocolResponses,
			field: "thinking", wantAction: RequestPathWireReject,
			wantObligation: RequestPathObligationBilling | RequestPathObligationResponse,
		},
		{
			name:         "Chat n one is consumed by Responses cardinality validation",
			clientFormat: types.RelayFormatOpenAI, finalProtocol: ProtocolResponses,
			field: "n", wantAction: RequestPathWireConsumeLocal,
			wantObligation: RequestPathObligationBilling | RequestPathObligationResponse,
		},
		{
			name:         "Responses metadata maps to Chat",
			clientFormat: types.RelayFormatOpenAIResponses, finalProtocol: ProtocolChat,
			field: "metadata", wantAction: RequestPathWireTranslate,
			wantObligation: RequestPathObligationAffinity,
		},
		{
			name:         "Responses metadata has no Messages mapping",
			clientFormat: types.RelayFormatOpenAIResponses, finalProtocol: ProtocolMessages,
			field: "metadata", wantAction: RequestPathWireReject,
			wantObligation: RequestPathObligationAffinity,
		},
		{
			name:         "Responses prompt cache key is consumed for Messages identity",
			clientFormat: types.RelayFormatOpenAIResponses, finalProtocol: ProtocolMessages,
			field: "prompt_cache_key", wantAction: RequestPathWireConsumeLocal,
			wantObligation: RequestPathObligationAffinity,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			contract, found := LookupRequestPathContract(test.clientFormat, test.finalProtocol, test.field)
			require.True(t, found)
			assert.Equal(t, test.wantAction, contract.WireAction)
			assert.True(t, contract.LocalObligations.Has(test.wantObligation))
		})
	}

	for _, finalProtocol := range []Protocol{ProtocolMessages, ProtocolResponses} {
		contract, found := LookupRequestPathContract(types.RelayFormatClaude, finalProtocol, "stop")
		require.True(t, found)
		assert.Equal(t, RequestPathWireReject, contract.WireAction)
		assert.Equal(t, RequestContractUnmappedPathRule, contract.RuleID)
	}
}

func TestBuildRequestPreflightPlanAcceptsRelayableUnknownTopLevelField(t *testing.T) {
	const (
		unknownField = "provider_extension"
		unknownValue = "client-private-marker"
	)
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, endpoint := range requestPreflightEndpoints {
			for _, finalProtocol := range requestContractProtocols {
				name := endpoint.name + "/" + string(finalProtocol)
				t.Run(name, func(t *testing.T) {
					c, info := newRequestContractFixture(
						t,
						channelType,
						endpoint,
						finalProtocol,
						map[string]any{unknownField: unknownValue},
					)

					plan, err := BuildRequestPreflightPlan(c, info)
					require.NoError(t, err)
					assert.Equal(t, finalProtocol, plan.FinalProtocol)
				})
			}
		}
	}
}

func TestBuildRequestPreflightPlanRejectsUnknownTopLevelTargetCollision(t *testing.T) {
	tests := []struct {
		name          string
		endpoint      requestPreflightEndpoint
		finalProtocol Protocol
		field         string
	}{
		{name: "messages to chat", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolChat, field: "n"},
		{name: "messages to responses", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolResponses, field: "include"},
		{name: "chat to messages", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolMessages, field: "system"},
		{name: "chat to responses", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolResponses, field: "include"},
		{name: "responses to chat", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat, field: "messages"},
		{name: "responses to messages", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolMessages, field: "messages"},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("type-%d/%s", channelType, test.name), func(t *testing.T) {
				c, info := newRequestContractFixture(
					t,
					channelType,
					test.endpoint,
					test.finalProtocol,
					map[string]any{test.field: "client-private-marker"},
				)

				_, err := BuildRequestPreflightPlan(c, info)
				require.Error(t, err)
				preflightErr, ok := AsRequestPreflightError(err)
				require.True(t, ok)
				assert.Equal(t, http.StatusBadRequest, preflightErr.StatusCode)
				assert.Equal(t, types.ErrorOriginLocalValidation, preflightErr.Origin)
				assert.Equal(t, RequestContractTargetCollisionRule, preflightErr.RuleID)
				assert.Equal(t, RequestContractPreflightStage, preflightErr.StageID)
				assert.Equal(t, RequestContractPublicMessage, preflightErr.Message)
				assert.NotContains(t, err.Error(), "client-private-marker")
			})
		}
	}
}

func TestBuildRequestPreflightPlanKeepsClassifiedControlReachable(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, endpoint := range requestPreflightEndpoints {
			for _, finalProtocol := range requestContractProtocols {
				name := endpoint.name + "/" + string(finalProtocol)
				t.Run(name, func(t *testing.T) {
					c, info := newRequestContractFixture(t, channelType, endpoint, finalProtocol, nil)
					plan, err := BuildRequestPreflightPlan(c, info)
					require.NoError(t, err)
					assert.Equal(t, finalProtocol, plan.FinalProtocol)
				})
			}
		}
	}
}

func TestBuildRequestPreflightPlanRejectsUnmappedNestedCrossProtocolFields(t *testing.T) {
	tests := []struct {
		name          string
		endpoint      requestPreflightEndpoint
		finalProtocol Protocol
		extraFields   map[string]any
	}{
		{
			name:          "messages to chat",
			endpoint:      requestPreflightEndpoints[0],
			finalProtocol: ProtocolChat,
			extraFields: map[string]any{"messages": []any{map[string]any{
				"role": "user", "content": []any{map[string]any{
					"type": "text", "text": "hello", "provider_extension": map[string]any{"mode": "opaque"},
				}},
			}}},
		},
		{
			name:          "messages to responses",
			endpoint:      requestPreflightEndpoints[0],
			finalProtocol: ProtocolResponses,
			extraFields: map[string]any{"messages": []any{map[string]any{
				"role": "user", "content": []any{map[string]any{
					"type": "text", "text": "hello", "provider_extension": map[string]any{"mode": "opaque"},
				}},
			}}},
		},
		{
			name:          "chat to messages",
			endpoint:      requestPreflightEndpoints[1],
			finalProtocol: ProtocolMessages,
			extraFields: map[string]any{"messages": []any{map[string]any{
				"role": "user", "content": []any{map[string]any{
					"type": "text", "text": "hello", "provider_extension": map[string]any{"mode": "opaque"},
				}},
			}}},
		},
		{
			name:          "chat to responses",
			endpoint:      requestPreflightEndpoints[1],
			finalProtocol: ProtocolResponses,
			extraFields: map[string]any{"messages": []any{map[string]any{
				"role": "user", "content": []any{map[string]any{
					"type": "text", "text": "hello", "provider_extension": map[string]any{"mode": "opaque"},
				}},
			}}},
		},
		{
			name:          "responses to chat",
			endpoint:      requestPreflightEndpoints[2],
			finalProtocol: ProtocolChat,
			extraFields: map[string]any{"input": []any{map[string]any{
				"role": "user", "content": []any{map[string]any{
					"type": "input_text", "text": "hello", "provider_extension": map[string]any{"mode": "opaque"},
				}},
			}}},
		},
		{
			name:          "responses to messages",
			endpoint:      requestPreflightEndpoints[2],
			finalProtocol: ProtocolMessages,
			extraFields: map[string]any{"input": []any{map[string]any{
				"role": "user", "content": []any{map[string]any{
					"type": "input_text", "text": "hello", "provider_extension": map[string]any{"mode": "opaque"},
				}},
			}}},
		},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("type-%d/%s", channelType, test.name), func(t *testing.T) {
				c, info := newRequestContractFixture(t, channelType, test.endpoint, test.finalProtocol, test.extraFields)
				_, err := BuildRequestPreflightPlan(c, info)
				require.Error(t, err)
				preflightErr, ok := AsRequestPreflightError(err)
				require.True(t, ok)
				assert.Equal(t, http.StatusBadRequest, preflightErr.StatusCode)
				assert.Equal(t, types.ErrorOriginLocalValidation, preflightErr.Origin)
				assert.Equal(t, RequestContractUnmappedNestedRule, preflightErr.RuleID)
				assert.Equal(t, RequestContractPreflightStage, preflightErr.StageID)
				assert.NotContains(t, err.Error(), "provider_extension")
				assert.NotContains(t, err.Error(), "opaque")
			})
		}
	}
}

func TestClaudeNativeMessagesContractPreservesOnlyEstablishedMessageExtensions(t *testing.T) {
	messages := []any{
		map[string]any{
			"role": "user", "content": "hello",
			"provider_extension": map[string]any{"mode": "opaque-user"},
		},
		map[string]any{
			"role": "assistant", "content": "reply",
			"provider_extension": map[string]any{"mode": "opaque-assistant"},
		},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		t.Run(fmt.Sprintf("type-%d/native", channelType), func(t *testing.T) {
			c, info := newRequestContractFixture(
				t, channelType, requestPreflightEndpoints[0], ProtocolMessages,
				map[string]any{"messages": messages},
			)
			_, err := BuildRequestPreflightPlan(c, info)
			require.NoError(t, err)
		})

		for _, finalProtocol := range []Protocol{ProtocolChat, ProtocolResponses} {
			t.Run(fmt.Sprintf("type-%d/%s", channelType, finalProtocol), func(t *testing.T) {
				c, info := newRequestContractFixture(
					t, channelType, requestPreflightEndpoints[0], finalProtocol,
					map[string]any{"messages": messages},
				)
				_, err := BuildRequestPreflightPlan(c, info)
				require.Error(t, err)
				preflightErr, ok := AsRequestPreflightError(err)
				require.True(t, ok)
				assert.Equal(t, RequestContractUnmappedNestedRule, preflightErr.RuleID)
				assert.NotContains(t, err.Error(), "provider_extension")
				assert.NotContains(t, err.Error(), "opaque")
			})
		}
	}
}

func TestBuildRequestPreflightPlanRejectsUnknownConverterOwnedFamilyMembers(t *testing.T) {
	const (
		unknownMember = "provider_private_nested"
		unknownValue  = "private_marker"
	)
	tests := []struct {
		name          string
		endpoint      requestPreflightEndpoint
		finalProtocol Protocol
		field         string
		value         json.RawMessage
	}{
		{
			name: "chat image child", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolMessages,
			field: "messages", value: json.RawMessage(`[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/image.png","provider_private_nested":"private_marker"}}]}]`),
		},
		{
			name: "chat audio child", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolResponses,
			field: "messages", value: json.RawMessage(`[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"ZGF0YQ==","format":"wav","provider_private_nested":"private_marker"}}]}]`),
		},
		{
			name: "chat file child", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolResponses,
			field: "messages", value: json.RawMessage(`[{"role":"user","content":[{"type":"file","file":{"file_id":"file_1","provider_private_nested":"private_marker"}}]}]`),
		},
		{
			name: "chat video child", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolMessages,
			field: "messages", value: json.RawMessage(`[{"role":"user","content":[{"type":"video_url","video_url":"https://example.invalid/video.mp4","provider_private_nested":"private_marker"}]}]`),
		},
		{
			name: "chat tool call wrapper", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolResponses,
			field: "messages", value: json.RawMessage(`[{"role":"assistant","content":"working","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"},"provider_private_nested":"private_marker"}]}]`),
		},
		{
			name: "chat tool call function", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolResponses,
			field: "messages", value: json.RawMessage(`[{"role":"assistant","content":"working","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}","provider_private_nested":"private_marker"}}]}]`),
		},
		{
			name: "chat tool declaration", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolMessages,
			field: "tools", value: json.RawMessage(`[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"},"provider_private_nested":"private_marker"}}]`),
		},
		{
			name: "chat tool declaration wrapper", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolResponses,
			field: "tools", value: json.RawMessage(`[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}},"provider_private_nested":"private_marker"}]`),
		},
		{
			name: "chat tool choice", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolResponses,
			field: "tool_choice", value: json.RawMessage(`{"type":"function","function":{"name":"lookup","provider_private_nested":"private_marker"}}`),
		},
		{
			name: "chat tool choice wrapper", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolMessages,
			field: "tool_choice", value: json.RawMessage(`{"type":"function","function":{"name":"lookup"},"provider_private_nested":"private_marker"}`),
		},
		{
			name: "messages system part", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolChat,
			field: "system", value: json.RawMessage(`[{"type":"text","text":"system prompt","provider_private_nested":"private_marker"}]`),
		},
		{
			name: "messages image source", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolChat,
			field: "messages", value: json.RawMessage(`[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"ZGF0YQ==","provider_private_nested":"private_marker"}}]}]`),
		},
		{
			name: "messages tool declaration", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolResponses,
			field: "tools", value: json.RawMessage(`[{"name":"lookup","description":"lookup data","input_schema":{"type":"object"},"provider_private_nested":"private_marker"}]`),
		},
		{
			name: "responses function tool", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolMessages,
			field: "tools", value: json.RawMessage(`[{"type":"function","name":"lookup","description":"lookup data","parameters":{"type":"object"},"provider_private_nested":"private_marker"}]`),
		},
		{
			name: "responses custom tool", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			field: "tools", value: json.RawMessage(`[{"type":"custom","name":"apply_patch","description":"Apply","provider_private_nested":"private_marker"}]`),
		},
		{
			name: "responses namespace tool", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			field: "tools", value: json.RawMessage(`[{"type":"namespace","name":"fs","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}],"provider_private_nested":"private_marker"}]`),
		},
		{
			name: "responses namespace child function", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			field: "tools", value: json.RawMessage(`[{"type":"namespace","name":"fs","tools":[{"type":"function","name":"read","parameters":{"type":"object"},"provider_private_nested":"private_marker"}]}]`),
		},
		{
			name: "responses function tool choice", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			field: "tool_choice", value: json.RawMessage(`{"type":"function","name":"lookup","provider_private_nested":"private_marker"}`),
		},
		{
			name: "responses namespace tool choice child", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			field: "tool_choice", value: json.RawMessage(`{"type":"namespace","namespace":"fs","function":{"name":"read","provider_private_nested":"private_marker"}}`),
		},
		{
			name: "responses hosted tool choice", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			field: "tool_choice", value: json.RawMessage(`{"type":"web_search_preview","provider_private_nested":"private_marker"}`),
		},
		{
			name: "responses text format", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			field: "text", value: json.RawMessage(`{"format":{"type":"text","provider_private_nested":"private_marker"}}`),
		},
		{
			name: "responses text wrapper", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			field: "text", value: json.RawMessage(`{"format":{"type":"text"},"provider_private_nested":"private_marker"}`),
		},
		{
			name: "responses top-level reasoning", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			field: "reasoning", value: json.RawMessage(`{"effort":"low","provider_private_nested":"private_marker"}`),
		},
		{
			name: "responses reasoning item", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			field: "input", value: json.RawMessage(`[{"type":"reasoning","id":"rs_1","status":"completed","summary":[{"type":"summary_text","text":"why"}],"provider_private_nested":"private_marker"}]`),
		},
		{
			name: "responses reasoning summary part", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			field: "input", value: json.RawMessage(`[{"type":"reasoning","id":"rs_1","status":"completed","summary":[{"type":"summary_text","text":"why","provider_private_nested":"private_marker"}]}]`),
		},
		{
			name: "responses function call", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			field: "input", value: json.RawMessage(`[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}","provider_private_nested":"private_marker"}]`),
		},
		{
			name: "responses function output", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolMessages,
			field: "input", value: json.RawMessage(`[{"type":"function_call_output","call_id":"call_1","output":"done","provider_private_nested":"private_marker"}]`),
		},
		{
			name: "responses custom call", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolMessages,
			field: "input", value: json.RawMessage(`[{"type":"custom_tool_call","call_id":"call_1","name":"apply_patch","input":"patch","provider_private_nested":"private_marker"}]`),
		},
		{
			name: "responses custom call to chat", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			field: "input", value: json.RawMessage(`[{"type":"custom_tool_call","call_id":"call_1","name":"apply_patch","input":"patch","provider_private_nested":"private_marker"}]`),
		},
		{
			name: "responses custom output", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolMessages,
			field: "input", value: json.RawMessage(`[{"type":"custom_tool_call_output","call_id":"call_1","output":"done","provider_private_nested":"private_marker"}]`),
		},
		{
			name: "responses hosted tool", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			field: "tools", value: json.RawMessage(`[{"type":"web_search_preview","search_context_size":"medium","provider_private_nested":"private_marker"}]`),
		},
		{
			name: "responses non-function tool choice", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			field: "tool_choice", value: json.RawMessage(`{"type":"computer","provider_private_nested":"private_marker"}`),
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
					map[string]any{test.field: test.value},
				)

				_, err := BuildRequestPreflightPlan(c, info)
				require.Error(t, err)
				preflightErr, ok := AsRequestPreflightError(err)
				require.True(t, ok)
				assert.Equal(t, http.StatusBadRequest, preflightErr.StatusCode)
				assert.Equal(t, types.ErrorOriginLocalValidation, preflightErr.Origin)
				assert.Equal(t, RequestContractUnmappedNestedRule, preflightErr.RuleID)
				assert.Equal(t, RequestContractPreflightStage, preflightErr.StageID)
				assert.Equal(t, RequestContractPublicMessage, preflightErr.Message)
				assert.NotContains(t, err.Error(), unknownMember)
				assert.NotContains(t, err.Error(), unknownValue)
			})
		}
	}
}

func TestBuildRequestPreflightPlanKeepsOpaqueConverterFamiliesReachable(t *testing.T) {
	tests := []struct {
		name          string
		endpoint      requestPreflightEndpoint
		finalProtocol Protocol
		extraFields   map[string]any
	}{
		{
			name: "chat tool result content", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolMessages,
			extraFields: map[string]any{"messages": json.RawMessage(`[{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"done","provider_extension":{"big":9007199254740993}}]}]`)},
		},
		{
			name: "messages tool input", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolChat,
			extraFields: map[string]any{"messages": json.RawMessage(`[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{"provider_extension":{"big":9007199254740993}}}]}]`)},
		},
		{
			name: "responses custom call", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			extraFields: map[string]any{"input": json.RawMessage(`[{"type":"custom_tool_call","call_id":"call_1","name":"apply_patch","input":{"patch":"*** Begin Patch"}}]`)},
		},
		{
			name: "responses custom output", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			extraFields: map[string]any{"input": json.RawMessage(`[{"type":"custom_tool_call_output","call_id":"call_1","output":{"ok":true}}]`)},
		},
		{
			name: "responses custom tool declaration", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			extraFields: map[string]any{"tools": json.RawMessage(`[{"type":"custom","name":"apply_patch","description":"Apply a patch"}]`)},
		},
		{
			name: "responses json schema format", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			extraFields: map[string]any{"text": json.RawMessage(`{"format":{"type":"json_schema","name":"result","schema":{"type":"object","provider_schema_extension":{"big":9007199254740993}}}}`)},
		},
		{
			name: "responses reasoning summary", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			extraFields: map[string]any{"input": json.RawMessage(`[{"type":"reasoning","id":"rs_1","status":"completed","summary":[{"type":"summary_text","text":"short"}]}]`)},
		},
		{
			name: "responses reasoning full content", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat,
			extraFields: map[string]any{"input": json.RawMessage(`[{"type":"reasoning","id":"rs_1","status":"completed","summary":[{"type":"summary_text","text":"short"}],"content":[{"type":"reasoning_text","text":"full"}]}]`)},
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
				plan, err := BuildRequestPreflightPlan(c, info)
				require.NoError(t, err)
				assert.Equal(t, test.finalProtocol, plan.FinalProtocol)
			})
		}
	}
}

func TestClaudeMessagesCacheParentContractAcceptsLegalReplayShapes(t *testing.T) {
	marker := map[string]any{"type": "ephemeral"}
	tests := []struct {
		name     string
		messages []any
		assert   func(t *testing.T, root map[string]any)
	}{
		{
			name: "assistant thinking requires and preserves signature",
			messages: []any{
				map[string]any{"role": "user", "content": "hello"},
				map[string]any{"role": "assistant", "content": []any{map[string]any{
					"type": "thinking", "thinking": "reasoning", "signature": "signed-replay",
				}}},
			},
			assert: func(t *testing.T, root map[string]any) {
				messages := root["messages"].([]any)
				thinking := messages[1].(map[string]any)["content"].([]any)[0].(map[string]any)
				assert.Equal(t, "signed-replay", thinking["signature"])
			},
		},
		{
			name: "assistant text and tool use may share one native turn",
			messages: []any{
				map[string]any{"role": "user", "content": "hello"},
				map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "text", "text": "calling tool"},
					map[string]any{
						"type": "tool_use", "id": "call-1", "name": "lookup",
						"input": map[string]any{"query": "value"},
					},
				}},
			},
			assert: func(t *testing.T, root map[string]any) {
				messages := root["messages"].([]any)
				content := messages[1].(map[string]any)["content"].([]any)
				require.Len(t, content, 2)
				assert.Equal(t, "text", content[0].(map[string]any)["type"])
				assert.Equal(t, "tool_use", content[1].(map[string]any)["type"])
			},
		},
		{
			name: "user tool result preserves is error",
			messages: []any{
				map[string]any{"role": "assistant", "content": []any{map[string]any{
					"type": "tool_use", "id": "call-1", "name": "lookup", "input": map[string]any{},
				}}},
				map[string]any{"role": "user", "content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": "call-1", "content": "failed", "is_error": true,
				}}},
			},
			assert: func(t *testing.T, root map[string]any) {
				messages := root["messages"].([]any)
				result := messages[1].(map[string]any)["content"].([]any)[0].(map[string]any)
				assert.Equal(t, true, result["is_error"])
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, info := newRequestContractFixture(
				t,
				constant.ChannelTypeOpenCodeAPIKey,
				requestPreflightEndpoints[0],
				ProtocolMessages,
				map[string]any{
					"system": []any{map[string]any{
						"type": "text", "text": "system", "cache_control": marker,
					}},
					"messages": test.messages,
				},
			)

			body := convertAndFinalizeRequestForPresenceTest(t, c, info)
			var root map[string]any
			require.NoError(t, common.Unmarshal(body, &root))
			test.assert(t, root)
		})
	}
}

func TestClaudeMessagesContractPreservesValidatedDocumentBlocks(t *testing.T) {
	tests := []struct {
		name   string
		source map[string]any
	}{
		{
			name: "base64 pdf",
			source: map[string]any{
				"type": "base64", "media_type": "application/pdf", "data": "AA==",
			},
		},
		{
			name: "plain text",
			source: map[string]any{
				"type": "text", "media_type": "text/plain", "data": "synthetic document",
			},
		},
		{
			name:   "url pdf",
			source: map[string]any{"type": "url", "url": "https://example.invalid/document.pdf"},
		},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("type-%d/%s", channelType, test.name), func(t *testing.T) {
				document := map[string]any{
					"type": "document", "source": test.source,
					"citations": map[string]any{"enabled": true},
					"context":   "synthetic context", "title": "synthetic title",
				}
				c, info := newRequestContractFixture(
					t,
					channelType,
					requestPreflightEndpoints[0],
					ProtocolMessages,
					map[string]any{
						"system": []any{map[string]any{
							"type": "text", "text": "system",
							"cache_control": map[string]any{"type": "ephemeral"},
						}},
						"messages": []any{map[string]any{
							"role": "user", "content": []any{document},
						}},
					},
				)

				body := convertAndFinalizeRequestForPresenceTest(t, c, info)
				var root map[string]any
				require.NoError(t, common.Unmarshal(body, &root))
				messages := root["messages"].([]any)
				content := messages[0].(map[string]any)["content"].([]any)
				assert.Equal(t, document, content[0])
			})
		}
	}
}

func TestClaudeMessagesContractRejectsDocumentWithoutTargetRepresentation(t *testing.T) {
	document := map[string]any{
		"type": "document",
		"source": map[string]any{
			"type": "base64", "media_type": "application/pdf", "data": "AA==",
		},
	}
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, finalProtocol := range []Protocol{ProtocolChat, ProtocolResponses} {
			t.Run(fmt.Sprintf("type-%d/%s", channelType, finalProtocol), func(t *testing.T) {
				c, info := newRequestContractFixture(
					t,
					channelType,
					requestPreflightEndpoints[0],
					finalProtocol,
					map[string]any{"messages": []any{map[string]any{
						"role": "user", "content": []any{document},
					}}},
				)

				_, err := BuildRequestPreflightPlan(c, info)
				require.Error(t, err)
				preflightErr, ok := AsRequestPreflightError(err)
				require.True(t, ok)
				assert.Equal(t, http.StatusBadRequest, preflightErr.StatusCode)
				assert.Equal(t, RequestContractUnmappedNestedRule, preflightErr.RuleID)
				assert.Equal(t, RequestContractPublicMessage, preflightErr.Message)
			})
		}
	}
}

func TestClaudeDocumentFixtureRejectsAfterKnownOptionalCacheDrop(t *testing.T) {
	marker := map[string]any{"type": "ephemeral"}
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			c, info := newRequestContractFixture(
				t,
				channelType,
				requestPreflightEndpoints[0],
				ProtocolChat,
				map[string]any{
					"system": []any{map[string]any{
						"type": "text", "text": "system", "cache_control": marker,
					}},
					"tools": []any{map[string]any{
						"name": "lookup", "input_schema": map[string]any{"type": "object"},
						"cache_control": marker,
					}},
					"messages": []any{map[string]any{
						"role": "user", "content": []any{
							map[string]any{"type": "text", "text": "question", "cache_control": marker},
							map[string]any{
								"type": "document",
								"source": map[string]any{
									"type": "base64", "media_type": "application/pdf", "data": "AA==",
								},
							},
						},
					}},
				},
			)
			settings := dto.ChannelOtherSettings{OpenCodeGo: &dto.OpenCodeGoConfig{
				ModelProtocols:                 map[string]string{"glm-5.2": string(ProtocolChat)},
				UnsupportedOptionalFieldPolicy: dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
			}}
			common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, settings)

			envelope, found, err := helper.GetValidatedRequestEnvelope(c, types.RelayFormatClaude)
			require.NoError(t, err)
			require.True(t, found)
			cachePlan, err := BuildCacheControlDispositionPlan(
				envelope,
				types.RelayFormatClaude,
				ProtocolChat,
				info.Request,
				dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
			)
			require.NoError(t, err)
			assert.Equal(t, 3, cachePlan.DropCount)
			assert.Zero(t, cachePlan.PreserveCount)

			_, err = BuildRequestPreflightPlan(c, info)
			require.Error(t, err)
			preflightErr, ok := AsRequestPreflightError(err)
			require.True(t, ok)
			assert.Equal(t, http.StatusBadRequest, preflightErr.StatusCode)
			assert.Equal(t, RequestContractUnmappedNestedRule, preflightErr.RuleID)
			assert.Equal(t, RequestContractPreflightStage, preflightErr.StageID)
			assert.Equal(t, RequestContractPublicMessage, preflightErr.Message)
		})
	}
}

func TestClaudeMessagesCacheParentContractRejectsInvalidReplayShapes(t *testing.T) {
	imageSource := func() map[string]any {
		return map[string]any{
			"type": "base64", "media_type": "image/png", "data": "aGVsbG8=",
		}
	}
	tests := []struct {
		name     string
		messages []any
		tools    any
	}{
		{
			name: "assistant image",
			messages: []any{map[string]any{"role": "assistant", "content": []any{map[string]any{
				"type": "image", "source": imageSource(),
			}}}},
		},
		{
			name: "user tool use",
			messages: []any{map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "tool_use", "id": "call-1", "name": "lookup", "input": map[string]any{},
			}}}},
		},
		{
			name: "assistant tool result",
			messages: []any{map[string]any{"role": "assistant", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": "call-1", "content": "done",
			}}}},
		},
		{
			name: "user thinking",
			messages: []any{map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "thinking", "thinking": "reasoning", "signature": "signed-replay",
			}}}},
		},
		{
			name: "thinking missing signature",
			messages: []any{map[string]any{"role": "assistant", "content": []any{map[string]any{
				"type": "thinking", "thinking": "reasoning",
			}}}},
		},
		{
			name: "thinking empty signature",
			messages: []any{map[string]any{"role": "assistant", "content": []any{map[string]any{
				"type": "thinking", "thinking": "reasoning", "signature": "",
			}}}},
		},
		{
			name: "tool result is error wrong type",
			messages: []any{map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": "call-1", "content": "failed", "is_error": "yes",
			}}}},
		},
		{
			name: "image source unsupported media type",
			messages: []any{map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "image", "source": map[string]any{
					"type": "base64", "media_type": "text/plain", "data": "aGVsbG8=",
				},
			}}}},
		},
		{
			name:     "tool description wrong type",
			messages: []any{map[string]any{"role": "user", "content": "hello"}},
			tools: []any{map[string]any{
				"name": "lookup", "description": true, "input_schema": map[string]any{"type": "object"},
			}},
		},
		{
			name:     "tool schema missing object discriminator",
			messages: []any{map[string]any{"role": "user", "content": "hello"}},
			tools: []any{map[string]any{
				"name": "lookup", "input_schema": map[string]any{"properties": map[string]any{}},
			}},
		},
	}

	marker := map[string]any{"type": "ephemeral"}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			extra := map[string]any{
				"system": []any{map[string]any{
					"type": "text", "text": "system", "cache_control": marker,
				}},
				"messages": test.messages,
			}
			if test.tools != nil {
				extra["tools"] = test.tools
			}
			c, info := newRequestContractFixture(
				t,
				constant.ChannelTypeOpenCodeAPIKey,
				requestPreflightEndpoints[0],
				ProtocolMessages,
				extra,
			)

			_, err := BuildRequestPreflightPlan(c, info)
			require.Error(t, err)
			preflightErr, ok := AsRequestPreflightError(err)
			require.True(t, ok)
			assert.Equal(t, http.StatusBadRequest, preflightErr.StatusCode)
			assert.Equal(t, RequestContractUnmappedNestedRule, preflightErr.RuleID)
			assert.Equal(t, RequestContractPreflightStage, preflightErr.StageID)
			assert.NotContains(t, err.Error(), test.name)
		})
	}
}

func TestBuildRequestPreflightPlanAcceptsMappedNestedCrossProtocolControls(t *testing.T) {
	claudeFields := map[string]any{
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "text", "text": "hello",
			}},
		}},
		"tools": []any{map[string]any{
			"name": "lookup", "description": "lookup data",
			"input_schema": map[string]any{
				"type": "object", "properties": map[string]any{
					"query": map[string]any{"type": "string", "provider_schema_extension": 1},
				},
			},
		}},
	}
	chatFields := map[string]any{
		"messages": []any{map[string]any{
			"role": "user", "content": []any{map[string]any{"type": "text", "text": "hello"}},
		}},
		"tools": []any{map[string]any{
			"type": "function", "function": map[string]any{
				"name": "lookup", "description": "lookup data",
				"parameters": map[string]any{
					"type": "object", "properties": map[string]any{
						"query": map[string]any{"type": "string", "provider_schema_extension": 1},
					},
				},
			},
		}},
		"tool_choice": map[string]any{
			"type": "function", "function": map[string]any{"name": "lookup"},
		},
	}
	responsesFields := map[string]any{
		"input": []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "hello"}},
		}},
		"tools": []any{map[string]any{
			"type": "function", "name": "lookup", "description": "lookup data",
			"parameters": map[string]any{
				"type": "object", "properties": map[string]any{
					"query": map[string]any{"type": "string", "provider_schema_extension": 1},
				},
			},
		}},
		"tool_choice": map[string]any{"type": "function", "name": "lookup"},
	}
	tests := []struct {
		name          string
		endpoint      requestPreflightEndpoint
		finalProtocol Protocol
		extraFields   map[string]any
	}{
		{name: "messages to chat", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolChat, extraFields: claudeFields},
		{name: "messages to responses", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolResponses, extraFields: claudeFields},
		{name: "chat to messages", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolMessages, extraFields: chatFields},
		{name: "chat to responses", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolResponses, extraFields: chatFields},
		{name: "responses to chat", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat, extraFields: responsesFields},
		{name: "responses to messages", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolMessages, extraFields: responsesFields},
		{
			name: "chat function tools stay on configured chat", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolChat,
			extraFields: chatFields,
		},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("type-%d/%s", channelType, test.name), func(t *testing.T) {
				c, info := newRequestContractFixture(t, channelType, test.endpoint, test.finalProtocol, test.extraFields)
				plan, err := BuildRequestPreflightPlan(c, info)
				require.NoError(t, err)
				assert.Equal(t, test.finalProtocol, plan.FinalProtocol)
			})
		}
	}
}

func TestValidateFinalizedRequestPathContractsFailsClosed(t *testing.T) {
	for _, finalProtocol := range requestContractProtocols {
		validBody := map[string]any{"model": "glm-5.2", "stream": false}
		switch finalProtocol {
		case ProtocolChat:
			validBody["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
		case ProtocolMessages:
			validBody["max_tokens"] = 16
			validBody["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
		case ProtocolResponses:
			validBody["input"] = "hello"
		}
		encoded, err := common.Marshal(validBody)
		require.NoError(t, err)
		require.NoError(t, ValidateFinalizedRequestPathContracts(encoded, finalProtocol))

		validBody["future_output_copies"] = 64
		encoded, err = common.Marshal(validBody)
		require.NoError(t, err)
		err = ValidateFinalizedRequestPathContracts(encoded, finalProtocol)
		require.EqualError(t, err, RequestContractFinalizedMessage)
		assert.NotContains(t, err.Error(), "future_output_copies")
	}
}

func requestContractJSONFields(dtoType reflect.Type) []string {
	fields := make([]string, 0, dtoType.NumField())
	for index := 0; index < dtoType.NumField(); index++ {
		name := strings.Split(dtoType.Field(index).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		fields = append(fields, name)
	}
	return fields
}

func newRequestContractFixture(
	t *testing.T,
	channelType int,
	endpoint requestPreflightEndpoint,
	finalProtocol Protocol,
	extraFields map[string]any,
) (*gin.Context, *relaycommon.RelayInfo) {
	t.Helper()
	body := map[string]any{"model": "glm-5.2"}
	switch endpoint.format {
	case types.RelayFormatClaude:
		body["max_tokens"] = 16
		body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
	case types.RelayFormatOpenAI:
		body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
	case types.RelayFormatOpenAIResponses:
		body["input"] = "hello"
	default:
		t.Fatalf("unsupported relay format %q", endpoint.format)
	}
	for field, value := range extraFields {
		body[field] = value
	}
	encoded, err := common.Marshal(body)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, endpoint.path, bytes.NewReader(encoded))
	storage, err := common.CreateBodyStorage(encoded)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })
	c.Set(common.KeyBodyStorage, storage)
	common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
	common.SetContextKey(c, constant.ContextKeyChannelId, 6200+channelType)
	common.SetContextKey(c, constant.ContextKeyChannelModelMapping, "")
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{
		OpenCodeGo: &dto.OpenCodeGoConfig{
			ModelProtocols: map[string]string{"glm-5.2": string(finalProtocol)},
		},
	})
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "glm-5.2")

	request, err := helper.GetAndValidateRequest(c, endpoint.format)
	require.NoError(t, err)
	info, err := relaycommon.GenRelayInfo(c, endpoint.format, request, nil)
	require.NoError(t, err)
	return c, info
}
