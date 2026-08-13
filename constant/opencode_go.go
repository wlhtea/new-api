package constant

import (
	"net/http"
	"strings"
)

const (
	OpenCodeGoPublicOverloadMessage        = "当前分组上游负载已饱和，请稍后再试"
	OpenCodeGoPublicRateLimitErrorCode     = "rate_limit_error"
	OpenCodeGoPublicInvalidRequestMessage  = "请求体或参数无效，请检查请求内容"
	OpenCodeGoPublicInvalidRequestCode     = "invalid_request_error"
	OpenCodeGoPublicRequestCanceledMessage = "请求已取消"
	OpenCodeGoPublicRequestCanceledCode    = "request_canceled"

	OpenCodeGoAffinitySourceToken                 = "token"
	OpenCodeGoAffinitySourceClaudeCodeSession     = "claude-code-session"
	OpenCodeGoAffinitySourceClaudeMetadataSession = "claude-metadata-session"
	OpenCodeGoAffinitySourceOpenCodeSession       = "opencode-session"
	OpenCodeGoAffinitySourcePromptCacheKey        = "prompt_cache_key"
	OpenCodeGoAffinitySourceNone                  = "none"
)

type OpenCodeGoPublicError struct {
	StatusCode int
	Message    string
	Type       string
	Code       string
}

var openCodeGoClientErrorMarkers = []string{
	"invalid_request",
	"bad_request",
	"bad_request_body",
	"validation_error",
	"invalid_prompt",
	"input_too_long",
	"context_length",
	"request_too_large",
	"unsupported_parameter",
	"unknown_parameter",
	"convert_request_failed",
	"channel:param_override_invalid",
	"channel:header_override_invalid",
}

var openCodeGoRateLimitMarkers = []string{
	"rate_limit",
	"ratelimit",
	"too_many_requests",
	"usage_limit",
	"usagelimit",
	"quota_exhausted",
	"creditserror",
	"monthlylimiterror",
	"userlimiterror",
	"blackusagelimiterror",
	"violation_fee.",
}

var openCodeGoResponseErrorMarkers = []string{
	"bad_response",
	"bad_response_body",
	"empty_response",
	"read_response_body_failed",
	"unexpected_eof",
}

var openCodeGoPoolExhaustionMarkers = []string{
	"no eligible workspace",
	"pool exhausted",
	"pool is saturated",
}

var openCodeGoSelectionStaleMarkers = []string{
	"selection_stale",
	"selected opencode go workspace is no longer available",
	"selected workspace is no longer available",
	"workspace credential is unavailable",
	"workspace credential unavailable",
	"selected credential is unavailable",
	"selected credential unavailable",
	"workspace_unavailable",
	"workspace unavailable",
}

var openCodeGoInternalFailureMarkers = []string{
	"protocol is not configured",
	"relay info is missing",
	"request adaptor is invalid",
	"channel http settings are invalid",
	"channel identity proxy settings are invalid",
	"upstream error: do request failed",
}

// ClassifyOpenCodeGoPublicError produces a provider-neutral public projection.
// Client request failures are 400, cancellation is 499, and every recognized
// OpenCode Go capacity/upstream failure is exposed as the same generic 429.
func ClassifyOpenCodeGoPublicError(statusCode int, errorType, errorCode, message string) OpenCodeGoPublicError {
	typeCode := strings.ToLower(strings.Join([]string{errorType, errorCode}, " "))
	message = strings.ToLower(message)
	if statusCode == 499 || strings.Contains(message, "context canceled") || strings.Contains(message, "context cancelled") {
		return OpenCodeGoPublicError{
			StatusCode: 499,
			Message:    OpenCodeGoPublicRequestCanceledMessage,
			Type:       OpenCodeGoPublicRequestCanceledCode,
			Code:       OpenCodeGoPublicRequestCanceledCode,
		}
	}
	if openCodeGoErrorMessageContains(message, openCodeGoInternalFailureMarkers) {
		return openCodeGoPublicOverload()
	}
	if openCodeGoErrorClassificationContains(typeCode, openCodeGoClientErrorMarkers) {
		return openCodeGoPublicInvalidRequest()
	}
	if statusCode == http.StatusBadRequest || statusCode == http.StatusRequestEntityTooLarge ||
		statusCode == http.StatusUnsupportedMediaType || statusCode == http.StatusUnprocessableEntity {
		return openCodeGoPublicInvalidRequest()
	}
	if statusCode == http.StatusTooManyRequests ||
		openCodeGoErrorClassificationContains(typeCode, openCodeGoRateLimitMarkers) ||
		openCodeGoErrorClassificationContains(typeCode, openCodeGoResponseErrorMarkers) ||
		openCodeGoErrorMessageContains(message, openCodeGoPoolExhaustionMarkers) ||
		openCodeGoErrorMessageContains(message, openCodeGoSelectionStaleMarkers) ||
		strings.Contains(message, "unexpected end of json") ||
		strings.Contains(message, "unexpected eof") {
		return openCodeGoPublicOverload()
	}
	// Callers invoke this classifier only after establishing that a type-62
	// error must cross the private-to-public boundary. Fail closed so an
	// unknown upstream payload never exposes provider details.
	return openCodeGoPublicOverload()
}

func openCodeGoPublicInvalidRequest() OpenCodeGoPublicError {
	return OpenCodeGoPublicError{
		StatusCode: http.StatusBadRequest,
		Message:    OpenCodeGoPublicInvalidRequestMessage,
		Type:       OpenCodeGoPublicInvalidRequestCode,
		Code:       OpenCodeGoPublicInvalidRequestCode,
	}
}

func openCodeGoPublicOverload() OpenCodeGoPublicError {
	return OpenCodeGoPublicError{
		StatusCode: http.StatusTooManyRequests,
		Message:    OpenCodeGoPublicOverloadMessage,
		Type:       OpenCodeGoPublicRateLimitErrorCode,
		Code:       OpenCodeGoPublicRateLimitErrorCode,
	}
}

// IsOpenCodeGoPublicErrorCandidate reports whether an error carries a known
// OpenCode Go semantic classification that should be projected at the public
// boundary. Generic provider errors are intentionally excluded so their
// original status and payload can continue through unchanged.
func IsOpenCodeGoPublicErrorCandidate(statusCode int, errorType, errorCode, message string) bool {
	typeCode := strings.ToLower(strings.Join([]string{errorType, errorCode}, " "))
	message = strings.ToLower(message)
	return statusCode == http.StatusBadRequest ||
		statusCode == http.StatusRequestEntityTooLarge ||
		statusCode == http.StatusUnsupportedMediaType ||
		statusCode == http.StatusUnprocessableEntity ||
		statusCode == http.StatusTooManyRequests ||
		statusCode == 499 ||
		openCodeGoErrorClassificationContains(typeCode, openCodeGoClientErrorMarkers) ||
		openCodeGoErrorClassificationContains(typeCode, openCodeGoRateLimitMarkers) ||
		openCodeGoErrorClassificationContains(typeCode, openCodeGoResponseErrorMarkers) ||
		openCodeGoErrorMessageContains(message, openCodeGoPoolExhaustionMarkers) ||
		openCodeGoErrorMessageContains(message, openCodeGoSelectionStaleMarkers) ||
		openCodeGoErrorMessageContains(message, openCodeGoInternalFailureMarkers) ||
		openCodeGoStringHasPrivateCandidate(message) ||
		strings.Contains(message, "context canceled") ||
		strings.Contains(message, "context cancelled") ||
		strings.Contains(message, "unexpected end of json") ||
		strings.Contains(message, "unexpected eof")
}

func openCodeGoStringHasPrivateCandidate(value string) bool {
	value = strings.ToLower(value)
	for _, marker := range []string{"opencode go", "console go", "workspace", "endpoint", "channel"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func openCodeGoErrorClassificationContains(classification string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(classification, marker) {
			return true
		}
	}
	return false
}

func openCodeGoErrorMessageContains(message string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

var openCodeGoDistinctPrivateErrorMarkers = []string{
	"opencode",
	"open_code",
	"console go",
	"console_go",
	"console-go",
	"consolego",
	"workspace",
	"wrk_",
	"endpoint",
}

func OpenCodeGoStringHasDistinctPrivateErrorMarker(value string) bool {
	normalized := strings.ToLower(value)
	for _, marker := range openCodeGoDistinctPrivateErrorMarkers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	collapsed := strings.NewReplacer(" ", "", "_", "", "-", "").Replace(normalized)
	for _, marker := range []string{"opencode", "consolego", "workspace"} {
		if strings.Contains(collapsed, marker) {
			return true
		}
	}
	return false
}

func OpenCodeGoStringHasPrivateErrorMarker(value string) bool {
	return OpenCodeGoStringHasDistinctPrivateErrorMarker(value) || strings.Contains(strings.ToLower(value), "channel")
}
