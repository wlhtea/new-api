package middleware

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestSanitizeAccessLogPathRedactsOpenCodeGoDynamicIdentifiers(t *testing.T) {
	previousSecret := common.CryptoSecret
	common.CryptoSecret = "test-only-access-log-secret"
	t.Cleanup(func() { common.CryptoSecret = previousSecret })

	for _, path := range []string{
		"/api/channel/7/opencode-go/workspaces/workspace-private-value/refresh",
		"/api/channel/7/opencode-go/identities/identity-private-value/enabled",
	} {
		sanitized := sanitizeAccessLogPath(path)
		require.NotContains(t, sanitized, "private-value")
		require.Contains(t, sanitized, "ocg_")
	}
	require.Equal(t, "/api/channel/7/opencode-go/workspaces/non-members", sanitizeAccessLogPath("/api/channel/7/opencode-go/workspaces/non-members"))
	require.Equal(t, "/v1/messages", sanitizeAccessLogPath("/v1/messages"))
}

func TestSanitizeAccessLogPathRedactsInferenceQueryValues(t *testing.T) {
	require.Equal(t, "/v1/messages?beta=true", sanitizeAccessLogPath("/v1/messages?beta=true"))

	for _, path := range []string{
		"/v1/messages?api_key=client-secret",
		"/v1/messages?beta=false",
		"/v1/chat/completions?unknown=private-value",
		"/v1/responses?broken=%ZZ",
	} {
		sanitized := sanitizeAccessLogPath(path)
		require.NotContains(t, sanitized, "client-secret", path)
		require.NotContains(t, sanitized, "private-value", path)
		require.Contains(t, sanitized, "***redacted***", path)
	}
}
