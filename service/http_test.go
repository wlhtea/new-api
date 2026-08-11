package service

import (
	"errors"
	"net/http/httptest"
	"testing"

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
