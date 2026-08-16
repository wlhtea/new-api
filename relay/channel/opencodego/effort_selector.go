package opencodego

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
)

const (
	EffortSelectorShapeRule        = "request.effort.selector-shape"
	EffortSelectorCollisionRule    = "request.effort.alias-collision"
	EffortSelectorCrossNullRule    = "request.effort.cross-protocol-null"
	EffortSelectorPreflightStage   = "preflight.effort-selector"
	EffortSelectorFinalizedMessage = "finalized OpenCode effort selector is invalid"
	maxEffortSelectorRunes         = 64
	maxEffortSelectorBytes         = 256
	finalEffortSelectionContextKey = "opencodego_final_effort_selection_v1"
)

type EffortSelectorOrigin string

const (
	EffortSelectorOriginNone             EffortSelectorOrigin = "none"
	EffortSelectorOriginClientDirect     EffortSelectorOrigin = "client_direct"
	EffortSelectorOriginClientTranslated EffortSelectorOrigin = "client_translated"
	EffortSelectorOriginOperatorOverride EffortSelectorOrigin = "operator_override"
)

type EffortSelection struct {
	Path    []string
	Present bool
	Null    bool
	Value   string
	Origin  EffortSelectorOrigin
}

// StoreFinalEffortSelection records the selector proven at the finalized wire
// boundary. Candidate planning reads this from its isolated context so model
// capability checks and every physical retry bind the same value.
func StoreFinalEffortSelection(c *gin.Context, selection EffortSelection) error {
	if c == nil {
		return errors.New("finalized effort selection context is nil")
	}
	selection.Path = append([]string(nil), selection.Path...)
	c.Set(finalEffortSelectionContextKey, selection)
	return nil
}

func GetFinalEffortSelection(c *gin.Context) (EffortSelection, bool, error) {
	if c == nil {
		return EffortSelection{}, false, errors.New("finalized effort selection context is nil")
	}
	value, found := c.Get(finalEffortSelectionContextKey)
	if !found {
		return EffortSelection{}, false, nil
	}
	selection, ok := value.(EffortSelection)
	if !ok {
		return EffortSelection{}, true, errors.New("finalized effort selection is corrupt")
	}
	if selection.Present && len(selection.Path) == 0 {
		return EffortSelection{}, true, errors.New("finalized effort selection is incomplete")
	}
	selection.Path = append([]string(nil), selection.Path...)
	return selection, true, nil
}

var effortSelectorPaths = map[types.RelayFormat][][]string{
	types.RelayFormatOpenAI: {
		{"reasoningEffort"},
		{"reasoning_effort"},
		{"reasoning", "effort"},
	},
	types.RelayFormatOpenAIResponses: {
		{"reasoningEffort"},
		{"reasoning_effort"},
		{"reasoning", "effort"},
	},
	types.RelayFormatClaude: {
		{"effort"},
		{"output_config", "effort"},
		{"outputConfig", "effort"},
		{"thinking", "effort"},
	},
}

func ParseEnvelopeEffortSelection(
	envelope *helper.ValidatedRequestEnvelope,
	clientFormat types.RelayFormat,
) (EffortSelection, error) {
	if envelope == nil || envelope.Format() != clientFormat {
		return EffortSelection{}, errors.New("validated request envelope is unavailable for effort selection")
	}
	paths, found := effortSelectorPaths[clientFormat]
	if !found {
		return EffortSelection{}, errors.New("validated request format has no effort selector contract")
	}
	selection := EffortSelection{Origin: EffortSelectorOriginNone}
	for _, path := range paths {
		raw, kind, present, err := envelope.RawObjectPath(path...)
		if err != nil {
			return EffortSelection{}, err
		}
		if !present {
			continue
		}
		if selection.Present {
			return EffortSelection{}, newEffortSelectorClientError(EffortSelectorCollisionRule)
		}
		selection.Present = true
		selection.Path = append([]string(nil), path...)
		selection.Origin = EffortSelectorOriginClientDirect
		if kind == helper.JSONValueNull {
			selection.Null = true
			continue
		}
		if kind != helper.JSONValueString {
			return EffortSelection{}, newEffortSelectorClientError(EffortSelectorShapeRule)
		}
		if err := common.Unmarshal(raw, &selection.Value); err != nil || !validEffortSelectorValue(selection.Value) {
			return EffortSelection{}, newEffortSelectorClientError(EffortSelectorShapeRule)
		}
	}
	return selection, nil
}

func validateEnvelopeEffortSelection(
	envelope *helper.ValidatedRequestEnvelope,
	clientFormat types.RelayFormat,
	finalProtocol Protocol,
) (EffortSelection, error) {
	selection, err := ParseEnvelopeEffortSelection(envelope, clientFormat)
	if err != nil {
		return EffortSelection{}, err
	}
	if selection.Present && selection.Null && finalProtocol.RelayFormat() != clientFormat {
		if clientFormat == types.RelayFormatClaude && finalProtocol == ProtocolChat &&
			strings.Join(selection.Path, "\x00") == "output_config\x00effort" {
			return selection, nil
		}
		return EffortSelection{}, newEffortSelectorClientError(EffortSelectorCrossNullRule)
	}
	if clientFormat == types.RelayFormatClaude && finalProtocol.RelayFormat() != clientFormat {
		if err := validateClaudeCamelOutputConfig(envelope); err != nil {
			return EffortSelection{}, err
		}
	}
	return selection, nil
}

func validateClaudeCamelOutputConfig(envelope *helper.ValidatedRequestEnvelope) error {
	raw, present, err := envelope.RawTopLevelField("outputConfig")
	if err != nil || !present {
		return err
	}
	object, err := decodeStrictRawObject(raw)
	if err != nil {
		return newEffortSelectorClientError(EffortSelectorShapeRule)
	}
	for key := range object {
		if key != "effort" {
			return newEffortSelectorClientError(EffortSelectorShapeRule)
		}
	}
	return nil
}

func parseFinalEffortSelection(jsonData []byte, finalProtocol Protocol) (EffortSelection, error) {
	paths, found := effortSelectorPaths[finalProtocol.RelayFormat()]
	if !found {
		return EffortSelection{}, errors.New("finalized protocol has no effort selector contract")
	}
	selection := EffortSelection{Origin: EffortSelectorOriginNone}
	for _, path := range paths {
		raw, present, err := rawJSONPath(jsonData, path)
		if err != nil {
			return EffortSelection{}, err
		}
		if !present {
			continue
		}
		if selection.Present {
			return EffortSelection{}, errors.New(EffortSelectorFinalizedMessage)
		}
		selection.Present = true
		selection.Path = append([]string(nil), path...)
		trimmed := bytes.TrimSpace(raw)
		if bytes.Equal(trimmed, []byte("null")) {
			selection.Null = true
			continue
		}
		if len(trimmed) == 0 || trimmed[0] != '"' ||
			common.Unmarshal(trimmed, &selection.Value) != nil || !validEffortSelectorValue(selection.Value) {
			return EffortSelection{}, errors.New(EffortSelectorFinalizedMessage)
		}
	}
	return selection, nil
}

func classifyFinalEffortSelection(
	jsonData []byte,
	finalProtocol Protocol,
	wireSelection EffortSelection,
) (EffortSelection, error) {
	selection, err := parseFinalEffortSelection(jsonData, finalProtocol)
	if err != nil {
		return EffortSelection{}, err
	}
	if wireSelection.Present {
		if !selection.Present ||
			strings.Join(wireSelection.Path, "\x00") != strings.Join(selection.Path, "\x00") ||
			wireSelection.Null != selection.Null || wireSelection.Value != selection.Value {
			return EffortSelection{}, errors.New("finalized OpenCode effort selector changed after projection")
		}
		selection.Origin = wireSelection.Origin
		return selection, nil
	}
	if selection.Present {
		selection.Origin = EffortSelectorOriginOperatorOverride
	}
	return selection, nil
}

func applyEffortProjection(
	envelope *helper.ValidatedRequestEnvelope,
	clientFormat types.RelayFormat,
	finalProtocol Protocol,
	jsonData []byte,
) ([]byte, EffortSelection, error) {
	selection, err := validateEnvelopeEffortSelection(envelope, clientFormat, finalProtocol)
	if err != nil {
		return nil, EffortSelection{}, err
	}
	if finalProtocol.RelayFormat() == clientFormat {
		return jsonData, selection, nil
	}

	result := jsonData
	for _, format := range []types.RelayFormat{clientFormat, finalProtocol.RelayFormat()} {
		for _, path := range effortSelectorPaths[format] {
			result, err = deleteJSONPath(result, path)
			if err != nil {
				return nil, EffortSelection{}, errors.New("remove cross-protocol effort selector")
			}
		}
	}
	if finalProtocol == ProtocolMessages &&
		(clientFormat == types.RelayFormatOpenAI || clientFormat == types.RelayFormatOpenAIResponses) &&
		selection.Present {
		result, err = deleteJSONPath(result, []string{"thinking"})
		if err != nil {
			return nil, EffortSelection{}, errors.New("remove approximate Messages thinking projection")
		}
	}
	if !selection.Present {
		return result, selection, nil
	}
	if selection.Null {
		return nil, EffortSelection{}, newEffortSelectorClientError(EffortSelectorCrossNullRule)
	}
	targetPath := canonicalEffortSelectorPath(finalProtocol)
	result, err = setJSONPath(result, targetPath, selection.Value)
	if err != nil {
		return nil, EffortSelection{}, errors.New("translate cross-protocol effort selector")
	}
	selection.Path = targetPath
	selection.Origin = EffortSelectorOriginClientTranslated
	return result, selection, nil
}

func canonicalEffortSelectorPath(protocol Protocol) []string {
	switch protocol {
	case ProtocolChat:
		return []string{"reasoning_effort"}
	case ProtocolResponses:
		return []string{"reasoning", "effort"}
	case ProtocolMessages:
		return []string{"output_config", "effort"}
	default:
		return nil
	}
}

func rawJSONPath(jsonData []byte, path []string) (json.RawMessage, bool, error) {
	if len(path) == 0 {
		return nil, false, errors.New("JSON path is empty")
	}
	object, err := decodeStrictRawObject(jsonData)
	if err != nil {
		return nil, false, errors.New("JSON path root is not an object")
	}
	for index, segment := range path {
		raw, present := object[segment]
		if !present {
			return nil, false, nil
		}
		if index == len(path)-1 {
			return append(json.RawMessage(nil), raw...), true, nil
		}
		object, err = decodeStrictRawObject(raw)
		if err != nil {
			return nil, false, nil
		}
	}
	return nil, false, nil
}

func setJSONPath(jsonData []byte, path []string, value any) ([]byte, error) {
	if len(path) == 0 {
		return nil, errors.New("JSON path is empty")
	}
	return sjson.SetBytes(jsonData, sjsonPath(path), value)
}

func deleteJSONPath(jsonData []byte, path []string) ([]byte, error) {
	if len(path) == 0 {
		return nil, errors.New("JSON path is empty")
	}
	return sjson.DeleteBytes(jsonData, sjsonPath(path))
}

func sjsonPath(path []string) string {
	escaped := make([]string, len(path))
	for index, segment := range path {
		escaped[index] = escapeSJSONLiteralKey(segment)
	}
	return strings.Join(escaped, ".")
}

func validEffortSelectorValue(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && utf8.ValidString(value) &&
		len(value) <= maxEffortSelectorBytes && utf8.RuneCountInString(value) <= maxEffortSelectorRunes
}

func isEffortSelectorAliasTopLevelField(format types.RelayFormat, field string) bool {
	for _, path := range effortSelectorPaths[format] {
		if len(path) == 1 && path[0] == field {
			return true
		}
	}
	return format == types.RelayFormatClaude && field == "outputConfig"
}

func newEffortSelectorClientError(ruleID string) error {
	return &helper.ClientRequestValidationError{
		StatusCode: http.StatusBadRequest,
		Message:    RequestContractPublicMessage,
		RuleID:     ruleID,
		StageID:    EffortSelectorPreflightStage,
	}
}
