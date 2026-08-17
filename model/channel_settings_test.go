package model

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelSettingGettersDoNotPersistMalformedJSON(t *testing.T) {
	setupChannelStatusTest(t)

	malformedSetting := `{invalid-setting`
	malformedOtherSettings := `{invalid-settings`
	channel := Channel{
		Name:          "malformed channel settings",
		Key:           "test-key",
		Setting:       &malformedSetting,
		OtherSettings: malformedOtherSettings,
	}
	require.NoError(t, DB.Create(&channel).Error)

	assert.Equal(t, dto.ChannelSettings{}, channel.GetSetting())
	assert.Equal(t, dto.ChannelOtherSettings{}, channel.GetOtherSettings())

	var persisted Channel
	require.NoError(t, DB.First(&persisted, channel.Id).Error)
	require.NotNil(t, persisted.Setting)
	assert.Equal(t, malformedSetting, *persisted.Setting)
	assert.Equal(t, malformedOtherSettings, persisted.OtherSettings)
	assert.Equal(t, malformedSetting, *channel.Setting)
	assert.Equal(t, malformedOtherSettings, channel.OtherSettings)
}

func TestChannelValidateSettingsRejectsInvalidHTTPTransport(t *testing.T) {
	tests := []struct {
		name    string
		setting dto.ChannelSettings
		wantErr string
	}{
		{
			name:    "auto with shards is valid",
			setting: dto.ChannelSettings{HTTPProtocol: "auto", HTTP2ConnectionShards: 4},
		},
		{
			name:    "http1 with shards greater than one rejected",
			setting: dto.ChannelSettings{HTTPProtocol: "http1", HTTP2ConnectionShards: 2},
			wantErr: "http2_connection_shards",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channel := &Channel{}
			channel.SetSetting(tt.setting)
			err := channel.ValidateSettings()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestOpenCodeGoChannelValidateIdentityProxySettings(t *testing.T) {
	tests := []struct {
		name        string
		proxy       string
		config      *dto.OpenCodeGoConfig
		wantErr     string
		wantCountry string
		wantMinutes int
	}{
		{name: "disabled without proxy", config: &dto.OpenCodeGoConfig{}},
		{
			name:        "enabled infers template policy",
			proxy:       "http://account_custom_zone_GB_sid_1_time_20:secret@proxy.example:8080",
			config:      &dto.OpenCodeGoConfig{IdentityProxyEnabled: true},
			wantCountry: "GB",
			wantMinutes: 20,
		},
		{
			name:        "explicit policy overrides template",
			proxy:       "http://account_custom_zone_GB_sid_1_time_20:secret@proxy.example:8080",
			config:      &dto.OpenCodeGoConfig{IdentityProxyEnabled: true, IdentityProxyCountry: "ca", IdentityProxyRotateMinutes: 30},
			wantCountry: "CA",
			wantMinutes: 30,
		},
		{
			name:        "enabled defaults missing optional template policy",
			proxy:       "http://account_sid_1:secret@proxy.example:8080",
			config:      &dto.OpenCodeGoConfig{IdentityProxyEnabled: true},
			wantCountry: dto.OpenCodeGoIdentityProxyDefaultCountry,
			wantMinutes: dto.OpenCodeGoIdentityProxyDefaultRotateMinutes,
		},
		{
			name:    "enabled requires proxy",
			config:  &dto.OpenCodeGoConfig{IdentityProxyEnabled: true, IdentityProxyCountry: "US", IdentityProxyRotateMinutes: 10},
			wantErr: "identity proxy",
		},
		{
			name:    "enabled rejects socks proxy",
			proxy:   "socks5://account_sid_1:secret@proxy.example:1080",
			config:  &dto.OpenCodeGoConfig{IdentityProxyEnabled: true, IdentityProxyCountry: "US", IdentityProxyRotateMinutes: 10},
			wantErr: "http or https",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			channel := &Channel{Type: constant.ChannelTypeOpenCodeGo}
			channel.SetSetting(dto.ChannelSettings{Proxy: test.proxy})
			channel.SetOtherSettings(dto.ChannelOtherSettings{OpenCodeGo: test.config})
			err := channel.ValidateSettings()
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			if test.wantCountry != "" {
				persisted := channel.GetOtherSettings().OpenCodeGo
				require.NotNil(t, persisted)
				assert.Equal(t, test.wantCountry, persisted.IdentityProxyCountry)
				assert.Equal(t, test.wantMinutes, persisted.IdentityProxyRotateMinutes)
			}
		})
	}
}

func TestOpenCodeGoChannelValidateIdentityProxyPreservesUnknownSettings(t *testing.T) {
	setting := dto.ChannelSettings{Proxy: "http://account_sid_1:secret@proxy.example:8080"}
	channel := &Channel{
		Type: constant.ChannelTypeOpenCodeGo,
		OtherSettings: `{
			"future_root":{"enabled":true},
			"opencode_go":{
				"identity_proxy_enabled":true,
				"identity_proxy_country":"us",
				"future_nested":{"mode":"keep"}
			}
		}`,
	}
	channel.SetSetting(setting)

	require.NoError(t, channel.ValidateSettings())
	var persisted map[string]any
	require.NoError(t, common.Unmarshal([]byte(channel.OtherSettings), &persisted))
	assert.Equal(t, map[string]any{"enabled": true}, persisted["future_root"])
	openCodeGo := persisted["opencode_go"].(map[string]any)
	assert.Equal(t, map[string]any{"mode": "keep"}, openCodeGo["future_nested"])
	assert.Equal(t, "US", openCodeGo["identity_proxy_country"])
	assert.Equal(t, float64(dto.OpenCodeGoIdentityProxyDefaultRotateMinutes), openCodeGo["identity_proxy_rotate_minutes"])
}

func TestOpenCodeGoChannelValidateIdentityProxyRejectsExplicitZeroMinutes(t *testing.T) {
	channel := &Channel{
		Type: constant.ChannelTypeOpenCodeGo,
		OtherSettings: `{
			"opencode_go":{
				"identity_proxy_enabled":true,
				"identity_proxy_country":"US",
				"identity_proxy_rotate_minutes":0
			}
		}`,
	}
	channel.SetSetting(dto.ChannelSettings{Proxy: "http://account_sid_1:secret@proxy.example:8080"})

	require.ErrorContains(t, channel.ValidateSettings(), "identity_proxy_rotate_minutes")
}

func TestOpenCodeAPIKeyChannelValidatesOnlySharedProtocolSettings(t *testing.T) {
	poolOnlyInvalid := &dto.OpenCodeGoConfig{
		DefaultProtocol:            dto.OpenCodeGoProtocolResponses,
		ModelProtocols:             map[string]string{"future-*": dto.OpenCodeGoProtocolMessages},
		GenericFailoverThreshold:   1,
		AffinityFallback:           "invalid-pool-value",
		IdentityProxyEnabled:       true,
		IdentityProxyCountry:       "invalid",
		IdentityProxyRotateMinutes: -1,
	}
	channel := &Channel{Type: constant.ChannelTypeOpenCodeAPIKey}
	channel.SetOtherSettings(dto.ChannelOtherSettings{OpenCodeGo: poolOnlyInvalid})
	require.NoError(t, channel.ValidateSettings())

	poolOnlyInvalid.DefaultProtocol = "auto"
	channel.SetOtherSettings(dto.ChannelOtherSettings{OpenCodeGo: poolOnlyInvalid})
	require.ErrorContains(t, channel.ValidateSettings(), "default protocol")

	poolOnlyInvalid.DefaultProtocol = dto.OpenCodeGoProtocolChat
	poolOnlyInvalid.ModelProtocols = map[string]string{"future-[": dto.OpenCodeGoProtocolMessages}
	channel.SetOtherSettings(dto.ChannelOtherSettings{OpenCodeGo: poolOnlyInvalid})
	require.ErrorContains(t, channel.ValidateSettings(), "model protocol pattern")
}

func TestOpenCodeChannelsValidateUnsupportedOptionalFieldPolicyAtSaveTime(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, rawValue := range []string{`""`, `null`, `true`, `1`, `{}`, `[]`, `"ignore_all"`} {
			t.Run(fmt.Sprintf("type-%d/%s", channelType, rawValue), func(t *testing.T) {
				channel := &Channel{
					Type:          channelType,
					OtherSettings: `{"opencode_go":{"unsupported_optional_field_policy":` + rawValue + `}}`,
				}
				err := channel.ValidateSettings()
				require.Error(t, err)
			})
		}
		for _, policy := range []string{
			dto.OpenCodeGoUnsupportedOptionalFieldStrict,
			dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
		} {
			t.Run(fmt.Sprintf("type-%d/%s", channelType, policy), func(t *testing.T) {
				channel := &Channel{Type: channelType}
				channel.SetOtherSettings(dto.ChannelOtherSettings{OpenCodeGo: &dto.OpenCodeGoConfig{
					UnsupportedOptionalFieldPolicy: policy,
				}})
				require.NoError(t, channel.ValidateSettings())
				persisted := channel.GetOtherSettings().OpenCodeGo
				require.NotNil(t, persisted)
				assert.Equal(t, policy, persisted.EffectiveUnsupportedOptionalFieldPolicy())
			})
		}
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		channel := &Channel{Type: channelType, OtherSettings: `{"opencode_go":{"future":true}}`}
		require.NoError(t, channel.ValidateSettings())
		assert.Equal(
			t,
			dto.OpenCodeGoUnsupportedOptionalFieldStrict,
			channel.GetOtherSettings().OpenCodeGo.EffectiveUnsupportedOptionalFieldPolicy(),
		)
	}
}

func TestAdvancedCustomChannelRequiresModelListRouteOnlyWhenUpdateChecksEnabled(t *testing.T) {
	inferenceRoute := dto.AdvancedCustomRoute{
		IncomingPath: "/v1/chat/completions",
		UpstreamPath: "/v1/chat/completions",
		Converter:    "none",
	}

	tests := []struct {
		name          string
		checksEnabled bool
		routes        []dto.AdvancedCustomRoute
		wantErr       string
	}{
		{
			name:   "legacy channel without discovery route remains valid",
			routes: []dto.AdvancedCustomRoute{inferenceRoute},
		},
		{
			name:          "enabled checks require discovery route",
			checksEnabled: true,
			routes:        []dto.AdvancedCustomRoute{inferenceRoute},
			wantErr:       dto.AdvancedCustomModelListPath,
		},
		{
			name:          "enabled checks accept discovery route",
			checksEnabled: true,
			routes: []dto.AdvancedCustomRoute{
				inferenceRoute,
				{
					IncomingPath: dto.AdvancedCustomModelListPath,
					UpstreamPath: dto.AdvancedCustomModelListPath,
					Converter:    "none",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channel := &Channel{Type: constant.ChannelTypeAdvancedCustom}
			channel.SetOtherSettings(dto.ChannelOtherSettings{
				UpstreamModelUpdateCheckEnabled: tt.checksEnabled,
				AdvancedCustom: &dto.AdvancedCustomConfig{
					Routes: tt.routes,
				},
			})

			err := channel.ValidateSettings()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
