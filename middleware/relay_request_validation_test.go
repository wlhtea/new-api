package middleware

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestPreValidateRelayRequestRendersProtocolSpecificErrorsBeforeNext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name     string
		path     string
		body     string
		wantType string
	}{
		{
			name:     "claude envelope",
			path:     "/v1/messages",
			body:     `{"model":"test-model","messages":[{"content":"hello"}]}`,
			wantType: "invalid_request_error",
		},
		{
			name:     "chat envelope",
			path:     "/v1/chat/completions",
			body:     `{"model":"test-model","messages":[{"content":"hello"}]}`,
			wantType: "invalid_request_error",
		},
		{
			name:     "responses envelope",
			path:     "/v1/responses",
			body:     `{"model":"test-model","input":[{"type":"unknown"}]}`,
			wantType: "invalid_request_error",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			nextCalled := false
			engine := gin.New()
			engine.Use(RequestId(), BodyStorageCleanup(), PreValidateRelayRequest())
			engine.POST(test.path, func(c *gin.Context) {
				nextCalled = true
				c.Status(http.StatusNoContent)
			})

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, test.path, bytes.NewBufferString(test.body))
			request.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.False(t, nextCalled)
			require.Contains(t, recorder.Body.String(), test.wantType)
			require.NotContains(t, recorder.Body.String(), "new_api_error")
			require.Contains(t, recorder.Body.String(), "request id:")
			require.NotContains(t, recorder.Body.String(), "Invalid request: Invalid request:")
		})
	}
}

func TestPreValidateRelayRequestPassesOtherPathsAndCachesTargetModel(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		body      string
		wantModel string
	}{
		{
			name:      "target route caches model",
			path:      "/v1/messages",
			body:      `{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`,
			wantModel: "test-model",
		},
		{
			name: "responses compact is not intercepted",
			path: "/v1/responses/compact",
			body: `{not-json`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine := gin.New()
			engine.Use(BodyStorageCleanup(), PreValidateRelayRequest())
			engine.POST(test.path, func(c *gin.Context) {
				if test.wantModel != "" {
					model, found, err := helper.GetCachedValidatedModel(c)
					require.NoError(t, err)
					require.True(t, found)
					require.Equal(t, test.wantModel, model)
				}
				c.Status(http.StatusNoContent)
			})

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, test.path, bytes.NewBufferString(test.body))
			request.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusNoContent, recorder.Code)
		})
	}
}

func TestPreValidateRelayRequestCachesNormalizedCodexOutputReplay(t *testing.T) {
	engine := gin.New()
	engine.Use(BodyStorageCleanup(), PreValidateRelayRequest())
	engine.POST("/v1/responses", func(c *gin.Context) {
		request, err := helper.GetAndValidateRequest(c, types.RelayFormatOpenAIResponses)
		require.NoError(t, err)
		responsesRequest, ok := request.(*dto.OpenAIResponsesRequest)
		require.True(t, ok)

		var input []map[string]any
		require.NoError(t, common.Unmarshal(responsesRequest.Input, &input))
		require.Equal(t, "completed", input[1]["status"])

		converted, err := service.ConvertRequest(c, nil, types.RelayFormatOpenAI, responsesRequest)
		require.NoError(t, err)
		chatRequest, ok := converted.Value.(*dto.GeneralOpenAIRequest)
		require.True(t, ok)
		require.Len(t, chatRequest.Messages, 3)
		require.Equal(t, []string{"user", "assistant", "user"}, []string{
			chatRequest.Messages[0].Role,
			chatRequest.Messages[1].Role,
			chatRequest.Messages[2].Role,
		})
		require.Equal(t, "Hello", chatRequest.Messages[1].StringContent())
		c.Status(http.StatusNoContent)
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{
		"model":"kimi-k3",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hello","annotations":[],"logprobs":[]}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"do you love me?"}]}
		]
	}`))
	request.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusNoContent, recorder.Code, recorder.Body.String())
}

func TestPreValidateRelayRequestPreparesTokenAffinityBeforeDistribution(t *testing.T) {
	previousSecret := common.CryptoSecret
	common.CryptoSecret = "test-pre-validation-affinity-secret"
	t.Cleanup(func() { common.CryptoSecret = previousSecret })

	const tokenID = 7001
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyTokenId, tokenID)
		c.Next()
	})
	engine.Use(BodyStorageCleanup(), PreValidateRelayRequest())
	engine.POST("/v1/responses", func(c *gin.Context) {
		identity, ok := service.GetOpenCodeAffinityIdentity(c)
		require.True(t, ok)
		require.Equal(t, constant.OpenCodeGoAffinitySourceToken, identity.Source)
		require.Equal(t, common.OpenCodeGoDiagnosticRef("token-fallback", "7001"), identity.Value)
		require.NotContains(t, identity.Value, "7001")
		c.Status(http.StatusNoContent)
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		bytes.NewBufferString(`{"model":"gpt-5.6-luna","input":"hello"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusNoContent, recorder.Code, recorder.Body.String())
}

func TestPreValidateRelayRequestAllowsMissingContentTypeButRejectsExplicitNonJSON(t *testing.T) {
	validBody := `{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`

	for _, test := range []struct {
		name        string
		contentType string
		wantStatus  int
	}{
		{name: "missing content type remains compatible", wantStatus: http.StatusNoContent},
		{name: "json with charset is accepted", contentType: "application/json; charset=utf-8", wantStatus: http.StatusNoContent},
		{name: "json suffix is accepted", contentType: "application/vnd.api+json", wantStatus: http.StatusNoContent},
		{name: "jsonp is not json", contentType: "application/jsonp", wantStatus: http.StatusUnsupportedMediaType},
		{name: "explicit text plain is unsupported", contentType: "text/plain", wantStatus: http.StatusUnsupportedMediaType},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := gin.New()
			engine.Use(BodyStorageCleanup(), PreValidateRelayRequest())
			engine.POST("/v1/messages", func(c *gin.Context) { c.Status(http.StatusNoContent) })

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(validBody))
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			engine.ServeHTTP(recorder, request)

			require.Equal(t, test.wantStatus, recorder.Code)
		})
	}
}

type relayValidationErrorReader struct {
	err error
}

func (r relayValidationErrorReader) Read(_ []byte) (int, error) {
	return 0, r.err
}

func TestPreValidateRelayRequestPreservesBodyReadStatus(t *testing.T) {
	originalMaxRequestBodyMB := constant.MaxRequestBodyMB
	constant.MaxRequestBodyMB = 1
	t.Cleanup(func() { constant.MaxRequestBodyMB = originalMaxRequestBodyMB })

	tests := []struct {
		name       string
		body       io.Reader
		wantStatus int
		wantType   string
	}{
		{
			name:       "body too large",
			body:       bytes.NewReader(bytes.Repeat([]byte("x"), (1<<20)+1)),
			wantStatus: http.StatusRequestEntityTooLarge,
			wantType:   "invalid_request_error",
		},
		{
			name:       "request canceled while reading",
			body:       relayValidationErrorReader{err: context.Canceled},
			wantStatus: 499,
			wantType:   "request_canceled",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine := gin.New()
			engine.Use(RequestId(), BodyStorageCleanup(), PreValidateRelayRequest())
			engine.POST("/v1/messages", func(c *gin.Context) { c.Status(http.StatusNoContent) })

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", test.body)
			request.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(recorder, request)

			require.Equal(t, test.wantStatus, recorder.Code)
			require.Contains(t, recorder.Body.String(), test.wantType)
			require.NotEmpty(t, recorder.Header().Get(common.RequestIdKey))
		})
	}
}

func TestRenderRelayRequestValidationErrorDoesNotExposeUncontrolledError(t *testing.T) {
	engine := gin.New()
	engine.Use(RequestId())
	engine.POST("/v1/messages", func(c *gin.Context) {
		renderRelayRequestValidationError(c, "claude", errors.New("private workspace upstream detail"))
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	engine.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "request body could not be read")
	require.NotContains(t, recorder.Body.String(), "private workspace upstream detail")
}
