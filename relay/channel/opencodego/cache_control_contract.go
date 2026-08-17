package opencodego

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

const (
	CacheControlRegistryVersion = "claude-cache-control-v1"
	CacheControlPlanVersion     = "opencodego-cache-control-plan-v1"
	CacheControlMarkerMaxBytes  = int64(4 << 10)

	CacheControlPreflightStage      = "preflight.cache-control"
	CacheControlShapeRule           = "request.cache-control.shape"
	CacheControlPathRule            = "request.cache-control.path"
	CacheControlParentRule          = "request.cache-control.parent"
	CacheControlBreakpointLimitRule = "request.cache-control.breakpoint-limit"
	CacheControlTTLOrderRule        = "request.cache-control.ttl-order"
	CacheControlAutomaticTTLRule    = "request.cache-control.automatic-ttl-conflict"
	CacheControlAutomaticSlotRule   = "request.cache-control.automatic-slot-exhausted"
	CacheControlUnsupportedRule     = "request.cache-control.unsupported-target"

	CacheControlFinalizerStage       = "finalize.cache-control"
	CacheControlPlanMismatchRule     = "gateway.cache-control.plan-mismatch"
	CacheControlPreserveMutationRule = "gateway.cache-control.preserve-mutation"
	CacheControlDropAssertionRule    = "gateway.cache-control.drop-assertion"
	CacheControlUnexpectedMarkerRule = "gateway.cache-control.unclassified-marker"

	cacheControlRuleAutomatic  = "claude.messages.cache-control.automatic"
	cacheControlRuleSystem     = "claude.messages.cache-control.system-text"
	cacheControlRuleText       = "claude.messages.cache-control.message-text"
	cacheControlRuleImage      = "claude.messages.cache-control.user-image"
	cacheControlRuleToolUse    = "claude.messages.cache-control.assistant-tool-use"
	cacheControlRuleToolResult = "claude.messages.cache-control.user-tool-result"
	cacheControlRuleTool       = "claude.messages.cache-control.tool-definition"

	cacheControlTargetScopeTopLevel  = "target.top-level.cache-control"
	cacheControlTargetScopeConverted = "target.converted-cache-owned-paths"
)

type CacheControlAction string

const (
	CacheControlActionPreserve CacheControlAction = "preserve"
	CacheControlActionDrop     CacheControlAction = "drop"
)

type CacheControlPathSegment struct {
	Kind  string `json:"kind"`
	Key   string `json:"key,omitempty"`
	Index int    `json:"index,omitempty"`
}

type CacheControlDisposition struct {
	RuleID               string                    `json:"rule_id"`
	OccurrenceID         string                    `json:"occurrence_id"`
	SourcePath           []CacheControlPathSegment `json:"source_path"`
	NormalizedSourcePath []CacheControlPathSegment `json:"normalized_source_path"`
	TargetPath           []CacheControlPathSegment `json:"target_path,omitempty"`
	TargetScope          string                    `json:"target_scope,omitempty"`
	MarkerDigest         string                    `json:"marker_digest"`
	TTL                  string                    `json:"ttl"`
	Action               CacheControlAction        `json:"action"`
	Automatic            bool                      `json:"automatic,omitempty"`
	SemanticPosition     int                       `json:"semantic_position"`
}

type CacheControlDispositionPlan struct {
	Version         string                    `json:"version"`
	RegistryVersion string                    `json:"registry_version"`
	Policy          string                    `json:"policy"`
	ClientFormat    types.RelayFormat         `json:"client_format"`
	FinalProtocol   Protocol                  `json:"final_protocol"`
	Entries         []CacheControlDisposition `json:"entries"`
	PreserveCount   int                       `json:"preserve_count"`
	DropCount       int                       `json:"drop_count"`
	Canonical       string                    `json:"-"`
	Fingerprint     string                    `json:"-"`
}

type cacheControlMarker struct {
	TTL    string
	Digest string
}

type cacheControlOccurrence struct {
	ruleID               string
	sourcePath           []helper.JSONPathSegment
	normalizedSourcePath []helper.JSONPathSegment
	marker               cacheControlMarker
	automatic            bool
	semanticPosition     int
}

type cacheControlUnit struct {
	markerPath []helper.JSONPathSegment
	position   int
}

type CacheControlFinalizerError struct {
	Configuration bool
	RuleID        string
	StageID       string
	cause         error
}

func (e *CacheControlFinalizerError) Error() string {
	if e == nil || e.cause == nil {
		return "OpenCode cache-control finalization failed"
	}
	return e.cause.Error()
}

func (e *CacheControlFinalizerError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func AsCacheControlFinalizerError(err error) (*CacheControlFinalizerError, bool) {
	var finalizerErr *CacheControlFinalizerError
	ok := errors.As(err, &finalizerErr)
	return finalizerErr, ok
}

func newCacheControlFinalizerInvariantError(cause error) error {
	return wrapCacheControlFinalizerError(cause, false)
}

func newCacheControlFinalizerConfigError(cause error) error {
	return wrapCacheControlFinalizerError(cause, true)

}

func wrapCacheControlFinalizerError(cause error, configuration bool) error {
	ruleID := CacheControlPlanMismatchRule
	stageID := CacheControlFinalizerStage
	underlying := cause
	if typed, ok := AsCacheControlFinalizerError(cause); ok && typed != nil {
		if strings.TrimSpace(typed.RuleID) != "" {
			ruleID = typed.RuleID
		}
		if strings.TrimSpace(typed.StageID) != "" {
			stageID = typed.StageID
		}
		if typed.cause != nil {
			underlying = typed.cause
		}
	}
	return &CacheControlFinalizerError{
		Configuration: configuration,
		RuleID:        ruleID,
		StageID:       stageID,
		cause:         underlying,
	}

}

func newCacheControlDispositionAssertionError(ruleID string, message string) error {
	return &CacheControlFinalizerError{
		RuleID:  ruleID,
		StageID: CacheControlFinalizerStage,
		cause:   errors.New(message),
	}
}

func BuildCacheControlDispositionPlan(
	envelope *helper.ValidatedRequestEnvelope,
	clientFormat types.RelayFormat,
	finalProtocol Protocol,
	typedRequest any,
	policy string,
) (CacheControlDispositionPlan, error) {
	if envelope == nil || envelope.Format() != clientFormat {
		return CacheControlDispositionPlan{}, errors.New("validated request envelope is unavailable for cache-control planning")
	}
	if !validCacheControlPolicy(policy) {
		return CacheControlDispositionPlan{}, errors.New("OpenCode unsupported optional field policy is invalid")
	}
	if !validRequestContractProtocol(finalProtocol) {
		return CacheControlDispositionPlan{}, errors.New("OpenCode cache-control target protocol is invalid")
	}

	plan := CacheControlDispositionPlan{
		Version:         CacheControlPlanVersion,
		RegistryVersion: CacheControlRegistryVersion,
		Policy:          policy,
		ClientFormat:    clientFormat,
		FinalProtocol:   finalProtocol,
		Entries:         []CacheControlDisposition{},
	}
	if clientFormat != types.RelayFormatClaude {
		if err := rejectNonClaudeCacheControlOccurrences(envelope, clientFormat); err != nil {
			return CacheControlDispositionPlan{}, err
		}
		return finalizeCacheControlPlan(envelope, plan)
	}

	request, ok := typedClaudeRequest(typedRequest)
	if !ok || request == nil {
		return CacheControlDispositionPlan{}, errors.New("typed Messages request is unavailable for cache-control planning")
	}
	occurrences, err := classifyCacheControlOccurrences(envelope)
	if err != nil {
		return CacheControlDispositionPlan{}, err
	}
	if len(occurrences) == 0 {
		return finalizeCacheControlPlan(envelope, plan)
	}
	if err := validateCacheControlSourceParents(envelope, occurrences); err != nil {
		return CacheControlDispositionPlan{}, err
	}

	var automatic *cacheControlOccurrence
	explicit := make(map[string]*cacheControlOccurrence, len(occurrences))
	for index := range occurrences {
		occurrence := &occurrences[index]
		if occurrence.automatic {
			if automatic != nil {
				return CacheControlDispositionPlan{}, errors.New("validated request inventory contains duplicate automatic cache control")
			}
			automatic = occurrence
			continue
		}
		explicit[cacheControlPathKey(occurrence.sourcePath)] = occurrence
	}
	if len(explicit) > 4 {
		return CacheControlDispositionPlan{}, newCacheControlClientError(CacheControlBreakpointLimitRule)
	}
	if automatic != nil && len(explicit) >= 4 {
		return CacheControlDispositionPlan{}, newCacheControlClientError(CacheControlAutomaticSlotRule)
	}

	units := buildCacheControlUnits(envelope)
	orderedExplicit := make([]*cacheControlOccurrence, 0, len(explicit))
	for _, unit := range units {
		occurrence, found := explicit[cacheControlPathKey(unit.markerPath)]
		if !found {
			continue
		}
		occurrence.semanticPosition = unit.position
		orderedExplicit = append(orderedExplicit, occurrence)
	}
	if len(orderedExplicit) != len(explicit) {
		return CacheControlDispositionPlan{}, newCacheControlClientError(CacheControlParentRule)
	}
	if !validCacheControlTTLOrder(orderedExplicit, nil) {
		return CacheControlDispositionPlan{}, newCacheControlClientError(CacheControlTTLOrderRule)
	}

	if automatic != nil {
		automatic.semanticPosition = -1
		if len(units) > 0 {
			selected := units[len(units)-1]
			automatic.semanticPosition = selected.position
			if existing, found := explicit[cacheControlPathKey(selected.markerPath)]; found {
				if existing.marker.TTL != automatic.marker.TTL {
					return CacheControlDispositionPlan{}, newCacheControlClientError(CacheControlAutomaticTTLRule)
				}
			} else if !validCacheControlTTLOrder(orderedExplicit, automatic) {
				return CacheControlDispositionPlan{}, newCacheControlClientError(CacheControlTTLOrderRule)
			}
		}
	}

	canonicalOccurrences := make([]cacheControlOccurrence, 0, len(occurrences))
	for _, occurrence := range orderedExplicit {
		canonicalOccurrences = append(canonicalOccurrences, *occurrence)
	}
	if automatic != nil {
		canonicalOccurrences = append(canonicalOccurrences, *automatic)
	}
	sort.SliceStable(canonicalOccurrences, func(left, right int) bool {
		if canonicalOccurrences[left].semanticPosition != canonicalOccurrences[right].semanticPosition {
			return canonicalOccurrences[left].semanticPosition < canonicalOccurrences[right].semanticPosition
		}
		return !canonicalOccurrences[left].automatic && canonicalOccurrences[right].automatic
	})

	for _, occurrence := range canonicalOccurrences {
		action := CacheControlActionDrop
		unsupported := occurrence.automatic || finalProtocol != ProtocolMessages
		if !unsupported {
			action = CacheControlActionPreserve
		} else if policy == dto.OpenCodeGoUnsupportedOptionalFieldStrict {
			return CacheControlDispositionPlan{}, newCacheControlClientError(CacheControlUnsupportedRule)
		}

		entry := CacheControlDisposition{
			RuleID:               occurrence.ruleID,
			OccurrenceID:         cacheControlOccurrenceID(occurrence),
			SourcePath:           publicCacheControlPath(occurrence.sourcePath),
			NormalizedSourcePath: publicCacheControlPath(occurrence.normalizedSourcePath),
			MarkerDigest:         occurrence.marker.Digest,
			TTL:                  occurrence.marker.TTL,
			Action:               action,
			Automatic:            occurrence.automatic,
			SemanticPosition:     occurrence.semanticPosition,
		}
		if action == CacheControlActionPreserve {
			entry.TargetPath = publicCacheControlPath(occurrence.normalizedSourcePath)
			plan.PreserveCount++
		} else {
			if occurrence.automatic {
				entry.TargetScope = cacheControlTargetScopeTopLevel
			} else {
				entry.TargetScope = cacheControlTargetScopeConverted
			}
			plan.DropCount++
		}
		plan.Entries = append(plan.Entries, entry)
	}
	return finalizeCacheControlPlan(envelope, plan)
}

func validateCacheControlSourceParents(
	envelope *helper.ValidatedRequestEnvelope,
	occurrences []cacheControlOccurrence,
) error {
	classified := CacheControlDispositionPlan{
		Entries: make([]CacheControlDisposition, 0, len(occurrences)),
	}
	for _, occurrence := range occurrences {
		classified.Entries = append(classified.Entries, CacheControlDisposition{
			SourcePath: publicCacheControlPath(occurrence.sourcePath),
		})
	}
	return validateSameProtocolClaudeCacheParents(envelope, ProtocolMessages, &classified)
}

func validCacheControlPolicy(policy string) bool {
	return policy == dto.OpenCodeGoUnsupportedOptionalFieldStrict ||
		policy == dto.OpenCodeGoUnsupportedOptionalFieldDropKnown
}

func cacheControlPlanForFinalizer(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	envelope *helper.ValidatedRequestEnvelope,
	finalProtocol Protocol,
	originalRequest any,
) (CacheControlDispositionPlan, error) {
	if c == nil || info == nil || envelope == nil {
		return CacheControlDispositionPlan{}, newCacheControlFinalizerInvariantError(
			errors.New("cache-control finalizer planning input is invalid"),
		)
	}
	rebuilt, err := BuildCacheControlDispositionPlan(
		envelope,
		info.RelayFormat,
		finalProtocol,
		originalRequest,
		info.ChannelOtherSettings.OpenCodeGo.EffectiveUnsupportedOptionalFieldPolicy(),
	)
	if err != nil {
		return CacheControlDispositionPlan{}, err
	}
	preflight, found, err := GetRequestPreflightPlanForSelection(c, info.SelectionGroup, info.ChannelId)
	if err != nil {
		return CacheControlDispositionPlan{}, newCacheControlFinalizerInvariantError(err)
	}
	if !found {
		return rebuilt, nil
	}
	frozen, err := cacheControlDispositionPlanFromPreflight(preflight)
	if err != nil {
		return CacheControlDispositionPlan{}, newCacheControlFinalizerInvariantError(err)
	}
	if frozen.ClientFormat != info.RelayFormat || frozen.FinalProtocol != finalProtocol ||
		frozen.Canonical != rebuilt.Canonical || frozen.Fingerprint != rebuilt.Fingerprint {
		return CacheControlDispositionPlan{}, newCacheControlFinalizerInvariantError(
			errors.New("cache-control finalizer plan no longer matches preflight"),
		)
	}
	return frozen, nil
}

func assertCacheControlDisposition(jsonData []byte, plan CacheControlDispositionPlan) error {
	if err := validateCacheControlPlanShape(plan); err != nil {
		return newCacheControlDispositionAssertionError(CacheControlPlanMismatchRule, err.Error())
	}
	root, err := decodeCacheControlFinalBody(jsonData)
	if err != nil {
		return newCacheControlDispositionAssertionError(CacheControlPlanMismatchRule, err.Error())
	}
	expected := make(map[string]struct{}, plan.PreserveCount)
	for _, entry := range plan.Entries {
		switch entry.Action {
		case CacheControlActionPreserve:
			path, err := helperCacheControlPath(entry.TargetPath)
			if err != nil {
				return newCacheControlDispositionAssertionError(CacheControlPlanMismatchRule, err.Error())
			}
			expected[cacheControlPathKey(path)] = struct{}{}
			value, present := cacheControlValueAtPath(root, path)
			if !present {
				return newCacheControlDispositionAssertionError(
					CacheControlPreserveMutationRule,
					"preserved cache-control marker is absent from finalized request",
				)
			}
			digest, err := cacheControlMarkerDigestFromValue(value)
			if err != nil || digest != entry.MarkerDigest {
				return newCacheControlDispositionAssertionError(
					CacheControlPreserveMutationRule,
					"preserved cache-control marker changed during finalization",
				)
			}
		case CacheControlActionDrop:
			// Exact absence is checked below against the complete target-owned
			// marker set. Opaque tool input and schema data are intentionally
			// outside that bounded set.
		default:
			return newCacheControlDispositionAssertionError(
				CacheControlPlanMismatchRule,
				"cache-control disposition action is invalid",
			)
		}
	}
	actual := targetOwnedCacheControlPaths(root, plan.FinalProtocol)
	for key := range actual {
		if _, planned := expected[key]; planned {
			continue
		}
		ruleID := CacheControlUnexpectedMarkerRule
		if plan.DropCount > 0 {
			ruleID = CacheControlDropAssertionRule
		}
		return newCacheControlDispositionAssertionError(
			ruleID,
			"unplanned cache-control marker is present in finalized request",
		)
	}
	if len(actual) != len(expected) {
		return newCacheControlDispositionAssertionError(
			CacheControlPreserveMutationRule,
			"preserved cache-control marker set changed during finalization",
		)
	}
	return nil
}

func decodeCacheControlFinalBody(jsonData []byte) (any, error) {
	decoder := common.NewJsonDecoderUseNumber(bytes.NewReader(jsonData))
	var root any
	if err := decoder.Decode(&root); err != nil {
		return nil, errors.New("finalized cache-control request cannot be decoded")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("finalized cache-control request has trailing JSON")
	}
	if _, ok := root.(map[string]any); !ok {
		return nil, errors.New("finalized cache-control request is not an object")
	}
	return root, nil
}

func cacheControlValueAtPath(root any, path []helper.JSONPathSegment) (any, bool) {
	current := root
	for _, segment := range path {
		if segment.IsIndex {
			array, ok := current.([]any)
			if !ok || segment.Index < 0 || segment.Index >= len(array) {
				return nil, false
			}
			current = array[segment.Index]
			continue
		}
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		value, present := object[segment.Key]
		if !present {
			return nil, false
		}
		current = value
	}
	return current, true
}

func cacheControlMarkerDigestFromValue(value any) (string, error) {
	object, ok := value.(map[string]any)
	if !ok || len(object) < 1 || len(object) > 2 || object["type"] != "ephemeral" {
		return "", errors.New("finalized cache-control marker has an invalid shape")
	}
	ttl := "5m"
	_, ttlPresent := object["ttl"]
	if rawTTL, present := object["ttl"]; present {
		var ok bool
		ttl, ok = rawTTL.(string)
		if !ok || (ttl != "5m" && ttl != "1h") {
			return "", errors.New("finalized cache-control marker has an invalid TTL")
		}
	}
	for key := range object {
		if key != "type" && key != "ttl" {
			return "", errors.New("finalized cache-control marker has an unknown member")
		}
	}
	return cacheControlMarkerDigestWithPresence(ttl, ttlPresent), nil
}

func targetOwnedCacheControlPresent(root any, protocol Protocol) bool {
	return len(targetOwnedCacheControlPaths(root, protocol)) > 0
}

func targetOwnedCacheControlPaths(root any, protocol Protocol) map[string][]helper.JSONPathSegment {
	paths := make(map[string][]helper.JSONPathSegment)
	object, ok := root.(map[string]any)
	if !ok {
		return paths
	}
	if _, present := object["cache_control"]; present {
		path := cachePath(cacheKey("cache_control"))
		paths[cacheControlPathKey(path)] = path
	}
	collectTargetOwnedCacheControlPaths(
		paths,
		object["tools"],
		cachePath(cacheKey("tools")),
		protocol,
	)
	switch protocol {
	case ProtocolMessages:
		collectTargetOwnedCacheControlPaths(
			paths,
			object["system"],
			cachePath(cacheKey("system")),
			protocol,
		)
		collectTargetOwnedCacheControlPaths(
			paths,
			object["messages"],
			cachePath(cacheKey("messages")),
			protocol,
		)
	case ProtocolChat:
		collectTargetOwnedCacheControlPaths(
			paths,
			object["messages"],
			cachePath(cacheKey("messages")),
			protocol,
		)
	case ProtocolResponses:
		collectTargetOwnedCacheControlPaths(
			paths,
			object["input"],
			cachePath(cacheKey("input")),
			protocol,
		)
	}
	return paths
}

func collectTargetOwnedCacheControlPaths(
	paths map[string][]helper.JSONPathSegment,
	value any,
	path []helper.JSONPathSegment,
	protocol Protocol,
) {
	switch typed := value.(type) {
	case []any:
		for index, item := range typed {
			collectTargetOwnedCacheControlPaths(paths, item, appendCachePath(path, cacheIndex(index)), protocol)
		}
	case map[string]any:
		object := typed
		if _, present := object["cache_control"]; present {
			markerPath := appendCachePath(path, cacheKey("cache_control"))
			paths[cacheControlPathKey(markerPath)] = markerPath
		}
		for key, child := range object {
			if key == "cache_control" || targetCacheControlOpaqueChild(object, path, key, protocol) {
				continue
			}
			collectTargetOwnedCacheControlPaths(paths, child, appendCachePath(path, cacheKey(key)), protocol)
		}
	}
}

func targetCacheControlOpaqueChild(
	parent map[string]any,
	path []helper.JSONPathSegment,
	key string,
	protocol Protocol,
) bool {
	switch protocol {
	case ProtocolMessages:
		if key == "input_schema" && cacheControlIndexedRootPath(path, "tools") {
			name, _ := parent["name"].(string)
			schema, schemaOK := parent["input_schema"].(map[string]any)
			return strings.TrimSpace(name) != "" && schemaOK && schema["type"] == "object"
		}
		return key == "input" && cacheControlMessageContentPath(path) && parent["type"] == "tool_use"
	case ProtocolChat:
		if cacheControlIndexedRootPath(path, "messages") && cacheControlChatToolResultRole(parent["role"]) &&
			(key == "content" || key == "provider_extension") {
			return true
		}
		if key == "parameters" && cacheControlChatToolFunctionPath(path) {
			return true
		}
		return key == "custom" && cacheControlIndexedRootPath(path, "tools") && parent["type"] == "custom"
	case ProtocolResponses:
		if key == "parameters" && cacheControlPathStartsWithKey(path, "tools") && parent["type"] == "function" {
			return true
		}
		if cacheControlIndexedRootPath(path, "tools") && parent["type"] == "custom" &&
			(key == "custom" || key == "format") {
			return true
		}
		if !cacheControlIndexedRootPath(path, "input") {
			return false
		}
		itemType, _ := parent["type"].(string)
		switch key {
		case "input":
			return itemType == "custom_tool_call"
		case "output":
			return itemType == "function_call_output" || itemType == "custom_tool_call_output"
		}
	}
	return false
}

func cacheControlIndexedRootPath(path []helper.JSONPathSegment, root string) bool {
	return len(path) == 2 && cachePathKeyEquals(path[0], root) && path[1].IsIndex
}

func cacheControlMessageContentPath(path []helper.JSONPathSegment) bool {
	return len(path) == 4 && cachePathKeyEquals(path[0], "messages") && path[1].IsIndex &&
		cachePathKeyEquals(path[2], "content") && path[3].IsIndex
}

func cacheControlChatToolFunctionPath(path []helper.JSONPathSegment) bool {
	return len(path) == 3 && cachePathKeyEquals(path[0], "tools") && path[1].IsIndex &&
		cachePathKeyEquals(path[2], "function")
}

func cacheControlChatToolResultRole(value any) bool {
	role, _ := value.(string)
	return role == "tool" || role == "function"
}

func cacheControlPathStartsWithKey(path []helper.JSONPathSegment, key string) bool {
	return len(path) > 0 && cachePathKeyEquals(path[0], key)
}

func typedClaudeRequest(request any) (*dto.ClaudeRequest, bool) {
	switch typed := request.(type) {
	case *dto.ClaudeRequest:
		return typed, typed != nil
	case dto.ClaudeRequest:
		copy := typed
		return &copy, true
	default:
		return nil, false
	}
}

type cacheControlInventoryView struct {
	entries []helper.JSONInventoryEntry
	byPath  map[string]helper.JSONInventoryEntry
}

func classifyCacheControlOccurrences(
	envelope *helper.ValidatedRequestEnvelope,
) ([]cacheControlOccurrence, error) {
	view := newCacheControlInventoryView(envelope.Inventory())
	occurrences := make([]cacheControlOccurrence, 0, 4)
	for _, entry := range view.entries {
		if !cacheControlInventoryEntry(entry) {
			continue
		}
		if cacheControlPathIsOpaqueData(envelope, view, entry.Segments) {
			continue
		}
		kind, ruleID, automatic := classifyCacheControlSourcePath(envelope, view, entry.Segments)
		if kind == "" {
			rule := CacheControlPathRule
			if cacheControlDirectRegisteredPattern(entry.Segments) {
				rule = CacheControlParentRule
			}
			return nil, newCacheControlClientError(rule)
		}
		marker, err := decodeCacheControlMarker(envelope, entry)
		if err != nil {
			return nil, err
		}
		normalizedPath, err := normalizedCacheControlSourcePath(envelope, view, entry.Segments)
		if err != nil {
			return nil, err
		}
		occurrences = append(occurrences, cacheControlOccurrence{
			ruleID: ruleID, sourcePath: cloneJSONPath(entry.Segments), normalizedSourcePath: normalizedPath, marker: marker,
			automatic: automatic, semanticPosition: -1,
		})
	}
	return occurrences, nil
}

func newCacheControlInventoryView(entries []helper.JSONInventoryEntry) cacheControlInventoryView {
	view := cacheControlInventoryView{
		entries: entries,
		byPath:  make(map[string]helper.JSONInventoryEntry, len(entries)),
	}
	for _, entry := range entries {
		view.byPath[cacheControlPathKey(entry.Segments)] = entry
	}
	return view
}

func normalizedCacheControlSourcePath(
	envelope *helper.ValidatedRequestEnvelope,
	view cacheControlInventoryView,
	path []helper.JSONPathSegment,
) ([]helper.JSONPathSegment, error) {
	normalized := cloneJSONPath(path)
	if len(path) != 5 || !cachePathKeyEquals(path[0], "messages") || !path[1].IsIndex ||
		!cachePathKeyEquals(path[2], "content") || !path[3].IsIndex ||
		!cachePathKeyEquals(path[4], "cache_control") {
		return normalized, nil
	}
	normalizedIndex := 0
	found := false
	for _, sourceIndex := range inventoryObjectArrayIndexes(view, cachePath(cacheKey("messages"))) {
		role, ok := inventoryString(
			envelope,
			view,
			cachePath(cacheKey("messages"), cacheIndex(sourceIndex), cacheKey("role")),
			64,
		)
		if !ok {
			return nil, errors.New("validated Messages role is unavailable during cache-control normalization")
		}
		if sourceIndex == path[1].Index {
			if role != "user" && role != "assistant" {
				return nil, errors.New("registered cache-control message role cannot be normalized")
			}
			found = true
			break
		}
		if role == "user" || role == "assistant" {
			normalizedIndex++
		}
	}
	if !found {
		return nil, errors.New("registered cache-control message index cannot be normalized")
	}
	normalized[1].Index = normalizedIndex
	return normalized, nil
}

func cacheControlDirectRegisteredPattern(path []helper.JSONPathSegment) bool {
	if len(path) == 3 && path[1].IsIndex && cachePathKeyEquals(path[2], "cache_control") {
		return cachePathKeyEquals(path[0], "system") || cachePathKeyEquals(path[0], "tools")
	}
	return len(path) == 5 && path[1].IsIndex && path[3].IsIndex &&
		cachePathKeyEquals(path[0], "messages") && cachePathKeyEquals(path[2], "content") &&
		cachePathKeyEquals(path[4], "cache_control")
}

func cacheControlInventoryEntry(entry helper.JSONInventoryEntry) bool {
	if len(entry.Segments) == 0 {
		return false
	}
	last := entry.Segments[len(entry.Segments)-1]
	return !last.IsIndex && last.Key == "cache_control"
}

func cacheControlPathIsOpaqueData(
	envelope *helper.ValidatedRequestEnvelope,
	view cacheControlInventoryView,
	path []helper.JSONPathSegment,
) bool {
	if len(path) >= 4 && cachePathKeyEquals(path[0], "tools") && path[1].IsIndex &&
		cachePathKeyEquals(path[2], "input_schema") {
		return true
	}
	if len(path) >= 6 && cachePathKeyEquals(path[0], "messages") && path[1].IsIndex &&
		cachePathKeyEquals(path[2], "content") && path[3].IsIndex &&
		cachePathKeyEquals(path[4], "input") {
		partType, ok := inventoryString(
			envelope,
			view,
			cachePath(
				cacheKey("messages"), cacheIndex(path[1].Index), cacheKey("content"),
				cacheIndex(path[3].Index), cacheKey("type"),
			),
			64,
		)
		return ok && partType == "tool_use"
	}
	first := path[0]
	return !first.IsIndex && first.Key != "cache_control" && first.Key != "system" &&
		first.Key != "messages" && first.Key != "tools" && first.Key != "thinking"
}

func rejectNonClaudeCacheControlOccurrences(
	envelope *helper.ValidatedRequestEnvelope,
	clientFormat types.RelayFormat,
) error {
	view := newCacheControlInventoryView(envelope.Inventory())
	for _, entry := range view.entries {
		if !cacheControlInventoryEntry(entry) ||
			nonClaudeCacheControlPathIsOpaqueData(envelope, view, clientFormat, entry.Segments) {
			continue
		}
		ruleID := RequestContractUnmappedNestedRule
		if len(entry.Segments) == 1 {
			ruleID = RequestContractUnmappedPathRule
		}
		return newRequestPathContractClientError(ruleID)
	}
	return nil
}

func nonClaudeCacheControlPathIsOpaqueData(
	envelope *helper.ValidatedRequestEnvelope,
	view cacheControlInventoryView,
	clientFormat types.RelayFormat,
	path []helper.JSONPathSegment,
) bool {
	if len(path) == 0 || path[0].IsIndex {
		return false
	}
	if len(path) == 1 {
		return path[0].Key != "cache_control"
	}
	switch clientFormat {
	case types.RelayFormatOpenAI:
		switch path[0].Key {
		case "messages":
			return chatToolMessageOpaqueDataPath(envelope, view, path)
		case "tools":
			return chatFunctionSchemaDataPath(envelope, view, path) ||
				chatCustomToolDataPath(envelope, view, path)
		default:
			return true
		}
	case types.RelayFormatOpenAIResponses:
		switch path[0].Key {
		case "input":
			return responsesOpaqueInputDataPath(envelope, view, path)
		case "tools":
			return responsesToolOpaqueDataPath(envelope, view, path)
		default:
			return true
		}
	default:
		return false
	}
}

func chatFunctionSchemaDataPath(
	envelope *helper.ValidatedRequestEnvelope,
	view cacheControlInventoryView,
	path []helper.JSONPathSegment,
) bool {
	if len(path) < 5 || !path[1].IsIndex || !cachePathKeyEquals(path[2], "function") ||
		!cachePathKeyEquals(path[3], "parameters") {
		return false
	}
	toolPath := cachePath(cacheKey("tools"), cacheIndex(path[1].Index))
	functionPath := appendCachePath(toolPath, cacheKey("function"))
	return inventoryStringEquals(envelope, view, appendCachePath(toolPath, cacheKey("type")), "function") &&
		inventoryKind(view, functionPath) == helper.JSONValueObject &&
		inventoryNonEmptyString(view, appendCachePath(functionPath, cacheKey("name"))) &&
		inventoryKind(view, appendCachePath(functionPath, cacheKey("parameters"))) == helper.JSONValueObject
}

func chatToolMessageOpaqueDataPath(
	envelope *helper.ValidatedRequestEnvelope,
	view cacheControlInventoryView,
	path []helper.JSONPathSegment,
) bool {
	if len(path) < 4 || !path[1].IsIndex || path[2].IsIndex ||
		(path[2].Key != "content" && path[2].Key != "provider_extension") {
		return false
	}
	role, ok := inventoryString(
		envelope,
		view,
		cachePath(cacheKey("messages"), cacheIndex(path[1].Index), cacheKey("role")),
		64,
	)
	return ok && cacheControlChatToolResultRole(role)
}

func chatCustomToolDataPath(
	envelope *helper.ValidatedRequestEnvelope,
	view cacheControlInventoryView,
	path []helper.JSONPathSegment,
) bool {
	if len(path) < 4 || !path[1].IsIndex || !cachePathKeyEquals(path[2], "custom") {
		return false
	}
	toolType, ok := inventoryString(
		envelope,
		view,
		cachePath(cacheKey("tools"), cacheIndex(path[1].Index), cacheKey("type")),
		64,
	)
	return ok && toolType == "custom"
}

func responsesOpaqueInputDataPath(
	envelope *helper.ValidatedRequestEnvelope,
	view cacheControlInventoryView,
	path []helper.JSONPathSegment,
) bool {
	if len(path) < 4 || !path[1].IsIndex || path[2].IsIndex {
		return false
	}
	itemType, ok := inventoryString(
		envelope,
		view,
		cachePath(cacheKey("input"), cacheIndex(path[1].Index), cacheKey("type")),
		64,
	)
	if !ok {
		return false
	}
	switch path[2].Key {
	case "output":
		return itemType == "function_call_output" || itemType == "custom_tool_call_output"
	case "input":
		return itemType == "custom_tool_call"
	default:
		return false
	}
}

func responsesToolOpaqueDataPath(
	envelope *helper.ValidatedRequestEnvelope,
	view cacheControlInventoryView,
	path []helper.JSONPathSegment,
) bool {
	if len(path) < 4 || !path[1].IsIndex {
		return false
	}
	for memberIndex := 2; memberIndex < len(path)-1; memberIndex++ {
		member := path[memberIndex]
		if member.IsIndex || (member.Key != "parameters" && member.Key != "format") {
			continue
		}
		toolPath := path[:memberIndex]
		if !responsesToolObjectPath(toolPath) ||
			inventoryKind(view, toolPath) != helper.JSONValueObject ||
			!inventoryNonEmptyString(view, appendCachePath(toolPath, cacheKey("name"))) {
			return false
		}
		toolType, ok := inventoryString(envelope, view, appendCachePath(toolPath, cacheKey("type")), 64)
		if !ok {
			return false
		}
		switch member.Key {
		case "parameters":
			return toolType == "function" &&
				inventoryKind(view, appendCachePath(toolPath, cacheKey("parameters"))) == helper.JSONValueObject
		case "format":
			return toolType == "custom"
		}
	}
	return false
}

func responsesToolObjectPath(path []helper.JSONPathSegment) bool {
	if len(path) < 2 || !cachePathKeyEquals(path[0], "tools") || !path[1].IsIndex || len(path)%2 != 0 {
		return false
	}
	for index := 2; index < len(path); index += 2 {
		if path[index].IsIndex || (path[index].Key != "tools" && path[index].Key != "children") ||
			!path[index+1].IsIndex {
			return false
		}
	}
	return true
}

func classifyCacheControlSourcePath(
	envelope *helper.ValidatedRequestEnvelope,
	view cacheControlInventoryView,
	path []helper.JSONPathSegment,
) (kind string, ruleID string, automatic bool) {
	if len(path) == 1 && cachePathKeyEquals(path[0], "cache_control") {
		return "automatic", cacheControlRuleAutomatic, true
	}
	if len(path) == 3 && cachePathKeyEquals(path[0], "system") && path[1].IsIndex &&
		cachePathKeyEquals(path[2], "cache_control") {
		if validCacheSystemParent(envelope, view, path[1].Index) {
			return "system", cacheControlRuleSystem, false
		}
		return "", "", false
	}
	if len(path) == 3 && cachePathKeyEquals(path[0], "tools") && path[1].IsIndex &&
		cachePathKeyEquals(path[2], "cache_control") {
		if validCacheToolParent(envelope, view, path[1].Index) {
			return "tool", cacheControlRuleTool, false
		}
		return "", "", false
	}
	if len(path) == 5 && cachePathKeyEquals(path[0], "messages") && path[1].IsIndex &&
		cachePathKeyEquals(path[2], "content") && path[3].IsIndex &&
		cachePathKeyEquals(path[4], "cache_control") {
		return validCacheMessageParent(envelope, view, path[1].Index, path[3].Index)
	}
	return "", "", false
}

func validCacheSystemParent(
	envelope *helper.ValidatedRequestEnvelope,
	view cacheControlInventoryView,
	index int,
) bool {
	parent := cachePath(cacheKey("system"), cacheIndex(index))
	return inventoryKind(view, parent) == helper.JSONValueObject &&
		inventoryStringEquals(envelope, view, appendCachePath(parent, cacheKey("type")), "text") &&
		inventoryNonEmptyString(view, appendCachePath(parent, cacheKey("text"))) &&
		inventoryObjectHasOnlyKeys(view, parent, "type", "text", "cache_control")
}

func validCacheToolParent(
	envelope *helper.ValidatedRequestEnvelope,
	view cacheControlInventoryView,
	index int,
) bool {
	parent := cachePath(cacheKey("tools"), cacheIndex(index))
	inputSchemaPath := appendCachePath(parent, cacheKey("input_schema"))
	descriptionKind := inventoryKind(view, appendCachePath(parent, cacheKey("description")))
	return inventoryKind(view, parent) == helper.JSONValueObject &&
		inventoryNonEmptyString(view, appendCachePath(parent, cacheKey("name"))) &&
		(descriptionKind == "" || descriptionKind == helper.JSONValueString) &&
		inventoryKind(view, inputSchemaPath) == helper.JSONValueObject &&
		inventoryStringEquals(envelope, view, appendCachePath(inputSchemaPath, cacheKey("type")), "object") &&
		inventoryObjectHasOnlyKeys(view, parent, "name", "description", "input_schema", "cache_control")
}

func validCacheMessageParent(
	envelope *helper.ValidatedRequestEnvelope,
	view cacheControlInventoryView,
	messageIndex int,
	partIndex int,
) (kind string, ruleID string, automatic bool) {
	messagePath := cachePath(cacheKey("messages"), cacheIndex(messageIndex))
	if inventoryKind(view, messagePath) != helper.JSONValueObject ||
		!inventoryObjectHasOnlyKeys(view, messagePath, "role", "content") {
		return "", "", false
	}
	role, ok := inventoryString(envelope, view, appendCachePath(messagePath, cacheKey("role")), 64)
	if !ok || (role != "user" && role != "assistant") {
		return "", "", false
	}
	partPath := cachePath(
		cacheKey("messages"), cacheIndex(messageIndex), cacheKey("content"), cacheIndex(partIndex),
	)
	if inventoryKind(view, partPath) != helper.JSONValueObject {
		return "", "", false
	}
	partType, ok := inventoryString(envelope, view, appendCachePath(partPath, cacheKey("type")), 64)
	if !ok {
		return "", "", false
	}
	switch partType {
	case "text":
		if !inventoryObjectHasOnlyKeys(view, partPath, "type", "text", "cache_control") ||
			!inventoryNonEmptyString(view, appendCachePath(partPath, cacheKey("text"))) {
			return "", "", false
		}
		return "text", cacheControlRuleText, false
	case "image":
		if role != "user" || !inventoryObjectHasOnlyKeys(view, partPath, "type", "source", "cache_control") {
			return "", "", false
		}
		if !validCacheImageSource(envelope, view, appendCachePath(partPath, cacheKey("source"))) {
			return "", "", false
		}
		return "image", cacheControlRuleImage, false
	case "tool_use":
		if role != "assistant" || !inventoryObjectHasOnlyKeys(
			view, partPath, "type", "id", "name", "input", "cache_control",
		) {
			return "", "", false
		}
		if !inventoryNonEmptyString(view, appendCachePath(partPath, cacheKey("id"))) ||
			!inventoryNonEmptyString(view, appendCachePath(partPath, cacheKey("name"))) ||
			inventoryKind(view, appendCachePath(partPath, cacheKey("input"))) == "" {
			return "", "", false
		}
		return "tool_use", cacheControlRuleToolUse, false
	case "tool_result":
		isErrorKind := inventoryKind(view, appendCachePath(partPath, cacheKey("is_error")))
		if role != "user" || !inventoryObjectHasOnlyKeys(
			view, partPath, "type", "tool_use_id", "content", "is_error", "cache_control",
		) {
			return "", "", false
		}
		if !inventoryNonEmptyString(view, appendCachePath(partPath, cacheKey("tool_use_id"))) ||
			(isErrorKind != "" && isErrorKind != helper.JSONValueBoolean) ||
			!validCacheToolResultContent(envelope, view, appendCachePath(partPath, cacheKey("content"))) {
			return "", "", false
		}
		return "tool_result", cacheControlRuleToolResult, false
	default:
		return "", "", false
	}
}

func validCacheToolResultContent(
	envelope *helper.ValidatedRequestEnvelope,
	view cacheControlInventoryView,
	contentPath []helper.JSONPathSegment,
) bool {
	switch inventoryKind(view, contentPath) {
	case helper.JSONValueString:
		return true
	case helper.JSONValueArray:
		childCount := 0
		for _, entry := range view.entries {
			if len(entry.Segments) != len(contentPath)+1 ||
				!cacheControlPathPrefix(entry.Segments, contentPath) {
				continue
			}
			last := entry.Segments[len(contentPath)]
			if !last.IsIndex || entry.Kind != helper.JSONValueObject {
				return false
			}
			childCount++
			partPath := appendCachePath(contentPath, last)
			partType, ok := inventoryString(
				envelope,
				view,
				appendCachePath(partPath, cacheKey("type")),
				64,
			)
			if !ok {
				return false
			}
			switch partType {
			case "text":
				if !inventoryObjectHasOnlyKeys(view, partPath, "type", "text") ||
					inventoryKind(view, appendCachePath(partPath, cacheKey("text"))) != helper.JSONValueString {
					return false
				}
			case "image":
				if !inventoryObjectHasOnlyKeys(view, partPath, "type", "source") {
					return false
				}
				if !validCacheImageSource(envelope, view, appendCachePath(partPath, cacheKey("source"))) {
					return false
				}
			default:
				return false
			}
		}
		return childCount > 0
	default:
		return false
	}
}

func validCacheImageSource(
	envelope *helper.ValidatedRequestEnvelope,
	view cacheControlInventoryView,
	sourcePath []helper.JSONPathSegment,
) bool {
	if inventoryKind(view, sourcePath) != helper.JSONValueObject ||
		!inventoryObjectHasOnlyKeys(view, sourcePath, "type", "media_type", "data") ||
		!inventoryStringEquals(envelope, view, appendCachePath(sourcePath, cacheKey("type")), "base64") ||
		!inventoryNonEmptyString(view, appendCachePath(sourcePath, cacheKey("data"))) {
		return false
	}
	mediaType, ok := inventoryString(
		envelope,
		view,
		appendCachePath(sourcePath, cacheKey("media_type")),
		64,
	)
	if !ok {
		return false
	}
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func inventoryKind(view cacheControlInventoryView, path []helper.JSONPathSegment) helper.JSONValueKind {
	entry, found := view.byPath[cacheControlPathKey(path)]
	if !found {
		return ""
	}
	return entry.Kind
}

func inventoryNonEmptyString(view cacheControlInventoryView, path []helper.JSONPathSegment) bool {
	entry, found := view.byPath[cacheControlPathKey(path)]
	return found && entry.Kind == helper.JSONValueString && entry.End-entry.Start > 2
}

func inventoryStringEquals(
	envelope *helper.ValidatedRequestEnvelope,
	view cacheControlInventoryView,
	path []helper.JSONPathSegment,
	want string,
) bool {
	actual, ok := inventoryString(envelope, view, path, 256)
	return ok && actual == want
}

func inventoryString(
	envelope *helper.ValidatedRequestEnvelope,
	view cacheControlInventoryView,
	path []helper.JSONPathSegment,
	maxBytes int64,
) (string, bool) {
	entry, found := view.byPath[cacheControlPathKey(path)]
	if !found || entry.Kind != helper.JSONValueString {
		return "", false
	}
	reader, err := envelope.OpenSpan(entry, maxBytes)
	if err != nil {
		return "", false
	}
	defer reader.Close()
	var value string
	decoder := common.NewJsonDecoderUseNumber(reader)
	if err := decoder.Decode(&value); err != nil {
		return "", false
	}
	var trailing any
	return value, decoder.Decode(&trailing) == io.EOF
}

func inventoryObjectHasOnlyKeys(
	view cacheControlInventoryView,
	parent []helper.JSONPathSegment,
	keys ...string,
) bool {
	allowed := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		allowed[key] = struct{}{}
	}
	foundChild := false
	for _, entry := range view.entries {
		if len(entry.Segments) != len(parent)+1 || !cacheControlPathPrefix(entry.Segments, parent) {
			continue
		}
		child := entry.Segments[len(parent)]
		if child.IsIndex {
			return false
		}
		foundChild = true
		if _, found := allowed[child.Key]; !found {
			return false
		}
	}
	return foundChild
}

func cacheControlPathPrefix(path []helper.JSONPathSegment, prefix []helper.JSONPathSegment) bool {
	if len(path) < len(prefix) {
		return false
	}
	for index, segment := range prefix {
		candidate := path[index]
		if candidate.IsIndex != segment.IsIndex || candidate.Key != segment.Key || candidate.Index != segment.Index {
			return false
		}
	}
	return true
}

func appendCachePath(path []helper.JSONPathSegment, segments ...helper.JSONPathSegment) []helper.JSONPathSegment {
	result := make([]helper.JSONPathSegment, 0, len(path)+len(segments))
	result = append(result, path...)
	return append(result, segments...)
}

func decodeCacheControlMarker(
	envelope *helper.ValidatedRequestEnvelope,
	entry helper.JSONInventoryEntry,
) (cacheControlMarker, error) {
	if entry.Kind != helper.JSONValueObject {
		return cacheControlMarker{}, newCacheControlClientError(CacheControlShapeRule)
	}
	reader, err := envelope.OpenSpan(entry, CacheControlMarkerMaxBytes)
	if err != nil {
		if errors.Is(err, helper.ErrJSONSpanTooLarge) {
			return cacheControlMarker{}, newCacheControlClientError(CacheControlShapeRule)
		}
		return cacheControlMarker{}, err
	}
	defer reader.Close()

	var object map[string]json.RawMessage
	decoder := common.NewJsonDecoderUseNumber(reader)
	if err := decoder.Decode(&object); err != nil || object == nil {
		return cacheControlMarker{}, newCacheControlClientError(CacheControlShapeRule)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return cacheControlMarker{}, newCacheControlClientError(CacheControlShapeRule)
	}
	if len(object) < 1 || len(object) > 2 {
		return cacheControlMarker{}, newCacheControlClientError(CacheControlShapeRule)
	}
	typeRaw, typePresent := object["type"]
	if !typePresent {
		return cacheControlMarker{}, newCacheControlClientError(CacheControlShapeRule)
	}
	var markerType string
	if err := common.Unmarshal(typeRaw, &markerType); err != nil || markerType != "ephemeral" {
		return cacheControlMarker{}, newCacheControlClientError(CacheControlShapeRule)
	}
	ttl := "5m"
	_, ttlPresent := object["ttl"]
	if ttlRaw, present := object["ttl"]; present {
		if common.GetJsonType(ttlRaw) != "string" ||
			common.Unmarshal(ttlRaw, &ttl) != nil || (ttl != "5m" && ttl != "1h") {
			return cacheControlMarker{}, newCacheControlClientError(CacheControlShapeRule)
		}
	}
	for key := range object {
		if key != "type" && key != "ttl" {
			return cacheControlMarker{}, newCacheControlClientError(CacheControlShapeRule)
		}
	}
	return cacheControlMarker{TTL: ttl, Digest: cacheControlMarkerDigestWithPresence(ttl, ttlPresent)}, nil
}

func cacheControlMarkerDigest(ttl string) string {
	return cacheControlMarkerDigestWithPresence(ttl, true)
}

func cacheControlMarkerDigestWithPresence(ttl string, ttlPresent bool) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte("ephemeral"))
	_, _ = hasher.Write([]byte{0})
	if ttlPresent {
		_, _ = hasher.Write([]byte("ttl"))
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write([]byte(ttl))
	} else {
		_, _ = hasher.Write([]byte("ttl-absent"))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func buildCacheControlUnits(envelope *helper.ValidatedRequestEnvelope) []cacheControlUnit {
	view := newCacheControlInventoryView(envelope.Inventory())
	units := make([]cacheControlUnit, 0)
	position := 0
	for _, index := range inventoryObjectArrayIndexes(view, cachePath(cacheKey("tools"))) {
		if validCacheToolParent(envelope, view, index) {
			units = append(units, cacheControlUnit{
				markerPath: cachePath(cacheKey("tools"), cacheIndex(index), cacheKey("cache_control")),
				position:   position,
			})
			position++
		}
	}
	systemPath := cachePath(cacheKey("system"))
	if inventoryNonEmptyString(view, systemPath) {
		units = append(units, cacheControlUnit{position: position})
		position++
	} else {
		for _, index := range inventoryObjectArrayIndexes(view, systemPath) {
			if !validCacheSystemParent(envelope, view, index) {
				continue
			}
			units = append(units, cacheControlUnit{
				markerPath: cachePath(cacheKey("system"), cacheIndex(index), cacheKey("cache_control")),
				position:   position,
			})
			position++
		}
	}
	messageIndexes := inventoryObjectArrayIndexes(view, cachePath(cacheKey("messages")))
	for _, messageIndex := range messageIndexes {
		messagePath := cachePath(cacheKey("messages"), cacheIndex(messageIndex))
		if !inventoryObjectHasOnlyKeys(view, messagePath, "role", "content") {
			continue
		}
		role, ok := inventoryString(envelope, view, appendCachePath(messagePath, cacheKey("role")), 64)
		if !ok || (role != "user" && role != "assistant") {
			continue
		}
		contentPath := appendCachePath(messagePath, cacheKey("content"))
		if inventoryNonEmptyString(view, contentPath) {
			units = append(units, cacheControlUnit{position: position})
			position++
			continue
		}
		for _, partIndex := range inventoryObjectArrayIndexes(view, contentPath) {
			kind, _, _ := validCacheMessageParent(envelope, view, messageIndex, partIndex)
			if kind == "" {
				continue
			}
			partPath := appendCachePath(contentPath, cacheIndex(partIndex))
			units = append(units, cacheControlUnit{
				markerPath: appendCachePath(partPath, cacheKey("cache_control")),
				position:   position,
			})
			position++
		}
	}
	return units
}

func inventoryObjectArrayIndexes(
	view cacheControlInventoryView,
	arrayPath []helper.JSONPathSegment,
) []int {
	indexes := make([]int, 0)
	for _, entry := range view.entries {
		if entry.Kind != helper.JSONValueObject || len(entry.Segments) != len(arrayPath)+1 ||
			!cacheControlPathPrefix(entry.Segments, arrayPath) {
			continue
		}
		last := entry.Segments[len(arrayPath)]
		if last.IsIndex {
			indexes = append(indexes, last.Index)
		}
	}
	sort.Ints(indexes)
	return indexes
}

func validCacheControlTTLOrder(
	explicit []*cacheControlOccurrence,
	automatic *cacheControlOccurrence,
) bool {
	effective := make([]*cacheControlOccurrence, 0, len(explicit)+1)
	effective = append(effective, explicit...)
	if automatic != nil {
		effective = append(effective, automatic)
	}
	sort.SliceStable(effective, func(left, right int) bool {
		return effective[left].semanticPosition < effective[right].semanticPosition
	})
	seenFiveMinutes := false
	for _, occurrence := range effective {
		if occurrence.marker.TTL == "5m" {
			seenFiveMinutes = true
			continue
		}
		if occurrence.marker.TTL == "1h" && seenFiveMinutes {
			return false
		}
	}
	return true
}

func finalizeCacheControlPlan(
	envelope *helper.ValidatedRequestEnvelope,
	plan CacheControlDispositionPlan,
) (CacheControlDispositionPlan, error) {
	canonical, err := common.Marshal(plan)
	if err != nil {
		return CacheControlDispositionPlan{}, err
	}
	plan.Canonical = string(canonical)
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(envelope.ContractFingerprint()))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write(canonical)
	plan.Fingerprint = hex.EncodeToString(hasher.Sum(nil))
	return plan, nil
}

func cacheControlOccurrenceID(occurrence cacheControlOccurrence) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(occurrence.ruleID))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write([]byte(cacheControlPathKey(occurrence.sourcePath)))
	return hex.EncodeToString(hasher.Sum(nil))
}

func publicCacheControlPath(path []helper.JSONPathSegment) []CacheControlPathSegment {
	result := make([]CacheControlPathSegment, len(path))
	for index, segment := range path {
		if segment.IsIndex {
			result[index] = CacheControlPathSegment{Kind: "index", Index: segment.Index}
		} else {
			result[index] = CacheControlPathSegment{Kind: "key", Key: segment.Key}
		}
	}
	return result
}

func helperCacheControlPath(path []CacheControlPathSegment) ([]helper.JSONPathSegment, error) {
	result := make([]helper.JSONPathSegment, len(path))
	for index, segment := range path {
		switch segment.Kind {
		case "key":
			if segment.Key == "" || segment.Index != 0 {
				return nil, errors.New("cache-control key path segment is invalid")
			}
			result[index] = helper.JSONPathSegment{Key: segment.Key}
		case "index":
			if segment.Key != "" || segment.Index < 0 {
				return nil, errors.New("cache-control index path segment is invalid")
			}
			result[index] = helper.JSONPathSegment{IsIndex: true, Index: segment.Index}
		default:
			return nil, errors.New("cache-control path segment kind is invalid")
		}
	}
	return result, nil
}

func cacheControlPathKey(path []helper.JSONPathSegment) string {
	var builder strings.Builder
	for _, segment := range path {
		if segment.IsIndex {
			builder.WriteByte('i')
			builder.WriteString(strconv.Itoa(segment.Index))
			builder.WriteByte(';')
			continue
		}
		builder.WriteByte('k')
		builder.WriteString(strconv.Itoa(len(segment.Key)))
		builder.WriteByte(':')
		builder.WriteString(segment.Key)
		builder.WriteByte(';')
	}
	return builder.String()
}

func cloneJSONPath(path []helper.JSONPathSegment) []helper.JSONPathSegment {
	return append([]helper.JSONPathSegment(nil), path...)
}

func cachePath(segments ...helper.JSONPathSegment) []helper.JSONPathSegment {
	return segments
}

func cacheKey(key string) helper.JSONPathSegment {
	return helper.JSONPathSegment{Key: key}
}

func cacheIndex(index int) helper.JSONPathSegment {
	return helper.JSONPathSegment{IsIndex: true, Index: index}
}

func cachePathKeyEquals(segment helper.JSONPathSegment, key string) bool {
	return !segment.IsIndex && segment.Key == key
}

func mapHasOnlyKeys(object map[string]any, keys ...string) bool {
	allowed := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		allowed[key] = struct{}{}
	}
	for key := range object {
		if _, found := allowed[key]; !found {
			return false
		}
	}
	return true
}

func newCacheControlClientError(ruleID string) error {
	return &helper.ClientRequestValidationError{
		StatusCode: http.StatusBadRequest,
		Message:    RequestContractPublicMessage,
		RuleID:     ruleID,
		StageID:    CacheControlPreflightStage,
	}
}

func validateCacheControlPlanShape(plan CacheControlDispositionPlan) error {
	if plan.Version != CacheControlPlanVersion || plan.RegistryVersion != CacheControlRegistryVersion ||
		!validCacheControlPolicy(plan.Policy) || !validRequestContractClientFormat(plan.ClientFormat) ||
		!validRequestContractProtocol(plan.FinalProtocol) || len(plan.Entries) > 4 ||
		plan.PreserveCount < 0 || plan.DropCount < 0 ||
		plan.PreserveCount+plan.DropCount != len(plan.Entries) {
		return errors.New("cache-control disposition plan is invalid")
	}
	for _, entry := range plan.Entries {
		if entry.RuleID == "" || entry.OccurrenceID == "" || entry.MarkerDigest == "" ||
			(entry.TTL != "5m" && entry.TTL != "1h") ||
			(entry.Action != CacheControlActionPreserve && entry.Action != CacheControlActionDrop) {
			return errors.New("cache-control disposition entry is invalid")
		}
		if _, err := helperCacheControlPath(entry.SourcePath); err != nil {
			return err
		}
		if _, err := helperCacheControlPath(entry.NormalizedSourcePath); err != nil {
			return err
		}
		if entry.Action == CacheControlActionPreserve {
			if len(entry.TargetPath) == 0 || entry.TargetScope != "" {
				return errors.New("cache-control preserve target is invalid")
			}
			if _, err := helperCacheControlPath(entry.TargetPath); err != nil {
				return err
			}
		} else if len(entry.TargetPath) != 0 ||
			(entry.TargetScope != cacheControlTargetScopeTopLevel && entry.TargetScope != cacheControlTargetScopeConverted) {
			return errors.New("cache-control drop target is invalid")
		}
	}
	return nil
}

func parseCacheControlDispositionPlan(canonical string) (CacheControlDispositionPlan, error) {
	if strings.TrimSpace(canonical) == "" {
		return CacheControlDispositionPlan{}, errors.New("cache-control disposition plan is absent")
	}
	var plan CacheControlDispositionPlan
	if err := common.UnmarshalJsonStr(canonical, &plan); err != nil {
		return CacheControlDispositionPlan{}, err
	}
	if err := validateCacheControlPlanShape(plan); err != nil {
		return CacheControlDispositionPlan{}, err
	}
	reencoded, err := common.Marshal(plan)
	if err != nil {
		return CacheControlDispositionPlan{}, err
	}
	if string(reencoded) != canonical {
		return CacheControlDispositionPlan{}, fmt.Errorf("cache-control disposition plan is not canonical")
	}
	plan.Canonical = canonical
	return plan, nil
}

func cacheControlDispositionPlanFromPreflight(
	preflight RequestPreflightPlan,
) (CacheControlDispositionPlan, error) {
	plan, err := parseCacheControlDispositionPlan(preflight.CacheControlPlanCanonical)
	if err != nil {
		return CacheControlDispositionPlan{}, err
	}
	if plan.Policy != preflight.UnsupportedOptionalFieldPolicy ||
		plan.RegistryVersion != preflight.CacheControlRegistryVersion ||
		plan.PreserveCount != preflight.CacheControlPreserveCount ||
		plan.DropCount != preflight.CacheControlDropCount ||
		strings.TrimSpace(preflight.CacheControlPlanFingerprint) == "" {
		return CacheControlDispositionPlan{}, errors.New("cache-control preflight binding is invalid")
	}
	plan.Fingerprint = preflight.CacheControlPlanFingerprint
	return plan, nil
}

func applyCacheControlDispositionPlan(
	request any,
	plan CacheControlDispositionPlan,
) (any, error) {
	if err := validateCacheControlPlanShape(plan); err != nil {
		return nil, err
	}
	if plan.DropCount == 0 {
		return request, nil
	}
	if plan.ClientFormat != types.RelayFormatClaude {
		return nil, errors.New("cache-control drop plan has an unsupported source format")
	}
	source, ok := typedClaudeRequest(request)
	if !ok || source == nil {
		return nil, errors.New("cache-control drop plan has no typed Messages request")
	}
	encoded, err := common.Marshal(source)
	if err != nil {
		return nil, fmt.Errorf("clone Messages request for cache-control disposition: %w", err)
	}
	var clone dto.ClaudeRequest
	if err := common.Unmarshal(encoded, &clone); err != nil {
		return nil, fmt.Errorf("bind cloned Messages request for cache-control disposition: %w", err)
	}
	for _, entry := range plan.Entries {
		if entry.Action != CacheControlActionDrop {
			continue
		}
		path, err := helperCacheControlPath(entry.NormalizedSourcePath)
		if err != nil {
			return nil, err
		}
		if err := deleteClaudeCacheControlAtPath(&clone, path); err != nil {
			return nil, err
		}
	}
	return &clone, nil
}

func deleteClaudeCacheControlAtPath(
	request *dto.ClaudeRequest,
	path []helper.JSONPathSegment,
) error {
	if request == nil {
		return errors.New("cache-control request clone is nil")
	}
	if len(path) == 1 && cachePathKeyEquals(path[0], "cache_control") {
		if len(request.CacheControl) == 0 {
			return errors.New("planned automatic cache-control field is absent from request clone")
		}
		request.CacheControl = nil
		return nil
	}
	if len(path) == 3 && cachePathKeyEquals(path[0], "system") && path[1].IsIndex &&
		cachePathKeyEquals(path[2], "cache_control") {
		parts, ok := request.System.([]any)
		if !ok || path[1].Index < 0 || path[1].Index >= len(parts) {
			return errors.New("planned system cache-control field cannot be located")
		}
		return deleteCacheControlMapMember(parts[path[1].Index])
	}
	if len(path) == 3 && cachePathKeyEquals(path[0], "tools") && path[1].IsIndex &&
		cachePathKeyEquals(path[2], "cache_control") {
		tools, ok := request.Tools.([]any)
		if !ok || path[1].Index < 0 || path[1].Index >= len(tools) {
			return errors.New("planned tool cache-control field cannot be located")
		}
		return deleteCacheControlMapMember(tools[path[1].Index])
	}
	if len(path) == 5 && cachePathKeyEquals(path[0], "messages") && path[1].IsIndex &&
		cachePathKeyEquals(path[2], "content") && path[3].IsIndex &&
		cachePathKeyEquals(path[4], "cache_control") {
		if path[1].Index < 0 || path[1].Index >= len(request.Messages) {
			return errors.New("planned message cache-control field cannot be located")
		}
		parts, ok := request.Messages[path[1].Index].Content.([]any)
		if !ok || path[3].Index < 0 || path[3].Index >= len(parts) {
			return errors.New("planned message content cache-control field cannot be located")
		}
		return deleteCacheControlMapMember(parts[path[3].Index])
	}
	return errors.New("planned cache-control source path is unsupported")
}

func deleteCacheControlMapMember(value any) error {
	object, ok := value.(map[string]any)
	if !ok {
		return errors.New("planned cache-control parent is not an object")
	}
	if _, present := object["cache_control"]; !present {
		return errors.New("planned cache-control field is absent from request clone")
	}
	delete(object, "cache_control")
	return nil
}

func (plan CacheControlDispositionPlan) hasNormalizedSourcePath(path []helper.JSONPathSegment) bool {
	want := cacheControlPathKey(path)
	for _, entry := range plan.Entries {
		candidate, err := helperCacheControlPath(entry.NormalizedSourcePath)
		if err == nil && cacheControlPathKey(candidate) == want {
			return true
		}
	}
	return false
}

func (plan CacheControlDispositionPlan) hasSourcePath(path []helper.JSONPathSegment) bool {
	want := cacheControlPathKey(path)
	for _, entry := range plan.Entries {
		candidate, err := helperCacheControlPath(entry.SourcePath)
		if err == nil && cacheControlPathKey(candidate) == want {
			return true
		}
	}
	return false
}

func (plan CacheControlDispositionPlan) dropsSourcePath(path []helper.JSONPathSegment) bool {
	want := cacheControlPathKey(path)
	for _, entry := range plan.Entries {
		if entry.Action != CacheControlActionDrop {
			continue
		}
		candidate, err := helperCacheControlPath(entry.SourcePath)
		if err == nil && cacheControlPathKey(candidate) == want {
			return true
		}
	}
	return false
}
