package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeOpenCodeGoAuthCookie(t *testing.T) {
	tests := map[string]string{
		"raw value":          "Fe26.2**token",
		"padded raw value":   "padded-token==",
		"auth pair":          "auth=Fe26.2**token",
		"full Cookie header": "other=value; auth=Fe26.2**token; oc_locale=en",
		"header prefix":      "Cookie: other=value; auth=Fe26.2**token",
	}

	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			value, err := NormalizeOpenCodeGoAuthCookie(input)
			require.NoError(t, err)
			if name == "padded raw value" {
				require.Equal(t, "padded-token==", value)
			} else {
				require.Equal(t, "Fe26.2**token", value)
			}
		})
	}
}

func TestNormalizeOpenCodeGoAuthCookieRejectsAmbiguousOrInvalidInput(t *testing.T) {
	inputs := []string{
		"",
		"auth=",
		"other=value; oc_locale=en",
		"auth=first; auth=second",
		"raw token with spaces",
		"raw\ntoken",
		"raw;token",
	}
	for _, input := range inputs {
		_, err := NormalizeOpenCodeGoAuthCookie(input)
		require.Error(t, err, input)
	}
}

func TestParseOpenCodeGoAuthCookieLinesDeduplicatesNormalizedValues(t *testing.T) {
	values, err := ParseOpenCodeGoAuthCookieLines("raw-A\r\nauth=raw-B\nraw-A\n")
	require.NoError(t, err)
	require.Equal(t, []string{"raw-A", "raw-B"}, values)
}

func TestBuildOpenCodeGoCookieHeader(t *testing.T) {
	header, err := BuildOpenCodeGoCookieHeader("auth=Fe26.2**token")
	require.NoError(t, err)
	require.Equal(t, "auth=Fe26.2**token; oc_locale=zh", header)
}
