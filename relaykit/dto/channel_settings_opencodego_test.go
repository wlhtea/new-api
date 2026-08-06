package dto

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenCodeGoConfigValidate(t *testing.T) {
	require.NoError(t, (&OpenCodeGoConfig{
		DefaultProtocol:              OpenCodeGoProtocolChat,
		GenericFailoverEnabled:       true,
		GenericFailoverThreshold:     OpenCodeGoGenericFailoverDefaultThreshold,
		GenericFailoverWindowSeconds: OpenCodeGoGenericFailoverDefaultWindowSeconds,
		GenericFailoverMaxBackups:    OpenCodeGoGenericFailoverDefaultMaxBackups,
		GenericFailoverLeaseSeconds:  OpenCodeGoGenericFailoverDefaultLeaseSeconds,
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
		{name: "failover threshold too small", config: OpenCodeGoConfig{GenericFailoverThreshold: 1}, match: "generic_failover_threshold"},
		{name: "failover threshold too large", config: OpenCodeGoConfig{GenericFailoverThreshold: OpenCodeGoGenericFailoverMaxThreshold + 1}, match: "generic_failover_threshold"},
		{name: "failover window too large", config: OpenCodeGoConfig{GenericFailoverWindowSeconds: OpenCodeGoGenericFailoverMaxWindowSeconds + 1}, match: "generic_failover_window_seconds"},
		{name: "failover backups exceed MVP", config: OpenCodeGoConfig{GenericFailoverMaxBackups: 2}, match: "generic_failover_max_backups"},
		{name: "failover lease too large", config: OpenCodeGoConfig{GenericFailoverLeaseSeconds: OpenCodeGoGenericFailoverMaxLeaseSeconds + 1}, match: "generic_failover_lease_seconds"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.ErrorContains(t, test.config.Validate(), test.match)
		})
	}
}
