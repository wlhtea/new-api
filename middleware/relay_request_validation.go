package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/gin-gonic/gin"
)

const (
	RelayRawSensitiveScanRuleID = "request.security.raw-string"
	relayRawSensitiveScanKey    = "relay_raw_sensitive_scan_v1"
)

func PreValidateRelayRequest() gin.HandlerFunc {
	return func(c *gin.Context) {
		format, matched := preValidatedRelayFormat(c.Request.Method, c.Request.URL.Path)
		if !matched {
			c.Next()
			return
		}

		request, err := helper.GetAndValidateRequest(c, format)
		if err != nil {
			renderRelayRequestValidationError(c, format, err)
			return
		}
		if setting.ShouldCheckPromptSensitive() {
			matched, matchCount, scanErr := ScanValidatedRelayRequestSensitiveValues(c, format)
			if scanErr != nil {
				renderRelaySensitiveScanError(c, format, scanErr)
				return
			}
			c.Set(relayRawSensitiveScanKey, true)
			if matched {
				logger.LogWarn(
					c.Request.Context(),
					fmt.Sprintf(
						"relay request security rejected: rule_id=%s match_count=%d",
						RelayRawSensitiveScanRuleID,
						matchCount,
					),
				)
				renderRelayValidationError(
					c,
					format,
					http.StatusBadRequest,
					"request contains sensitive content",
					"invalid_request_error",
					types.ErrorCodeSensitiveWordsDetected,
				)
				return
			}
		}
		service.PrepareOpenCodeAffinityIdentity(c, request)
		c.Next()
	}
}

// ScanValidatedRelayRequestSensitiveValues checks every decoded JSON string
// value in the immutable strict envelope. It does not expose paths or values
// and does not materialize a disk-backed request body.
func ScanValidatedRelayRequestSensitiveValues(c *gin.Context, format types.RelayFormat) (bool, int, error) {
	if c == nil || c.Request == nil {
		return false, 0, errors.New("validated relay request context is unavailable")
	}
	envelope, found, err := helper.GetValidatedRequestEnvelope(c, format)
	if err != nil {
		return false, 0, err
	}
	if !found || envelope == nil {
		return false, 0, errors.New("validated relay request envelope is unavailable for security scan")
	}

	matched := false
	matchCount := 0
	err = envelope.VisitStringValues(c.Request.Context(), func(value string) error {
		contains, words := service.CheckSensitiveText(value)
		if contains {
			matched = true
			matchCount += len(words)
		}
		return nil
	})
	return matched, matchCount, err
}

func ValidatedRelayRequestSensitiveScanComplete(c *gin.Context) bool {
	return c != nil && c.GetBool(relayRawSensitiveScanKey)
}

func renderRelaySensitiveScanError(c *gin.Context, format types.RelayFormat, err error) {
	statusCode := http.StatusInternalServerError
	errorType := "api_error"
	message := "request security validation failed"
	if errors.Is(err, context.Canceled) {
		statusCode = 499
		errorType = "request_canceled"
		message = "request was canceled"
	} else if errors.Is(err, context.DeadlineExceeded) {
		statusCode = http.StatusGatewayTimeout
		errorType = "request_timeout"
		message = "request validation timed out"
	}
	renderRelayValidationError(c, format, statusCode, message, errorType, types.ErrorCodeBadResponse)
}

func preValidatedRelayFormat(method, path string) (types.RelayFormat, bool) {
	if method != http.MethodPost {
		return "", false
	}
	switch path {
	case "/v1/messages":
		return types.RelayFormatClaude, true
	case "/v1/chat/completions":
		return types.RelayFormatOpenAI, true
	case "/v1/responses":
		return types.RelayFormatOpenAIResponses, true
	default:
		return "", false
	}
}

func renderRelayRequestValidationError(c *gin.Context, format types.RelayFormat, err error) {
	statusCode := http.StatusBadRequest
	errorType := "invalid_request_error"
	errorCode := types.ErrorCodeInvalidRequest
	message := err.Error()

	if validationErr, ok := helper.AsClientRequestValidationError(err); ok {
		statusCode = validationErr.StatusCode
	} else if common.IsRequestBodyTooLargeError(err) || errors.Is(err, common.ErrRequestBodyTooLarge) {
		statusCode = http.StatusRequestEntityTooLarge
		message = "request body is too large"
	} else if errors.Is(err, context.Canceled) {
		statusCode = 499
		errorType = "request_canceled"
		errorCode = types.ErrorCodeBadResponse
		message = "request was canceled"
	} else if errors.Is(err, errInvalidRequestContentEncoding) {
		statusCode = http.StatusBadRequest
		message = "request body does not match its Content-Encoding"
	} else {
		message = "request body could not be read"
	}
	renderRelayValidationError(c, format, statusCode, message, errorType, errorCode)
}

func renderRelayTransportValidationError(c *gin.Context, format types.RelayFormat, statusCode int, message string) {
	renderRelayValidationError(c, format, statusCode, message, "invalid_request_error", types.ErrorCodeInvalidRequest)
}

func renderRelayValidationError(c *gin.Context, format types.RelayFormat, statusCode int, message, errorType string, errorCode types.ErrorCode) {
	message = common.MessageWithRequestId(message, c.GetString(common.RequestIdKey))
	logger.LogWarn(
		c.Request.Context(),
		fmt.Sprintf("relay request validation failed: format=%s status=%d error=%s", format, statusCode, common.LocalLogPreview(message)),
	)

	if format == types.RelayFormatClaude {
		relayErr := types.WithClaudeError(
			types.ClaudeError{Type: errorType, Message: message},
			statusCode,
			types.ErrOptionWithSkipRetry(),
			types.ErrOptionWithNoRecordErrorLog(),
		)
		c.JSON(statusCode, gin.H{
			"type":  "error",
			"error": relayErr.ToClaudeError(),
		})
		c.Abort()
		return
	}

	relayErr := types.WithOpenAIError(
		types.OpenAIError{
			Message: message,
			Type:    errorType,
			Code:    string(errorCode),
		},
		statusCode,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithNoRecordErrorLog(),
	)
	c.JSON(statusCode, gin.H{"error": relayErr.ToOpenAIError()})
	c.Abort()
}
