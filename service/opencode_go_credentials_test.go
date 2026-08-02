package service

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestOpenCodeGoCredentialCodecRoundTripAndRandomNonce(t *testing.T) {
	codec, err := NewOpenCodeGoCredentialCodec("test-only-stable-secret")
	require.NoError(t, err)

	first, err := codec.Encrypt(OpenCodeGoCredentialAuthCookie, 42, "identity-a", "cookie-value")
	require.NoError(t, err)
	second, err := codec.Encrypt(OpenCodeGoCredentialAuthCookie, 42, "identity-a", "cookie-value")
	require.NoError(t, err)

	require.NotEqual(t, first, second)
	require.True(t, strings.HasPrefix(first, openCodeGoCredentialEnvelopePrefix))
	plaintext, err := codec.Decrypt(OpenCodeGoCredentialAuthCookie, 42, "identity-a", first)
	require.NoError(t, err)
	require.Equal(t, "cookie-value", plaintext)
}

func TestOpenCodeGoCredentialCodecFailsClosed(t *testing.T) {
	codec, err := NewOpenCodeGoCredentialCodec("test-only-stable-secret")
	require.NoError(t, err)
	envelope, err := codec.Encrypt(OpenCodeGoCredentialAPIKey, 42, "workspace-a", "sk-test-value")
	require.NoError(t, err)

	t.Run("tamper", func(t *testing.T) {
		tampered := envelope[:len(envelope)-1] + "A"
		if strings.HasSuffix(envelope, "A") {
			tampered = envelope[:len(envelope)-1] + "B"
		}
		_, err := codec.Decrypt(OpenCodeGoCredentialAPIKey, 42, "workspace-a", tampered)
		require.Error(t, err)
	})

	t.Run("wrong secret", func(t *testing.T) {
		other, err := NewOpenCodeGoCredentialCodec("different-test-secret")
		require.NoError(t, err)
		_, err = other.Decrypt(OpenCodeGoCredentialAPIKey, 42, "workspace-a", envelope)
		require.Error(t, err)
	})

	t.Run("row swap", func(t *testing.T) {
		_, err := codec.Decrypt(OpenCodeGoCredentialAPIKey, 42, "workspace-b", envelope)
		require.Error(t, err)
	})

	t.Run("channel swap", func(t *testing.T) {
		_, err := codec.Decrypt(OpenCodeGoCredentialAPIKey, 43, "workspace-a", envelope)
		require.Error(t, err)
	})

	t.Run("kind swap", func(t *testing.T) {
		_, err := codec.Decrypt(OpenCodeGoCredentialAuthCookie, 42, "workspace-a", envelope)
		require.Error(t, err)
	})

	t.Run("version mismatch", func(t *testing.T) {
		_, err := codec.Decrypt(OpenCodeGoCredentialAPIKey, 42, "workspace-a", strings.Replace(envelope, "ocg:v1:", "ocg:v2:", 1))
		require.Error(t, err)
	})
}

func TestOpenCodeGoCredentialFingerprintIsStableAndSeparated(t *testing.T) {
	codec, err := NewOpenCodeGoCredentialCodec("test-only-stable-secret")
	require.NoError(t, err)

	first, err := codec.Fingerprint(OpenCodeGoCredentialAuthCookie, "same-value")
	require.NoError(t, err)
	second, err := codec.Fingerprint(OpenCodeGoCredentialAuthCookie, "same-value")
	require.NoError(t, err)
	apiKey, err := codec.Fingerprint(OpenCodeGoCredentialAPIKey, "same-value")
	require.NoError(t, err)

	require.Equal(t, first, second)
	require.Len(t, first, 64)
	require.NotEqual(t, first, apiKey)
}

func TestConfiguredOpenCodeGoCredentialCodecRequiresExplicitSecret(t *testing.T) {
	originalSecret := common.CryptoSecret
	originalConfigured := common.CryptoSecretExplicitlyConfigured
	t.Cleanup(func() {
		common.CryptoSecret = originalSecret
		common.CryptoSecretExplicitlyConfigured = originalConfigured
	})

	common.CryptoSecret = "process-fallback"
	common.CryptoSecretExplicitlyConfigured = false
	codec, err := NewConfiguredOpenCodeGoCredentialCodec()
	require.Nil(t, codec)
	require.ErrorIs(t, err, ErrOpenCodeGoCryptoSecretRequired)

	common.CryptoSecret = "explicit-test-secret"
	common.CryptoSecretExplicitlyConfigured = true
	codec, err = NewConfiguredOpenCodeGoCredentialCodec()
	require.NoError(t, err)
	require.NotNil(t, codec)
}
