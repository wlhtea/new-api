package opencodego

import (
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveProtocolPrecedence(t *testing.T) {
	tests := []struct {
		name   string
		model  string
		config *dto.OpenCodeGoConfig
		want   Protocol
	}{
		{name: "built in chat", model: "glm-5.2", want: ProtocolChat},
		{name: "built in messages", model: "minimax-m3", want: ProtocolMessages},
		{name: "built in responses", model: "gpt-5.6-luna", want: ProtocolResponses},
		{name: "provider prefixed family", model: "provider/qwen3.8-next", want: ProtocolMessages},
		{
			name:  "exact override beats built in",
			model: "glm-5.2",
			config: &dto.OpenCodeGoConfig{ModelProtocols: map[string]string{
				" GLM-5.2 ": dto.OpenCodeGoProtocolResponses,
			}},
			want: ProtocolResponses,
		},
		{
			name:  "longest wildcard wins",
			model: "future-code-v2",
			config: &dto.OpenCodeGoConfig{ModelProtocols: map[string]string{
				"future-*":      dto.OpenCodeGoProtocolChat,
				"future-code-*": dto.OpenCodeGoProtocolMessages,
			}},
			want: ProtocolMessages,
		},
		{
			name:  "explicit fallback",
			model: "unknown-model",
			config: &dto.OpenCodeGoConfig{
				DefaultProtocol: dto.OpenCodeGoProtocolChat,
			},
			want: ProtocolChat,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveProtocol(test.model, test.config)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
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
}
