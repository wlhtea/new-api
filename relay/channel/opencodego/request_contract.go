package opencodego

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
)

const (
	RequestContractUnclassifiedPathRule = "request.contract.unclassified-top-level"
	RequestContractUnmappedPathRule     = "request.contract.unmapped-top-level"
	RequestContractTypedPathRule        = "request.contract.typed-top-level"
	RequestContractLocalPathRule        = "request.contract.local-top-level"
	RequestContractMessagesRawPathRule  = "request.contract.messages-raw-top-level"
	RequestContractThinkingBudgetRule   = "request.contract.thinking-budget-model"
	RequestContractKimiTemperatureRule  = "request.contract.kimi-temperature"
	RequestContractPreserveConflictRule = "request.contract.preserve-mutation-conflict"
	RequestContractTargetCollisionRule  = "request.contract.target-path-collision"
	RequestContractUnmappedNestedRule   = "request.contract.unmapped-nested-path"
	RequestContractPublicMessage        = "request contains a field that cannot be safely relayed"
	RequestContractFinalizedMessage     = "finalized OpenCode request contains an unclassified field"
)

// RequestPathLocalObligation is the local-work axis of a request path contract.
// More than one obligation can apply to a field that is also sent upstream.
type RequestPathLocalObligation uint16

const (
	RequestPathObligationValidate RequestPathLocalObligation = 1 << iota
	RequestPathObligationSecurity
	RequestPathObligationBilling
	RequestPathObligationAffinity
	RequestPathObligationResponse
)

const requestPathObligationAll = RequestPathObligationValidate |
	RequestPathObligationSecurity |
	RequestPathObligationBilling |
	RequestPathObligationAffinity |
	RequestPathObligationResponse

func (o RequestPathLocalObligation) Has(obligation RequestPathLocalObligation) bool {
	return obligation != 0 && o&obligation == obligation
}

// RequestPathWireAction is the final-wire axis of a request path contract.
type RequestPathWireAction string

const (
	RequestPathWirePreserve     RequestPathWireAction = "preserve"
	RequestPathWireTranslate    RequestPathWireAction = "translate"
	RequestPathWireForwardRaw   RequestPathWireAction = "forward_raw"
	RequestPathWireConsumeLocal RequestPathWireAction = "consume_local"
	RequestPathWireReject       RequestPathWireAction = "reject"
)

// RequestPathContract describes one decoded top-level source path for one
// client-format/final-protocol pair. SourcePath uses decoded segments rather
// than dotted syntax; this first fail-closed rollout intentionally covers only
// top-level paths.
type RequestPathContract struct {
	RuleID           string
	ClientFormat     types.RelayFormat
	FinalProtocol    Protocol
	SourcePath       []string
	LocalObligations RequestPathLocalObligation
	WireAction       RequestPathWireAction
}

type requestPathContractKey struct {
	clientFormat  types.RelayFormat
	finalProtocol Protocol
	topLevelField string
}

type requestPathProtocolKey struct {
	clientFormat  types.RelayFormat
	finalProtocol Protocol
}

var requestContractProtocols = []Protocol{ProtocolChat, ProtocolMessages, ProtocolResponses}

// This list is deliberately independent from DTO reflection. The completeness
// test fails when a DTO tag is added without a conscious contract decision.
var typedRequestTopLevelFields = map[types.RelayFormat][]string{
	types.RelayFormatClaude: {
		"model", "prompt", "system", "messages", "cache_control", "inference_geo",
		"max_tokens", "max_tokens_to_sample", "stop_sequences", "temperature", "top_p",
		"top_k", "stream", "tools", "context_management", "output_config", "output_format",
		"container", "tool_choice", "thinking", "mcp_servers", "metadata", "speed",
		"service_tier",
	},
	types.RelayFormatOpenAI: {
		"model", "messages", "prompt", "prefix", "suffix", "stream", "stream_options",
		"max_tokens", "max_completion_tokens", "reasoning_effort", "verbosity", "temperature",
		"top_p", "top_k", "stop", "n", "input", "instruction", "size", "functions",
		"frequency_penalty", "presence_penalty", "response_format", "encoding_format", "seed",
		"parallel_tool_calls", "tools", "tool_choice", "function_call", "user", "service_tier",
		"logprobs", "top_logprobs", "dimensions", "modalities", "audio", "safety_identifier",
		"store", "prompt_cache_key", "prompt_cache_retention", "logit_bias", "metadata",
		"prediction", "extra_body", "search_parameters", "web_search_options", "usage",
		"reasoning", "vl_high_resolution_images", "enable_thinking", "thinking_budget",
		"chat_template_kwargs", "enable_search", "think", "web_search", "thinking",
		"search_domain_filter", "search_recency_filter", "return_images",
		"return_related_questions", "search_mode", "reasoning_split",
	},
	types.RelayFormatOpenAIResponses: {
		"model", "input", "include", "conversation", "context_management", "instructions",
		"max_output_tokens", "top_logprobs", "metadata", "moderation", "parallel_tool_calls",
		"frequency_penalty", "presence_penalty", "previous_response_id", "reasoning",
		"service_tier", "store", "prompt_cache_key", "prompt_cache_options",
		"prompt_cache_retention", "safety_identifier", "stream", "stream_options",
		"temperature", "text", "tool_choice", "tools", "top_p", "truncation", "user",
		"max_tool_calls", "prompt", "client_metadata", "enable_thinking", "thinking_budget",
		"preset",
	},
}

// These OpenCode selector aliases are intentionally outside the public DTOs.
// Explicit contract rows keep them from falling through as opaque extensions.
var effortAliasTopLevelFields = map[types.RelayFormat][]string{
	types.RelayFormatClaude:          {"effort", "outputConfig"},
	types.RelayFormatOpenAI:          {"reasoningEffort"},
	types.RelayFormatOpenAIResponses: {"reasoningEffort", "reasoning_effort"},
}

// Cross-protocol rows exist only when the current converter/finalizer performs
// the stated action. Every other typed cross-protocol field receives an
// explicit reject row instead of relying on DTO loss.
var crossProtocolRequestWireActions = map[requestPathProtocolKey]map[string]RequestPathWireAction{
	{clientFormat: types.RelayFormatClaude, finalProtocol: ProtocolChat}: {
		"effort":             RequestPathWireTranslate,
		"outputConfig":       RequestPathWireTranslate,
		"model":              RequestPathWireTranslate,
		"system":             RequestPathWireTranslate,
		"messages":           RequestPathWireTranslate,
		"max_tokens":         RequestPathWireTranslate,
		"stop_sequences":     RequestPathWireTranslate,
		"temperature":        RequestPathWireTranslate,
		"top_p":              RequestPathWireTranslate,
		"top_k":              RequestPathWireTranslate,
		"stream":             RequestPathWireTranslate,
		"tools":              RequestPathWireTranslate,
		"thinking":           RequestPathWireForwardRaw,
		"metadata":           RequestPathWireTranslate,
		"output_config":      RequestPathWireTranslate,
		"context_management": RequestPathWireConsumeLocal,
	},
	{clientFormat: types.RelayFormatClaude, finalProtocol: ProtocolResponses}: {
		"model":       RequestPathWireTranslate,
		"system":      RequestPathWireTranslate,
		"messages":    RequestPathWireTranslate,
		"max_tokens":  RequestPathWireTranslate,
		"temperature": RequestPathWireTranslate,
		"top_p":       RequestPathWireTranslate,
		"stream":      RequestPathWireTranslate,
		"tools":       RequestPathWireTranslate,
	},
	{clientFormat: types.RelayFormatOpenAI, finalProtocol: ProtocolMessages}: {
		"model":                 RequestPathWireTranslate,
		"messages":              RequestPathWireTranslate,
		"stream":                RequestPathWireTranslate,
		"max_tokens":            RequestPathWireTranslate,
		"max_completion_tokens": RequestPathWireTranslate,
		"reasoning_effort":      RequestPathWireTranslate,
		"reasoningEffort":       RequestPathWireTranslate,
		"temperature":           RequestPathWireTranslate,
		"top_p":                 RequestPathWireTranslate,
		"top_k":                 RequestPathWireTranslate,
		"stop":                  RequestPathWireTranslate,
		"parallel_tool_calls":   RequestPathWireTranslate,
		"tools":                 RequestPathWireTranslate,
		"tool_choice":           RequestPathWireTranslate,
		"web_search_options":    RequestPathWireTranslate,
		"reasoning":             RequestPathWireTranslate,
		"prompt_cache_key":      RequestPathWireConsumeLocal,
	},
	{clientFormat: types.RelayFormatOpenAI, finalProtocol: ProtocolResponses}: {
		"model":                 RequestPathWireTranslate,
		"messages":              RequestPathWireTranslate,
		"stream":                RequestPathWireTranslate,
		"max_tokens":            RequestPathWireTranslate,
		"max_completion_tokens": RequestPathWireTranslate,
		"reasoning_effort":      RequestPathWireTranslate,
		"reasoningEffort":       RequestPathWireTranslate,
		"reasoning":             RequestPathWireTranslate,
		"temperature":           RequestPathWireTranslate,
		"top_p":                 RequestPathWireTranslate,
		"n":                     RequestPathWireConsumeLocal,
		"frequency_penalty":     RequestPathWireTranslate,
		"presence_penalty":      RequestPathWireTranslate,
		"response_format":       RequestPathWireTranslate,
		"parallel_tool_calls":   RequestPathWireTranslate,
		"tools":                 RequestPathWireTranslate,
		"tool_choice":           RequestPathWireTranslate,
		"user":                  RequestPathWireTranslate,
		"store":                 RequestPathWireTranslate,
		"prompt_cache_key":      RequestPathWireTranslate,
		"metadata":              RequestPathWireTranslate,
		"enable_thinking":       RequestPathWireTranslate,
		"thinking_budget":       RequestPathWireTranslate,
	},
	{clientFormat: types.RelayFormatOpenAIResponses, finalProtocol: ProtocolChat}: {
		"model":                  RequestPathWireTranslate,
		"input":                  RequestPathWireTranslate,
		"instructions":           RequestPathWireTranslate,
		"max_output_tokens":      RequestPathWireTranslate,
		"top_logprobs":           RequestPathWireTranslate,
		"metadata":               RequestPathWireTranslate,
		"parallel_tool_calls":    RequestPathWireTranslate,
		"frequency_penalty":      RequestPathWireTranslate,
		"presence_penalty":       RequestPathWireTranslate,
		"reasoning":              RequestPathWireTranslate,
		"reasoningEffort":        RequestPathWireTranslate,
		"reasoning_effort":       RequestPathWireTranslate,
		"service_tier":           RequestPathWireTranslate,
		"store":                  RequestPathWireTranslate,
		"prompt_cache_key":       RequestPathWireTranslate,
		"prompt_cache_retention": RequestPathWireTranslate,
		"safety_identifier":      RequestPathWireTranslate,
		"stream":                 RequestPathWireTranslate,
		"stream_options":         RequestPathWireTranslate,
		"temperature":            RequestPathWireTranslate,
		"text":                   RequestPathWireTranslate,
		"tool_choice":            RequestPathWireTranslate,
		"tools":                  RequestPathWireTranslate,
		"top_p":                  RequestPathWireTranslate,
		"user":                   RequestPathWireTranslate,
		"enable_thinking":        RequestPathWireTranslate,
		"thinking_budget":        RequestPathWireTranslate,
	},
	{clientFormat: types.RelayFormatOpenAIResponses, finalProtocol: ProtocolMessages}: {
		"model":               RequestPathWireTranslate,
		"input":               RequestPathWireTranslate,
		"instructions":        RequestPathWireTranslate,
		"max_output_tokens":   RequestPathWireTranslate,
		"parallel_tool_calls": RequestPathWireTranslate,
		"reasoning":           RequestPathWireTranslate,
		"reasoningEffort":     RequestPathWireTranslate,
		"reasoning_effort":    RequestPathWireTranslate,
		"stream":              RequestPathWireTranslate,
		"temperature":         RequestPathWireTranslate,
		"tool_choice":         RequestPathWireTranslate,
		"tools":               RequestPathWireTranslate,
		"top_p":               RequestPathWireTranslate,
		"prompt_cache_key":    RequestPathWireConsumeLocal,
	},
}

var responseObligationFields = map[string]struct{}{
	"stream": {}, "stream_options": {}, "max_tokens": {}, "max_tokens_to_sample": {},
	"max_completion_tokens": {}, "max_output_tokens": {}, "stop_sequences": {}, "stop": {},
	"temperature": {}, "top_p": {}, "top_k": {}, "n": {}, "reasoning_effort": {},
	"verbosity": {}, "frequency_penalty": {}, "presence_penalty": {}, "response_format": {},
	"parallel_tool_calls": {}, "tools": {}, "tool_choice": {}, "thinking": {}, "reasoning": {},
	"logprobs": {}, "top_logprobs": {}, "modalities": {}, "audio": {}, "output_config": {},
	"output_format": {}, "enable_thinking": {}, "thinking_budget": {}, "return_images": {},
	"return_related_questions": {}, "text": {}, "include": {}, "max_tool_calls": {},
	"context_management": {},
}

var affinityObligationFields = map[string]struct{}{
	"metadata": {}, "prompt_cache_key": {}, "user": {}, "client_metadata": {},
}

var requestPathContracts = mustBuildRequestPathContracts()

func LookupRequestPathContract(
	clientFormat types.RelayFormat,
	finalProtocol Protocol,
	topLevelField string,
) (RequestPathContract, bool) {
	contract, found := requestPathContracts[requestPathContractKey{
		clientFormat:  clientFormat,
		finalProtocol: finalProtocol,
		topLevelField: topLevelField,
	}]
	if !found {
		return RequestPathContract{}, false
	}
	contract.SourcePath = append([]string(nil), contract.SourcePath...)
	return contract, true
}

// ValidateRequestPathContracts is a side-effect-free disposition check.
// Unknown top-level fields are relayable extensions unless they collide with a
// known target field. Unknown members inside converter-owned structures remain
// fail-closed. Errors never include a client-controlled field name or value.
func ValidateRequestPathContracts(
	envelope *helper.ValidatedRequestEnvelope,
	clientFormat types.RelayFormat,
	finalProtocol Protocol,
	typedRequest any,
) error {
	if envelope == nil {
		return errors.New("validated request envelope is unavailable for request contract")
	}
	if envelope.Format() != clientFormat || !validRequestContractClientFormat(clientFormat) ||
		!validRequestContractProtocol(finalProtocol) {
		return errors.New("validated request contract routing input is invalid")
	}
	if clientFormat == types.RelayFormatClaude && finalProtocol == ProtocolChat {
		if _, err := ParseClaudeChatProjection(envelope); err != nil {
			return err
		}
	}
	if _, err := validateEnvelopeEffortSelection(envelope, clientFormat, finalProtocol); err != nil {
		return err
	}
	for _, field := range envelope.TopLevelFieldNames() {
		contract, found := LookupRequestPathContract(clientFormat, finalProtocol, field)
		if !found {
			if finalProtocol.RelayFormat() != clientFormat &&
				requestTargetHasKnownTopLevelField(finalProtocol, field) {
				return newRequestPathContractClientError(RequestContractTargetCollisionRule)
			}
			continue
		}
		if contract.WireAction == RequestPathWireReject {
			return newRequestPathContractClientError(RequestContractUnmappedPathRule)
		}
	}
	if finalProtocol.RelayFormat() == clientFormat {
		return nil
	}
	if typedRequest == nil {
		return errors.New("typed request is unavailable for cross-protocol contract validation")
	}
	for _, field := range envelope.TopLevelFieldNames() {
		contract, found := LookupRequestPathContract(clientFormat, finalProtocol, field)
		if !found {
			continue
		}
		if contract.WireAction != RequestPathWireTranslate {
			continue
		}
		if isEffortSelectorAliasTopLevelField(clientFormat, field) {
			continue
		}
		raw, present, err := envelope.RawTopLevelField(field)
		if err != nil {
			return err
		}
		if !present {
			return errors.New("validated request inventory lost a translated field")
		}
		var value any
		if err := common.Unmarshal(raw, &value); err != nil {
			return errors.New("validated translated request value cannot be decoded")
		}
		fieldType, found := requestJSONFieldType(reflect.TypeOf(typedRequest), field)
		if !found {
			return errors.New("typed request field is unavailable for cross-protocol contract validation")
		}
		if !jsonValueFitsDeclaredType(value, fieldType) ||
			!validateConverterOwnedNestedValue(clientFormat, finalProtocol, field, value) {
			return newRequestPathContractClientError(RequestContractUnmappedNestedRule)
		}
	}
	return nil
}

func requestTargetHasKnownTopLevelField(finalProtocol Protocol, field string) bool {
	_, found := LookupRequestPathContract(finalProtocol.RelayFormat(), finalProtocol, field)
	return found
}

func collectClientExtensionFields(
	envelope *helper.ValidatedRequestEnvelope,
	clientFormat types.RelayFormat,
	finalProtocol Protocol,
) (map[string]json.RawMessage, error) {
	if envelope == nil || envelope.Format() != clientFormat {
		return nil, errors.New("validated request envelope is unavailable for client extensions")
	}
	extensions := make(map[string]json.RawMessage)
	for _, field := range envelope.TopLevelFieldNames() {
		if _, found := LookupRequestPathContract(clientFormat, finalProtocol, field); found {
			continue
		}
		if finalProtocol.RelayFormat() != clientFormat && requestTargetHasKnownTopLevelField(finalProtocol, field) {
			return nil, newRequestPathContractClientError(RequestContractTargetCollisionRule)
		}
		raw, present, err := envelope.RawTopLevelField(field)
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, errors.New("validated request inventory lost a client extension")
		}
		extensions[field] = append(json.RawMessage(nil), raw...)
	}
	return extensions, nil
}

func requestJSONFieldType(requestType reflect.Type, field string) (reflect.Type, bool) {
	for requestType != nil && requestType.Kind() == reflect.Pointer {
		requestType = requestType.Elem()
	}
	if requestType == nil || requestType.Kind() != reflect.Struct {
		return nil, false
	}
	for index := 0; index < requestType.NumField(); index++ {
		structField := requestType.Field(index)
		if structField.PkgPath != "" {
			continue
		}
		name := strings.Split(structField.Tag.Get("json"), ",")[0]
		if name == field {
			return structField.Type, true
		}
	}
	return nil, false
}

var rawMessageType = reflect.TypeOf(json.RawMessage{})

// jsonValueFitsDeclaredType rejects members erased by decoding into concrete
// DTO structs. Maps, interfaces, and RawMessage values are intentional opaque
// boundaries and are audited by validateConverterOwnedNestedValue when a
// converter parses them.
func jsonValueFitsDeclaredType(value any, declaredType reflect.Type) bool {
	if value == nil {
		return true
	}
	for declaredType.Kind() == reflect.Pointer {
		declaredType = declaredType.Elem()
	}
	if declaredType == rawMessageType || declaredType.Kind() == reflect.Interface ||
		declaredType.Kind() == reflect.Map {
		return true
	}
	switch declaredType.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return false
		}
		fields := make(map[string]reflect.Type, declaredType.NumField())
		for index := 0; index < declaredType.NumField(); index++ {
			structField := declaredType.Field(index)
			if structField.PkgPath != "" {
				continue
			}
			name := strings.Split(structField.Tag.Get("json"), ",")[0]
			if name != "" && name != "-" {
				fields[name] = structField.Type
			}
		}
		for name, member := range object {
			memberType, found := fields[name]
			if !found || !jsonValueFitsDeclaredType(member, memberType) {
				return false
			}
		}
		return true
	case reflect.Slice, reflect.Array:
		array, ok := value.([]any)
		if !ok {
			return false
		}
		for _, item := range array {
			if !jsonValueFitsDeclaredType(item, declaredType.Elem()) {
				return false
			}
		}
		return true
	default:
		return true
	}
}

var (
	chatMessageKeys = requestContractKeySet(
		"role", "content", "name", "prefix", "reasoning_content", "reasoning", "tool_calls", "tool_call_id",
	)
	chatToolCallKeys       = requestContractKeySet("id", "type", "function")
	chatToolDefinitionKeys = requestContractKeySet("id", "type", "function", "custom")
	chatCallFunctionKeys   = requestContractKeySet("name", "arguments")
	chatToolFunctionKeys   = requestContractKeySet("description", "name", "parameters")
	chatToolChoiceKeys     = requestContractKeySet("type", "name", "function")
	chatFunctionNameKeys   = requestContractKeySet("name")
	chatReasoningKeys      = requestContractKeySet("max_tokens", "effort")
	chatLocationKeys       = requestContractKeySet("approximate")
	chatApproximateKeys    = requestContractKeySet("timezone", "country", "region", "city")

	claudeMessageKeys = requestContractKeySet("role", "content")
	claudeSourceKeys  = requestContractKeySet("type", "media_type", "data")
	claudeToolKeys    = requestContractKeySet("name", "description", "input_schema")

	responsesMessageItemKeys  = requestContractKeySet("type", "role", "content")
	responsesReasoningKeys    = requestContractKeySet("type", "id", "status", "summary", "content")
	responsesFunctionToolKeys = requestContractKeySet(
		"type", "name", "description", "parameters",
	)
	responsesCustomToolKeys      = requestContractKeySet("type", "name", "description")
	responsesNamespaceToolKeys   = requestContractKeySet("type", "name", "tools", "children")
	responsesToolChoiceKeys      = requestContractKeySet("type", "name")
	responsesNamespaceChoiceKeys = requestContractKeySet("type", "name", "namespace", "function")
	responsesTextKeys            = requestContractKeySet("format")
	responsesTextTypeKeys        = requestContractKeySet("type")
	responsesTopReasoningKeys    = requestContractKeySet("effort")
)

func validateConverterOwnedNestedValue(
	clientFormat types.RelayFormat,
	finalProtocol Protocol,
	field string,
	value any,
) bool {
	switch clientFormat {
	case types.RelayFormatOpenAI:
		switch field {
		case "messages":
			return validateChatMessages(value, finalProtocol)
		case "tools":
			return validateChatTools(value, finalProtocol)
		case "tool_choice":
			return validateChatToolChoice(value, finalProtocol)
		case "reasoning":
			return value == nil || requestContractObjectHasOnlyKeys(value, chatReasoningKeys)
		case "web_search_options":
			return validateChatWebSearchOptions(value)
		}
	case types.RelayFormatClaude:
		switch field {
		case "system":
			return validateClaudeSystem(value)
		case "messages":
			return validateClaudeMessages(value, finalProtocol)
		case "tools":
			return validateClaudeTools(value)
		}
	case types.RelayFormatOpenAIResponses:
		switch field {
		case "input":
			return validateResponsesInput(value, finalProtocol)
		case "tools":
			return validateResponsesTools(value, finalProtocol)
		case "tool_choice":
			return validateResponsesToolChoice(value, finalProtocol)
		case "text":
			return validateResponsesText(value)
		case "reasoning":
			return value == nil || requestContractObjectHasOnlyKeys(value, responsesTopReasoningKeys)
		}
	}
	return true
}

func validateChatMessages(value any, finalProtocol Protocol) bool {
	if value == nil {
		return true
	}
	messages, ok := value.([]any)
	if !ok {
		return false
	}
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			return false
		}
		role, _ := message["role"].(string)
		allowed := requestContractKeySet("role", "content")
		switch finalProtocol {
		case ProtocolMessages:
			switch role {
			case "assistant":
				allowed["tool_calls"] = struct{}{}
			case "tool":
				allowed["tool_call_id"] = struct{}{}
			}
		case ProtocolResponses:
			switch role {
			case "assistant":
				allowed["tool_calls"] = struct{}{}
				allowed["reasoning_content"] = struct{}{}
				allowed["reasoning"] = struct{}{}
				_, reasoningContent := message["reasoning_content"]
				_, reasoning := message["reasoning"]
				if reasoningContent && reasoning {
					return false
				}
			case "tool", "function":
				allowed["tool_call_id"] = struct{}{}
			}
		}
		if !requestContractMapHasOnlyKeys(message, allowed) ||
			!validateChatContent(message["content"], finalProtocol, role) {
			return false
		}
		if toolCalls, present := message["tool_calls"]; present && !validateChatToolCalls(toolCalls, finalProtocol) {
			return false
		}
	}
	return true
}

func validateChatContent(value any, finalProtocol Protocol, role string) bool {
	if value == nil {
		return true
	}
	if role == "tool" || role == "function" {
		return true
	}
	if _, ok := value.(string); ok {
		return true
	}
	parts, ok := value.([]any)
	if !ok {
		return false
	}
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			return false
		}
		var allowed map[string]struct{}
		switch part["type"] {
		case "text":
			allowed = requestContractKeySet("type", "text")
		case "image_url":
			if role == "system" || role == "developer" || !validateChatImageURL(part["image_url"]) {
				return false
			}
			allowed = requestContractKeySet("type", "image_url")
		case "input_audio":
			if role == "system" || role == "developer" ||
				!requestContractObjectHasOnlyKeys(part["input_audio"], requestContractKeySet("data", "format")) {
				return false
			}
			allowed = requestContractKeySet("type", "input_audio")
		case "file":
			if role == "system" || role == "developer" || finalProtocol == ProtocolMessages ||
				!validateChatFile(part["file"]) {
				return false
			}
			allowed = requestContractKeySet("type", "file")
		case "video_url":
			if role == "system" || role == "developer" {
				return false
			}
			if _, ok := part["video_url"].(string); !ok {
				return false
			}
			allowed = requestContractKeySet("type", "video_url")
		default:
			return false
		}
		if !requestContractMapHasOnlyKeys(part, allowed) {
			return false
		}
	}
	return true
}

func validateChatImageURL(value any) bool {
	if _, ok := value.(string); ok {
		return true
	}
	return requestContractObjectHasOnlyKeys(value, requestContractKeySet("url"))
}

func validateChatFile(value any) bool {
	file, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if !requestContractMapHasOnlyKeys(file, requestContractKeySet("file_id", "filename", "file_data")) {
		return false
	}
	_, fileID := file["file_id"]
	_, filename := file["filename"]
	_, fileData := file["file_data"]
	if fileID {
		return !filename && !fileData
	}
	return filename && fileData
}

func validateChatToolCalls(value any, finalProtocol Protocol) bool {
	if value == nil {
		return true
	}
	toolCalls, ok := value.([]any)
	if !ok {
		return false
	}
	for _, rawToolCall := range toolCalls {
		toolCall, ok := rawToolCall.(map[string]any)
		if !ok || !requestContractMapHasOnlyKeys(toolCall, chatToolCallKeys) {
			return false
		}
		toolType, _ := toolCall["type"].(string)
		if toolType != "" && toolType != "function" {
			return false
		}
		if function, present := toolCall["function"]; present &&
			!requestContractObjectHasOnlyKeys(function, chatCallFunctionKeys) {
			return false
		}
		if finalProtocol == ProtocolMessages {
			function, _ := toolCall["function"].(map[string]any)
			if arguments, present := function["arguments"]; present {
				argumentsString, ok := arguments.(string)
				if !ok {
					return false
				}
				var input map[string]any
				if err := common.Unmarshal([]byte(argumentsString), &input); err != nil || input == nil {
					return false
				}
			}
		}
	}
	return true
}

func validateChatTools(value any, finalProtocol Protocol) bool {
	if value == nil {
		return true
	}
	tools, ok := value.([]any)
	if !ok {
		return false
	}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			return false
		}
		toolType, _ := tool["type"].(string)
		if toolType == "function" {
			if !requestContractMapHasOnlyKeys(tool, requestContractKeySet("type", "function")) {
				return false
			}
			if function, present := tool["function"]; !present ||
				!requestContractObjectHasOnlyKeys(function, chatToolFunctionKeys) {
				return false
			}
			continue
		}
		if finalProtocol == ProtocolMessages ||
			!requestContractMapHasOnlyKeys(tool, chatToolDefinitionKeys) {
			return false
		}
		if function, present := tool["function"]; present &&
			!requestContractObjectHasOnlyKeys(function, requestContractKeySet("description", "name", "parameters", "arguments")) {
			return false
		}
	}
	return true
}

func validateChatToolChoice(value any, finalProtocol Protocol) bool {
	if value == nil {
		return true
	}
	if choice, ok := value.(string); ok {
		return finalProtocol != ProtocolMessages || choice == "auto" || choice == "required" || choice == "none"
	}
	choice, ok := value.(map[string]any)
	if !ok {
		return false
	}
	choiceType, _ := choice["type"].(string)
	if finalProtocol == ProtocolResponses && choiceType != "function" {
		return true
	}
	if !requestContractMapHasOnlyKeys(choice, chatToolChoiceKeys) {
		return false
	}
	if function, present := choice["function"]; present &&
		!requestContractObjectHasOnlyKeys(function, chatFunctionNameKeys) {
		return false
	}
	if choiceType == "function" {
		_, topName := choice["name"]
		_, nestedName := choice["function"]
		if topName == nestedName {
			return false
		}
	}
	return true
}

func validateChatWebSearchOptions(value any) bool {
	if value == nil {
		return true
	}
	options, ok := value.(map[string]any)
	if !ok {
		return false
	}
	location, present := options["user_location"]
	if !present || location == nil {
		return true
	}
	locationObject, ok := location.(map[string]any)
	if !ok || !requestContractMapHasOnlyKeys(locationObject, chatLocationKeys) {
		return false
	}
	if approximate, present := locationObject["approximate"]; present {
		return requestContractObjectHasOnlyKeys(approximate, chatApproximateKeys)
	}
	return true
}

func validateClaudeSystem(value any) bool {
	if value == nil {
		return true
	}
	if _, ok := value.(string); ok {
		return true
	}
	parts, ok := value.([]any)
	if !ok {
		return false
	}
	for _, part := range parts {
		object, ok := part.(map[string]any)
		if !ok || (object["type"] != "text" && object["type"] != "input_text") ||
			!requestContractMapHasOnlyKeys(object, requestContractKeySet("type", "text")) {
			return false
		}
	}
	return true
}

func validateClaudeMessages(value any, finalProtocol Protocol) bool {
	if value == nil {
		return true
	}
	messages, ok := value.([]any)
	if !ok {
		return false
	}
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok || !requestContractMapHasOnlyKeys(message, claudeMessageKeys) ||
			!validateClaudeContent(message["content"], finalProtocol) {
			return false
		}
	}
	return true
}

func validateClaudeContent(value any, finalProtocol Protocol) bool {
	if value == nil {
		return true
	}
	if _, ok := value.(string); ok {
		return true
	}
	parts, ok := value.([]any)
	if !ok {
		return false
	}
	hasToolUse := false
	hasRegularContent := false
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			return false
		}
		var allowed map[string]struct{}
		switch part["type"] {
		case "text", "input_text":
			hasRegularContent = true
			allowed = requestContractKeySet("type", "text")
			if finalProtocol == ProtocolChat {
				allowed["cache_control"] = struct{}{}
			}
		case "image":
			hasRegularContent = true
			allowed = requestContractKeySet("type", "source")
			if source, present := part["source"]; !present ||
				!requestContractObjectHasOnlyKeys(source, claudeSourceKeys) {
				return false
			}
		case "tool_use":
			hasToolUse = true
			allowed = requestContractKeySet("type", "id", "name", "input")
		case "tool_result":
			allowed = requestContractKeySet("type", "tool_use_id", "content")
			if finalProtocol == ProtocolChat {
				allowed["name"] = struct{}{}
			}
			if content, present := part["content"]; present && content != nil {
				if _, ok := content.(string); !ok {
					return false
				}
			}
		case "thinking":
			allowed = requestContractKeySet("type", "thinking")
		default:
			return false
		}
		if !requestContractMapHasOnlyKeys(part, allowed) {
			return false
		}
	}
	return !hasToolUse || !hasRegularContent
}

func validateClaudeTools(value any) bool {
	if value == nil {
		return true
	}
	tools, ok := value.([]any)
	if !ok {
		return false
	}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok || !requestContractMapHasOnlyKeys(tool, claudeToolKeys) {
			return false
		}
	}
	return true
}

func validateResponsesInput(value any, finalProtocol Protocol) bool {
	if value == nil {
		return true
	}
	if _, ok := value.(string); ok {
		return true
	}
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			return false
		}
		itemType, _ := item["type"].(string)
		switch itemType {
		case "", "message":
			if !requestContractMapHasOnlyKeys(item, responsesMessageItemKeys) ||
				!validateResponsesContent(item["content"], finalProtocol) {
				return false
			}
		case "reasoning":
			if finalProtocol == ProtocolMessages ||
				!requestContractMapHasOnlyKeys(item, responsesReasoningKeys) ||
				!validateResponsesReasoningParts(item["summary"]) ||
				!validateResponsesReasoningParts(item["content"]) {
				return false
			}
			id, idPresent := item["id"].(string)
			_, summaryPresent := item["summary"]
			if !idPresent || strings.TrimSpace(id) == "" || !summaryPresent {
				return false
			}
			if status, present := item["status"]; present && status != "completed" {
				return false
			}
		case "function_call":
			if !validateResponsesCallItem(
				item,
				requestContractKeySet("type", "id", "call_id", "name", "arguments"),
				true,
			) {
				return false
			}
		case "function_call_output":
			allowed := requestContractKeySet("type", "call_id", "output")
			allowID := false
			if finalProtocol == ProtocolMessages {
				allowed["id"] = struct{}{}
				allowID = true
			}
			if !validateResponsesCallItem(item, allowed, allowID) {
				return false
			}
		case "custom_tool_call":
			if !validateResponsesCallItem(
				item,
				requestContractKeySet("type", "id", "call_id", "name", "input"),
				true,
			) {
				return false
			}
		case "custom_tool_call_output":
			if !validateResponsesCallItem(
				item,
				requestContractKeySet("type", "id", "call_id", "output"),
				true,
			) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validateResponsesCallItem(
	item map[string]any,
	allowed map[string]struct{},
	allowID bool,
) bool {
	if !requestContractMapHasOnlyKeys(item, allowed) {
		return false
	}
	_, idPresent := item["id"]
	_, callIDPresent := item["call_id"]
	if idPresent && (!allowID || callIDPresent) {
		return false
	}
	return true
}

func validateResponsesContent(value any, finalProtocol Protocol) bool {
	if value == nil {
		return true
	}
	if _, ok := value.(string); ok {
		return true
	}
	parts, ok := value.([]any)
	if !ok {
		return false
	}
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			return false
		}
		var allowed map[string]struct{}
		switch part["type"] {
		case "input_text", "output_text", "text":
			allowed = requestContractKeySet("type", "text")
		case "refusal":
			allowed = requestContractKeySet("type", "refusal")
		case "input_image":
			if finalProtocol == ProtocolChat {
				if _, present := part["image_url"]; present {
					allowed = requestContractKeySet("type", "image_url")
				} else {
					allowed = requestContractKeySet("type", "url", "file_id", "detail")
				}
			} else {
				allowed = requestContractKeySet("type", "mime_type", "image_url", "url")
				if !validateResponsesMediaMembers(part, "image_url", false) {
					return false
				}
			}
		case "input_file":
			if finalProtocol == ProtocolChat {
				if _, present := part["file"]; present {
					allowed = requestContractKeySet("type", "file")
				} else {
					allowed = requestContractKeySet("type", "file_id", "file_data", "file_url", "filename")
				}
			} else {
				allowed = requestContractKeySet("type", "mime_type", "file", "file_data", "file_url", "url")
				if !validateResponsesMediaMembers(part, "file", false) {
					return false
				}
			}
		case "input_audio":
			if finalProtocol == ProtocolChat {
				if _, present := part["input_audio"]; present {
					allowed = requestContractKeySet("type", "input_audio")
				} else {
					continue
				}
			} else {
				allowed = requestContractKeySet("type", "mime_type", "input_audio", "data", "url")
				if !validateResponsesMediaMembers(part, "input_audio", true) {
					return false
				}
			}
		case "input_video":
			if finalProtocol == ProtocolChat {
				if video, present := part["video_url"]; present {
					if object, ok := video.(map[string]any); ok &&
						!requestContractMapHasOnlyKeys(object, requestContractKeySet("url")) {
						return false
					}
					allowed = requestContractKeySet("type", "video_url")
				} else if _, present := part["url"]; present {
					allowed = requestContractKeySet("type", "url")
				} else {
					continue
				}
			} else {
				allowed = requestContractKeySet("type", "mime_type", "video_url", "url")
				if !validateResponsesMediaMembers(part, "video_url", false) {
					return false
				}
			}
		default:
			if finalProtocol == ProtocolChat {
				continue
			}
			return false
		}
		if !requestContractMapHasOnlyKeys(part, allowed) {
			return false
		}
	}
	return true
}

func validateResponsesMediaMembers(part map[string]any, nestedKey string, allowFormat bool) bool {
	nested, present := part[nestedKey]
	if !present || nested == nil {
		return true
	}
	if _, ok := nested.(string); ok {
		return true
	}
	allowed := requestContractKeySet("mime_type", "url", "file_data", "file_url", "data")
	if allowFormat {
		allowed["format"] = struct{}{}
	}
	return requestContractObjectHasOnlyKeys(nested, allowed)
}

func validateResponsesReasoningParts(value any) bool {
	if value == nil {
		return true
	}
	parts, ok := value.([]any)
	if !ok {
		return false
	}
	allowed := requestContractKeySet("type", "text")
	for _, part := range parts {
		if !requestContractObjectHasOnlyKeys(part, allowed) {
			return false
		}
	}
	return true
}

func validateResponsesTools(value any, finalProtocol Protocol) bool {
	if value == nil {
		return true
	}
	tools, ok := value.([]any)
	if !ok {
		return false
	}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			return false
		}
		toolType, _ := tool["type"].(string)
		switch toolType {
		case "function":
			if !requestContractMapHasOnlyKeys(tool, responsesFunctionToolKeys) {
				return false
			}
		case "custom":
			if !requestContractMapHasOnlyKeys(tool, responsesCustomToolKeys) {
				return false
			}
		case "namespace":
			if !validateResponsesNamespaceTool(tool) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validateResponsesNamespaceTool(tool map[string]any) bool {
	if !requestContractMapHasOnlyKeys(tool, responsesNamespaceToolKeys) {
		return false
	}
	childrenValue, toolsPresent := tool["tools"]
	if !toolsPresent {
		childrenValue = tool["children"]
	}
	_, childrenPresent := tool["children"]
	if toolsPresent == childrenPresent {
		return false
	}
	children, ok := childrenValue.([]any)
	if !ok || len(children) == 0 {
		return false
	}
	for _, rawChild := range children {
		child, ok := rawChild.(map[string]any)
		if !ok {
			return false
		}
		switch child["type"] {
		case "function":
			if !requestContractMapHasOnlyKeys(child, responsesFunctionToolKeys) {
				return false
			}
		case "namespace":
			if !validateResponsesNamespaceTool(child) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validateResponsesToolChoice(value any, finalProtocol Protocol) bool {
	if value == nil {
		return true
	}
	if choice, ok := value.(string); ok {
		return finalProtocol != ProtocolMessages || choice == "auto" || choice == "required" || choice == "none"
	}
	choice, ok := value.(map[string]any)
	if !ok {
		return false
	}
	choiceType, _ := choice["type"].(string)
	switch {
	case choiceType == "function" || choiceType == "custom":
		return requestContractMapHasOnlyKeys(choice, responsesToolChoiceKeys)
	case choiceType == "namespace":
		if !requestContractMapHasOnlyKeys(choice, responsesNamespaceChoiceKeys) {
			return false
		}
		if function, present := choice["function"]; present &&
			!requestContractObjectHasOnlyKeys(function, chatFunctionNameKeys) {
			return false
		}
		return true
	case strings.HasPrefix(choiceType, "web_search") || choiceType == "tool_search":
		return requestContractMapHasOnlyKeys(choice, requestContractKeySet("type"))
	default:
		return false
	}
}

func validateResponsesText(value any) bool {
	if value == nil {
		return true
	}
	text, ok := value.(map[string]any)
	if !ok || !requestContractMapHasOnlyKeys(text, responsesTextKeys) {
		return false
	}
	format, present := text["format"]
	if !present || format == nil {
		return true
	}
	formatObject, ok := format.(map[string]any)
	if !ok {
		return false
	}
	if formatObject["type"] == "json_schema" {
		return true
	}
	return requestContractMapHasOnlyKeys(formatObject, responsesTextTypeKeys)
}

func requestContractKeySet(keys ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		set[key] = struct{}{}
	}
	return set
}

func requestContractObjectHasOnlyKeys(value any, allowed map[string]struct{}) bool {
	object, ok := value.(map[string]any)
	return ok && requestContractMapHasOnlyKeys(object, allowed)
}

func requestContractMapHasOnlyKeys(object map[string]any, allowed map[string]struct{}) bool {
	for key := range object {
		if _, found := allowed[key]; !found {
			return false
		}
	}
	return true
}

// ValidateRequestModelFieldContracts turns historical serializer deletions
// into explicit preflight decisions. A client field must never disappear only
// because a model-specific MarshalJSON branch happened to omit it.
func ValidateRequestModelFieldContracts(
	envelope *helper.ValidatedRequestEnvelope,
	finalModel string,
) error {
	if envelope == nil {
		return errors.New("validated request envelope is unavailable for model field contract")
	}
	if _, present := envelope.TopLevelKind("thinking_budget"); present &&
		!dto.IsQwenThinkingBudgetModel(finalModel) {
		return newRequestPathContractClientError(RequestContractThinkingBudgetRule)
	}
	if _, present := envelope.TopLevelKind("temperature"); present &&
		strings.EqualFold(strings.TrimSpace(finalModel), "kimi-k2.7-code") {
		raw, _, err := envelope.RawTopLevelField("temperature")
		if err != nil {
			return err
		}
		var value *float64
		if err := common.Unmarshal(raw, &value); err != nil {
			return errors.New("validated Kimi temperature cannot be decoded")
		}
		if value != nil && *value != 1 {
			return newRequestPathContractClientError(RequestContractKimiTemperatureRule)
		}
	}
	return nil
}

// ValidateFinalizedRequestPathContracts applies the target protocol's typed
// top-level contract to the post-override candidate. It is intentionally run
// before reservation so an operator override cannot inject an unclassified
// output or cost control after ingress validation.
func ValidateFinalizedRequestPathContracts(jsonData []byte, finalProtocol Protocol) error {
	return validateFinalizedRequestPathContracts(jsonData, finalProtocol, nil)
}

func validateFinalizedRequestPathContracts(
	jsonData []byte,
	finalProtocol Protocol,
	clientExtensions map[string]json.RawMessage,
) error {
	if !validRequestContractProtocol(finalProtocol) {
		return errors.New("finalized request contract protocol is invalid")
	}
	var fields map[string]json.RawMessage
	if err := common.Unmarshal(jsonData, &fields); err != nil || fields == nil {
		return errors.New("finalized OpenCode request is not a JSON object")
	}
	clientFormat := finalProtocol.RelayFormat()
	seenExtensions := make(map[string]struct{}, len(clientExtensions))
	for field, actual := range fields {
		contract, found := LookupRequestPathContract(clientFormat, finalProtocol, field)
		if found {
			if contract.WireAction == RequestPathWireReject {
				return errors.New(RequestContractFinalizedMessage)
			}
			continue
		}
		expected, allowed := clientExtensions[field]
		if !allowed {
			return errors.New(RequestContractFinalizedMessage)
		}
		if !bytes.Equal(expected, actual) {
			return errors.New(RequestContractFinalizedMessage)
		}
		seenExtensions[field] = struct{}{}
	}
	if len(seenExtensions) != len(clientExtensions) {
		return errors.New(RequestContractFinalizedMessage)
	}
	return nil
}

func newRequestPathContractClientError(ruleID string) error {
	return &helper.ClientRequestValidationError{
		StatusCode: http.StatusBadRequest,
		Message:    RequestContractPublicMessage,
		RuleID:     ruleID,
		StageID:    RequestContractPreflightStage,
	}
}

func mustBuildRequestPathContracts() map[requestPathContractKey]RequestPathContract {
	contracts, err := buildRequestPathContracts()
	if err != nil {
		panic(err)
	}
	return contracts
}

func buildRequestPathContracts() (map[requestPathContractKey]RequestPathContract, error) {
	contracts := make(map[requestPathContractKey]RequestPathContract)
	for clientFormat, fields := range typedRequestTopLevelFields {
		seen := make(map[string]struct{}, len(fields))
		for _, field := range fields {
			if field == "" {
				return nil, errors.New("request contract contains an empty typed field")
			}
			if _, duplicate := seen[field]; duplicate {
				return nil, fmt.Errorf("request contract contains duplicate typed field %q", field)
			}
			seen[field] = struct{}{}
			for _, finalProtocol := range requestContractProtocols {
				action := typedRequestWireAction(clientFormat, finalProtocol, field)
				ruleID := RequestContractTypedPathRule
				if action == RequestPathWireReject {
					ruleID = RequestContractUnmappedPathRule
				} else if action == RequestPathWireConsumeLocal {
					ruleID = RequestContractLocalPathRule
				}
				if err := addRequestPathContract(contracts, RequestPathContract{
					RuleID:           ruleID,
					ClientFormat:     clientFormat,
					FinalProtocol:    finalProtocol,
					SourcePath:       []string{field},
					LocalObligations: requestPathLocalObligations(field),
					WireAction:       action,
				}); err != nil {
					return nil, err
				}
			}
		}
	}
	for clientFormat, fields := range effortAliasTopLevelFields {
		for _, field := range fields {
			if field == "" {
				return nil, errors.New("request contract contains an empty effort alias")
			}
			for _, finalProtocol := range requestContractProtocols {
				action := typedRequestWireAction(clientFormat, finalProtocol, field)
				ruleID := RequestContractTypedPathRule
				if action == RequestPathWireReject {
					ruleID = RequestContractUnmappedPathRule
				}
				if err := addRequestPathContract(contracts, RequestPathContract{
					RuleID:           ruleID,
					ClientFormat:     clientFormat,
					FinalProtocol:    finalProtocol,
					SourcePath:       []string{field},
					LocalObligations: requestPathLocalObligations(field),
					WireAction:       action,
				}); err != nil {
					return nil, err
				}
			}
		}
	}

	for _, finalProtocol := range requestContractProtocols {
		action := RequestPathWireReject
		if finalProtocol == ProtocolChat {
			action = RequestPathWireForwardRaw
		}
		ruleID := RequestContractUnmappedPathRule
		if action != RequestPathWireReject {
			ruleID = RequestContractMessagesRawPathRule
		}
		if err := addRequestPathContract(contracts, RequestPathContract{
			RuleID:           ruleID,
			ClientFormat:     types.RelayFormatClaude,
			FinalProtocol:    finalProtocol,
			SourcePath:       []string{"stop"},
			LocalObligations: requestPathLocalObligations("stop"),
			WireAction:       action,
		}); err != nil {
			return nil, err
		}
	}
	return contracts, nil
}

func addRequestPathContract(
	contracts map[requestPathContractKey]RequestPathContract,
	contract RequestPathContract,
) error {
	if !validRequestContractClientFormat(contract.ClientFormat) ||
		!validRequestContractProtocol(contract.FinalProtocol) ||
		len(contract.SourcePath) != 1 || contract.SourcePath[0] == "" ||
		contract.RuleID == "" || contract.LocalObligations&^requestPathObligationAll != 0 ||
		!validRequestPathWireAction(contract.WireAction) {
		return errors.New("request path contract row is invalid")
	}
	key := requestPathContractKey{
		clientFormat:  contract.ClientFormat,
		finalProtocol: contract.FinalProtocol,
		topLevelField: contract.SourcePath[0],
	}
	if _, duplicate := contracts[key]; duplicate {
		return fmt.Errorf("request path contract row is duplicated for %q", contract.SourcePath[0])
	}
	contract.SourcePath = append([]string(nil), contract.SourcePath...)
	contracts[key] = contract
	return nil
}

func typedRequestWireAction(
	clientFormat types.RelayFormat,
	finalProtocol Protocol,
	field string,
) RequestPathWireAction {
	if field == "model" || field == "stream" {
		return RequestPathWireTranslate
	}
	if clientFormat == types.RelayFormatOpenAIResponses && field == "prompt_cache_key" {
		if finalProtocol == ProtocolMessages {
			return RequestPathWireConsumeLocal
		}
		return RequestPathWireTranslate
	}
	if finalProtocol.RelayFormat() == clientFormat {
		return RequestPathWirePreserve
	}
	actions := crossProtocolRequestWireActions[requestPathProtocolKey{
		clientFormat:  clientFormat,
		finalProtocol: finalProtocol,
	}]
	if action, found := actions[field]; found {
		return action
	}
	return RequestPathWireReject
}

func requestPathLocalObligations(field string) RequestPathLocalObligation {
	if field == "model" {
		return RequestPathObligationValidate
	}
	obligations := RequestPathObligationValidate |
		RequestPathObligationSecurity |
		RequestPathObligationBilling
	if _, found := responseObligationFields[field]; found {
		obligations |= RequestPathObligationResponse
	}
	if _, found := affinityObligationFields[field]; found {
		obligations |= RequestPathObligationAffinity
	}
	return obligations
}

func validRequestContractClientFormat(format types.RelayFormat) bool {
	switch format {
	case types.RelayFormatClaude, types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses:
		return true
	default:
		return false
	}
}

func validRequestContractProtocol(protocol Protocol) bool {
	return protocol == ProtocolChat || protocol == ProtocolMessages || protocol == ProtocolResponses
}

func validRequestPathWireAction(action RequestPathWireAction) bool {
	switch action {
	case RequestPathWirePreserve, RequestPathWireTranslate, RequestPathWireForwardRaw,
		RequestPathWireConsumeLocal, RequestPathWireReject:
		return true
	default:
		return false
	}
}
