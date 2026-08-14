package operation_setting

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/setting/config"
	"github.com/stretchr/testify/require"
)

func TestChannelAffinityEffectiveRulesRestoreBuiltInAfterPersistedRulesReplaceDefaults(t *testing.T) {
	setting := ChannelAffinitySetting{Enabled: true}
	err := config.UpdateConfigFromMap(&setting, map[string]string{
		"rules": `[
			{"name":"legacy custom","model_regex":["^custom-.*$"],"path_regex":["^/v1/responses$"],"key_sources":[{"type":"context_int","key":"token_id"}]},
			{"name":"codex cli trace","model_regex":["^gpt-.*$"],"path_regex":["/v1/responses"],"key_sources":[{"type":"gjson","path":"prompt_cache_key"}]}
		]`,
	})
	require.NoError(t, err)
	require.Len(t, setting.Rules, 2, "persisted rules replace the compiled defaults")

	effective := setting.EffectiveRules()
	require.Len(t, effective, 3)
	require.Equal(t, builtInOpenCodeAPIKeyAffinityRuleName, effective[0].Name)
	require.Equal(t, []ChannelAffinityKeySource{{Type: "opencode_identity"}}, effective[0].KeySources)
	require.Equal(t, []string{"^/v1/(chat/completions|messages|responses)$"}, effective[0].PathRegex)
	require.False(t, effective[0].SkipRetryOnFailure)
	require.True(t, effective[0].IncludeUsingGroup)
	require.True(t, effective[0].IncludeModelName)
	require.True(t, effective[0].IncludeRuleName)
	require.Equal(t, "legacy custom", effective[1].Name)
	require.Equal(t, "codex cli trace", effective[2].Name)
}

func TestChannelAffinityEffectiveRulesIgnorePersistedBuiltInDuplicate(t *testing.T) {
	setting := ChannelAffinitySetting{}
	err := config.UpdateConfigFromMap(&setting, map[string]string{
		"rules": `[
			{"name":" OpenCode API Key Trace ","model_regex":["^unsafe-override$"],"path_regex":[".*"],"key_sources":[{"type":"request_header","key":"X-Raw-Identity"}],"skip_retry_on_failure":true},
			{"name":"operator rule","model_regex":["^operator-.*$"],"path_regex":["^/v1/messages$"],"key_sources":[{"type":"context_string","key":"tenant"}]}
		]`,
	})
	require.NoError(t, err)

	effective := setting.EffectiveRules()
	require.Len(t, effective, 2)
	require.Equal(t, builtInOpenCodeAPIKeyAffinityRuleName, effective[0].Name)
	require.NotContains(t, effective[0].ModelRegex, "^unsafe-override$")
	require.Equal(t, []ChannelAffinityKeySource{{Type: "opencode_identity"}}, effective[0].KeySources)
	require.Equal(t, "operator rule", effective[1].Name)

	builtInCount := 0
	for _, rule := range effective {
		if strings.EqualFold(strings.TrimSpace(rule.Name), builtInOpenCodeAPIKeyAffinityRuleName) {
			builtInCount++
		}
	}
	require.Equal(t, 1, builtInCount)
}

func TestChannelAffinityGlobalLoadKeepsEnabledAndRestoresBuiltInRule(t *testing.T) {
	setting := ChannelAffinitySetting{Enabled: true}
	manager := config.NewConfigManager()
	manager.Register("channel_affinity_setting", &setting)

	err := manager.LoadFromDB(map[string]string{
		"channel_affinity_setting.enabled": "false",
		"channel_affinity_setting.rules": `[
			{"name":"persisted only","model_regex":["^persisted-.*$"],"path_regex":["^/v1/responses$"],"key_sources":[{"type":"gjson","path":"prompt_cache_key"}]}
		]`,
	})
	require.NoError(t, err)
	require.False(t, setting.Enabled)
	require.Len(t, setting.Rules, 1)

	effective := setting.EffectiveRules()
	require.Len(t, effective, 2)
	require.Equal(t, builtInOpenCodeAPIKeyAffinityRuleName, effective[0].Name)
	require.Equal(t, "persisted only", effective[1].Name)
}
