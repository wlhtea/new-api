package dto

import (
	"encoding/json"
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
		AffinityFallback:             "token",
		LoadAwareEnabled:             true,
		IdentityProxyEnabled:         true,
		IdentityProxyCountry:         "us",
		IdentityProxyRotateMinutes:   10,
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
		{name: "affinity fallback unknown", config: OpenCodeGoConfig{AffinityFallback: "apikey"}, match: "affinity_fallback"},
		{name: "identity proxy country too long", config: OpenCodeGoConfig{IdentityProxyEnabled: true, IdentityProxyCountry: "USA", IdentityProxyRotateMinutes: 10}, match: "identity_proxy_country"},
		{name: "identity proxy country non ASCII", config: OpenCodeGoConfig{IdentityProxyEnabled: true, IdentityProxyCountry: "\u7f8e\u56fd", IdentityProxyRotateMinutes: 10}, match: "identity_proxy_country"},
		{name: "identity proxy country unicode uppercase lookalike", config: OpenCodeGoConfig{IdentityProxyEnabled: true, IdentityProxyCountry: "u\u017f", IdentityProxyRotateMinutes: 10}, match: "identity_proxy_country"},
		{name: "identity proxy interval too small", config: OpenCodeGoConfig{IdentityProxyEnabled: true, IdentityProxyCountry: "US", IdentityProxyRotateMinutes: -1}, match: "identity_proxy_rotate_minutes"},
		{name: "identity proxy interval too large", config: OpenCodeGoConfig{IdentityProxyEnabled: true, IdentityProxyCountry: "US", IdentityProxyRotateMinutes: OpenCodeGoIdentityProxyMaxRotateMinutes + 1}, match: "identity_proxy_rotate_minutes"},
	}
	require.NoError(t, (&OpenCodeGoConfig{AffinityFallback: "none"}).Validate())

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.ErrorContains(t, test.config.Validate(), test.match)
		})
	}
}

func TestOpenCodeGoConfigValidateProtocolRoutingIgnoresPoolOnlySettings(t *testing.T) {
	config := OpenCodeGoConfig{
		DefaultProtocol:              OpenCodeGoProtocolResponses,
		ModelProtocols:               map[string]string{"future-*": OpenCodeGoProtocolMessages},
		GenericFailoverThreshold:     1,
		AffinityFallback:             "invalid-pool-value",
		IdentityProxyEnabled:         true,
		IdentityProxyCountry:         "invalid",
		IdentityProxyRotateMinutes:   -1,
		ReferralRewardsMaxPerRun:     func() *int { value := 21; return &value }(),
		GenericFailoverWindowSeconds: OpenCodeGoGenericFailoverMaxWindowSeconds + 1,
	}

	require.NoError(t, config.ValidateProtocolRouting())
	require.Error(t, config.Validate())

	config.DefaultProtocol = "auto"
	require.ErrorContains(t, config.ValidateProtocolRouting(), "default protocol")
	config.DefaultProtocol = OpenCodeGoProtocolChat
	config.ModelProtocols = map[string]string{"future-[": OpenCodeGoProtocolMessages}
	require.ErrorContains(t, config.ValidateProtocolRouting(), "model protocol pattern")
}

func TestOpenCodeGoConfigRejectsExplicitZeroRotateMinutes(t *testing.T) {
	var config OpenCodeGoConfig
	require.NoError(t, json.Unmarshal([]byte(`{
		"identity_proxy_enabled": true,
		"identity_proxy_country": "US",
		"identity_proxy_rotate_minutes": 0
	}`), &config))

	require.ErrorContains(t, config.Validate(), "identity_proxy_rotate_minutes")
	encoded, err := json.Marshal(config)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"identity_proxy_enabled": true,
		"identity_proxy_country": "US",
		"identity_proxy_rotate_minutes": 0
	}`, string(encoded))
}

func TestChannelOtherSettingsPreserveUnknownJSONFields(t *testing.T) {
	raw := `{
		"future_root": {"enabled": true},
		"opencode_go": {
			"identity_proxy_enabled": true,
			"identity_proxy_country": "us",
			"future_nested": {"mode": "keep"}
		}
	}`
	var settings ChannelOtherSettings
	require.NoError(t, json.Unmarshal([]byte(raw), &settings))
	require.NotNil(t, settings.OpenCodeGo)
	require.True(t, settings.OpenCodeGo.NormalizeIdentityProxy())

	encoded, err := json.Marshal(settings)
	require.NoError(t, err)
	var roundTrip map[string]any
	require.NoError(t, json.Unmarshal(encoded, &roundTrip))
	require.Equal(t, map[string]any{"enabled": true}, roundTrip["future_root"])
	openCodeGo := roundTrip["opencode_go"].(map[string]any)
	require.Equal(t, map[string]any{"mode": "keep"}, openCodeGo["future_nested"])
	require.Equal(t, "US", openCodeGo["identity_proxy_country"])
	require.Equal(t, float64(OpenCodeGoIdentityProxyDefaultRotateMinutes), openCodeGo["identity_proxy_rotate_minutes"])
}

func TestOpenCodeGoConfigNormalizeIdentityProxy(t *testing.T) {
	config := OpenCodeGoConfig{IdentityProxyEnabled: true, IdentityProxyCountry: " gb "}
	require.True(t, config.NormalizeIdentityProxy())
	require.Equal(t, "GB", config.IdentityProxyCountry)
	require.Equal(t, OpenCodeGoIdentityProxyDefaultRotateMinutes, config.IdentityProxyRotateMinutes)
	require.NoError(t, config.Validate())
}

func TestOpenCodeGoBillingUsageConversionPresenceAndDefault(t *testing.T) {
	var absent OpenCodeGoConfig
	require.NoError(t, json.Unmarshal([]byte(`{"default_protocol":"chat"}`), &absent))
	require.Nil(t, absent.BillingUsageConversionEnabled)
	require.True(t, absent.IsBillingUsageConversionEnabled())

	var disabled OpenCodeGoConfig
	require.NoError(t, json.Unmarshal([]byte(`{
		"billing_usage_conversion_enabled": false,
		"future_nested": {"preserve": true}
	}`), &disabled))
	require.NotNil(t, disabled.BillingUsageConversionEnabled)
	require.False(t, *disabled.BillingUsageConversionEnabled)
	require.False(t, disabled.IsBillingUsageConversionEnabled())

	encoded, err := json.Marshal(disabled)
	require.NoError(t, err)
	var roundTrip map[string]any
	require.NoError(t, json.Unmarshal(encoded, &roundTrip))
	require.Equal(t, false, roundTrip["billing_usage_conversion_enabled"])
	require.Equal(t, map[string]any{"preserve": true}, roundTrip["future_nested"])

	enabled := true
	config := OpenCodeGoConfig{BillingUsageConversionEnabled: &enabled}
	require.True(t, config.IsBillingUsageConversionEnabled())
	encoded, err = json.Marshal(config)
	require.NoError(t, err)
	require.JSONEq(t, `{"billing_usage_conversion_enabled":true}`, string(encoded))
}

func TestOpenCodeGoUnsupportedOptionalFieldPolicyPresenceAndValidation(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		effective string
		wantError bool
		wantJSON  string
	}{
		{
			name:      "legacy absence defaults to strict",
			raw:       `{"future_nested":{"preserve":true}}`,
			effective: OpenCodeGoUnsupportedOptionalFieldStrict,
			wantJSON:  `{"future_nested":{"preserve":true}}`,
		},
		{
			name:      "explicit strict",
			raw:       `{"unsupported_optional_field_policy":"strict"}`,
			effective: OpenCodeGoUnsupportedOptionalFieldStrict,
			wantJSON:  `{"unsupported_optional_field_policy":"strict"}`,
		},
		{
			name:      "explicit drop known optional",
			raw:       `{"unsupported_optional_field_policy":"drop_known_optional"}`,
			effective: OpenCodeGoUnsupportedOptionalFieldDropKnown,
			wantJSON:  `{"unsupported_optional_field_policy":"drop_known_optional"}`,
		},
		{
			name:      "explicit empty remains invalid",
			raw:       `{"unsupported_optional_field_policy":""}`,
			effective: "",
			wantError: true,
			wantJSON:  `{"unsupported_optional_field_policy":""}`,
		},
		{
			name:      "explicit null remains invalid",
			raw:       `{"unsupported_optional_field_policy":null}`,
			effective: "",
			wantError: true,
			wantJSON:  `{"unsupported_optional_field_policy":null}`,
		},
		{
			name:      "unknown value",
			raw:       `{"unsupported_optional_field_policy":"ignore_all"}`,
			effective: "ignore_all",
			wantError: true,
			wantJSON:  `{"unsupported_optional_field_policy":"ignore_all"}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var config OpenCodeGoConfig
			require.NoError(t, json.Unmarshal([]byte(test.raw), &config))
			require.Equal(t, test.effective, config.EffectiveUnsupportedOptionalFieldPolicy())

			validateErr := config.Validate()
			routingErr := config.ValidateProtocolRouting()
			if test.wantError {
				require.ErrorContains(t, validateErr, "unsupported_optional_field_policy")
				require.ErrorContains(t, routingErr, "unsupported_optional_field_policy")
			} else {
				require.NoError(t, validateErr)
				require.NoError(t, routingErr)
			}

			encoded, err := json.Marshal(config)
			require.NoError(t, err)
			require.JSONEq(t, test.wantJSON, string(encoded))
		})
	}
}

func TestOpenCodeGoUnsupportedOptionalFieldPolicyPreservesWrongJSONTypesForValidation(t *testing.T) {
	for _, rawValue := range []string{`true`, `1`, `{}`, `[]`} {
		t.Run(rawValue, func(t *testing.T) {
			var config OpenCodeGoConfig
			require.NoError(t, json.Unmarshal(
				[]byte(`{"unsupported_optional_field_policy":`+rawValue+`}`),
				&config,
			))
			require.Equal(t, "", config.EffectiveUnsupportedOptionalFieldPolicy())
			require.ErrorContains(t, config.Validate(), "unsupported_optional_field_policy")
			require.ErrorContains(t, config.ValidateProtocolRouting(), "unsupported_optional_field_policy")

			encoded, err := json.Marshal(config)
			require.NoError(t, err)
			require.JSONEq(
				t,
				`{"unsupported_optional_field_policy":`+rawValue+`}`,
				string(encoded),
			)
		})
	}
}

func TestOpenCodeGoUnsupportedOptionalFieldPolicyProgrammaticValues(t *testing.T) {
	for _, policy := range []string{
		OpenCodeGoUnsupportedOptionalFieldStrict,
		OpenCodeGoUnsupportedOptionalFieldDropKnown,
	} {
		config := OpenCodeGoConfig{UnsupportedOptionalFieldPolicy: policy}
		require.Equal(t, policy, config.EffectiveUnsupportedOptionalFieldPolicy())
		require.NoError(t, config.Validate())
		require.NoError(t, config.ValidateProtocolRouting())
	}
}
