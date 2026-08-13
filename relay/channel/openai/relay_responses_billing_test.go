package openai

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOaiResponsesHandlerCountsOutputCallsNotDeclarations(t *testing.T) {
	gin.SetMode(gin.TestMode)
	operation_setting.SetToolPriceForTest("priced_fn", 5.0)
	t.Cleanup(func() {
		operation_setting.DeleteToolPriceForTest("priced_fn")
	})

	body, err := common.Marshal(dto.OpenAIResponsesResponse{
		Tools: []map[string]any{
			{"type": "web_search_preview"},
			{"type": "file_search"},
		},
		Output: []dto.ResponsesOutput{
			{Type: dto.BuildInCallWebSearchCall},
			{Type: dto.BuildInCallWebSearchCall},
			{Type: dto.BuildInCallFunctionCall, Name: "priced_fn"},
			{Type: dto.BuildInCallFunctionCall, Name: "unpriced_fn"},
		},
		Usage: &dto.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	info := &relaycommon.RelayInfo{
		OriginModelName: "gpt-5.1",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
				dto.BuildInToolWebSearchPreview: {ToolName: dto.BuildInToolWebSearchPreview, CallCount: 0},
				dto.BuildInToolFileSearch:       {ToolName: dto.BuildInToolFileSearch, CallCount: 0},
			},
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	usage, apiErr := OaiResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 2, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearchPreview].CallCount)
	assert.Equal(t, 0, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolFileSearch].CallCount)
	require.Contains(t, info.ResponsesUsageInfo.BuiltInTools, "priced_fn")
	assert.Equal(t, 1, info.ResponsesUsageInfo.BuiltInTools["priced_fn"].CallCount)
	assert.NotContains(t, info.ResponsesUsageInfo.BuiltInTools, "unpriced_fn")
}

func TestOaiResponsesHandlerDeclaredToolsWithoutOutputCountZero(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body, err := common.Marshal(dto.OpenAIResponsesResponse{
		Tools: []map[string]any{
			{"type": "web_search_preview"},
			{"type": "file_search"},
		},
		Output: []dto.ResponsesOutput{
			{Type: "message", Role: "assistant"},
		},
		Usage: &dto.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	info := &relaycommon.RelayInfo{
		OriginModelName: "gpt-5.1",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
				dto.BuildInToolWebSearchPreview: {ToolName: dto.BuildInToolWebSearchPreview, CallCount: 0},
				dto.BuildInToolFileSearch:       {ToolName: dto.BuildInToolFileSearch, CallCount: 0},
			},
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	_, apiErr := OaiResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	assert.Equal(t, 0, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearchPreview].CallCount)
	assert.Equal(t, 0, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolFileSearch].CallCount)
}

func TestOaiResponsesHandlerCountsCompletedImageGenerationOutputs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body, err := common.Marshal(dto.OpenAIResponsesResponse{
		Status: []byte(`"completed"`),
		Output: []dto.ResponsesOutput{
			{
				Type:   dto.ResponsesOutputTypeImageGenerationCall,
				ID:     "img_1",
				Status: "completed",
				Result: "base64-a",
			},
			{
				Type:   dto.ResponsesOutputTypeImageGenerationCall,
				ID:     "img_2",
				Status: "completed",
				Result: "base64-b",
			},
			{
				Type:   dto.ResponsesOutputTypeImageGenerationCall,
				ID:     "img_empty",
				Status: "completed",
				Result: "",
			},
		},
		Usage: &dto.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{OriginModelName: "gpt-5.1"}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	_, apiErr := OaiResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.Contains(t, info.ResponsesUsageInfo.BuiltInTools, dto.BuildInToolImageGeneration)
	assert.Equal(t, 2, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration].CallCount)
	assert.False(t, c.GetBool("image_generation_call"))
}

func TestOaiResponsesHandlerIncompleteStatusCommitsZeroImageGeneration(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body, err := common.Marshal(dto.OpenAIResponsesResponse{
		Status: []byte(`"incomplete"`),
		Output: []dto.ResponsesOutput{
			{
				Type:   dto.ResponsesOutputTypeImageGenerationCall,
				ID:     "img_1",
				Status: "completed",
				Result: "base64-a",
			},
		},
		Usage: &dto.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{
		OriginModelName: "gpt-5.1",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
				dto.BuildInToolImageGeneration: {ToolName: dto.BuildInToolImageGeneration, CallCount: 0},
			},
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	_, apiErr := OaiResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	assert.Equal(t, 0, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration].CallCount)
}

func TestOaiResponsesHandlerRejectsPrivateUnknownErrorShape(t *testing.T) {
	body := `{"error":{"metadata":{"workspace":"wrk_private"},"detail":"Error from provider (Console Go): Endpoint is unavailable."}}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenCodeGo}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	usage, apiErr := OaiResponsesHandler(c, info, resp)

	require.Nil(t, usage)
	require.NotNil(t, apiErr)
	assert.Contains(t, apiErr.Error(), "Console Go")
	assert.Contains(t, apiErr.Error(), "workspace")
	assert.Empty(t, w.Body.String())
	publicErr := service.PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, apiErr)
	require.NotSame(t, apiErr, publicErr)
	assert.Equal(t, http.StatusTooManyRequests, publicErr.StatusCode)
}

func TestOaiResponsesHandlerMarksUnknownOpenCodeGoError(t *testing.T) {
	body := `{"error":{"type":"upstream_error","code":"shard_failure","message":"internal shard zen-primary failed"}}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenCodeGo}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	usage, apiErr := OaiResponsesHandler(c, info, resp)

	require.Nil(t, usage)
	require.NotNil(t, apiErr)
	assert.Equal(t, "internal shard zen-primary failed", apiErr.Error())
	assert.True(t, service.IsOpenCodeGoUpstreamRelayError(apiErr))
	assert.Empty(t, w.Body.String())
	publicErr := service.PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, apiErr)
	require.NotSame(t, apiErr, publicErr)
	assert.Equal(t, http.StatusTooManyRequests, publicErr.StatusCode)
}

func TestOaiResponsesHandlerKeepsPrivateWordsOutsideError(t *testing.T) {
	body := `{"id":"resp_1","object":"response","status":"completed","metadata":{"topic":"workspace planning"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OpenCode workspace guide"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenCodeGo}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	usage, apiErr := OaiResponsesHandler(c, info, resp)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Contains(t, w.Body.String(), "OpenCode workspace guide")
}

func runResponsesImageBillingStream(t *testing.T, events ...string) *relaycommon.RelayInfo {
	t.Helper()
	gin.SetMode(gin.TestMode)
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() {
		constant.StreamingTimeout = oldTimeout
	})

	var body strings.Builder
	for _, event := range events {
		body.WriteString("data: ")
		body.WriteString(event)
		body.WriteString("\n\n")
	}
	body.WriteString("data: [DONE]\n\n")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(common.RequestIdKey, "responses-image-billing-test")
	info := &relaycommon.RelayInfo{
		OriginModelName: "gpt-5.1",
		DisablePing:     true,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gpt-5.1",
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body.String())),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}

	_, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, info.ResponsesUsageInfo)
	require.Contains(t, info.ResponsesUsageInfo.BuiltInTools, dto.BuildInToolImageGeneration)
	return info
}

func TestOaiResponsesStreamHandlerDeduplicatesCompletedImageOutput(t *testing.T) {
	item := `{"type":"image_generation_call","id":"img_1","call_id":"call_1","status":"completed","result":"base64-a"}`
	info := runResponsesImageBillingStream(
		t,
		`{"type":"response.output_item.done","output_index":0,"item":`+item+`}`,
		`{"type":"response.completed","response":{"status":"completed","output":[`+item+`],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
	)

	assert.Equal(t, 1, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration].CallCount)
}

func TestOaiResponsesStreamHandlerDiscardsImageOutputOnIncomplete(t *testing.T) {
	info := runResponsesImageBillingStream(
		t,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"image_generation_call","id":"img_1","status":"completed","result":"base64-a"}}`,
		`{"type":"response.incomplete","response":{"status":"incomplete"}}`,
	)

	assert.Equal(t, 0, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration].CallCount)
}

func TestOaiResponsesStreamHandlerDoesNotCountPartialImageEvent(t *testing.T) {
	info := runResponsesImageBillingStream(
		t,
		`{"type":"response.image_generation_call.partial_image","output_index":0,"partial_image_b64":"partial-bytes"}`,
		`{"type":"response.completed","response":{"status":"completed","output":[]}}`,
	)

	assert.Equal(t, 0, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration].CallCount)
}

func TestOaiResponsesStreamHandlerClassifiesCodeOnlyErrors(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	tests := []struct {
		name      string
		eventType string
		code      string
		upstream  bool
	}{
		{name: "failed deterministic", eventType: "response.failed", code: "invalid_prompt", upstream: false},
		{name: "failed transient", eventType: "response.failed", code: "server_error", upstream: true},
		{name: "error deterministic", eventType: "response.error", code: "input_too_long", upstream: false},
		{name: "error transient", eventType: "response.error", code: "rate_limit_exceeded", upstream: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `data: {"type":"` + tt.eventType + `","response":{"error":{"code":"` + tt.code + `","message":"provider rejected request"}}}` + "\n\n"
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Set(common.RequestIdKey, "responses-code-only-error-test")
			info := &relaycommon.RelayInfo{
				DisablePing: true,
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelType:       constant.ChannelTypeOpenCodeGo,
					UpstreamModelName: "gpt-test",
				},
			}
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			}

			_, apiErr := OaiResponsesStreamHandler(c, info, resp)
			require.NotNil(t, apiErr)
			assert.Equal(t, types.ErrorCode(tt.code), apiErr.GetErrorCode())
			require.NotNil(t, info.StreamStatus)
			assert.Equal(t, tt.upstream, info.StreamStatus.UpstreamFailureObserved())
		})
	}
}

func TestOaiResponsesStreamHandlerDoesNotWritePrivateOpenCodeGoErrorEvent(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "responses failed event",
			body: `data: {"type":"response.failed","response":{"error":{"type":"upstream_error","code":"workspace_unavailable","message":"Error from provider (Console Go): Endpoint is unavailable.","metadata":{"workspace":"wrk_private"}}}}` + "\n\n",
			want: "Console Go",
		},
		{
			name: "top level error event",
			body: `data: {"type":"error","error":{"type":"upstream_error","code":"workspace_unavailable","message":"Error from provider (Console Go): Endpoint is unavailable.","metadata":{"workspace":"wrk_private"}}}` + "\n\n",
			want: "Console Go",
		},
		{
			name: "untyped top level error",
			body: `data: {"error":{"type":"upstream_error","code":"workspace_unavailable","message":"Error from provider (Console Go): Endpoint is unavailable.","metadata":{"workspace":"wrk_private"}}}` + "\n\n",
			want: "Console Go",
		},
		{
			name: "escaped private details",
			body: `data: {"type":"error","error":{"type":"upstream_error","code":"work\u0073pace_unavailable","message":"Error from provider (Console G\u006f): Endpoint is unavailable.","metadata":{"work\u0073pace":"wrk\u005fprivate"}}}` + "\n\n",
			want: "Console Go",
		},
		{
			name: "metadata only private error",
			body: `data: {"type":"response.failed","response":{"error":{"metadata":{"workspace":"wrk_private"},"detail":"Console Go internal channel unavailable"}}}` + "\n\n",
			want: "Console Go",
		},
		{
			name: "failed root fields",
			body: `data: {"type":"response.failed","message":"Error from provider (Console Go): Endpoint is unavailable.","code":"workspace_unavailable","metadata":{"workspace":"wrk_private"}}` + "\n\n",
			want: "Console Go",
		},
		{
			name: "response error root fields",
			body: `data: {"type":"response.error","detail":"Console Go workspace wrk_private is unavailable"}` + "\n\n",
			want: "Console Go",
		},
		{
			name: "cancelled root fields",
			body: `data: {"type":"response.cancelled","message":"OpenCode workspace wrk_private endpoint is unavailable"}` + "\n\n",
			want: "OpenCode",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Set(common.RequestIdKey, "responses-private-error-test")
			info := &relaycommon.RelayInfo{
				DisablePing: true,
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelType:       constant.ChannelTypeOpenCodeGo,
					UpstreamModelName: "gpt-test",
				},
			}
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(test.body)),
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			}

			_, apiErr := OaiResponsesStreamHandler(c, info, resp)

			require.NotNil(t, apiErr)
			assert.Contains(t, apiErr.Error(), test.want)
			assert.Empty(t, w.Body.String())
			publicErr := service.PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, apiErr)
			require.NotSame(t, apiErr, publicErr)
			assert.Equal(t, http.StatusTooManyRequests, publicErr.StatusCode)
			assert.Equal(t, service.OpenCodeGoPublicOverloadMessage, publicErr.Error())
		})
	}
}

func TestOaiResponsesStreamHandlerDoesNotWriteUnknownOpenCodeGoErrorEvent(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := `data: {"type":"response.failed","response":{"error":{"type":"upstream_error","code":"shard_failure","message":"internal shard zen-primary failed"}}}` + "\n\n"
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenCodeGo,
			UpstreamModelName: "gpt-test",
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}

	_, apiErr := OaiResponsesStreamHandler(c, info, resp)

	require.NotNil(t, apiErr)
	assert.Equal(t, "internal shard zen-primary failed", apiErr.Error())
	assert.True(t, service.IsOpenCodeGoUpstreamRelayError(apiErr))
	assert.Empty(t, w.Body.String())
	publicErr := service.PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, apiErr)
	require.NotSame(t, apiErr, publicErr)
	assert.Equal(t, http.StatusTooManyRequests, publicErr.StatusCode)
}

func TestOaiResponsesStreamHandlerIgnoresEmptyErrorPlaceholder(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"public text","error":{}}`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		"",
	}, "\n\n")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenCodeGo,
			UpstreamModelName: "gpt-test",
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}

	_, apiErr := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, apiErr)
	assert.Contains(t, w.Body.String(), "public text")
}

func TestOaiResponsesStreamHandlerFailsClosedForExplicitEmptyErrorEvent(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	for _, eventType := range []string{"error", "response.error", "response.failed"} {
		t.Run(eventType, func(t *testing.T) {
			body := `data: {"type":"` + eventType + `","error":{}}` + "\n\n"
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			info := &relaycommon.RelayInfo{
				DisablePing: true,
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelType:       constant.ChannelTypeOpenCodeGo,
					UpstreamModelName: "gpt-test",
				},
			}
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			}

			_, apiErr := OaiResponsesStreamHandler(c, info, resp)

			require.NotNil(t, apiErr)
			assert.True(t, service.IsOpenCodeGoUpstreamRelayError(apiErr))
			assert.Empty(t, w.Body.String())
		})
	}
}

func TestOaiResponsesStreamHandlerDoesNotAppendUnknownErrorAfterDelta(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"public text"}`,
		`data: {"type":"response.error","response":{"error":{"type":"upstream_error","code":"shard_failure","message":"internal shard zen-primary failed"}}}`,
		"",
	}, "\n\n")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenCodeGo,
			UpstreamModelName: "gpt-test",
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}

	_, apiErr := OaiResponsesStreamHandler(c, info, resp)

	require.NotNil(t, apiErr)
	assert.True(t, service.IsOpenCodeGoUpstreamRelayError(apiErr))
	responseBody := w.Body.String()
	assert.Contains(t, responseBody, "public text")
	assert.NotContains(t, responseBody, "internal shard")
	assert.NotContains(t, responseBody, "zen-primary")
}

func TestOaiResponsesStreamHandlerDoesNotTreatNormalOutputAsPrivateError(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := `data: {"type":"response.incomplete","response":{"status":"incomplete","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"workspace planning"}]}],"incomplete_details":{"reason":"max_output_tokens"}}}` + "\n\n"
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenCodeGo,
			UpstreamModelName: "gpt-test",
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}

	_, apiErr := OaiResponsesStreamHandler(c, info, resp)

	require.NotNil(t, apiErr)
	assert.Contains(t, w.Body.String(), "workspace planning")
	publicErr := service.PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, apiErr)
	require.NotSame(t, apiErr, publicErr)
	assert.Equal(t, http.StatusTooManyRequests, publicErr.StatusCode)
	assert.Equal(t, constant.OpenCodeGoPublicRateLimitErrorCode, string(publicErr.GetErrorCode()))
}

func TestOaiResponsesStreamHandlerRejectsPrivateIncompleteDetails(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := `data: {"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"workspace_unavailable"}}}` + "\n\n"
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenCodeGo,
			UpstreamModelName: "gpt-test",
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}

	_, apiErr := OaiResponsesStreamHandler(c, info, resp)

	require.NotNil(t, apiErr)
	assert.Contains(t, apiErr.Error(), "workspace_unavailable")
	assert.Empty(t, w.Body.String())
	require.NotSame(t, apiErr, service.PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, apiErr))
}

func TestOaiResponsesStreamHandlerDoesNotAppendPrivateOpenCodeGoErrorAfterDelta(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"public text"}`,
		`data: {"type":"response.failed","response":{"error":{"type":"upstream_error","code":"workspace_unavailable","message":"Error from provider (Console Go): Endpoint is unavailable.","metadata":{"workspace":"wrk_private"}}}}`,
		"",
	}, "\n\n")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(common.RequestIdKey, "responses-private-error-after-delta-test")
	info := &relaycommon.RelayInfo{
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenCodeGo,
			UpstreamModelName: "gpt-test",
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}

	_, apiErr := OaiResponsesStreamHandler(c, info, resp)

	require.NotNil(t, apiErr)
	assert.True(t, w.Flushed)
	responseBody := strings.ToLower(w.Body.String())
	assert.Contains(t, responseBody, "public text")
	for _, marker := range []string{"opencode", "console go", "workspace", "wrk_", "endpoint is unavailable"} {
		assert.NotContains(t, responseBody, marker)
	}
}

func TestOaiResponsesStreamHandlerKeepsOtherChannelErrorEventBehavior(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := `data: {"type":"response.error","response":{"error":{"code":"workspace_unavailable","message":"workspace is unavailable"}}}` + "\n\n"
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(common.RequestIdKey, "responses-other-channel-error-test")
	info := &relaycommon.RelayInfo{
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenAI,
			UpstreamModelName: "gpt-test",
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}

	_, apiErr := OaiResponsesStreamHandler(c, info, resp)

	require.NotNil(t, apiErr)
	assert.True(t, w.Flushed)
	assert.Contains(t, w.Body.String(), "workspace is unavailable")
}
