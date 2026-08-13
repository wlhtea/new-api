package service

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
)

const (
	OpenCodeGoPublicOverloadMessage    = constant.OpenCodeGoPublicOverloadMessage
	OpenCodeGoPublicRateLimitErrorCode = constant.OpenCodeGoPublicRateLimitErrorCode
)

var errOpenCodeGoUpstreamOrigin = errors.New("opencode go upstream-origin error")

type openCodeGoUpstreamOriginError struct {
	cause              error
	upstreamStatusCode int
}

func (e *openCodeGoUpstreamOriginError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *openCodeGoUpstreamOriginError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *openCodeGoUpstreamOriginError) Is(target error) bool {
	return target == errOpenCodeGoUpstreamOrigin
}

// MarkOpenCodeGoUpstreamRelayError records that a relay error came from the
// type-62 upstream. The wrapper is not serialized and preserves Error() so
// internal logs keep the original diagnostics.
func MarkOpenCodeGoUpstreamRelayError(relayErr *types.NewAPIError) *types.NewAPIError {
	if relayErr == nil || errors.Is(relayErr, errOpenCodeGoUpstreamOrigin) {
		return relayErr
	}
	return markOpenCodeGoUpstreamRelayErrorWithStatus(relayErr, relayErr.StatusCode)
}

func markOpenCodeGoUpstreamRelayErrorWithStatus(relayErr *types.NewAPIError, upstreamStatusCode int) *types.NewAPIError {
	if relayErr == nil || errors.Is(relayErr, errOpenCodeGoUpstreamOrigin) {
		return relayErr
	}
	cause := relayErr.Err
	if cause == nil {
		cause = errors.New(relayErr.Error())
	}
	relayErr.Err = &openCodeGoUpstreamOriginError{
		cause:              cause,
		upstreamStatusCode: upstreamStatusCode,
	}
	return relayErr
}

// IsOpenCodeGoUpstreamRelayError reports whether the error was explicitly
// marked at a type-62 upstream boundary.
func IsOpenCodeGoUpstreamRelayError(err error) bool {
	return errors.Is(err, errOpenCodeGoUpstreamOrigin)
}

func openCodeGoUpstreamRelayStatusCode(err error) (int, bool) {
	var originErr *openCodeGoUpstreamOriginError
	if !errors.As(err, &originErr) || originErr == nil || originErr.upstreamStatusCode <= 0 {
		return 0, false
	}
	return originErr.upstreamStatusCode, true
}

// PublicOpenCodeGoRelayError replaces type-62 errors that name internal
// infrastructure with a provider-neutral error. It returns a fresh error so
// private relay metadata and the original unwrap chain cannot be serialized.
func PublicOpenCodeGoRelayError(channelType int, relayErr *types.NewAPIError) *types.NewAPIError {
	if relayErr == nil {
		return nil
	}
	poolExhausted := errors.Is(relayErr, ErrOpenCodeGoNoEligibleWorkspace)
	selectionStale := channelType == constant.ChannelTypeOpenCodeGo &&
		(errors.Is(relayErr, ErrOpenCodeGoIdentityProxySelectionStale) ||
			errors.Is(relayErr, ErrOpenCodeGoSelectedCredentialUnavailable))
	upstreamOrigin := channelType == constant.ChannelTypeOpenCodeGo && IsOpenCodeGoUpstreamRelayError(relayErr)
	privateDetail := channelType == constant.ChannelTypeOpenCodeGo && openCodeGoRelayErrorContainsPrivateDetail(relayErr)
	projectionCandidate := channelType == constant.ChannelTypeOpenCodeGo && openCodeGoRelayErrorProjectionCandidate(relayErr)
	if !poolExhausted && !selectionStale && !upstreamOrigin && !privateDetail && !projectionCandidate {
		return relayErr
	}
	if poolExhausted {
		return newOpenCodeGoPublicRelayError(constant.ClassifyOpenCodeGoPublicError(
			http.StatusTooManyRequests,
			OpenCodeGoPublicRateLimitErrorCode,
			OpenCodeGoPublicRateLimitErrorCode,
			relayErr.Error(),
		))
	}
	if selectionStale {
		return newOpenCodeGoPublicRelayError(constant.ClassifyOpenCodeGoPublicError(
			http.StatusServiceUnavailable,
			"",
			"",
			relayErr.Error(),
		))
	}
	projection := openCodeGoPublicErrorProjection(relayErr)
	explicitClientRequest := upstreamOrigin && openCodeGoRelayErrorIsExplicitClientRequest(relayErr)
	if explicitClientRequest {
		projection = constant.ClassifyOpenCodeGoPublicError(
			http.StatusBadRequest,
			constant.OpenCodeGoPublicInvalidRequestCode,
			constant.OpenCodeGoPublicInvalidRequestCode,
			relayErr.Error(),
		)
		projection.Message = openCodeGoPublicClientRequestMessage(relayErr)
	}
	// Explicit client request classifications remain 400 even when they were
	// reported by the upstream. Every other marked upstream error is collapsed
	// to the generic 429 without changing its raw status used by retry/health.
	if upstreamOrigin && !explicitClientRequest && projection.StatusCode != 499 {
		projection = constant.ClassifyOpenCodeGoPublicError(
			http.StatusTooManyRequests,
			OpenCodeGoPublicRateLimitErrorCode,
			OpenCodeGoPublicRateLimitErrorCode,
			relayErr.Error(),
		)
	}
	return newOpenCodeGoPublicRelayError(projection)
}

func openCodeGoRelayErrorIsExplicitClientRequest(relayErr *types.NewAPIError) bool {
	if relayErr == nil {
		return false
	}
	switch relayErr.GetErrorCode() {
	case types.ErrorCodeInvalidRequest,
		types.ErrorCodeBadRequestBody,
		types.ErrorCodeConvertRequestFailed,
		types.ErrorCodeChannelParamOverrideInvalid,
		types.ErrorCodeChannelHeaderOverrideInvalid:
		return true
	}
	errorType, errorCode := openCodeGoRelayErrorClassification(relayErr)
	return constant.IsOpenCodeGoClientRequestError(errorType, errorCode, relayErr.Error())
}

func openCodeGoPublicClientRequestMessage(relayErr *types.NewAPIError) string {
	if relayErr == nil {
		return constant.OpenCodeGoPublicInvalidRequestMessage
	}
	message := strings.TrimSpace(relayErr.ToOpenAIError().Message)
	for _, prefix := range []string{
		"Error from provider (Console Go):",
		"Error from provider (OpenCode Go):",
		"Upstream request failed:",
		"[invalid_request_error]",
		"[invalid_request]",
		"[validation_error]",
		"[bad_request]",
		"[bad_request_body]",
	} {
		message = trimOpenCodeGoPublicPrefix(message, prefix)
	}
	if message == "" || OpenCodeGoErrorHasPrivateDetail(message) {
		return constant.OpenCodeGoPublicInvalidRequestMessage
	}
	return message
}

func trimOpenCodeGoPublicPrefix(value, prefix string) string {
	value = strings.TrimSpace(value)
	if len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
		return strings.TrimSpace(value[len(prefix):])
	}
	return value
}

func openCodeGoRelayErrorClassification(relayErr *types.NewAPIError) (string, string) {
	if relayErr == nil {
		return "", ""
	}
	errorType := string(relayErr.GetErrorType())
	errorCode := string(relayErr.GetErrorCode())
	switch typed := relayErr.RelayError.(type) {
	case types.OpenAIError:
		if typed.Type != "" {
			errorType = typed.Type
		}
		if typed.Code != nil && fmt.Sprint(typed.Code) != "" {
			errorCode = fmt.Sprint(typed.Code)
		}
	case *types.OpenAIError:
		if typed != nil {
			if typed.Type != "" {
				errorType = typed.Type
			}
			if typed.Code != nil && fmt.Sprint(typed.Code) != "" {
				errorCode = fmt.Sprint(typed.Code)
			}
		}
	case types.ClaudeError:
		if typed.Type != "" {
			errorType, errorCode = typed.Type, typed.Type
		}
	case *types.ClaudeError:
		if typed != nil && typed.Type != "" {
			errorType, errorCode = typed.Type, typed.Type
		}
	}
	return errorType, errorCode
}

func newOpenCodeGoPublicRelayError(projection constant.OpenCodeGoPublicError) *types.NewAPIError {
	return types.WithOpenAIError(types.OpenAIError{
		Message: projection.Message,
		Type:    projection.Type,
		Code:    projection.Code,
	}, projection.StatusCode, types.ErrOptionWithSkipRetry())
}

func openCodeGoPublicErrorProjection(relayErr *types.NewAPIError) constant.OpenCodeGoPublicError {
	if relayErr == nil {
		return constant.ClassifyOpenCodeGoPublicError(http.StatusBadGateway, "", "", "")
	}
	errorType, errorCode := openCodeGoRelayErrorClassification(relayErr)
	statusCode := relayErr.StatusCode
	if upstreamStatusCode, ok := openCodeGoUpstreamRelayStatusCode(relayErr); ok {
		statusCode = upstreamStatusCode
	}
	return constant.ClassifyOpenCodeGoPublicError(statusCode, errorType, errorCode, relayErr.Error())
}

func openCodeGoRelayErrorProjectionCandidate(relayErr *types.NewAPIError) bool {
	if relayErr == nil {
		return false
	}
	errorType, errorCode := openCodeGoRelayErrorClassification(relayErr)
	return constant.IsOpenCodeGoPublicErrorCandidate(relayErr.StatusCode, errorType, errorCode, relayErr.Error())
}

func openCodeGoRelayErrorContainsPrivateDetail(relayErr *types.NewAPIError) bool {
	if relayErr == nil {
		return false
	}
	values := []string{
		relayErr.Error(),
		string(relayErr.GetErrorType()),
		string(relayErr.GetErrorCode()),
		string(relayErr.Metadata),
	}
	switch typed := relayErr.RelayError.(type) {
	case types.OpenAIError:
		values = append(values, typed.Message, typed.Type, typed.Param, fmt.Sprint(typed.Code), string(typed.Metadata))
	case *types.OpenAIError:
		if typed != nil {
			values = append(values, typed.Message, typed.Type, typed.Param, fmt.Sprint(typed.Code), string(typed.Metadata))
		}
	case types.ClaudeError:
		values = append(values, typed.Message, typed.Type)
	case *types.ClaudeError:
		if typed != nil {
			values = append(values, typed.Message, typed.Type)
		}
	case nil:
	default:
		values = append(values, fmt.Sprint(typed))
	}
	return OpenCodeGoErrorHasPrivateDetail(values...)
}

// OpenCodeGoErrorHasPrivateDetail reports whether an error representation
// contains provider, channel, or workspace details that must stay internal.
func OpenCodeGoErrorHasPrivateDetail(values ...string) bool {
	for _, value := range values {
		if openCodeGoStringHasPrivateErrorMarker(value) {
			return true
		}
		var decoded any
		if common.UnmarshalJsonStr(value, &decoded) == nil && openCodeGoJSONHasPrivateErrorMarker(decoded) {
			return true
		}
	}
	return false
}

func openCodeGoJSONHasPrivateErrorMarker(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if openCodeGoStringHasPrivateErrorMarker(key) || openCodeGoJSONHasPrivateErrorMarker(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if openCodeGoJSONHasPrivateErrorMarker(child) {
				return true
			}
		}
	case string:
		return openCodeGoStringHasPrivateErrorMarker(typed)
	}
	return false
}

func openCodeGoStringHasPrivateErrorMarker(value string) bool {
	return constant.OpenCodeGoStringHasPrivateErrorMarker(value)
}
