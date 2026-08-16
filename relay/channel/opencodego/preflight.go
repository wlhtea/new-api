package opencodego

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

const (
	requestPreflightContextKey          = "opencodego_request_preflight_v1"
	requestPreflightRejectionContextKey = "opencodego_request_preflight_rejection_v1"
	requestPreflightVersion             = "opencodego-request-preflight-v2"

	PreflightRoutingStage               = "preflight.routing"
	PreflightAssertionStage             = "preflight.assertion"
	PreflightEnvelopeInvariantRule      = "gateway.envelope.invalid"
	PreflightModelMappingInvalidRule    = "gateway.model-mapping.invalid"
	PreflightProtocolConfigInvalidRule  = "gateway.protocol-routing.invalid"
	PreflightCandidateConfigInvalidRule = "gateway.channel-config.invalid"
	PreflightPlanMismatchRule           = "gateway.preflight-plan.mismatch"

	DynamicProtocolReasonChatFunctionTools = "chat-function-tools-without-assistant-reasoning"
)

type RequestPreflightPlan struct {
	Version            string
	ChannelType        int
	ChannelID          int
	SelectionGroup     string
	ClientFormat       types.RelayFormat
	OriginModel        string
	FinalModel         string
	ModelMapped        bool
	BaseProtocol       Protocol
	FinalProtocol      Protocol
	ProtocolSource     ProtocolResolutionSource
	DynamicReason      string
	ConfigFingerprint  string
	CapabilityRevision string
}

// RequestPreflightPlanKey identifies one frozen routing candidate. Channel IDs
// alone are insufficient because an auto-group request may select the same row
// through more than one concrete group.
type RequestPreflightPlanKey struct {
	SelectionGroup string
	ChannelID      int
}

type requestPreflightPlanRegistry struct {
	Version string
	Plans   map[RequestPreflightPlanKey]RequestPreflightPlan
}

type RequestPreflightError struct {
	StatusCode int
	Origin     types.ErrorOrigin
	RuleID     string
	StageID    string
	Message    string
	cause      error
}

// RequestPreflightRejection is safe request-scoped evidence for deterministic
// tests and local audit. It intentionally contains no client path or value.
type RequestPreflightRejection struct {
	RuleID  string
	StageID string
}

func (e *RequestPreflightError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *RequestPreflightError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func AsRequestPreflightError(err error) (*RequestPreflightError, bool) {
	var preflightErr *RequestPreflightError
	ok := errors.As(err, &preflightErr)
	return preflightErr, ok
}

func StoreRequestPreflightRejection(c *gin.Context, rejection RequestPreflightRejection) error {
	if c == nil || strings.TrimSpace(rejection.RuleID) == "" || strings.TrimSpace(rejection.StageID) == "" {
		return errors.New("OpenCode request preflight rejection evidence is invalid")
	}
	c.Set(requestPreflightRejectionContextKey, rejection)
	return nil
}

func GetRequestPreflightRejection(c *gin.Context) (RequestPreflightRejection, bool) {
	if c == nil {
		return RequestPreflightRejection{}, false
	}
	value, found := c.Get(requestPreflightRejectionContextKey)
	if !found {
		return RequestPreflightRejection{}, false
	}
	rejection, ok := value.(RequestPreflightRejection)
	if !ok || strings.TrimSpace(rejection.RuleID) == "" || strings.TrimSpace(rejection.StageID) == "" {
		return RequestPreflightRejection{}, false
	}
	return rejection, true
}

func BuildRequestPreflightPlan(c *gin.Context, info *relaycommon.RelayInfo) (RequestPreflightPlan, error) {
	if c == nil || info == nil {
		return RequestPreflightPlan{}, newPreflightInvariantError(errors.New("OpenCode request preflight input is missing"))
	}
	channelType := common.GetContextKeyInt(c, constant.ContextKeyChannelType)
	if !constant.IsOpenCodeChannelType(channelType) {
		return RequestPreflightPlan{}, newPreflightInvariantError(errors.New("OpenCode request preflight channel type is invalid"))
	}

	envelope, found, err := helper.GetValidatedRequestEnvelope(c, info.RelayFormat)
	if err != nil {
		return RequestPreflightPlan{}, newPreflightInvariantError(err)
	}
	if !found || envelope == nil {
		return RequestPreflightPlan{}, newPreflightInvariantError(errors.New("validated request envelope is unavailable"))
	}

	mapping, err := helper.ResolveModelMapping(envelope.OriginalModel(), c.GetString("model_mapping"))
	if err != nil {
		return RequestPreflightPlan{}, &RequestPreflightError{
			StatusCode: http.StatusServiceUnavailable,
			Origin:     types.ErrorOriginGatewayConfig,
			RuleID:     PreflightModelMappingInvalidRule,
			StageID:    PreflightRoutingStage,
			Message:    "OpenCode model routing configuration is invalid",
			cause:      err,
		}
	}

	otherSettings, _ := common.GetContextKeyType[dto.ChannelOtherSettings](c, constant.ContextKeyChannelOtherSetting)
	protocol, err := ResolveProtocolWithSource(mapping.FinalModel, otherSettings.OpenCodeGo)
	if err != nil {
		return RequestPreflightPlan{}, &RequestPreflightError{
			StatusCode: http.StatusServiceUnavailable,
			Origin:     types.ErrorOriginGatewayConfig,
			RuleID:     PreflightProtocolConfigInvalidRule,
			StageID:    PreflightRoutingStage,
			Message:    "OpenCode protocol routing configuration is invalid",
			cause:      err,
		}
	}

	finalProtocol, dynamicReason := effectiveRequestProtocol(protocol.Protocol, info.RelayFormat, info.Request)
	if err := ValidateMessagesStopSourceCollision(envelope, info.RelayFormat); err != nil {
		if preflightErr, ok := newRequestPreflightClientError(err); ok {
			return RequestPreflightPlan{}, preflightErr
		}
		return RequestPreflightPlan{}, newPreflightInvariantError(err)
	}
	if err := helper.ValidateOpenCodeModelCapability(envelope, mapping.FinalModel, string(finalProtocol)); err != nil {
		if preflightErr, ok := newRequestPreflightClientError(err); ok {
			return RequestPreflightPlan{}, preflightErr
		}
		return RequestPreflightPlan{}, newPreflightInvariantError(err)
	}
	if err := ValidateRequestModelFieldContracts(envelope, mapping.FinalModel); err != nil {
		if preflightErr, ok := newRequestPreflightClientError(err); ok {
			return RequestPreflightPlan{}, preflightErr
		}
		return RequestPreflightPlan{}, newPreflightInvariantError(err)
	}
	if err := ValidateRequestPathContracts(envelope, info.RelayFormat, finalProtocol, info.Request); err != nil {
		if preflightErr, ok := newRequestPreflightClientError(err); ok {
			return RequestPreflightPlan{}, preflightErr
		}
		return RequestPreflightPlan{}, newPreflightInvariantError(err)
	}

	fingerprint, err := requestPreflightConfigFingerprint(c.GetString("model_mapping"), otherSettings.OpenCodeGo)
	if err != nil {
		return RequestPreflightPlan{}, newPreflightInvariantError(err)
	}
	return RequestPreflightPlan{
		Version:           requestPreflightVersion,
		ChannelType:       channelType,
		ChannelID:         common.GetContextKeyInt(c, constant.ContextKeyChannelId),
		SelectionGroup:    relaycommon.ResolveSelectionGroup(c, info),
		ClientFormat:      info.RelayFormat,
		OriginModel:       mapping.OriginModel,
		FinalModel:        mapping.FinalModel,
		ModelMapped:       mapping.Mapped,
		BaseProtocol:      protocol.Protocol,
		FinalProtocol:     finalProtocol,
		ProtocolSource:    protocol.Source,
		DynamicReason:     dynamicReason,
		ConfigFingerprint: fingerprint,
	}, nil
}

func newRequestPreflightClientError(err error) (*RequestPreflightError, bool) {
	validationErr, ok := helper.AsClientRequestValidationError(err)
	if !ok {
		return nil, false
	}
	return &RequestPreflightError{
		StatusCode: validationErr.StatusCode,
		Origin:     types.ErrorOriginLocalValidation,
		RuleID:     validationErr.RuleID,
		StageID:    validationErr.StageID,
		Message:    validationErr.Message,
		cause:      err,
	}, true
}

func StoreRequestPreflightPlan(c *gin.Context, plan RequestPreflightPlan) error {
	return StoreRequestPreflightPlans(c, []RequestPreflightPlan{plan})
}

// StoreRequestPreflightPlans replaces the request's immutable candidate-plan
// registry. Duplicate selection keys are accepted only when their plans are
// identical, so a retry cannot silently bind one row/group pair to two configs.
func StoreRequestPreflightPlans(c *gin.Context, plans []RequestPreflightPlan) error {
	if c == nil || len(plans) == 0 {
		return errors.New("OpenCode request preflight plan is invalid")
	}
	registry := requestPreflightPlanRegistry{
		Version: requestPreflightVersion,
		Plans:   make(map[RequestPreflightPlanKey]RequestPreflightPlan, len(plans)),
	}
	for _, plan := range plans {
		if plan.Version != requestPreflightVersion || plan.ChannelID <= 0 || plan.ConfigFingerprint == "" {
			return errors.New("OpenCode request preflight plan is invalid")
		}
		key := plan.Key()
		if existing, found := registry.Plans[key]; found && existing != plan {
			return errors.New("OpenCode request preflight plans conflict for one selection")
		}
		registry.Plans[key] = plan
	}
	c.Set(requestPreflightContextKey, registry)
	return nil
}

func NewRequestPreflightPlanStorageError(cause error) error {
	return newPreflightPlanMismatchError(cause)
}

func NewRequestPreflightCandidateConfigError(cause error) error {
	return &RequestPreflightError{
		StatusCode: http.StatusServiceUnavailable,
		Origin:     types.ErrorOriginGatewayConfig,
		RuleID:     PreflightCandidateConfigInvalidRule,
		StageID:    PreflightRoutingStage,
		Message:    "OpenCode channel configuration is invalid",
		cause:      cause,
	}
}

// NewRequestPreflightFinalizationError preserves client-caused contract
// failures discovered while materializing a candidate. All other finalizer
// failures describe frozen operator configuration or gateway invariants.
func NewRequestPreflightFinalizationError(cause error) error {
	if validationErr, ok := helper.AsClientRequestValidationError(cause); ok {
		return &RequestPreflightError{
			StatusCode: validationErr.StatusCode,
			Origin:     types.ErrorOriginLocalValidation,
			RuleID:     validationErr.RuleID,
			StageID:    validationErr.StageID,
			Message:    validationErr.Message,
			cause:      cause,
		}
	}
	return NewRequestPreflightCandidateConfigError(cause)
}

func GetRequestPreflightPlan(c *gin.Context) (RequestPreflightPlan, bool, error) {
	if c == nil {
		return RequestPreflightPlan{}, false, errors.New("OpenCode request context is nil")
	}
	return GetRequestPreflightPlanForSelection(
		c,
		relaycommon.ResolveSelectionGroup(c, nil),
		common.GetContextKeyInt(c, constant.ContextKeyChannelId),
	)
}

// GetRequestPreflightPlanForSelection returns the plan for one exact frozen
// row/group candidate.
func GetRequestPreflightPlanForSelection(
	c *gin.Context,
	selectionGroup string,
	channelID int,
) (RequestPreflightPlan, bool, error) {
	plan, found, _, err := getRequestPreflightPlanForSelection(c, selectionGroup, channelID)
	return plan, found, err
}

func getRequestPreflightPlanForSelection(
	c *gin.Context,
	selectionGroup string,
	channelID int,
) (RequestPreflightPlan, bool, bool, error) {
	if c == nil {
		return RequestPreflightPlan{}, false, false, errors.New("OpenCode request context is nil")
	}
	value, found := c.Get(requestPreflightContextKey)
	if !found {
		return RequestPreflightPlan{}, false, false, nil
	}
	registry, ok := value.(requestPreflightPlanRegistry)
	if !ok || registry.Version != requestPreflightVersion || len(registry.Plans) == 0 {
		return RequestPreflightPlan{}, false, true, errors.New("OpenCode request preflight plan is corrupt")
	}
	plan, planFound := registry.Plans[RequestPreflightPlanKey{
		SelectionGroup: strings.TrimSpace(selectionGroup),
		ChannelID:      channelID,
	}]
	if !planFound {
		return RequestPreflightPlan{}, false, true, nil
	}
	if plan.Version != requestPreflightVersion || plan.ChannelID != channelID ||
		plan.SelectionGroup != strings.TrimSpace(selectionGroup) || plan.ConfigFingerprint == "" {
		return RequestPreflightPlan{}, false, true, errors.New("OpenCode request preflight plan is corrupt")
	}
	return plan, true, true, nil
}

func (plan RequestPreflightPlan) Key() RequestPreflightPlanKey {
	return RequestPreflightPlanKey{
		SelectionGroup: strings.TrimSpace(plan.SelectionGroup),
		ChannelID:      plan.ChannelID,
	}
}

// AssertRequestPreflightPlan proves that the immutable plan built before
// billing still describes the attempt about to be converted. Direct adaptor
// unit tests without a validated envelope retain their legacy seam; a real
// validated request may never continue without a stored plan.
func AssertRequestPreflightPlan(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	request any,
) (RequestPreflightPlan, bool, error) {
	selectionGroup := ""
	channelID := 0
	if info != nil && info.ChannelMeta != nil {
		selectionGroup = info.SelectionGroup
		channelID = info.ChannelId
	}
	plan, found, registryFound, err := getRequestPreflightPlanForSelection(c, selectionGroup, channelID)
	if err != nil {
		return RequestPreflightPlan{}, found, newPreflightPlanMismatchError(err)
	}
	if !found {
		if registryFound {
			return RequestPreflightPlan{}, true, newPreflightPlanMismatchError(errors.New("selected OpenCode attempt has no preflight plan"))
		}
		if info == nil {
			return RequestPreflightPlan{}, false, nil
		}
		_, envelopeFound, envelopeErr := helper.GetValidatedRequestEnvelope(c, info.RelayFormat)
		if envelopeErr != nil {
			return RequestPreflightPlan{}, false, newPreflightPlanMismatchError(envelopeErr)
		}
		if envelopeFound {
			return RequestPreflightPlan{}, false, newPreflightPlanMismatchError(errors.New("validated OpenCode request has no preflight plan"))
		}
		return RequestPreflightPlan{}, false, nil
	}
	if info == nil || info.ChannelMeta == nil {
		return RequestPreflightPlan{}, true, newPreflightPlanMismatchError(errors.New("OpenCode attempt metadata is unavailable"))
	}

	envelope, envelopeFound, err := helper.GetValidatedRequestEnvelope(c, info.RelayFormat)
	if err != nil || !envelopeFound || envelope == nil {
		if err == nil {
			err = errors.New("validated OpenCode request envelope is unavailable")
		}
		return RequestPreflightPlan{}, true, newPreflightPlanMismatchError(err)
	}
	mapping, err := helper.ResolveModelMapping(envelope.OriginalModel(), c.GetString("model_mapping"))
	if err != nil {
		return RequestPreflightPlan{}, true, newPreflightPlanMismatchError(err)
	}
	protocol, err := ResolveProtocolWithSource(mapping.FinalModel, info.ChannelOtherSettings.OpenCodeGo)
	if err != nil {
		return RequestPreflightPlan{}, true, newPreflightPlanMismatchError(err)
	}
	finalProtocol, dynamicReason := effectiveRequestProtocol(protocol.Protocol, info.RelayFormat, request)
	fingerprint, err := requestPreflightConfigFingerprint(c.GetString("model_mapping"), info.ChannelOtherSettings.OpenCodeGo)
	if err != nil {
		return RequestPreflightPlan{}, true, newPreflightPlanMismatchError(err)
	}

	if plan.ChannelType != info.ChannelType ||
		plan.ChannelID != info.ChannelId ||
		plan.SelectionGroup != info.SelectionGroup ||
		plan.SelectionGroup != relaycommon.ResolveSelectionGroup(c, info) ||
		plan.ClientFormat != info.RelayFormat ||
		plan.OriginModel != mapping.OriginModel ||
		plan.FinalModel != mapping.FinalModel ||
		plan.ModelMapped != mapping.Mapped ||
		plan.BaseProtocol != protocol.Protocol ||
		plan.FinalProtocol != finalProtocol ||
		plan.ProtocolSource != protocol.Source ||
		plan.DynamicReason != dynamicReason ||
		plan.ConfigFingerprint != fingerprint ||
		info.OriginModelName != plan.OriginModel ||
		info.UpstreamModelName != plan.FinalModel ||
		info.IsModelMapped != plan.ModelMapped {
		return RequestPreflightPlan{}, true, newPreflightPlanMismatchError(errors.New("OpenCode request preflight plan no longer matches the selected attempt"))
	}
	return plan, true, nil
}

func effectiveRequestProtocol(base Protocol, clientFormat types.RelayFormat, request any) (Protocol, string) {
	if base == ProtocolChat && clientFormat != types.RelayFormatClaude &&
		requestUsesFunctionTools(request) && !requestContainsAssistantReasoning(request) {
		return ProtocolResponses, DynamicProtocolReasonChatFunctionTools
	}
	return base, ""
}

func requestPreflightConfigFingerprint(rawMapping string, config *dto.OpenCodeGoConfig) (string, error) {
	configJSON, err := common.Marshal(config)
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(requestPreflightVersion))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write([]byte(strings.TrimSpace(rawMapping)))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write(configJSON)
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func newPreflightInvariantError(cause error) error {
	return &RequestPreflightError{
		StatusCode: http.StatusInternalServerError,
		Origin:     types.ErrorOriginGatewayInvariant,
		RuleID:     PreflightEnvelopeInvariantRule,
		StageID:    PreflightRoutingStage,
		Message:    "OpenCode request preflight failed",
		cause:      cause,
	}
}

func newPreflightPlanMismatchError(cause error) error {
	return &RequestPreflightError{
		StatusCode: http.StatusInternalServerError,
		Origin:     types.ErrorOriginGatewayInvariant,
		RuleID:     PreflightPlanMismatchRule,
		StageID:    PreflightAssertionStage,
		Message:    "OpenCode request preflight assertion failed",
		cause:      cause,
	}
}
