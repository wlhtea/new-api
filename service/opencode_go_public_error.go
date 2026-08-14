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

// MarkOpenCodeGoUpstreamRelayError records that a relay error came from an
// OpenCode upstream boundary. The wrapper is not serialized and preserves
// Error() so internal logs keep the original diagnostics.
func MarkOpenCodeGoUpstreamRelayError(relayErr *types.NewAPIError) *types.NewAPIError {
	if relayErr == nil || errors.Is(relayErr, errOpenCodeGoUpstreamOrigin) {
		return relayErr
	}
	return markOpenCodeGoUpstreamRelayErrorWithStatus(relayErr, relayErr.StatusCode)
}

// MarkOpenCodeGoUpstreamRelayErrorWithStatus records the HTTP status observed
// at the upstream boundary. This is needed for HTTP-200 error envelopes whose
// protocol handler may otherwise synthesize a different relay status.
func MarkOpenCodeGoUpstreamRelayErrorWithStatus(relayErr *types.NewAPIError, upstreamStatusCode int) *types.NewAPIError {
	return markOpenCodeGoUpstreamRelayErrorWithStatus(relayErr, upstreamStatusCode)
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
// marked at an OpenCode upstream boundary.
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

// OpenCodeGoUpstreamRelayStatusCode returns the status observed at the
// upstream boundary before channel status mapping or public projection.
func OpenCodeGoUpstreamRelayStatusCode(relayErr *types.NewAPIError) (int, bool) {
	return openCodeGoUpstreamRelayStatusCode(relayErr)
}

// OpenCodeGoRelayPolicyStatusCode returns the status used for internal retry
// and channel-disable decisions. A real non-200 upstream status wins over
// channel status mapping. HTTP-200 error envelopes instead use their trusted
// structured type/code so they cannot be mistaken for successful responses.
// The relay error itself is intentionally left unchanged for admin diagnostics
// and the later public projection.
func OpenCodeGoRelayPolicyStatusCode(relayErr *types.NewAPIError) int {
	if relayErr == nil {
		return 0
	}
	upstreamStatusCode, marked := openCodeGoUpstreamRelayStatusCode(relayErr)
	if !marked {
		return relayErr.StatusCode
	}
	if upstreamStatusCode != http.StatusOK {
		return upstreamStatusCode
	}

	errorType, errorCode := openCodeGoRelayErrorClassification(relayErr)
	classification := strings.ToLower(strings.Join([]string{errorType, errorCode}, " "))
	switch {
	case openCodeGoPolicyClassificationContains(classification,
		"authentication_error", "authenticationerror", "auth_error", "autherror",
		"invalid_api_key", "invalid api key", "unauthorized", "invalid_token"):
		return http.StatusUnauthorized
	case constant.IsOpenCodeGoClientRequestError(errorType, errorCode, ""):
		return http.StatusBadRequest
	case openCodeGoPolicyClassificationContains(classification,
		"rate_limit", "rate-limit", "ratelimit", "too_many_requests",
		"overload", "usage_limit", "usagelimit", "quota", "credits"):
		return http.StatusTooManyRequests
	case openCodeGoPolicyClassificationContains(classification,
		"server_error", "servererror", "internal_error", "internalerror",
		"service_unavailable", "serviceunavailable", "api_error", "apierror",
		"gateway_error", "gatewayerror", "timeout"):
		return http.StatusInternalServerError
	default:
		// A recognized error envelope is never a successful response. Unknown
		// upstream classifications fail closed as a retryable bad gateway.
		return http.StatusBadGateway
	}
}

// SanitizeOpenCodeGoAdminError keeps useful upstream diagnostics while
// removing credentials, internal addresses, and raw account/session values.
func SanitizeOpenCodeGoAdminError(err error) string {
	return sanitizeOpenCodeGoError(err)
}

// OpenCodeGoAdminErrorWithStatusCode keeps useful upstream diagnostics for
// administrators while removing credentials and raw account/session values.
func OpenCodeGoAdminErrorWithStatusCode(relayErr *types.NewAPIError) string {
	if relayErr == nil {
		return ""
	}
	message := SanitizeOpenCodeGoAdminError(relayErr)
	if relayErr.StatusCode == 0 {
		return message
	}
	if message == "" {
		return fmt.Sprintf("status_code=%d", relayErr.StatusCode)
	}
	return fmt.Sprintf("status_code=%d, %s", relayErr.StatusCode, message)
}

func openCodeGoPolicyClassificationContains(classification string, markers ...string) bool {
	for _, marker := range markers {
		if strings.Contains(classification, marker) {
			return true
		}
	}
	return false
}

// PublicOpenCodeGoRelayError replaces OpenCode-channel errors that name
// internal infrastructure with a provider-neutral error. It returns a fresh
// error so private relay metadata and the original unwrap chain cannot be
// serialized. Type-62 workspace sentinels remain scoped to the account pool.
func PublicOpenCodeGoRelayError(channelType int, relayErr *types.NewAPIError) *types.NewAPIError {
	if relayErr == nil {
		return nil
	}
	isOpenCodeChannel := constant.IsOpenCodeChannelType(channelType)
	// Pool exhaustion can be raised before relay metadata has recorded the
	// selected channel type. Preserve the existing unknown-context projection,
	// but never apply this Type-62 sentinel rule to the API-key channel.
	isOpenCodeGoPool := channelType == constant.ChannelTypeUnknown ||
		constant.IsOpenCodeGoPoolChannelType(channelType)
	poolExhausted := isOpenCodeGoPool && errors.Is(relayErr, ErrOpenCodeGoNoEligibleWorkspace)
	selectionStale := isOpenCodeGoPool &&
		(errors.Is(relayErr, ErrOpenCodeGoIdentityProxySelectionStale) ||
			errors.Is(relayErr, ErrOpenCodeGoSelectedCredentialUnavailable))
	upstreamOrigin := isOpenCodeChannel && IsOpenCodeGoUpstreamRelayError(relayErr)
	privateDetail := isOpenCodeChannel && openCodeGoRelayErrorContainsPrivateDetail(relayErr)
	projectionCandidate := isOpenCodeChannel && openCodeGoRelayErrorProjectionCandidate(relayErr)
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
	safeUpstreamClientRequest := upstreamOrigin && openCodeGoRelayErrorIsSafeUpstreamClientRequest(relayErr)
	if safeUpstreamClientRequest {
		projection = constant.ClassifyOpenCodeGoPublicError(
			http.StatusBadRequest,
			constant.OpenCodeGoPublicInvalidRequestCode,
			constant.OpenCodeGoPublicInvalidRequestCode,
			relayErr.Error(),
		)
		projection.Message = openCodeGoPublicClientRequestMessage(relayErr)
	}
	// A marked upstream error may keep 400 only when its unmodified upstream
	// status was 400/422 and its classification passed the non-operator client
	// allowlist. HTTP-200 error envelopes and all other upstream statuses fail
	// closed to 429 without changing the raw status used by retry/health.
	// Cancellation is a 499 only when it originated from the downstream
	// request. An upstream 499 or HTTP-200 error envelope that happens to say
	// "context canceled" is still an upstream failure and must not bypass the
	// provider-neutral overload projection.
	if upstreamOrigin && !safeUpstreamClientRequest {
		projection = constant.OpenCodeGoPublicError{
			StatusCode: http.StatusTooManyRequests,
			Message:    OpenCodeGoPublicOverloadMessage,
			Type:       OpenCodeGoPublicRateLimitErrorCode,
			Code:       OpenCodeGoPublicRateLimitErrorCode,
		}
	}
	return newOpenCodeGoPublicRelayError(projection)
}

func openCodeGoRelayErrorIsSafeUpstreamClientRequest(relayErr *types.NewAPIError) bool {
	if relayErr == nil {
		return false
	}
	upstreamStatusCode, marked := openCodeGoUpstreamRelayStatusCode(relayErr)
	if !marked {
		return false
	}
	errorType, errorCode := openCodeGoRelayErrorClassification(relayErr)
	return constant.IsOpenCodeGoSafeUpstreamClientRequestError(
		upstreamStatusCode,
		errorType,
		errorCode,
		relayErr.Error(),
	)
}

func openCodeGoPublicClientRequestMessage(relayErr *types.NewAPIError) string {
	if relayErr == nil {
		return constant.OpenCodeGoPublicInvalidRequestMessage
	}
	message := ""
	switch typed := relayErr.RelayError.(type) {
	case types.OpenAIError:
		message = typed.Message
	case *types.OpenAIError:
		if typed != nil {
			message = typed.Message
		}
	case types.ClaudeError:
		message = typed.Message
	case *types.ClaudeError:
		if typed != nil {
			message = typed.Message
		}
	default:
		message = relayErr.ToOpenAIError().Message
	}
	return common.OpenCodeGoPublicClientRequestMessage(message)
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
	return common.OpenCodeGoErrorHasPrivateDetail(values...)
}
