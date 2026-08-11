package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const responsesDetailedUsageJSON = `{"input_tokens":210,"output_tokens":40,"total_tokens":250,"input_tokens_details":{"cached_tokens":80,"cached_creation_tokens":30,"cache_write_tokens":20,"text_tokens":71,"audio_tokens":7,"image_tokens":2},"output_tokens_details":{"text_tokens":20,"audio_tokens":3,"image_tokens":2,"reasoning_tokens":15}}`

func assertDetailedResponsesUsage(t *testing.T, usage *dto.Usage) {
	t.Helper()
	require.NotNil(t, usage)
	assert.Equal(t, 210, usage.PromptTokens)
	assert.Equal(t, 40, usage.CompletionTokens)
	assert.Equal(t, 250, usage.TotalTokens)
	assert.Equal(t, 210, usage.InputTokens)
	assert.Equal(t, 40, usage.OutputTokens)
	wantInputDetails := dto.InputTokenDetails{
		CachedTokens:         80,
		CachedCreationTokens: 30,
		CacheWriteTokens:     20,
		TextTokens:           71,
		AudioTokens:          7,
		ImageTokens:          2,
	}
	assert.Equal(t, wantInputDetails, usage.PromptTokensDetails)
	require.NotNil(t, usage.InputTokensDetails)
	assert.Equal(t, wantInputDetails, *usage.InputTokensDetails)
	assert.Equal(t, dto.OutputTokenDetails{
		TextTokens:      20,
		AudioTokens:     3,
		ImageTokens:     2,
		ReasoningTokens: 15,
	}, usage.CompletionTokenDetails)
}

func TestCopyResponsesUsageDeepCopiesAndClearsInputDetails(t *testing.T) {
	details := &dto.InputTokenDetails{CachedTokens: 80, TextTokens: 71}
	src := &dto.Usage{
		PromptTokens:                    999,
		CompletionTokens:                998,
		TotalTokens:                     250,
		PromptCacheHitTokens:            80,
		UsageSemantic:                   dto.BillingUsageSemanticAnthropic,
		UsageSource:                     dto.BillingUsageSourceClaudeMessages,
		BillingUsage:                    dto.NewClaudeMessagesBillingUsage(&dto.ClaudeUsage{InputTokens: 100}),
		PromptTokensDetails:             dto.InputTokenDetails{CachedTokens: 999},
		CompletionTokenDetails:          dto.OutputTokenDetails{ReasoningTokens: 15},
		InputTokens:                     210,
		OutputTokens:                    40,
		InputTokensDetails:              details,
		ClaudeCacheCreation5mTokens:     20,
		ClaudeCacheCreation1hTokens:     10,
		Cost:                            1.25,
	}
	dst := &dto.Usage{
		InputTokensDetails:  &dto.InputTokenDetails{CachedTokens: 999},
		PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 999},
	}

	copyResponsesUsage(dst, src)
	assert.Equal(t, 210, dst.PromptTokens)
	assert.Equal(t, 40, dst.CompletionTokens)
	assert.Equal(t, 250, dst.TotalTokens)
	assert.Equal(t, 80, dst.PromptCacheHitTokens)
	assert.Equal(t, dto.BillingUsageSemanticAnthropic, dst.UsageSemantic)
	assert.Equal(t, dto.BillingUsageSourceClaudeMessages, dst.UsageSource)
	assert.Equal(t, 20, dst.ClaudeCacheCreation5mTokens)
	assert.Equal(t, 10, dst.ClaudeCacheCreation1hTokens)
	assert.Equal(t, 1.25, dst.Cost)
	assert.Equal(t, dto.OutputTokenDetails{ReasoningTokens: 15}, dst.CompletionTokenDetails)
	require.NotNil(t, dst.InputTokensDetails)
	assert.NotSame(t, src.InputTokensDetails, dst.InputTokensDetails)
	assert.Equal(t, *src.InputTokensDetails, *dst.InputTokensDetails)
	require.NotNil(t, dst.BillingUsage)
	require.NotNil(t, dst.BillingUsage.ClaudeUsage)
	assert.NotSame(t, src.BillingUsage, dst.BillingUsage)
	assert.NotSame(t, src.BillingUsage.ClaudeUsage, dst.BillingUsage.ClaudeUsage)
	src.InputTokensDetails.CachedTokens = 123
	src.BillingUsage.ClaudeUsage.InputTokens = 321
	assert.Equal(t, 80, dst.InputTokensDetails.CachedTokens)
	assert.Equal(t, 100, dst.BillingUsage.ClaudeUsage.InputTokens)

	copyResponsesUsage(dst, &dto.Usage{})
	assert.Nil(t, dst.InputTokensDetails)
	assert.Equal(t, dto.InputTokenDetails{}, dst.PromptTokensDetails)
}

func newResponsesTestContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(common.RequestIdKey, "responses-usage-details-test")
	return c, recorder
}

func TestOaiResponsesHandlerPreservesAllUsageDetails(t *testing.T) {
	body := `{"id":"resp_usage","status":"completed","output":[],"usage":` + responsesDetailedUsageJSON + `}`
	c, recorder := newResponsesTestContext(t)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	usage, apiErr := OaiResponsesHandler(c, &relaycommon.RelayInfo{}, resp)
	require.Nil(t, apiErr)
	assertDetailedResponsesUsage(t, usage)
	assert.JSONEq(t, body, recorder.Body.String())
}

func TestOaiResponsesStreamHandlerPreservesAllTerminalUsageDetails(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := "data: " + `{"type":"response.completed","response":{"id":"resp_usage","status":"completed","output":[],"usage":` + responsesDetailedUsageJSON + `}}` + "\n\n" +
		"data: [DONE]\n\n"
	c, recorder := newResponsesTestContext(t)
	info := &relaycommon.RelayInfo{
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-5.1"},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	assertDetailedResponsesUsage(t, usage)
	assert.Contains(t, recorder.Body.String(), `"output_tokens_details":{"text_tokens":20,"audio_tokens":3,"image_tokens":2,"reasoning_tokens":15}`)
}

func TestOaiResponsesCompactionHandlerPreservesAllUsageDetails(t *testing.T) {
	body := `{"id":"resp_compact","object":"response.compaction","output":[],"usage":` + responsesDetailedUsageJSON + `}`
	c, recorder := newResponsesTestContext(t)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	usage, apiErr := OaiResponsesCompactionHandler(c, resp)
	require.Nil(t, apiErr)
	assertDetailedResponsesUsage(t, usage)
	assert.JSONEq(t, body, recorder.Body.String())
}
