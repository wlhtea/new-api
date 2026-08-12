package service

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
)

type responseBodyFailingWriter struct {
	gin.ResponseWriter
	err error
}

func (w *responseBodyFailingWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestIOCopyBytesGracefullyRecordsFirstWriteError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	firstErr := errors.New("first response write failed")
	secondErr := errors.New("second response write failed")
	w := &responseBodyFailingWriter{ResponseWriter: c.Writer, err: firstErr}
	c.Writer = w

	IOCopyBytesGracefully(c, nil, []byte("first"))
	w.err = secondErr
	IOCopyBytesGracefully(c, nil, []byte("second"))

	if got := ResponseBodyWriteError(c); !errors.Is(got, firstErr) {
		t.Fatalf("ResponseBodyWriteError() = %v, want first error %v", got, firstErr)
	}
}

func TestResetResponseBodyWriteErrorClearsReusedContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	writeErr := errors.New("response write failed")
	c.Writer = &responseBodyFailingWriter{ResponseWriter: c.Writer, err: writeErr}

	IOCopyBytesGracefully(c, nil, []byte("response"))
	if got := ResponseBodyWriteError(c); !errors.Is(got, writeErr) {
		t.Fatalf("ResponseBodyWriteError() = %v, want %v", got, writeErr)
	}

	ResetResponseBodyWriteError(c)
	if got := ResponseBodyWriteError(c); got != nil {
		t.Fatalf("ResponseBodyWriteError() after reset = %v, want nil", got)
	}
}

func TestShouldCopyUpstreamHeaderOpenCodeGoRejectsAllUpstreamHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeGo)

	for _, header := range []string{
		"Content-Type",
		"Content-Length",
		"Cache-Control",
		"Retry-After",
		"X-Codex-Turn-State",
		"X-Reasoning-Included",
		"X-Internal-Shard",
	} {
		t.Run(header, func(t *testing.T) {
			if ShouldCopyUpstreamHeader(c, header, []string{"workspace=wrk_private"}) {
				t.Fatalf("ShouldCopyUpstreamHeader(%q) = true, want false", header)
			}
		})
	}

	if ShouldCopyUpstreamHeader(c, "x-oneapi-request-id", []string{"upstream-private-id"}) {
		t.Fatal("ShouldCopyUpstreamHeader(X-Oneapi-Request-Id) = true, want false")
	}
	if got := c.GetString(common.UpstreamRequestIdKey); got != "upstream-private-id" {
		t.Fatalf("captured upstream request ID = %q, want %q", got, "upstream-private-id")
	}
}

func TestShouldCopyUpstreamHeaderPreservesOtherChannelBehavior(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)

	for _, header := range []string{
		"Content-Type",
		"Cache-Control",
		"Retry-After",
		"X-Codex-Turn-State",
		"X-Reasoning-Included",
		"X-Internal-Shard",
	} {
		t.Run(header, func(t *testing.T) {
			if !ShouldCopyUpstreamHeader(c, header, []string{"preserved"}) {
				t.Fatalf("ShouldCopyUpstreamHeader(%q) = false, want true", header)
			}
		})
	}

	if ShouldCopyUpstreamHeader(c, "content-length", []string{"123"}) {
		t.Fatal("ShouldCopyUpstreamHeader(Content-Length) = true, want false")
	}
	if ShouldCopyUpstreamHeader(c, common.RequestIdKey, []string{"other-upstream-id"}) {
		t.Fatal("ShouldCopyUpstreamHeader(X-Oneapi-Request-Id) = true, want false")
	}
	if got := c.GetString(common.UpstreamRequestIdKey); got != "other-upstream-id" {
		t.Fatalf("captured upstream request ID = %q, want %q", got, "other-upstream-id")
	}
}

func TestIOCopyBytesGracefullyOpenCodeGoSynthesizesJSONHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeGo)
	c.Writer.Header().Set("X-Local-Response", "preserved")

	body := []byte(`{"ok":true}`)
	response := &http.Response{
		StatusCode: http.StatusAccepted,
		Header: http.Header{
			"Content-Type":         []string{"application/json; workspace=wrk_private"},
			"Content-Length":       []string{"999"},
			"Cache-Control":        []string{"private-endpoint-cache"},
			"Retry-After":          []string{"workspace=wrk_private"},
			"X-Codex-Turn-State":   []string{"internal-turn-state"},
			"X-Reasoning-Included": []string{"workspace=wrk_private"},
			"X-Internal-Shard":     []string{"zen-primary"},
			common.RequestIdKey:    []string{"upstream-request-private"},
		},
	}

	IOCopyBytesGracefully(c, response, body)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusAccepted)
	}
	if got := recorder.Body.String(); got != string(body) {
		t.Fatalf("body = %q, want %q", got, body)
	}
	if got := recorder.Header().Get("Content-Type"); got != gin.MIMEJSON {
		t.Fatalf("Content-Type = %q, want %q", got, gin.MIMEJSON)
	}
	if got := recorder.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length = %q, want %q", got, strconv.Itoa(len(body)))
	}
	if got := recorder.Header().Get("X-Local-Response"); got != "preserved" {
		t.Fatalf("local response header = %q, want preserved", got)
	}
	for _, header := range []string{
		"Cache-Control",
		"Retry-After",
		"X-Codex-Turn-State",
		"X-Reasoning-Included",
		"X-Internal-Shard",
		common.RequestIdKey,
	} {
		if got := recorder.Header().Get(header); got != "" {
			t.Fatalf("%s = %q, want empty", header, got)
		}
	}
	if got := c.GetString(common.UpstreamRequestIdKey); got != "upstream-request-private" {
		t.Fatalf("captured upstream request ID = %q, want %q", got, "upstream-request-private")
	}
}
