package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/channel/opencodego"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

const (
	openCodeCacheControlDiagnosticContextKey = "opencode_cache_control_diagnostics_v1"
	openCodeFullRequestDebugEnv              = "OPENCODE_DEBUG_FULL_REQUEST"
	openCodeFullRequestDebugMarker           = "OpenCode full inbound request debug:"
	openCodeFullRequestDebugFailureMarker    = "OpenCode full inbound request debug failed:"

	openCodeCacheControlCompatibilityRule    = "request.cache-control.compatibility-summary"
	openCodeCacheControlPhysicalAttemptStage = "relay.physical-attempt"
	openCodeCacheControlAttemptStatus        = "attempt"

	openCodeDiagnosticProtocolUnknown = "unknown"
	openCodeDiagnosticProtocolMixed   = "mixed"
	openCodeDiagnosticPolicyMixed     = "mixed"

	openCodeDiagnosticUnknownRule  = "gateway.preflight.unknown"
	openCodeDiagnosticUnknownStage = "preflight.unknown"

	openCodeDiagnosticMaxCandidates   = maxOpenCodeFinalizedCandidateCount
	openCodeDiagnosticMaxDispositions = maxOpenCodeFinalizedCandidateCount * 4
)

const (
	openCodeDiagnosticPolicyStrictMask uint8 = 1 << iota
	openCodeDiagnosticPolicyDropMask
	openCodeDiagnosticPolicyInvalidMask
)

const (
	openCodeDiagnosticProtocolChatMask uint8 = 1 << iota
	openCodeDiagnosticProtocolMessagesMask
	openCodeDiagnosticProtocolResponsesMask
)

type openCodeCacheControlDiagnosticState struct {
	mu sync.Mutex

	channelType    int
	clientProtocol string
	fallbackPolicy string

	candidateCount       int
	policyMask           uint8
	routingCount         int
	unknownRoutingCount  int
	finalProtocolMask    uint8
	preserveCount        int
	dropCount            int
	rejectionLogged      bool
	compatibilityLogKeys map[string]struct{}
}

type openCodeCacheControlDiagnosticSnapshot struct {
	channelType    int
	clientProtocol string
	finalProtocol  string
	policy         string
	candidateCount int
	preserveCount  int
	dropCount      int
}

func initializeOpenCodeCacheControlDiagnostics(c *gin.Context, info *relaycommon.RelayInfo) {
	if c == nil || info == nil {
		return
	}
	channelType := common.GetContextKeyInt(c, constant.ContextKeyChannelType)
	if !constant.IsOpenCodeChannelType(channelType) {
		return
	}
	otherSettings, _ := common.GetContextKeyType[dto.ChannelOtherSettings](c, constant.ContextKeyChannelOtherSetting)
	fallbackPolicy := boundedOpenCodeDiagnosticPolicy(otherSettings.OpenCodeGo)
	if channelType == constant.ChannelTypeOpenCodeAPIKey {
		// A multi-candidate request may not use the initially selected row. Until
		// candidate enumeration records the complete set, do not attribute that
		// row's policy to an all-rejected result.
		fallbackPolicy = openCodeDiagnosticPolicyMixed
	}
	c.Set(openCodeCacheControlDiagnosticContextKey, &openCodeCacheControlDiagnosticState{
		channelType:          channelType,
		clientProtocol:       boundedOpenCodeClientProtocol(info.RelayFormat),
		fallbackPolicy:       fallbackPolicy,
		compatibilityLogKeys: make(map[string]struct{}),
	})
}

func getOpenCodeCacheControlDiagnosticState(c *gin.Context) (*openCodeCacheControlDiagnosticState, bool) {
	if c == nil {
		return nil, false
	}
	value, found := c.Get(openCodeCacheControlDiagnosticContextKey)
	state, ok := value.(*openCodeCacheControlDiagnosticState)
	return state, found && ok && state != nil
}

func recordOpenCodeDiagnosticCandidate(
	c *gin.Context,
	config *dto.OpenCodeGoConfig,
	settingsValid bool,
) {
	state, found := getOpenCodeCacheControlDiagnosticState(c)
	if !found {
		return
	}
	policy := openCodeDiagnosticPolicyMixed
	if settingsValid {
		policy = boundedOpenCodeDiagnosticPolicy(config)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.candidateCount = boundedOpenCodeDiagnosticCount(state.candidateCount + 1)
	state.policyMask |= openCodeDiagnosticPolicyMask(policy)
}

func recordOpenCodeDiagnosticCandidateFromContext(c *gin.Context) {
	otherSettings, found := common.GetContextKeyType[dto.ChannelOtherSettings](c, constant.ContextKeyChannelOtherSetting)
	recordOpenCodeDiagnosticCandidate(c, otherSettings.OpenCodeGo, found)
}

func recordOpenCodeDiagnosticRouting(
	root *gin.Context,
	candidate *gin.Context,
	info *relaycommon.RelayInfo,
) {
	state, found := getOpenCodeCacheControlDiagnosticState(root)
	if !found || candidate == nil || info == nil {
		return
	}
	protocol := openCodeDiagnosticProtocolUnknown
	envelope, envelopeFound, err := helper.GetValidatedRequestEnvelope(candidate, info.RelayFormat)
	if err == nil && envelopeFound && envelope != nil {
		mapping, mappingErr := helper.ResolveModelMapping(envelope.OriginalModel(), candidate.GetString("model_mapping"))
		if mappingErr == nil {
			otherSettings, settingsFound := common.GetContextKeyType[dto.ChannelOtherSettings](
				candidate,
				constant.ContextKeyChannelOtherSetting,
			)
			if settingsFound {
				resolution, resolutionErr := opencodego.ResolveProtocolWithSource(mapping.FinalModel, otherSettings.OpenCodeGo)
				if resolutionErr == nil {
					protocol = boundedOpenCodeFinalProtocol(resolution.Protocol)
				}
			}
		}
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	state.routingCount = boundedOpenCodeDiagnosticCount(state.routingCount + 1)
	if protocol == openCodeDiagnosticProtocolUnknown {
		state.unknownRoutingCount = boundedOpenCodeDiagnosticCount(state.unknownRoutingCount + 1)
	}
	state.finalProtocolMask |= openCodeDiagnosticProtocolMask(protocol)
}

func recordOpenCodeDiagnosticPlan(c *gin.Context, plan opencodego.RequestPreflightPlan) {
	state, found := getOpenCodeCacheControlDiagnosticState(c)
	if !found {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.preserveCount = boundedOpenCodeDiagnosticCountWithLimit(
		state.preserveCount+plan.CacheControlPreserveCount,
		openCodeDiagnosticMaxDispositions,
	)
	state.dropCount = boundedOpenCodeDiagnosticCountWithLimit(
		state.dropCount+plan.CacheControlDropCount,
		openCodeDiagnosticMaxDispositions,
	)
}

func logOpenCodeRequestPreflightRejection(
	c *gin.Context,
	ruleID string,
	stageID string,
	statusCode int,
) {
	if c == nil || !constant.IsOpenCodeChannelType(common.GetContextKeyInt(c, constant.ContextKeyChannelType)) {
		return
	}
	state, found := getOpenCodeCacheControlDiagnosticState(c)
	if !found {
		otherSettings, _ := common.GetContextKeyType[dto.ChannelOtherSettings](c, constant.ContextKeyChannelOtherSetting)
		fallbackPolicy := boundedOpenCodeDiagnosticPolicy(otherSettings.OpenCodeGo)
		if common.GetContextKeyInt(c, constant.ContextKeyChannelType) == constant.ChannelTypeOpenCodeAPIKey {
			fallbackPolicy = openCodeDiagnosticPolicyMixed
		}
		state = &openCodeCacheControlDiagnosticState{
			channelType:          common.GetContextKeyInt(c, constant.ContextKeyChannelType),
			clientProtocol:       openCodeDiagnosticProtocolUnknown,
			fallbackPolicy:       fallbackPolicy,
			compatibilityLogKeys: make(map[string]struct{}),
		}
		c.Set(openCodeCacheControlDiagnosticContextKey, state)
	}

	state.mu.Lock()
	if state.rejectionLogged {
		state.mu.Unlock()
		return
	}
	state.rejectionLogged = true
	snapshot := state.snapshotLocked()
	state.mu.Unlock()

	statusCode = boundedOpenCodeDiagnosticHTTPStatus(statusCode)
	ruleID = boundedOpenCodeDiagnosticRule(ruleID)
	stageID = boundedOpenCodeDiagnosticStage(stageID)
	logOpenCodeFullRequestDebug(c, ruleID, stageID)
	logger.LogWarn(context.Background(), fmt.Sprintf(
		"OpenCode request preflight rejected: rule_id=%s stage=%s status=%d client_protocol=%s final_protocol=%s channel_type=%d policy=%s candidate_count=%d preserve_count=%d drop_count=%d",
		ruleID,
		stageID,
		statusCode,
		snapshot.clientProtocol,
		snapshot.finalProtocol,
		snapshot.channelType,
		snapshot.policy,
		snapshot.candidateCount,
		snapshot.preserveCount,
		snapshot.dropCount,
	))
}

func logOpenCodeFullRequestDebug(c *gin.Context, ruleID string, stageID string) {
	if c == nil || !common.GetEnvOrDefaultBool(openCodeFullRequestDebugEnv, false) {
		return
	}
	logContext := openCodeFullRequestLogContext(c)
	dump, err := dumpOpenCodeFullRequest(c)
	if err != nil {
		logger.LogWarn(logContext, fmt.Sprintf(
			"%s rule_id=%s stage=%s error=%s",
			openCodeFullRequestDebugFailureMarker,
			ruleID,
			stageID,
			common.LocalLogPreview(common.MaskSensitiveInfo(err.Error())),
		))
		return
	}
	logger.LogWarn(logContext, fmt.Sprintf(
		"%s rule_id=%s stage=%s\n%s",
		openCodeFullRequestDebugMarker,
		ruleID,
		stageID,
		dump,
	))
}

func dumpOpenCodeFullRequest(c *gin.Context) ([]byte, error) {
	if c == nil || c.Request == nil {
		return nil, fmt.Errorf("request is unavailable")
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, fmt.Errorf("get request body storage: %w", err)
	}
	body, err := storage.NewReader()
	if err != nil {
		return nil, fmt.Errorf("open request body reader: %w", err)
	}
	defer body.Close()

	request := c.Request.Clone(c.Request.Context())
	request.Body = body
	request.GetBody = nil
	request.RequestURI = request.URL.RequestURI()
	dump, err := httputil.DumpRequest(request, true)
	if err != nil {
		return nil, fmt.Errorf("dump request: %w", err)
	}
	return dump, nil
}

func openCodeFullRequestLogContext(c *gin.Context) context.Context {
	if c == nil {
		return context.Background()
	}
	if c.Request != nil {
		requestContext := c.Request.Context()
		if requestContext.Value(common.RequestIdKey) != nil {
			return requestContext
		}
	}
	if requestID := strings.TrimSpace(c.GetString(common.RequestIdKey)); requestID != "" {
		return context.WithValue(context.Background(), common.RequestIdKey, requestID)
	}
	return context.Background()
}

func logOpenCodeCacheControlCompatibilityAttempt(c *gin.Context) {
	if c == nil || !constant.IsOpenCodeChannelType(common.GetContextKeyInt(c, constant.ContextKeyChannelType)) {
		return
	}
	plan, found, err := opencodego.GetRequestPreflightPlan(c)
	if err != nil || !found ||
		plan.UnsupportedOptionalFieldPolicy != dto.OpenCodeGoUnsupportedOptionalFieldDropKnown ||
		strings.TrimSpace(plan.CacheControlPlanFingerprint) == "" {
		return
	}
	state, stateFound := getOpenCodeCacheControlDiagnosticState(c)
	if !stateFound {
		state = &openCodeCacheControlDiagnosticState{
			channelType:          plan.ChannelType,
			clientProtocol:       boundedOpenCodeClientProtocol(plan.ClientFormat),
			fallbackPolicy:       dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
			candidateCount:       1,
			policyMask:           openCodeDiagnosticPolicyDropMask,
			routingCount:         1,
			finalProtocolMask:    openCodeDiagnosticProtocolMask(boundedOpenCodeFinalProtocol(plan.FinalProtocol)),
			compatibilityLogKeys: make(map[string]struct{}),
		}
		c.Set(openCodeCacheControlDiagnosticContextKey, state)
	}

	dedupKey := openCodeCacheControlCompatibilityRule + "\x00" + plan.CacheControlPlanFingerprint
	state.mu.Lock()
	if state.compatibilityLogKeys == nil {
		state.compatibilityLogKeys = make(map[string]struct{})
	}
	if _, logged := state.compatibilityLogKeys[dedupKey]; logged {
		state.mu.Unlock()
		return
	}
	state.compatibilityLogKeys[dedupKey] = struct{}{}
	state.mu.Unlock()

	logger.LogInfo(context.Background(), fmt.Sprintf(
		"OpenCode request compatibility attempt: rule_id=%s stage=%s status=%s registry_version=%s client_protocol=%s final_protocol=%s channel_type=%d policy=%s candidate_count=%d preserve_count=%d drop_count=%d",
		openCodeCacheControlCompatibilityRule,
		openCodeCacheControlPhysicalAttemptStage,
		openCodeCacheControlAttemptStatus,
		boundedOpenCodeCacheControlRegistryVersion(plan.CacheControlRegistryVersion),
		boundedOpenCodeClientProtocol(plan.ClientFormat),
		boundedOpenCodeFinalProtocol(plan.FinalProtocol),
		boundedOpenCodeDiagnosticChannelType(plan.ChannelType),
		dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
		1,
		boundedOpenCodeDiagnosticCountWithLimit(plan.CacheControlPreserveCount, openCodeDiagnosticMaxDispositions),
		boundedOpenCodeDiagnosticCountWithLimit(plan.CacheControlDropCount, openCodeDiagnosticMaxDispositions),
	))
}

func relayOpenCodePhysicalAttempt(c *gin.Context, attempt func() *types.NewAPIError) *types.NewAPIError {
	if attempt == nil {
		return nil
	}
	logOpenCodeCacheControlCompatibilityAttempt(c)
	return attempt()
}

func (state *openCodeCacheControlDiagnosticState) snapshotLocked() openCodeCacheControlDiagnosticSnapshot {
	policy := state.fallbackPolicy
	switch state.policyMask {
	case openCodeDiagnosticPolicyStrictMask:
		policy = dto.OpenCodeGoUnsupportedOptionalFieldStrict
	case openCodeDiagnosticPolicyDropMask:
		policy = dto.OpenCodeGoUnsupportedOptionalFieldDropKnown
	case 0:
	default:
		policy = openCodeDiagnosticPolicyMixed
	}
	if policy != dto.OpenCodeGoUnsupportedOptionalFieldStrict &&
		policy != dto.OpenCodeGoUnsupportedOptionalFieldDropKnown {
		policy = openCodeDiagnosticPolicyMixed
	}

	finalProtocol := openCodeDiagnosticProtocolUnknown
	switch state.finalProtocolMask {
	case openCodeDiagnosticProtocolChatMask:
		finalProtocol = string(opencodego.ProtocolChat)
	case openCodeDiagnosticProtocolMessagesMask:
		finalProtocol = string(opencodego.ProtocolMessages)
	case openCodeDiagnosticProtocolResponsesMask:
		finalProtocol = string(opencodego.ProtocolResponses)
	case 0:
	default:
		finalProtocol = openCodeDiagnosticProtocolMixed
	}
	if (state.routingCount < state.candidateCount && state.routingCount > 0) ||
		(state.unknownRoutingCount > 0 && state.finalProtocolMask != 0) {
		finalProtocol = openCodeDiagnosticProtocolMixed
	}

	return openCodeCacheControlDiagnosticSnapshot{
		channelType:    boundedOpenCodeDiagnosticChannelType(state.channelType),
		clientProtocol: boundedOpenCodeDiagnosticClientProtocol(state.clientProtocol),
		finalProtocol:  finalProtocol,
		policy:         policy,
		candidateCount: boundedOpenCodeDiagnosticCount(state.candidateCount),
		preserveCount:  boundedOpenCodeDiagnosticCountWithLimit(state.preserveCount, openCodeDiagnosticMaxDispositions),
		dropCount:      boundedOpenCodeDiagnosticCountWithLimit(state.dropCount, openCodeDiagnosticMaxDispositions),
	}
}

func boundedOpenCodeCacheControlRegistryVersion(value string) string {
	if strings.TrimSpace(value) == opencodego.CacheControlRegistryVersion {
		return opencodego.CacheControlRegistryVersion
	}
	return openCodeDiagnosticProtocolUnknown
}

func boundedOpenCodeClientProtocol(format types.RelayFormat) string {
	switch format {
	case types.RelayFormatClaude:
		return string(opencodego.ProtocolMessages)
	case types.RelayFormatOpenAI:
		return string(opencodego.ProtocolChat)
	case types.RelayFormatOpenAIResponses:
		return string(opencodego.ProtocolResponses)
	default:
		return openCodeDiagnosticProtocolUnknown
	}
}

func boundedOpenCodeDiagnosticClientProtocol(protocol string) string {
	switch protocol {
	case string(opencodego.ProtocolChat), string(opencodego.ProtocolMessages), string(opencodego.ProtocolResponses):
		return protocol
	default:
		return openCodeDiagnosticProtocolUnknown
	}
}

func boundedOpenCodeFinalProtocol(protocol opencodego.Protocol) string {
	switch protocol {
	case opencodego.ProtocolChat, opencodego.ProtocolMessages, opencodego.ProtocolResponses:
		return string(protocol)
	default:
		return openCodeDiagnosticProtocolUnknown
	}
}

func boundedOpenCodeDiagnosticPolicy(config *dto.OpenCodeGoConfig) string {
	policy := config.EffectiveUnsupportedOptionalFieldPolicy()
	switch policy {
	case dto.OpenCodeGoUnsupportedOptionalFieldStrict, dto.OpenCodeGoUnsupportedOptionalFieldDropKnown:
		return policy
	default:
		return openCodeDiagnosticPolicyMixed
	}
}

func openCodeDiagnosticPolicyMask(policy string) uint8 {
	switch policy {
	case dto.OpenCodeGoUnsupportedOptionalFieldStrict:
		return openCodeDiagnosticPolicyStrictMask
	case dto.OpenCodeGoUnsupportedOptionalFieldDropKnown:
		return openCodeDiagnosticPolicyDropMask
	default:
		return openCodeDiagnosticPolicyInvalidMask
	}
}

func openCodeDiagnosticProtocolMask(protocol string) uint8 {
	switch protocol {
	case string(opencodego.ProtocolChat):
		return openCodeDiagnosticProtocolChatMask
	case string(opencodego.ProtocolMessages):
		return openCodeDiagnosticProtocolMessagesMask
	case string(opencodego.ProtocolResponses):
		return openCodeDiagnosticProtocolResponsesMask
	default:
		return 0
	}
}

func boundedOpenCodeDiagnosticRule(value string) string {
	switch strings.TrimSpace(value) {
	case openCodeCapabilityUnsupportedRule,
		openCodeCapabilityUnknownRule,
		helper.OpenCodeGLM53ThinkingDisabledRule,
		opencodego.PreflightEnvelopeInvariantRule,
		opencodego.PreflightModelMappingInvalidRule,
		opencodego.PreflightProtocolConfigInvalidRule,
		opencodego.PreflightCandidateConfigInvalidRule,
		opencodego.PreflightPlanMismatchRule,
		opencodego.MessagesStopSourceCollisionRule,
		opencodego.EffortSelectorShapeRule,
		opencodego.EffortSelectorCollisionRule,
		opencodego.EffortSelectorCrossNullRule,
		opencodego.ClaudeMetadataShapeRule,
		opencodego.ClaudeMetadataTargetLimitRule,
		opencodego.ClaudeOutputConfigShapeRule,
		opencodego.ClaudeOutputConfigNullRule,
		opencodego.ClaudeOutputConfigUnsupportedRule,
		opencodego.ClaudeContextManagementActiveRule,
		opencodego.ClaudeContextManagementShapeRule,
		opencodego.ChatMetadataShapeRule,
		opencodego.RequestContractUnclassifiedPathRule,
		opencodego.RequestContractUnmappedPathRule,
		opencodego.RequestContractTypedPathRule,
		opencodego.RequestContractLocalPathRule,
		opencodego.RequestContractMessagesRawPathRule,
		opencodego.RequestContractThinkingBudgetRule,
		opencodego.RequestContractKimiTemperatureRule,
		opencodego.RequestContractPreserveConflictRule,
		opencodego.RequestContractTargetCollisionRule,
		opencodego.RequestContractUnmappedNestedRule,
		opencodego.CacheControlShapeRule,
		opencodego.CacheControlPathRule,
		opencodego.CacheControlParentRule,
		opencodego.CacheControlBreakpointLimitRule,
		opencodego.CacheControlTTLOrderRule,
		opencodego.CacheControlAutomaticTTLRule,
		opencodego.CacheControlAutomaticSlotRule,
		opencodego.CacheControlUnsupportedRule,
		opencodego.CacheControlPlanMismatchRule,
		opencodego.CacheControlPreserveMutationRule,
		opencodego.CacheControlDropAssertionRule,
		opencodego.CacheControlUnexpectedMarkerRule:
		return strings.TrimSpace(value)
	default:
		return openCodeDiagnosticUnknownRule
	}
}

func boundedOpenCodeDiagnosticStage(value string) string {
	switch strings.TrimSpace(value) {
	case openCodeCapabilityStage,
		opencodego.PreflightRoutingStage,
		opencodego.PreflightAssertionStage,
		opencodego.RequestContractPreflightStage,
		opencodego.EffortSelectorPreflightStage,
		opencodego.ClaudeFieldContractPreflightStage,
		opencodego.ChatMetadataFinalizationStage,
		opencodego.CacheControlPreflightStage,
		opencodego.CacheControlFinalizerStage:
		return strings.TrimSpace(value)
	default:
		return openCodeDiagnosticUnknownStage
	}
}

func boundedOpenCodeDiagnosticHTTPStatus(statusCode int) int {
	if statusCode < http.StatusBadRequest || statusCode > 599 {
		return http.StatusInternalServerError
	}
	return statusCode
}

func boundedOpenCodeDiagnosticChannelType(channelType int) int {
	if constant.IsOpenCodeChannelType(channelType) {
		return channelType
	}
	return 0
}

func boundedOpenCodeDiagnosticCount(count int) int {
	return boundedOpenCodeDiagnosticCountWithLimit(count, openCodeDiagnosticMaxCandidates)
}

func boundedOpenCodeDiagnosticCountWithLimit(count int, limit int) int {
	if count < 0 {
		return 0
	}
	if count > limit {
		return limit
	}
	return count
}
