package opencodego

import (
	"encoding/json"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

func requestUsesFunctionTools(request any) bool {
	switch typed := request.(type) {
	case *dto.GeneralOpenAIRequest:
		return typed != nil && generalOpenAIRequestUsesFunctionTools(typed)
	case dto.GeneralOpenAIRequest:
		return generalOpenAIRequestUsesFunctionTools(&typed)
	case *dto.ClaudeRequest:
		return typed != nil && claudeToolsContainFunction(typed.Tools)
	case dto.ClaudeRequest:
		return claudeToolsContainFunction(typed.Tools)
	default:
		return false
	}
}

func generalOpenAIRequestUsesFunctionTools(request *dto.GeneralOpenAIRequest) bool {
	if request == nil {
		return false
	}
	if len(request.Functions) > 0 && string(request.Functions) != "null" && string(request.Functions) != "[]" {
		return true
	}
	for _, tool := range request.Tools {
		if tool.Type == "" || strings.EqualFold(tool.Type, "function") || strings.TrimSpace(tool.Function.Name) != "" {
			return true
		}
	}
	return false
}

func claudeToolsContainFunction(tools any) bool {
	if tools == nil {
		return false
	}
	raw, err := common.Marshal(tools)
	if err != nil {
		return false
	}
	var entries []map[string]any
	if common.Unmarshal(raw, &entries) != nil {
		return false
	}
	for _, tool := range entries {
		toolType, _ := tool["type"].(string)
		if toolType == "" || !strings.HasPrefix(strings.ToLower(toolType), "web_search") {
			return true
		}
	}
	return false
}

// OpenCode Go currently reconstructs Responses tool history from
// function_call.id, while standard clients commonly send only call_id.
func prepareOpenCodeGoResponsesToolHistory(request *dto.OpenAIResponsesRequest) error {
	if request == nil || len(request.Input) == 0 {
		return nil
	}
	var input []map[string]any
	if err := common.Unmarshal(request.Input, &input); err != nil {
		return nil
	}
	changed := false
	for _, item := range input {
		itemType, _ := item["type"].(string)
		if itemType != "function_call" {
			continue
		}
		if id, _ := item["id"].(string); strings.TrimSpace(id) != "" {
			continue
		}
		callID, _ := item["call_id"].(string)
		if strings.TrimSpace(callID) == "" {
			continue
		}
		item["id"] = callID
		changed = true
	}
	if !changed {
		return nil
	}
	raw, err := common.Marshal(input)
	if err != nil {
		return err
	}
	request.Input = json.RawMessage(raw)
	return nil
}
