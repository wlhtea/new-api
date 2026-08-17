package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel/opencodego"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

func preflightOpenCodeRequest(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	if c == nil || !constant.IsOpenCodeChannelType(common.GetContextKeyInt(c, constant.ContextKeyChannelType)) {
		return nil
	}
	initializeOpenCodeCacheControlDiagnostics(c, info)
	recordOpenCodeDiagnosticCandidateFromContext(c)
	recordOpenCodeDiagnosticRouting(c, c, info)

	plan, err := opencodego.BuildRequestPreflightPlan(c, info)
	if err != nil {
		return newOpenCodeRequestPreflightAPIError(c, err)
	}
	recordOpenCodeDiagnosticPlan(c, plan)
	if err := opencodego.StoreRequestPreflightPlan(c, plan); err != nil {
		return newOpenCodeRequestPreflightAPIError(c, opencodego.NewRequestPreflightPlanStorageError(err))
	}
	return nil
}

func newOpenCodeRequestPreflightAPIError(c *gin.Context, err error) *types.NewAPIError {
	preflightErr, ok := opencodego.AsRequestPreflightError(err)
	if !ok || preflightErr == nil {
		preflightErr = &opencodego.RequestPreflightError{
			StatusCode: http.StatusInternalServerError,
			Origin:     types.ErrorOriginGatewayInvariant,
			RuleID:     opencodego.PreflightEnvelopeInvariantRule,
			StageID:    opencodego.PreflightRoutingStage,
			Message:    "OpenCode request preflight failed",
		}
		err = preflightErr
	}
	if c != nil {
		_ = opencodego.StoreRequestPreflightRejection(c, opencodego.RequestPreflightRejection{
			RuleID:  preflightErr.RuleID,
			StageID: preflightErr.StageID,
		})
	}

	statusCode := preflightErr.StatusCode
	if statusCode < 400 || statusCode > 599 {
		statusCode = http.StatusInternalServerError
	}
	logOpenCodeRequestPreflightRejection(c, preflightErr.RuleID, preflightErr.StageID, statusCode)
	errorCode := types.ErrorCodeGetChannelFailed
	if preflightErr.Origin == types.ErrorOriginLocalValidation {
		errorCode = types.ErrorCodeInvalidRequest
	}
	return types.NewOpenAIError(
		err,
		errorCode,
		statusCode,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithProvenance(types.ErrorProvenance{
			Origin:  preflightErr.Origin,
			Subtype: preflightErr.RuleID,
		}),
	)
}

func openCodeRequestPreflightRejection(c *gin.Context) (ruleID string, stageID string, found bool) {
	rejection, found := opencodego.GetRequestPreflightRejection(c)
	return rejection.RuleID, rejection.StageID, found
}
