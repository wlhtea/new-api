package common

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenCodeGoDiagnosticRefIsStableAndOpaque(t *testing.T) {
	previousSecret := CryptoSecret
	CryptoSecret = "test-only-opencode-go-log-secret"
	t.Cleanup(func() { CryptoSecret = previousSecret })

	first := OpenCodeGoDiagnosticRef("diagnostic-workspace", "workspace-private-value")
	second := OpenCodeGoDiagnosticRef("diagnostic-workspace", "workspace-private-value")

	require.Equal(t, first, second)
	require.True(t, strings.HasPrefix(first, "ocg_"))
	require.Len(t, first, 26)
	require.NotContains(t, first, "workspace-private-value")
	require.NotEqual(t, first, OpenCodeGoDiagnosticRef("diagnostic-identity", "workspace-private-value"))
}
