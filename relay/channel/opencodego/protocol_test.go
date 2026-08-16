package opencodego

import (
	"sort"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveProtocolPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		config     *dto.OpenCodeGoConfig
		want       Protocol
		wantSource ProtocolResolutionSource
		wantMatch  string
	}{
		{name: "built in chat", model: "glm-5.2", want: ProtocolChat, wantSource: ProtocolSourceExactBuiltIn},
		{name: "built in messages", model: "minimax-m3", want: ProtocolMessages, wantSource: ProtocolSourceExactBuiltIn},
		{name: "new advertised qwen exact route", model: " QWEN3.8-MAX ", want: ProtocolMessages, wantSource: ProtocolSourceExactBuiltIn},
		{name: "built in responses", model: "gpt-5.6-luna", want: ProtocolResponses, wantSource: ProtocolSourceExactBuiltIn},
		{name: "provider prefixed family", model: "provider/qwen3.8-next", want: ProtocolMessages, wantSource: ProtocolSourceFamilyFallback},
		{
			name:  "exact override beats built in",
			model: "glm-5.2",
			config: &dto.OpenCodeGoConfig{ModelProtocols: map[string]string{
				" GLM-5.2 ": dto.OpenCodeGoProtocolResponses,
			}},
			want:       ProtocolResponses,
			wantSource: ProtocolSourceExactOverride,
			wantMatch:  "glm-5.2",
		},
		{
			name:  "longest wildcard wins",
			model: "future-code-v2",
			config: &dto.OpenCodeGoConfig{ModelProtocols: map[string]string{
				"future-*":      dto.OpenCodeGoProtocolChat,
				"future-code-*": dto.OpenCodeGoProtocolMessages,
			}},
			want:       ProtocolMessages,
			wantSource: ProtocolSourceWildcardOverride,
			wantMatch:  "future-code-*",
		},
		{
			name:  "explicit fallback",
			model: "unknown-model",
			config: &dto.OpenCodeGoConfig{
				DefaultProtocol: dto.OpenCodeGoProtocolChat,
			},
			want:       ProtocolChat,
			wantSource: ProtocolSourceConfiguredDefault,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveProtocolWithSource(test.model, test.config)
			require.NoError(t, err)
			assert.Equal(t, test.want, got.Protocol)
			assert.Equal(t, test.wantSource, got.Source)
			assert.Equal(t, test.wantMatch, got.MatchedPattern)

			compatibilityProtocol, err := ResolveProtocol(test.model, test.config)
			require.NoError(t, err)
			assert.Equal(t, got.Protocol, compatibilityProtocol)
		})
	}
}

func TestResolveProtocolRejectsUnknownAndAmbiguousModels(t *testing.T) {
	_, err := ResolveProtocol("unknown-model", nil)
	require.ErrorContains(t, err, "protocol is not configured")

	_, err = ResolveProtocol("glm-5.2", &dto.OpenCodeGoConfig{ModelProtocols: map[string]string{
		"GLM-5.2":  dto.OpenCodeGoProtocolChat,
		" glm-5.2": dto.OpenCodeGoProtocolMessages,
	}})
	require.ErrorContains(t, err, "duplicate")

	_, err = ResolveProtocol("glm-5.2", &dto.OpenCodeGoConfig{
		DefaultProtocol: "invalid-unused-default",
	})
	require.ErrorContains(t, err, "invalid OpenCode Go protocol configuration")

	_, err = ResolveProtocol("glm-5.2", &dto.OpenCodeGoConfig{ModelProtocols: map[string]string{
		"unmatched-model": "invalid-unused-protocol",
	}})
	require.ErrorContains(t, err, "invalid OpenCode Go protocol configuration")

	_, err = ResolveProtocol("glm-5.2", &dto.OpenCodeGoConfig{ModelProtocols: map[string]string{
		"unmatched-[": dto.OpenCodeGoProtocolMessages,
	}})
	require.ErrorContains(t, err, "invalid OpenCode Go protocol configuration")
}

func TestAdvertisedModelsHaveExactBuiltInProtocolEvidence(t *testing.T) {
	manifest := AdvertisedModelProtocolManifest()
	require.Len(t, manifest, 19)
	require.Len(t, ModelList, len(manifest))
	require.Len(t, builtInModelProtocols, len(manifest))

	seen := make(map[string]struct{}, len(manifest))
	manifestModels := make([]string, 0, len(manifest))
	for index, entry := range manifest {
		normalized := strings.ToLower(strings.TrimSpace(entry.Model))
		require.Equal(t, entry.Model, normalized)
		require.NotEmpty(t, entry.Protocol)
		_, duplicate := seen[normalized]
		require.False(t, duplicate, "duplicate advertised model %q", entry.Model)
		seen[normalized] = struct{}{}
		manifestModels = append(manifestModels, entry.Model)
		assert.Equal(t, entry.Model, ModelList[index])

		resolution, err := ResolveProtocolWithSource(entry.Model, nil)
		require.NoError(t, err)
		assert.Equal(t, entry.Protocol, resolution.Protocol)
		assert.Equal(t, ProtocolSourceExactBuiltIn, resolution.Source)
	}

	sortedManifest := append([]string(nil), manifestModels...)
	sortedModelList := append([]string(nil), ModelList...)
	sort.Strings(sortedManifest)
	sort.Strings(sortedModelList)
	assert.Equal(t, sortedManifest, sortedModelList)
}
