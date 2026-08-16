package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cleanupTestBodyStorage struct {
	data       []byte
	reader     *bytes.Reader
	closeCalls atomic.Int32
}

func newCleanupTestBodyStorage(data []byte) *cleanupTestBodyStorage {
	return &cleanupTestBodyStorage{data: data, reader: bytes.NewReader(data)}
}

func (s *cleanupTestBodyStorage) Read(p []byte) (int, error) {
	return s.reader.Read(p)
}

func (s *cleanupTestBodyStorage) Seek(offset int64, whence int) (int64, error) {
	return s.reader.Seek(offset, whence)
}

func (s *cleanupTestBodyStorage) Close() error {
	s.closeCalls.Add(1)
	return nil
}

func (s *cleanupTestBodyStorage) Bytes() ([]byte, error) {
	return s.data, nil
}

func (s *cleanupTestBodyStorage) Size() int64 {
	return int64(len(s.data))
}

func (s *cleanupTestBodyStorage) IsDisk() bool {
	return true
}

func (s *cleanupTestBodyStorage) NewReader() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}

func TestBodyStorageCleanupRunsAllRequestCleanupsDuringPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	storage := newCleanupTestBodyStorage([]byte(`{"model":"test"}`))
	cleanupOrder := make([]int, 0, 3)

	engine := gin.New()
	engine.Use(ProviderNeutralRecovery(), BodyStorageCleanup())
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Set(common.KeyBodyStorage, storage)
		c.Set(common.RequestIdKey, "local-request-id")
		c.Header("Access-Control-Allow-Origin", "https://client.example")
		c.Header("Retry-After", "workspace=private")
		c.Header("Set-Cookie", "credential=private")
		common.RegisterRequestCleanup(c, func() { cleanupOrder = append(cleanupOrder, 1) })
		common.RegisterRequestCleanup(c, func() {
			cleanupOrder = append(cleanupOrder, 2)
			panic("cleanup-private-value")
		})
		common.RegisterRequestCleanup(c, func() { cleanupOrder = append(cleanupOrder, 3) })
		panic("handler-private-value")
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
	engine.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	assert.Equal(t, []int{3, 2, 1}, cleanupOrder)
	assert.Equal(t, int32(1), storage.closeCalls.Load())
	assert.Equal(t, "local-request-id", recorder.Header().Get(common.RequestIdKey))
	assert.Equal(t, "https://client.example", recorder.Header().Get("Access-Control-Allow-Origin"))
	assert.Empty(t, recorder.Header().Get("Retry-After"))
	assert.Empty(t, recorder.Header().Get("Set-Cookie"))
	assert.JSONEq(t, `{"error":{"message":"服务器内部错误","type":"internal_server_error","code":"internal_server_error"}}`, recorder.Body.String())
	assert.NotContains(t, recorder.Body.String(), "handler-private-value")
	assert.NotContains(t, recorder.Body.String(), "cleanup-private-value")
}

func TestProviderNeutralRecoveryDoesNotAppendJSONAfterResponseCommit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(ProviderNeutralRecovery())
	engine.GET("/stream", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/event-stream", []byte("data: public\n\n"))
		panic("private-after-commit")
	})

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/stream", nil))

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "data: public\n\n", recorder.Body.String())
	assert.NotContains(t, recorder.Body.String(), "error")
	assert.NotContains(t, recorder.Body.String(), "private-after-commit")
}
