package dto

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenCodeGoConfigValidate(t *testing.T) {
	require.NoError(t, (&OpenCodeGoConfig{
		DefaultProtocol: OpenCodeGoProtocolChat,
		ModelProtocols: map[string]string{
			"future-*": OpenCodeGoProtocolMessages,
		},
	}).Validate())

	tests := []struct {
		name   string
		config OpenCodeGoConfig
		match  string
	}{
		{name: "invalid default", config: OpenCodeGoConfig{DefaultProtocol: "auto"}, match: "default protocol"},
		{name: "empty pattern", config: OpenCodeGoConfig{ModelProtocols: map[string]string{"": OpenCodeGoProtocolChat}}, match: "cannot be empty"},
		{name: "invalid wildcard", config: OpenCodeGoConfig{ModelProtocols: map[string]string{"future-[": OpenCodeGoProtocolChat}}, match: "invalid"},
		{name: "invalid protocol", config: OpenCodeGoConfig{ModelProtocols: map[string]string{"future": "auto"}}, match: "invalid"},
		{name: "normalized duplicate", config: OpenCodeGoConfig{ModelProtocols: map[string]string{"Future-*": OpenCodeGoProtocolChat, " future-* ": OpenCodeGoProtocolMessages}}, match: "duplicate"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.ErrorContains(t, test.config.Validate(), test.match)
		})
	}
}
