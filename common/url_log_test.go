package common

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeRelayRequestPathForLogKeepsCanonicalClaudeBetaMarker(t *testing.T) {
	assert.Equal(t, "/v1/messages?beta=true", SanitizeRelayRequestPathForLog("/v1/messages?beta=true"))
}

func TestSanitizeRelayRequestPathForLogRedactsRejectedQueries(t *testing.T) {
	for _, rawPath := range []string{
		"/v1/messages?beta=false",
		"/v1/messages?beta=true&api_key=sk-client-secret",
		"/v1/chat/completions?token=client-secret",
		"/v1/responses?unknown=private-value",
		"/v1/messages?broken=%ZZ",
		"/v1/messages?beta=true&",
		"/v1/messages?beta=%74rue",
	} {
		sanitized := SanitizeRelayRequestPathForLog(rawPath)
		assert.NotContains(t, sanitized, "sk-client-secret", rawPath)
		assert.NotContains(t, sanitized, "client-secret", rawPath)
		assert.NotContains(t, sanitized, "private-value", rawPath)
		assert.Contains(t, sanitized, "***redacted***", rawPath)
	}
}

func TestSanitizeURLForLogFailsClosedForMalformedQueryAndMasksDuplicates(t *testing.T) {
	assert.Equal(t, "/v1/models?***redacted***", SanitizeURLForLog("/v1/models?broken=%ZZ"))

	sanitized := SanitizeURLForLog("https://example.test/v1/models?api_key=one&api_key=two")
	parsed, err := url.Parse(sanitized)
	require.NoError(t, err)
	assert.Equal(t, []string{"***masked***"}, parsed.Query()["api_key"])
}
