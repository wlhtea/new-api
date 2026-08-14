package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newOpenCodeAffinityContext(tokenID int) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	common.SetContextKey(c, constant.ContextKeyTokenId, tokenID)
	return c
}

func TestResolveOpenCodeAffinityIdentityPriorityAndPrivacy(t *testing.T) {
	previousSecret := common.CryptoSecret
	common.CryptoSecret = "test-open-code-affinity-secret"
	t.Cleanup(func() { common.CryptoSecret = previousSecret })

	metadata, err := json.Marshal(dto.ClaudeMetadata{
		UserId: `{"session_id":"metadata-private","account_uuid":"account-private"}`,
	})
	require.NoError(t, err)
	request := &dto.ClaudeRequest{Metadata: metadata}
	c := newOpenCodeAffinityContext(7001)
	c.Request.Header.Set(openCodeSessionHeader, "opencode-private")
	c.Request.Header.Set(claudeCodeSessionHeader, "claude-private")

	identity := ResolveOpenCodeAffinityIdentity(c, request, true)
	require.Equal(t, constant.OpenCodeGoAffinitySourceClaudeCodeSession, identity.Source)
	require.True(t, strings.HasPrefix(identity.Value, "ocg_"))
	require.Len(t, identity.Value, 26)
	require.NotContains(t, identity.Value, "claude-private")

	c.Request.Header.Del(claudeCodeSessionHeader)
	identity = ResolveOpenCodeAffinityIdentity(c, request, true)
	require.Equal(t, constant.OpenCodeGoAffinitySourceClaudeMetadataSession, identity.Source)
	require.NotContains(t, identity.Value, "metadata-private")

	request.Metadata = nil
	identity = ResolveOpenCodeAffinityIdentity(c, request, true)
	require.Equal(t, constant.OpenCodeGoAffinitySourceOpenCodeSession, identity.Source)
	require.NotContains(t, identity.Value, "opencode-private")

	c.Request.Header.Del(openCodeSessionHeader)
	chat := &dto.GeneralOpenAIRequest{PromptCacheKey: "prompt-private"}
	identity = ResolveOpenCodeAffinityIdentity(c, chat, true)
	require.Equal(t, constant.OpenCodeGoAffinitySourcePromptCacheKey, identity.Source)
	require.NotContains(t, identity.Value, "prompt-private")

	identity = ResolveOpenCodeAffinityIdentity(c, nil, true)
	require.Equal(t, constant.OpenCodeGoAffinitySourceToken, identity.Source)
	require.NotContains(t, identity.Value, "7001")
}

func TestResolveOpenCodeAffinityIdentityTokenFallbackAndContext(t *testing.T) {
	previousSecret := common.CryptoSecret
	common.CryptoSecret = "test-open-code-affinity-secret"
	t.Cleanup(func() { common.CryptoSecret = previousSecret })

	c := newOpenCodeAffinityContext(42)
	require.Empty(t, ResolveOpenCodeAffinityIdentity(c, nil, false))

	first := PrepareOpenCodeAffinityIdentity(c, nil)
	require.Equal(t, constant.OpenCodeGoAffinitySourceToken, first.Source)
	require.NotContains(t, first.Value, "42")
	cached, ok := GetOpenCodeAffinityIdentity(c)
	require.True(t, ok)
	require.Equal(t, first, cached)

	require.Empty(t, ResolveOpenCodeAffinityIdentity(c, nil, false))
	require.Equal(t, first, ResolveOpenCodeAffinityIdentity(c, nil, true))
	require.Equal(t, first, ResolveOpenCodeAffinityIdentity(newOpenCodeAffinityContext(42), nil, true))
	require.NotEqual(t, first.Value, ResolveOpenCodeAffinityIdentity(newOpenCodeAffinityContext(43), nil, true).Value)
}

func TestResolveOpenCodeAffinityIdentityPromptKeyIsStableAcrossFormats(t *testing.T) {
	previousSecret := common.CryptoSecret
	common.CryptoSecret = "test-open-code-affinity-secret"
	t.Cleanup(func() { common.CryptoSecret = previousSecret })

	c := newOpenCodeAffinityContext(0)
	chat := ResolveOpenCodeAffinityIdentity(c, &dto.GeneralOpenAIRequest{PromptCacheKey: "same-key"}, true)
	responses := ResolveOpenCodeAffinityIdentity(c, &dto.OpenAIResponsesRequest{PromptCacheKey: json.RawMessage(`"same-key"`)}, true)

	require.NotEmpty(t, chat.Value)
	require.Equal(t, chat, responses)

	malformed := ResolveOpenCodeAffinityIdentity(c, &dto.OpenAIResponsesRequest{PromptCacheKey: json.RawMessage(`{"not":"a string"}`)}, true)
	require.Empty(t, malformed)
}

func TestResolveOpenCodeAffinityIdentityIgnoresMalformedClaudeMetadata(t *testing.T) {
	c := newOpenCodeAffinityContext(0)
	for _, userID := range []string{
		"plain-user",
		`{"session_id":42}`,
		`{"session_id":"   "}`,
		`{"session_id":`,
	} {
		metadata, err := json.Marshal(dto.ClaudeMetadata{UserId: userID})
		require.NoError(t, err)
		identity := ResolveOpenCodeAffinityIdentity(c, &dto.ClaudeRequest{Metadata: metadata}, true)
		require.Empty(t, identity)
	}
}
