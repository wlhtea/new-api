package dto

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCodeGoIdentityProxyTemplateRewrite(t *testing.T) {
	raw := "http://account_custom_zone_US_sid_123_time_10:password@proxy.example:8080"
	template, err := ParseOpenCodeGoIdentityProxyTemplate(raw)
	require.NoError(t, err)
	country, minutes := template.InferredPolicy()
	assert.Equal(t, "US", country)
	assert.Equal(t, 10, minutes)

	derived, err := template.Rewrite("gb", "987654321", 15)
	require.NoError(t, err)
	parsed, err := url.Parse(derived)
	require.NoError(t, err)
	assert.Equal(t, "account_custom_zone_GB_sid_987654321_time_15", parsed.User.Username())
	password, ok := parsed.User.Password()
	require.True(t, ok)
	assert.Equal(t, "password", password)
	assert.Equal(t, "proxy.example:8080", parsed.Host)
}

func TestOpenCodeGoIdentityProxyTemplateAppendsMissingOptionalComponents(t *testing.T) {
	template, err := ParseOpenCodeGoIdentityProxyTemplate("https://account_plan_premium_sid_1:secret@proxy.example:8443")
	require.NoError(t, err)

	derived, err := template.Rewrite("ca", "2", 30)
	require.NoError(t, err)
	parsed, err := url.Parse(derived)
	require.NoError(t, err)
	assert.Equal(t, "account_plan_premium_sid_2_zone_CA_time_30", parsed.User.Username())
}

func TestOpenCodeGoIdentityProxyTemplateRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name  string
		proxy string
		match string
	}{
		{name: "empty", proxy: "", match: "http or https"},
		{name: "socks", proxy: "socks5://account_sid_1:secret@proxy.example:1080", match: "http or https"},
		{name: "missing username", proxy: "http://proxy.example:8080", match: "username"},
		{name: "missing password", proxy: "http://account_sid_1@proxy.example:8080", match: "password"},
		{name: "missing sid", proxy: "http://account_zone_US_time_10:secret@proxy.example:8080", match: "sid"},
		{name: "duplicate sid", proxy: "http://account_sid_1_sid_2:secret@proxy.example:8080", match: "multiple sid"},
		{name: "duplicate zone", proxy: "http://account_zone_US_custom_zone_GB_sid_1:secret@proxy.example:8080", match: "multiple zone"},
		{name: "duplicate time", proxy: "http://account_sid_1_time_10_time_20:secret@proxy.example:8080", match: "multiple time"},
		{name: "bad time", proxy: "http://account_sid_1_time_zero:secret@proxy.example:8080", match: "time"},
		{name: "marker as sid value", proxy: "http://account_sid_time_10:secret@proxy.example:8080", match: "malformed sid"},
		{name: "marker as zone value", proxy: "http://account_zone_sid_1:secret@proxy.example:8080", match: "malformed zone"},
		{name: "invalid zone country", proxy: "http://account_zone_USA_sid_1:secret@proxy.example:8080", match: "malformed zone"},
		{name: "non ASCII zone country", proxy: "http://account_zone_%E7%BE%8E%E5%9B%BD_sid_1:secret@proxy.example:8080", match: "malformed zone"},
		{name: "unicode uppercase lookalike zone", proxy: "http://account_zone_u%C5%BF_sid_1:secret@proxy.example:8080", match: "malformed zone"},
		{name: "path", proxy: "http://account_sid_1:secret@proxy.example:8080/path", match: "path"},
		{name: "empty fragment", proxy: "http://account_sid_1:secret@proxy.example:8080#", match: "fragment"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseOpenCodeGoIdentityProxyTemplate(test.proxy)
			require.ErrorContains(t, err, test.match)
		})
	}
}
