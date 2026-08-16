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
	openCodeGoPublicCancelMessage      = constant.OpenCodeGoPublicRequestCanceledMessage
	openCodeGoPublicDeadlineMessage    = "请求处理超时"
	openCodeGoPublicWriterMessage      = "响应写入失败"
	openCodeGoPublicConfigMessage      = constant.OpenCodeGoPublicGatewayConfigMessage
	openCodeGoPublicCapabilityMessage  = constant.OpenCodeGoPublicCapabilityMessage
	openCodeGoPublicInvariantMessage   = "请求处理失败"
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
	if relayErr == nil {
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

// MarkOpenCodeGoUpstreamTransportError records a transport failure without
// inventing a raw HTTP status. A live downstream context is required before a
// context.Canceled transport error can enter this path.
func MarkOpenCodeGoUpstreamTransportError(relayErr *types.NewAPIError) *types.NewAPIError {
	return MarkOpenCodeGoUpstreamTransportErrorWithSubtype(relayErr, "request_transport")
}

// MarkOpenCodeGoUpstreamTransportErrorWithSubtype records a transport phase
// without inventing an HTTP status that was never received.
func MarkOpenCodeGoUpstreamTransportErrorWithSubtype(relayErr *types.NewAPIError, subtype string) *types.NewAPIError {
	if relayErr != nil {
		existing := relayErr.Provenance()
		if existing.Origin == types.ErrorOriginUpstreamTransport && strings.TrimSpace(existing.Subtype) != "" {
			subtype = existing.Subtype
		}
	}
	subtype = strings.TrimSpace(subtype)
	if subtype == "" {
		subtype = "request_transport"
	}
	return markOpenCodeGoUpstreamRelayErrorWithProvenance(relayErr, types.ErrorProvenance{
		Origin:  types.ErrorOriginUpstreamTransport,
		Subtype: subtype,
	})
}

// MarkOpenCodeGoUpstreamHTTPErrorWithSubtype retains a status observed at the
// upstream boundary while recording which bounded response phase failed.
func MarkOpenCodeGoUpstreamHTTPErrorWithSubtype(relayErr *types.NewAPIError, rawStatusCode int, subtype string) *types.NewAPIError {
	subtype = strings.TrimSpace(subtype)
	if subtype == "" {
		subtype = "non_2xx"
	}
	return markOpenCodeGoUpstreamRelayErrorWithProvenance(relayErr, types.ErrorProvenance{
		Origin:        types.ErrorOriginUpstreamHTTP,
		Subtype:       subtype,
		RawStatusCode: rawStatusCode,
	})
}

// MarkOpenCodeGoUpstreamMalformedError records a response that crossed the
// upstream boundary but violated a local structural or resource contract. It
// intentionally carries no raw HTTP status: a relay-side response limit is not
// evidence that the upstream returned HTTP 502 (or any other failure status).
func MarkOpenCodeGoUpstreamMalformedError(relayErr *types.NewAPIError, subtype string) *types.NewAPIError {
	subtype = strings.TrimSpace(subtype)
	if subtype == "" {
		subtype = "malformed_response"
	}
	return markOpenCodeGoUpstreamRelayErrorWithProvenance(relayErr, types.ErrorProvenance{
		Origin:  types.ErrorOriginUpstreamMalformed,
		Subtype: subtype,
	})
}

func markOpenCodeGoUpstreamRelayErrorWithStatus(relayErr *types.NewAPIError, upstreamStatusCode int) *types.NewAPIError {
	return markOpenCodeGoUpstreamRelayErrorWithProvenance(relayErr, openCodeGoUpstreamProvenance(upstreamStatusCode))
}

func markOpenCodeGoUpstreamRelayErrorWithProvenance(relayErr *types.NewAPIError, provenance types.ErrorProvenance) *types.NewAPIError {
	if relayErr == nil {
		return relayErr
	}
	existing := relayErr.Provenance()
	if !existing.IsZero() && existing != provenance {
		// Provenance is first-write immutable. A later boundary must not turn a
		// proven local/gateway error into upstream evidence, or replace one
		// upstream phase with another.
		return relayErr
	}
	if !relayErr.SetProvenance(provenance) {
		return relayErr
	}
	if errors.Is(relayErr, errOpenCodeGoUpstreamOrigin) {
		return relayErr
	}
	cause := relayErr.Err
	if cause == nil {
		cause = errors.New(relayErr.Error())
	}
	relayErr.Err = &openCodeGoUpstreamOriginError{
		cause:              cause,
		upstreamStatusCode: provenance.RawStatusCode,
	}
	return relayErr
}

func openCodeGoUpstreamProvenance(upstreamStatusCode int) types.ErrorProvenance {
	if upstreamStatusCode == http.StatusOK {
		return types.ErrorProvenance{
			Origin:        types.ErrorOriginUpstreamEnvelope,
			Subtype:       "structured_error",
			RawStatusCode: upstreamStatusCode,
		}
	}
	return types.ErrorProvenance{
		Origin:        types.ErrorOriginUpstreamHTTP,
		Subtype:       "non_2xx",
		RawStatusCode: upstreamStatusCode,
	}
}

// IsOpenCodeGoUpstreamRelayError reports whether the error was explicitly
// marked at an OpenCode upstream boundary.
func IsOpenCodeGoUpstreamRelayError(err error) bool {
	var relayErr *types.NewAPIError
	if errors.As(err, &relayErr) && relayErr != nil && relayErr.Provenance().IsUpstream() {
		return true
	}
	return errors.Is(err, errOpenCodeGoUpstreamOrigin)
}

func openCodeGoUpstreamRelayStatusCode(err error) (int, bool) {
	var relayErr *types.NewAPIError
	if errors.As(err, &relayErr) && relayErr != nil {
		provenance := relayErr.Provenance()
		if provenance.IsUpstream() && provenance.RawStatusCode > 0 {
			return provenance.RawStatusCode, true
		}
	}
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
	provenance := relayErr.Provenance()
	upstreamOrigin := isOpenCodeChannel && (provenance.IsUpstream() ||
		(provenance.IsZero() && errors.Is(relayErr, errOpenCodeGoUpstreamOrigin)))
	if isOpenCodeChannel && relayErr.Provenance().IsLocal() {
		return newOpenCodeGoLocalPublicError(relayErr.Provenance())
	}
	if isOpenCodeChannel && relayErr.Provenance().IsGateway() {
		return newOpenCodeGoGatewayPublicError(relayErr.Provenance())
	}
	if upstreamOrigin && IsOpenCodeGoRawInvalidRequestError(relayErr) {
		rawStatus, _ := OpenCodeGoUpstreamRelayStatusCode(relayErr)
		return newOpenCodeGoFixedInvalidRequestError(rawStatus)
	}
	if poolExhausted {
		return newOpenCodeGoOverloadError()
	}
	if selectionStale {
		return newOpenCodeGoOverloadError()
	}
	if upstreamOrigin {
		// Raw 400/422 was handled above. Every other upstream origin is rebuilt
		// from fixed provider-neutral content without carrying raw policy evidence.
		return newOpenCodeGoOverloadError()
	}
	if isOpenCodeChannel {
		// Missing or conflicting provenance cannot establish that free text is a
		// controlled gateway message. Fail closed rather than inspect its content.
		return newOpenCodeGoOverloadError()
	}
	return relayErr
}

func newOpenCodeGoOverloadError() *types.NewAPIError {
	return newOpenCodeGoPublicRelayError(constant.OpenCodeGoPublicError{
		StatusCode: http.StatusTooManyRequests,
		Message:    OpenCodeGoPublicOverloadMessage,
		Type:       OpenCodeGoPublicRateLimitErrorCode,
		Code:       OpenCodeGoPublicRateLimitErrorCode,
	})
}

func newOpenCodeGoLocalPublicError(provenance types.ErrorProvenance) *types.NewAPIError {
	statusCode := http.StatusInternalServerError
	message := openCodeGoPublicWriterMessage
	errorType := "internal_server_error"
	switch provenance.Origin {
	case types.ErrorOriginLocalValidation:
		return newOpenCodeGoFixedInvalidRequestErrorWithProvenance(provenance)
	case types.ErrorOriginLocalCancel:
		statusCode = 499
		message = openCodeGoPublicCancelMessage
		errorType = constant.OpenCodeGoPublicRequestCanceledCode
	case types.ErrorOriginLocalDeadline:
		statusCode = http.StatusGatewayTimeout
		message = openCodeGoPublicDeadlineMessage
		errorType = "gateway_timeout"
	}
	publicErr := types.WithOpenAIError(types.OpenAIError{
		Message: message,
		Type:    errorType,
		Code:    errorType,
	}, statusCode, types.ErrOptionWithSkipRetry())
	publicErr.SetProvenance(provenance)
	return publicErr
}

func newOpenCodeGoGatewayPublicError(provenance types.ErrorProvenance) *types.NewAPIError {
	statusCode := http.StatusInternalServerError
	message := openCodeGoPublicInvariantMessage
	errorType := "internal_server_error"
	if provenance.Origin == types.ErrorOriginGatewayConfig {
		statusCode = http.StatusServiceUnavailable
		message = openCodeGoPublicConfigMessage
		errorType = constant.OpenCodeGoPublicServiceUnavailableCode
	} else if provenance.Origin == types.ErrorOriginGatewayDependency {
		statusCode = http.StatusServiceUnavailable
		message = openCodeGoPublicCapabilityMessage
		errorType = constant.OpenCodeGoPublicServiceUnavailableCode
	}
	publicErr := types.WithOpenAIError(types.OpenAIError{
		Message: message,
		Type:    errorType,
		Code:    errorType,
	}, statusCode, types.ErrOptionWithSkipRetry())
	publicErr.SetProvenance(provenance)
	return publicErr
}

// IsOpenCodeGoRawInvalidRequestError is true only for a marked raw upstream
// HTTP 400/422. Status mapping and free-form type/message text cannot create
// this classification.
func IsOpenCodeGoRawInvalidRequestError(relayErr *types.NewAPIError) bool {
	if relayErr == nil || !IsOpenCodeGoUpstreamRelayError(relayErr) {
		return false
	}
	provenance := relayErr.Provenance()
	if !provenance.IsZero() && provenance.Origin != types.ErrorOriginUpstreamHTTP {
		return false
	}
	statusCode, ok := OpenCodeGoUpstreamRelayStatusCode(relayErr)
	return ok && (statusCode == http.StatusBadRequest || statusCode == http.StatusUnprocessableEntity)
}

// IsOpenCodeGoFixedInvalidRequestProjection identifies the fresh public value
// produced for a raw upstream 400/422. It does not inspect error text.
func IsOpenCodeGoFixedInvalidRequestProjection(relayErr *types.NewAPIError) bool {
	if relayErr == nil || relayErr.StatusCode != http.StatusBadRequest {
		return false
	}
	provenance := relayErr.Provenance()
	return provenance.Origin == types.ErrorOriginUpstreamHTTP &&
		provenance.Subtype == "fixed_invalid_request" &&
		(provenance.RawStatusCode == http.StatusBadRequest || provenance.RawStatusCode == http.StatusUnprocessableEntity)
}

func newOpenCodeGoFixedInvalidRequestError(rawStatus int) *types.NewAPIError {
	return newOpenCodeGoFixedInvalidRequestErrorWithProvenance(types.ErrorProvenance{
		Origin:        types.ErrorOriginUpstreamHTTP,
		Subtype:       "fixed_invalid_request",
		RawStatusCode: rawStatus,
	})
}

func newOpenCodeGoFixedInvalidRequestErrorWithProvenance(provenance types.ErrorProvenance) *types.NewAPIError {
	publicErr := types.WithOpenAIError(types.OpenAIError{
		Message: constant.OpenCodeGoPublicInvalidRequestMessage,
		Type:    constant.OpenCodeGoPublicInvalidRequestCode,
		Code:    constant.OpenCodeGoPublicInvalidRequestCode,
	}, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	publicErr.SetProvenance(provenance)
	return publicErr
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

// OpenCodeGoErrorHasPrivateDetail reports whether an error representation
// contains provider, channel, or workspace details that must stay internal.
func OpenCodeGoErrorHasPrivateDetail(values ...string) bool {
	return common.OpenCodeGoErrorHasPrivateDetail(values...)
}
