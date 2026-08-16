package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"unicode"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

const (
	openCodeGoCapabilitySchemaVersion       = 1
	openCodeGoCapabilityCatalogMaxBytes     = 8 << 20
	openCodeGoCapabilityNormalizedMaxBytes  = 60 << 10
	openCodeGoCapabilityMaxProviders        = 2048
	openCodeGoCapabilityMaxModels           = 4096
	openCodeGoCapabilityMaxModelIDBytes     = 256
	openCodeGoCapabilityMaxOptionsPerModel  = 32
	openCodeGoCapabilityMaxEffortsPerOption = 32
	openCodeGoCapabilityMaxEffortIDBytes    = 64
	openCodeGoCapabilityMaxJSONDepth        = 64
)

var errOpenCodeGoCapabilityInvalidCatalog = errors.New("invalid OpenCode Go capability catalog")

type openCodeGoNormalizedPayload struct {
	SchemaVersion int                         `json:"schema_version"`
	Provider      string                      `json:"provider"`
	Models        []openCodeGoNormalizedModel `json:"models"`
}

type openCodeGoNormalizedModel struct {
	ID           string   `json:"id"`
	OptionsKnown bool     `json:"options_known"`
	Efforts      []string `json:"efforts"`
}

type openCodeGoModelCapability struct {
	optionsKnown bool
	efforts      map[string]struct{}
}

type openCodeGoCapabilitySemantic struct {
	schemaVersion int
	provider      string
	revision      string
	payload       string
	models        map[string]openCodeGoModelCapability
	modelCount    int
}

func normalizeOpenCodeGoCapabilityCatalog(data []byte) (*openCodeGoCapabilitySemantic, error) {
	if len(data) == 0 || len(data) > openCodeGoCapabilityCatalogMaxBytes || !utf8.Valid(data) {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	if !validOpenCodeGoCapabilityUnicodeEscapes(data) {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	if err := validateOpenCodeGoCapabilityUniqueJSON(data); err != nil {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}

	root, err := decodeOpenCodeGoCapabilityObject(data)
	if err != nil || len(root) == 0 || len(root) > openCodeGoCapabilityMaxProviders {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	providerRaw, ok := root[model.OpenCodeGoCapabilityProvider]
	if !ok {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	provider, err := decodeOpenCodeGoCapabilityObject(providerRaw)
	if err != nil {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	providerID, err := decodeOpenCodeGoCapabilityString(provider["id"], openCodeGoCapabilityMaxModelIDBytes)
	if err != nil || providerID != model.OpenCodeGoCapabilityProvider {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	modelsRaw, ok := provider["models"]
	if !ok {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	modelObjects, err := decodeOpenCodeGoCapabilityObject(modelsRaw)
	if err != nil || len(modelObjects) == 0 || len(modelObjects) > openCodeGoCapabilityMaxModels {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}

	normalizedModels := make([]openCodeGoNormalizedModel, 0, len(modelObjects))
	for modelID, rawModel := range modelObjects {
		if !validOpenCodeGoCapabilityIdentifier(modelID, openCodeGoCapabilityMaxModelIDBytes) {
			return nil, errOpenCodeGoCapabilityInvalidCatalog
		}
		modelObject, err := decodeOpenCodeGoCapabilityObject(rawModel)
		if err != nil {
			return nil, errOpenCodeGoCapabilityInvalidCatalog
		}
		declaredID, err := decodeOpenCodeGoCapabilityString(modelObject["id"], openCodeGoCapabilityMaxModelIDBytes)
		if err != nil || declaredID != modelID {
			return nil, errOpenCodeGoCapabilityInvalidCatalog
		}

		reasoningRaw, optionsPresent := modelObject["reasoning_options"]
		if !optionsPresent {
			normalizedModels = append(normalizedModels, openCodeGoNormalizedModel{
				ID:           modelID,
				OptionsKnown: false,
				Efforts:      []string{},
			})
			continue
		}
		options, err := decodeOpenCodeGoCapabilityArray(reasoningRaw)
		if err != nil || len(options) > openCodeGoCapabilityMaxOptionsPerModel {
			return nil, errOpenCodeGoCapabilityInvalidCatalog
		}
		efforts, err := normalizeOpenCodeGoCapabilityOptions(options)
		if err != nil {
			return nil, errOpenCodeGoCapabilityInvalidCatalog
		}
		normalizedModels = append(normalizedModels, openCodeGoNormalizedModel{
			ID:           modelID,
			OptionsKnown: true,
			Efforts:      efforts,
		})
	}

	sort.Slice(normalizedModels, func(i, j int) bool {
		return normalizedModels[i].ID < normalizedModels[j].ID
	})
	return buildOpenCodeGoCapabilitySemantic(openCodeGoNormalizedPayload{
		SchemaVersion: openCodeGoCapabilitySchemaVersion,
		Provider:      model.OpenCodeGoCapabilityProvider,
		Models:        normalizedModels,
	})
}

func normalizeOpenCodeGoCapabilityOptions(options []json.RawMessage) ([]string, error) {
	effortEntrySeen := false
	effortSet := make(map[string]struct{})
	for _, rawOption := range options {
		option, err := decodeOpenCodeGoCapabilityObject(rawOption)
		if err != nil {
			return nil, err
		}
		optionType, err := decodeOpenCodeGoCapabilityString(option["type"], openCodeGoCapabilityMaxEffortIDBytes)
		if err != nil {
			return nil, err
		}
		switch optionType {
		case "effort":
			if effortEntrySeen || hasOpenCodeGoCapabilityUnknownMember(option, "type", "values") {
				return nil, errOpenCodeGoCapabilityInvalidCatalog
			}
			effortEntrySeen = true
			valuesRaw, ok := option["values"]
			if !ok {
				return nil, errOpenCodeGoCapabilityInvalidCatalog
			}
			values, err := decodeOpenCodeGoCapabilityArray(valuesRaw)
			if err != nil || len(values) > openCodeGoCapabilityMaxEffortsPerOption {
				return nil, errOpenCodeGoCapabilityInvalidCatalog
			}
			for _, rawValue := range values {
				trimmed := bytes.TrimSpace(rawValue)
				if bytes.Equal(trimmed, []byte("null")) {
					effortSet["none"] = struct{}{}
					continue
				}
				value, err := decodeOpenCodeGoCapabilityString(rawValue, openCodeGoCapabilityMaxEffortIDBytes)
				if err != nil {
					return nil, err
				}
				effortSet[value] = struct{}{}
			}
		default:
			// Only effort options authorize the selector used by this gateway.
			// Other provider-owned reasoning controls may evolve independently;
			// ignore their shape without invalidating the effort authority.
			continue
		}
	}

	efforts := make([]string, 0, len(effortSet))
	for effort := range effortSet {
		efforts = append(efforts, effort)
	}
	sort.Strings(efforts)
	return efforts, nil
}

func parseOpenCodeGoCapabilityNormalizedPayload(payload string) (*openCodeGoCapabilitySemantic, error) {
	data := []byte(payload)
	if len(data) == 0 || len(data) > openCodeGoCapabilityNormalizedMaxBytes || !utf8.Valid(data) {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	if !validOpenCodeGoCapabilityUnicodeEscapes(data) {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	if err := validateOpenCodeGoCapabilityUniqueJSON(data); err != nil {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	root, err := decodeOpenCodeGoCapabilityObject(data)
	if err != nil || hasOpenCodeGoCapabilityUnknownMember(root, "schema_version", "provider", "models") {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	var schemaVersion int
	if raw, ok := root["schema_version"]; !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || common.Unmarshal(raw, &schemaVersion) != nil {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	provider, err := decodeOpenCodeGoCapabilityString(root["provider"], openCodeGoCapabilityMaxModelIDBytes)
	if err != nil {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	modelValues, err := decodeOpenCodeGoCapabilityArray(root["models"])
	if err != nil || len(modelValues) == 0 || len(modelValues) > openCodeGoCapabilityMaxModels {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}

	normalized := openCodeGoNormalizedPayload{
		SchemaVersion: schemaVersion,
		Provider:      provider,
		Models:        make([]openCodeGoNormalizedModel, 0, len(modelValues)),
	}
	previousID := ""
	for _, rawModel := range modelValues {
		modelObject, err := decodeOpenCodeGoCapabilityObject(rawModel)
		if err != nil || hasOpenCodeGoCapabilityUnknownMember(modelObject, "id", "options_known", "efforts") {
			return nil, errOpenCodeGoCapabilityInvalidCatalog
		}
		id, err := decodeOpenCodeGoCapabilityString(modelObject["id"], openCodeGoCapabilityMaxModelIDBytes)
		if err != nil || (previousID != "" && id <= previousID) {
			return nil, errOpenCodeGoCapabilityInvalidCatalog
		}
		previousID = id
		knownRaw, ok := modelObject["options_known"]
		if !ok || bytes.Equal(bytes.TrimSpace(knownRaw), []byte("null")) {
			return nil, errOpenCodeGoCapabilityInvalidCatalog
		}
		var optionsKnown bool
		if err := common.Unmarshal(knownRaw, &optionsKnown); err != nil {
			return nil, errOpenCodeGoCapabilityInvalidCatalog
		}
		effortValues, err := decodeOpenCodeGoCapabilityArray(modelObject["efforts"])
		if err != nil || len(effortValues) > openCodeGoCapabilityMaxEffortsPerOption {
			return nil, errOpenCodeGoCapabilityInvalidCatalog
		}
		efforts := make([]string, 0, len(effortValues))
		previousEffort := ""
		for _, rawEffort := range effortValues {
			effort, err := decodeOpenCodeGoCapabilityString(rawEffort, openCodeGoCapabilityMaxEffortIDBytes)
			if err != nil || (previousEffort != "" && effort <= previousEffort) {
				return nil, errOpenCodeGoCapabilityInvalidCatalog
			}
			previousEffort = effort
			efforts = append(efforts, effort)
		}
		if !optionsKnown && len(efforts) != 0 {
			return nil, errOpenCodeGoCapabilityInvalidCatalog
		}
		normalized.Models = append(normalized.Models, openCodeGoNormalizedModel{
			ID:           id,
			OptionsKnown: optionsKnown,
			Efforts:      efforts,
		})
	}
	return buildOpenCodeGoCapabilitySemantic(normalized)
}

func buildOpenCodeGoCapabilitySemantic(payload openCodeGoNormalizedPayload) (*openCodeGoCapabilitySemantic, error) {
	if payload.SchemaVersion != openCodeGoCapabilitySchemaVersion ||
		payload.Provider != model.OpenCodeGoCapabilityProvider ||
		len(payload.Models) == 0 || len(payload.Models) > openCodeGoCapabilityMaxModels {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	encoded, err := common.Marshal(payload)
	if err != nil || len(encoded) > openCodeGoCapabilityNormalizedMaxBytes {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	digest := sha256.Sum256(encoded)
	models := make(map[string]openCodeGoModelCapability, len(payload.Models))
	for _, normalizedModel := range payload.Models {
		efforts := make(map[string]struct{}, len(normalizedModel.Efforts))
		for _, effort := range normalizedModel.Efforts {
			efforts[effort] = struct{}{}
		}
		models[normalizedModel.ID] = openCodeGoModelCapability{
			optionsKnown: normalizedModel.OptionsKnown,
			efforts:      efforts,
		}
	}
	return &openCodeGoCapabilitySemantic{
		schemaVersion: payload.SchemaVersion,
		provider:      payload.Provider,
		revision:      hex.EncodeToString(digest[:]),
		payload:       string(encoded),
		models:        models,
		modelCount:    len(models),
	}, nil
}

func decodeOpenCodeGoCapabilityObject(raw []byte) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '{' {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	var value map[string]json.RawMessage
	if err := common.Unmarshal(trimmed, &value); err != nil || value == nil {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	return value, nil
}

func decodeOpenCodeGoCapabilityArray(raw []byte) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '[' {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	var value []json.RawMessage
	if err := common.Unmarshal(trimmed, &value); err != nil || value == nil {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	return value, nil
}

func decodeOpenCodeGoCapabilityString(raw []byte, maxBytes int) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '"' {
		return "", errOpenCodeGoCapabilityInvalidCatalog
	}
	var value string
	if err := common.Unmarshal(trimmed, &value); err != nil || !validOpenCodeGoCapabilityIdentifier(value, maxBytes) {
		return "", errOpenCodeGoCapabilityInvalidCatalog
	}
	return value, nil
}

func validOpenCodeGoCapabilityIdentifier(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func hasOpenCodeGoCapabilityUnknownMember(object map[string]json.RawMessage, allowed ...string) bool {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, member := range allowed {
		allowedSet[member] = struct{}{}
	}
	for member := range object {
		if _, ok := allowedSet[member]; !ok {
			return true
		}
	}
	return false
}

func validateOpenCodeGoCapabilityUniqueJSON(data []byte) error {
	decoder := common.NewJsonDecoderUseNumber(bytes.NewReader(data))
	if err := walkOpenCodeGoCapabilityJSON(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errOpenCodeGoCapabilityInvalidCatalog
		}
		return err
	}
	return nil
}

func validOpenCodeGoCapabilityUnicodeEscapes(data []byte) bool {
	inString := false
	for index := 0; index < len(data); index++ {
		switch data[index] {
		case '"':
			inString = !inString
		case '\\':
			if !inString || index+1 >= len(data) {
				continue
			}
			index++
			if data[index] != 'u' {
				continue
			}
			codeUnit, ok := decodeOpenCodeGoCapabilityHexUnit(data, index+1)
			if !ok {
				return false
			}
			index += 4
			switch {
			case codeUnit >= 0xd800 && codeUnit <= 0xdbff:
				if index+6 >= len(data) || data[index+1] != '\\' || data[index+2] != 'u' {
					return false
				}
				low, ok := decodeOpenCodeGoCapabilityHexUnit(data, index+3)
				if !ok || low < 0xdc00 || low > 0xdfff {
					return false
				}
				index += 6
			case codeUnit >= 0xdc00 && codeUnit <= 0xdfff:
				return false
			}
		}
	}
	return true
}

func decodeOpenCodeGoCapabilityHexUnit(data []byte, start int) (uint16, bool) {
	if start < 0 || start+4 > len(data) {
		return 0, false
	}
	var value uint16
	for _, char := range data[start : start+4] {
		value <<= 4
		switch {
		case char >= '0' && char <= '9':
			value += uint16(char - '0')
		case char >= 'a' && char <= 'f':
			value += uint16(char-'a') + 10
		case char >= 'A' && char <= 'F':
			value += uint16(char-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func walkOpenCodeGoCapabilityJSON(decoder interface {
	Token() (json.Token, error)
	More() bool
}, depth int) error {
	if depth > openCodeGoCapabilityMaxJSONDepth {
		return errOpenCodeGoCapabilityInvalidCatalog
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errOpenCodeGoCapabilityInvalidCatalog
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("%w: duplicate member", errOpenCodeGoCapabilityInvalidCatalog)
			}
			seen[key] = struct{}{}
			if err := walkOpenCodeGoCapabilityJSON(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errOpenCodeGoCapabilityInvalidCatalog
		}
	case '[':
		for decoder.More() {
			if err := walkOpenCodeGoCapabilityJSON(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errOpenCodeGoCapabilityInvalidCatalog
		}
	default:
		return errOpenCodeGoCapabilityInvalidCatalog
	}
	return nil
}
