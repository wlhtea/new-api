package opencodego

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	MessagesStopSourceCollisionRule = "request.messages.stop-source-collision"
	RequestContractPreflightStage   = "preflight.contract"
)

type outboundGatewayFields struct {
	model         string
	stream        bool
	streamPresent bool
}

type protectedOutboundField struct {
	path    []string
	raw     json.RawMessage
	present bool
}

// ValidateMessagesStopSourceCollision rejects two source fields that both map
// to Chat's `stop`. Presence, including an explicit null, is what conflicts.
func ValidateMessagesStopSourceCollision(
	envelope *helper.ValidatedRequestEnvelope,
	clientFormat types.RelayFormat,
) error {
	if clientFormat != types.RelayFormatClaude {
		return nil
	}
	if envelope == nil {
		return errors.New("validated Messages request envelope is unavailable")
	}
	_, stopSequencesPresent := envelope.TopLevelKind("stop_sequences")
	_, stopPresent := envelope.TopLevelKind("stop")
	if !stopSequencesPresent || !stopPresent {
		return nil
	}
	return &helper.ClientRequestValidationError{
		StatusCode: http.StatusBadRequest,
		Message:    "Messages stop_sequences and stop cannot both be provided",
		RuleID:     MessagesStopSourceCollisionRule,
		StageID:    RequestContractPreflightStage,
	}
}

func (a *Adaptor) FinalizeOutboundRequest(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	convertedRequest any,
) ([]byte, error) {
	if a == nil || !a.converted {
		return nil, errors.New("OpenCode outbound request was not converted")
	}
	return finalizeOutboundRequest(c, info, convertedRequest)
}

func finalizeOutboundRequest(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	convertedRequest any,
) ([]byte, error) {
	if c == nil || info == nil || !constant.IsOpenCodeChannelType(info.GetChannelType()) {
		return nil, errors.New("OpenCode outbound finalizer input is invalid")
	}
	jsonData, err := common.Marshal(convertedRequest)
	if err != nil {
		return nil, fmt.Errorf("marshal converted OpenCode request: %w", err)
	}
	finalProtocol, err := protocolForRelayFormat(info.GetFinalRequestRelayFormat())
	if err != nil {
		return nil, err
	}
	envelope, found, err := helper.GetValidatedRequestEnvelope(c, info.RelayFormat)
	if err != nil {
		return nil, err
	}
	if !found || envelope == nil {
		return nil, errors.New("validated OpenCode request envelope is unavailable")
	}
	if err := ValidateMessagesStopSourceCollision(envelope, info.RelayFormat); err != nil {
		return nil, err
	}
	if err := ValidateRequestModelFieldContracts(envelope, info.UpstreamModelName); err != nil {
		return nil, err
	}
	originalRequest, err := helper.GetAndValidateRequest(c, info.RelayFormat)
	if err != nil {
		return nil, err
	}
	if err := ValidateRequestPathContracts(envelope, info.RelayFormat, finalProtocol, originalRequest); err != nil {
		return nil, err
	}
	clientExtensions, err := collectClientExtensionFields(envelope, info.RelayFormat, finalProtocol)
	if err != nil {
		return nil, err
	}
	gatewayFields, err := captureOutboundGatewayFields(jsonData, info.UpstreamModelName, envelope)
	if err != nil {
		return nil, err
	}

	jsonData, err = mergeSameProtocolPreservedFields(c, info, envelope, finalProtocol, jsonData)
	if err != nil {
		return nil, err
	}
	jsonData, err = mergeClientExtensionFields(
		envelope,
		info.RelayFormat,
		finalProtocol,
		clientExtensions,
		jsonData,
	)
	if err != nil {
		return nil, err
	}
	jsonData, err = mergeCrossProtocolSameWireFields(
		envelope,
		info.RelayFormat,
		finalProtocol,
		jsonData,
	)
	if err != nil {
		return nil, err
	}
	jsonData, err = mergeCrossProtocolOpaqueFields(
		envelope,
		info.RelayFormat,
		finalProtocol,
		jsonData,
	)
	if err != nil {
		return nil, err
	}
	jsonData, err = mergeMessagesRawFields(c, info, jsonData)
	if err != nil {
		return nil, err
	}
	jsonData, projection, err := applyClaudeChatProjection(envelope, info.RelayFormat, finalProtocol, jsonData)
	if err != nil {
		return nil, err
	}
	jsonData, wireEffort, err := applyEffortProjection(envelope, info.RelayFormat, finalProtocol, jsonData)
	if err != nil {
		return nil, err
	}
	if metadata := gjson.GetBytes(jsonData, "metadata"); metadata.Exists() && finalProtocol == ProtocolChat {
		if err := validateClientChatMetadata(json.RawMessage(metadata.Raw)); err != nil {
			return nil, err
		}
	}
	protectedFields, err := captureProtectedOutboundFields(
		envelope, info.RelayFormat, finalProtocol, projection, wireEffort, jsonData,
	)
	if err != nil {
		return nil, err
	}
	if len(info.ParamOverride) > 0 {
		jsonData, err = relaycommon.ApplyParamOverrideWithRelayInfo(jsonData, info)
		if err != nil {
			return nil, channel.NewOutboundParamOverrideError(err)
		}
	}
	jsonData, err = reassertOutboundGatewayFields(jsonData, gatewayFields)
	if err != nil {
		return nil, err
	}
	jsonData, err = removeDisabledFieldsExact(jsonData, info)
	if err != nil {
		return nil, err
	}
	if err := assertProtectedOutboundFields(jsonData, protectedFields); err != nil {
		return nil, err
	}
	finalEffort, err := validateFinalOutboundRequest(
		jsonData,
		info,
		gatewayFields,
		finalProtocol,
		clientExtensions,
		wireEffort,
	)
	if err != nil {
		return nil, err
	}
	info.SetReasoningEffort("")
	if finalEffort.Present && !finalEffort.Null {
		info.SetReasoningEffort(finalEffort.Value)
	}
	if err := StoreFinalEffortSelection(c, finalEffort); err != nil {
		return nil, err
	}
	return jsonData, nil
}

func applyClaudeChatProjection(
	envelope *helper.ValidatedRequestEnvelope,
	clientFormat types.RelayFormat,
	finalProtocol Protocol,
	jsonData []byte,
) ([]byte, ClaudeChatProjection, error) {
	projection := ClaudeChatProjection{}
	if clientFormat != types.RelayFormatClaude || finalProtocol != ProtocolChat {
		return jsonData, projection, nil
	}
	var err error
	projection, err = ParseClaudeChatProjection(envelope)
	if err != nil {
		return nil, projection, err
	}
	result := jsonData
	if projection.MetadataPresent {
		result, err = sjson.SetRawBytes(result, "metadata", projection.MetadataRaw)
		if err != nil {
			return nil, projection, errors.New("translate Claude metadata to Chat")
		}
	}
	for _, sourceField := range []string{"output_config", "context_management"} {
		result, err = sjson.DeleteBytes(result, sourceField)
		if err != nil {
			return nil, projection, errors.New("remove translated Claude field from Chat")
		}
	}
	return result, projection, nil
}

func captureProtectedOutboundFields(
	envelope *helper.ValidatedRequestEnvelope,
	clientFormat types.RelayFormat,
	finalProtocol Protocol,
	projection ClaudeChatProjection,
	wireEffort EffortSelection,
	jsonData []byte,
) ([]protectedOutboundField, error) {
	paths := make([][]string, 0, 4)
	if clientFormat == types.RelayFormatClaude && finalProtocol == ProtocolChat {
		if projection.MetadataPresent {
			paths = append(paths, []string{"metadata"})
		}
	}
	if wireEffort.Present {
		paths = append(paths, append([]string(nil), wireEffort.Path...))
	}
	if clientFormat == types.RelayFormatClaude && finalProtocol == ProtocolMessages {
		for _, path := range []string{"metadata", "output_config", "context_management"} {
			if _, present := envelope.TopLevelKind(path); present {
				paths = append(paths, []string{path})
			}
		}
	}
	protected := make([]protectedOutboundField, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		key := strings.Join(path, "\x00")
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		value, present, err := rawJSONPath(jsonData, path)
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, errors.New("protected client field is absent before overrides")
		}
		protected = append(protected, protectedOutboundField{
			path: append([]string(nil), path...), raw: value, present: true,
		})
	}
	return protected, nil
}

func assertProtectedOutboundFields(jsonData []byte, protected []protectedOutboundField) error {
	for _, field := range protected {
		actual, present, err := rawJSONPath(jsonData, field.path)
		if err != nil {
			return err
		}
		equal, err := semanticJSONPresenceEqual(field.raw, field.present, actual, present)
		if err != nil {
			return err
		}
		if !equal {
			return errors.New("operator override changed a protected client field")
		}
	}
	return nil
}

func mergeCrossProtocolSameWireFields(
	envelope *helper.ValidatedRequestEnvelope,
	clientFormat types.RelayFormat,
	finalProtocol Protocol,
	convertedJSON []byte,
) ([]byte, error) {
	if clientFormat == finalProtocol.RelayFormat() {
		return convertedJSON, nil
	}
	result := convertedJSON
	for _, field := range envelope.TopLevelFieldNames() {
		contract, found := LookupRequestPathContract(clientFormat, finalProtocol, field)
		if !found || contract.WireAction != RequestPathWireTranslate ||
			!requestTargetHasKnownTopLevelField(finalProtocol, field) {
			continue
		}
		// Responses input and reasoning translate to Chat messages and
		// reasoning_effort. Their same-named Chat DTO fields are unrelated.
		if clientFormat == types.RelayFormatOpenAIResponses && finalProtocol == ProtocolChat &&
			(field == "input" || field == "reasoning") {
			continue
		}
		raw, present, err := envelope.RawTopLevelField(field)
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, errors.New("validated request inventory lost a same-wire field")
		}
		preserve := clientFormat == types.RelayFormatOpenAIResponses &&
			finalProtocol == ProtocolChat && field == "stream_options"
		if !preserve {
			preserve, err = representableEmptyJSON(raw)
			if err != nil {
				return nil, err
			}
		}
		if !preserve {
			continue
		}
		result, err = sjson.SetRawBytes(result, escapeSJSONLiteralKey(field), raw)
		if err != nil {
			return nil, errors.New("preserve translated OpenCode same-wire field")
		}
	}
	return result, nil
}

func representableEmptyJSON(raw []byte) (bool, error) {
	var value any
	if err := common.Unmarshal(raw, &value); err != nil {
		return false, errors.New("validated same-wire field cannot be decoded")
	}
	switch typed := value.(type) {
	case nil:
		return true, nil
	case string:
		return typed == "", nil
	case []any:
		return len(typed) == 0, nil
	case map[string]any:
		return len(typed) == 0, nil
	default:
		return false, nil
	}
}

// mergeCrossProtocolOpaqueFields restores raw subtrees that converters move
// through interface-typed DTO fields. Those fields are validated before this
// stage, but encoding/json would otherwise round large integers while mapping
// the subtree to its target location.
func mergeCrossProtocolOpaqueFields(
	envelope *helper.ValidatedRequestEnvelope,
	clientFormat types.RelayFormat,
	finalProtocol Protocol,
	convertedJSON []byte,
) ([]byte, error) {
	if envelope == nil || clientFormat == finalProtocol.RelayFormat() {
		return convertedJSON, nil
	}
	var (
		result []byte
		err    error
	)
	switch clientFormat {
	case types.RelayFormatOpenAI:
		result, err = mergeChatCrossProtocolOpaqueFields(envelope, finalProtocol, convertedJSON)
	case types.RelayFormatClaude:
		result, err = mergeClaudeCrossProtocolOpaqueFields(envelope, finalProtocol, convertedJSON)
	case types.RelayFormatOpenAIResponses:
		result, err = mergeResponsesCrossProtocolOpaqueFields(envelope, finalProtocol, convertedJSON)
	default:
		return nil, errors.New("unsupported OpenCode client format for opaque merge")
	}
	if err != nil {
		return nil, fmt.Errorf("merge OpenCode cross-protocol opaque fields: %w", err)
	}
	return result, nil
}

func mergeChatCrossProtocolOpaqueFields(
	envelope *helper.ValidatedRequestEnvelope,
	finalProtocol Protocol,
	convertedJSON []byte,
) ([]byte, error) {
	result := convertedJSON
	tools, err := envelopeRawArray(envelope, "tools")
	if err != nil {
		return nil, err
	}
	for sourceIndex, rawTool := range tools {
		tool, err := rawJSONObject(rawTool)
		if err != nil {
			return nil, err
		}
		toolType, _ := rawJSONStringField(tool, "type")
		switch toolType {
		case "function":
			function, present, err := rawJSONObjectField(tool, "function")
			if err != nil || !present {
				if err != nil {
					return nil, err
				}
				continue
			}
			parameters, present := function["parameters"]
			if !present {
				continue
			}
			name, _ := rawJSONStringField(function, "name")
			targetIndex, err := finalizedToolIndex(result, finalProtocol, name)
			if err != nil {
				return nil, err
			}
			if targetIndex < 0 {
				return nil, errors.New("converted function tool is unavailable")
			}
			path := fmt.Sprintf("tools.%d.parameters", targetIndex)
			if finalProtocol == ProtocolChat {
				path = fmt.Sprintf("tools.%d.function.parameters", targetIndex)
			} else if finalProtocol == ProtocolMessages {
				path = fmt.Sprintf("tools.%d.input_schema", targetIndex)
			}
			result, err = sjson.SetRawBytes(result, path, parameters)
			if err != nil {
				return nil, err
			}
		case "custom":
			if finalProtocol != ProtocolResponses {
				continue
			}
			custom, present := tool["custom"]
			if !present {
				continue
			}
			targetTools, err := finalizedTopLevelArray(result, "tools")
			if err != nil {
				return nil, err
			}
			if sourceIndex >= len(targetTools) {
				return nil, errors.New("converted custom tool is unavailable")
			}
			result, err = sjson.SetRawBytes(result, fmt.Sprintf("tools.%d.custom", sourceIndex), custom)
			if err != nil {
				return nil, err
			}
		}
	}

	if finalProtocol == ProtocolResponses {
		if rawChoice, present, err := envelope.RawTopLevelField("tool_choice"); err != nil {
			return nil, err
		} else if present {
			choice, objectErr := rawJSONObject(rawChoice)
			if objectErr == nil {
				choiceType, _ := rawJSONStringField(choice, "type")
				if choiceType != "function" {
					result, err = sjson.SetRawBytes(result, "tool_choice", rawChoice)
					if err != nil {
						return nil, err
					}
				}
			}
		}

		messages, err := envelopeRawArray(envelope, "messages")
		if err != nil {
			return nil, err
		}
		for _, rawMessage := range messages {
			message, err := rawJSONObject(rawMessage)
			if err != nil {
				return nil, err
			}
			role, _ := rawJSONStringField(message, "role")
			if role != "tool" && role != "function" {
				continue
			}
			callID, _ := rawJSONStringField(message, "tool_call_id")
			content, present := message["content"]
			if callID == "" || !present {
				continue
			}
			path, err := finalizedResponsesInputFieldPath(result, "function_call_output", callID, "output")
			if err != nil {
				return nil, err
			}
			if path == "" {
				return nil, errors.New("converted Chat tool output is unavailable")
			}
			output, err := rawConverterString(content, true)
			if err != nil {
				return nil, err
			}
			result, err = sjson.SetBytes(result, path, output)
			if err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

func mergeClaudeCrossProtocolOpaqueFields(
	envelope *helper.ValidatedRequestEnvelope,
	finalProtocol Protocol,
	convertedJSON []byte,
) ([]byte, error) {
	result := convertedJSON
	tools, err := envelopeRawArray(envelope, "tools")
	if err != nil {
		return nil, err
	}
	for _, rawTool := range tools {
		tool, err := rawJSONObject(rawTool)
		if err != nil {
			return nil, err
		}
		inputSchema, present := tool["input_schema"]
		if !present {
			continue
		}
		name, _ := rawJSONStringField(tool, "name")
		targetIndex, err := finalizedToolIndex(result, finalProtocol, name)
		if err != nil {
			return nil, err
		}
		if targetIndex < 0 {
			return nil, errors.New("converted Messages tool is unavailable")
		}
		path := fmt.Sprintf("tools.%d.parameters", targetIndex)
		if finalProtocol == ProtocolChat {
			path = fmt.Sprintf("tools.%d.function.parameters", targetIndex)
		}
		result, err = sjson.SetRawBytes(result, path, inputSchema)
		if err != nil {
			return nil, err
		}
	}

	messages, err := envelopeRawArray(envelope, "messages")
	if err != nil {
		return nil, err
	}
	for _, rawMessage := range messages {
		message, err := rawJSONObject(rawMessage)
		if err != nil {
			return nil, err
		}
		parts, err := rawJSONArray(message["content"])
		if err != nil {
			continue
		}
		for _, rawPart := range parts {
			part, err := rawJSONObject(rawPart)
			if err != nil {
				return nil, err
			}
			partType, _ := rawJSONStringField(part, "type")
			if partType != "tool_use" {
				continue
			}
			callID, _ := rawJSONStringField(part, "id")
			input, present := part["input"]
			if callID == "" || !present {
				continue
			}
			arguments := string(input)
			var path string
			switch finalProtocol {
			case ProtocolChat:
				path, err = finalizedChatToolCallFieldPath(result, callID, "arguments")
			case ProtocolResponses:
				path, err = finalizedResponsesInputFieldPath(result, "function_call", callID, "arguments")
			}
			if err != nil {
				return nil, err
			}
			if path == "" {
				return nil, errors.New("converted Messages tool input is unavailable")
			}
			result, err = sjson.SetBytes(result, path, arguments)
			if err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

func mergeResponsesCrossProtocolOpaqueFields(
	envelope *helper.ValidatedRequestEnvelope,
	finalProtocol Protocol,
	convertedJSON []byte,
) ([]byte, error) {
	result := convertedJSON
	tools, err := envelopeRawArray(envelope, "tools")
	if err != nil {
		return nil, err
	}
	parameters := make(map[string]json.RawMessage)
	for _, rawTool := range tools {
		if err := collectResponsesRawToolParameters(rawTool, "", parameters); err != nil {
			return nil, err
		}
	}
	for name, rawParameters := range parameters {
		targetIndex, err := finalizedToolIndex(result, finalProtocol, name)
		if err != nil {
			return nil, err
		}
		if targetIndex < 0 {
			return nil, errors.New("converted Responses tool is unavailable")
		}
		path := fmt.Sprintf("tools.%d.parameters", targetIndex)
		if finalProtocol == ProtocolChat {
			path = fmt.Sprintf("tools.%d.function.parameters", targetIndex)
		} else if finalProtocol == ProtocolMessages {
			path = fmt.Sprintf("tools.%d.input_schema", targetIndex)
		}
		result, err = sjson.SetRawBytes(result, path, rawParameters)
		if err != nil {
			return nil, err
		}
	}

	if finalProtocol == ProtocolChat {
		items, err := envelopeRawArray(envelope, "input")
		if err != nil {
			return nil, err
		}
		for _, rawItem := range items {
			item, err := rawJSONObject(rawItem)
			if err != nil {
				return nil, err
			}
			itemType, _ := rawJSONStringField(item, "type")
			callID, _ := rawJSONStringField(item, "call_id")
			if callID == "" {
				callID, _ = rawJSONStringField(item, "id")
			}
			switch itemType {
			case "function_call_output", "custom_tool_call_output":
				output, present := item["output"]
				if callID == "" || !present {
					continue
				}
				content, err := rawConverterString(output, true)
				if err != nil {
					return nil, err
				}
				path, err := finalizedChatToolOutputFieldPath(result, callID, "content")
				if err != nil {
					return nil, err
				}
				if path == "" {
					return nil, errors.New("converted Responses tool output is unavailable")
				}
				result, err = sjson.SetBytes(result, path, content)
				if err != nil {
					return nil, err
				}
			case "custom_tool_call":
				input, present := item["input"]
				if callID == "" || !present {
					continue
				}
				inputText, err := rawConverterString(input, false)
				if err != nil {
					return nil, err
				}
				argumentsJSON, err := common.Marshal(map[string]string{"input": inputText})
				if err != nil {
					return nil, err
				}
				path, err := finalizedChatToolCallFieldPath(result, callID, "arguments")
				if err != nil {
					return nil, err
				}
				if path == "" {
					return nil, errors.New("converted Responses custom call is unavailable")
				}
				result, err = sjson.SetBytes(result, path, string(argumentsJSON))
				if err != nil {
					return nil, err
				}
			}
		}

		rawText, present, err := envelope.RawTopLevelField("text")
		if err != nil {
			return nil, err
		}
		if present {
			text, objectErr := rawJSONObject(rawText)
			if objectErr == nil {
				format, formatPresent := text["format"]
				if formatPresent {
					formatObject, formatErr := rawJSONObject(format)
					if formatErr == nil {
						formatType, _ := rawJSONStringField(formatObject, "type")
						if formatType == "json_schema" {
							result, err = sjson.SetRawBytes(result, "response_format.json_schema", format)
							if err != nil {
								return nil, err
							}
						}
					}
				}
			}
		}
	}
	return result, nil
}

func collectResponsesRawToolParameters(
	rawTool json.RawMessage,
	parentNamespace string,
	parameters map[string]json.RawMessage,
) error {
	tool, err := rawJSONObject(rawTool)
	if err != nil {
		return err
	}
	toolType, _ := rawJSONStringField(tool, "type")
	name, _ := rawJSONStringField(tool, "name")
	switch toolType {
	case "function":
		if parentNamespace != "" {
			name = qualifyOpenCodeGoNamespaceToolName(parentNamespace, name)
		}
		if rawParameters, present := tool["parameters"]; present && name != "" {
			parameters[name] = rawParameters
		}
	case "namespace":
		namespace := name
		if parentNamespace != "" {
			namespace = qualifyOpenCodeGoNamespaceToolName(parentNamespace, name)
		}
		children, err := rawJSONArray(tool["tools"])
		if err != nil {
			children, err = rawJSONArray(tool["children"])
		}
		if err != nil {
			return err
		}
		for _, child := range children {
			if err := collectResponsesRawToolParameters(child, namespace, parameters); err != nil {
				return err
			}
		}
	}
	return nil
}

func envelopeRawArray(envelope *helper.ValidatedRequestEnvelope, field string) ([]json.RawMessage, error) {
	raw, present, err := envelope.RawTopLevelField(field)
	if err != nil || !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, err
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, nil
	}
	return rawJSONArray(raw)
}

func finalizedTopLevelArray(jsonData []byte, field string) ([]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := common.Unmarshal(jsonData, &object); err != nil || object == nil {
		return nil, errors.New("finalized OpenCode request is not an object")
	}
	raw, present := object[field]
	if !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	return rawJSONArray(raw)
}

func rawJSONArray(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("raw JSON array is unavailable")
	}
	var values []json.RawMessage
	if err := common.Unmarshal(raw, &values); err != nil {
		return nil, errors.New("raw JSON value is not an array")
	}
	return values, nil
}

func rawJSONObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := common.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("raw JSON value is not an object")
	}
	return object, nil
}

func rawJSONObjectField(
	object map[string]json.RawMessage,
	field string,
) (map[string]json.RawMessage, bool, error) {
	raw, present := object[field]
	if !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false, nil
	}
	value, err := rawJSONObject(raw)
	return value, true, err
}

func rawJSONStringField(object map[string]json.RawMessage, field string) (string, bool) {
	raw, present := object[field]
	if !present {
		return "", false
	}
	var value string
	if common.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func rawConverterString(raw json.RawMessage, nullAsEmpty bool) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) && nullAsEmpty {
		return "", nil
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var value string
		if err := common.Unmarshal(trimmed, &value); err != nil {
			return "", errors.New("raw converter string is invalid")
		}
		return value, nil
	}
	return string(raw), nil
}

func finalizedToolIndex(jsonData []byte, protocol Protocol, name string) (int, error) {
	tools, err := finalizedTopLevelArray(jsonData, "tools")
	if err != nil {
		return -1, err
	}
	for index, rawTool := range tools {
		tool, err := rawJSONObject(rawTool)
		if err != nil {
			return -1, err
		}
		candidate := ""
		if protocol == ProtocolChat {
			function, present, err := rawJSONObjectField(tool, "function")
			if err != nil {
				return -1, err
			}
			if present {
				candidate, _ = rawJSONStringField(function, "name")
			}
		} else {
			candidate, _ = rawJSONStringField(tool, "name")
		}
		if candidate == name {
			return index, nil
		}
	}
	return -1, nil
}

func finalizedResponsesInputFieldPath(
	jsonData []byte,
	itemType string,
	callID string,
	field string,
) (string, error) {
	items, err := finalizedTopLevelArray(jsonData, "input")
	if err != nil {
		return "", err
	}
	for index, rawItem := range items {
		item, err := rawJSONObject(rawItem)
		if err != nil {
			return "", err
		}
		candidateType, _ := rawJSONStringField(item, "type")
		candidateID, _ := rawJSONStringField(item, "call_id")
		if candidateType == itemType && candidateID == callID {
			return fmt.Sprintf("input.%d.%s", index, field), nil
		}
	}
	return "", nil
}

func finalizedChatToolCallFieldPath(jsonData []byte, callID string, field string) (string, error) {
	messages, err := finalizedTopLevelArray(jsonData, "messages")
	if err != nil {
		return "", err
	}
	for messageIndex, rawMessage := range messages {
		message, err := rawJSONObject(rawMessage)
		if err != nil {
			return "", err
		}
		toolCalls, err := rawJSONArray(message["tool_calls"])
		if err != nil {
			continue
		}
		for callIndex, rawCall := range toolCalls {
			call, err := rawJSONObject(rawCall)
			if err != nil {
				return "", err
			}
			candidateID, _ := rawJSONStringField(call, "id")
			if candidateID == callID {
				return fmt.Sprintf("messages.%d.tool_calls.%d.function.%s", messageIndex, callIndex, field), nil
			}
		}
	}
	return "", nil
}

func finalizedChatToolOutputFieldPath(jsonData []byte, callID string, field string) (string, error) {
	messages, err := finalizedTopLevelArray(jsonData, "messages")
	if err != nil {
		return "", err
	}
	for index, rawMessage := range messages {
		message, err := rawJSONObject(rawMessage)
		if err != nil {
			return "", err
		}
		candidateID, _ := rawJSONStringField(message, "tool_call_id")
		if candidateID == callID {
			return fmt.Sprintf("messages.%d.%s", index, field), nil
		}
	}
	return "", nil
}

func mergeClientExtensionFields(
	envelope *helper.ValidatedRequestEnvelope,
	clientFormat types.RelayFormat,
	finalProtocol Protocol,
	clientExtensions map[string]json.RawMessage,
	convertedJSON []byte,
) ([]byte, error) {
	if len(clientExtensions) == 0 {
		return convertedJSON, nil
	}
	if envelope == nil || envelope.Format() != clientFormat {
		return nil, errors.New("validated request envelope is unavailable for client extensions")
	}
	var convertedFields map[string]json.RawMessage
	if err := common.Unmarshal(convertedJSON, &convertedFields); err != nil || convertedFields == nil {
		return nil, errors.New("converted OpenCode request is not a JSON object")
	}

	result := convertedJSON
	for field, raw := range clientExtensions {
		if clientFormat != finalProtocol.RelayFormat() {
			if _, collision := convertedFields[field]; collision {
				return nil, newRequestPathContractClientError(RequestContractTargetCollisionRule)
			}
		}
		var err error
		result, err = sjson.SetRawBytes(result, escapeSJSONLiteralKey(field), raw)
		if err != nil {
			return nil, errors.New("merge OpenCode client extension")
		}
	}
	return result, nil
}

func escapeSJSONLiteralKey(key string) string {
	if !strings.ContainsAny(key, ".*?\\") {
		return key
	}
	var escaped strings.Builder
	escaped.Grow(len(key) + 4)
	for index := 0; index < len(key); index++ {
		character := key[index]
		if character == '.' || character == '*' || character == '?' || character == '\\' {
			escaped.WriteByte('\\')
		}
		escaped.WriteByte(character)
	}
	return escaped.String()
}

func protocolForRelayFormat(format types.RelayFormat) (Protocol, error) {
	switch format {
	case types.RelayFormatOpenAI:
		return ProtocolChat, nil
	case types.RelayFormatClaude:
		return ProtocolMessages, nil
	case types.RelayFormatOpenAIResponses:
		return ProtocolResponses, nil
	default:
		return "", fmt.Errorf("unsupported finalized OpenCode relay format %q", format)
	}
}

// mergeSameProtocolPreservedFields performs a bounded three-view decision per
// top-level field: raw client JSON, the original typed DTO, and the converted
// DTO. Raw bytes are restored only when the typed value was not intentionally
// changed. If a gateway mutation would otherwise erase a raw-only nested
// extension, the request fails closed instead of silently dropping it.
func mergeSameProtocolPreservedFields(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	envelope *helper.ValidatedRequestEnvelope,
	finalProtocol Protocol,
	convertedJSON []byte,
) ([]byte, error) {
	if info.RelayFormat != finalProtocol.RelayFormat() {
		return convertedJSON, nil
	}
	originalRequest, err := helper.GetAndValidateRequest(c, info.RelayFormat)
	if err != nil {
		return nil, err
	}
	originalJSON, err := common.Marshal(originalRequest)
	if err != nil {
		return nil, fmt.Errorf("marshal original typed OpenCode request: %w", err)
	}
	var originalFields map[string]json.RawMessage
	if err := common.Unmarshal(originalJSON, &originalFields); err != nil || originalFields == nil {
		return nil, errors.New("original typed OpenCode request is not a JSON object")
	}
	var convertedFields map[string]json.RawMessage
	if err := common.Unmarshal(convertedJSON, &convertedFields); err != nil || convertedFields == nil {
		return nil, errors.New("converted OpenCode request is not a JSON object")
	}

	result := convertedJSON
	for _, field := range envelope.TopLevelFieldNames() {
		contract, found := LookupRequestPathContract(info.RelayFormat, finalProtocol, field)
		if !found || contract.WireAction != RequestPathWirePreserve {
			continue
		}
		raw, present, err := envelope.RawTopLevelField(field)
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, errors.New("validated OpenCode request inventory lost a top-level field")
		}

		original, originalPresent := originalFields[field]
		converted, convertedPresent := convertedFields[field]
		unchanged, err := semanticJSONPresenceEqual(original, originalPresent, converted, convertedPresent)
		if err != nil {
			return nil, err
		}
		if unchanged {
			if originalPresent {
				originalFitsRaw, err := semanticJSONSubset(original, raw)
				if err != nil {
					return nil, err
				}
				if !originalFitsRaw {
					rawFitsOriginal, subsetErr := semanticJSONSubset(raw, original)
					if subsetErr != nil {
						return nil, subsetErr
					}
					if rawFitsOriginal {
						// Strict validation normalized this value by adding a
						// required default. Keep the normalized typed value.
						continue
					}
					return nil, newRequestPathContractClientError(RequestContractPreserveConflictRule)
				}
			}
			result, err = sjson.SetRawBytes(result, field, raw)
			if err != nil {
				return nil, fmt.Errorf("preserve OpenCode request field: %w", err)
			}
			continue
		}

		if !originalPresent {
			return nil, newRequestPathContractClientError(RequestContractPreserveConflictRule)
		}
		rawFitsOriginal, err := semanticJSONSubset(raw, original)
		if err != nil {
			return nil, err
		}
		if !rawFitsOriginal {
			return nil, newRequestPathContractClientError(RequestContractPreserveConflictRule)
		}
	}
	return result, nil
}

func semanticJSONPresenceEqual(
	left json.RawMessage,
	leftPresent bool,
	right json.RawMessage,
	rightPresent bool,
) (bool, error) {
	if leftPresent != rightPresent {
		return false, nil
	}
	if !leftPresent {
		return true, nil
	}
	leftSubset, err := semanticJSONSubset(left, right)
	if err != nil || !leftSubset {
		return leftSubset, err
	}
	rightSubset, err := semanticJSONSubset(right, left)
	return rightSubset, err
}

func semanticJSONSubset(expectedRaw, actualRaw []byte) (bool, error) {
	var expected any
	if err := common.DecodeJsonUseNumber(bytes.NewReader(expectedRaw), &expected); err != nil {
		return false, errors.New("typed OpenCode JSON value cannot be decoded")
	}
	var actual any
	if err := common.DecodeJsonUseNumber(bytes.NewReader(actualRaw), &actual); err != nil {
		return false, errors.New("raw OpenCode JSON value cannot be decoded")
	}
	return semanticJSONValueSubset(expected, actual), nil
}

func semanticJSONValueSubset(expected, actual any) bool {
	switch typed := expected.(type) {
	case map[string]any:
		candidate, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		for key, value := range typed {
			actualValue, found := candidate[key]
			if !found || !semanticJSONValueSubset(value, actualValue) {
				return false
			}
		}
		return true
	case []any:
		candidate, ok := actual.([]any)
		if !ok || len(typed) != len(candidate) {
			return false
		}
		for index := range typed {
			if !semanticJSONValueSubset(typed[index], candidate[index]) {
				return false
			}
		}
		return true
	case json.Number:
		candidate, ok := actual.(json.Number)
		if !ok {
			return false
		}
		left, leftOK := new(big.Rat).SetString(typed.String())
		right, rightOK := new(big.Rat).SetString(candidate.String())
		return leftOK && rightOK && left.Cmp(right) == 0
	case string:
		candidate, ok := actual.(string)
		return ok && typed == candidate
	case bool:
		candidate, ok := actual.(bool)
		return ok && typed == candidate
	case nil:
		return actual == nil
	default:
		return false
	}
}

func mergeMessagesRawFields(c *gin.Context, info *relaycommon.RelayInfo, jsonData []byte) ([]byte, error) {
	if info.RelayFormat != types.RelayFormatClaude || info.GetFinalRequestRelayFormat() != types.RelayFormatOpenAI {
		return jsonData, nil
	}
	envelope, found, err := helper.GetValidatedRequestEnvelope(c, info.RelayFormat)
	if err != nil {
		return nil, err
	}
	if !found || envelope == nil {
		return nil, errors.New("validated Messages request envelope is unavailable")
	}
	if err := ValidateMessagesStopSourceCollision(envelope, info.RelayFormat); err != nil {
		return nil, err
	}

	if rawThinking, present, rawErr := envelope.RawTopLevelField("thinking"); rawErr != nil {
		return nil, rawErr
	} else if present {
		jsonData, err = sjson.SetRawBytes(jsonData, "thinking", rawThinking)
		if err != nil {
			return nil, fmt.Errorf("merge Messages thinking: %w", err)
		}
	}

	rawStopSequences, stopSequencesPresent, err := envelope.RawTopLevelField("stop_sequences")
	if err != nil {
		return nil, err
	}
	if stopSequencesPresent {
		jsonData, err = sjson.SetRawBytes(jsonData, "stop", rawStopSequences)
		if err != nil {
			return nil, fmt.Errorf("translate Messages stop_sequences: %w", err)
		}
	} else if rawStop, stopPresent, rawErr := envelope.RawTopLevelField("stop"); rawErr != nil {
		return nil, rawErr
	} else if stopPresent {
		jsonData, err = sjson.SetRawBytes(jsonData, "stop", rawStop)
		if err != nil {
			return nil, fmt.Errorf("forward Messages stop: %w", err)
		}
	}
	jsonData, err = sjson.DeleteBytes(jsonData, "stop_sequences")
	if err != nil {
		return nil, fmt.Errorf("remove translated Messages stop_sequences: %w", err)
	}
	return jsonData, nil
}

func captureOutboundGatewayFields(
	jsonData []byte,
	expectedModel string,
	envelope *helper.ValidatedRequestEnvelope,
) (outboundGatewayFields, error) {
	if !isJSONObject(jsonData) {
		return outboundGatewayFields{}, errors.New("converted OpenCode request is not a JSON object")
	}
	model := gjson.GetBytes(jsonData, "model")
	if !model.Exists() {
		return outboundGatewayFields{}, errors.New("converted OpenCode request has no model")
	}
	if model.Type != gjson.String || model.String() != expectedModel {
		return outboundGatewayFields{}, errors.New("converted OpenCode request model is invalid")
	}
	stream := gjson.GetBytes(jsonData, "stream")
	streamPresent := stream.Exists()
	if streamPresent {
		if stream.Type != gjson.True && stream.Type != gjson.False {
			return outboundGatewayFields{}, errors.New("converted OpenCode request stream is invalid")
		}
	} else if sourcePresent, sourceStream, sourceValid := envelope.Stream(); sourcePresent {
		if !sourceValid {
			return outboundGatewayFields{}, errors.New("validated OpenCode request stream is invalid")
		}
		streamPresent = true
		stream = gjson.Result{Type: gjson.False}
		if sourceStream {
			stream.Type = gjson.True
		}
	}
	return outboundGatewayFields{
		model:         expectedModel,
		stream:        stream.Bool(),
		streamPresent: streamPresent,
	}, nil
}

func reassertOutboundGatewayFields(jsonData []byte, fields outboundGatewayFields) ([]byte, error) {
	result, err := sjson.SetBytes(jsonData, "model", fields.model)
	if err != nil {
		return nil, fmt.Errorf("reassert OpenCode model: %w", err)
	}
	if fields.streamPresent {
		result, err = sjson.SetBytes(result, "stream", fields.stream)
	} else {
		result, err = sjson.DeleteBytes(result, "stream")
	}
	if err != nil {
		return nil, fmt.Errorf("reassert OpenCode stream: %w", err)
	}
	return result, nil
}

func removeDisabledFieldsExact(jsonData []byte, info *relaycommon.RelayInfo) ([]byte, error) {
	settings := info.ChannelOtherSettings
	topLevelDeletes := make([]string, 0, 5)
	if !settings.AllowServiceTier {
		topLevelDeletes = append(topLevelDeletes, "service_tier")
	}
	if !settings.AllowInferenceGeo {
		topLevelDeletes = append(topLevelDeletes, "inference_geo")
	}
	if !settings.AllowSpeed {
		topLevelDeletes = append(topLevelDeletes, "speed")
	}
	if settings.DisableStore {
		topLevelDeletes = append(topLevelDeletes, "store")
	}
	if !settings.AllowSafetyIdentifier {
		topLevelDeletes = append(topLevelDeletes, "safety_identifier")
	}

	result := jsonData
	var err error
	for _, field := range topLevelDeletes {
		result, err = sjson.DeleteBytes(result, field)
		if err != nil {
			return nil, fmt.Errorf("remove disabled OpenCode field %s: %w", field, err)
		}
	}
	if !settings.AllowIncludeObfuscation {
		result, err = sjson.DeleteBytes(result, "stream_options.include_obfuscation")
		if err != nil {
			return nil, fmt.Errorf("remove disabled OpenCode stream option: %w", err)
		}
	}
	return result, nil
}

func validateFinalOutboundRequest(
	jsonData []byte,
	info *relaycommon.RelayInfo,
	fields outboundGatewayFields,
	finalProtocol Protocol,
	clientExtensions map[string]json.RawMessage,
	wireEffort EffortSelection,
) (EffortSelection, error) {
	if !isJSONObject(jsonData) {
		return EffortSelection{}, errors.New("finalized OpenCode request is not a JSON object")
	}
	model := gjson.GetBytes(jsonData, "model")
	if !model.Exists() || model.Type != gjson.String || model.String() != fields.model {
		return EffortSelection{}, errors.New("finalized OpenCode model invariant failed")
	}
	stream := gjson.GetBytes(jsonData, "stream")
	streamPresent := stream.Exists()
	if streamPresent != fields.streamPresent ||
		(streamPresent && (stream.Type != gjson.True && stream.Type != gjson.False || stream.Bool() != fields.stream)) {
		return EffortSelection{}, errors.New("finalized OpenCode stream invariant failed")
	}
	if info.RelayFormat == types.RelayFormatClaude && info.GetFinalRequestRelayFormat() == types.RelayFormatOpenAI {
		if gjson.GetBytes(jsonData, "stop_sequences").Exists() {
			return EffortSelection{}, errors.New("finalized Chat request contains Messages stop_sequences")
		}
		if gjson.GetBytes(jsonData, "output_config").Exists() || gjson.GetBytes(jsonData, "context_management").Exists() {
			return EffortSelection{}, errors.New("finalized Chat request contains a Claude-only field")
		}
	}
	if metadata := gjson.GetBytes(jsonData, "metadata"); metadata.Exists() && finalProtocol == ProtocolChat {
		if err := validateChatMetadata(json.RawMessage(metadata.Raw)); err != nil {
			return EffortSelection{}, err
		}
	}
	finalEffort, err := classifyFinalEffortSelection(jsonData, finalProtocol, wireEffort)
	if err != nil {
		return EffortSelection{}, err
	}

	settings := info.ChannelOtherSettings
	protected := map[string]bool{
		"service_tier":      !settings.AllowServiceTier,
		"inference_geo":     !settings.AllowInferenceGeo,
		"speed":             !settings.AllowSpeed,
		"store":             settings.DisableStore,
		"safety_identifier": !settings.AllowSafetyIdentifier,
	}
	for field, mustBeAbsent := range protected {
		if mustBeAbsent && gjson.GetBytes(jsonData, field).Exists() {
			return EffortSelection{}, fmt.Errorf("finalized OpenCode protected field %s is present", field)
		}
	}
	if !settings.AllowIncludeObfuscation {
		streamOptions := gjson.GetBytes(jsonData, "stream_options")
		if streamOptions.Exists() {
			if streamOptions.Type != gjson.JSON || !isJSONObject([]byte(streamOptions.Raw)) {
				return EffortSelection{}, errors.New("finalized OpenCode stream_options is invalid")
			}
			if gjson.Get(streamOptions.Raw, "include_obfuscation").Exists() {
				return EffortSelection{}, errors.New("finalized OpenCode include_obfuscation is present")
			}
		}
	}
	if err := validateFinalizedRequestPathContracts(jsonData, finalProtocol, clientExtensions); err != nil {
		return EffortSelection{}, err
	}
	return finalEffort, nil
}

func isJSONObject(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) >= 2 && trimmed[0] == '{' && gjson.ValidBytes(trimmed)
}
