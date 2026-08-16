package common

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	rootcommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCodeChannelTypesSupportStreaming(t *testing.T) {
	assert.True(t, streamSupportedChannels[constant.ChannelTypeOpenCodeGo])
	assert.True(t, streamSupportedChannels[constant.ChannelTypeOpenCodeAPIKey])
}

func TestResolveSelectionGroupUsesConcreteAutoGroupBeforeRequestGroup(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	rootcommon.SetContextKey(c, constant.ContextKeyUsingGroup, "auto")
	rootcommon.SetContextKey(c, constant.ContextKeyTokenGroup, "auto")
	rootcommon.SetContextKey(c, constant.ContextKeyAutoGroup, "group-b")
	info := &RelayInfo{UsingGroup: "auto", TokenGroup: "auto"}

	assert.Equal(t, "group-b", ResolveSelectionGroup(c, info))

	rootcommon.SetContextKey(c, constant.ContextKeyAutoGroup, "")
	info.UsingGroup = "default"
	assert.Equal(t, "default", ResolveSelectionGroup(c, info))
}

func TestOpenCodeStreamReadyForFinalization(t *testing.T) {
	assert.True(t, OpenCodeStreamReadyForFinalization(nil))
	assert.True(t, OpenCodeStreamReadyForFinalization(&RelayInfo{
		ChannelMeta: &ChannelMeta{ChannelType: constant.ChannelTypeOpenAI},
	}))

	for _, channelType := range []int{
		constant.ChannelTypeOpenCodeGo,
		constant.ChannelTypeOpenCodeAPIKey,
	} {
		t.Run(constant.ChannelTypeNames[channelType], func(t *testing.T) {
			chatInfo := &RelayInfo{ChannelMeta: &ChannelMeta{ChannelType: channelType}}
			assert.False(t, OpenCodeStreamReadyForFinalization(chatInfo))
			chatInfo.StreamStatus = NewStreamStatus()
			assert.False(t, OpenCodeStreamReadyForFinalization(chatInfo))
			chatInfo.StreamStatus.MarkDoneSentinel()
			assert.True(t, OpenCodeStreamReadyForFinalization(chatInfo))

			protocolInfo := &RelayInfo{
				ChannelMeta:                    &ChannelMeta{ChannelType: channelType},
				StreamStatus:                   NewStreamStatus(),
				StreamProtocolTerminalRequired: true,
			}
			assert.False(t, OpenCodeStreamReadyForFinalization(protocolInfo))
			protocolInfo.StreamStatus.MarkDoneSentinel()
			assert.False(t, OpenCodeStreamReadyForFinalization(protocolInfo))
			protocolInfo.StreamStatus.MarkProtocolTerminal()
			assert.True(t, OpenCodeStreamReadyForFinalization(protocolInfo))
		})
	}
}

func TestRestoreUnwrittenStreamAttemptRestoresResponseState(t *testing.T) {
	start := time.Now()
	info := &RelayInfo{
		StartTime:                      start,
		FirstResponseTime:              start.Add(time.Second),
		isFirstResponse:                false,
		IsStream:                       true,
		SendResponseCount:              3,
		ReceivedResponseCount:          2,
		StreamStatus:                   NewStreamStatus(),
		StreamProtocolTerminalRequired: true,
		ThinkingContentInfo: ThinkingContentInfo{
			IsFirstThinkingContent: true,
		},
		ClaudeConvertInfo: &ClaudeConvertInfo{Index: 2},
		ResponsesUsageInfo: &ResponsesUsageInfo{BuiltInTools: map[string]*BuildInToolInfo{
			"web_search": {ToolName: "web_search", CallCount: 1},
		}},
	}
	snapshot := info.SnapshotUnwrittenStreamAttempt()

	info.IsStream = false
	info.FirstResponseTime = start.Add(2 * time.Second)
	info.isFirstResponse = false
	info.SendResponseCount = 5
	info.ReceivedResponseCount = 6
	info.StreamStatus = NewStreamStatus()
	info.StreamProtocolTerminalRequired = false
	info.ThinkingContentInfo.HasSentThinkingContent = true
	info.ClaudeConvertInfo.Done = true
	info.ResponsesUsageInfo.BuiltInTools["web_search"].CallCount = 3

	info.RestoreUnwrittenStreamAttempt(snapshot)

	assert.True(t, info.IsStream)
	assert.Equal(t, start.Add(time.Second), info.FirstResponseTime)
	assert.False(t, info.isFirstResponse)
	assert.Equal(t, 3, info.SendResponseCount)
	assert.Equal(t, 2, info.ReceivedResponseCount)
	assert.NotNil(t, info.StreamStatus)
	assert.True(t, info.StreamProtocolTerminalRequired)
	assert.True(t, info.ThinkingContentInfo.IsFirstThinkingContent)
	assert.False(t, info.ThinkingContentInfo.HasSentThinkingContent)
	assert.Equal(t, 2, info.ClaudeConvertInfo.Index)
	assert.False(t, info.ClaudeConvertInfo.Done)
	assert.Equal(t, 1, info.ResponsesUsageInfo.BuiltInTools["web_search"].CallCount)
}

func TestRestoreRelayAttemptRebuildsAllAttemptLocalState(t *testing.T) {
	start := time.Now()
	info := &RelayInfo{
		RelayMode:              2,
		RequestURLPath:         "/v1/chat/completions",
		IsStream:               false,
		StartTime:              start,
		FirstResponseTime:      start.Add(-time.Second),
		isFirstResponse:        true,
		ShouldIncludeUsage:     false,
		ReasoningEffort:        "high",
		RequestConversionChain: []types.RelayFormat{types.RelayFormatOpenAI},
		RuntimeHeadersOverride: map[string]interface{}{
			"x-safe": "baseline",
			"nested": map[string]interface{}{"value": "baseline"},
		},
		UseRuntimeHeadersOverride: true,
		ParamOverrideAudit:        []string{"baseline-audit"},
		ResponsesUsageInfo: &ResponsesUsageInfo{BuiltInTools: map[string]*BuildInToolInfo{
			"web_search": {ToolName: "web_search", CallCount: 0},
		}},
	}
	baseline := info.SnapshotRelayAttempt()

	info.RelayMode = 9
	info.RequestURLPath = "/v1/responses"
	info.IsStream = true
	info.FirstResponseTime = start.Add(time.Second)
	info.isFirstResponse = false
	info.ShouldIncludeUsage = true
	info.DisablePing = true
	info.ReasoningEffort = "low"
	info.ChannelMeta = &ChannelMeta{
		ChannelType:       constant.ChannelTypeOpenCodeAPIKey,
		ChannelId:         99,
		UpstreamModelName: "attempt-model",
		ParamOverride:     map[string]interface{}{"attempt": true},
	}
	info.RequestConversionChain = []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses}
	info.FinalRequestRelayFormat = types.RelayFormatOpenAIResponses
	info.RuntimeHeadersOverride["x-safe"] = "dirty"
	info.RuntimeHeadersOverride["nested"].(map[string]interface{})["value"] = "dirty"
	info.UseRuntimeHeadersOverride = false
	info.ParamOverrideAudit[0] = "dirty-audit"
	info.UpstreamRequestBodySize = 1234
	info.StreamStatus = NewStreamStatus()
	info.StreamStatus.RecordError("attempt error")
	info.ReceivedResponseCount = 7
	info.ResponsesUsageInfo.BuiltInTools["web_search"].CallCount = 4

	info.RestoreRelayAttempt(baseline)

	assert.Equal(t, 2, info.RelayMode)
	assert.Equal(t, "/v1/chat/completions", info.RequestURLPath)
	assert.False(t, info.IsStream)
	assert.Equal(t, start.Add(-time.Second), info.FirstResponseTime)
	assert.True(t, info.isFirstResponse)
	assert.False(t, info.ShouldIncludeUsage)
	assert.False(t, info.DisablePing)
	assert.Equal(t, "high", info.ReasoningEffort)
	assert.Nil(t, info.ChannelMeta)
	assert.Equal(t, []types.RelayFormat{types.RelayFormatOpenAI}, info.RequestConversionChain)
	assert.Empty(t, info.FinalRequestRelayFormat)
	assert.Equal(t, "baseline", info.RuntimeHeadersOverride["x-safe"])
	assert.Equal(t, "baseline", info.RuntimeHeadersOverride["nested"].(map[string]interface{})["value"])
	assert.True(t, info.UseRuntimeHeadersOverride)
	assert.Equal(t, []string{"baseline-audit"}, info.ParamOverrideAudit)
	assert.Zero(t, info.UpstreamRequestBodySize)
	assert.Nil(t, info.StreamStatus)
	assert.Zero(t, info.ReceivedResponseCount)
	assert.Zero(t, info.ResponsesUsageInfo.BuiltInTools["web_search"].CallCount)
}

func TestRelayInfoGetFinalRequestRelayFormatPrefersExplicitFinal(t *testing.T) {
	info := &RelayInfo{
		RelayFormat:             types.RelayFormatOpenAI,
		RequestConversionChain:  []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatClaude},
		FinalRequestRelayFormat: types.RelayFormatOpenAIResponses,
	}

	require.Equal(t, types.RelayFormat(types.RelayFormatOpenAIResponses), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoGetFinalRequestRelayFormatFallsBackToConversionChain(t *testing.T) {
	info := &RelayInfo{
		RelayFormat:            types.RelayFormatOpenAI,
		RequestConversionChain: []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatClaude},
	}

	require.Equal(t, types.RelayFormat(types.RelayFormatClaude), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoGetFinalRequestRelayFormatFallsBackToRelayFormat(t *testing.T) {
	info := &RelayInfo{
		RelayFormat: types.RelayFormatGemini,
	}

	require.Equal(t, types.RelayFormat(types.RelayFormatGemini), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoGetFinalRequestRelayFormatNilReceiver(t *testing.T) {
	var info *RelayInfo
	require.Equal(t, types.RelayFormat(""), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoMetaTypedNilReceiver(t *testing.T) {
	var info *RelayInfo
	var meta convmeta.Meta = info

	assert.Empty(t, meta.GetOriginModelName())
	assert.Empty(t, meta.GetUpstreamModelName())
	assert.False(t, meta.HasChannelMeta())
	assert.Zero(t, meta.GetChannelID())
	assert.Zero(t, meta.GetChannelType())
	assert.False(t, meta.GetIsStream())
	assert.Empty(t, meta.GetReasoningEffort())
	assert.Zero(t, meta.GetEstimatePromptTokens())
	assert.Zero(t, meta.GetSendResponseCount())

	assert.NotPanics(t, func() {
		meta.SetReasoningEffort("high")
		meta.IncrSendResponseCount()
		meta.AppendRequestConversion(types.RelayFormatClaude)
	})

	firstState := meta.EnsureClaudeConvertInfo()
	secondState := meta.EnsureClaudeConvertInfo()
	require.NotNil(t, firstState)
	require.NotNil(t, secondState)
	assert.Equal(t, convmeta.LastMessageTypeNone, firstState.LastMessagesType)
	assert.NotSame(t, firstState, secondState)

	firstOptions := meta.ConvOptions()
	secondOptions := meta.ConvOptions()
	require.NotNil(t, firstOptions)
	require.NotNil(t, secondOptions)
	assert.NotSame(t, firstOptions, secondOptions)
	assert.NotNil(t, firstOptions.Claude.DefaultMaxTokens)
	assert.NotNil(t, firstOptions.Gemini.SupportsImagine)
	assert.NotNil(t, firstOptions.Gemini.SafetySetting)
	assert.NotNil(t, firstOptions.PreserveThinkingSuffix)
}

func TestGenRelayInfoCapturesRequestReasoningEffort(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name        string
		path        string
		relayFormat types.RelayFormat
		request     dto.Request
		expected    string
	}{
		{
			name:        "OpenAI chat top-level effort",
			path:        "/v1/chat/completions",
			relayFormat: types.RelayFormatOpenAI,
			request:     &dto.GeneralOpenAIRequest{Model: "gpt-5.6-sol", ReasoningEffort: " high "},
			expected:    "high",
		},
		{
			name:        "OpenRouter nested chat effort",
			path:        "/v1/chat/completions",
			relayFormat: types.RelayFormatOpenAI,
			request:     &dto.GeneralOpenAIRequest{Model: "anthropic/claude", Reasoning: json.RawMessage(`{"effort":"xhigh"}`)},
			expected:    "xhigh",
		},
		{
			name:        "OpenAI Responses effort",
			path:        "/v1/responses",
			relayFormat: types.RelayFormatOpenAIResponses,
			request:     &dto.OpenAIResponsesRequest{Model: "gpt-5.6-sol", Reasoning: &dto.Reasoning{Effort: "max"}},
			expected:    "max",
		},
		{
			name:        "explicit none is preserved",
			path:        "/v1/responses",
			relayFormat: types.RelayFormatOpenAIResponses,
			request:     &dto.OpenAIResponsesRequest{Model: "gpt-5.6-sol", Reasoning: &dto.Reasoning{Effort: "none"}},
			expected:    "none",
		},
		{
			name:        "non-string nested effort is ignored",
			path:        "/v1/chat/completions",
			relayFormat: types.RelayFormatOpenAI,
			request:     &dto.GeneralOpenAIRequest{Model: "anthropic/claude", Reasoning: json.RawMessage(`{"effort":42}`)},
			expected:    "",
		},
		{
			name:        "Claude output config effort",
			path:        "/v1/messages",
			relayFormat: types.RelayFormatClaude,
			request:     &dto.ClaudeRequest{Model: "claude-opus-4-7", OutputConfig: json.RawMessage(`{"effort":"medium"}`)},
			expected:    "medium",
		},
		{
			name:        "Gemini thinking level",
			path:        "/v1beta/models/gemini-3-pro:generateContent",
			relayFormat: types.RelayFormatGemini,
			request: &dto.GeminiChatRequest{GenerationConfig: dto.GeminiChatGenerationConfig{
				ThinkingConfig: &dto.GeminiThinkingConfig{ThinkingLevel: "low"},
			}},
			expected: "low",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest("POST", tt.path, nil)

			info, err := GenRelayInfo(ctx, tt.relayFormat, tt.request, nil)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, info.ReasoningEffort)
		})
	}
}

func TestInitChannelMetaRestoresRequestReasoningEffortForRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	request := &dto.OpenAIResponsesRequest{
		Model:     "gpt-5.6-sol",
		Reasoning: &dto.Reasoning{Effort: "max"},
	}
	info, err := GenRelayInfo(ctx, types.RelayFormatOpenAIResponses, request, nil)
	require.NoError(t, err)

	info.SetReasoningEffort("high")
	info.InitChannelMeta(ctx)
	assert.Equal(t, "max", info.ReasoningEffort)

	info.SetReasoningEffort("low")
	info.InitChannelMeta(ctx)
	assert.Equal(t, "max", info.ReasoningEffort)
}
