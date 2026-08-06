package opencodego

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
)

const (
	cacheIdentityHeader      = "x-opencode-session"
	claudeCodeSessionHeader  = "x-claude-code-session-id"
	cacheIdentityMaxLength   = 64
	cacheIdentityPrefix      = "ocg_"
	cacheIdentityDomain      = "new-api/opencode-go/cache-identity/v1"
	claudeMetadataSessionKey = "session_id"
)

func cacheIdentityForRequest(c *gin.Context, info *relaycommon.RelayInfo, request any) string {
	if identity := affinityIdentityForRequest(c, info, request); identity != "" {
		return identity
	}
	if source, value := requestCacheIdentity(request); value != "" {
		if source == "messages" {
			return hashCacheIdentity(source, value)
		}
		return canonicalCacheIdentity(source, value)
	}

	userID, tokenID, model := 0, 0, ""
	if info != nil {
		userID = info.UserId
		tokenID = info.TokenId
		model = info.OriginModelName
	}
	seed := strconv.Itoa(userID) + "\x00" + strconv.Itoa(tokenID) + "\x00" + model
	return hashCacheIdentity("fallback", seed)
}

// affinityIdentityForRequest resolves the workspace-affinity key for a
// request. Session markers keep priority; when none is present and the channel
// opted into token fallback, the caller token identity is used so stateless
// traffic (for example load tests) stays on a stable workspace instead of
// round-robining and losing its cache on every request.
func affinityIdentityForRequest(c *gin.Context, info *relaycommon.RelayInfo, request any) string {
	if c != nil && c.Request != nil {
		if value := strings.TrimSpace(c.Request.Header.Get(claudeCodeSessionHeader)); value != "" {
			return hashCacheIdentity("claude-code-session", value)
		}
	}
	if sessionID := requestClaudeSessionID(request); sessionID != "" {
		return hashCacheIdentity("claude-metadata-session", sessionID)
	}
	if c != nil && c.Request != nil {
		if value := c.Request.Header.Get(cacheIdentityHeader); value != "" {
			return canonicalCacheIdentity("header", value)
		}
	}
	if source, value := requestCacheIdentity(request); value != "" && source != "messages" {
		return canonicalCacheIdentity(source, value)
	}
	if info != nil && info.ChannelMeta != nil && info.ChannelOtherSettings.OpenCodeGo != nil &&
		info.ChannelOtherSettings.OpenCodeGo.AffinityFallback == "token" && info.TokenId > 0 {
		return hashCacheIdentity("token-fallback", strconv.Itoa(info.TokenId))
	}
	return ""
}

func requestClaudeSessionID(request any) string {
	var metadata json.RawMessage
	switch typed := request.(type) {
	case *dto.ClaudeRequest:
		if typed != nil {
			metadata = typed.Metadata
		}
	case dto.ClaudeRequest:
		metadata = typed.Metadata
	}
	return claudeMetadataSessionID(metadata)
}

func claudeMetadataSessionID(raw json.RawMessage) string {
	userID := claudeMetadataUserID(raw)
	if !strings.HasPrefix(strings.TrimSpace(userID), "{") {
		return ""
	}
	var metadata map[string]any
	if err := common.Unmarshal([]byte(userID), &metadata); err != nil {
		return ""
	}
	sessionID, _ := metadata[claudeMetadataSessionKey].(string)
	return strings.TrimSpace(sessionID)
}

func requestCacheIdentity(request any) (string, string) {
	switch typed := request.(type) {
	case *dto.OpenAIResponsesRequest:
		return "responses", rawJSONString(typed.PromptCacheKey)
	case dto.OpenAIResponsesRequest:
		return "responses", rawJSONString(typed.PromptCacheKey)
	case *dto.GeneralOpenAIRequest:
		return "chat", typed.PromptCacheKey
	case dto.GeneralOpenAIRequest:
		return "chat", typed.PromptCacheKey
	case *dto.ClaudeRequest:
		return "messages", claudeMetadataUserID(typed.Metadata)
	case dto.ClaudeRequest:
		return "messages", claudeMetadataUserID(typed.Metadata)
	default:
		return "", ""
	}
}

func claudeMetadataUserID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var metadata dto.ClaudeMetadata
	if err := common.Unmarshal(raw, &metadata); err != nil {
		return ""
	}
	return metadata.UserId
}

func rawJSONString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if err := common.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

func canonicalCacheIdentity(source string, value string) string {
	if isValidCacheIdentity(value) {
		return value
	}
	return hashCacheIdentity(source, value)
}

func isValidCacheIdentity(value string) bool {
	if value == "" || len(value) > cacheIdentityMaxLength || value != strings.TrimSpace(value) {
		return false
	}
	for i := 0; i < len(value); i++ {
		b := value[i]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') || b == '-' || b == '_' || b == '.' || b == ':' {
			continue
		}
		return false
	}
	return true
}

func hashCacheIdentity(source string, value string) string {
	h := hmac.New(sha256.New, []byte(common.CryptoSecret))
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s", cacheIdentityDomain, source, value)
	digest := h.Sum(nil)
	return cacheIdentityPrefix + base64.RawURLEncoding.EncodeToString(digest[:16])
}
