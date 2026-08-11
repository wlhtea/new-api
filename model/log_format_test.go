package model

import (
	"strings"
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

func TestFormatUserLogsProjectsSafeOpenCodeGoAffinityFields(t *testing.T) {
	allowedSources := []string{
		"token",
		"claude-code-session",
		"claude-metadata-session",
		"opencode-session",
		"prompt_cache_key",
		"none",
	}

	for _, source := range allowedSources {
		t.Run(source, func(t *testing.T) {
			logs := []*Log{{Other: common.MapToJsonStr(map[string]interface{}{
				"model_price":                 0.004,
				"opencode_go_affinity_source": "untrusted-top-level-source",
				"opencode_go_workspace_uid":   "untrusted-top-level-workspace",
				"admin_info": map[string]interface{}{
					"opencode_go_affinity_source": source,
					"opencode_go_workspace_uid":   "workspace_0123456789abcdef",
					"opencode_go_affinity_key":    "private-affinity-key",
					"use_channel":                 []int{1, 2},
				},
			})}}

			formatUserLogs(logs, 0)

			parsed, err := common.StrToMap(logs[0].Other)
			require.NoError(t, err)
			require.Equal(t, source, parsed["opencode_go_affinity_source"])
			require.Equal(t, "workspace_0123456789abcdef", parsed["opencode_go_workspace_uid"])
			require.NotContains(t, parsed, "admin_info")
			require.NotContains(t, parsed, "opencode_go_affinity_key")
			require.Contains(t, parsed, "model_price")
		})
	}
}

func TestFormatUserLogsOmitsUnsafeOpenCodeGoAffinityValues(t *testing.T) {
	logs := []*Log{{Other: common.MapToJsonStr(map[string]interface{}{
		"opencode_go_affinity_source": "untrusted-top-level-source",
		"opencode_go_workspace_uid":   "untrusted-top-level-workspace",
		"admin_info": map[string]interface{}{
			"opencode_go_affinity_source": "future-private-source",
			"opencode_go_workspace_uid":   "workspace_safe_uid",
			"opencode_go_affinity_key":    "private-affinity-key",
			"caller_ip":                   "192.0.2.1",
		},
	})}}

	formatUserLogs(logs, 0)

	parsed, err := common.StrToMap(logs[0].Other)
	require.NoError(t, err)
	require.NotContains(t, parsed, "opencode_go_affinity_source")
	require.Equal(t, "workspace_safe_uid", parsed["opencode_go_workspace_uid"])
	require.NotContains(t, parsed, "admin_info")
	require.NotContains(t, parsed, "opencode_go_affinity_key")
	require.NotContains(t, parsed, "caller_ip")
}

func TestFormatUserLogsRejectsInvalidOpenCodeGoWorkspaceUIDs(t *testing.T) {
	testCases := map[string]interface{}{
		"empty":      "",
		"blank":      "   ",
		"oversized":  strings.Repeat("w", 65),
		"wrong type": 42,
		"nil":        nil,
	}

	for name, workspaceUID := range testCases {
		t.Run(name, func(t *testing.T) {
			logs := []*Log{{Other: common.MapToJsonStr(map[string]interface{}{
				"admin_info": map[string]interface{}{
					"opencode_go_affinity_source": "token",
					"opencode_go_workspace_uid":   workspaceUID,
				},
			})}}

			formatUserLogs(logs, 0)

			parsed, err := common.StrToMap(logs[0].Other)
			require.NoError(t, err)
			require.Equal(t, "token", parsed["opencode_go_affinity_source"])
			require.NotContains(t, parsed, "opencode_go_workspace_uid")
			require.NotContains(t, parsed, "admin_info")
		})
	}
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
