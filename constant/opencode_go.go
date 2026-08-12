package constant

import "strings"

const (
	OpenCodeGoPublicOverloadMessage    = "当前分组上游负载已饱和，请稍后再试"
	OpenCodeGoPublicRateLimitErrorCode = "rate_limit_error"

	OpenCodeGoAffinitySourceToken                 = "token"
	OpenCodeGoAffinitySourceClaudeCodeSession     = "claude-code-session"
	OpenCodeGoAffinitySourceClaudeMetadataSession = "claude-metadata-session"
	OpenCodeGoAffinitySourceOpenCodeSession       = "opencode-session"
	OpenCodeGoAffinitySourcePromptCacheKey        = "prompt_cache_key"
	OpenCodeGoAffinitySourceNone                  = "none"
)

var openCodeGoDistinctPrivateErrorMarkers = []string{
	"opencode",
	"open_code",
	"console go",
	"console_go",
	"console-go",
	"consolego",
	"workspace",
	"wrk_",
	"endpoint",
}

func OpenCodeGoStringHasDistinctPrivateErrorMarker(value string) bool {
	normalized := strings.ToLower(value)
	for _, marker := range openCodeGoDistinctPrivateErrorMarkers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	collapsed := strings.NewReplacer(" ", "", "_", "", "-", "").Replace(normalized)
	for _, marker := range []string{"opencode", "consolego", "workspace"} {
		if strings.Contains(collapsed, marker) {
			return true
		}
	}
	return false
}

func OpenCodeGoStringHasPrivateErrorMarker(value string) bool {
	return OpenCodeGoStringHasDistinctPrivateErrorMarker(value) || strings.Contains(strings.ToLower(value), "channel")
}
