package opencodego

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeChatProjectionAcceptsProvenFieldDispositions(t *testing.T) {
	tests := []struct {
		name                    string
		extra                   map[string]any
		wantMetadataDisposition string
		wantOutputDisposition   string
		wantContextDisposition  string
		wantEffort              string
	}{
		{
			name:                    "absent",
			wantMetadataDisposition: ClaudeFieldDispositionAbsent,
			wantOutputDisposition:   ClaudeFieldDispositionAbsent,
			wantContextDisposition:  ClaudeFieldDispositionAbsent,
		},
		{
			name: "empty objects",
			extra: map[string]any{
				"metadata":           map[string]any{},
				"output_config":      map[string]any{},
				"context_management": map[string]any{},
			},
			wantMetadataDisposition: ClaudeFieldDispositionTranslated,
			wantOutputDisposition:   ClaudeFieldDispositionValidatedNoop,
			wantContextDisposition:  ClaudeFieldDispositionValidatedNoop,
		},
		{
			name: "captured shape with supported effort",
			extra: map[string]any{
				"metadata":      map[string]any{"user_id": "customer-session"},
				"output_config": map[string]any{"effort": "high"},
				"context_management": map[string]any{"edits": []any{
					map[string]any{"type": "clear_thinking_20251015", "keep": "all"},
					map[string]any{"type": "clear_thinking_20251015", "keep": map[string]any{"type": "all"}},
				}},
			},
			wantMetadataDisposition: ClaudeFieldDispositionTranslated,
			wantOutputDisposition:   ClaudeFieldDispositionTranslated,
			wantContextDisposition:  ClaudeFieldDispositionValidatedNoop,
			wantEffort:              "high",
		},
		{
			name: "null and empty edits are no effect",
			extra: map[string]any{
				"context_management": nil,
				"output_config":      map[string]any{},
			},
			wantMetadataDisposition: ClaudeFieldDispositionAbsent,
			wantOutputDisposition:   ClaudeFieldDispositionValidatedNoop,
			wantContextDisposition:  ClaudeFieldDispositionValidatedNoop,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, info := newRequestContractFixture(
				t,
				constant.ChannelTypeOpenCodeAPIKey,
				requestPreflightEndpoints[0],
				ProtocolChat,
				test.extra,
			)
			envelope, found, err := helper.GetValidatedRequestEnvelope(c, types.RelayFormatClaude)
			require.NoError(t, err)
			require.True(t, found)
			projection, err := ParseClaudeChatProjection(envelope)
			require.NoError(t, err)
			assert.Equal(t, test.wantMetadataDisposition, projection.MetadataDisposition)
			assert.Equal(t, test.wantOutputDisposition, projection.OutputConfigDisposition)
			assert.Equal(t, test.wantContextDisposition, projection.ContextDisposition)
			assert.Equal(t, test.wantEffort, projection.Effort)

			plan, err := BuildRequestPreflightPlan(c, info)
			require.NoError(t, err)
			assert.Equal(t, ProtocolChat, plan.FinalProtocol)
		})
	}
}

func TestClaudeChatProjectionRejectsUnprovedSemantics(t *testing.T) {
	longUserID := strings.Repeat("x", maxChatMetadataValueRunes+1)
	tests := []struct {
		name     string
		extra    map[string]any
		wantRule string
	}{
		{name: "metadata null", extra: map[string]any{"metadata": nil}, wantRule: ClaudeMetadataShapeRule},
		{name: "metadata user null", extra: map[string]any{"metadata": map[string]any{"user_id": nil}}, wantRule: ClaudeMetadataShapeRule},
		{name: "metadata unknown", extra: map[string]any{"metadata": map[string]any{"tenant": "private"}}, wantRule: ClaudeMetadataShapeRule},
		{name: "metadata too long", extra: map[string]any{"metadata": map[string]any{"user_id": longUserID}}, wantRule: ClaudeMetadataTargetLimitRule},
		{name: "output null", extra: map[string]any{"output_config": nil}, wantRule: ClaudeOutputConfigShapeRule},
		{name: "effort null", extra: map[string]any{"output_config": map[string]any{"effort": nil}}, wantRule: ClaudeOutputConfigNullRule},
		{name: "effort wrong type", extra: map[string]any{"output_config": map[string]any{"effort": true}}, wantRule: ClaudeOutputConfigShapeRule},
		{name: "effort unknown", extra: map[string]any{"output_config": map[string]any{"effort": "ultra"}}, wantRule: ClaudeOutputConfigShapeRule},
		{name: "output unknown member", extra: map[string]any{"output_config": map[string]any{"future": true}}, wantRule: ClaudeOutputConfigShapeRule},
		{name: "format unsupported", extra: map[string]any{"output_config": map[string]any{"format": nil}}, wantRule: ClaudeOutputConfigUnsupportedRule},
		{name: "structured format shape remains unsupported", extra: map[string]any{"output_config": map[string]any{
			"effort": "high",
			"format": map[string]any{
				"type":   "json_schema",
				"schema": map[string]any{"type": "object", "properties": map[string]any{}},
			},
		}}, wantRule: ClaudeOutputConfigUnsupportedRule},
		{name: "task budget unsupported", extra: map[string]any{"output_config": map[string]any{"task_budget": nil}}, wantRule: ClaudeOutputConfigUnsupportedRule},
		{name: "captured task budget shape remains unsupported", extra: map[string]any{"output_config": map[string]any{
			"effort": "high",
			"task_budget": map[string]any{
				"type": "synthetic", "total": 1024,
			},
		}}, wantRule: ClaudeOutputConfigUnsupportedRule},
		{name: "context wrong type", extra: map[string]any{"context_management": []any{}}, wantRule: ClaudeContextManagementShapeRule},
		{name: "context unknown member", extra: map[string]any{"context_management": map[string]any{"future": true}}, wantRule: ClaudeContextManagementShapeRule},
		{name: "context missing keep", extra: map[string]any{"context_management": map[string]any{"edits": []any{map[string]any{"type": "clear_thinking_20251015"}}}}, wantRule: ClaudeContextManagementActiveRule},
		{name: "context active clear", extra: map[string]any{"context_management": map[string]any{"edits": []any{map[string]any{"type": "clear_tool_uses_20250919", "keep": "all"}}}}, wantRule: ClaudeContextManagementActiveRule},
		{name: "context active keep", extra: map[string]any{"context_management": map[string]any{"edits": []any{map[string]any{"type": "clear_thinking_20251015", "keep": map[string]any{"type": "thinking_turns"}}}}}, wantRule: ClaudeContextManagementActiveRule},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("type-%d/%s", channelType, test.name), func(t *testing.T) {
				c, info := newRequestContractFixture(
					t, channelType, requestPreflightEndpoints[0], ProtocolChat, test.extra,
				)
				_, err := BuildRequestPreflightPlan(c, info)
				require.Error(t, err)
				preflightErr, ok := AsRequestPreflightError(err)
				require.True(t, ok)
				assert.Equal(t, http.StatusBadRequest, preflightErr.StatusCode)
				assert.Equal(t, types.ErrorOriginLocalValidation, preflightErr.Origin)
				assert.Equal(t, test.wantRule, preflightErr.RuleID)
				assert.Equal(t, ClaudeFieldContractPreflightStage, preflightErr.StageID)
				assert.NotContains(t, err.Error(), "private")
			})
		}
	}
}

func TestFinalizeClaudeCodeShapeProjectsToChatWithoutSilentLoss(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			c, info := newRequestContractFixture(
				t,
				channelType,
				requestPreflightEndpoints[0],
				ProtocolChat,
				map[string]any{
					"metadata":      map[string]any{"user_id": "customer-session"},
					"output_config": map[string]any{"effort": "high"},
					"context_management": map[string]any{"edits": []any{
						map[string]any{"type": "clear_thinking_20251015", "keep": "all"},
					}},
				},
			)

			result := convertAndFinalizeRequestForPresenceTest(t, c, info)
			object := decodeFinalizerTestObject(t, result)
			assert.JSONEq(t, `{"user_id":"customer-session"}`, string(object["metadata"]))
			assert.JSONEq(t, `"high"`, string(object["reasoning_effort"]))
			_, outputConfigPresent := object["output_config"]
			_, contextPresent := object["context_management"]
			assert.False(t, outputConfigPresent)
			assert.False(t, contextPresent)
		})
	}
}

func TestFinalizeProtectsProjectedAndNativeClaudeFieldsFromOverride(t *testing.T) {
	tests := []struct {
		name          string
		finalProtocol Protocol
		extra         map[string]any
		override      map[string]any
	}{
		{
			name:          "translated effort",
			finalProtocol: ProtocolChat,
			extra:         map[string]any{"output_config": map[string]any{"effort": "high"}},
			override:      map[string]any{"reasoning_effort": "max"},
		},
		{
			name:          "native output config",
			finalProtocol: ProtocolMessages,
			extra:         map[string]any{"output_config": map[string]any{"effort": "high", "format": map[string]any{"type": "json_schema"}}},
			override:      map[string]any{"output_config": map[string]any{"effort": "max"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, info := newRequestContractFixture(
				t, constant.ChannelTypeOpenCodeAPIKey, requestPreflightEndpoints[0], test.finalProtocol, test.extra,
			)
			plan, err := BuildRequestPreflightPlan(c, info)
			require.NoError(t, err)
			require.NoError(t, StoreRequestPreflightPlan(c, plan))
			info.InitChannelMeta(c)
			info.UpstreamModelName = plan.FinalModel
			info.IsModelMapped = plan.ModelMapped
			info.FinalRequestRelayFormat = plan.FinalProtocol.RelayFormat()
			info.ParamOverride = test.override

			_, err = finalizeOutboundRequest(c, info, minimalFinalizerConvertedRequest(test.finalProtocol))
			require.ErrorContains(t, err, "protected client field")
		})
	}
}
