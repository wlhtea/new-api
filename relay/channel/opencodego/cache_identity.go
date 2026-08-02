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
	cacheIdentityHeader    = "x-opencode-session"
	cacheIdentityMaxLength = 64
	cacheIdentityPrefix    = "ocg_"
	cacheIdentityDomain    = "new-api/opencode-go/cache-identity/v1"
)

func cacheIdentityForRequest(c *gin.Context, info *relaycommon.RelayInfo, request any) string {
	if c != nil && c.Request != nil {
		if value := c.Request.Header.Get(cacheIdentityHeader); value != "" {
			return canonicalCacheIdentity("header", value)
		}
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
