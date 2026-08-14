package service

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
)

const (
	OpenCodeAffinityIdentityContextKey = "opencode_affinity_identity"
	OpenCodeAffinitySourceContextKey   = "opencode_affinity_source"

	openCodeSessionHeader       = "x-opencode-session"
	claudeCodeSessionHeader     = "x-claude-code-session-id"
	claudeMetadataSessionIDKey  = "session_id"
	openCodeAffinityTokenSource = "token-fallback"
)

type OpenCodeAffinityIdentity struct {
	Value  string
	Source string
}

// PrepareOpenCodeAffinityIdentity resolves and caches the privacy-safe identity
// before channel distribution. The API-key channel enables token fallback;
// pool channels can still opt out when they resolve the identity themselves.
func PrepareOpenCodeAffinityIdentity(c *gin.Context, request dto.Request) OpenCodeAffinityIdentity {
	identity := ResolveOpenCodeAffinityIdentity(c, request, true)
	setOpenCodeAffinityIdentity(c, identity)
	return identity
}

func ResolveOpenCodeAffinityIdentity(c *gin.Context, request any, allowTokenFallback bool) OpenCodeAffinityIdentity {
	return ResolveOpenCodeAffinityIdentityWithTokenID(c, request, allowTokenFallback, 0)
}

// ResolveOpenCodeAffinityIdentityWithTokenID accepts an optional trusted token
// identity for relay paths which already materialized RelayInfo. The regular
// pre-distribution route leaves it as zero and reads token_id from Gin context.
func ResolveOpenCodeAffinityIdentityWithTokenID(c *gin.Context, request any, allowTokenFallback bool, tokenID int) OpenCodeAffinityIdentity {
	if cached, ok := GetOpenCodeAffinityIdentity(c); ok &&
		(cached.Source != constant.OpenCodeGoAffinitySourceToken || allowTokenFallback) {
		return cached
	}

	if c != nil && c.Request != nil {
		if value := strings.TrimSpace(c.Request.Header.Get(claudeCodeSessionHeader)); value != "" {
			return newOpenCodeAffinityIdentity(
				"claude-code-session",
				value,
				constant.OpenCodeGoAffinitySourceClaudeCodeSession,
			)
		}
	}
	if sessionID := openCodeClaudeMetadataSessionID(request); sessionID != "" {
		return newOpenCodeAffinityIdentity(
			"claude-metadata-session",
			sessionID,
			constant.OpenCodeGoAffinitySourceClaudeMetadataSession,
		)
	}
	if c != nil && c.Request != nil {
		if value := strings.TrimSpace(c.Request.Header.Get(openCodeSessionHeader)); value != "" {
			return newOpenCodeAffinityIdentity(
				"opencode-session",
				value,
				constant.OpenCodeGoAffinitySourceOpenCodeSession,
			)
		}
	}
	if value := openCodePromptCacheKey(request); value != "" {
		return newOpenCodeAffinityIdentity(
			"prompt-cache-key",
			value,
			constant.OpenCodeGoAffinitySourcePromptCacheKey,
		)
	}
	if allowTokenFallback {
		if tokenID <= 0 && c != nil {
			tokenID = common.GetContextKeyInt(c, constant.ContextKeyTokenId)
		}
		if tokenID > 0 {
			return newOpenCodeAffinityIdentity(
				openCodeAffinityTokenSource,
				strconv.Itoa(tokenID),
				constant.OpenCodeGoAffinitySourceToken,
			)
		}
	}
	return OpenCodeAffinityIdentity{}
}

func GetOpenCodeAffinityIdentity(c *gin.Context) (OpenCodeAffinityIdentity, bool) {
	if c == nil {
		return OpenCodeAffinityIdentity{}, false
	}
	value := strings.TrimSpace(c.GetString(OpenCodeAffinityIdentityContextKey))
	source := strings.TrimSpace(c.GetString(OpenCodeAffinitySourceContextKey))
	if value == "" || source == "" {
		return OpenCodeAffinityIdentity{}, false
	}
	return OpenCodeAffinityIdentity{Value: value, Source: source}, true
}

func setOpenCodeAffinityIdentity(c *gin.Context, identity OpenCodeAffinityIdentity) {
	if c == nil {
		return
	}
	c.Set(OpenCodeAffinityIdentityContextKey, identity.Value)
	c.Set(OpenCodeAffinitySourceContextKey, identity.Source)
}

func newOpenCodeAffinityIdentity(domain, rawValue, source string) OpenCodeAffinityIdentity {
	return OpenCodeAffinityIdentity{
		Value:  common.OpenCodeGoDiagnosticRef(domain, rawValue),
		Source: source,
	}
}

func openCodeClaudeMetadataSessionID(request any) string {
	var raw json.RawMessage
	switch typed := request.(type) {
	case *dto.ClaudeRequest:
		if typed != nil {
			raw = typed.Metadata
		}
	case dto.ClaudeRequest:
		raw = typed.Metadata
	}
	if len(raw) == 0 {
		return ""
	}

	var metadata dto.ClaudeMetadata
	if err := common.Unmarshal(raw, &metadata); err != nil {
		return ""
	}
	userID := strings.TrimSpace(metadata.UserId)
	if !strings.HasPrefix(userID, "{") {
		return ""
	}
	var nested map[string]any
	if err := common.Unmarshal([]byte(userID), &nested); err != nil {
		return ""
	}
	sessionID, _ := nested[claudeMetadataSessionIDKey].(string)
	return strings.TrimSpace(sessionID)
}

func openCodePromptCacheKey(request any) string {
	switch typed := request.(type) {
	case *dto.GeneralOpenAIRequest:
		if typed != nil {
			return strings.TrimSpace(typed.PromptCacheKey)
		}
	case dto.GeneralOpenAIRequest:
		return strings.TrimSpace(typed.PromptCacheKey)
	case *dto.OpenAIResponsesRequest:
		if typed != nil {
			return openCodeRawJSONString(typed.PromptCacheKey)
		}
	case dto.OpenAIResponsesRequest:
		return openCodeRawJSONString(typed.PromptCacheKey)
	}
	return ""
}

func openCodeRawJSONString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if err := common.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}
