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
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	claudeReplayCallID       = "toolu_replay_1"
	claudeReplayPrivateValue = "synthetic-private-replay-marker"
)

type claudeReplayStreamCase struct {
	name    string
	value   *bool
	present bool
}

var claudeReplayStreamCases = []claudeReplayStreamCase{
	{name: "omitted"},
	{name: "false", value: common.GetPointer(false), present: true},
	{name: "true", value: common.GetPointer(true), present: true},
}

var claudeReplayPolicies = []string{
	dto.OpenCodeGoUnsupportedOptionalFieldStrict,
	dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
}

func TestClaudeMessagesReplayMatrixPreservesTextThinkingAndBashHistory(t *testing.T) {
	for _, channelType := range []int{
		constant.ChannelTypeOpenCodeGo,
		constant.ChannelTypeOpenCodeAPIKey,
	} {
		for _, finalProtocol := range []Protocol{ProtocolChat, ProtocolResponses} {
			for _, streamCase := range claudeReplayStreamCases {
				for _, policy := range claudeReplayPolicies {
					name := fmt.Sprintf(
						"type-%d/%s/stream-%s/%s",
						channelType,
						finalProtocol,
						streamCase.name,
						policy,
					)
					t.Run(name, func(t *testing.T) {
						extra := claudeReplayPositiveFields()
						if streamCase.value != nil {
							extra["stream"] = *streamCase.value
						}
						c, info := newRequestContractFixture(
							t,
							channelType,
							requestPreflightEndpoints[0],
							finalProtocol,
							extra,
						)
						setClaudeReplayPolicy(c, finalProtocol, policy)

						wire := convertAndFinalizeRequestForPresenceTest(t, c, info)
						var root map[string]any
						require.NoError(t, common.Unmarshal(wire, &root))
						assertClaudeReplayStream(t, root, streamCase)

						switch finalProtocol {
						case ProtocolChat:
							assertClaudeReplayChatWire(t, root)
						case ProtocolResponses:
							assertClaudeReplayResponsesWire(t, root)
						default:
							t.Fatalf("unexpected protocol %q", finalProtocol)
						}
					})
				}
			}
		}
	}
}

func TestClaudeMessagesReplayMatrixRejectsUnrepresentableContentAtPreflight(t *testing.T) {
	tests := []struct {
		name          string
		messages      []any
		expectedRule  string
		expectedStage string
	}{
		{
			name: "tool result image",
			messages: claudeReplayToolResultMessages(map[string]any{
				"type": "tool_result", "tool_use_id": claudeReplayCallID,
				"content": []any{map[string]any{
					"type": "image",
					"source": map[string]any{
						"type": "base64", "media_type": "image/png", "data": "c3ludGhldGlj",
					},
				}},
			}),
		},
		{
			name: "tool result document",
			messages: claudeReplayToolResultMessages(map[string]any{
				"type": "tool_result", "tool_use_id": claudeReplayCallID,
				"content": []any{claudeReplayDocumentBlock()},
			}),
		},
		{
			name: "tool result mixed text and image",
			messages: claudeReplayToolResultMessages(map[string]any{
				"type": "tool_result", "tool_use_id": claudeReplayCallID,
				"content": []any{
					map[string]any{"type": "text", "text": claudeReplayPrivateValue},
					map[string]any{
						"type": "image",
						"source": map[string]any{
							"type": "base64", "media_type": "image/png", "data": "c3ludGhldGlj",
						},
					},
				},
			}),
		},
		{
			name: "tool result is error",
			messages: claudeReplayToolResultMessages(map[string]any{
				"type": "tool_result", "tool_use_id": claudeReplayCallID,
				"content": claudeReplayPrivateValue, "is_error": true,
			}),
		},
		{
			name: "standalone document",
			messages: []any{map[string]any{
				"role": "user", "content": []any{claudeReplayDocumentBlock()},
			}},
		},
		{
			name:          "standalone document cache marker",
			expectedRule:  CacheControlParentRule,
			expectedStage: CacheControlPreflightStage,
			messages: []any{map[string]any{
				"role": "user", "content": []any{func() map[string]any {
					document := claudeReplayDocumentBlock()
					document["cache_control"] = map[string]any{"type": "ephemeral"}
					return document
				}()},
			}},
		},
	}

	for _, channelType := range []int{
		constant.ChannelTypeOpenCodeGo,
		constant.ChannelTypeOpenCodeAPIKey,
	} {
		for _, finalProtocol := range []Protocol{ProtocolChat, ProtocolResponses} {
			for _, streamCase := range claudeReplayStreamCases {
				for _, policy := range claudeReplayPolicies {
					for _, test := range tests {
						name := fmt.Sprintf(
							"type-%d/%s/stream-%s/%s/%s",
							channelType,
							finalProtocol,
							streamCase.name,
							policy,
							test.name,
						)
						t.Run(name, func(t *testing.T) {
							extra := map[string]any{"messages": test.messages}
							if streamCase.value != nil {
								extra["stream"] = *streamCase.value
							}
							c, info := newRequestContractFixture(
								t,
								channelType,
								requestPreflightEndpoints[0],
								finalProtocol,
								extra,
							)
							setClaudeReplayPolicy(c, finalProtocol, policy)

							_, err := BuildRequestPreflightPlan(c, info)
							require.Error(t, err)
							preflightErr, ok := AsRequestPreflightError(err)
							require.True(t, ok)
							assert.Equal(t, http.StatusBadRequest, preflightErr.StatusCode)
							assert.Equal(t, types.ErrorOriginLocalValidation, preflightErr.Origin)
							expectedRule := test.expectedRule
							if expectedRule == "" {
								expectedRule = RequestContractUnmappedNestedRule
							}
							expectedStage := test.expectedStage
							if expectedStage == "" {
								expectedStage = RequestContractPreflightStage
							}
							assert.Equal(t, expectedRule, preflightErr.RuleID)
							assert.Equal(t, expectedStage, preflightErr.StageID)
							assert.Equal(t, RequestContractPublicMessage, preflightErr.Message)
							assertClaudeReplayErrorIsSanitized(t, err)
						})
					}
				}
			}
		}
	}
}

func TestClaudeMessagesReplayMatrixRejectsUnknownToolResultAtIngress(t *testing.T) {
	for _, channelType := range []int{
		constant.ChannelTypeOpenCodeGo,
		constant.ChannelTypeOpenCodeAPIKey,
	} {
		for _, finalProtocol := range []Protocol{ProtocolChat, ProtocolResponses} {
			for _, streamCase := range claudeReplayStreamCases {
				for _, policy := range claudeReplayPolicies {
					name := fmt.Sprintf(
						"type-%d/%s/stream-%s/%s",
						channelType,
						finalProtocol,
						streamCase.name,
						policy,
					)
					t.Run(name, func(t *testing.T) {
						body := map[string]any{
							"model":      "glm-5.2",
							"max_tokens": 16,
							"messages": claudeReplayToolResultMessages(map[string]any{
								"type": "tool_result", "tool_use_id": claudeReplayCallID,
								"content": []any{map[string]any{
									"type": "future_private_block", "value": claudeReplayPrivateValue,
								}},
							}),
						}
						if streamCase.value != nil {
							body["stream"] = *streamCase.value
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
						setClaudeReplayPolicy(c, finalProtocol, policy)

						_, err = helper.GetAndValidateRequest(c, types.RelayFormatClaude)
						require.Error(t, err)
						validationErr, ok := helper.AsClientRequestValidationError(err)
						require.True(t, ok)
						assert.Equal(t, http.StatusBadRequest, validationErr.StatusCode)
						assertClaudeReplayErrorIsSanitized(t, err)
					})
				}
			}
		}
	}
}

func claudeReplayPositiveFields() map[string]any {
	return map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "text", "text": "read the attached local reference",
				}},
			},
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type": "thinking", "thinking": "inspect the local reference", "signature": "signed-replay",
					},
					map[string]any{"type": "text", "text": "I will inspect it."},
					map[string]any{
						"type": "tool_use", "id": claudeReplayCallID, "name": "Bash",
						"input": map[string]any{"command": "cat -- <local-reference>"},
					},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": claudeReplayCallID,
					"content": []any{
						map[string]any{"type": "text", "text": "first"},
						map[string]any{"type": "text", "text": " second"},
					},
				}},
			},
		},
		"tools": []any{map[string]any{
			"name": "Bash", "description": "Read a local reference",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string"},
				},
			},
		}},
	}
}

func claudeReplayToolResultMessages(toolResult map[string]any) []any {
	return []any{
		map[string]any{
			"role": "assistant",
			"content": []any{map[string]any{
				"type": "tool_use", "id": claudeReplayCallID, "name": "Bash",
				"input": map[string]any{"command": "synthetic"},
			}},
		},
		map[string]any{"role": "user", "content": []any{toolResult}},
	}
}

func claudeReplayDocumentBlock() map[string]any {
	return map[string]any{
		"type": "document",
		"source": map[string]any{
			"type": "text", "media_type": "text/plain", "data": claudeReplayPrivateValue,
		},
		"title": claudeReplayPrivateValue,
	}
}

func setClaudeReplayPolicy(c *gin.Context, finalProtocol Protocol, policy string) {
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{
		OpenCodeGo: &dto.OpenCodeGoConfig{
			ModelProtocols:                 map[string]string{"glm-5.2": string(finalProtocol)},
			UnsupportedOptionalFieldPolicy: policy,
		},
	})
}

func assertClaudeReplayStream(t *testing.T, root map[string]any, streamCase claudeReplayStreamCase) {
	t.Helper()
	stream, present := root["stream"]
	assert.Equal(t, streamCase.present, present)
	if !streamCase.present {
		return
	}
	// Function-tool Claude streams are deliberately buffered upstream. The
	// client stream presence survives, while the final upstream value is false.
	assert.Equal(t, false, stream)
}

func assertClaudeReplayChatWire(t *testing.T, root map[string]any) {
	t.Helper()
	messages, ok := root["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 3)

	user := claudeReplayObject(t, messages[0])
	assert.Equal(t, "user", user["role"])
	assertClaudeReplayTextPart(t, user["content"], "text", "read the attached local reference")

	assistant := claudeReplayObject(t, messages[1])
	assert.Equal(t, "assistant", assistant["role"])
	assert.Equal(t, "I will inspect it.", assistant["content"])
	assert.Equal(t, "inspect the local reference", assistant["reasoning_content"])
	toolCalls, ok := assistant["tool_calls"].([]any)
	require.True(t, ok)
	require.Len(t, toolCalls, 1)
	toolCall := claudeReplayObject(t, toolCalls[0])
	assert.Equal(t, claudeReplayCallID, toolCall["id"])
	function := claudeReplayObject(t, toolCall["function"])
	assert.Equal(t, "Bash", function["name"])
	arguments, ok := function["arguments"].(string)
	require.True(t, ok)
	var decodedArguments map[string]any
	require.NoError(t, json.Unmarshal([]byte(arguments), &decodedArguments))
	assert.Equal(t, "cat -- <local-reference>", decodedArguments["command"])

	toolResult := claudeReplayObject(t, messages[2])
	assert.Equal(t, "tool", toolResult["role"])
	assert.Equal(t, claudeReplayCallID, toolResult["tool_call_id"])
	assert.Equal(t, "first second", toolResult["content"])
}

func assertClaudeReplayResponsesWire(t *testing.T, root map[string]any) {
	t.Helper()
	input, ok := root["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 5)

	user := claudeReplayObject(t, input[0])
	assert.Equal(t, "user", user["role"])
	assertClaudeReplayTextPart(t, user["content"], "input_text", "read the attached local reference")

	reasoning := claudeReplayObject(t, input[1])
	assert.Equal(t, "reasoning", reasoning["type"])
	assert.Equal(t, "completed", reasoning["status"])
	assert.Regexp(t, `^rs_[0-9a-f]{24}$`, reasoning["id"])
	summary, ok := reasoning["summary"].([]any)
	require.True(t, ok)
	require.Len(t, summary, 1)
	summaryPart := claudeReplayObject(t, summary[0])
	assert.Equal(t, "summary_text", summaryPart["type"])
	assert.Equal(t, "inspect the local reference", summaryPart["text"])

	assistant := claudeReplayObject(t, input[2])
	assert.Equal(t, "assistant", assistant["role"])
	assert.Equal(t, "I will inspect it.", assistant["content"])

	functionCall := claudeReplayObject(t, input[3])
	assert.Equal(t, "function_call", functionCall["type"])
	assert.Equal(t, claudeReplayCallID, functionCall["id"])
	assert.Equal(t, claudeReplayCallID, functionCall["call_id"])
	assert.Equal(t, "Bash", functionCall["name"])
	arguments, ok := functionCall["arguments"].(string)
	require.True(t, ok)
	var decodedArguments map[string]any
	require.NoError(t, json.Unmarshal([]byte(arguments), &decodedArguments))
	assert.Equal(t, "cat -- <local-reference>", decodedArguments["command"])

	toolResult := claudeReplayObject(t, input[4])
	assert.Equal(t, "function_call_output", toolResult["type"])
	assert.Equal(t, claudeReplayCallID, toolResult["call_id"])
	assert.Equal(t, "first second", toolResult["output"])

	for _, rawItem := range input {
		item := claudeReplayObject(t, rawItem)
		if item["role"] == "assistant" {
			content, present := item["content"]
			assert.True(t, present)
			assert.NotEqual(t, "", content)
		}
	}
}

func assertClaudeReplayErrorIsSanitized(t *testing.T, err error) {
	t.Helper()
	for _, privateValue := range []string{
		claudeReplayPrivateValue,
		claudeReplayCallID,
		"future_private_block",
		"application/pdf",
		"text/plain",
	} {
		assert.NotContains(t, err.Error(), privateValue)
	}
}

func claudeReplayObject(t *testing.T, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	require.Truef(t, ok, "expected object, got %T", value)
	return object
}

func assertClaudeReplayTextPart(t *testing.T, value any, partType string, text string) {
	t.Helper()
	parts, ok := value.([]any)
	require.True(t, ok)
	require.Len(t, parts, 1)
	part := claudeReplayObject(t, parts[0])
	assert.Equal(t, partType, part["type"])
	assert.Equal(t, text, part["text"])
}
