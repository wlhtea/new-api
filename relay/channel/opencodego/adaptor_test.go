package opencodego

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAdaptorTestContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c
}

func newAdaptorTestInfo(model string, stream bool) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		OriginModelName: "public-model",
		IsStream:        stream,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenCodeGo,
			ApiType:           constant.APITypeOpenCodeGo,
			ApiKey:            "test-account-key",
			ChannelBaseUrl:    "https://untrusted.invalid/custom",
			UpstreamModelName: model,
		},
	}
}

func requestForFormat(format types.RelayFormat) any {
	stream := true
	switch format {
	case types.RelayFormatOpenAI:
		return &dto.GeneralOpenAIRequest{
			Model:    "public-model",
			Messages: []dto.Message{{Role: "user", Content: "hello"}},
			Stream:   &stream,
		}
	case types.RelayFormatClaude:
		maxTokens := uint(32)
		return &dto.ClaudeRequest{
			Model:     "public-model",
			Messages:  []dto.ClaudeMessage{{Role: "user", Content: "hello"}},
			MaxTokens: &maxTokens,
			Stream:    &stream,
		}
	case types.RelayFormatOpenAIResponses:
		return &dto.OpenAIResponsesRequest{
			Model:  "public-model",
			Input:  json.RawMessage(`"hello"`),
			Stream: &stream,
		}
	default:
		panic("unsupported test format")
	}
}

func convertAdaptorRequest(a *Adaptor, c *gin.Context, info *relaycommon.RelayInfo, format types.RelayFormat, request any) (any, error) {
	switch format {
	case types.RelayFormatOpenAI:
		return a.ConvertOpenAIRequest(c, info, request.(*dto.GeneralOpenAIRequest))
	case types.RelayFormatClaude:
		return a.ConvertClaudeRequest(c, info, request.(*dto.ClaudeRequest))
	case types.RelayFormatOpenAIResponses:
		return a.ConvertOpenAIResponsesRequest(c, info, *request.(*dto.OpenAIResponsesRequest))
	default:
		panic("unsupported test format")
	}
}

func TestAdaptorRequestConversionMatrix(t *testing.T) {
	targets := []struct {
		model    string
		protocol Protocol
		wantType any
	}{
		{model: "glm-5.2", protocol: ProtocolChat, wantType: &dto.GeneralOpenAIRequest{}},
		{model: "minimax-m3", protocol: ProtocolMessages, wantType: &dto.ClaudeRequest{}},
		{model: "gpt-5.6-luna", protocol: ProtocolResponses, wantType: &dto.OpenAIResponsesRequest{}},
	}
	clients := []types.RelayFormat{
		types.RelayFormatOpenAI,
		types.RelayFormatClaude,
		types.RelayFormatOpenAIResponses,
	}

	for _, target := range targets {
		for _, client := range clients {
			name := string(client) + "_to_" + string(target.protocol)
			t.Run(name, func(t *testing.T) {
				info := newAdaptorTestInfo(target.model, true)
				adaptor := &Adaptor{}
				adaptor.Init(info)

				converted, err := convertAdaptorRequest(adaptor, newAdaptorTestContext(), info, client, requestForFormat(client))
				require.NoError(t, err)
				assert.Equal(t, reflect.TypeOf(target.wantType), reflect.TypeOf(converted))
				assert.Equal(t, target.protocol.RelayFormat(), info.FinalRequestRelayFormat)

				switch value := converted.(type) {
				case *dto.GeneralOpenAIRequest:
					assert.Equal(t, target.model, value.Model)
					require.NotNil(t, value.StreamOptions)
					assert.True(t, value.StreamOptions.IncludeUsage)
				case *dto.ClaudeRequest:
					assert.Equal(t, target.model, value.Model)
				case *dto.OpenAIResponsesRequest:
					assert.Equal(t, target.model, value.Model)
					var cacheKey string
					require.NoError(t, json.Unmarshal(value.PromptCacheKey, &cacheKey))
					assert.Equal(t, adaptor.cacheIdentity, cacheKey)
				}
			})
		}
	}
}

func TestAdaptorUsesFixedURLsAndProtocolAuthentication(t *testing.T) {
	tests := []struct {
		model       string
		path        string
		wantBearer  bool
		wantAPIKey  bool
		wantVersion bool
	}{
		{model: "glm-5.2", path: "/chat/completions", wantBearer: true},
		{model: "minimax-m3", path: "/messages", wantAPIKey: true, wantVersion: true},
		{model: "gpt-5.6-luna", path: "/responses", wantBearer: true},
	}

	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			info := newAdaptorTestInfo(test.model, false)
			adaptor := &Adaptor{}
			adaptor.Init(info)
			_, err := adaptor.ConvertOpenAIRequest(newAdaptorTestContext(), info, requestForFormat(types.RelayFormatOpenAI).(*dto.GeneralOpenAIRequest))
			require.NoError(t, err)

			requestURL, err := adaptor.GetRequestURL(info)
			require.NoError(t, err)
			assert.Equal(t, constant.ChannelBaseURLs[constant.ChannelTypeOpenCodeGo]+test.path, requestURL)
			assert.NotContains(t, requestURL, "untrusted.invalid")

			header := http.Header{}
			require.NoError(t, adaptor.SetupRequestHeader(newAdaptorTestContext(), &header, info))
			assert.Equal(t, test.wantBearer, header.Get("Authorization") == "Bearer test-account-key")
			assert.Equal(t, test.wantAPIKey, header.Get("x-api-key") == "test-account-key")
			assert.Equal(t, test.wantVersion, header.Get("anthropic-version") == "2023-06-01")
			assert.Equal(t, adaptor.cacheIdentity, header.Get(cacheIdentityHeader))
		})
	}
}

func TestAdaptorRejectsPassThroughAndUnknownProtocol(t *testing.T) {
	info := newAdaptorTestInfo("glm-5.2", false)
	adaptor := &Adaptor{}
	adaptor.Init(info)
	_, err := adaptor.DoRequest(newAdaptorTestContext(), info, nil)
	require.ErrorContains(t, err, "does not allow pass-through")

	info = newAdaptorTestInfo("future-unknown", false)
	adaptor.Init(info)
	_, err = adaptor.GetRequestURL(info)
	require.ErrorContains(t, err, "protocol is not configured")
}
