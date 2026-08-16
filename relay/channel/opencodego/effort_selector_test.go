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

func TestParseEnvelopeEffortSelectionRecognizesEveryOpenCodeAlias(t *testing.T) {
	tests := []struct {
		name     string
		endpoint requestPreflightEndpoint
		extra    map[string]any
		wantPath []string
	}{
		{name: "chat camel", endpoint: requestPreflightEndpoints[1], extra: map[string]any{"reasoningEffort": "high"}, wantPath: []string{"reasoningEffort"}},
		{name: "chat snake", endpoint: requestPreflightEndpoints[1], extra: map[string]any{"reasoning_effort": "high"}, wantPath: []string{"reasoning_effort"}},
		{name: "chat nested", endpoint: requestPreflightEndpoints[1], extra: map[string]any{"reasoning": map[string]any{"effort": "high"}}, wantPath: []string{"reasoning", "effort"}},
		{name: "responses camel", endpoint: requestPreflightEndpoints[2], extra: map[string]any{"reasoningEffort": "high"}, wantPath: []string{"reasoningEffort"}},
		{name: "responses snake", endpoint: requestPreflightEndpoints[2], extra: map[string]any{"reasoning_effort": "high"}, wantPath: []string{"reasoning_effort"}},
		{name: "responses nested", endpoint: requestPreflightEndpoints[2], extra: map[string]any{"reasoning": map[string]any{"effort": "high"}}, wantPath: []string{"reasoning", "effort"}},
		{name: "messages top", endpoint: requestPreflightEndpoints[0], extra: map[string]any{"effort": "high"}, wantPath: []string{"effort"}},
		{name: "messages snake", endpoint: requestPreflightEndpoints[0], extra: map[string]any{"output_config": map[string]any{"effort": "high"}}, wantPath: []string{"output_config", "effort"}},
		{name: "messages camel", endpoint: requestPreflightEndpoints[0], extra: map[string]any{"outputConfig": map[string]any{"effort": "high"}}, wantPath: []string{"outputConfig", "effort"}},
		{name: "messages thinking", endpoint: requestPreflightEndpoints[0], extra: map[string]any{"thinking": map[string]any{"effort": "high"}}, wantPath: []string{"thinking", "effort"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			finalProtocol, err := protocolForRelayFormat(test.endpoint.format)
			require.NoError(t, err)
			c, info := newRequestContractFixture(
				t, constant.ChannelTypeOpenCodeAPIKey, test.endpoint, finalProtocol, test.extra,
			)
			envelope, found, err := helper.GetValidatedRequestEnvelope(c, test.endpoint.format)
			require.NoError(t, err)
			require.True(t, found)
			selection, err := ParseEnvelopeEffortSelection(envelope, test.endpoint.format)
			require.NoError(t, err)
			assert.True(t, selection.Present)
			assert.False(t, selection.Null)
			assert.Equal(t, "high", selection.Value)
			assert.Equal(t, test.wantPath, selection.Path)
			assert.Equal(t, EffortSelectorOriginClientDirect, selection.Origin)
			_, err = BuildRequestPreflightPlan(c, info)
			require.NoError(t, err)
		})
	}
}

func TestEnvelopeEffortSelectionRejectsCollisionShapeAndCrossProtocolNull(t *testing.T) {
	tests := []struct {
		name          string
		endpoint      requestPreflightEndpoint
		finalProtocol Protocol
		extra         map[string]any
		wantRule      string
	}{
		{
			name: "chat equal collision", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolChat,
			extra: map[string]any{"reasoningEffort": "high", "reasoning_effort": "high"}, wantRule: EffortSelectorCollisionRule,
		},
		{
			name: "responses null collision", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolResponses,
			extra: map[string]any{"reasoning_effort": nil, "reasoning": map[string]any{"effort": nil}}, wantRule: EffortSelectorCollisionRule,
		},
		{
			name: "messages collision", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolMessages,
			extra: map[string]any{"effort": "high", "output_config": map[string]any{"effort": "max"}}, wantRule: EffortSelectorCollisionRule,
		},
		{
			name: "wrong type", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolChat,
			extra: map[string]any{"reasoningEffort": true}, wantRule: EffortSelectorShapeRule,
		},
		{
			name: "empty string", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolResponses,
			extra: map[string]any{"reasoning_effort": ""}, wantRule: EffortSelectorShapeRule,
		},
		{
			name: "over limit", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolMessages,
			extra: map[string]any{"effort": strings.Repeat("x", maxEffortSelectorRunes+1)}, wantRule: EffortSelectorShapeRule,
		},
		{
			name: "chat cross null", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolMessages,
			extra: map[string]any{"reasoning_effort": nil}, wantRule: EffortSelectorCrossNullRule,
		},
		{
			name: "messages cross null", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolChat,
			extra: map[string]any{"effort": nil}, wantRule: EffortSelectorCrossNullRule,
		},
		{
			name: "camel wrapper member", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolChat,
			extra: map[string]any{"outputConfig": map[string]any{"future": true}}, wantRule: EffortSelectorShapeRule,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, info := newRequestContractFixture(
				t, constant.ChannelTypeOpenCodeAPIKey, test.endpoint, test.finalProtocol, test.extra,
			)
			_, err := BuildRequestPreflightPlan(c, info)
			require.Error(t, err)
			preflightErr, ok := AsRequestPreflightError(err)
			require.True(t, ok)
			assert.Equal(t, http.StatusBadRequest, preflightErr.StatusCode)
			assert.Equal(t, types.ErrorOriginLocalValidation, preflightErr.Origin)
			assert.Equal(t, test.wantRule, preflightErr.RuleID)
			assert.Equal(t, EffortSelectorPreflightStage, preflightErr.StageID)
		})
	}
}

func TestNativeExplicitNullEffortAliasesRemainPreservedNoSelection(t *testing.T) {
	tests := []struct {
		name     string
		endpoint requestPreflightEndpoint
		extra    map[string]any
	}{
		{name: "chat", endpoint: requestPreflightEndpoints[1], extra: map[string]any{"reasoningEffort": nil}},
		{name: "responses", endpoint: requestPreflightEndpoints[2], extra: map[string]any{"reasoning": map[string]any{"effort": nil}}},
		{name: "messages", endpoint: requestPreflightEndpoints[0], extra: map[string]any{"outputConfig": map[string]any{"effort": nil}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			finalProtocol, err := protocolForRelayFormat(test.endpoint.format)
			require.NoError(t, err)
			c, info := newRequestContractFixture(
				t, constant.ChannelTypeOpenCodeAPIKey, test.endpoint, finalProtocol, test.extra,
			)
			result := convertAndFinalizeRequestForPresenceTest(t, c, info)
			selection, err := parseFinalEffortSelection(result, finalProtocol)
			require.NoError(t, err)
			assert.True(t, selection.Present)
			assert.True(t, selection.Null)
			assert.Empty(t, selection.Value)
		})
	}
}

func TestCrossProtocolEffortAliasesProjectOnlyToTargetCanonicalPath(t *testing.T) {
	tests := []struct {
		name          string
		endpoint      requestPreflightEndpoint
		finalProtocol Protocol
		extra         map[string]any
	}{
		{name: "messages top to chat", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolChat, extra: map[string]any{"effort": "high"}},
		{name: "messages camel to chat", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolChat, extra: map[string]any{"outputConfig": map[string]any{"effort": "high"}}},
		{name: "messages thinking to chat", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolChat, extra: map[string]any{"thinking": map[string]any{"effort": "high"}}},
		{name: "chat camel to messages", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolMessages, extra: map[string]any{"reasoningEffort": "high"}},
		{name: "chat nested to responses", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolResponses, extra: map[string]any{"reasoning": map[string]any{"effort": "high"}}},
		{name: "responses snake to chat", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat, extra: map[string]any{"reasoning_effort": "high"}},
		{name: "responses camel to messages", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolMessages, extra: map[string]any{"reasoningEffort": "high"}},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("type-%d/%s", channelType, test.name), func(t *testing.T) {
				c, info := newRequestContractFixture(t, channelType, test.endpoint, test.finalProtocol, test.extra)
				result := convertAndFinalizeRequestForPresenceTest(t, c, info)
				selection, err := parseFinalEffortSelection(result, test.finalProtocol)
				require.NoError(t, err)
				assert.True(t, selection.Present)
				assert.False(t, selection.Null)
				assert.Equal(t, "high", selection.Value)
				assert.Equal(t, canonicalEffortSelectorPath(test.finalProtocol), selection.Path)
				if test.finalProtocol == ProtocolMessages {
					_, thinkingPresent, pathErr := rawJSONPath(result, []string{"thinking"})
					require.NoError(t, pathErr)
					assert.False(t, thinkingPresent, "approximate thinking budget survived exact effort translation")
				}
			})
		}
	}
}

func TestEmptyMessagesCamelOutputConfigIsValidatedNoEffect(t *testing.T) {
	c, info := newRequestContractFixture(
		t,
		constant.ChannelTypeOpenCodeAPIKey,
		requestPreflightEndpoints[0],
		ProtocolChat,
		map[string]any{"outputConfig": map[string]any{}},
	)
	result := convertAndFinalizeRequestForPresenceTest(t, c, info)
	selection, err := parseFinalEffortSelection(result, ProtocolChat)
	require.NoError(t, err)
	assert.False(t, selection.Present)
	_, present, err := rawJSONPath(result, []string{"outputConfig"})
	require.NoError(t, err)
	assert.False(t, present)
}

func TestFinalEffortSelectionAttributesOperatorOverridesWithoutExposingValues(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		override map[string]interface{}
		wantText string
	}{
		{
			name:     "protected client selector mutation",
			body:     `{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}],"reasoningEffort":"high"}`,
			override: map[string]interface{}{"reasoningEffort": "max"},
			wantText: "operator override changed a protected client field",
		},
		{
			name:     "operator alias collision",
			body:     `{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}],"reasoningEffort":"high"}`,
			override: map[string]interface{}{"reasoning_effort": "high"},
			wantText: EffortSelectorFinalizedMessage,
		},
		{
			name:     "operator malformed selector",
			body:     `{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}`,
			override: map[string]interface{}{"reasoningEffort": true},
			wantText: EffortSelectorFinalizedMessage,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, info, request := newSameProtocolFinalizerFixture(
				t, types.RelayFormatOpenAI, "/v1/chat/completions", []byte(test.body),
			)
			info.ParamOverride = test.override
			_, err := finalizeOutboundRequest(c, info, request)
			require.EqualError(t, err, test.wantText)
			_, isClientError := helper.AsClientRequestValidationError(err)
			assert.False(t, isClientError)
			assert.NotContains(t, err.Error(), "high")
			assert.NotContains(t, err.Error(), "max")
		})
	}
}

func TestClassifyFinalEffortSelectionTracksClientAndOperatorOrigin(t *testing.T) {
	wire := EffortSelection{
		Path:    []string{"reasoning_effort"},
		Present: true,
		Value:   "high",
		Origin:  EffortSelectorOriginClientTranslated,
	}
	selection, err := classifyFinalEffortSelection(
		[]byte(`{"reasoning_effort":"high"}`), ProtocolChat, wire,
	)
	require.NoError(t, err)
	assert.Equal(t, EffortSelectorOriginClientTranslated, selection.Origin)

	selection, err = classifyFinalEffortSelection(
		[]byte(`{"reasoningEffort":"high"}`), ProtocolChat, EffortSelection{},
	)
	require.NoError(t, err)
	assert.Equal(t, EffortSelectorOriginOperatorOverride, selection.Origin)

	_, err = classifyFinalEffortSelection(
		[]byte(`{"reasoning_effort":"max"}`), ProtocolChat, wire,
	)
	require.EqualError(t, err, "finalized OpenCode effort selector changed after projection")
}
