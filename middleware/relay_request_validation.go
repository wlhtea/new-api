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
	"github.com/gin-gonic/gin"
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
		service.PrepareOpenCodeAffinityIdentity(c, request)
		c.Next()
	}
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
	} else {
		message = "request body could not be read"
	}

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
