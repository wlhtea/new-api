package helper

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestResolveIncomingBillingExprRequestInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("Content-Type", "application/json")

	body := []byte(`{"service_tier":"fast"}`)
	ctx.Request.Body = io.NopCloser(bytes.NewReader(body))
	ctx.Set(common.KeyRequestBody, body)

	info := &relaycommon.RelayInfo{
		RequestHeaders: map[string]string{"Content-Type": "application/json"},
	}

	input, err := ResolveIncomingBillingExprRequestInput(ctx, info)
	require.NoError(t, err)
	require.Equal(t, body, input.Body)
	require.Equal(t, "application/json", input.Headers["Content-Type"])
}

func TestBuildBillingExprRequestInputFromRequest(t *testing.T) {
	request := &dto.GeneralOpenAIRequest{
		Model:  "gemini-3.1-pro-preview",
		Stream: lo.ToPtr(true),
		Messages: []dto.Message{
			{
				Role:    "user",
				Content: "hi",
			},
		},
		MaxTokens: lo.ToPtr(uint(3000)),
	}

	input, err := BuildBillingExprRequestInputFromRequest(request, map[string]string{
		"Content-Type": "application/json",
		"X-Test":       "1",
	})
	require.NoError(t, err)
	require.Equal(t, "application/json", input.Headers["Content-Type"])
	require.Equal(t, "1", input.Headers["X-Test"])
	require.True(t, gjson.GetBytes(input.Body, "stream").Bool())
	require.Equal(t, "user", gjson.GetBytes(input.Body, "messages.0.role").String())
	require.Equal(t, float64(3000), gjson.GetBytes(input.Body, "max_tokens").Float())
}

func TestResolveIncomingBillingExprRequestInputUsesValidatedEnvelopeRegardlessOfContentType(t *testing.T) {
	for _, contentType := range []string{"", "application/problem+json"} {
		t.Run(contentType, func(t *testing.T) {
			body := []byte(`{
				"model":"test-model",
				"messages":[{"role":"user","content":"hi"}],
				"service_tier":"fast",
				"stream_options":{"fast_mode":true},
				"literal.dot":{"value":17},
				"explicit_null":null
			}`)
			underlying, err := common.CreateBodyStorage(body)
			require.NoError(t, err)
			storage := &bytesForbiddenBodyStorage{BodyStorage: underlying}
			ctx := newRelayValidationContext(t, "/v1/chat/completions", body)
			ctx.Set(common.KeyBodyStorage, storage)
			if contentType != "" {
				ctx.Request.Header.Set("Content-Type", contentType)
			}
			t.Cleanup(func() { _ = storage.Close() })
			request, err := GetAndValidateRequest(ctx, types.RelayFormatOpenAI)
			require.NoError(t, err)

			input, err := ResolveIncomingBillingExprRequestInput(ctx, &relaycommon.RelayInfo{
				RelayFormat: types.RelayFormatOpenAI,
				Request:     request,
			})
			require.NoError(t, err)
			require.Empty(t, input.Body)
			require.NotNil(t, input.ResolveParam)

			value, found, err := input.ResolveParam("service_tier")
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, "fast", value)
			value, found, err = input.ResolveParam("stream_options.fast_mode")
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, true, value)
			value, found, err = input.ResolveParam("messages.#")
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, float64(1), value)
			value, found, err = input.ResolveParam(`literal\.dot.value`)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, float64(17), value)
			value, found, err = input.ResolveParam("explicit_null")
			require.NoError(t, err)
			require.True(t, found)
			require.Nil(t, value)
			_, found, err = input.ResolveParam("missing.value")
			require.NoError(t, err)
			require.False(t, found)
			_, _, err = input.ResolveParam("#.invalid")
			require.Error(t, err)
			require.Zero(t, storage.bytesCalls.Load())
		})
	}
}
