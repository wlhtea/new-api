package relay

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

const openCodeOutboundPlanCaptureContextKey = "opencode_outbound_plan_capture_v1"

const openCodeBoundOutboundPlanContextKey = "opencode_bound_outbound_plan_v1"

const openCodeOutboundPlanRequiredContextKey = "opencode_outbound_plan_required_v1"

var errOpenCodeOutboundPlanCaptured = errors.New("OpenCode outbound candidate plan captured")

type openCodeOutboundPlanCapture struct {
	body     []byte
	captured bool
}

type openCodeBoundOutboundPlan struct {
	selectionGroup string
	channelID      int
	body           []byte
	verified       bool
}

func finalizeConvertedRequest(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	adaptor channel.Adaptor,
	convertedRequest any,
	marshalErrorCode types.ErrorCode,
) ([]byte, *types.NewAPIError) {
	if finalizer, ok := adaptor.(channel.OutboundRequestFinalizer); ok {
		jsonData, err := finalizer.FinalizeOutboundRequest(c, info, convertedRequest)
		if err != nil {
			if overrideErr, isOverride := channel.AsOutboundParamOverrideError(err); isOverride {
				return nil, newAPIErrorFromParamOverride(overrideErr)
			}
			return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}
		if err := verifyBoundOpenCodeOutboundPlan(c, info, jsonData); err != nil {
			return nil, types.NewError(
				err,
				types.ErrorCodeConvertRequestFailed,
				types.ErrOptionWithSkipRetry(),
				types.ErrOptionWithProvenance(types.ErrorProvenance{
					Origin:  types.ErrorOriginGatewayInvariant,
					Subtype: "request.finalized-body-mismatch",
				}),
			)
		}
		return captureOpenCodeOutboundPlan(c, info, jsonData, marshalErrorCode)
	}

	jsonData, err := common.Marshal(convertedRequest)
	if err != nil {
		return nil, types.NewError(err, marshalErrorCode, types.ErrOptionWithSkipRetry())
	}

	jsonData, err = relaycommon.RemoveDisabledFields(
		jsonData,
		info.ChannelOtherSettings,
		info.ChannelSetting.PassThroughBodyEnabled,
	)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}

	if len(info.ParamOverride) > 0 {
		jsonData, err = relaycommon.ApplyParamOverrideWithRelayInfo(jsonData, info)
		if err != nil {
			return nil, newAPIErrorFromParamOverride(err)
		}
	}
	return captureOpenCodeOutboundPlan(c, info, jsonData, marshalErrorCode)
}

// BindOpenCodeOutboundPlan binds the immutable pre-billing candidate body to
// the selected physical attempt. The actual handler still performs conversion
// to initialize protocol-specific adaptor state, then must reproduce this body
// exactly before any upstream I/O.
func BindOpenCodeOutboundPlan(c *gin.Context, info *relaycommon.RelayInfo, body []byte) error {
	if c == nil || info == nil || len(body) == 0 ||
		!constant.IsOpenCodeChannelType(common.GetContextKeyInt(c, constant.ContextKeyChannelType)) {
		return errors.New("OpenCode outbound candidate binding is invalid")
	}
	selectionGroup := relaycommon.ResolveSelectionGroup(c, info)
	channelID := common.GetContextKeyInt(c, constant.ContextKeyChannelId)
	if selectionGroup == "" || channelID <= 0 {
		return errors.New("OpenCode outbound candidate selection is invalid")
	}
	c.Set(openCodeBoundOutboundPlanContextKey, &openCodeBoundOutboundPlan{
		selectionGroup: selectionGroup,
		channelID:      channelID,
		body:           append([]byte(nil), body...),
	})
	c.Set(openCodeOutboundPlanRequiredContextKey, true)
	return nil
}

// RequireOpenCodeOutboundPlanBinding marks the controller path after all
// candidates have been planned. Direct handler/adaptor unit seams remain
// usable, while a production attempt can no longer proceed without a bind.
func RequireOpenCodeOutboundPlanBinding(c *gin.Context) error {
	if c == nil {
		return errors.New("OpenCode outbound candidate context is nil")
	}
	c.Set(openCodeOutboundPlanRequiredContextKey, true)
	return nil
}

// PrepareOpenCodeGoOutboundPlanReplay rearms the existing Type 62 binding for
// its one same-workspace physical retry. It deliberately keeps the original
// bound body and requires the retry to reproduce it through the normal
// finalizer before any second upstream call.
func PrepareOpenCodeGoOutboundPlanReplay(c *gin.Context, info *relaycommon.RelayInfo) error {
	if c == nil || info == nil {
		return errors.New("OpenCode Go outbound replay context is invalid")
	}
	value, found := c.Get(openCodeBoundOutboundPlanContextKey)
	if !found {
		// Direct handler/controller unit seams may run without candidate planning.
		// Production marks the binding required before any physical attempt.
		required, _ := c.Get(openCodeOutboundPlanRequiredContextKey)
		if required != true {
			return nil
		}
		return errors.New("OpenCode Go outbound replay has no bound candidate")
	}
	if common.GetContextKeyInt(c, constant.ContextKeyChannelType) != constant.ChannelTypeOpenCodeGo ||
		info.GetChannelType() != constant.ChannelTypeOpenCodeGo {
		return errors.New("OpenCode Go outbound replay channel type changed")
	}
	bound, ok := value.(*openCodeBoundOutboundPlan)
	if !ok || bound == nil || !bound.verified || len(bound.body) == 0 {
		return errors.New("OpenCode Go outbound replay binding state is invalid")
	}
	if bound.selectionGroup != relaycommon.ResolveSelectionGroup(c, info) ||
		bound.channelID != common.GetContextKeyInt(c, constant.ContextKeyChannelId) ||
		bound.channelID != info.ChannelId {
		return errors.New("OpenCode Go outbound replay selection changed")
	}
	bound.verified = false
	return nil
}

func verifyBoundOpenCodeOutboundPlan(c *gin.Context, info *relaycommon.RelayInfo, body []byte) error {
	if c == nil || info == nil || !constant.IsOpenCodeChannelType(info.GetChannelType()) {
		return nil
	}
	value, found := c.Get(openCodeBoundOutboundPlanContextKey)
	if !found {
		// Candidate planning intentionally has no binding; it stops at the
		// capture hook below. A physical relay always binds in controller.Relay.
		if _, planning := c.Get(openCodeOutboundPlanCaptureContextKey); planning {
			return nil
		}
		required, _ := c.Get(openCodeOutboundPlanRequiredContextKey)
		if required != true {
			return nil
		}
		return errors.New("OpenCode physical attempt has no bound outbound candidate")
	}
	bound, ok := value.(*openCodeBoundOutboundPlan)
	if !ok || bound == nil || bound.verified || len(bound.body) == 0 {
		return errors.New("OpenCode outbound candidate binding state is invalid")
	}
	if bound.selectionGroup != relaycommon.ResolveSelectionGroup(c, info) ||
		bound.channelID != common.GetContextKeyInt(c, constant.ContextKeyChannelId) ||
		bound.channelID != info.ChannelId {
		return errors.New("OpenCode outbound candidate selection changed before materialization")
	}
	if !bytes.Equal(bound.body, body) {
		return errors.New("OpenCode finalized body differs from the pre-billing candidate")
	}
	bound.verified = true
	return nil
}

func captureOpenCodeOutboundPlan(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	jsonData []byte,
	errorCode types.ErrorCode,
) ([]byte, *types.NewAPIError) {
	if c == nil || info == nil || !constant.IsOpenCodeChannelType(info.GetChannelType()) {
		return jsonData, nil
	}
	value, found := c.Get(openCodeOutboundPlanCaptureContextKey)
	if !found {
		return jsonData, nil
	}
	capture, ok := value.(*openCodeOutboundPlanCapture)
	if !ok || capture == nil || capture.captured {
		return nil, types.NewError(
			fmt.Errorf("OpenCode outbound candidate plan capture state is invalid"),
			errorCode,
			types.ErrOptionWithSkipRetry(),
		)
	}
	capture.body = append([]byte(nil), jsonData...)
	capture.captured = true
	return nil, types.NewError(errOpenCodeOutboundPlanCaptured, errorCode, types.ErrOptionWithSkipRetry())
}

// PlanOpenCodeOutboundRequest runs the normal handler conversion and finalizer
// but stops immediately after the final JSON fence. It never reaches adaptor
// network setup, workspace acquisition, or response handling.
func PlanOpenCodeOutboundRequest(
	c *gin.Context,
	info *relaycommon.RelayInfo,
) ([]byte, *types.NewAPIError) {
	if c == nil || info == nil || !constant.IsOpenCodeChannelType(common.GetContextKeyInt(c, constant.ContextKeyChannelType)) {
		return nil, types.NewError(
			fmt.Errorf("OpenCode outbound candidate planning input is invalid"),
			types.ErrorCodeConvertRequestFailed,
			types.ErrOptionWithSkipRetry(),
		)
	}
	capture := &openCodeOutboundPlanCapture{}
	c.Set(openCodeOutboundPlanCaptureContextKey, capture)

	plannedInfo := *info
	plannedInfo.UsingGroup = relaycommon.ResolveSelectionGroup(c, &plannedInfo)
	var relayErr *types.NewAPIError
	switch plannedInfo.RelayFormat {
	case types.RelayFormatClaude:
		relayErr = ClaudeHelper(c, &plannedInfo)
	case types.RelayFormatOpenAIResponses:
		relayErr = ResponsesHelper(c, &plannedInfo)
	case types.RelayFormatOpenAI:
		relayErr = TextHelper(c, &plannedInfo)
	default:
		return nil, types.NewError(
			fmt.Errorf("unsupported OpenCode outbound planning format %q", plannedInfo.RelayFormat),
			types.ErrorCodeConvertRequestFailed,
			types.ErrOptionWithSkipRetry(),
		)
	}

	if !capture.captured {
		if relayErr != nil {
			return nil, relayErr
		}
		return nil, types.NewError(
			fmt.Errorf("OpenCode outbound candidate planning completed without a final body"),
			types.ErrorCodeConvertRequestFailed,
			types.ErrOptionWithSkipRetry(),
		)
	}
	if relayErr == nil || !errors.Is(relayErr.Err, errOpenCodeOutboundPlanCaptured) {
		return nil, types.NewError(
			fmt.Errorf("OpenCode outbound candidate planning did not stop at the finalizer"),
			types.ErrorCodeConvertRequestFailed,
			types.ErrOptionWithSkipRetry(),
		)
	}
	return append([]byte(nil), capture.body...), nil
}
