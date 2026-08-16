package middleware

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

type observedRequestBody struct {
	reader io.Reader
	reads  int
	closes int
}

func (body *observedRequestBody) Read(p []byte) (int, error) {
	body.reads++
	return body.reader.Read(p)
}

func (body *observedRequestBody) Close() error {
	body.closes++
	return nil
}

func encodeMiddlewareTestBody(t *testing.T, encoding string, payload []byte) []byte {
	t.Helper()

	var encoded bytes.Buffer
	switch encoding {
	case "", "identity":
		return bytes.Clone(payload)
	case "gzip":
		writer := gzip.NewWriter(&encoded)
		_, err := writer.Write(payload)
		require.NoError(t, err)
		require.NoError(t, writer.Close())
	case "br":
		writer := brotli.NewWriter(&encoded)
		_, err := writer.Write(payload)
		require.NoError(t, err)
		require.NoError(t, writer.Close())
	case "zstd":
		writer, err := zstd.NewWriter(&encoded)
		require.NoError(t, err)
		_, err = writer.Write(payload)
		require.NoError(t, err)
		require.NoError(t, writer.Close())
	default:
		t.Fatalf("unsupported test encoding %q", encoding)
	}
	return encoded.Bytes()
}

func requireEncodingErrorEnvelope(t *testing.T, body []byte, claude bool, fixed400 bool) {
	t.Helper()

	var envelope map[string]any
	require.NoError(t, json.Unmarshal(body, &envelope))
	if claude {
		require.Equal(t, "error", envelope["type"])
		errorObject, ok := envelope["error"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "invalid_request_error", errorObject["type"])
		if fixed400 {
			require.Equal(t, constant.OpenCodeGoPublicInvalidRequestMessage, errorObject["message"])
		}
		return
	}
	errorObject, ok := envelope["error"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "invalid_request_error", errorObject["type"])
	if fixed400 {
		require.Equal(t, "invalid_request_error", errorObject["code"])
		require.Equal(t, "", errorObject["param"])
		require.Equal(t, constant.OpenCodeGoPublicInvalidRequestMessage, errorObject["message"])
	} else {
		require.Equal(t, "invalid_request", errorObject["code"])
	}
}

func TestNormalizedRequestContentEncoding(t *testing.T) {
	tests := []struct {
		name    string
		values  []string
		want    string
		wantErr bool
	}{
		{name: "absent"},
		{name: "identity", values: []string{" IdEnTiTy "}, want: "identity"},
		{name: "gzip", values: []string{"GZip"}, want: "gzip"},
		{name: "brotli", values: []string{"BR"}, want: "br"},
		{name: "zstd", values: []string{"ZsTd"}, want: "zstd"},
		{name: "unknown", values: []string{"snappy"}, wantErr: true},
		{name: "stacked", values: []string{"gzip, br"}, wantErr: true},
		{name: "repeated lines", values: []string{"gzip", "br"}, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header := make(http.Header)
			for _, value := range test.values {
				header.Add("Content-Encoding", value)
			}
			got, err := normalizedRequestContentEncoding(header)
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}

func TestDecompressRequestMiddlewareSupportsSingleEncodingsAfterAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	payload := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`)
	tests := []struct {
		name        string
		encoding    string
		headerValue string
	}{
		{name: "missing", encoding: ""},
		{name: "identity", encoding: "identity", headerValue: "IdEnTiTy"},
		{name: "gzip", encoding: "gzip", headerValue: "GZip"},
		{name: "brotli", encoding: "br", headerValue: "BR"},
		{name: "zstd", encoding: "zstd", headerValue: "ZsTd"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded := encodeMiddlewareTestBody(t, test.encoding, payload)
			observedBody := &observedRequestBody{reader: bytes.NewReader(encoded)}
			authenticated := false

			engine := gin.New()
			engine.Use(encodedRequestAdmissionMiddleware(1 << 20))
			engine.Use(func(c *gin.Context) {
				require.Zero(t, observedBody.reads)
				authenticated = true
				c.Next()
			})
			engine.Use(decompressRequestMiddleware(1 << 20))
			engine.POST("/v1/messages", func(c *gin.Context) {
				require.True(t, authenticated)
				require.Empty(t, c.Request.Header.Get("Content-Encoding"))
				require.Empty(t, c.Request.Header.Get("Content-Length"))
				require.Equal(t, int64(-1), c.Request.ContentLength)
				decoded, err := io.ReadAll(c.Request.Body)
				require.NoError(t, err)
				require.Equal(t, payload, decoded)
				require.NoError(t, c.Request.Body.Close())
				c.Status(http.StatusNoContent)
			})

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			request.Body = observedBody
			request.ContentLength = int64(len(encoded))
			request.Header.Set("Content-Length", strconv.Itoa(len(encoded)))
			if test.headerValue != "" {
				request.Header.Set("Content-Encoding", test.headerValue)
			}
			engine.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusNoContent, recorder.Code, recorder.Body.String())
			require.Positive(t, observedBody.reads)
			require.Equal(t, 1, observedBody.closes)
		})
	}
}

func TestDecompressRequestMiddlewareDoesNotReadBeforeAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := &observedRequestBody{reader: bytes.NewBufferString("not-gzip")}

	engine := gin.New()
	engine.Use(encodedRequestAdmissionMiddleware(64))
	engine.Use(func(c *gin.Context) {
		require.Zero(t, body.reads)
		c.AbortWithStatus(http.StatusUnauthorized)
	})
	engine.Use(decompressRequestMiddleware(64))
	engine.POST("/v1/messages", func(c *gin.Context) {
		t.Fatal("unauthenticated request reached the handler")
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	request.Body = body
	request.Header.Set("Content-Encoding", "gzip")
	engine.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	require.Zero(t, body.reads)
}

func TestDecompressRequestMiddlewareRejectsUnsupportedAndStackedEncodings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	encodings := []struct {
		name   string
		values []string
	}{
		{name: "unknown", values: []string{"snappy"}},
		{name: "stacked", values: []string{"gzip, br"}},
		{name: "multiple lines", values: []string{"gzip", "br"}},
	}
	paths := []struct {
		path   string
		claude bool
	}{
		{path: "/v1/messages", claude: true},
		{path: "/v1/chat/completions"},
	}

	for _, encoding := range encodings {
		for _, path := range paths {
			t.Run(encoding.name+"_"+path.path, func(t *testing.T) {
				closedBody := &observedRequestBody{reader: bytes.NewBufferString("body")}
				nextCalled := false
				engine := gin.New()
				engine.Use(RequestId(), decompressRequestMiddleware(64))
				engine.POST(path.path, func(c *gin.Context) {
					nextCalled = true
					c.Status(http.StatusNoContent)
				})

				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, path.path, nil)
				request.Body = closedBody
				for _, value := range encoding.values {
					request.Header.Add("Content-Encoding", value)
				}
				engine.ServeHTTP(recorder, request)

				require.Equal(t, http.StatusUnsupportedMediaType, recorder.Code)
				require.False(t, nextCalled)
				require.Equal(t, 1, closedBody.closes)
				requireEncodingErrorEnvelope(t, recorder.Body.Bytes(), path.claude, false)
			})
		}
	}
}

func TestDecompressRequestMiddlewareRejectsMalformedTruncatedAndMultiMemberBodies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	payload := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`)
	truncated := func(encoded []byte, bytesToRemove int) []byte {
		require.Greater(t, len(encoded), bytesToRemove)
		return bytes.Clone(encoded[:len(encoded)-bytesToRemove])
	}
	multiMember := append(
		encodeMiddlewareTestBody(t, "gzip", payload),
		encodeMiddlewareTestBody(t, "gzip", []byte(`{}`))...,
	)
	brotliBody := encodeMiddlewareTestBody(t, "br", payload)
	tests := []struct {
		name     string
		path     string
		encoding string
		body     []byte
		claude   bool
		message  string
	}{
		{name: "malformed gzip", path: "/v1/messages", encoding: "gzip", body: []byte("not-gzip"), claude: true, message: "request body is not valid gzip data"},
		{name: "truncated gzip", path: "/v1/chat/completions", encoding: "gzip", body: truncated(encodeMiddlewareTestBody(t, "gzip", payload), 1), message: "request body does not match its Content-Encoding"},
		{name: "truncated brotli", path: "/v1/messages", encoding: "br", body: truncated(brotliBody, len(brotliBody)/2), claude: true, message: "request body does not match its Content-Encoding"},
		{name: "truncated zstd", path: "/v1/responses", encoding: "zstd", body: truncated(encodeMiddlewareTestBody(t, "zstd", payload), 1), message: "request body does not match its Content-Encoding"},
		{name: "multiple gzip members", path: "/v1/chat/completions", encoding: "gzip", body: multiMember, message: "request body does not match its Content-Encoding"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			nextCalled := false
			engine := gin.New()
			engine.Use(RequestId(), decompressRequestMiddleware(1<<20), BodyStorageCleanup(), PreValidateRelayRequest())
			engine.POST(test.path, func(c *gin.Context) {
				nextCalled = true
				c.Status(http.StatusNoContent)
			})

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, test.path, bytes.NewReader(test.body))
			request.Header.Set("Content-Encoding", test.encoding)
			request.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
			require.False(t, nextCalled)
			requireEncodingErrorEnvelope(t, recorder.Body.Bytes(), test.claude, true)
			require.NotContains(t, recorder.Body.String(), test.message)
			require.NotContains(t, recorder.Body.String(), "not-gzip")
		})
	}
}

func TestRequestBodyEncodingLimitsAtBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const limit = int64(64)

	t.Run("encoded bytes", func(t *testing.T) {
		for _, test := range []struct {
			name       string
			size       int
			wantStatus int
		}{
			{name: "at limit", size: int(limit), wantStatus: http.StatusNoContent},
			{name: "limit plus one", size: int(limit) + 1, wantStatus: http.StatusRequestEntityTooLarge},
		} {
			t.Run(test.name, func(t *testing.T) {
				engine := gin.New()
				engine.Use(encodedRequestAdmissionMiddleware(limit))
				engine.POST("/body", func(c *gin.Context) {
					_, err := io.ReadAll(c.Request.Body)
					if common.IsRequestBodyTooLargeError(err) {
						c.Status(http.StatusRequestEntityTooLarge)
						return
					}
					require.NoError(t, err)
					c.Status(http.StatusNoContent)
				})

				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, "/body", bytes.NewReader(bytes.Repeat([]byte("x"), test.size)))
				engine.ServeHTTP(recorder, request)
				require.Equal(t, test.wantStatus, recorder.Code)
			})
		}
	})

	t.Run("decoded bytes", func(t *testing.T) {
		for _, test := range []struct {
			name       string
			size       int
			wantStatus int
		}{
			{name: "at limit", size: int(limit), wantStatus: http.StatusNoContent},
			{name: "limit plus one", size: int(limit) + 1, wantStatus: http.StatusRequestEntityTooLarge},
		} {
			t.Run(test.name, func(t *testing.T) {
				encoded := encodeMiddlewareTestBody(t, "gzip", bytes.Repeat([]byte("x"), test.size))
				require.LessOrEqual(t, int64(len(encoded)), limit)
				engine := gin.New()
				engine.Use(encodedRequestAdmissionMiddleware(limit), decompressRequestMiddleware(limit))
				engine.POST("/body", func(c *gin.Context) {
					_, err := io.ReadAll(c.Request.Body)
					if common.IsRequestBodyTooLargeError(err) {
						c.Status(http.StatusRequestEntityTooLarge)
						return
					}
					require.NoError(t, err)
					c.Status(http.StatusNoContent)
				})

				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, "/body", bytes.NewReader(encoded))
				request.Header.Set("Content-Encoding", "gzip")
				engine.ServeHTTP(recorder, request)
				require.Equal(t, test.wantStatus, recorder.Code)
			})
		}
	})
}
