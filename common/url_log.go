package common

import (
	"net/url"
	"strings"
)

const redactedURLQuery = "***redacted***"

// SanitizeURLForLog masks credential-like query values while preserving
// ordinary operational parameters. Invalid query syntax fails closed so a
// malformed value cannot bypass the masking boundary.
func SanitizeURLForLog(rawURL string) string {
	if rawURL == "" {
		return rawURL
	}

	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return redactURLQuery(rawURL)
	}
	if parsedURL.RawQuery == "" {
		return rawURL
	}

	query, err := url.ParseQuery(parsedURL.RawQuery)
	if err != nil {
		return redactURLQuery(rawURL)
	}

	changed := false
	for key := range query {
		if !isSensitiveURLQueryKey(key) {
			continue
		}
		query[key] = []string{"***masked***"}
		changed = true
	}
	if !changed {
		return rawURL
	}

	parsedURL.RawQuery = query.Encode()
	return parsedURL.String()
}

// SanitizeRelayRequestPathForLog applies a stricter policy to the public
// inference paths. The observed Claude Code marker is safe and useful to keep;
// every other query is redacted as a unit because it was either rejected by
// the relay contract or may contain an undocumented credential-like value.
func SanitizeRelayRequestPathForLog(rawPath string) string {
	sanitized := SanitizeURLForLog(rawPath)
	parsedURL, err := url.Parse(sanitized)
	if err != nil || !isRelayRequestPath(parsedURL.Path) || parsedURL.RawQuery == "" {
		if err != nil {
			return redactURLQuery(sanitized)
		}
		return sanitized
	}

	query, err := url.ParseQuery(parsedURL.RawQuery)
	if err != nil {
		return redactURLQuery(sanitized)
	}
	if parsedURL.Path == "/v1/messages" && parsedURL.RawQuery == "beta=true" && len(query) == 1 && len(query["beta"]) == 1 && query["beta"][0] == "true" {
		parsedURL.RawQuery = "beta=true"
		return parsedURL.String()
	}
	return redactURLQuery(sanitized)
}

func isRelayRequestPath(path string) bool {
	switch path {
	case "/v1/messages", "/v1/chat/completions", "/v1/responses", "/pg/chat/completions":
		return true
	default:
		return false
	}
}

func redactURLQuery(rawURL string) string {
	base := rawURL
	if queryIndex := strings.IndexByte(base, '?'); queryIndex >= 0 {
		base = base[:queryIndex]
	}
	if fragmentIndex := strings.IndexByte(base, '#'); fragmentIndex >= 0 {
		base = base[:fragmentIndex]
	}
	return base + "?" + redactedURLQuery
}

func isSensitiveURLQueryKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	switch normalized {
	case "key",
		"api_key",
		"api-key",
		"apikey",
		"x-api-key",
		"access_token",
		"refresh_token",
		"id_token",
		"token",
		"authorization",
		"auth",
		"client_secret",
		"secret",
		"password",
		"passwd",
		"signature",
		"sig",
		"awsaccesskeyid",
		"x-amz-credential",
		"x-amz-security-token",
		"x-amz-signature":
		return true
	}
	return strings.Contains(normalized, "token") ||
		strings.Contains(normalized, "secret") ||
		strings.Contains(normalized, "signature")
}
