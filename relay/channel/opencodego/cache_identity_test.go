package opencodego

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newCacheIdentityContext(headerValue string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if headerValue != "" {
		c.Request.Header.Set(cacheIdentityHeader, headerValue)
	}
	return c
}

func TestCacheIdentityPreservesExplicitShortValues(t *testing.T) {
	info := &relaycommon.RelayInfo{OriginModelName: "public-model", UserId: 17, TokenId: 29}
	c := newCacheIdentityContext("session.explicit:1")
	request := &dto.OpenAIResponsesRequest{PromptCacheKey: json.RawMessage(`"body-key"`)}

	assert.Equal(t, "session.explicit:1", cacheIdentityForRequest(c, info, request))

	c = newCacheIdentityContext("")
	assert.Equal(t, "body-key", cacheIdentityForRequest(c, info, request))
}

func TestCacheIdentityHashesLongAndUserDerivedValues(t *testing.T) {
	infoA := &relaycommon.RelayInfo{
		OriginModelName: "public-model",
		UserId:          17,
		TokenId:         29,
		ChannelMeta:     &relaycommon.ChannelMeta{ApiKey: "account-a"},
	}
	infoB := &relaycommon.RelayInfo{
		OriginModelName: "public-model",
		UserId:          17,
		TokenId:         29,
		ChannelMeta:     &relaycommon.ChannelMeta{ApiKey: "account-b"},
	}
	longValue := strings.Repeat("long-session-", 12)
	identity := cacheIdentityForRequest(newCacheIdentityContext(longValue), infoA, nil)

	assert.True(t, strings.HasPrefix(identity, cacheIdentityPrefix))
	assert.LessOrEqual(t, len(identity), cacheIdentityMaxLength)
	assert.NotContains(t, identity, "long-session")
	assert.Equal(t, identity, cacheIdentityForRequest(newCacheIdentityContext(longValue), infoB, nil))

	metadata, err := json.Marshal(dto.ClaudeMetadata{UserId: "customer-plain-id"})
	require.NoError(t, err)
	claudeIdentity := cacheIdentityForRequest(newCacheIdentityContext(""), infoA, &dto.ClaudeRequest{Metadata: metadata})
	assert.True(t, strings.HasPrefix(claudeIdentity, cacheIdentityPrefix))
	assert.NotContains(t, claudeIdentity, "customer-plain-id")

	fallbackA := cacheIdentityForRequest(newCacheIdentityContext(""), infoA, nil)
	fallbackB := cacheIdentityForRequest(newCacheIdentityContext(""), infoB, nil)
	assert.Equal(t, fallbackA, fallbackB)
	assert.NotContains(t, fallbackA, "17")
	assert.NotContains(t, fallbackA, "29")
}

func TestClaudeCodeSessionIdentityDrivesCacheAndAffinity(t *testing.T) {
	info := &relaycommon.RelayInfo{OriginModelName: "public-model", UserId: 17, TokenId: 29}
	c := newCacheIdentityContext("explicit-cache-key")
	c.Request.Header.Set(claudeCodeSessionHeader, "claude-session-raw")
	metadata, err := json.Marshal(dto.ClaudeMetadata{UserId: `{"account_uuid":"account","session_id":"metadata-session"}`})
	require.NoError(t, err)
	request := &dto.ClaudeRequest{Metadata: metadata}

	identity, source := affinityIdentityForRequest(c, info, request)
	assert.Equal(t, identity, cacheIdentityForRequest(c, info, request))
	assert.True(t, strings.HasPrefix(identity, cacheIdentityPrefix))
	assert.NotContains(t, identity, "claude-session-raw")
	assert.Equal(t, AffinitySourceClaudeCodeSession, source)

	c.Request.Header.Del(claudeCodeSessionHeader)
	metadataIdentity, metadataSource := affinityIdentityForRequest(c, info, request)
	assert.NotEmpty(t, metadataIdentity)
	assert.NotEqual(t, identity, metadataIdentity)
	assert.NotContains(t, metadataIdentity, "metadata-session")
	assert.Equal(t, AffinitySourceClaudeMetadata, metadataSource)
}

func TestAffinityIdentityIsEmptyWithoutSessionMarker(t *testing.T) {
	metadata, err := json.Marshal(dto.ClaudeMetadata{UserId: "plain-customer-id"})
	require.NoError(t, err)
	request := &dto.ClaudeRequest{Metadata: metadata}

	identity, source := affinityIdentityForRequest(newCacheIdentityContext(""), nil, request)
	assert.Empty(t, identity)
	assert.Empty(t, source)
	assert.NotEmpty(t, cacheIdentityForRequest(newCacheIdentityContext(""), nil, request))
}

func TestAffinityIdentityTokenFallbackIsOptInAndStable(t *testing.T) {
	metadata, err := json.Marshal(dto.ClaudeMetadata{UserId: "plain-customer-id"})
	require.NoError(t, err)
	request := &dto.ClaudeRequest{Metadata: metadata}

	noFallback := &relaycommon.RelayInfo{OriginModelName: "model", UserId: 7, TokenId: 42}
	identity, source := affinityIdentityForRequest(newCacheIdentityContext(""), noFallback, request)
	assert.Empty(t, identity)
	assert.Empty(t, source)

	fallback := &relaycommon.RelayInfo{
		OriginModelName: "model",
		UserId:          7,
		TokenId:         42,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelOtherSettings: dto.ChannelOtherSettings{
				OpenCodeGo: &dto.OpenCodeGoConfig{AffinityFallback: "token"},
			},
		},
	}
	tokenIdentity, tokenSource := affinityIdentityForRequest(newCacheIdentityContext(""), fallback, request)
	require.NotEmpty(t, tokenIdentity)
	assert.True(t, strings.HasPrefix(tokenIdentity, cacheIdentityPrefix))
	assert.NotContains(t, tokenIdentity, "42")
	assert.Equal(t, AffinitySourceToken, tokenSource)
	// Stable for the same token, distinct from a different token.
	otherToken := &relaycommon.RelayInfo{
		OriginModelName: "model",
		UserId:          7,
		TokenId:         43,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelOtherSettings: dto.ChannelOtherSettings{
				OpenCodeGo: &dto.OpenCodeGoConfig{AffinityFallback: "token"},
			},
		},
	}
	sameTokenIdentity, _ := affinityIdentityForRequest(newCacheIdentityContext(""), fallback, request)
	otherTokenIdentity, _ := affinityIdentityForRequest(newCacheIdentityContext(""), otherToken, request)
	assert.Equal(t, tokenIdentity, sameTokenIdentity)
	assert.NotEqual(t, tokenIdentity, otherTokenIdentity)
}
