package opencodego

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func TestCacheControlContractRegisteredExplicitLocations(t *testing.T) {
	tests := []struct {
		name string
		body map[string]any
	}{
		{
			name: "system text",
			body: map[string]any{"system": []any{cacheSystemText("system")}},
		},
		{
			name: "message-level system text",
			body: map[string]any{"messages": []any{
				cacheMessage("system", cacheText("system")),
				cacheMessage("user", map[string]any{"type": "text", "text": "hello"}),
			}},
		},
		{
			name: "user text",
			body: map[string]any{"messages": []any{cacheMessage("user", cacheText("user"))}},
		},
		{
			name: "assistant text",
			body: map[string]any{"messages": []any{cacheMessage("assistant", cacheText("assistant"))}},
		},
		{
			name: "user image",
			body: map[string]any{"messages": []any{cacheMessage("user", map[string]any{
				"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AA=="},
				"cache_control": cacheMarker("5m"),
			})}},
		},
		{
			name: "assistant tool use",
			body: map[string]any{"messages": []any{cacheMessage("assistant", map[string]any{
				"type": "tool_use", "id": "call-1", "name": "lookup", "input": map[string]any{"q": "x"},
				"cache_control": cacheMarker("5m"),
			})}},
		},
		{
			name: "user outer tool result",
			body: map[string]any{"messages": []any{cacheMessage("user", map[string]any{
				"type": "tool_result", "tool_use_id": "call-1", "content": "ok",
				"cache_control": cacheMarker("5m"),
			})}},
		},
		{
			name: "tool definition",
			body: map[string]any{"tools": []any{map[string]any{
				"name": "lookup", "description": "lookup", "input_schema": map[string]any{"type": "object"},
				"cache_control": cacheMarker("5m"),
			}}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			envelope, request := newCacheControlContractFixture(t, test.body)
			plan, err := BuildCacheControlDispositionPlan(
				envelope,
				types.RelayFormatClaude,
				ProtocolMessages,
				request,
				dto.OpenCodeGoUnsupportedOptionalFieldStrict,
			)
			require.NoError(t, err)
			assert.Equal(t, 1, plan.PreserveCount)
			assert.Zero(t, plan.DropCount)
			require.Len(t, plan.Entries, 1)
			assert.Equal(t, CacheControlActionPreserve, plan.Entries[0].Action)
			assert.NotEmpty(t, plan.Canonical)
			assert.NotEmpty(t, plan.Fingerprint)
		})
	}
}

func TestCacheControlContractNormalizesCapturedSystemRoleTextMarker(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, finalProtocol := range []Protocol{ProtocolMessages, ProtocolChat, ProtocolResponses} {
			for _, policy := range []string{
				dto.OpenCodeGoUnsupportedOptionalFieldStrict,
				dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
			} {
				name := "type-" + strconv.Itoa(channelType) + "/" + string(finalProtocol) + "/" + policy
				t.Run(name, func(t *testing.T) {
					c, info := newCacheControlPreflightFixture(
						t,
						capturedSystemRoleCacheBody(),
						channelType,
						finalProtocol,
						policy,
					)
					plan, err := BuildRequestPreflightPlan(c, info)
					if finalProtocol != ProtocolMessages && policy == dto.OpenCodeGoUnsupportedOptionalFieldStrict {
						requireCacheControlPreflightError(
							t, err, CacheControlUnsupportedRule, CacheControlPreflightStage,
						)
						return
					}
					require.NoError(t, err)
					cachePlan, err := cacheControlDispositionPlanFromPreflight(plan)
					require.NoError(t, err)
					require.Len(t, cachePlan.Entries, 3)

					var systemMessageEntry *CacheControlDisposition
					for index := range cachePlan.Entries {
						entry := &cachePlan.Entries[index]
						if len(entry.SourcePath) == 5 && entry.SourcePath[0].Key == "messages" &&
							entry.SourcePath[1].Index == 1 {
							systemMessageEntry = entry
						}
					}
					require.NotNil(t, systemMessageEntry)
					require.Len(t, systemMessageEntry.NormalizedSourcePath, 3)
					assert.Equal(t, "system", systemMessageEntry.NormalizedSourcePath[0].Key)
					assert.Equal(t, 3, systemMessageEntry.NormalizedSourcePath[1].Index)
					assert.Equal(t, cacheControlRuleSystem, systemMessageEntry.RuleID)

					wire := convertAndFinalizeRequestForPresenceTest(t, c, info)
					root, err := decodeCacheControlFinalBody(wire)
					require.NoError(t, err)
					if finalProtocol == ProtocolMessages {
						system, ok := root.(map[string]any)["system"].([]any)
						require.True(t, ok)
						require.Len(t, system, 4)
						for _, index := range []int{1, 2, 3} {
							marker, present := system[index].(map[string]any)["cache_control"]
							assert.True(t, present)
							assert.Equal(t, "ephemeral", marker.(map[string]any)["type"])
						}
						messages := root.(map[string]any)["messages"].([]any)
						assert.Len(t, messages, 1)
						assert.Equal(t, "user", messages[0].(map[string]any)["role"])
					} else {
						assert.Empty(t, targetOwnedCacheControlPaths(root, finalProtocol))
						assert.Contains(t, string(wire), "message-system")
					}
				})
			}
		}
	}
}

func TestCacheControlContractToolUseInputPreservesUnknownJSONValues(t *testing.T) {
	tests := []struct {
		name  string
		input any
	}{
		{name: "object", input: map[string]any{"query": "value"}},
		{name: "array", input: []any{"value", true}},
		{name: "string", input: "value"},
		{name: "boolean", input: true},
		{name: "null", input: nil},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, info := newCacheControlPreflightFixture(
				t,
				map[string]any{"messages": []any{cacheMessage("assistant", map[string]any{
					"type": "tool_use", "id": "call-1", "name": "lookup", "input": test.input,
					"cache_control": cacheMarker("5m"),
				})}},
				constant.ChannelTypeOpenCodeAPIKey,
				ProtocolMessages,
				dto.OpenCodeGoUnsupportedOptionalFieldStrict,
			)

			result := convertAndFinalizeRequestForPresenceTest(t, c, info)
			root, err := decodeCacheControlFinalBody(result)
			require.NoError(t, err)
			input, present := cacheControlValueAtPath(root, cachePath(
				cacheKey("messages"), cacheIndex(0), cacheKey("content"), cacheIndex(0), cacheKey("input"),
			))
			require.True(t, present)
			assert.Equal(t, test.input, input)
		})
	}
}

func TestCacheControlContractRegisteredLocationsFinalWireMatrix(t *testing.T) {
	locations := []struct {
		name string
		body map[string]any
	}{
		{
			name: "system text",
			body: map[string]any{"system": []any{cacheSystemText("system")}},
		},
		{
			name: "message-level system text",
			body: map[string]any{"messages": []any{
				cacheMessage("system", cacheText("system")),
				cacheMessage("user", map[string]any{"type": "text", "text": "hello"}),
			}},
		},
		{
			name: "user text",
			body: map[string]any{"messages": []any{cacheMessage("user", cacheText("user"))}},
		},
		{
			name: "assistant text",
			body: map[string]any{"messages": []any{cacheMessage("assistant", cacheText("assistant"))}},
		},
		{
			name: "user image",
			body: map[string]any{"messages": []any{cacheMessage("user", map[string]any{
				"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AA=="},
				"cache_control": cacheMarker("5m"),
			})}},
		},
		{
			name: "assistant tool use",
			body: map[string]any{"messages": []any{cacheMessage("assistant", map[string]any{
				"type": "tool_use", "id": "call-1", "name": "lookup", "input": map[string]any{"q": "x"},
				"cache_control": cacheMarker("5m"),
			})}},
		},
		{
			name: "user outer tool result",
			body: map[string]any{"messages": []any{cacheMessage("user", map[string]any{
				"type": "tool_result", "tool_use_id": "call-1", "content": "ok",
				"cache_control": cacheMarker("5m"),
			})}},
		},
		{
			name: "tool definition",
			body: map[string]any{"tools": []any{map[string]any{
				"name": "lookup", "description": "lookup", "input_schema": map[string]any{"type": "object"},
				"cache_control": cacheMarker("5m"),
			}}},
		},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, location := range locations {
			name := "preserve/type-" + strconv.Itoa(channelType) + "/" + location.name
			t.Run(name, func(t *testing.T) {
				c, info := newCacheControlPreflightFixture(
					t,
					location.body,
					channelType,
					ProtocolMessages,
					dto.OpenCodeGoUnsupportedOptionalFieldStrict,
				)
				result := convertAndFinalizeRequestForPresenceTest(t, c, info)
				root, err := decodeCacheControlFinalBody(result)
				require.NoError(t, err)
				paths := targetOwnedCacheControlPaths(root, ProtocolMessages)
				require.Len(t, paths, 1)
				for _, path := range paths {
					value, present := cacheControlValueAtPath(root, path)
					require.True(t, present)
					digest, digestErr := cacheControlMarkerDigestFromValue(value)
					require.NoError(t, digestErr)
					assert.Equal(t, cacheControlMarkerDigest("5m"), digest)
				}
			})

			for _, finalProtocol := range []Protocol{ProtocolChat, ProtocolResponses} {
				name := "drop/type-" + strconv.Itoa(channelType) + "/" + string(finalProtocol) + "/" + location.name
				t.Run(name, func(t *testing.T) {
					c, info := newCacheControlPreflightFixture(
						t,
						location.body,
						channelType,
						finalProtocol,
						dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
					)
					result := convertAndFinalizeRequestForPresenceTest(t, c, info)
					root, err := decodeCacheControlFinalBody(result)
					require.NoError(t, err)
					assert.Empty(t, targetOwnedCacheControlPaths(root, finalProtocol))
				})
			}
		}
	}
}

func TestCacheControlContractTargetDispositionMatrix(t *testing.T) {
	explicitBody := map[string]any{
		"messages": []any{cacheMessage("user", cacheText("hello"))},
	}
	automaticBody := map[string]any{
		"cache_control": cacheMarker("5m"),
		"messages":      []any{cacheMessage("user", map[string]any{"type": "text", "text": "hello"})},
	}

	for _, test := range []struct {
		name          string
		body          map[string]any
		finalProtocol Protocol
		policy        string
		wantAction    CacheControlAction
		wantError     bool
	}{
		{name: "native explicit strict preserves", body: explicitBody, finalProtocol: ProtocolMessages, policy: dto.OpenCodeGoUnsupportedOptionalFieldStrict, wantAction: CacheControlActionPreserve},
		{name: "native explicit compatibility preserves", body: explicitBody, finalProtocol: ProtocolMessages, policy: dto.OpenCodeGoUnsupportedOptionalFieldDropKnown, wantAction: CacheControlActionPreserve},
		{name: "chat explicit strict rejects", body: explicitBody, finalProtocol: ProtocolChat, policy: dto.OpenCodeGoUnsupportedOptionalFieldStrict, wantError: true},
		{name: "chat explicit compatibility drops", body: explicitBody, finalProtocol: ProtocolChat, policy: dto.OpenCodeGoUnsupportedOptionalFieldDropKnown, wantAction: CacheControlActionDrop},
		{name: "responses explicit strict rejects", body: explicitBody, finalProtocol: ProtocolResponses, policy: dto.OpenCodeGoUnsupportedOptionalFieldStrict, wantError: true},
		{name: "responses explicit compatibility drops", body: explicitBody, finalProtocol: ProtocolResponses, policy: dto.OpenCodeGoUnsupportedOptionalFieldDropKnown, wantAction: CacheControlActionDrop},
		{name: "native automatic strict rejects", body: automaticBody, finalProtocol: ProtocolMessages, policy: dto.OpenCodeGoUnsupportedOptionalFieldStrict, wantError: true},
		{name: "native automatic compatibility drops", body: automaticBody, finalProtocol: ProtocolMessages, policy: dto.OpenCodeGoUnsupportedOptionalFieldDropKnown, wantAction: CacheControlActionDrop},
	} {
		t.Run(test.name, func(t *testing.T) {
			envelope, request := newCacheControlContractFixture(t, test.body)
			plan, err := BuildCacheControlDispositionPlan(
				envelope, types.RelayFormatClaude, test.finalProtocol, request, test.policy,
			)
			if test.wantError {
				requireCacheControlClientRule(t, err, CacheControlUnsupportedRule)
				return
			}
			require.NoError(t, err)
			require.Len(t, plan.Entries, 1)
			assert.Equal(t, test.wantAction, plan.Entries[0].Action)
		})
	}
}

func TestCacheControlPreflightCartesianMatrix(t *testing.T) {
	streamStates := []struct {
		name  string
		value *bool
	}{
		{name: "absent"},
		{name: "false", value: common.GetPointer(false)},
		{name: "true", value: common.GetPointer(true)},
	}
	protocols := []Protocol{ProtocolMessages, ProtocolChat, ProtocolResponses}
	policies := []string{
		dto.OpenCodeGoUnsupportedOptionalFieldStrict,
		dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, endpoint := range requestPreflightEndpoints {
			for _, finalProtocol := range protocols {
				for _, policy := range policies {
					for _, stream := range streamStates {
						name := "type-" + strconv.Itoa(channelType) + "/" + endpoint.name + "-to-" +
							string(finalProtocol) + "/" + policy + "/stream-" + stream.name
						t.Run(name, func(t *testing.T) {
							extra := cacheControlMatrixSourceFields(endpoint.format)
							if stream.value != nil {
								extra["stream"] = *stream.value
							}
							c, info := newRequestContractFixture(t, channelType, endpoint, finalProtocol, extra)
							common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{
								OpenCodeGo: &dto.OpenCodeGoConfig{
									ModelProtocols:                 map[string]string{"glm-5.2": string(finalProtocol)},
									UnsupportedOptionalFieldPolicy: policy,
								},
							})

							envelope, found, err := helper.GetValidatedRequestEnvelope(c, endpoint.format)
							require.NoError(t, err)
							require.True(t, found)
							present, value, valid := envelope.Stream()
							if stream.value == nil {
								assert.False(t, present)
							} else {
								assert.True(t, present)
								assert.True(t, valid)
								assert.Equal(t, *stream.value, value)
							}

							plan, err := BuildRequestPreflightPlan(c, info)
							if endpoint.format != types.RelayFormatClaude {
								requireCacheControlPreflightError(
									t, err, RequestContractUnmappedNestedRule, RequestContractPreflightStage,
								)
								return
							}
							if finalProtocol != ProtocolMessages && policy == dto.OpenCodeGoUnsupportedOptionalFieldStrict {
								requireCacheControlPreflightError(
									t, err, CacheControlUnsupportedRule, CacheControlPreflightStage,
								)
								return
							}

							require.NoError(t, err)
							assert.Equal(t, channelType, plan.ChannelType)
							assert.Equal(t, endpoint.format, plan.ClientFormat)
							assert.Equal(t, finalProtocol, plan.FinalProtocol)
							assert.Equal(t, policy, plan.UnsupportedOptionalFieldPolicy)
							if finalProtocol == ProtocolMessages {
								assert.Equal(t, 1, plan.CacheControlPreserveCount)
								assert.Zero(t, plan.CacheControlDropCount)
							} else {
								assert.Zero(t, plan.CacheControlPreserveCount)
								assert.Equal(t, 1, plan.CacheControlDropCount)
							}
						})
					}
				}
			}
		}
	}
}

func cacheControlMatrixSourceFields(format types.RelayFormat) map[string]any {
	marker := cacheMarker("5m")
	switch format {
	case types.RelayFormatClaude:
		return map[string]any{"system": []any{map[string]any{
			"type": "text", "text": "matrix", "cache_control": marker,
		}}}
	case types.RelayFormatOpenAI:
		return map[string]any{"messages": []any{map[string]any{
			"role": "user", "content": []any{map[string]any{
				"type": "text", "text": "matrix", "cache_control": marker,
			}},
		}}}
	case types.RelayFormatOpenAIResponses:
		return map[string]any{"input": []any{map[string]any{
			"type": "message", "role": "user", "content": []any{map[string]any{
				"type": "input_text", "text": "matrix", "cache_control": marker,
			}},
		}}}
	default:
		return nil
	}
}

func requireCacheControlPreflightError(t *testing.T, err error, ruleID string, stageID string) {
	t.Helper()
	require.Error(t, err)
	preflightErr, ok := AsRequestPreflightError(err)
	require.True(t, ok)
	assert.Equal(t, http.StatusBadRequest, preflightErr.StatusCode)
	assert.Equal(t, ruleID, preflightErr.RuleID)
	assert.Equal(t, stageID, preflightErr.StageID)
	assert.Equal(t, RequestContractPublicMessage, preflightErr.Message)
}

func TestCacheControlContractRejectsNonMessagesControlFieldsWithoutApplyingPolicy(t *testing.T) {
	tests := []struct {
		name          string
		clientFormat  types.RelayFormat
		finalProtocol Protocol
		body          map[string]any
		wantRule      string
	}{
		{
			name:          "chat top level",
			clientFormat:  types.RelayFormatOpenAI,
			finalProtocol: ProtocolChat,
			wantRule:      RequestContractUnmappedPathRule,
			body: map[string]any{
				"model": "glm-5.2", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
				"cache_control": cacheMarker("5m"),
			},
		},
		{
			name:          "chat content block",
			clientFormat:  types.RelayFormatOpenAI,
			finalProtocol: ProtocolChat,
			wantRule:      RequestContractUnmappedNestedRule,
			body: map[string]any{
				"model": "glm-5.2", "messages": []any{map[string]any{
					"role": "user", "content": []any{map[string]any{
						"type": "text", "text": "hello", "cache_control": cacheMarker("5m"),
					}},
				}},
			},
		},
		{
			name:          "responses top level",
			clientFormat:  types.RelayFormatOpenAIResponses,
			finalProtocol: ProtocolResponses,
			wantRule:      RequestContractUnmappedPathRule,
			body: map[string]any{
				"model": "gpt-5.6-luna", "input": "hello", "cache_control": cacheMarker("5m"),
			},
		},
		{
			name:          "responses content block",
			clientFormat:  types.RelayFormatOpenAIResponses,
			finalProtocol: ProtocolResponses,
			wantRule:      RequestContractUnmappedNestedRule,
			body: map[string]any{
				"model": "gpt-5.6-luna", "input": []any{map[string]any{
					"type": "message", "role": "user", "content": []any{map[string]any{
						"type": "input_text", "text": "hello", "cache_control": cacheMarker("5m"),
					}},
				}},
			},
		},
	}

	for _, policy := range []string{
		dto.OpenCodeGoUnsupportedOptionalFieldStrict,
		dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
	} {
		for _, test := range tests {
			t.Run(policy+"/"+test.name, func(t *testing.T) {
				envelope, request := newCacheControlSourceFormatFixture(t, test.clientFormat, test.body)
				_, err := BuildCacheControlDispositionPlan(
					envelope, test.clientFormat, test.finalProtocol, request, policy,
				)
				requireCacheControlClientRuleAtStage(
					t, err, test.wantRule, RequestContractPreflightStage,
				)
			})
		}
	}
}

func TestCacheControlContractTreatsNonMessagesOpaqueSameNamedDataAsData(t *testing.T) {
	tests := []struct {
		name          string
		clientFormat  types.RelayFormat
		finalProtocol Protocol
		body          map[string]any
	}{
		{
			name:          "chat function schema",
			clientFormat:  types.RelayFormatOpenAI,
			finalProtocol: ProtocolChat,
			body: map[string]any{
				"model": "glm-5.2", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
				"tools": []any{map[string]any{
					"type": "function", "function": map[string]any{
						"name": "lookup", "parameters": map[string]any{
							"type": "object", "properties": map[string]any{
								"cache_control": map[string]any{"type": "string"},
							},
						},
					},
				}},
			},
		},
		{
			name:          "responses tool output",
			clientFormat:  types.RelayFormatOpenAIResponses,
			finalProtocol: ProtocolResponses,
			body: map[string]any{
				"model": "gpt-5.6-luna", "input": []any{map[string]any{
					"type": "function_call_output", "call_id": "call-1",
					"output": map[string]any{"cache_control": map[string]any{"customer": true}},
				}},
			},
		},
		{
			name:          "responses function schema",
			clientFormat:  types.RelayFormatOpenAIResponses,
			finalProtocol: ProtocolResponses,
			body: map[string]any{
				"model": "gpt-5.6-luna", "input": "hello",
				"tools": []any{map[string]any{
					"type": "function", "name": "lookup", "parameters": map[string]any{
						"type": "object", "properties": map[string]any{
							"cache_control": map[string]any{"type": "string"},
						},
					},
				}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			envelope, request := newCacheControlSourceFormatFixture(t, test.clientFormat, test.body)
			plan, err := BuildCacheControlDispositionPlan(
				envelope,
				test.clientFormat,
				test.finalProtocol,
				request,
				dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
			)
			require.NoError(t, err)
			assert.Empty(t, plan.Entries)
		})
	}
}

func TestCacheControlContractRejectsOpaqueKeyNamesUnderInvalidToolParents(t *testing.T) {
	tests := []struct {
		name         string
		clientFormat types.RelayFormat
		body         map[string]any
	}{
		{
			name:         "chat deceptive parameters",
			clientFormat: types.RelayFormatOpenAI,
			body: map[string]any{
				"model": "glm-5.2", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
				"tools": []any{map[string]any{
					"type": "function", "function": map[string]any{
						"name": "lookup", "deceptive": map[string]any{
							"parameters": map[string]any{"cache_control": cacheMarker("5m")},
						},
					},
				}},
			},
		},
		{
			name:         "responses deceptive parameters",
			clientFormat: types.RelayFormatOpenAIResponses,
			body: map[string]any{
				"model": "gpt-5.6-luna", "input": "hello",
				"tools": []any{map[string]any{
					"type": "function", "name": "lookup", "deceptive": map[string]any{
						"parameters": map[string]any{"cache_control": cacheMarker("5m")},
					},
				}},
			},
		},
		{
			name:         "responses deceptive format",
			clientFormat: types.RelayFormatOpenAIResponses,
			body: map[string]any{
				"model": "gpt-5.6-luna", "input": "hello",
				"tools": []any{map[string]any{
					"type": "custom", "name": "patch", "deceptive": map[string]any{
						"format": map[string]any{"cache_control": cacheMarker("5m")},
					},
				}},
			},
		},
	}

	for _, test := range tests {
		for _, policy := range []string{
			dto.OpenCodeGoUnsupportedOptionalFieldStrict,
			dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
		} {
			t.Run(test.name+"/"+policy, func(t *testing.T) {
				envelope, request := newCacheControlSourceFormatFixture(t, test.clientFormat, test.body)
				_, err := BuildCacheControlDispositionPlan(
					envelope, test.clientFormat, ProtocolResponses, request, policy,
				)
				requireCacheControlClientRuleAtStage(
					t, err, RequestContractUnmappedNestedRule, RequestContractPreflightStage,
				)
			})
		}
	}
}

func TestCacheControlContractTreatsChatToolMessagePayloadAsOpaqueData(t *testing.T) {
	for _, role := range []string{"tool", "function"} {
		body := chatToolOpaqueCacheControlBodyForRole(role)
		for _, policy := range []string{
			dto.OpenCodeGoUnsupportedOptionalFieldStrict,
			dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
		} {
			t.Run(role+"/"+policy, func(t *testing.T) {
				envelope, request := newCacheControlSourceFormatFixture(t, types.RelayFormatOpenAI, body)
				plan, err := BuildCacheControlDispositionPlan(
					envelope,
					types.RelayFormatOpenAI,
					ProtocolChat,
					request,
					policy,
				)
				require.NoError(t, err)
				assert.Empty(t, plan.Entries)
			})
		}
	}
}

func TestCacheControlFinalWirePreservesChatToolMessageOpaqueData(t *testing.T) {
	for _, role := range []string{"tool", "function"} {
		t.Run(role, func(t *testing.T) {
			body := chatToolOpaqueCacheControlBodyForRole(role)
			c, info := newRequestContractFixture(
				t,
				constant.ChannelTypeOpenCodeAPIKey,
				requestPreflightEndpoints[1],
				ProtocolChat,
				map[string]any{"messages": body["messages"]},
			)

			result := convertAndFinalizeRequestForPresenceTest(t, c, info)
			for _, sentinel := range []string{
				"opaque-tool-content-cache-sentinel",
				"opaque-tool-content-extension-cache-sentinel",
				"opaque-tool-message-extension-cache-sentinel",
			} {
				assert.Contains(t, string(result), sentinel)
			}
			root, err := decodeCacheControlFinalBody(result)
			require.NoError(t, err)
			assert.Empty(t, targetOwnedCacheControlPaths(root, ProtocolChat))
		})
	}
}

func TestCacheControlTargetTraversalOnlyIgnoresChatToolMessagePayload(t *testing.T) {
	for _, role := range []string{"tool", "function"} {
		t.Run(role, func(t *testing.T) {
			root := map[string]any{
				"messages": []any{
					chatToolOpaqueCacheControlBodyForRole(role)["messages"].([]any)[0],
					map[string]any{
						"role": "user",
						"content": []any{map[string]any{
							"type": "text", "text": "semantic",
							"cache_control": cacheMarker("5m"),
						}},
					},
				},
			}

			paths := targetOwnedCacheControlPaths(root, ProtocolChat)
			require.Len(t, paths, 1)
			semanticPath := cachePath(
				cacheKey("messages"), cacheIndex(1), cacheKey("content"), cacheIndex(0), cacheKey("cache_control"),
			)
			_, found := paths[cacheControlPathKey(semanticPath)]
			assert.True(t, found)
		})
	}
}

func TestCacheControlTargetTraversalDoesNotTrustOpaqueKeyNamesOutsideOwnedParents(t *testing.T) {
	for _, key := range []string{
		"input_schema", "parameters", "schema", "arguments", "metadata", "format", "custom",
	} {
		t.Run(key, func(t *testing.T) {
			root := map[string]any{"messages": []any{map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "text", "text": "semantic",
					key: map[string]any{"cache_control": cacheMarker("5m")},
				}},
			}}}
			paths := targetOwnedCacheControlPaths(root, ProtocolChat)
			require.Len(t, paths, 1)
			markerPath := cachePath(
				cacheKey("messages"), cacheIndex(0), cacheKey("content"), cacheIndex(0),
				cacheKey(key), cacheKey("cache_control"),
			)
			_, found := paths[cacheControlPathKey(markerPath)]
			assert.True(t, found)
		})
	}
}

func TestCacheControlContractRejectsMalformedAndUnregisteredMarkers(t *testing.T) {
	tests := []struct {
		name string
		body map[string]any
		rule string
	}{
		{name: "null", body: cacheBodyWithTextMarker(nil), rule: CacheControlShapeRule},
		{name: "array", body: cacheBodyWithTextMarker([]any{}), rule: CacheControlShapeRule},
		{name: "scalar", body: cacheBodyWithTextMarker("ephemeral"), rule: CacheControlShapeRule},
		{name: "missing type", body: cacheBodyWithTextMarker(map[string]any{"ttl": "5m"}), rule: CacheControlShapeRule},
		{name: "wrong type", body: cacheBodyWithTextMarker(map[string]any{"type": "persistent"}), rule: CacheControlShapeRule},
		{name: "wrong ttl", body: cacheBodyWithTextMarker(map[string]any{"type": "ephemeral", "ttl": "10m"}), rule: CacheControlShapeRule},
		{name: "null ttl", body: cacheBodyWithTextMarker(map[string]any{"type": "ephemeral", "ttl": nil}), rule: CacheControlShapeRule},
		{name: "boolean ttl", body: cacheBodyWithTextMarker(map[string]any{"type": "ephemeral", "ttl": true}), rule: CacheControlShapeRule},
		{name: "unknown member", body: cacheBodyWithTextMarker(map[string]any{"type": "ephemeral", "future": true}), rule: CacheControlShapeRule},
		{name: "empty marked text", body: map[string]any{"messages": []any{cacheMessage("user", map[string]any{"type": "text", "text": "", "cache_control": cacheMarker("5m")})}}, rule: CacheControlParentRule},
		{name: "thinking marker", body: map[string]any{"messages": []any{cacheMessage("assistant", map[string]any{"type": "thinking", "thinking": "x", "signature": "signed", "cache_control": cacheMarker("5m")})}}, rule: CacheControlParentRule},
		{name: "system role marker with unknown parent member", body: map[string]any{"messages": []any{
			map[string]any{
				"role": "system", "content": []any{cacheText("x")}, "name": "unexpected",
			},
			cacheMessage("user", map[string]any{"type": "text", "text": "control"}),
		}}, rule: CacheControlParentRule},
		{name: "system role marker with unknown block member", body: map[string]any{"messages": []any{
			cacheMessage("system", map[string]any{
				"type": "text", "text": "x", "cache_control": cacheMarker("5m"), "name": "unexpected",
			}),
			cacheMessage("user", map[string]any{"type": "text", "text": "control"}),
		}}, rule: CacheControlParentRule},
		{name: "system role marker with empty text", body: map[string]any{"messages": []any{
			cacheMessage("system", map[string]any{
				"type": "text", "text": "", "cache_control": cacheMarker("5m"),
			}),
			cacheMessage("user", map[string]any{"type": "text", "text": "control"}),
		}}, rule: CacheControlParentRule},
		{name: "system role marker with input text block", body: map[string]any{"messages": []any{
			cacheMessage("system", map[string]any{
				"type": "input_text", "text": "x", "cache_control": cacheMarker("5m"),
			}),
			cacheMessage("user", map[string]any{"type": "text", "text": "control"}),
		}}, rule: CacheControlParentRule},
		{name: "nested tool result marker", body: map[string]any{"messages": []any{cacheMessage("user", map[string]any{
			"type": "tool_result", "tool_use_id": "call-1", "content": []any{cacheText("nested")},
		})}}, rule: CacheControlPathRule},
	}

	for _, policy := range []string{
		dto.OpenCodeGoUnsupportedOptionalFieldStrict,
		dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
	} {
		for _, test := range tests {
			t.Run(policy+"/"+test.name, func(t *testing.T) {
				envelope, request := newCacheControlContractFixture(t, test.body)
				_, err := BuildCacheControlDispositionPlan(
					envelope, types.RelayFormatClaude, ProtocolMessages, request, policy,
				)
				requireCacheControlClientRule(t, err, test.rule)
			})
		}
	}
}

func TestCacheControlContractMarkerDoesNotAuthorizeMalformedRegisteredParents(t *testing.T) {
	tests := []struct {
		name string
		body map[string]any
	}{
		{
			name: "tool input schema missing object type",
			body: map[string]any{"tools": []any{map[string]any{
				"name": "lookup", "description": "lookup",
				"input_schema":  map[string]any{"properties": map[string]any{}},
				"cache_control": cacheMarker("5m"),
			}}},
		},
		{
			name: "tool input schema has unsupported type",
			body: map[string]any{"tools": []any{map[string]any{
				"name": "lookup", "description": "lookup",
				"input_schema":  map[string]any{"type": "array"},
				"cache_control": cacheMarker("5m"),
			}}},
		},
		{
			name: "tool input schema type is not a string",
			body: map[string]any{"tools": []any{map[string]any{
				"name": "lookup", "description": "lookup",
				"input_schema":  map[string]any{"type": true},
				"cache_control": cacheMarker("5m"),
			}}},
		},
		{
			name: "tool description is not a string",
			body: map[string]any{"tools": []any{map[string]any{
				"name": "lookup", "description": 42,
				"input_schema":  map[string]any{"type": "object"},
				"cache_control": cacheMarker("5m"),
			}}},
		},
		{
			name: "user image source type is not base64",
			body: map[string]any{"messages": []any{cacheMessage("user", map[string]any{
				"type": "image", "source": map[string]any{
					"type": "text", "media_type": "image/png", "data": "AA==",
				},
				"cache_control": cacheMarker("5m"),
			})}},
		},
		{
			name: "user image media type is outside locked SDK enum",
			body: map[string]any{"messages": []any{cacheMessage("user", map[string]any{
				"type": "image", "source": map[string]any{
					"type": "base64", "media_type": "text/plain", "data": "AA==",
				},
				"cache_control": cacheMarker("5m"),
			})}},
		},
		{
			name: "tool result nested image source type is not base64",
			body: map[string]any{"messages": []any{cacheMessage("user", map[string]any{
				"type": "tool_result", "tool_use_id": "call-1",
				"content": []any{map[string]any{
					"type": "image", "source": map[string]any{
						"type": "text", "media_type": "image/png", "data": "AA==",
					},
				}},
				"cache_control": cacheMarker("5m"),
			})}},
		},
		{
			name: "tool result nested image media type is outside locked SDK enum",
			body: map[string]any{"messages": []any{cacheMessage("user", map[string]any{
				"type": "tool_result", "tool_use_id": "call-1",
				"content": []any{map[string]any{
					"type": "image", "source": map[string]any{
						"type": "base64", "media_type": "application/octet-stream", "data": "AA==",
					},
				}},
				"cache_control": cacheMarker("5m"),
			})}},
		},
		{
			name: "tool result is_error is not boolean",
			body: map[string]any{"messages": []any{cacheMessage("user", map[string]any{
				"type": "tool_result", "tool_use_id": "call-1", "content": "failed",
				"is_error": "true", "cache_control": cacheMarker("5m"),
			})}},
		},
	}

	for _, policy := range []string{
		dto.OpenCodeGoUnsupportedOptionalFieldStrict,
		dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
	} {
		for _, test := range tests {
			t.Run(policy+"/"+test.name, func(t *testing.T) {
				envelope, request := newCacheControlContractFixture(t, test.body)
				_, err := BuildCacheControlDispositionPlan(
					envelope, types.RelayFormatClaude, ProtocolMessages, request, policy,
				)
				requireCacheControlClientRule(t, err, CacheControlParentRule)
			})
		}
	}
}

func TestCacheControlContractAcceptsLockedSDKImageSourceValues(t *testing.T) {
	for _, mediaType := range []string{"image/jpeg", "image/png", "image/gif", "image/webp"} {
		t.Run(mediaType, func(t *testing.T) {
			envelope, request := newCacheControlContractFixture(t, map[string]any{
				"messages": []any{cacheMessage("user", map[string]any{
					"type": "image", "source": map[string]any{
						"type": "base64", "media_type": mediaType, "data": "AA==",
					},
					"cache_control": cacheMarker("5m"),
				})},
			})
			plan, err := BuildCacheControlDispositionPlan(
				envelope,
				types.RelayFormatClaude,
				ProtocolMessages,
				request,
				dto.OpenCodeGoUnsupportedOptionalFieldStrict,
			)
			require.NoError(t, err)
			require.Len(t, plan.Entries, 1)
			assert.Equal(t, cacheControlRuleImage, plan.Entries[0].RuleID)
		})
	}

	t.Run("nested tool result image", func(t *testing.T) {
		envelope, request := newCacheControlContractFixture(t, map[string]any{
			"messages": []any{cacheMessage("user", map[string]any{
				"type": "tool_result", "tool_use_id": "call-1",
				"content": []any{map[string]any{
					"type": "image", "source": map[string]any{
						"type": "base64", "media_type": "image/png", "data": "AA==",
					},
				}},
				"cache_control": cacheMarker("5m"),
			})},
		})
		plan, err := BuildCacheControlDispositionPlan(
			envelope,
			types.RelayFormatClaude,
			ProtocolMessages,
			request,
			dto.OpenCodeGoUnsupportedOptionalFieldStrict,
		)
		require.NoError(t, err)
		require.Len(t, plan.Entries, 1)
		assert.Equal(t, cacheControlRuleToolResult, plan.Entries[0].RuleID)
	})

	t.Run("tool result is_error boolean", func(t *testing.T) {
		envelope, request := newCacheControlContractFixture(t, map[string]any{
			"messages": []any{cacheMessage("user", map[string]any{
				"type": "tool_result", "tool_use_id": "call-1", "content": "failed",
				"is_error": true, "cache_control": cacheMarker("5m"),
			})},
		})
		plan, err := BuildCacheControlDispositionPlan(
			envelope,
			types.RelayFormatClaude,
			ProtocolMessages,
			request,
			dto.OpenCodeGoUnsupportedOptionalFieldStrict,
		)
		require.NoError(t, err)
		require.Len(t, plan.Entries, 1)
		assert.Equal(t, cacheControlRuleToolResult, plan.Entries[0].RuleID)
	})
}

func TestCacheControlContractTTLOrderAndSlotLimit(t *testing.T) {
	for _, test := range []struct {
		name      string
		body      map[string]any
		wantRule  string
		wantCount int
	}{
		{
			name: "one hour markers precede five minute markers",
			body: map[string]any{
				"tools":    []any{cacheTool("tool-a", "1h")},
				"messages": []any{cacheMessage("user", cacheText("hello"))},
			},
			wantCount: 2,
		},
		{
			name: "one hour after five minute",
			body: map[string]any{
				"tools":    []any{cacheTool("tool-a", "5m")},
				"messages": []any{cacheMessage("user", cacheTextTTL("hello", "1h"))},
			},
			wantRule: CacheControlTTLOrderRule,
		},
		{
			name: "exactly four explicit",
			body: map[string]any{
				"tools":    []any{cacheTool("a", "1h"), cacheTool("b", "1h")},
				"messages": []any{cacheMessage("user", cacheText("one"), cacheText("two"))},
			},
			wantCount: 4,
		},
		{
			name: "fifth explicit",
			body: map[string]any{
				"tools":    []any{cacheTool("a", "1h"), cacheTool("b", "1h")},
				"messages": []any{cacheMessage("user", cacheText("one"), cacheText("two"), cacheText("three"))},
			},
			wantRule: CacheControlBreakpointLimitRule,
		},
		{
			name: "automatic plus four explicit",
			body: map[string]any{
				"cache_control": cacheMarker("5m"),
				"tools":         []any{cacheTool("a", "1h"), cacheTool("b", "1h")},
				"messages":      []any{cacheMessage("user", cacheText("one"), cacheText("two"))},
			},
			wantRule: CacheControlAutomaticSlotRule,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			envelope, request := newCacheControlContractFixture(t, test.body)
			plan, err := BuildCacheControlDispositionPlan(
				envelope, types.RelayFormatClaude, ProtocolMessages, request,
				dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
			)
			if test.wantRule != "" {
				requireCacheControlClientRule(t, err, test.wantRule)
				return
			}
			require.NoError(t, err)
			assert.Len(t, plan.Entries, test.wantCount)
		})
	}
}

func TestCacheControlContractAutomaticMarkerPlacement(t *testing.T) {
	tests := []struct {
		name     string
		body     map[string]any
		wantRule string
		check    func(t *testing.T, envelope *helper.ValidatedRequestEnvelope, plan CacheControlDispositionPlan)
	}{
		{
			name: "same ttl on selected explicit block is a no-op",
			body: map[string]any{
				"cache_control": cacheMarker("5m"),
				"messages":      []any{cacheMessage("user", cacheTextTTL("selected", "5m"))},
			},
			check: func(t *testing.T, _ *helper.ValidatedRequestEnvelope, plan CacheControlDispositionPlan) {
				require.Len(t, plan.Entries, 2)
				assert.Equal(t, plan.Entries[0].SemanticPosition, plan.Entries[1].SemanticPosition)
				assert.True(t, plan.Entries[1].Automatic)
			},
		},
		{
			name: "omitted automatic ttl is five minute and a same ttl no-op",
			body: map[string]any{
				"cache_control": cacheMarker(""),
				"messages":      []any{cacheMessage("user", cacheTextTTL("selected", "5m"))},
			},
			check: func(t *testing.T, _ *helper.ValidatedRequestEnvelope, plan CacheControlDispositionPlan) {
				require.Len(t, plan.Entries, 2)
				assert.Equal(t, "5m", plan.Entries[1].TTL)
				assert.True(t, plan.Entries[1].Automatic)
			},
		},
		{
			name: "same one hour ttl on selected explicit block is a no-op",
			body: map[string]any{
				"cache_control": cacheMarker("1h"),
				"messages":      []any{cacheMessage("user", cacheTextTTL("selected", "1h"))},
			},
			check: func(t *testing.T, _ *helper.ValidatedRequestEnvelope, plan CacheControlDispositionPlan) {
				require.Len(t, plan.Entries, 2)
				assert.Equal(t, plan.Entries[0].SemanticPosition, plan.Entries[1].SemanticPosition)
				assert.Equal(t, "1h", plan.Entries[1].TTL)
				assert.True(t, plan.Entries[1].Automatic)
			},
		},
		{
			name: "automatic one hour conflicts with selected five minute",
			body: map[string]any{
				"cache_control": cacheMarker("1h"),
				"messages":      []any{cacheMessage("user", cacheTextTTL("selected", "5m"))},
			},
			wantRule: CacheControlAutomaticTTLRule,
		},
		{
			name: "automatic five minute conflicts with selected one hour",
			body: map[string]any{
				"cache_control": cacheMarker("5m"),
				"messages":      []any{cacheMessage("user", cacheTextTTL("selected", "1h"))},
			},
			wantRule: CacheControlAutomaticTTLRule,
		},
		{
			name: "automatic plus three explicit fills the fourth slot",
			body: map[string]any{
				"cache_control": cacheMarker("5m"),
				"tools":         []any{cacheTool("one", "1h"), cacheTool("two", "1h")},
				"system":        []any{cacheSystemTextTTL("system", "1h")},
				"messages":      []any{cacheMessage("user", map[string]any{"type": "text", "text": "tail"})},
			},
			check: func(t *testing.T, _ *helper.ValidatedRequestEnvelope, plan CacheControlDispositionPlan) {
				require.Len(t, plan.Entries, 4)
				assert.True(t, plan.Entries[3].Automatic)
				assert.Equal(t, "5m", plan.Entries[3].TTL)
			},
		},
		{
			name: "earlier five minute followed by automatic one hour is invalid",
			body: map[string]any{
				"cache_control": cacheMarker("1h"),
				"tools":         []any{cacheTool("one", "5m")},
				"messages":      []any{cacheMessage("user", map[string]any{"type": "text", "text": "tail"})},
			},
			wantRule: CacheControlTTLOrderRule,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			envelope, request := newCacheControlContractFixture(t, test.body)
			plan, err := BuildCacheControlDispositionPlan(
				envelope, types.RelayFormatClaude, ProtocolMessages, request,
				dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
			)
			if test.wantRule != "" {
				requireCacheControlClientRule(t, err, test.wantRule)
				return
			}
			require.NoError(t, err)
			test.check(t, envelope, plan)
		})
	}
}

func TestCacheControlContractExplicitOmittedTTLIsFiveMinutes(t *testing.T) {
	envelope, request := newCacheControlContractFixture(t, map[string]any{
		"messages": []any{cacheMessage("user", map[string]any{
			"type": "text", "text": "selected", "cache_control": cacheMarker(""),
		})},
	})
	plan, err := BuildCacheControlDispositionPlan(
		envelope,
		types.RelayFormatClaude,
		ProtocolMessages,
		request,
		dto.OpenCodeGoUnsupportedOptionalFieldStrict,
	)
	require.NoError(t, err)
	require.Len(t, plan.Entries, 1)
	assert.Equal(t, "5m", plan.Entries[0].TTL)
	assert.Equal(t, cacheControlMarkerDigestWithPresence("5m", false), plan.Entries[0].MarkerDigest)
	assert.NotEqual(t, cacheControlMarkerDigest("5m"), plan.Entries[0].MarkerDigest)
}

func TestCacheControlContractAutomaticMarkerEligibleLookup(t *testing.T) {
	tests := []struct {
		name            string
		body            map[string]any
		wantPath        []helper.JSONPathSegment
		wantNone        bool
		wantParentError bool
	}{
		{
			name: "tail thinking and empty text fall back to preceding text",
			body: map[string]any{
				"cache_control": cacheMarker("5m"),
				"messages": []any{
					cacheMessage("user", map[string]any{"type": "text", "text": "selected"}),
					cacheMessage("assistant",
						map[string]any{"type": "thinking", "thinking": "internal", "signature": "signed"},
						map[string]any{"type": "text", "text": ""},
					),
				},
			},
			wantPath: cachePath(cacheKey("messages"), cacheIndex(0), cacheKey("content"), cacheIndex(0), cacheKey("cache_control")),
		},
		{
			name: "message string is eligible",
			body: map[string]any{
				"cache_control": cacheMarker("5m"),
				"messages":      []any{map[string]any{"role": "user", "content": "selected"}},
			},
			wantPath: nil,
		},
		{
			name: "system string is eligible when messages have no unit",
			body: map[string]any{
				"cache_control": cacheMarker("5m"),
				"system":        "selected",
				"messages": []any{cacheMessage("assistant", map[string]any{
					"type": "thinking", "thinking": "internal", "signature": "signed",
				})},
			},
			wantPath: nil,
		},
		{
			name: "fallback reaches tools after messages and system have no unit",
			body: map[string]any{
				"cache_control": cacheMarker("5m"),
				"system":        "",
				"messages":      []any{cacheMessage("assistant", map[string]any{"type": "thinking", "thinking": "internal", "signature": "signed"})},
				"tools":         []any{map[string]any{"name": "selected", "input_schema": map[string]any{"type": "object"}}},
			},
			wantPath: cachePath(cacheKey("tools"), cacheIndex(0), cacheKey("cache_control")),
		},
		{
			name: "malformed text tail cannot displace a valid system unit",
			body: map[string]any{
				"cache_control": cacheMarker("5m"),
				"system":        []any{map[string]any{"type": "text", "text": "selected"}},
				"messages": []any{cacheMessage("user", map[string]any{
					"type": "text", "text": "malformed", "unsupported": true,
				})},
			},
			wantPath:        cachePath(cacheKey("system"), cacheIndex(0), cacheKey("cache_control")),
			wantParentError: true,
		},
		{
			name: "unsupported tool use member cannot displace a valid system unit",
			body: map[string]any{
				"cache_control": cacheMarker("5m"),
				"system":        []any{map[string]any{"type": "text", "text": "selected"}},
				"messages": []any{cacheMessage("assistant", map[string]any{
					"type": "tool_use", "id": "call-1", "name": "lookup", "input": map[string]any{},
					"unsupported": true,
				})},
			},
			wantPath:        cachePath(cacheKey("system"), cacheIndex(0), cacheKey("cache_control")),
			wantParentError: true,
		},
		{
			name: "outer tool result is eligible but subcontent is not",
			body: map[string]any{
				"cache_control": cacheMarker("5m"),
				"messages": []any{cacheMessage("user", map[string]any{
					"type": "tool_result", "tool_use_id": "call-1", "content": []any{map[string]any{"type": "text", "text": "nested"}},
				})},
			},
			wantPath: cachePath(cacheKey("messages"), cacheIndex(0), cacheKey("content"), cacheIndex(0), cacheKey("cache_control")),
		},
		{
			name: "no eligible block is a legal no-op",
			body: map[string]any{
				"cache_control": cacheMarker("5m"),
				"system":        "",
				"messages":      []any{cacheMessage("assistant", map[string]any{"type": "thinking", "thinking": "internal", "signature": "signed"})},
			},
			wantNone: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			envelope, request := newCacheControlContractFixture(t, test.body)
			units := buildCacheControlUnits(envelope)
			if !test.wantNone {
				require.NotEmpty(t, units)
				assert.Equal(t, cacheControlPathKey(test.wantPath), cacheControlPathKey(units[len(units)-1].markerPath))
			}
			plan, err := BuildCacheControlDispositionPlan(
				envelope, types.RelayFormatClaude, ProtocolMessages, request,
				dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
			)
			if test.wantParentError {
				requireCacheControlClientRuleAtStage(
					t,
					err,
					RequestContractUnmappedNestedRule,
					RequestContractPreflightStage,
				)
				return
			}
			require.NoError(t, err)
			require.Len(t, plan.Entries, 1)
			assert.True(t, plan.Entries[0].Automatic)
			if test.wantNone {
				assert.Equal(t, -1, plan.Entries[0].SemanticPosition)
				return
			}
			assert.Equal(t, units[len(units)-1].position, plan.Entries[0].SemanticPosition)
		})
	}
}

func TestCacheControlContractValidatesMalformedParentsBeforeStrictPolicy(t *testing.T) {
	for _, policy := range []string{
		dto.OpenCodeGoUnsupportedOptionalFieldStrict,
		dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
	} {
		t.Run(policy, func(t *testing.T) {
			envelope, request := newCacheControlContractFixture(t, map[string]any{
				"cache_control": cacheMarker("5m"),
				"messages": []any{cacheMessage("user", map[string]any{
					"type": "text", "text": "malformed", "unsupported": true,
				})},
			})
			_, err := BuildCacheControlDispositionPlan(
				envelope,
				types.RelayFormatClaude,
				ProtocolMessages,
				request,
				policy,
			)
			requireCacheControlClientRuleAtStage(
				t,
				err,
				RequestContractUnmappedNestedRule,
				RequestContractPreflightStage,
			)
		})
	}
}

func TestCacheControlContractRawEnvelopeControls(t *testing.T) {
	t.Run("escaped marker key and numeric opaque key remain distinct", func(t *testing.T) {
		envelope, request := newCacheControlContractRawFixture(t, []byte(`{"model":"qwen3.8-max","max_tokens":16,"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"0":{"cache_control":{"type":"string"}}}}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello","\u0063ache_control":{"type":"ephemeral"}}]}]}`))
		plan, err := BuildCacheControlDispositionPlan(
			envelope, types.RelayFormatClaude, ProtocolMessages, request,
			dto.OpenCodeGoUnsupportedOptionalFieldStrict,
		)
		require.NoError(t, err)
		require.Len(t, plan.Entries, 1)
		assert.Equal(t, cacheControlRuleText, plan.Entries[0].RuleID)
	})

	t.Run("duplicate marker member is rejected by the strict envelope", func(t *testing.T) {
		_, _, err := newCacheControlContractRawFixtureWithoutRequest(t, []byte(`{"model":"qwen3.8-max","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","type":"ephemeral"}}]}]}`))
		validationErr, ok := helper.AsClientRequestValidationError(err)
		require.True(t, ok)
		assert.Equal(t, "json.duplicate_key", validationErr.RuleID)
	})

	t.Run("over four kibibyte marker is a fixed client shape error", func(t *testing.T) {
		body := []byte(`{"model":"qwen3.8-max","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","padding":"` + strings.Repeat("x", 4096) + `"}}]}]}`)
		envelope, request := newCacheControlContractRawFixture(t, body)
		_, err := BuildCacheControlDispositionPlan(
			envelope, types.RelayFormatClaude, ProtocolMessages, request,
			dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
		)
		requireCacheControlClientRule(t, err, CacheControlShapeRule)
	})
}

func TestCacheControlContractPlanIsStableAcrossJSONMemberOrder(t *testing.T) {
	bodies := [][]byte{
		[]byte(`{"model":"qwen3.8-max","max_tokens":16,"system":[{"type":"text","text":"system","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`),
		[]byte(`{"messages":[{"content":[{"cache_control":{"ttl":"5m","type":"ephemeral"},"text":"hello","type":"text"}],"role":"user"}],"system":[{"cache_control":{"ttl":"1h","type":"ephemeral"},"text":"system","type":"text"}],"max_tokens":16,"model":"qwen3.8-max"}`),
	}

	var canonical string
	var fingerprint string
	for index, body := range bodies {
		envelope, request := newCacheControlContractRawFixture(t, body)
		plan, err := BuildCacheControlDispositionPlan(
			envelope, types.RelayFormatClaude, ProtocolMessages, request,
			dto.OpenCodeGoUnsupportedOptionalFieldStrict,
		)
		require.NoError(t, err)
		if index == 0 {
			canonical = plan.Canonical
			fingerprint = plan.Fingerprint
			continue
		}
		assert.Equal(t, canonical, plan.Canonical)
		assert.Equal(t, fingerprint, plan.Fingerprint)
	}
}

func TestCacheControlContractRealClientObservedFixture(t *testing.T) {
	// real-client-observed: structure only; all customer text and identifiers are placeholders.
	envelope, request := newCacheControlContractFixture(t, map[string]any{
		"system": []any{cacheSystemText("<system-text>")},
		"messages": []any{cacheMessage("user", map[string]any{
			"type": "text", "text": "<user-text>", "cache_control": cacheMarker("5m"),
		})},
	})
	plan, err := BuildCacheControlDispositionPlan(
		envelope, types.RelayFormatClaude, ProtocolMessages, request,
		dto.OpenCodeGoUnsupportedOptionalFieldStrict,
	)
	require.NoError(t, err)
	assert.Equal(t, 2, plan.PreserveCount)
	assert.Zero(t, plan.DropCount)
}

func TestCacheControlContractIgnoresOpaqueSameNamedData(t *testing.T) {
	body := map[string]any{
		"tools": []any{map[string]any{
			"name": "lookup",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cache_control": map[string]any{"type": "string"},
				},
			},
		}},
		"messages": []any{cacheMessage("assistant", map[string]any{
			"type": "tool_use", "id": "call-1", "name": "lookup",
			"input": map[string]any{"cache_control": map[string]any{"customer": true}},
		})},
	}
	envelope, request := newCacheControlContractFixture(t, body)
	plan, err := BuildCacheControlDispositionPlan(
		envelope, types.RelayFormatClaude, ProtocolMessages, request,
		dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
	)
	require.NoError(t, err)
	assert.Empty(t, plan.Entries)
}

func TestCacheControlContractOnlyTreatsToolUseInputAsOpaque(t *testing.T) {
	envelope, request := newCacheControlContractFixture(t, map[string]any{
		"messages": []any{cacheMessage("assistant", map[string]any{
			"type": "text", "text": "not a tool input",
			"input": map[string]any{"cache_control": map[string]any{"customer": true}},
		})},
	})
	_, err := BuildCacheControlDispositionPlan(
		envelope, types.RelayFormatClaude, ProtocolMessages, request,
		dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
	)
	requireCacheControlClientRule(t, err, CacheControlPathRule)
}

func TestApplyCacheControlDispositionPlanDropsOnlyPlannedCandidateFields(t *testing.T) {
	body := map[string]any{
		"system": []any{cacheSystemText("system")},
		"tools": []any{map[string]any{
			"name": "lookup",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cache_control": map[string]any{"type": "string"},
				},
			},
		}},
		"messages": []any{cacheMessage("assistant", map[string]any{
			"type": "tool_use", "id": "call-1", "name": "lookup",
			"input":         map[string]any{"cache_control": map[string]any{"customer": true}},
			"cache_control": cacheMarker("5m"),
		})},
	}
	envelope, request := newCacheControlContractFixture(t, body)
	plan, err := BuildCacheControlDispositionPlan(
		envelope, types.RelayFormatClaude, ProtocolChat, request,
		dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
	)
	require.NoError(t, err)
	require.Equal(t, 2, plan.DropCount)

	plannedRequest, err := applyCacheControlDispositionPlan(request, plan)
	require.NoError(t, err)
	clone, ok := plannedRequest.(*dto.ClaudeRequest)
	require.True(t, ok)
	require.NotSame(t, request, clone)

	originalSystem := request.System.([]any)[0].(map[string]any)
	_, originalSystemMarker := originalSystem["cache_control"]
	assert.True(t, originalSystemMarker)
	cloneSystem := clone.System.([]any)[0].(map[string]any)
	_, cloneSystemMarker := cloneSystem["cache_control"]
	assert.False(t, cloneSystemMarker)

	originalPart := request.Messages[0].Content.([]any)[0].(map[string]any)
	_, originalPartMarker := originalPart["cache_control"]
	assert.True(t, originalPartMarker)
	clonePart := clone.Messages[0].Content.([]any)[0].(map[string]any)
	_, clonePartMarker := clonePart["cache_control"]
	assert.False(t, clonePartMarker)
	assert.Equal(t, map[string]any{"customer": true}, clonePart["input"].(map[string]any)["cache_control"])

	cloneTools := clone.Tools.([]any)
	cloneSchema := cloneTools[0].(map[string]any)["input_schema"].(map[string]any)
	cloneProperties := cloneSchema["properties"].(map[string]any)
	assert.Equal(t, map[string]any{"type": "string"}, cloneProperties["cache_control"])
}

func TestApplyCacheControlDispositionPlanUsesNormalizedMessageIndex(t *testing.T) {
	envelope, request := newCacheControlContractFixture(t, map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "system instruction"},
			cacheMessage("user", cacheText("marked user message")),
		},
	})
	plan, err := BuildCacheControlDispositionPlan(
		envelope,
		types.RelayFormatClaude,
		ProtocolChat,
		request,
		dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
	)
	require.NoError(t, err)
	require.Len(t, plan.Entries, 1)
	assert.Equal(t, 1, plan.Entries[0].SourcePath[1].Index)
	assert.Equal(t, 0, plan.Entries[0].NormalizedSourcePath[1].Index)

	plannedRequest, err := applyCacheControlDispositionPlan(request, plan)
	require.NoError(t, err)
	clone, ok := plannedRequest.(*dto.ClaudeRequest)
	require.True(t, ok)
	require.Len(t, clone.Messages, 1)
	clonePart := clone.Messages[0].Content.([]any)[0].(map[string]any)
	_, cloneMarker := clonePart["cache_control"]
	assert.False(t, cloneMarker)

	originalPart := request.Messages[0].Content.([]any)[0].(map[string]any)
	_, originalMarker := originalPart["cache_control"]
	assert.True(t, originalMarker)
}

func TestApplyCacheControlDispositionPlanUsesNormalizedSystemMessageIndex(t *testing.T) {
	envelope, request := newCacheControlContractFixture(t, map[string]any{
		"system": []any{map[string]any{"type": "text", "text": "existing system"}},
		"messages": []any{
			cacheMessage("user", map[string]any{"type": "text", "text": "hello"}),
			cacheMessage("system", cacheText("message system")),
		},
	})
	plan, err := BuildCacheControlDispositionPlan(
		envelope,
		types.RelayFormatClaude,
		ProtocolChat,
		request,
		dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
	)
	require.NoError(t, err)
	require.Len(t, plan.Entries, 1)
	assert.Equal(t, 1, plan.Entries[0].SourcePath[1].Index)
	assert.Equal(t, 1, plan.Entries[0].NormalizedSourcePath[1].Index)

	plannedRequest, err := applyCacheControlDispositionPlan(request, plan)
	require.NoError(t, err)
	clone, ok := plannedRequest.(*dto.ClaudeRequest)
	require.True(t, ok)
	system, ok := clone.System.([]any)
	require.True(t, ok)
	require.Len(t, system, 2)
	_, markerPresent := system[1].(map[string]any)["cache_control"]
	assert.False(t, markerPresent)

	originalSystem, ok := request.System.([]any)
	require.True(t, ok)
	_, originalMarkerPresent := originalSystem[1].(map[string]any)["cache_control"]
	assert.True(t, originalMarkerPresent)
}

func TestCacheControlContractNormalizesMultipleSystemMessagesInSourceOrder(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": []any{
				cacheText("first system"), cacheText("first system continuation"),
			}},
			cacheMessage("user", map[string]any{"type": "text", "text": "hello"}),
			map[string]any{"role": "system", "content": []any{cacheText("second system")}},
		},
	}
	c, info := newCacheControlPreflightFixture(
		t, body, constant.ChannelTypeOpenCodeAPIKey, ProtocolMessages,
		dto.OpenCodeGoUnsupportedOptionalFieldStrict,
	)
	preflight, err := BuildRequestPreflightPlan(c, info)
	require.NoError(t, err)
	plan, err := cacheControlDispositionPlanFromPreflight(preflight)
	require.NoError(t, err)
	require.Len(t, plan.Entries, 3)
	wantSourceIndexes := []int{0, 0, 2}
	wantPartIndexes := []int{0, 1, 0}
	for index, entry := range plan.Entries {
		assert.Equal(t, wantSourceIndexes[index], entry.SourcePath[1].Index)
		assert.Equal(t, wantPartIndexes[index], entry.SourcePath[3].Index)
		assert.Equal(t, index, entry.NormalizedSourcePath[1].Index)
	}

	wire := convertAndFinalizeRequestForPresenceTest(t, c, info)
	root, err := decodeCacheControlFinalBody(wire)
	require.NoError(t, err)
	system, ok := root.(map[string]any)["system"].([]any)
	require.True(t, ok)
	require.Len(t, system, 3)
	assert.Equal(t, "first system", system[0].(map[string]any)["text"])
	assert.Equal(t, "first system continuation", system[1].(map[string]any)["text"])
	assert.Equal(t, "second system", system[2].(map[string]any)["text"])

	t.Run("top-level string precedes message systems", func(t *testing.T) {
		c, info := newCacheControlPreflightFixture(
			t,
			map[string]any{
				"system": "existing system",
				"messages": []any{
					map[string]any{"role": "system", "content": []any{cacheText("message system")}},
					cacheMessage("user", map[string]any{"type": "text", "text": "hello"}),
				},
			},
			constant.ChannelTypeOpenCodeAPIKey,
			ProtocolMessages,
			dto.OpenCodeGoUnsupportedOptionalFieldStrict,
		)
		preflight, err := BuildRequestPreflightPlan(c, info)
		require.NoError(t, err)
		plan, err := cacheControlDispositionPlanFromPreflight(preflight)
		require.NoError(t, err)
		require.Len(t, plan.Entries, 1)
		assert.Equal(t, 1, plan.Entries[0].NormalizedSourcePath[1].Index)
	})
}

func TestBuildRequestPreflightPlanFreezesCacheControlPolicyForBothChannelTypes(t *testing.T) {
	body := map[string]any{
		"system": []any{cacheSystemText("system")},
		"messages": []any{cacheMessage("user", map[string]any{
			"type": "text", "text": "hello",
		})},
	}
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, policy := range []string{
			dto.OpenCodeGoUnsupportedOptionalFieldStrict,
			dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
		} {
			t.Run(policy+"/type-"+strconv.Itoa(channelType), func(t *testing.T) {
				c, info := newCacheControlPreflightFixture(t, body, channelType, ProtocolChat, policy)
				plan, err := BuildRequestPreflightPlan(c, info)
				if policy == dto.OpenCodeGoUnsupportedOptionalFieldStrict {
					require.Error(t, err)
					preflightErr, ok := AsRequestPreflightError(err)
					require.True(t, ok)
					assert.Equal(t, CacheControlUnsupportedRule, preflightErr.RuleID)
					return
				}
				require.NoError(t, err)
				assert.Equal(t, policy, plan.UnsupportedOptionalFieldPolicy)
				assert.Equal(t, CacheControlRegistryVersion, plan.CacheControlRegistryVersion)
				assert.Equal(t, 1, plan.CacheControlDropCount)
				assert.Zero(t, plan.CacheControlPreserveCount)
				assert.NotEmpty(t, plan.CacheControlPlanCanonical)
				assert.NotEmpty(t, plan.CacheControlPlanFingerprint)
			})
		}
	}
}

func TestCacheControlDispositionIsEnforcedOnFinalWire(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, finalProtocol := range []Protocol{ProtocolChat, ProtocolResponses} {
			t.Run("drop/type-"+strconv.Itoa(channelType)+"/"+string(finalProtocol), func(t *testing.T) {
				c, info := newCacheControlPreflightFixture(
					t,
					map[string]any{"system": []any{cacheSystemText("system")}},
					channelType,
					finalProtocol,
					dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
				)
				result := convertAndFinalizeRequestForPresenceTest(t, c, info)
				root, err := decodeCacheControlFinalBody(result)
				require.NoError(t, err)
				assert.False(t, targetOwnedCacheControlPresent(root, finalProtocol))
			})
		}

		t.Run("preserve/type-"+strconv.Itoa(channelType), func(t *testing.T) {
			c, info := newCacheControlPreflightFixture(
				t,
				map[string]any{"system": []any{cacheSystemText("system")}},
				channelType,
				ProtocolMessages,
				dto.OpenCodeGoUnsupportedOptionalFieldStrict,
			)
			result := convertAndFinalizeRequestForPresenceTest(t, c, info)
			root, err := decodeCacheControlFinalBody(result)
			require.NoError(t, err)
			value, present := cacheControlValueAtPath(root, cachePath(
				cacheKey("system"), cacheIndex(0), cacheKey("cache_control"),
			))
			require.True(t, present)
			digest, err := cacheControlMarkerDigestFromValue(value)
			require.NoError(t, err)
			assert.Equal(t, cacheControlMarkerDigest("5m"), digest)
		})

		t.Run("drop-automatic/type-"+strconv.Itoa(channelType), func(t *testing.T) {
			c, info := newCacheControlPreflightFixture(
				t,
				map[string]any{"cache_control": cacheMarker("5m")},
				channelType,
				ProtocolMessages,
				dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
			)
			result := convertAndFinalizeRequestForPresenceTest(t, c, info)
			root, err := decodeCacheControlFinalBody(result)
			require.NoError(t, err)
			_, present := root.(map[string]any)["cache_control"]
			assert.False(t, present)
		})
	}
}

func TestCacheControlFinalWireUsesNormalizedMessageIndex(t *testing.T) {
	tests := []struct {
		name              string
		messages          []any
		markedSourceIndex int
		markedTargetIndex int
		markedRole        string
		markedTTL         string
	}{
		{
			name: "user after leading system",
			messages: []any{
				map[string]any{"role": "system", "content": "system instruction"},
				cacheMessage("user", cacheTextTTL("marked user message", "1h")),
				cacheMessage("assistant", map[string]any{"type": "text", "text": "assistant reply"}),
			},
			markedSourceIndex: 1,
			markedTargetIndex: 0,
			markedRole:        "user",
			markedTTL:         "1h",
		},
		{
			name: "assistant after interleaved system",
			messages: []any{
				cacheMessage("user", map[string]any{"type": "text", "text": "user prompt"}),
				map[string]any{"role": "system", "content": "system instruction"},
				cacheMessage("assistant", cacheTextTTL("marked assistant message", "5m")),
			},
			markedSourceIndex: 2,
			markedTargetIndex: 1,
			markedRole:        "assistant",
			markedTTL:         "5m",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, info := newCacheControlPreflightFixture(
				t,
				map[string]any{"messages": test.messages},
				constant.ChannelTypeOpenCodeAPIKey,
				ProtocolMessages,
				dto.OpenCodeGoUnsupportedOptionalFieldStrict,
			)

			plan, err := BuildRequestPreflightPlan(c, info)
			require.NoError(t, err)
			cachePlan, err := cacheControlDispositionPlanFromPreflight(plan)
			require.NoError(t, err)
			require.Len(t, cachePlan.Entries, 1)
			entry := cachePlan.Entries[0]
			assert.Equal(t, test.markedSourceIndex, entry.SourcePath[1].Index)
			assert.Equal(t, test.markedTargetIndex, entry.NormalizedSourcePath[1].Index)
			assert.Equal(t, test.markedTargetIndex, entry.TargetPath[1].Index)

			result := convertAndFinalizeRequestForPresenceTest(t, c, info)
			root, err := decodeCacheControlFinalBody(result)
			require.NoError(t, err)
			rootObject := root.(map[string]any)
			assert.Equal(t, "system instruction", rootObject["system"])
			messages := rootObject["messages"].([]any)
			require.Len(t, messages, 2)

			markedMessage := messages[test.markedTargetIndex].(map[string]any)
			assert.Equal(t, test.markedRole, markedMessage["role"])

			value, present := cacheControlValueAtPath(root, cachePath(
				cacheKey("messages"), cacheIndex(test.markedTargetIndex), cacheKey("content"),
				cacheIndex(0), cacheKey("cache_control"),
			))
			require.True(t, present)
			digest, err := cacheControlMarkerDigestFromValue(value)
			require.NoError(t, err)
			assert.Equal(t, cacheControlMarkerDigest(test.markedTTL), digest)

			if test.markedSourceIndex != test.markedTargetIndex {
				_, staleSourceIndexPresent := cacheControlValueAtPath(root, cachePath(
					cacheKey("messages"), cacheIndex(test.markedSourceIndex), cacheKey("content"),
					cacheIndex(0), cacheKey("cache_control"),
				))
				assert.False(t, staleSourceIndexPresent)
			}
		})
	}
}

func TestCacheControlFinalWireNormalizationPreservesRawConversationExtensions(t *testing.T) {
	c, info := newCacheControlPreflightFixture(
		t,
		map[string]any{
			"messages": []any{
				map[string]any{"role": "system", "content": "system instruction"},
				map[string]any{
					"role": "user", "content": "user prompt",
					"provider_extension": map[string]any{"value": "raw-user-extension"},
				},
				map[string]any{
					"role": "assistant", "content": "assistant reply",
					"provider_extension": map[string]any{"value": "raw-assistant-extension"},
				},
			},
		},
		constant.ChannelTypeOpenCodeAPIKey,
		ProtocolMessages,
		dto.OpenCodeGoUnsupportedOptionalFieldStrict,
	)

	result := convertAndFinalizeRequestForPresenceTest(t, c, info)
	root, err := decodeCacheControlFinalBody(result)
	require.NoError(t, err)
	rootObject := root.(map[string]any)
	assert.Equal(t, "system instruction", rootObject["system"])
	messages := rootObject["messages"].([]any)
	require.Len(t, messages, 2)

	for index, want := range []struct {
		role      string
		extension string
	}{
		{role: "user", extension: "raw-user-extension"},
		{role: "assistant", extension: "raw-assistant-extension"},
	} {
		message := messages[index].(map[string]any)
		assert.Equal(t, want.role, message["role"])
		extension := message["provider_extension"].(map[string]any)
		assert.Equal(t, want.extension, extension["value"])
	}
}

func TestCacheControlFinalWireNormalizationStillRejectsUnsafeMessageMutation(t *testing.T) {
	const rawExtensionValue = "raw-normalized-extension"
	c, info := newCacheControlPreflightFixture(
		t,
		map[string]any{
			"messages": []any{
				map[string]any{"role": "system", "content": "system instruction"},
				map[string]any{
					"role": "user", "content": "user prompt",
					"provider_extension": map[string]any{"value": rawExtensionValue},
				},
				cacheMessage("assistant", map[string]any{"type": "text", "text": "assistant reply"}),
			},
		},
		constant.ChannelTypeOpenCodeAPIKey,
		ProtocolMessages,
		dto.OpenCodeGoUnsupportedOptionalFieldStrict,
	)
	plan, err := BuildRequestPreflightPlan(c, info)
	require.NoError(t, err)
	require.NoError(t, StoreRequestPreflightPlan(c, plan))
	info.InitChannelMeta(c)
	info.UpstreamModelName = plan.FinalModel
	info.IsModelMapped = plan.ModelMapped
	info.FinalRequestRelayFormat = plan.FinalProtocol.RelayFormat()

	converted, err := common.DeepCopy(info.Request.(*dto.ClaudeRequest))
	require.NoError(t, err)
	require.Len(t, converted.Messages, 2)
	converted.Messages = append([]dto.ClaudeMessage(nil), converted.Messages[1:]...)

	_, err = finalizeOutboundRequest(c, info, converted)
	require.Error(t, err)
	validationErr, ok := helper.AsClientRequestValidationError(err)
	require.True(t, ok)
	assert.Equal(t, RequestContractPreserveConflictRule, validationErr.RuleID)
	assert.NotContains(t, err.Error(), "provider_extension")
	assert.NotContains(t, err.Error(), rawExtensionValue)
}

func TestCacheControlFinalizerRejectsOperatorMutationAsConfigurationError(t *testing.T) {
	tests := []struct {
		name          string
		extra         map[string]any
		finalProtocol Protocol
		policy        string
		override      map[string]interface{}
		wantRule      string
	}{
		{
			name:          "removes a preserved marker",
			extra:         map[string]any{"system": []any{cacheSystemText("system")}},
			finalProtocol: ProtocolMessages,
			policy:        dto.OpenCodeGoUnsupportedOptionalFieldStrict,
			override: map[string]interface{}{
				"system": []any{map[string]any{"type": "text", "text": "operator replacement"}},
			},
			wantRule: CacheControlPreserveMutationRule,
		},
		{
			name:          "changes a preserved marker",
			extra:         map[string]any{"system": []any{cacheSystemText("system")}},
			finalProtocol: ProtocolMessages,
			policy:        dto.OpenCodeGoUnsupportedOptionalFieldStrict,
			override: map[string]interface{}{
				"system": []any{cacheSystemTextTTL("operator replacement", "1h")},
			},
			wantRule: CacheControlPreserveMutationRule,
		},
		{
			name:          "changes omitted ttl to explicit five minutes",
			extra:         map[string]any{"system": []any{cacheSystemTextTTL("system", "")}},
			finalProtocol: ProtocolMessages,
			policy:        dto.OpenCodeGoUnsupportedOptionalFieldStrict,
			override: map[string]interface{}{
				"system": []any{cacheSystemTextTTL("operator replacement", "5m")},
			},
			wantRule: CacheControlPreserveMutationRule,
		},
		{
			name:          "changes explicit five minutes to omitted ttl",
			extra:         map[string]any{"system": []any{cacheSystemTextTTL("system", "5m")}},
			finalProtocol: ProtocolMessages,
			policy:        dto.OpenCodeGoUnsupportedOptionalFieldStrict,
			override: map[string]interface{}{
				"system": []any{cacheSystemTextTTL("operator replacement", "")},
			},
			wantRule: CacheControlPreserveMutationRule,
		},
		{
			name: "introduces an unclassified marker",
			extra: map[string]any{"system": []any{map[string]any{
				"type": "text", "text": "system",
			}}},
			finalProtocol: ProtocolMessages,
			policy:        dto.OpenCodeGoUnsupportedOptionalFieldStrict,
			override: map[string]interface{}{
				"system": []any{cacheSystemText("operator replacement")},
			},
			wantRule: CacheControlUnexpectedMarkerRule,
		},
		{
			name:          "introduces a nested unclassified marker",
			extra:         map[string]any{},
			finalProtocol: ProtocolChat,
			policy:        dto.OpenCodeGoUnsupportedOptionalFieldStrict,
			override: map[string]interface{}{
				"messages": []any{map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []any{map[string]any{
						"id": "call-1", "type": "function",
						"function": map[string]any{
							"name": "lookup", "arguments": `{}`,
							"cache_control": cacheMarker("5m"),
						},
					}},
				}},
			},
			wantRule: CacheControlUnexpectedMarkerRule,
		},
		{
			name:          "reintroduces a planned cross protocol drop",
			extra:         map[string]any{"system": []any{cacheSystemText("system")}},
			finalProtocol: ProtocolChat,
			policy:        dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
			override: map[string]interface{}{
				"messages": []any{map[string]any{
					"role": "user",
					"content": []any{map[string]any{
						"type": "text", "text": "operator replacement", "cache_control": cacheMarker("5m"),
					}},
				}},
			},
			wantRule: CacheControlDropAssertionRule,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, info := newCacheControlPreflightFixture(
				t,
				test.extra,
				constant.ChannelTypeOpenCodeAPIKey,
				test.finalProtocol,
				test.policy,
			)
			plan, err := BuildRequestPreflightPlan(c, info)
			require.NoError(t, err)
			require.NoError(t, StoreRequestPreflightPlan(c, plan))
			info.InitChannelMeta(c)
			info.UpstreamModelName = plan.FinalModel
			info.IsModelMapped = plan.ModelMapped

			adaptor := &Adaptor{}
			request := info.Request.(*dto.ClaudeRequest)
			converted, err := adaptor.ConvertClaudeRequest(c, info, request)
			require.NoError(t, err)
			info.ParamOverride = test.override

			_, err = adaptor.FinalizeOutboundRequest(c, info, converted)
			require.Error(t, err)
			finalizerErr, ok := AsCacheControlFinalizerError(err)
			require.True(t, ok)
			assert.True(t, finalizerErr.Configuration)
			assert.Equal(t, test.wantRule, finalizerErr.RuleID)
			assert.Equal(t, CacheControlFinalizerStage, finalizerErr.StageID)

			classified := NewRequestPreflightFinalizationError(err)
			preflightErr, ok := AsRequestPreflightError(classified)
			require.True(t, ok)
			assert.Equal(t, http.StatusServiceUnavailable, preflightErr.StatusCode)
			assert.Equal(t, types.ErrorOriginGatewayConfig, preflightErr.Origin)
			assert.Equal(t, test.wantRule, preflightErr.RuleID)
			assert.Equal(t, CacheControlFinalizerStage, preflightErr.StageID)
		})
	}
}

func TestCacheControlFinalizerIgnoresOpaqueTargetData(t *testing.T) {
	c, info := newCacheControlPreflightFixture(
		t,
		map[string]any{
			"tools": []any{map[string]any{
				"name": "lookup",
				"input_schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"cache_control": map[string]any{"type": "string"},
					},
				},
			}},
		},
		constant.ChannelTypeOpenCodeAPIKey,
		ProtocolChat,
		dto.OpenCodeGoUnsupportedOptionalFieldStrict,
	)

	result := convertAndFinalizeRequestForPresenceTest(t, c, info)
	root, err := decodeCacheControlFinalBody(result)
	require.NoError(t, err)
	parameters, present := cacheControlValueAtPath(root, cachePath(
		cacheKey("tools"), cacheIndex(0), cacheKey("function"), cacheKey("parameters"),
		cacheKey("properties"), cacheKey("cache_control"),
	))
	require.True(t, present)
	assert.Equal(t, map[string]any{"type": "string"}, parameters)
}

func TestCacheControlFinalizerIgnoresOpaqueCustomToolData(t *testing.T) {
	root := map[string]any{
		"tools": []any{map[string]any{
			"type": "custom",
			"custom": map[string]any{
				"cache_control": map[string]any{"customer": true},
			},
		}},
	}
	assert.Empty(t, targetOwnedCacheControlPaths(root, ProtocolChat))
}

func TestCacheControlFinalizerRejectsPreOperatorIntroductionAsInvariant(t *testing.T) {
	c, info := newCacheControlPreflightFixture(
		t,
		map[string]any{"system": []any{map[string]any{"type": "text", "text": "system"}}},
		constant.ChannelTypeOpenCodeAPIKey,
		ProtocolMessages,
		dto.OpenCodeGoUnsupportedOptionalFieldStrict,
	)
	plan, err := BuildRequestPreflightPlan(c, info)
	require.NoError(t, err)
	require.NoError(t, StoreRequestPreflightPlan(c, plan))
	info.InitChannelMeta(c)
	info.UpstreamModelName = plan.FinalModel
	info.IsModelMapped = plan.ModelMapped

	adaptor := &Adaptor{}
	request := info.Request.(*dto.ClaudeRequest)
	converted, err := adaptor.ConvertClaudeRequest(c, info, request)
	require.NoError(t, err)
	convertedRequest, ok := converted.(*dto.ClaudeRequest)
	require.True(t, ok)
	convertedRequest.CacheControl = []byte(`{"type":"ephemeral"}`)

	_, err = adaptor.FinalizeOutboundRequest(c, info, convertedRequest)
	require.Error(t, err)
	finalizerErr, ok := AsCacheControlFinalizerError(err)
	require.True(t, ok)
	assert.False(t, finalizerErr.Configuration)
	assert.Equal(t, CacheControlUnexpectedMarkerRule, finalizerErr.RuleID)
	assert.Equal(t, CacheControlFinalizerStage, finalizerErr.StageID)

	classified := NewRequestPreflightFinalizationError(err)
	preflightErr, ok := AsRequestPreflightError(classified)
	require.True(t, ok)
	assert.Equal(t, http.StatusInternalServerError, preflightErr.StatusCode)
	assert.Equal(t, types.ErrorOriginGatewayInvariant, preflightErr.Origin)
	assert.Equal(t, CacheControlUnexpectedMarkerRule, preflightErr.RuleID)
	assert.Equal(t, CacheControlFinalizerStage, preflightErr.StageID)
}

func FuzzCacheControlContractDeterministic(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"model":"qwen3.8-max","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}]}`),
		[]byte(`{"model":"qwen3.8-max","max_tokens":16,"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"0":{"cache_control":{"type":"string"}}}}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello","\u0063ache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`),
		[]byte(`{"model":"qwen3.8-max","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","type":"ephemeral"}}]}]}`),
		[]byte(`{"model":"qwen3.8-max","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"lookup","input":{"cache_control":{"customer":true}}}]}]}`),
		[]byte(`{"model":"qwen3.8-max","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","padding":"` + strings.Repeat("x", 4096) + `"}}]}]}`),
		[]byte(`{"model":"qwen3.8-max","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"opaque":` + strings.Repeat(`{"nested":`, 80) + `null` + strings.Repeat(`}`, 80) + `}`),
	}

	parts := make([]any, 129)
	for index := range parts {
		parts[index] = map[string]any{"type": "text", "text": "unmarked"}
	}
	parts[len(parts)-1] = cacheText("marked")
	largeIndexBody, err := common.Marshal(map[string]any{
		"model": "qwen3.8-max", "max_tokens": 16,
		"messages": []any{cacheMessage("user", parts...)},
	})
	if err != nil {
		f.Fatal(err)
	}
	seeds = append(seeds, largeIndexBody)
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 128<<10 {
			return
		}
		c, request, err := newCacheControlContractRawFixtureWithoutRequest(t, body)
		if err != nil || request == nil {
			return
		}
		envelope, found, err := helper.GetValidatedRequestEnvelope(c, types.RelayFormatClaude)
		if err != nil || !found || envelope == nil {
			return
		}

		for _, protocol := range []Protocol{ProtocolMessages, ProtocolChat, ProtocolResponses} {
			for _, policy := range []string{
				dto.OpenCodeGoUnsupportedOptionalFieldStrict,
				dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
			} {
				first, firstErr := BuildCacheControlDispositionPlan(
					envelope, types.RelayFormatClaude, protocol, request, policy,
				)
				second, secondErr := BuildCacheControlDispositionPlan(
					envelope, types.RelayFormatClaude, protocol, request, policy,
				)
				if (firstErr == nil) != (secondErr == nil) {
					t.Fatalf("cache-control classification changed success state")
				}
				if firstErr != nil {
					if firstErr.Error() != secondErr.Error() {
						t.Fatalf("cache-control classification changed error identity")
					}
					continue
				}
				if first.Canonical != second.Canonical || first.Fingerprint != second.Fingerprint {
					t.Fatalf("cache-control classification is not deterministic")
				}
				if err := validateCacheControlPlanShape(first); err != nil {
					t.Fatalf("cache-control classifier produced an invalid plan: %v", err)
				}
			}
		}
	})
}

func cacheMarker(ttl string) map[string]any {
	marker := map[string]any{"type": "ephemeral"}
	if ttl != "" {
		marker["ttl"] = ttl
	}
	return marker
}

func cacheSystemText(text string) map[string]any {
	return cacheSystemTextTTL(text, "5m")
}

func cacheSystemTextTTL(text string, ttl string) map[string]any {
	return map[string]any{"type": "text", "text": text, "cache_control": cacheMarker(ttl)}
}

func cacheText(text string) map[string]any {
	return cacheTextTTL(text, "5m")
}

func cacheTextTTL(text string, ttl string) map[string]any {
	return map[string]any{"type": "text", "text": text, "cache_control": cacheMarker(ttl)}
}

func cacheMessage(role string, content ...any) map[string]any {
	return map[string]any{"role": role, "content": content}
}

func capturedSystemRoleCacheBody() map[string]any {
	return map[string]any{
		"system": []any{
			map[string]any{"type": "text", "text": "unmarked system"},
			cacheSystemText("top-level one"),
			cacheSystemText("top-level two"),
		},
		"messages": []any{
			cacheMessage("user", map[string]any{"type": "text", "text": "hello"}),
			cacheMessage("system", cacheText("message-system")),
		},
	}
}

func chatToolOpaqueCacheControlBody() map[string]any {
	return chatToolOpaqueCacheControlBodyForRole("tool")
}

func chatToolOpaqueCacheControlBodyForRole(role string) map[string]any {
	message := map[string]any{
		"role": role,
		"content": []any{map[string]any{
			"type": "text",
			"text": "opaque tool result",
			"cache_control": map[string]any{
				"customer": "opaque-tool-content-cache-sentinel",
			},
			"provider_extension": map[string]any{
				"cache_control": map[string]any{
					"customer": "opaque-tool-content-extension-cache-sentinel",
				},
			},
		}},
		"provider_extension": map[string]any{
			"cache_control": map[string]any{
				"customer": "opaque-tool-message-extension-cache-sentinel",
			},
		},
	}
	if role == "function" {
		message["name"] = "lookup"
	} else {
		message["tool_call_id"] = "call-1"
	}
	return map[string]any{
		"model":    "glm-5.2",
		"messages": []any{message},
	}
}

func cacheTool(name string, ttl string) map[string]any {
	return map[string]any{
		"name": name, "input_schema": map[string]any{"type": "object"},
		"cache_control": cacheMarker(ttl),
	}
}

func cacheBodyWithTextMarker(marker any) map[string]any {
	return map[string]any{"messages": []any{cacheMessage("user", map[string]any{
		"type": "text", "text": "hello", "cache_control": marker,
	})}}
}

func newCacheControlContractFixture(
	t *testing.T,
	extra map[string]any,
) (*helper.ValidatedRequestEnvelope, *dto.ClaudeRequest) {
	t.Helper()
	body := map[string]any{
		"model":      "qwen3.8-max",
		"max_tokens": 16,
		"messages":   []any{cacheMessage("user", map[string]any{"type": "text", "text": "hello"})},
	}
	for key, value := range extra {
		body[key] = value
	}
	encoded, err := common.Marshal(body)
	require.NoError(t, err)

	return newCacheControlContractRawFixture(t, encoded)
}

func newCacheControlSourceFormatFixture(
	t *testing.T,
	format types.RelayFormat,
	body map[string]any,
) (*helper.ValidatedRequestEnvelope, dto.Request) {
	t.Helper()
	encoded, err := common.Marshal(body)
	require.NoError(t, err)
	path := "/v1/chat/completions"
	if format == types.RelayFormatOpenAIResponses {
		path = "/v1/responses"
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	storage, err := common.CreateBodyStorage(encoded)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })
	c.Set(common.KeyBodyStorage, storage)

	request, err := helper.GetAndValidateRequest(c, format)
	require.NoError(t, err)
	envelope, found, err := helper.GetValidatedRequestEnvelope(c, format)
	require.NoError(t, err)
	require.True(t, found)
	return envelope, request
}

func newCacheControlContractRawFixture(
	t *testing.T,
	body []byte,
) (*helper.ValidatedRequestEnvelope, *dto.ClaudeRequest) {
	t.Helper()
	c, request, err := newCacheControlContractRawFixtureWithoutRequest(t, body)
	require.NoError(t, err)
	envelope, found, err := helper.GetValidatedRequestEnvelope(c, types.RelayFormatClaude)
	require.NoError(t, err)
	require.True(t, found)
	return envelope, request
}

func newCacheControlContractRawFixtureWithoutRequest(
	t *testing.T,
	body []byte,
) (*gin.Context, *dto.ClaudeRequest, error) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	storage, err := common.CreateBodyStorage(body)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })
	c.Set(common.KeyBodyStorage, storage)

	rawRequest, err := helper.GetAndValidateRequest(c, types.RelayFormatClaude)
	if err != nil {
		return c, nil, err
	}
	request, ok := rawRequest.(*dto.ClaudeRequest)
	if !ok {
		return c, nil, nil
	}
	return c, request, nil
}

func newCacheControlPreflightFixture(
	t *testing.T,
	extra map[string]any,
	channelType int,
	finalProtocol Protocol,
	policy string,
) (*gin.Context, *relaycommon.RelayInfo) {
	t.Helper()
	body := map[string]any{
		"model":      "qwen3.8-max",
		"max_tokens": 16,
		"messages":   []any{cacheMessage("user", map[string]any{"type": "text", "text": "hello"})},
	}
	for key, value := range extra {
		body[key] = value
	}
	encoded, err := common.Marshal(body)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(encoded))
	storage, err := common.CreateBodyStorage(encoded)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })
	c.Set(common.KeyBodyStorage, storage)
	common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
	common.SetContextKey(c, constant.ContextKeyChannelId, 6200+channelType)
	common.SetContextKey(c, constant.ContextKeyChannelModelMapping, "")
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{
		OpenCodeGo: &dto.OpenCodeGoConfig{
			ModelProtocols:                 map[string]string{"qwen3.8-max": string(finalProtocol)},
			UnsupportedOptionalFieldPolicy: policy,
		},
	})
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "qwen3.8-max")

	request, err := helper.GetAndValidateRequest(c, types.RelayFormatClaude)
	require.NoError(t, err)
	info, err := relaycommon.GenRelayInfo(c, types.RelayFormatClaude, request, nil)
	require.NoError(t, err)
	return c, info
}

func requireCacheControlClientRule(t *testing.T, err error, rule string) {
	t.Helper()
	requireCacheControlClientRuleAtStage(t, err, rule, CacheControlPreflightStage)
}

func requireCacheControlClientRuleAtStage(t *testing.T, err error, rule string, stage string) {
	t.Helper()
	require.Error(t, err)
	validationErr, ok := helper.AsClientRequestValidationError(err)
	require.True(t, ok)
	assert.Equal(t, http.StatusBadRequest, validationErr.StatusCode)
	assert.Equal(t, rule, validationErr.RuleID)
	assert.Equal(t, stage, validationErr.StageID)
	assert.Equal(t, RequestContractPublicMessage, validationErr.Message)
}
