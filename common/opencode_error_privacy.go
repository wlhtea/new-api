package common

import (
	"regexp"
	"strings"

	"github.com/QuantumNous/new-api/constant"
)

var (
	openCodeGoPrivateErrorHeaderPattern     = regexp.MustCompile(`(?i)\b(?:proxy-)?authorization\s*[:=]|\b(?:x-(?:api|goog)-key|api[_ -]?key|(?:set-)?cookie)\s*[:=]`)
	openCodeGoPrivateErrorCredentialPattern = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[a-z0-9._~+/=-]{6,}`)
	openCodeGoPrivateErrorAPIKeyPattern     = regexp.MustCompile(`(?i)\bsk-[a-z0-9][a-z0-9._-]{3,}\b`)
	openCodeGoPrivateErrorIdentifierPattern = regexp.MustCompile(`(?i)\b(?:access[_ -]?token|refresh[_ -]?token|id[_ -]?token|session(?:[_ -]?id)?|token[_ -]?id|x[_ -]?opencode[_ -]?session|request[_ -]?id|upstream[_ -]?request[_ -]?id|trace[_ -]?id|correlation[_ -]?id|workspace(?:[_ -]?id)?|endpoint(?:[_ -]?(?:url|host))?|proxy(?:[_ -]?(?:url|host))?|password|credential|secret)\s*[:=]\s*[^\s,;]+`)
	openCodeGoPrivateErrorURLPattern        = regexp.MustCompile(`(?i)\b(?:https?|socks5h?)://[^\s"'<>]+`)
	openCodeGoPrivateErrorHostPattern       = regexp.MustCompile(`(?i)\b(?:localhost|(?:10|127)\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[0-1])\.\d{1,3}\.\d{1,3})(?::\d{1,5})?\b|\b[a-z0-9][a-z0-9.-]*\.(?:internal|local|localhost|lan)\b`)
)

// OpenCodeGoPublicClientRequestMessage strips known provider wrappers from an
// explicitly classified client error and rejects any remaining private detail.
func OpenCodeGoPublicClientRequestMessage(message string) string {
	message = trimOpenCodeGoStoredStatusPrefix(message)
	for {
		original := message
		for _, prefix := range []string{
			"Error from provider (Console Go):",
			"Error from provider (ConsoleGo):",
			"Error from provider (OpenCode Go):",
			"Error from provider (OpenCode):",
			"Upstream request failed:",
			"Console Go rejected ",
			"Console Go rejected:",
			"ConsoleGo rejected ",
			"ConsoleGo rejected:",
			"OpenCode Go rejected ",
			"OpenCode Go rejected:",
			"OpenCode rejected ",
			"OpenCode rejected:",
			"[invalid_request_error]",
			"[invalid_request]",
			"[validation_error]",
			"[bad_request]",
			"[bad_request_body]",
		} {
			message = trimOpenCodeGoPublicPrefix(message, prefix)
		}
		if message == original {
			break
		}
	}
	if message == "" || OpenCodeGoErrorHasPrivateDetail(message) {
		return constant.OpenCodeGoPublicInvalidRequestMessage
	}
	return message
}

func trimOpenCodeGoStoredStatusPrefix(value string) string {
	value = strings.TrimSpace(value)
	const prefix = "status_code="
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return value
	}
	rest := value[len(prefix):]
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return value
	}
	rest = strings.TrimSpace(rest[digits:])
	if !strings.HasPrefix(rest, ",") {
		return value
	}
	return strings.TrimSpace(rest[1:])
}

func trimOpenCodeGoPublicPrefix(value, prefix string) string {
	value = strings.TrimSpace(value)
	if len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
		return strings.TrimSpace(value[len(prefix):])
	}
	return value
}

// OpenCodeGoErrorHasPrivateDetail scans both plain and JSON-escaped error
// representations. It is intended only for recognized error envelopes.
func OpenCodeGoErrorHasPrivateDetail(values ...string) bool {
	for _, value := range values {
		if openCodeGoErrorStringHasPrivateDetail(value) {
			return true
		}
		var decoded any
		if UnmarshalJsonStr(value, &decoded) == nil && openCodeGoJSONHasPrivateErrorMarker(decoded) {
			return true
		}
	}
	return false
}

// RedactOpenCodeGoPrivateErrorJSONFields removes private values from a
// structured upstream error before it reaches administrator or server logs.
// Text embedded in non-private fields is handled by the caller's normal
// sanitizer after this structural pass.
func RedactOpenCodeGoPrivateErrorJSONFields(value string) string {
	var decoded any
	if UnmarshalJsonStr(value, &decoded) != nil || !redactOpenCodeGoPrivateErrorJSONFields(decoded) {
		return value
	}
	encoded, err := Marshal(decoded)
	if err != nil {
		return value
	}
	return string(encoded)
}

func redactOpenCodeGoPrivateErrorJSONFields(value any) bool {
	redacted := false
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if openCodeGoJSONKeyHasPrivateDetail(key) {
				typed[key] = "[redacted]"
				redacted = true
				continue
			}
			redacted = redactOpenCodeGoPrivateErrorJSONFields(child) || redacted
		}
	case []any:
		for _, child := range typed {
			redacted = redactOpenCodeGoPrivateErrorJSONFields(child) || redacted
		}
	}
	return redacted
}

func openCodeGoErrorStringHasPrivateDetail(value string) bool {
	return constant.OpenCodeGoStringHasPrivateErrorMarker(value) ||
		openCodeGoPrivateErrorHeaderPattern.MatchString(value) ||
		openCodeGoPrivateErrorCredentialPattern.MatchString(value) ||
		openCodeGoPrivateErrorAPIKeyPattern.MatchString(value) ||
		openCodeGoPrivateErrorIdentifierPattern.MatchString(value) ||
		openCodeGoPrivateErrorURLPattern.MatchString(value) ||
		openCodeGoPrivateErrorHostPattern.MatchString(value)
}

func openCodeGoJSONKeyHasPrivateDetail(key string) bool {
	if openCodeGoErrorStringHasPrivateDetail(key) {
		return true
	}
	normalized := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(key)))
	switch normalized {
	case "authorization", "proxy_authorization", "cookie", "set_cookie",
		"x_api_key", "x_goog_api_key", "api_key", "access_token",
		"refresh_token", "id_token", "session", "session_id", "token_id",
		"x_opencode_session", "request_id", "requestid", "x_request_id",
		"trace_id", "traceid", "x_trace_id", "correlation_id", "correlationid",
		"upstream_request_id", "upstreamrequestid", "workspace", "workspace_id", "workspaceid",
		"endpoint", "endpoint_url", "endpointurl", "endpoint_host", "endpointhost",
		"proxy", "proxy_url", "proxy_host", "proxyhost",
		"password", "credential", "secret":
		return true
	}
	return false
}

func openCodeGoJSONHasPrivateErrorMarker(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if openCodeGoJSONKeyHasPrivateDetail(key) || openCodeGoJSONHasPrivateErrorMarker(child) {
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
		return openCodeGoErrorStringHasPrivateDetail(typed)
	}
	return false
}
