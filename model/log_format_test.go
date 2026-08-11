package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/require"
)

// TestFormatUserLogsStripsQuotaSaturation verifies the admin-only quota
// saturation marker (nested under other.admin_info) is removed for non-admin
// log views, since formatUserLogs strips the whole admin_info object.
func TestFormatUserLogsStripsQuotaSaturation(t *testing.T) {
	other := common.MapToJsonStr(map[string]interface{}{
		"model_price": 0.004,
		"admin_info": map[string]interface{}{
			"quota_saturation": map[string]interface{}{
				"op":      "QuotaFromDecimal",
				"kind":    "overflow",
				"clamped": common.MaxQuota,
			},
		},
	})
	logs := []*Log{{Other: other}}

	formatUserLogs(logs, 0)

	parsed, err := common.StrToMap(logs[0].Other)
	require.NoError(t, err)
	_, hasAdminInfo := parsed["admin_info"]
	require.False(t, hasAdminInfo, "admin_info (and nested quota_saturation) must be stripped for non-admin views")
	// Non-admin billing fields remain visible.
	require.Contains(t, parsed, "model_price")
}

func TestFormatUserLogsStripsOpenCodeGoAffinityFields(t *testing.T) {
	logs := []*Log{{Other: common.MapToJsonStr(map[string]interface{}{
		"model_price":                 0.004,
		"opencode_go_affinity_source": "top-level-source",
		"opencode_go_workspace_uid":   "top-level-workspace",
		"opencode_go_affinity_key":    "top-level-private-key",
		"admin_info": map[string]interface{}{
			"opencode_go_affinity_source": "claude-code-session",
			"opencode_go_workspace_uid":   "workspace_0123456789abcdef",
			"opencode_go_affinity_key":    "nested-private-key",
			"caller_ip":                   "192.0.2.1",
		},
	})}}

	formatUserLogs(logs, 0)

	parsed, err := common.StrToMap(logs[0].Other)
	require.NoError(t, err)
	require.NotContains(t, parsed, "opencode_go_affinity_source")
	require.NotContains(t, parsed, "opencode_go_workspace_uid")
	require.NotContains(t, parsed, "opencode_go_affinity_key")
	require.NotContains(t, parsed, "admin_info")
	require.Equal(t, 0.004, parsed["model_price"])
}

func TestFormatUserLogsToleratesMissingOrMalformedAffinityData(t *testing.T) {
	logs := []*Log{
		{Other: ""},
		{Other: "not-json"},
		{Other: `{"admin_info":"not-an-object"}`},
		{Other: `{}`},
	}

	require.NotPanics(t, func() {
		formatUserLogs(logs, 0)
	})
	for i, log := range logs {
		require.Equal(t, i+1, log.Id)
	}
}
