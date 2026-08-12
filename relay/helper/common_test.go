package helper

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type failingEventWriter struct {
	header http.Header
	writes int
	failAt int
}

func (w *failingEventWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failingEventWriter) WriteHeader(int) {}

func (w *failingEventWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.failAt > 0 && w.writes >= w.failAt {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

func (w *failingEventWriter) Flush() {}

func newFailingEventContext(t *testing.T, writer http.ResponseWriter) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c
}

func TestStringDataPropagatesClosedPipe(t *testing.T) {
	writer := &failingEventWriter{failAt: 1}
	c := newFailingEventContext(t, writer)

	err := StringData(c, "payload")
	require.Error(t, err)
	require.True(t, errors.Is(err, io.ErrClosedPipe), "error should retain the writer cause: %v", err)
}

func TestClaudeDataPropagatesSecondEventWriteError(t *testing.T) {
	writer := &failingEventWriter{failAt: 2}
	c := newFailingEventContext(t, writer)

	err := ClaudeData(c, dto.ClaudeResponse{Type: "message_stop"})
	require.Error(t, err)
	require.True(t, errors.Is(err, io.ErrClosedPipe), "error should retain the writer cause: %v", err)
}

func TestResetEventStreamHeadersAllowsAttemptToRestoreHeaders(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	SetEventStreamHeaders(c)
	c.Writer.Header().Del("Content-Type")
	ResetEventStreamHeaders(c)
	SetEventStreamHeaders(c)

	require.Equal(t, "text/event-stream", c.Writer.Header().Get("Content-Type"))
}
