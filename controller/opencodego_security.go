package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting"
	"github.com/gin-gonic/gin"
)

const openCodeRawSensitiveScanRuleID = middleware.RelayRawSensitiveScanRuleID

func scanOpenCodeRequestSensitiveValues(c *gin.Context, format types.RelayFormat) (bool, int, error) {
	if c == nil || !constant.IsOpenCodeChannelType(common.GetContextKeyInt(c, constant.ContextKeyChannelType)) {
		return false, 0, nil
	}
	return middleware.ScanValidatedRelayRequestSensitiveValues(c, format)
}

func preflightOpenCodeRequestSensitiveValues(c *gin.Context, format types.RelayFormat) *types.NewAPIError {
	if c == nil || !setting.ShouldCheckPromptSensitive() ||
		middleware.ValidatedRelayRequestSensitiveScanComplete(c) ||
		!constant.IsOpenCodeChannelType(common.GetContextKeyInt(c, constant.ContextKeyChannelType)) {
		return nil
	}

	matched, matchCount, err := scanOpenCodeRequestSensitiveValues(c, format)
	if err != nil {
		statusCode := http.StatusInternalServerError
		origin := types.ErrorOriginGatewayInvariant
		subtype := openCodeRawSensitiveScanRuleID + ".scan-failed"
		message := "request security validation failed"
		if errors.Is(err, context.Canceled) {
			statusCode = 499
			origin = types.ErrorOriginLocalCancel
			subtype = openCodeRawSensitiveScanRuleID + ".cancelled"
			message = "request was canceled"
		} else if errors.Is(err, context.DeadlineExceeded) {
			statusCode = http.StatusGatewayTimeout
			origin = types.ErrorOriginLocalDeadline
			subtype = openCodeRawSensitiveScanRuleID + ".deadline"
			message = "request validation timed out"
		}
		logger.LogWarn(c.Request.Context(), fmt.Sprintf(
			"relay request security scan failed: rule_id=%s status=%d",
			openCodeRawSensitiveScanRuleID,
			statusCode,
		))
		return types.NewOpenAIError(
			errors.New(message),
			types.ErrorCodeBadResponse,
			statusCode,
			types.ErrOptionWithSkipRetry(),
			types.ErrOptionWithNoRecordErrorLog(),
			types.ErrOptionWithProvenance(types.ErrorProvenance{
				Origin:  origin,
				Subtype: subtype,
			}),
		)
	}
	if !matched {
		return nil
	}

	logger.LogWarn(c.Request.Context(), fmt.Sprintf(
		"relay request security rejected: rule_id=%s match_count=%d",
		openCodeRawSensitiveScanRuleID,
		matchCount,
	))
	return types.NewOpenAIError(
		errors.New("request contains sensitive content"),
		types.ErrorCodeSensitiveWordsDetected,
		http.StatusBadRequest,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithNoRecordErrorLog(),
		types.ErrOptionWithProvenance(types.ErrorProvenance{
			Origin:  types.ErrorOriginLocalValidation,
			Subtype: openCodeRawSensitiveScanRuleID,
		}),
	)
}

func shouldRunTypedSensitiveScan(c *gin.Context) bool {
	if c == nil || middleware.ValidatedRelayRequestSensitiveScanComplete(c) {
		return false
	}
	return !constant.IsOpenCodeChannelType(common.GetContextKeyInt(c, constant.ContextKeyChannelType))
}
