package opencodego

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This fixture retains only the contract-relevant shape captured from Claude
// Code 2.1.234. All prompt, tool, and metadata values are synthetic.
func capturedClaudeCodeResumeShape(streamCase claudeReplayStreamCase) map[string]any {
	marker := map[string]any{"type": "ephemeral"}
	fields := map[string]any{
		"metadata":      map[string]any{"user_id": "synthetic-session"},
		"thinking":      map[string]any{"type": "adaptive", "display": "omitted"},
		"output_config": map[string]any{"effort": "high"},
		"context_management": map[string]any{"edits": []any{
			map[string]any{"type": "clear_thinking_20251015", "keep": "all"},
		}},
		"system": []any{
			map[string]any{"type": "text", "text": "synthetic base system"},
			map[string]any{"type": "text", "text": "synthetic cached system one", "cache_control": marker},
			map[string]any{"type": "text", "text": "synthetic cached system two", "cache_control": marker},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "synthetic first turn"},
				map[string]any{"type": "text", "text": "synthetic injected context"},
			}},
			map[string]any{"role": "system", "content": "synthetic injected system one"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "synthetic replay reasoning", "signature": ""},
				map[string]any{"type": "text", "text": "synthetic replay answer"},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "synthetic continuation"},
				map[string]any{"type": "text", "text": "synthetic continued context"},
			}},
			map[string]any{"role": "system", "content": "synthetic injected system two"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "synthetic second answer"},
			}},
			map[string]any{"role": "user", "content": "synthetic final question"},
			map[string]any{"role": "system", "content": []any{
				map[string]any{
					"type": "text", "text": "synthetic cached injected system", "cache_control": marker,
				},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "synthetic tool reasoning", "signature": ""},
				map[string]any{
					"type": "tool_use", "id": "synthetic-call", "name": "Bash",
					"input": map[string]any{"command": "read-only synthetic command"},
				},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{
					"type": "tool_result", "tool_use_id": "synthetic-call",
					"content": "synthetic successful result", "is_error": false,
				},
			}},
		},
		"tools": []any{},
	}
	if streamCase.value != nil {
		fields["stream"] = *streamCase.value
	}
	return fields
}

func TestCapturedClaudeCodeResumeShapeReachesChatAfterKnownCacheDrop(t *testing.T) {
	for _, channelType := range []int{
		constant.ChannelTypeOpenCodeGo,
		constant.ChannelTypeOpenCodeAPIKey,
	} {
		for _, streamCase := range claudeReplayStreamCases {
			name := fmt.Sprintf("type-%d/stream-%s", channelType, streamCase.name)
			t.Run(name, func(t *testing.T) {
				c, info := newRequestContractFixture(
					t,
					channelType,
					requestPreflightEndpoints[0],
					ProtocolChat,
					capturedClaudeCodeResumeShape(streamCase),
				)
				setClaudeReplayPolicy(c, ProtocolChat, dto.OpenCodeGoUnsupportedOptionalFieldDropKnown)

				plan, err := BuildRequestPreflightPlan(c, info)
				require.NoError(t, err)
				assert.Equal(t, 3, plan.CacheControlDropCount)
				assert.Zero(t, plan.CacheControlPreserveCount)

				wire := convertAndFinalizeRequestForPresenceTest(t, c, info)
				var root map[string]any
				require.NoError(t, common.Unmarshal(wire, &root))
				assert.Equal(t, "high", root["reasoning_effort"])
				assert.NotContains(t, root, "output_config")
				assert.NotContains(t, root, "context_management")
				assert.NotContains(t, string(wire), "cache_control")
				assert.NotContains(t, string(wire), "signature")
				assert.NotContains(t, string(wire), "is_error")

				messages, ok := root["messages"].([]any)
				require.True(t, ok)
				require.NotEmpty(t, messages)
				assert.True(t, capturedChatReasoningPresent(messages))
			})
		}
	}
}

func TestCapturedClaudeCodeResumeShapeStrictPolicyReturnsSafe400(t *testing.T) {
	for _, channelType := range []int{
		constant.ChannelTypeOpenCodeGo,
		constant.ChannelTypeOpenCodeAPIKey,
	} {
		for _, streamCase := range claudeReplayStreamCases {
			name := fmt.Sprintf("type-%d/stream-%s", channelType, streamCase.name)
			t.Run(name, func(t *testing.T) {
				c, info := newRequestContractFixture(
					t,
					channelType,
					requestPreflightEndpoints[0],
					ProtocolChat,
					capturedClaudeCodeResumeShape(streamCase),
				)
				setClaudeReplayPolicy(c, ProtocolChat, dto.OpenCodeGoUnsupportedOptionalFieldStrict)

				_, err := BuildRequestPreflightPlan(c, info)
				require.Error(t, err)
				preflightErr, ok := AsRequestPreflightError(err)
				require.True(t, ok)
				assert.Equal(t, http.StatusBadRequest, preflightErr.StatusCode)
				assert.Equal(t, types.ErrorOriginLocalValidation, preflightErr.Origin)
				assert.Equal(t, CacheControlUnsupportedRule, preflightErr.RuleID)
				assert.Equal(t, CacheControlPreflightStage, preflightErr.StageID)
				assert.Equal(t, RequestContractPublicMessage, preflightErr.Message)
				assert.NotContains(t, err.Error(), "synthetic")
			})
		}
	}
}

func TestCapturedClaudeCodeTaskBudgetRemainsSafePreflight400(t *testing.T) {
	for _, channelType := range []int{
		constant.ChannelTypeOpenCodeGo,
		constant.ChannelTypeOpenCodeAPIKey,
	} {
		for _, streamCase := range claudeReplayStreamCases {
			for _, policy := range claudeReplayPolicies {
				name := fmt.Sprintf("type-%d/stream-%s/%s", channelType, streamCase.name, policy)
				t.Run(name, func(t *testing.T) {
					fields := map[string]any{
						"output_config": map[string]any{
							"effort": "high",
							"task_budget": map[string]any{
								"type": "tokens", "total": 100,
							},
						},
					}
					if streamCase.value != nil {
						fields["stream"] = *streamCase.value
					}
					c, info := newRequestContractFixture(
						t,
						channelType,
						requestPreflightEndpoints[0],
						ProtocolChat,
						fields,
					)
					setClaudeReplayPolicy(c, ProtocolChat, policy)

					_, err := BuildRequestPreflightPlan(c, info)
					require.Error(t, err)
					preflightErr, ok := AsRequestPreflightError(err)
					require.True(t, ok)
					assert.Equal(t, http.StatusBadRequest, preflightErr.StatusCode)
					assert.Equal(t, types.ErrorOriginLocalValidation, preflightErr.Origin)
					assert.Equal(t, ClaudeOutputConfigUnsupportedRule, preflightErr.RuleID)
					assert.Equal(t, ClaudeFieldContractPreflightStage, preflightErr.StageID)
					assert.Equal(t, RequestContractPublicMessage, preflightErr.Message)
					assert.NotContains(t, err.Error(), "task_budget")
				})
			}
		}
	}
}

func capturedChatReasoningPresent(messages []any) bool {
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		if message["reasoning_content"] == "synthetic replay reasoning" {
			return true
		}
	}
	return false
}
