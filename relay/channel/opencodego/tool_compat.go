package opencodego

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

// openCodeGoNamespaceTool is the reversible identity for a function that was
// declared under a Codex Responses namespace. OpenCode Go only accepts flat
// Responses function tools, so the mapping is carried by the adaptor until the
// provider response is converted back to the client format.
type openCodeGoNamespaceTool struct {
	Namespace string
	Name      string
}

const openCodeGoResponsesToolNameLimit = 64

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

// prepareOpenCodeGoResponsesTools lowers Codex-only Responses tool shapes to
// the strict function-only schema accepted by the OpenCode Go Responses route.
// It mutates request in place and returns only names that need restoration on
// the response path.
func prepareOpenCodeGoResponsesTools(request *dto.OpenAIResponsesRequest) (map[string]openCodeGoNamespaceTool, error) {
	if request == nil {
		return nil, nil
	}

	var tools []map[string]any
	hasTools := false
	if len(request.Tools) > 0 && string(request.Tools) != "null" {
		if err := common.Unmarshal(request.Tools, &tools); err != nil {
			return nil, fmt.Errorf("invalid OpenCode Go Responses tools: %w", err)
		}
		hasTools = true
	}

	// Codex Desktop can put declarations in an additional_tools input item.
	// OpenCode Go's input schema does not define that item, so lift its tools to
	// the top-level declaration and remove the metadata item from history.
	var input []any
	hasInput := len(request.Input) > 0 && string(request.Input) != "null"
	if hasInput && common.Unmarshal(request.Input, &input) == nil {
		filtered := make([]any, 0, len(input))
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok || strings.TrimSpace(stringValue(item["type"])) != "additional_tools" {
				filtered = append(filtered, raw)
				continue
			}
			if extra, ok := responseToolMaps(item["tools"]); ok {
				tools = append(tools, extra...)
				hasTools = true
			}
		}
		if len(filtered) != len(input) {
			input = filtered
			encoded, err := common.Marshal(input)
			if err != nil {
				return nil, err
			}
			request.Input = json.RawMessage(encoded)
		}
	}

	if !hasTools {
		return nil, nil
	}

	// Detect collisions before lowering. A flat name cannot identify two
	// different namespace children or a top-level function unambiguously.
	topLevel := make(map[string]struct{})
	for _, tool := range tools {
		typ := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		if typ == "function" || typ == "custom" {
			if name := strings.TrimSpace(stringValue(tool["name"])); name != "" {
				topLevel[name] = struct{}{}
			}
		}
	}
	names := make(map[string]openCodeGoNamespaceTool)
	for _, tool := range tools {
		if strings.ToLower(strings.TrimSpace(stringValue(tool["type"]))) != "namespace" {
			continue
		}
		if err := collectOpenCodeGoNamespaceTools(tool, "", topLevel, names); err != nil {
			return nil, err
		}
	}

	lowered := make([]map[string]any, 0, len(tools))
	seen := make(map[string]struct{})
	for _, tool := range tools {
		if err := appendOpenCodeGoResponsesTool(&lowered, seen, tool, "", topLevel, names); err != nil {
			return nil, err
		}
	}
	if len(lowered) == 0 {
		request.Tools = nil
	} else {
		encoded, err := common.Marshal(lowered)
		if err != nil {
			return nil, err
		}
		request.Tools = json.RawMessage(encoded)
	}

	if hasInput {
		if common.Unmarshal(request.Input, &input) == nil {
			rewriteOpenCodeGoResponsesHistory(input, names)
			encoded, err := common.Marshal(input)
			if err != nil {
				return nil, err
			}
			request.Input = json.RawMessage(encoded)
		}
	}
	rewriteOpenCodeGoResponsesToolChoice(request, names)
	return names, nil
}

func responseToolMaps(value any) ([]map[string]any, bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if typed, ok := item.(map[string]any); ok {
			result = append(result, typed)
		}
	}
	return result, true
}

func responseToolChildren(tool map[string]any) []map[string]any {
	children, ok := responseToolMaps(tool["tools"])
	if ok {
		return children
	}
	children, _ = responseToolMaps(tool["children"])
	return children
}

func qualifyOpenCodeGoNamespaceToolName(namespace, child string) string {
	namespace = strings.TrimSpace(namespace)
	child = strings.TrimSpace(child)
	if namespace == "" || child == "" || strings.HasPrefix(child, "mcp__") || strings.HasPrefix(child, namespace) {
		return child
	}
	full := namespace + "__" + child
	if len(full) <= openCodeGoResponsesToolNameLimit {
		return full
	}
	// Responses function names are limited to 64 characters. Keep a readable
	// prefix and a deterministic suffix so response restoration remains stable.
	digest := sha256.Sum256([]byte(full))
	suffix := "__" + hex.EncodeToString(digest[:4])
	return full[:openCodeGoResponsesToolNameLimit-len(suffix)] + suffix
}

func collectOpenCodeGoNamespaceTools(tool map[string]any, parent string, topLevel map[string]struct{}, names map[string]openCodeGoNamespaceTool) error {
	namespace := strings.TrimSpace(stringValue(tool["name"]))
	if parent != "" {
		namespace = qualifyOpenCodeGoNamespaceToolName(parent, namespace)
	}
	if namespace == "" {
		return nil
	}
	for _, child := range responseToolChildren(tool) {
		typ := strings.ToLower(strings.TrimSpace(stringValue(child["type"])))
		name := strings.TrimSpace(stringValue(child["name"]))
		switch typ {
		case "function":
			if name == "" {
				continue
			}
			flat := qualifyOpenCodeGoNamespaceToolName(namespace, name)
			if _, exists := topLevel[flat]; exists {
				return fmt.Errorf("OpenCode Go namespace tool %q/%q conflicts with top-level tool %q", namespace, name, flat)
			}
			entry := openCodeGoNamespaceTool{Namespace: namespace, Name: name}
			if previous, exists := names[flat]; exists && previous != entry {
				return fmt.Errorf("OpenCode Go namespace tools %q/%q and %q/%q both flatten to %q", previous.Namespace, previous.Name, namespace, name, flat)
			}
			names[flat] = entry
		case "namespace":
			if err := collectOpenCodeGoNamespaceTools(child, namespace, topLevel, names); err != nil {
				return err
			}
		}
	}
	return nil
}

func appendOpenCodeGoResponsesTool(out *[]map[string]any, seen map[string]struct{}, tool map[string]any, parent string, topLevel map[string]struct{}, names map[string]openCodeGoNamespaceTool) error {
	typ := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
	switch typ {
	case "namespace":
		namespace := strings.TrimSpace(stringValue(tool["name"]))
		if parent != "" {
			namespace = qualifyOpenCodeGoNamespaceToolName(parent, namespace)
		}
		for _, child := range responseToolChildren(tool) {
			childType := strings.ToLower(strings.TrimSpace(stringValue(child["type"])))
			if childType == "namespace" {
				if err := appendOpenCodeGoResponsesTool(out, seen, child, namespace, topLevel, names); err != nil {
					return err
				}
				continue
			}
			if childType != "function" {
				continue
			}
			name := strings.TrimSpace(stringValue(child["name"]))
			if name == "" {
				continue
			}
			flat := qualifyOpenCodeGoNamespaceToolName(namespace, name)
			if _, exists := seen[flat]; exists {
				continue
			}
			seen[flat] = struct{}{}
			*out = append(*out, openCodeGoResponsesFunctionTool(child, flat))
		}
	case "function":
		name := strings.TrimSpace(stringValue(tool["name"]))
		if name == "" {
			return nil
		}
		if _, exists := seen[name]; exists {
			return nil
		}
		seen[name] = struct{}{}
		*out = append(*out, openCodeGoResponsesFunctionTool(tool, name))
	case "custom":
		// OpenCode Go does not implement Responses custom tools. A function
		// wrapper keeps Codex command tools usable without sending an unknown
		// tool type to the strict upstream schema.
		name := strings.TrimSpace(stringValue(tool["name"]))
		if name == "" {
			return nil
		}
		if _, exists := seen[name]; exists {
			return nil
		}
		seen[name] = struct{}{}
		*out = append(*out, openCodeGoResponsesCustomTool(name, stringValue(tool["description"])))
	default:
		// Hosted/server tools (web_search, image generation, tool search, and
		// unknown extensions) are intentionally omitted: OpenCode Go cannot
		// execute them and forwarding them causes a 400 unknown-tool-type error.
	}
	return nil
}

func openCodeGoResponsesFunctionTool(tool map[string]any, name string) map[string]any {
	parameters := tool["parameters"]
	if parameters == nil {
		parameters = tool["parametersJsonSchema"]
	}
	if parameters == nil {
		parameters = tool["input_schema"]
	}
	if parameters == nil {
		parameters = map[string]any{"type": "object"}
	}
	result := map[string]any{
		"type":        "function",
		"name":        name,
		"description": stringValue(tool["description"]),
		"parameters":  parameters,
	}
	if strict, ok := tool["strict"].(bool); ok {
		result["strict"] = strict
	}
	return result
}

func openCodeGoResponsesCustomTool(name, description string) map[string]any {
	return map[string]any{
		"type":        "function",
		"name":        name,
		"description": description,
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"input": map[string]any{"type": "string"}},
			"required":   []string{"input"},
		},
	}
}

func rewriteOpenCodeGoResponsesHistory(value any, names map[string]openCodeGoNamespaceTool) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			rewriteOpenCodeGoResponsesHistory(item, names)
		}
	case map[string]any:
		if strings.TrimSpace(stringValue(typed["type"])) == "function_call" {
			name := strings.TrimSpace(stringValue(typed["name"]))
			namespace := strings.TrimSpace(stringValue(typed["namespace"]))
			if namespace != "" {
				flat := qualifyOpenCodeGoNamespaceToolName(namespace, name)
				if _, exists := names[flat]; exists || flat != "" {
					typed["name"] = flat
					delete(typed, "namespace")
				}
			}
		}
		for _, child := range typed {
			rewriteOpenCodeGoResponsesHistory(child, names)
		}
	}
}

func rewriteOpenCodeGoResponsesToolChoice(request *dto.OpenAIResponsesRequest, names map[string]openCodeGoNamespaceTool) {
	if request == nil || len(request.ToolChoice) == 0 || string(request.ToolChoice) == "null" {
		return
	}
	var choice any
	if common.Unmarshal(request.ToolChoice, &choice) != nil {
		return
	}
	if object, ok := choice.(map[string]any); ok {
		typ := strings.ToLower(strings.TrimSpace(stringValue(object["type"])))
		namespace := strings.TrimSpace(stringValue(object["namespace"]))
		name := strings.TrimSpace(stringValue(object["name"]))
		if name == "" {
			if nested, ok := object["function"].(map[string]any); ok {
				name = strings.TrimSpace(stringValue(nested["name"]))
			}
		}
		switch {
		case namespace != "":
			flat := qualifyOpenCodeGoNamespaceToolName(namespace, name)
			if _, exists := names[flat]; exists {
				request.ToolChoice, _ = common.Marshal(map[string]any{"type": "function", "name": flat})
				return
			}
			request.ToolChoice = json.RawMessage(`"auto"`)
		case typ == "namespace" || strings.HasPrefix(typ, "web_search") || typ == "tool_search":
			request.ToolChoice = json.RawMessage(`"auto"`)
		case typ == "custom" && name != "":
			request.ToolChoice, _ = common.Marshal(map[string]any{"type": "function", "name": name})
		case typ == "function" && name != "":
			request.ToolChoice, _ = common.Marshal(map[string]any{"type": "function", "name": name})
		default:
			request.ToolChoice = json.RawMessage(`"auto"`)
		}
	}
}

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

// requestContainsAssistantReasoning reports whether the request carries prior
// assistant reasoning that a thinking-mode upstream provider must replay back.
// Console Go's Responses bridge drops standalone `reasoning` input items and
// does not preserve assistant reasoning on `assistant` output-history items,
// so a request that must pass prior reasoning back stays on the model's
// configured Chat protocol where reasoning_content flows through unchanged on
// the same assistant message as tool_calls.
func requestContainsAssistantReasoning(request any) bool {
	switch typed := request.(type) {
	case *dto.GeneralOpenAIRequest:
		return typed != nil && generalOpenAIRequestContainsAssistantReasoning(typed)
	case dto.GeneralOpenAIRequest:
		return generalOpenAIRequestContainsAssistantReasoning(&typed)
	case *dto.ClaudeRequest:
		return typed != nil && claudeRequestContainsAssistantReasoning(typed)
	case dto.ClaudeRequest:
		return claudeRequestContainsAssistantReasoning(&typed)
	case *dto.OpenAIResponsesRequest:
		return typed != nil && openAIResponsesRequestContainsReasoning(typed)
	case dto.OpenAIResponsesRequest:
		return openAIResponsesRequestContainsReasoning(&typed)
	default:
		return false
	}
}

func generalOpenAIRequestContainsAssistantReasoning(req *dto.GeneralOpenAIRequest) bool {
	for _, msg := range req.Messages {
		if msg.Role != "assistant" {
			continue
		}
		if strings.TrimSpace(msg.GetReasoningContent()) != "" {
			return true
		}
	}
	return false
}

func claudeRequestContainsAssistantReasoning(req *dto.ClaudeRequest) bool {
	for _, msg := range req.Messages {
		if msg.Role != "assistant" {
			continue
		}
		parts, err := msg.ParseContent()
		if err != nil {
			continue
		}
		for _, part := range parts {
			if part.Type == "thinking" && part.Thinking != nil && strings.TrimSpace(*part.Thinking) != "" {
				return true
			}
		}
	}
	return false
}

func openAIResponsesRequestContainsReasoning(req *dto.OpenAIResponsesRequest) bool {
	if len(req.Input) == 0 {
		return false
	}
	var input []map[string]any
	if err := common.Unmarshal(req.Input, &input); err != nil {
		return false
	}
	for _, item := range input {
		if itemType, _ := item["type"].(string); itemType == "reasoning" {
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
