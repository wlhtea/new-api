package opencodego

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAdaptorTestContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c
}

func newAdaptorTestInfo(model string, stream bool) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		OriginModelName: "public-model",
		IsStream:        stream,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenCodeGo,
			ApiType:           constant.APITypeOpenCodeGo,
			ApiKey:            "test-account-key",
			ChannelBaseUrl:    "https://untrusted.invalid/custom",
			UpstreamModelName: model,
		},
	}
}

func requestForFormat(format types.RelayFormat) any {
	stream := true
	switch format {
	case types.RelayFormatOpenAI:
		return &dto.GeneralOpenAIRequest{
			Model:    "public-model",
			Messages: []dto.Message{{Role: "user", Content: "hello"}},
			Stream:   &stream,
		}
	case types.RelayFormatClaude:
		maxTokens := uint(32)
		return &dto.ClaudeRequest{
			Model:     "public-model",
			Messages:  []dto.ClaudeMessage{{Role: "user", Content: "hello"}},
			MaxTokens: &maxTokens,
			Stream:    &stream,
		}
	case types.RelayFormatOpenAIResponses:
		return &dto.OpenAIResponsesRequest{
			Model:  "public-model",
			Input:  json.RawMessage(`"hello"`),
			Stream: &stream,
		}
	default:
		panic("unsupported test format")
	}
}

func convertAdaptorRequest(a *Adaptor, c *gin.Context, info *relaycommon.RelayInfo, format types.RelayFormat, request any) (any, error) {
	switch format {
	case types.RelayFormatOpenAI:
		return a.ConvertOpenAIRequest(c, info, request.(*dto.GeneralOpenAIRequest))
	case types.RelayFormatClaude:
		return a.ConvertClaudeRequest(c, info, request.(*dto.ClaudeRequest))
	case types.RelayFormatOpenAIResponses:
		return a.ConvertOpenAIResponsesRequest(c, info, *request.(*dto.OpenAIResponsesRequest))
	default:
		panic("unsupported test format")
	}
}

func TestAdaptorRequestConversionMatrix(t *testing.T) {
	targets := []struct {
		model    string
		protocol Protocol
		wantType any
	}{
		{model: "glm-5.2", protocol: ProtocolChat, wantType: &dto.GeneralOpenAIRequest{}},
		{model: "minimax-m3", protocol: ProtocolMessages, wantType: &dto.ClaudeRequest{}},
		{model: "gpt-5.6-luna", protocol: ProtocolResponses, wantType: &dto.OpenAIResponsesRequest{}},
	}
	clients := []types.RelayFormat{
		types.RelayFormatOpenAI,
		types.RelayFormatClaude,
		types.RelayFormatOpenAIResponses,
	}

	for _, target := range targets {
		for _, client := range clients {
			name := string(client) + "_to_" + string(target.protocol)
			t.Run(name, func(t *testing.T) {
				info := newAdaptorTestInfo(target.model, true)
				adaptor := &Adaptor{}
				adaptor.Init(info)

				converted, err := convertAdaptorRequest(adaptor, newAdaptorTestContext(), info, client, requestForFormat(client))
				require.NoError(t, err)
				assert.Equal(t, reflect.TypeOf(target.wantType), reflect.TypeOf(converted))
				assert.Equal(t, target.protocol.RelayFormat(), info.FinalRequestRelayFormat)

				switch value := converted.(type) {
				case *dto.GeneralOpenAIRequest:
					assert.Equal(t, target.model, value.Model)
					require.NotNil(t, value.StreamOptions)
					assert.True(t, value.StreamOptions.IncludeUsage)
				case *dto.ClaudeRequest:
					assert.Equal(t, target.model, value.Model)
				case *dto.OpenAIResponsesRequest:
					assert.Equal(t, target.model, value.Model)
					var cacheKey string
					require.NoError(t, json.Unmarshal(value.PromptCacheKey, &cacheKey))
					assert.Equal(t, adaptor.cacheIdentity, cacheKey)
				}
			})
		}
	}
}

func TestAdaptorUsesFixedURLsAndProtocolAuthentication(t *testing.T) {
	originalSelector := selectOpenCodeGoWorkspace
	selectOpenCodeGoWorkspace = func(_ int, _ string, _ service.OpenCodeGoPoolSelectOptions) (*service.OpenCodeGoPoolSelection, error) {
		return &service.OpenCodeGoPoolSelection{
			WorkspaceID:  1,
			WorkspaceUID: "workspace-test",
			APIKey:       "test-account-key",
		}, nil
	}
	t.Cleanup(func() { selectOpenCodeGoWorkspace = originalSelector })

	tests := []struct {
		model       string
		path        string
		wantBearer  bool
		wantAPIKey  bool
		wantVersion bool
	}{
		{model: "glm-5.2", path: "/chat/completions", wantBearer: true},
		{model: "minimax-m3", path: "/messages", wantAPIKey: true, wantVersion: true},
		{model: "gpt-5.6-luna", path: "/responses", wantBearer: true},
	}

	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			info := newAdaptorTestInfo(test.model, false)
			adaptor := &Adaptor{}
			adaptor.Init(info)
			_, err := adaptor.ConvertOpenAIRequest(newAdaptorTestContext(), info, requestForFormat(types.RelayFormatOpenAI).(*dto.GeneralOpenAIRequest))
			require.NoError(t, err)

			requestURL, err := adaptor.GetRequestURL(info)
			require.NoError(t, err)
			assert.Equal(t, constant.ChannelBaseURLs[constant.ChannelTypeOpenCodeGo]+test.path, requestURL)
			assert.NotContains(t, requestURL, "untrusted.invalid")

			header := http.Header{}
			require.NoError(t, adaptor.SetupRequestHeader(newAdaptorTestContext(), &header, info))
			assert.Equal(t, test.wantBearer, header.Get("Authorization") == "Bearer test-account-key")
			assert.Equal(t, test.wantAPIKey, header.Get("x-api-key") == "test-account-key")
			assert.Equal(t, test.wantVersion, header.Get("anthropic-version") == "2023-06-01")
			assert.Equal(t, adaptor.cacheIdentity, header.Get(cacheIdentityHeader))
		})
	}
}

func TestAdaptorPassesFailoverPolicyAndPinsStatefulResponses(t *testing.T) {
	originalSelector := selectOpenCodeGoWorkspace
	var captured service.OpenCodeGoPoolSelectOptions
	selectOpenCodeGoWorkspace = func(_ int, _ string, options service.OpenCodeGoPoolSelectOptions) (*service.OpenCodeGoPoolSelection, error) {
		captured = options
		return &service.OpenCodeGoPoolSelection{
			WorkspaceID:  1,
			WorkspaceUID: "workspace-test",
			APIKey:       "test-account-key",
		}, nil
	}
	t.Cleanup(func() { selectOpenCodeGoWorkspace = originalSelector })

	info := newAdaptorTestInfo("gpt-5.6-luna", false)
	info.RelayFormat = types.RelayFormatOpenAIResponses
	info.ChannelOtherSettings.OpenCodeGo = &dto.OpenCodeGoConfig{
		GenericFailoverEnabled:       true,
		GenericFailoverThreshold:     3,
		GenericFailoverWindowSeconds: 45,
		GenericFailoverMaxBackups:    1,
		GenericFailoverLeaseSeconds:  600,
	}
	request := requestForFormat(types.RelayFormatOpenAIResponses).(*dto.OpenAIResponsesRequest)
	request.PreviousResponseID = "resp_server_state"
	request.PromptCacheKey = json.RawMessage(`"stable-affinity"`)
	adaptor := &Adaptor{}
	adaptor.Init(info)
	_, err := adaptor.ConvertOpenAIResponsesRequest(newAdaptorTestContext(), info, *request)
	require.NoError(t, err)
	require.NoError(t, adaptor.SetupRequestHeader(newAdaptorTestContext(), &http.Header{}, info))

	assert.Equal(t, "stable-affinity", captured.AffinityKey)
	assert.Equal(t, string(ProtocolResponses), captured.Protocol)
	assert.True(t, captured.Stateful)
	assert.True(t, captured.Failover.Enabled)
	assert.Equal(t, 3, captured.Failover.FailureThreshold)
	assert.Equal(t, 45*time.Second, captured.Failover.FailureWindow)
	assert.Equal(t, 10*time.Minute, captured.Failover.LeaseDuration)
}

func TestAdaptorCacheIdentityRemainsStableAcrossPromotedWorkspace(t *testing.T) {
	originalSelector := selectOpenCodeGoWorkspace
	selectionCalls := 0
	selectOpenCodeGoWorkspace = func(_ int, _ string, _ service.OpenCodeGoPoolSelectOptions) (*service.OpenCodeGoPoolSelection, error) {
		selectionCalls++
		selection := &service.OpenCodeGoPoolSelection{
			WorkspaceID:           int64(selectionCalls),
			WorkspaceUID:          "workspace-primary",
			CanonicalWorkspaceUID: "workspace-primary",
			APIKey:                "test-account-key",
		}
		if selectionCalls == 2 {
			selection.WorkspaceUID = "workspace-backup"
			selection.CandidateRank = 1
			selection.FailoverActive = true
			selection.FailoverLeaseExpiresAt = time.Now().Add(30 * time.Minute)
		}
		return selection, nil
	}
	t.Cleanup(func() { selectOpenCodeGoWorkspace = originalSelector })

	requestOnce := func() (string, string) {
		info := newAdaptorTestInfo("gpt-5.6-luna", false)
		info.ChannelId = 42
		info.RelayFormat = types.RelayFormatOpenAIResponses
		info.ChannelOtherSettings.OpenCodeGo = &dto.OpenCodeGoConfig{GenericFailoverEnabled: true}
		request := requestForFormat(types.RelayFormatOpenAIResponses).(*dto.OpenAIResponsesRequest)
		c := newAdaptorTestContext()
		c.Request.Header.Set(claudeCodeSessionHeader, "stable-promotion-session")
		adaptor := &Adaptor{}
		adaptor.Init(info)
		_, err := adaptor.ConvertOpenAIResponsesRequest(c, info, *request)
		require.NoError(t, err)
		header := http.Header{}
		require.NoError(t, adaptor.SetupRequestHeader(c, &header, info))
		return header.Get(cacheIdentityHeader), adaptor.selectedWorkspaceUID
	}

	primaryCacheIdentity, primaryWorkspace := requestOnce()
	backupCacheIdentity, backupWorkspace := requestOnce()
	assert.Equal(t, "workspace-primary", primaryWorkspace)
	assert.Equal(t, "workspace-backup", backupWorkspace)
	assert.NotEmpty(t, primaryCacheIdentity)
	assert.Equal(t, primaryCacheIdentity, backupCacheIdentity)
	assert.Equal(t, 2, selectionCalls)
}

func TestRequestUsesStatefulResponsesFields(t *testing.T) {
	tests := []struct {
		name    string
		request dto.OpenAIResponsesRequest
		want    bool
	}{
		{name: "stateless", request: dto.OpenAIResponsesRequest{}, want: false},
		{name: "previous response", request: dto.OpenAIResponsesRequest{PreviousResponseID: "resp_1"}, want: true},
		{name: "conversation string", request: dto.OpenAIResponsesRequest{Conversation: json.RawMessage(`"conv_1"`)}, want: true},
		{name: "conversation object", request: dto.OpenAIResponsesRequest{Conversation: json.RawMessage(`{}`)}, want: true},
		{name: "context management", request: dto.OpenAIResponsesRequest{ContextManagement: json.RawMessage(`[]`)}, want: true},
		{name: "explicit null", request: dto.OpenAIResponsesRequest{Conversation: json.RawMessage(`null`)}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, requestUsesStatefulResponses(&test.request))
		})
	}
}

func TestAdaptorStreamCompletenessRequiresProtocolTerminalAndExcludesCancellation(t *testing.T) {
	adaptor := &Adaptor{
		requestUpstreamStream: true,
		failoverAttempt:       &service.OpenCodeGoFailoverAttempt{},
	}
	info := newAdaptorTestInfo("gpt-5.6-luna", true)
	info.StreamStatus = relaycommon.NewStreamStatus()
	c := newAdaptorTestContext()
	assert.True(t, adaptor.openCodeGoStreamIncomplete(c, info))

	info.StreamStatus.MarkProtocolTerminal()
	assert.False(t, adaptor.openCodeGoStreamIncomplete(c, info))

	info.StreamStatus = relaycommon.NewStreamStatus()
	requestContext, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(requestContext)
	cancel()
	assert.False(t, adaptor.openCodeGoStreamIncomplete(c, info))
}

func TestAdaptorCancellationSkipsFailoverFailureAndSuccessObservations(t *testing.T) {
	originalFailureObserver := observeOpenCodeGoFailoverFailure
	originalSuccessObserver := observeOpenCodeGoFailoverSuccess
	t.Cleanup(func() {
		observeOpenCodeGoFailoverFailure = originalFailureObserver
		observeOpenCodeGoFailoverSuccess = originalSuccessObserver
	})

	failureCalls := 0
	successCalls := 0
	observeOpenCodeGoFailoverFailure = func(_ *service.OpenCodeGoFailoverAttempt, _ time.Time) (service.OpenCodeGoFailoverObservation, error) {
		failureCalls++
		return service.OpenCodeGoFailoverObservation{}, nil
	}
	observeOpenCodeGoFailoverSuccess = func(_ *service.OpenCodeGoFailoverAttempt, _ time.Time) (service.OpenCodeGoFailoverObservation, error) {
		successCalls++
		return service.OpenCodeGoFailoverObservation{}, nil
	}

	c := newAdaptorTestContext()
	requestContext, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(requestContext)
	cancel()
	adaptor := &Adaptor{failoverAttempt: &service.OpenCodeGoFailoverAttempt{}}
	info := newAdaptorTestInfo("gpt-5.6-luna", true)

	adaptor.recordFailoverFailure(c, info, "upstream_stream_incomplete")
	adaptor.recordFailoverSuccess(c, info)

	assert.Zero(t, failureCalls)
	assert.Zero(t, successCalls)
}

func TestAdaptorLocalStreamEndReasonsDoNotBecomeFailoverEvidence(t *testing.T) {
	for _, reason := range []relaycommon.StreamEndReason{
		relaycommon.StreamEndReasonHandlerStop,
		relaycommon.StreamEndReasonPanic,
		relaycommon.StreamEndReasonPingFail,
	} {
		t.Run(string(reason), func(t *testing.T) {
			adaptor := &Adaptor{
				protocol:              ProtocolResponses,
				requestUpstreamStream: true,
				failoverAttempt:       &service.OpenCodeGoFailoverAttempt{},
			}
			info := newAdaptorTestInfo("gpt-5.6-luna", true)
			info.StreamStatus = relaycommon.NewStreamStatus()
			info.StreamStatus.SetEndReason(reason, errors.New("local stream failure"))

			assert.Empty(t, adaptor.openCodeGoStreamFailureReason(newAdaptorTestContext(), info, nil))
			assert.True(t, adaptor.openCodeGoStreamHasLocalErrors(info))
		})
	}
}

func TestAdaptorPostTerminalLocalFailureDoesNotRecordSuccess(t *testing.T) {
	adaptor := &Adaptor{
		protocol:              ProtocolChat,
		requestUpstreamStream: true,
		failoverAttempt:       &service.OpenCodeGoFailoverAttempt{},
	}
	info := newAdaptorTestInfo("glm-5.2", true)
	info.StreamStatus = relaycommon.NewStreamStatus()
	info.StreamStatus.MarkDoneSentinel()
	info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
	info.StreamStatus.MarkLocalFailure()

	assert.True(t, adaptor.openCodeGoStreamHasLocalErrors(info))
	assert.Empty(t, adaptor.openCodeGoStreamFailureReason(newAdaptorTestContext(), info, nil))
}

func TestAdaptorEmptyUpstreamStreamErrorBecomesFailoverEvidence(t *testing.T) {
	adaptor := &Adaptor{
		protocol:              ProtocolChat,
		requestUpstreamStream: true,
		failoverAttempt:       &service.OpenCodeGoFailoverAttempt{},
	}
	responseErr := types.NewOpenAIError(errors.New("empty upstream stream"), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)

	for _, reason := range []relaycommon.StreamEndReason{
		relaycommon.StreamEndReasonEOF,
		relaycommon.StreamEndReasonTimeout,
		relaycommon.StreamEndReasonScannerErr,
	} {
		t.Run(string(reason), func(t *testing.T) {
			info := newAdaptorTestInfo("glm-5.2", true)
			info.StreamStatus = relaycommon.NewStreamStatus()
			info.StreamStatus.SetEndReason(reason, nil)

			assert.Equal(t, "upstream_stream_incomplete", adaptor.openCodeGoStreamFailureReason(newAdaptorTestContext(), info, responseErr))
		})
	}
}

func TestAdaptorResponseErrorWithPayloadOrLocalFailureDoesNotInferIncompleteStream(t *testing.T) {
	adaptor := &Adaptor{
		protocol:              ProtocolChat,
		requestUpstreamStream: true,
		failoverAttempt:       &service.OpenCodeGoFailoverAttempt{},
	}
	responseErr := types.NewOpenAIError(errors.New("relay response error"), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)

	info := newAdaptorTestInfo("glm-5.2", true)
	info.StreamStatus = relaycommon.NewStreamStatus()
	info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonEOF, nil)
	info.ReceivedResponseCount = 1
	assert.Empty(t, adaptor.openCodeGoStreamFailureReason(newAdaptorTestContext(), info, responseErr))

	info.ReceivedResponseCount = 0
	info.StreamStatus.MarkLocalFailure()
	assert.Empty(t, adaptor.openCodeGoStreamFailureReason(newAdaptorTestContext(), info, responseErr))
}

func TestAdaptorKeepsClaudeChatFamilyFunctionToolsOnChat(t *testing.T) {
	info := newAdaptorTestInfo("glm-5.2", false)
	info.RelayFormat = types.RelayFormatClaude
	request := requestForFormat(types.RelayFormatClaude).(*dto.ClaudeRequest)
	request.Tools = []map[string]any{
		{
			"name":        "Bash",
			"description": "Run a command",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
	adaptor := &Adaptor{}
	adaptor.Init(info)

	converted, err := adaptor.ConvertClaudeRequest(newAdaptorTestContext(), info, request)
	require.NoError(t, err)
	require.IsType(t, &dto.GeneralOpenAIRequest{}, converted)
	assert.Equal(t, ProtocolChat, adaptor.protocol)
	assert.Equal(t, types.RelayFormat(types.RelayFormatOpenAI), info.FinalRequestRelayFormat)

	requestURL, err := adaptor.GetRequestURL(info)
	require.NoError(t, err)
	assert.Equal(t, constant.ChannelBaseURLs[constant.ChannelTypeOpenCodeGo]+"/chat/completions", requestURL)
}

func TestAdaptorRoutesOpenAIChatFamilyFunctionToolsThroughResponses(t *testing.T) {
	info := newAdaptorTestInfo("glm-5.2", false)
	request := requestForFormat(types.RelayFormatOpenAI).(*dto.GeneralOpenAIRequest)
	request.Tools = []dto.ToolCallRequest{
		{
			Type: "function",
			Function: dto.FunctionRequest{
				Name: "Bash",
			},
		},
	}
	adaptor := &Adaptor{}
	adaptor.Init(info)

	converted, err := adaptor.ConvertOpenAIRequest(newAdaptorTestContext(), info, request)
	require.NoError(t, err)
	require.IsType(t, &dto.OpenAIResponsesRequest{}, converted)
	assert.Equal(t, ProtocolResponses, adaptor.protocol)
	assert.Equal(t, types.RelayFormat(types.RelayFormatOpenAIResponses), info.FinalRequestRelayFormat)
}

func TestAdaptorAddsOpenCodeGoResponsesFunctionCallIDFromClaude(t *testing.T) {
	info := newAdaptorTestInfo("gpt-5.6-luna", false)
	request := requestForFormat(types.RelayFormatClaude).(*dto.ClaudeRequest)
	request.Tools = []map[string]any{
		{"name": "Bash", "input_schema": map[string]any{"type": "object"}},
	}
	request.Messages = []dto.ClaudeMessage{
		{Role: "user", Content: "list files"},
		{Role: "assistant", Content: []any{
			map[string]any{
				"type": "tool_use", "id": "toolu_claude", "name": "Bash", "input": map[string]any{},
			},
		}},
		{Role: "user", Content: []any{map[string]any{
			"type": "tool_result", "tool_use_id": "toolu_claude", "content": "OK",
		}}},
	}
	adaptor := &Adaptor{}
	adaptor.Init(info)

	converted, err := adaptor.ConvertClaudeRequest(newAdaptorTestContext(), info, request)
	require.NoError(t, err)
	responses := converted.(*dto.OpenAIResponsesRequest)
	var input []map[string]any
	require.NoError(t, json.Unmarshal(responses.Input, &input))

	var functionCall map[string]any
	for _, item := range input {
		if item["type"] == "function_call" {
			functionCall = item
		}
	}
	require.NotNil(t, functionCall)
	assert.Equal(t, "toolu_claude", functionCall["call_id"])
	assert.Equal(t, functionCall["call_id"], functionCall["id"])
}

func TestAdaptorBuffersStreamingClaudeFunctionToolsUpstream(t *testing.T) {
	info := newAdaptorTestInfo("glm-5.2", true)
	info.RelayFormat = types.RelayFormatClaude
	request := requestForFormat(types.RelayFormatClaude).(*dto.ClaudeRequest)
	request.Tools = []map[string]any{
		{
			"name": "Bash",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string"},
				},
				"required": []string{"command"},
			},
		},
	}
	adaptor := &Adaptor{}
	adaptor.Init(info)

	converted, err := adaptor.ConvertClaudeRequest(newAdaptorTestContext(), info, request)
	require.NoError(t, err)
	chat := converted.(*dto.GeneralOpenAIRequest)
	require.NotNil(t, chat.Stream)
	assert.False(t, *chat.Stream)
	assert.True(t, adaptor.bufferClaudeToolCall)
	assert.False(t, adaptor.requestUpstreamStream)
	assert.Equal(t, 1, adaptor.requestInputItems)
	assert.Equal(t, 1, adaptor.requestToolCount)
	assert.Equal(t, ProtocolChat, adaptor.protocol)
}

func TestAdaptorKeepsClaudeToolOnlyContinuationOnChat(t *testing.T) {
	info := newAdaptorTestInfo("deepseek-v4-flash", true)
	info.RelayFormat = types.RelayFormatClaude
	request := requestForFormat(types.RelayFormatClaude).(*dto.ClaudeRequest)
	request.Tools = []map[string]any{
		{
			"name": "Bash",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string"},
				},
				"required": []string{"command"},
			},
		},
	}
	request.Messages = []dto.ClaudeMessage{
		{Role: "user", Content: "inspect the repository"},
		{Role: "assistant", Content: []any{
			map[string]any{
				"type": "tool_use", "id": "toolu_missing_reasoning", "name": "Bash",
				"input": map[string]any{"command": "pwd"},
			},
		}},
		{Role: "user", Content: []any{map[string]any{
			"type": "tool_result", "tool_use_id": "toolu_missing_reasoning", "content": "/opt/opencode2api",
		}}},
	}
	adaptor := &Adaptor{}
	adaptor.Init(info)

	converted, err := adaptor.ConvertClaudeRequest(newAdaptorTestContext(), info, request)
	require.NoError(t, err)
	chat := converted.(*dto.GeneralOpenAIRequest)
	assert.Equal(t, ProtocolChat, adaptor.protocol)
	assert.True(t, adaptor.bufferClaudeToolCall)
	require.Len(t, chat.Messages, 3)
	assert.Equal(t, "assistant", chat.Messages[1].Role)
	require.Len(t, chat.Messages[1].ParseToolCalls(), 1)
	assert.Equal(t, "toolu_missing_reasoning", chat.Messages[1].ParseToolCalls()[0].ID)
	assert.Empty(t, chat.Messages[1].GetReasoningContent())
	assert.Equal(t, "tool", chat.Messages[2].Role)
	assert.Equal(t, "toolu_missing_reasoning", chat.Messages[2].ToolCallId)
}

func TestAdaptorKeepsOpenAIReasoningToolContinuationOnChat(t *testing.T) {
	info := newAdaptorTestInfo("deepseek-v4-flash", true)
	request := requestForFormat(types.RelayFormatOpenAI).(*dto.GeneralOpenAIRequest)
	request.Tools = []dto.ToolCallRequest{
		{
			Type: "function",
			Function: dto.FunctionRequest{
				Name: "Bash",
			},
		},
	}
	reasoning := "inspect before continuing"
	assistant := dto.Message{Role: "assistant", ReasoningContent: &reasoning}
	assistant.SetToolCalls([]dto.ToolCallRequest{{
		ID: "call_reasoning", Type: "function", Function: dto.FunctionRequest{
			Name: "Bash", Arguments: `{"command":"pwd"}`,
		},
	}})
	toolName := "Bash"
	toolResult := dto.Message{Role: "tool", Name: &toolName, ToolCallId: "call_reasoning"}
	toolResult.SetStringContent("/opt/opencode2api")
	request.Messages = []dto.Message{assistant, toolResult}

	adaptor := &Adaptor{}
	adaptor.Init(info)

	converted, err := adaptor.ConvertOpenAIRequest(newAdaptorTestContext(), info, request)
	require.NoError(t, err)
	chat := converted.(*dto.GeneralOpenAIRequest)
	assert.Equal(t, ProtocolChat, adaptor.protocol)
	assert.Equal(t, types.RelayFormat(types.RelayFormatOpenAI), info.FinalRequestRelayFormat)
	require.Len(t, chat.Messages, 2)
	assert.Equal(t, reasoning, chat.Messages[0].GetReasoningContent())
	require.Len(t, chat.Messages[0].ParseToolCalls(), 1)
	assert.Equal(t, "call_reasoning", chat.Messages[0].ParseToolCalls()[0].ID)
}

func TestAdaptorKeepsReasoningToolHistoryOnChatPath(t *testing.T) {
	info := newAdaptorTestInfo("glm-5.2", false)
	request := requestForFormat(types.RelayFormatClaude).(*dto.ClaudeRequest)
	request.Tools = []map[string]any{
		{"name": "Bash", "input_schema": map[string]any{"type": "object"}},
	}
	request.Messages = []dto.ClaudeMessage{
		{Role: "assistant", Content: []any{
			map[string]any{
				"type": "thinking", "thinking": "inspect the repository before calling Bash",
			},
			map[string]any{
				"type": "tool_use", "id": "toolu_test", "name": "Bash", "input": map[string]any{},
			},
		}},
		{Role: "user", Content: []any{map[string]any{
			"type": "tool_result", "tool_use_id": "toolu_test", "content": "OK",
		}}},
	}
	adaptor := &Adaptor{}
	adaptor.Init(info)

	converted, err := adaptor.ConvertClaudeRequest(newAdaptorTestContext(), info, request)
	require.NoError(t, err)
	require.IsType(t, &dto.GeneralOpenAIRequest{}, converted)
	assert.Equal(t, ProtocolChat, adaptor.protocol)
	assert.Equal(t, types.RelayFormat(types.RelayFormatOpenAI), info.FinalRequestRelayFormat)

	chat := converted.(*dto.GeneralOpenAIRequest)
	require.NotEmpty(t, chat.Messages)
	var assistant *dto.Message
	var toolIndex int
	for i := range chat.Messages {
		if chat.Messages[i].Role == "assistant" {
			assistant = &chat.Messages[i]
			toolIndex = i
			break
		}
	}
	require.NotNil(t, assistant)
	assert.Equal(t, "inspect the repository before calling Bash", assistant.GetReasoningContent())
	require.Len(t, assistant.ParseToolCalls(), 1)
	toolCall := assistant.ParseToolCalls()[0]
	assert.Equal(t, "toolu_test", toolCall.ID)
	assert.Equal(t, "Bash", toolCall.Function.Name)
	// The tool result must follow immediately on the same turn boundary.
	require.Greater(t, len(chat.Messages), toolIndex+1)
	assert.Equal(t, "tool", chat.Messages[toolIndex+1].Role)
	assert.Equal(t, "toolu_test", chat.Messages[toolIndex+1].ToolCallId)

	requestURL, err := adaptor.GetRequestURL(info)
	require.NoError(t, err)
	assert.Equal(t, constant.ChannelBaseURLs[constant.ChannelTypeOpenCodeGo]+"/chat/completions", requestURL)
}

func TestAdaptorBuffersStreamingClaudeChatReasoningContinuation(t *testing.T) {
	info := newAdaptorTestInfo("glm-5.2", true)
	info.RelayFormat = types.RelayFormatClaude
	request := requestForFormat(types.RelayFormatClaude).(*dto.ClaudeRequest)
	request.Tools = []map[string]any{
		{
			"name": "Bash",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string"},
				},
				"required": []string{"command"},
			},
		},
	}
	request.Messages = []dto.ClaudeMessage{
		{Role: "assistant", Content: []any{
			map[string]any{"type": "thinking", "thinking": "inspect before continuing"},
			map[string]any{"type": "tool_use", "id": "toolu_step", "name": "Bash", "input": map[string]any{"command": "ls"}},
		}},
		{Role: "user", Content: []any{map[string]any{
			"type": "tool_result", "tool_use_id": "toolu_step", "content": "OK",
		}}},
	}
	adaptor := &Adaptor{}
	adaptor.Init(info)

	converted, err := adaptor.ConvertClaudeRequest(newAdaptorTestContext(), info, request)
	require.NoError(t, err)
	chat := converted.(*dto.GeneralOpenAIRequest)
	require.NotNil(t, chat.Stream)
	assert.False(t, *chat.Stream)
	assert.True(t, adaptor.bufferClaudeToolCall)
	assert.False(t, adaptor.requestUpstreamStream)
	assert.Equal(t, ProtocolChat, adaptor.protocol)
}

func TestReasoningToolResponseRoundTripsIntoChatContinuation(t *testing.T) {
	reasoning := "inspect the repository before continuing"
	upstreamMessage := dto.Message{Role: "assistant"}
	upstreamMessage.ReasoningContent = &reasoning
	upstreamMessage.SetToolCalls([]dto.ToolCallRequest{{
		ID: "toolu_roundtrip", Type: "function", Function: dto.FunctionRequest{
			Name: "Bash", Arguments: `{"command":"pwd"}`,
		},
	}})
	convertedResponse, err := relayconvert.ConvertResponse(
		nil,
		nil,
		types.RelayFormatClaude,
		&dto.OpenAITextResponse{
			Id:    "chatcmpl_roundtrip",
			Model: "glm-5.2",
			Choices: []dto.OpenAITextResponseChoice{{
				Message: upstreamMessage, FinishReason: "tool_calls",
			}},
		},
	)
	require.NoError(t, err)
	claudeResponse := convertedResponse.Value.(*dto.ClaudeResponse)
	require.Len(t, claudeResponse.Content, 2)
	assert.Equal(t, "thinking", claudeResponse.Content[0].Type)
	assert.Equal(t, "tool_use", claudeResponse.Content[1].Type)

	info := newAdaptorTestInfo("glm-5.2", false)
	request := requestForFormat(types.RelayFormatClaude).(*dto.ClaudeRequest)
	request.Tools = []map[string]any{{
		"name": "Bash", "input_schema": map[string]any{"type": "object"},
	}}
	request.Messages = []dto.ClaudeMessage{
		{Role: "assistant", Content: claudeResponse.Content},
		{Role: "user", Content: []dto.ClaudeMediaMessage{{
			Type: "tool_result", ToolUseId: "toolu_roundtrip", Content: "OK",
		}}},
	}
	adaptor := &Adaptor{}
	adaptor.Init(info)

	convertedRequest, err := adaptor.ConvertClaudeRequest(newAdaptorTestContext(), info, request)
	require.NoError(t, err)
	chat := convertedRequest.(*dto.GeneralOpenAIRequest)
	assert.Equal(t, ProtocolChat, adaptor.protocol)
	require.Len(t, chat.Messages, 2)
	assert.Equal(t, "assistant", chat.Messages[0].Role)
	assert.Equal(t, reasoning, chat.Messages[0].GetReasoningContent())
	require.Len(t, chat.Messages[0].ParseToolCalls(), 1)
	assert.Equal(t, "toolu_roundtrip", chat.Messages[0].ParseToolCalls()[0].ID)
	assert.Equal(t, "tool", chat.Messages[1].Role)
	assert.Equal(t, "toolu_roundtrip", chat.Messages[1].ToolCallId)
}

func TestAdaptorAddsOpenCodeGoResponsesFunctionCallID(t *testing.T) {
	info := newAdaptorTestInfo("glm-5.2", false)
	request := requestForFormat(types.RelayFormatOpenAI).(*dto.GeneralOpenAIRequest)
	request.Tools = []dto.ToolCallRequest{
		{Type: "function", Function: dto.FunctionRequest{Name: "Bash"}},
	}
	assistant := dto.Message{Role: "assistant"}
	assistant.SetToolCalls([]dto.ToolCallRequest{
		{ID: "toolu_test", Type: "function", Function: dto.FunctionRequest{Name: "Bash", Arguments: "{}"}},
	})
	toolName := "Bash"
	toolResult := dto.Message{Role: "tool", Name: &toolName, ToolCallId: "toolu_test"}
	toolResult.SetStringContent("OK")
	user := dto.Message{Role: "user"}
	user.SetStringContent("list files")
	request.Messages = []dto.Message{
		user,
		assistant,
		toolResult,
	}
	adaptor := &Adaptor{}
	adaptor.Init(info)

	converted, err := adaptor.ConvertOpenAIRequest(newAdaptorTestContext(), info, request)
	require.NoError(t, err)
	responses := converted.(*dto.OpenAIResponsesRequest)
	var input []map[string]any
	require.NoError(t, json.Unmarshal(responses.Input, &input))

	var functionCall map[string]any
	for _, item := range input {
		assert.False(t, item["role"] == "assistant" && item["content"] == "")
		if item["type"] == "function_call" {
			functionCall = item
		}
	}
	require.NotNil(t, functionCall)
	assert.Equal(t, "toolu_test", functionCall["call_id"])
	assert.Equal(t, functionCall["call_id"], functionCall["id"])
}

func TestAdaptorRejectsPassThroughAndUnknownProtocol(t *testing.T) {
	info := newAdaptorTestInfo("glm-5.2", false)
	adaptor := &Adaptor{}
	adaptor.Init(info)
	_, err := adaptor.DoRequest(newAdaptorTestContext(), info, nil)
	require.ErrorContains(t, err, "does not allow pass-through")

	info = newAdaptorTestInfo("future-unknown", false)
	adaptor.Init(info)
	_, err = adaptor.GetRequestURL(info)
	require.ErrorContains(t, err, "protocol is not configured")
}

func TestAdaptorPersistsTransportFailureAndSkipsCallerCancellation(t *testing.T) {
	originalDoRequest := doOpenCodeGoAPIRequest
	originalObserver := observeOpenCodeGoTransportFailure
	originalNow := openCodeGoHealthNow
	t.Cleanup(func() {
		doOpenCodeGoAPIRequest = originalDoRequest
		observeOpenCodeGoTransportFailure = originalObserver
		openCodeGoHealthNow = originalNow
	})

	fixedNow := time.Unix(1_900_000_000, 0)
	openCodeGoHealthNow = func() time.Time { return fixedNow }
	doOpenCodeGoAPIRequest = func(_ relaychannel.Adaptor, _ *gin.Context, _ *relaycommon.RelayInfo, _ io.Reader) (*http.Response, error) {
		return nil, errors.New("connection reset")
	}

	observations := 0
	observeOpenCodeGoTransportFailure = func(channelID int, workspaceUID string, upstreamModel string, reason string, observedAt time.Time) (bool, error) {
		observations++
		assert.Equal(t, 42, channelID)
		assert.Equal(t, "workspace-transport", workspaceUID)
		assert.Equal(t, "glm-5.2", upstreamModel)
		assert.Equal(t, "connection reset", reason)
		assert.Equal(t, fixedNow, observedAt)
		return true, nil
	}

	info := newAdaptorTestInfo("glm-5.2", false)
	info.ChannelId = 42
	adaptor := &Adaptor{
		converted:            true,
		workspaceSelected:    true,
		selectedWorkspaceUID: "workspace-transport",
	}
	_, err := adaptor.DoRequest(newAdaptorTestContext(), info, nil)
	require.Error(t, err)
	assert.Equal(t, 1, observations)

	c := newAdaptorTestContext()
	requestContext, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(requestContext)
	cancel()
	doOpenCodeGoAPIRequest = func(_ relaychannel.Adaptor, _ *gin.Context, _ *relaycommon.RelayInfo, _ io.Reader) (*http.Response, error) {
		return nil, context.Canceled
	}
	_, err = adaptor.DoRequest(c, info, nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, observations)
}

func TestAdaptorDoRequestMakesExactlyOneUpstreamAttempt(t *testing.T) {
	originalDoRequest := doOpenCodeGoAPIRequest
	calls := 0
	doOpenCodeGoAPIRequest = func(_ relaychannel.Adaptor, _ *gin.Context, _ *relaycommon.RelayInfo, _ io.Reader) (*http.Response, error) {
		calls++
		return nil, errors.New("temporary upstream failure")
	}
	t.Cleanup(func() { doOpenCodeGoAPIRequest = originalDoRequest })

	adaptor := &Adaptor{converted: true}
	_, err := adaptor.DoRequest(newAdaptorTestContext(), newAdaptorTestInfo("gpt-5.6-luna", false), nil)
	require.Error(t, err)
	assert.Equal(t, 1, calls)
}
