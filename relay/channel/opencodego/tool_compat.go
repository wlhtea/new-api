package opencodego

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
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

func (a *Adaptor) captureRequestShape(request any) {
	if a == nil {
		return
	}
	a.requestInputItems = 0
	a.requestToolCount = 0
	a.requestUpstreamStream = false

	switch typed := request.(type) {
	case *dto.GeneralOpenAIRequest:
		if typed == nil {
			return
		}
		a.requestInputItems = len(typed.Messages)
		a.requestToolCount = len(typed.Tools) + countJSONListItems(typed.Functions)
		a.requestUpstreamStream = typed.Stream != nil && *typed.Stream
	case *dto.ClaudeRequest:
		if typed == nil {
			return
		}
		a.requestInputItems = len(typed.Messages)
		a.requestToolCount = countClaudeTools(typed.Tools)
		a.requestUpstreamStream = typed.Stream != nil && *typed.Stream
	case *dto.OpenAIResponsesRequest:
		if typed == nil {
			return
		}
		a.requestInputItems = countJSONListItems(typed.Input)
		if a.requestInputItems == 0 && len(typed.Input) > 0 && string(typed.Input) != "null" {
			a.requestInputItems = 1
		}
		a.requestToolCount = countJSONListItems(typed.Tools)
		a.requestUpstreamStream = typed.Stream != nil && *typed.Stream
	}
}

func countClaudeTools(tools any) int {
	if tools == nil {
		return 0
	}
	raw, err := common.Marshal(tools)
	if err != nil {
		return 0
	}
	return countJSONListItems(raw)
}

func countJSONListItems(raw []byte) int {
	if len(raw) == 0 {
		return 0
	}
	var entries []json.RawMessage
	if err := common.Unmarshal(raw, &entries); err != nil {
		return 0
	}
	return len(entries)
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
	for index, item := range input {
		itemType, _ := item["type"].(string)
		if itemType == "reasoning" {
			if id, _ := item["id"].(string); strings.TrimSpace(id) == "" {
				raw, err := common.Marshal(item)
				if err != nil {
					return err
				}
				seed := append([]byte(strconv.Itoa(index)+"\x00"), raw...)
				digest := sha256.Sum256(seed)
				item["id"] = "rs_" + hex.EncodeToString(digest[:12])
				changed = true
			}
			if status, _ := item["status"].(string); strings.TrimSpace(status) == "" {
				item["status"] = "completed"
				changed = true
			}
			continue
		}
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
