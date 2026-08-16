package common

import (
	"encoding/json"
	"errors"
	"testing"

	rootcommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/QuantumNous/new-api/relaykit/types"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCloneForOpenCodePlanningRejectsInvalidRoots(t *testing.T) {
	var nilInfo *RelayInfo
	clone, err := nilInfo.CloneForOpenCodePlanning()
	require.Error(t, err)
	assert.Nil(t, clone)

	clone, err = (&RelayInfo{}).CloneForOpenCodePlanning()
	require.Error(t, err)
	assert.Nil(t, clone)

	var nilChat *dto.GeneralOpenAIRequest
	clone, err = (&RelayInfo{Request: nilChat}).CloneForOpenCodePlanning()
	require.Error(t, err)
	assert.Nil(t, clone)

	clone, err = (&RelayInfo{Request: &dto.RerankRequest{}}).CloneForOpenCodePlanning()
	require.Error(t, err)
	assert.Nil(t, clone)
	assert.Contains(t, err.Error(), "unsupported request type")
}

func TestCloneForOpenCodePlanningPreservesChannelSettingExtensions(t *testing.T) {
	const settingsJSON = `{
		"upstream_model_update_last_detected_models": [],
		"opencode_go": {
			"model_protocols": {"Vendor/Model": "messages"},
			"future_nested": {"items": ["root"]}
		},
		"future_root": {"enabled": true}
	}`
	var settings dto.ChannelOtherSettings
	require.NoError(t, json.Unmarshal([]byte(settingsJSON), &settings))

	clone, err := (&RelayInfo{
		Request: &dto.GeneralOpenAIRequest{Model: "Vendor/Model"},
		ChannelMeta: &ChannelMeta{
			ChannelOtherSettings: settings,
		},
	}).CloneForOpenCodePlanning()
	require.NoError(t, err)

	rootEncoded, err := json.Marshal(settings)
	require.NoError(t, err)
	encoded, err := json.Marshal(clone.ChannelOtherSettings)
	require.NoError(t, err)
	assert.JSONEq(t, string(rootEncoded), string(encoded))
	assert.Contains(t, string(encoded), "future_nested")
	assert.Contains(t, string(encoded), "future_root")
	clone.ChannelOtherSettings.OpenCodeGo.ModelProtocols["Vendor/Model"] = dto.OpenCodeGoProtocolChat
	assert.Equal(t, dto.OpenCodeGoProtocolMessages, settings.OpenCodeGo.ModelProtocols["Vendor/Model"])
}

func TestCloneForOpenCodePlanningDeepCopiesSupportedRequests(t *testing.T) {
	t.Run("OpenAI Chat", func(t *testing.T) {
		request := &dto.GeneralOpenAIRequest{
			Model: "chat-model",
			Messages: []dto.Message{{
				Role: "user",
				Content: map[string]interface{}{
					"parts": []interface{}{map[string]interface{}{"text": "root"}},
				},
			}},
			Prompt: map[string]interface{}{"nested": map[string]interface{}{"value": "root"}},
			Tools: []dto.ToolCallRequest{{
				Type: "function",
				Function: dto.FunctionRequest{
					Name:       "lookup",
					Parameters: map[string]interface{}{"required": []interface{}{"query"}},
				},
			}},
			Metadata: json.RawMessage(`{"source":"root"}`),
		}
		first, err := (&RelayInfo{Request: request}).CloneForOpenCodePlanning()
		require.NoError(t, err)
		second, err := (&RelayInfo{Request: request}).CloneForOpenCodePlanning()
		require.NoError(t, err)

		firstRequest := first.Request.(*dto.GeneralOpenAIRequest)
		secondRequest := second.Request.(*dto.GeneralOpenAIRequest)
		require.NotSame(t, request, firstRequest)
		require.NotSame(t, firstRequest, secondRequest)
		firstRequest.Messages[0].Content.(map[string]interface{})["parts"].([]interface{})[0].(map[string]interface{})["text"] = "candidate"
		firstRequest.Prompt.(map[string]interface{})["nested"].(map[string]interface{})["value"] = "candidate"
		firstRequest.Tools[0].Function.Parameters.(map[string]interface{})["required"].([]interface{})[0] = "candidate"
		firstRequest.Metadata[0] = '['

		assert.Equal(t, "root", request.Messages[0].Content.(map[string]interface{})["parts"].([]interface{})[0].(map[string]interface{})["text"])
		assert.Equal(t, "root", secondRequest.Messages[0].Content.(map[string]interface{})["parts"].([]interface{})[0].(map[string]interface{})["text"])
		assert.Equal(t, "root", request.Prompt.(map[string]interface{})["nested"].(map[string]interface{})["value"])
		assert.Equal(t, "query", request.Tools[0].Function.Parameters.(map[string]interface{})["required"].([]interface{})[0])
		assert.Equal(t, byte('{'), request.Metadata[0])
	})

	t.Run("OpenAI Responses", func(t *testing.T) {
		stream := true
		request := &dto.OpenAIResponsesRequest{
			Model:     "responses-model",
			Input:     json.RawMessage(`[{"role":"user","content":"root"}]`),
			Tools:     json.RawMessage(`[{"type":"function","name":"root"}]`),
			Reasoning: &dto.Reasoning{Effort: "high", Context: json.RawMessage(`{"value":"root"}`)},
			Stream:    &stream,
		}
		first, err := (&RelayInfo{Request: request}).CloneForOpenCodePlanning()
		require.NoError(t, err)
		second, err := (&RelayInfo{Request: request}).CloneForOpenCodePlanning()
		require.NoError(t, err)

		firstRequest := first.Request.(*dto.OpenAIResponsesRequest)
		secondRequest := second.Request.(*dto.OpenAIResponsesRequest)
		firstRequest.Input[0] = '{'
		firstRequest.Tools[0] = '{'
		firstRequest.Reasoning.Effort = "low"
		*firstRequest.Stream = false

		assert.Equal(t, byte('['), request.Input[0])
		assert.Equal(t, byte('['), secondRequest.Input[0])
		assert.Equal(t, byte('['), request.Tools[0])
		assert.Equal(t, "high", request.Reasoning.Effort)
		assert.True(t, *request.Stream)
	})

	t.Run("Claude Messages", func(t *testing.T) {
		budget := 2048
		request := &dto.ClaudeRequest{
			Model:  "messages-model",
			System: []interface{}{map[string]interface{}{"type": "text", "text": "root"}},
			Messages: []dto.ClaudeMessage{{
				Role:    "user",
				Content: []interface{}{map[string]interface{}{"type": "text", "text": "root"}},
			}},
			StopSequences: []string{"root-stop"},
			Tools: []interface{}{map[string]interface{}{
				"name":         "root-tool",
				"input_schema": map[string]interface{}{"required": []interface{}{"query"}},
			}},
			OutputConfig: json.RawMessage(`{"effort":"high"}`),
			Thinking:     &dto.Thinking{Type: "enabled", BudgetTokens: &budget},
		}
		first, err := (&RelayInfo{Request: request}).CloneForOpenCodePlanning()
		require.NoError(t, err)
		second, err := (&RelayInfo{Request: request}).CloneForOpenCodePlanning()
		require.NoError(t, err)

		firstRequest := first.Request.(*dto.ClaudeRequest)
		secondRequest := second.Request.(*dto.ClaudeRequest)
		firstRequest.System.([]interface{})[0].(map[string]interface{})["text"] = "candidate"
		firstRequest.Messages[0].Content.([]interface{})[0].(map[string]interface{})["text"] = "candidate"
		firstRequest.StopSequences[0] = "candidate-stop"
		firstRequest.Tools.([]interface{})[0].(map[string]interface{})["input_schema"].(map[string]interface{})["required"].([]interface{})[0] = "candidate"
		firstRequest.OutputConfig[0] = '['
		*firstRequest.Thinking.BudgetTokens = 1

		assert.Equal(t, "root", request.System.([]interface{})[0].(map[string]interface{})["text"])
		assert.Equal(t, "root", secondRequest.System.([]interface{})[0].(map[string]interface{})["text"])
		assert.Equal(t, "root", request.Messages[0].Content.([]interface{})[0].(map[string]interface{})["text"])
		assert.Equal(t, "root-stop", request.StopSequences[0])
		assert.Equal(t, "query", request.Tools.([]interface{})[0].(map[string]interface{})["input_schema"].(map[string]interface{})["required"].([]interface{})[0])
		assert.Equal(t, byte('{'), request.OutputConfig[0])
		assert.Equal(t, 2048, *request.Thinking.BudgetTokens)
	})
}

func TestCloneForOpenCodePlanningIsolatesRootAndSiblingState(t *testing.T) {
	usageConversionEnabled := true
	root := &RelayInfo{
		Request: &dto.GeneralOpenAIRequest{
			Model:    "root-model",
			Messages: []dto.Message{{Role: "user", Content: "root"}},
		},
		RequestHeaders: map[string]string{"X-Root": "root"},
		RealtimeTools: []dto.RealTimeTool{{
			Name:       "root-tool",
			Parameters: map[string]interface{}{"nested": map[string]interface{}{"value": "root"}},
		}},
		RequestConversionChain: []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatClaude},
		RuntimeHeadersOverride: map[string]interface{}{
			"nested": map[string]interface{}{"items": []interface{}{map[string]interface{}{"value": "root"}}},
		},
		ParamOverrideAudit: []string{"root-audit"},
		ChannelMeta: &ChannelMeta{
			ParamOverride: map[string]interface{}{
				"operations": []interface{}{map[string]interface{}{"path": "metadata", "value": map[string]interface{}{"source": "root"}}},
			},
			HeadersOverride: map[string]interface{}{
				"X-Header": []interface{}{map[string]interface{}{"value": "root"}},
			},
			ChannelSetting: dto.ChannelSettings{SystemPrompt: "root-system"},
			ChannelOtherSettings: dto.ChannelOtherSettings{
				UpstreamModelUpdateLastDetectedModels: []string{"root-detected"},
				AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{
					IncomingPath: "/v1/messages",
					Models:       []string{"root-route-model"},
				}}},
				OpenCodeGo: &dto.OpenCodeGoConfig{
					ModelProtocols:                map[string]string{"root-model": dto.OpenCodeGoProtocolChat},
					BillingUsageConversionEnabled: &usageConversionEnabled,
				},
			},
		},
		ClaudeConvertInfo: &ClaudeConvertInfo{
			Index: 1,
			Usage: &dto.Usage{
				PromptTokens:       11,
				InputTokensDetails: &dto.InputTokenDetails{CachedTokens: 3},
				Cost:               map[string]interface{}{"nested": map[string]interface{}{"value": "root"}},
				BillingUsage: &dto.BillingUsage{
					ClaudeUsage: &dto.ClaudeUsage{InputTokens: 4},
				},
			},
		},
		ResponsesUsageInfo: &ResponsesUsageInfo{BuiltInTools: map[string]*BuildInToolInfo{
			"web_search": {ToolName: "web_search", CallCount: 1},
		}},
		RerankerInfo: &RerankerInfo{
			Documents: []interface{}{map[string]interface{}{"metadata": map[string]interface{}{"source": "root"}}},
		},
		QuotaClamp:            &rootcommon.QuotaClamp{Op: "root", Clamped: 7},
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{ModelName: "root-model", EstimatedPromptTokens: 11},
		BillingRequestInput: &billingexpr.RequestInput{
			Headers: map[string]string{"X-Billing": "root"},
			Body:    []byte(`{"source":"root"}`),
			ResolveParam: func(path string) (interface{}, bool, error) {
				return path, true, nil
			},
		},
		TaskRelayInfo: &TaskRelayInfo{Action: "root-action", LockedChannel: &struct{ Value string }{Value: "opaque"}},
		convOptions: &convmeta.Options{
			UsageConversionEnabled: &usageConversionEnabled,
		},
		PriceData: hosttypes.PriceData{ModelRatio: 1},
		LastError: types.NewError(errors.New("root error"), types.ErrorCodeBadResponse),
	}
	root.PriceData.AddOtherRatio("root", 2)
	root.StreamStatus = NewStreamStatus()
	root.StreamStatus.RecordError("root stream error")
	root.StreamStatus.RequireProtocolTerminal()

	first, err := root.CloneForOpenCodePlanning()
	require.NoError(t, err)
	second, err := root.CloneForOpenCodePlanning()
	require.NoError(t, err)

	require.NotSame(t, root.Request, first.Request)
	require.NotSame(t, root.ChannelMeta, first.ChannelMeta)
	require.NotSame(t, root.ChannelOtherSettings.OpenCodeGo, first.ChannelOtherSettings.OpenCodeGo)
	require.NotSame(t, root.ClaudeConvertInfo, first.ClaudeConvertInfo)
	require.NotSame(t, root.ResponsesUsageInfo, first.ResponsesUsageInfo)
	require.NotSame(t, root.StreamStatus, first.StreamStatus)
	require.NotSame(t, root.convOptions, first.convOptions)
	require.Same(t, root.LastError, first.LastError)
	require.Same(t, root.TaskRelayInfo.LockedChannel, first.TaskRelayInfo.LockedChannel)
	rootSettingsJSON, err := rootcommon.Marshal(root.ChannelMeta.ChannelOtherSettings)
	require.NoError(t, err)
	firstSettingsJSON, err := rootcommon.Marshal(first.ChannelMeta.ChannelOtherSettings)
	require.NoError(t, err)
	assert.JSONEq(t, string(rootSettingsJSON), string(firstSettingsJSON))

	first.RequestHeaders["X-Root"] = "candidate"
	first.RealtimeTools[0].Parameters.(map[string]interface{})["nested"].(map[string]interface{})["value"] = "candidate"
	first.RequestConversionChain[0] = types.RelayFormatClaude
	first.RuntimeHeadersOverride["nested"].(map[string]interface{})["items"].([]interface{})[0].(map[string]interface{})["value"] = "candidate"
	first.ParamOverrideAudit[0] = "candidate-audit"
	first.ChannelMeta.ParamOverride["operations"].([]interface{})[0].(map[string]interface{})["value"].(map[string]interface{})["source"] = "candidate"
	first.ChannelMeta.HeadersOverride["X-Header"].([]interface{})[0].(map[string]interface{})["value"] = "candidate"
	first.ChannelMeta.ChannelSetting.SystemPrompt = "candidate-system"
	first.ChannelMeta.ChannelOtherSettings.UpstreamModelUpdateLastDetectedModels[0] = "candidate-detected"
	first.ChannelMeta.ChannelOtherSettings.AdvancedCustom.Routes[0].Models[0] = "candidate-route-model"
	first.ChannelMeta.ChannelOtherSettings.OpenCodeGo.ModelProtocols["root-model"] = dto.OpenCodeGoProtocolResponses
	*first.ChannelMeta.ChannelOtherSettings.OpenCodeGo.BillingUsageConversionEnabled = false
	first.ClaudeConvertInfo.Index = 9
	first.ClaudeConvertInfo.Usage.PromptTokens = 99
	first.ClaudeConvertInfo.Usage.InputTokensDetails.CachedTokens = 99
	first.ClaudeConvertInfo.Usage.Cost.(map[string]interface{})["nested"].(map[string]interface{})["value"] = "candidate"
	first.ClaudeConvertInfo.Usage.BillingUsage.ClaudeUsage.InputTokens = 99
	first.ResponsesUsageInfo.BuiltInTools["web_search"].CallCount = 9
	first.StreamStatus.RecordError("candidate stream error")
	first.RerankerInfo.Documents[0].(map[string]interface{})["metadata"].(map[string]interface{})["source"] = "candidate"
	first.QuotaClamp.Clamped = 99
	first.TieredBillingSnapshot.EstimatedPromptTokens = 99
	first.BillingRequestInput.Headers["X-Billing"] = "candidate"
	first.BillingRequestInput.Body[0] = '['
	*first.convOptions.UsageConversionEnabled = false
	first.PriceData.AddOtherRatio("candidate", 3)
	first.TaskRelayInfo.Action = "candidate-action"

	for _, untouched := range []*RelayInfo{root, second} {
		assert.Equal(t, "root", untouched.RequestHeaders["X-Root"])
		assert.Equal(t, "root", untouched.RealtimeTools[0].Parameters.(map[string]interface{})["nested"].(map[string]interface{})["value"])
		assert.Equal(t, types.RelayFormatOpenAI, untouched.RequestConversionChain[0])
		assert.Equal(t, "root", untouched.RuntimeHeadersOverride["nested"].(map[string]interface{})["items"].([]interface{})[0].(map[string]interface{})["value"])
		assert.Equal(t, "root-audit", untouched.ParamOverrideAudit[0])
		assert.Equal(t, "root", untouched.ChannelMeta.ParamOverride["operations"].([]interface{})[0].(map[string]interface{})["value"].(map[string]interface{})["source"])
		assert.Equal(t, "root", untouched.ChannelMeta.HeadersOverride["X-Header"].([]interface{})[0].(map[string]interface{})["value"])
		assert.Equal(t, "root-system", untouched.ChannelMeta.ChannelSetting.SystemPrompt)
		assert.Equal(t, "root-detected", untouched.ChannelMeta.ChannelOtherSettings.UpstreamModelUpdateLastDetectedModels[0])
		assert.Equal(t, "root-route-model", untouched.ChannelMeta.ChannelOtherSettings.AdvancedCustom.Routes[0].Models[0])
		assert.Equal(t, dto.OpenCodeGoProtocolChat, untouched.ChannelMeta.ChannelOtherSettings.OpenCodeGo.ModelProtocols["root-model"])
		assert.True(t, *untouched.ChannelMeta.ChannelOtherSettings.OpenCodeGo.BillingUsageConversionEnabled)
		assert.Equal(t, 1, untouched.ClaudeConvertInfo.Index)
		assert.Equal(t, 11, untouched.ClaudeConvertInfo.Usage.PromptTokens)
		assert.Equal(t, 3, untouched.ClaudeConvertInfo.Usage.InputTokensDetails.CachedTokens)
		assert.Equal(t, "root", untouched.ClaudeConvertInfo.Usage.Cost.(map[string]interface{})["nested"].(map[string]interface{})["value"])
		assert.Equal(t, 4, untouched.ClaudeConvertInfo.Usage.BillingUsage.ClaudeUsage.InputTokens)
		assert.Equal(t, 1, untouched.ResponsesUsageInfo.BuiltInTools["web_search"].CallCount)
		assert.Equal(t, 1, untouched.StreamStatus.TotalErrorCount())
		assert.True(t, untouched.StreamStatus.ProtocolTerminalRequired())
		assert.Equal(t, "root", untouched.RerankerInfo.Documents[0].(map[string]interface{})["metadata"].(map[string]interface{})["source"])
		assert.Equal(t, 7, untouched.QuotaClamp.Clamped)
		assert.Equal(t, 11, untouched.TieredBillingSnapshot.EstimatedPromptTokens)
		assert.Equal(t, "root", untouched.BillingRequestInput.Headers["X-Billing"])
		assert.Equal(t, byte('{'), untouched.BillingRequestInput.Body[0])
		assert.True(t, *untouched.convOptions.UsageConversionEnabled)
		assert.True(t, untouched.PriceData.HasOtherRatio("root"))
		assert.False(t, untouched.PriceData.HasOtherRatio("candidate"))
		assert.Equal(t, "root-action", untouched.TaskRelayInfo.Action)
		value, found, resolveErr := untouched.BillingRequestInput.ResolveParam("path")
		require.NoError(t, resolveErr)
		assert.True(t, found)
		assert.Equal(t, "path", value)
	}
}
