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
)

const (
	ClaudeMetadataShapeRule             = "request.claude.metadata.shape"
	ClaudeMetadataTargetLimitRule       = "request.claude.metadata.target-limit"
	ClaudeOutputConfigShapeRule         = "request.claude.output-config.shape"
	ClaudeOutputConfigNullRule          = "request.claude.output-config.null-unproved"
	ClaudeOutputConfigUnsupportedRule   = "request.claude.output-config.unsupported-member"
	ClaudeContextManagementActiveRule   = "request.claude.context-management.active"
	ClaudeContextManagementShapeRule    = "request.claude.context-management.shape"
	ChatMetadataShapeRule               = "request.chat.metadata.shape"
	ChatMetadataFinalizationStage       = "finalize.chat-metadata"
	ClaudeFieldContractPreflightStage   = "preflight.claude-field-contract"
	ClaudeFieldDispositionAbsent        = "absent"
	ClaudeFieldDispositionTranslated    = "translated"
	ClaudeFieldDispositionValidatedNoop = "validated_no_effect"
)

const (
	maxChatMetadataEntries     = 16
	maxChatMetadataKeyRunes    = 64
	maxChatMetadataValueRunes  = 512
	maxClaudeEffortStringRunes = 16
)

var supportedClaudeEfforts = map[string]struct{}{
	"low": {}, "medium": {}, "high": {}, "xhigh": {}, "max": {},
}

// ClaudeChatProjection is the complete disposition of Claude-only fields when
// a Messages request is converted to Chat. Raw metadata is retained so the
// finalizer can preserve exact JSON string semantics.
type ClaudeChatProjection struct {
	MetadataPresent         bool
	MetadataRaw             json.RawMessage
	MetadataDisposition     string
	OutputConfigPresent     bool
	OutputConfigDisposition string
	Effort                  string
	ContextPresent          bool
	ContextDisposition      string
}

func ParseClaudeChatProjection(envelope *helper.ValidatedRequestEnvelope) (ClaudeChatProjection, error) {
	projection := ClaudeChatProjection{
		MetadataDisposition:     ClaudeFieldDispositionAbsent,
		OutputConfigDisposition: ClaudeFieldDispositionAbsent,
		ContextDisposition:      ClaudeFieldDispositionAbsent,
	}
	if envelope == nil {
		return projection, errors.New("validated Claude request envelope is unavailable")
	}

	metadata, present, err := envelope.RawTopLevelField("metadata")
	if err != nil {
		return projection, err
	}
	if present {
		if err := validateClaudeMetadataForChat(metadata); err != nil {
			return projection, err
		}
		projection.MetadataPresent = true
		projection.MetadataRaw = append(json.RawMessage(nil), metadata...)
		projection.MetadataDisposition = ClaudeFieldDispositionTranslated
	}

	outputConfig, present, err := envelope.RawTopLevelField("output_config")
	if err != nil {
		return projection, err
	}
	if present {
		effort, selected, err := validateClaudeOutputConfigForChat(outputConfig)
		if err != nil {
			return projection, err
		}
		projection.OutputConfigPresent = true
		projection.OutputConfigDisposition = ClaudeFieldDispositionValidatedNoop
		if selected {
			projection.OutputConfigDisposition = ClaudeFieldDispositionTranslated
			projection.Effort = effort
		}
	}

	contextManagement, present, err := envelope.RawTopLevelField("context_management")
	if err != nil {
		return projection, err
	}
	if present {
		if err := validateClaudeContextManagementNoEffect(contextManagement); err != nil {
			return projection, err
		}
		projection.ContextPresent = true
		projection.ContextDisposition = ClaudeFieldDispositionValidatedNoop
	}
	return projection, nil
}

func validateClaudeMetadataForChat(raw json.RawMessage) error {
	object, err := decodeStrictRawObject(raw)
	if err != nil {
		return newClaudeFieldClientError(ClaudeMetadataShapeRule)
	}
	if len(object) > maxChatMetadataEntries {
		return newClaudeFieldClientError(ClaudeMetadataTargetLimitRule)
	}
	for key, value := range object {
		if key != "user_id" {
			return newClaudeFieldClientError(ClaudeMetadataShapeRule)
		}
		var userID string
		if err := common.Unmarshal(value, &userID); err != nil || string(bytes.TrimSpace(value)) == "null" {
			return newClaudeFieldClientError(ClaudeMetadataShapeRule)
		}
		if utf8.RuneCountInString(key) > maxChatMetadataKeyRunes ||
			utf8.RuneCountInString(userID) > maxChatMetadataValueRunes {
			return newClaudeFieldClientError(ClaudeMetadataTargetLimitRule)
		}
	}
	return nil
}

func validateClaudeOutputConfigForChat(raw json.RawMessage) (string, bool, error) {
	object, err := decodeStrictRawObject(raw)
	if err != nil {
		return "", false, newClaudeFieldClientError(ClaudeOutputConfigShapeRule)
	}
	for key := range object {
		if key != "effort" && key != "format" && key != "task_budget" {
			return "", false, newClaudeFieldClientError(ClaudeOutputConfigShapeRule)
		}
	}
	if _, present := object["format"]; present {
		return "", false, newClaudeFieldClientError(ClaudeOutputConfigUnsupportedRule)
	}
	if _, present := object["task_budget"]; present {
		return "", false, newClaudeFieldClientError(ClaudeOutputConfigUnsupportedRule)
	}
	rawEffort, present := object["effort"]
	if !present {
		return "", false, nil
	}
	if string(bytes.TrimSpace(rawEffort)) == "null" {
		return "", false, newClaudeFieldClientError(ClaudeOutputConfigNullRule)
	}
	var effort string
	if err := common.Unmarshal(rawEffort, &effort); err != nil ||
		utf8.RuneCountInString(effort) > maxClaudeEffortStringRunes {
		return "", false, newClaudeFieldClientError(ClaudeOutputConfigShapeRule)
	}
	if _, supported := supportedClaudeEfforts[effort]; !supported {
		return "", false, newClaudeFieldClientError(ClaudeOutputConfigShapeRule)
	}
	return effort, true, nil
}

func validateClaudeContextManagementNoEffect(raw json.RawMessage) error {
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	object, err := decodeStrictRawObject(raw)
	if err != nil {
		return newClaudeFieldClientError(ClaudeContextManagementShapeRule)
	}
	for key := range object {
		if key != "edits" {
			return newClaudeFieldClientError(ClaudeContextManagementShapeRule)
		}
	}
	rawEdits, present := object["edits"]
	if !present {
		return nil
	}
	var edits []json.RawMessage
	if err := common.Unmarshal(rawEdits, &edits); err != nil || edits == nil {
		return newClaudeFieldClientError(ClaudeContextManagementShapeRule)
	}
	for _, rawEdit := range edits {
		edit, err := decodeStrictRawObject(rawEdit)
		if err != nil {
			return newClaudeFieldClientError(ClaudeContextManagementShapeRule)
		}
		for key := range edit {
			if key != "type" && key != "keep" {
				return newClaudeFieldClientError(ClaudeContextManagementShapeRule)
			}
		}
		var editType string
		if rawType, ok := edit["type"]; !ok || common.Unmarshal(rawType, &editType) != nil {
			return newClaudeFieldClientError(ClaudeContextManagementShapeRule)
		}
		if editType != "clear_thinking_20251015" {
			return newClaudeFieldClientError(ClaudeContextManagementActiveRule)
		}
		rawKeep, ok := edit["keep"]
		if !ok || !claudeContextKeepAll(rawKeep) {
			return newClaudeFieldClientError(ClaudeContextManagementActiveRule)
		}
	}
	return nil
}

func claudeContextKeepAll(raw json.RawMessage) bool {
	var keep string
	if err := common.Unmarshal(raw, &keep); err == nil {
		return keep == "all"
	}
	object, err := decodeStrictRawObject(raw)
	if err != nil || len(object) != 1 {
		return false
	}
	var keepType string
	return common.Unmarshal(object["type"], &keepType) == nil && keepType == "all"
}

func validateChatMetadata(raw json.RawMessage) error {
	object, err := decodeStrictRawObject(raw)
	if err != nil || len(object) > maxChatMetadataEntries {
		return errors.New("finalized Chat metadata is invalid")
	}
	for key, value := range object {
		var text string
		if common.Unmarshal(value, &text) != nil || string(bytes.TrimSpace(value)) == "null" ||
			utf8.RuneCountInString(key) > maxChatMetadataKeyRunes ||
			utf8.RuneCountInString(text) > maxChatMetadataValueRunes {
			return errors.New("finalized Chat metadata is invalid")
		}
	}
	return nil
}

func validateClientChatMetadata(raw json.RawMessage) error {
	if err := validateChatMetadata(raw); err != nil {
		return &helper.ClientRequestValidationError{
			StatusCode: http.StatusBadRequest,
			Message:    RequestContractPublicMessage,
			RuleID:     ChatMetadataShapeRule,
			StageID:    ChatMetadataFinalizationStage,
		}
	}
	return nil
}

func decodeStrictRawObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '{' {
		return nil, errors.New("JSON value is not an object")
	}
	var object map[string]json.RawMessage
	if err := common.Unmarshal(trimmed, &object); err != nil || object == nil {
		return nil, errors.New("JSON value is not an object")
	}
	return object, nil
}

func newClaudeFieldClientError(ruleID string) error {
	return &helper.ClientRequestValidationError{
		StatusCode: http.StatusBadRequest,
		Message:    RequestContractPublicMessage,
		RuleID:     strings.TrimSpace(ruleID),
		StageID:    ClaudeFieldContractPreflightStage,
	}
}
