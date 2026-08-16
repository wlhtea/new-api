package common

import (
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewReplayableBodyReaderKeepsStorageLifecycleWithCaller(t *testing.T) {
	payload := []byte(`{"model":"test-model","input":"hello"}`)
	storage, err := CreateBodyStorage(payload)
	require.NoError(t, err)
	defer storage.Close()

	body := NewReplayableBodyReader(storage)
	assert.EqualValues(t, len(payload), body.Size())
	_, exposesCloser := any(body).(io.Closer)
	assert.False(t, exposesCloser, "the request body must not expose the storage closer")

	req, err := http.NewRequest(http.MethodPost, "https://example.com", body)
	require.NoError(t, err)
	require.NoError(t, req.Body.Close())

	replayBody, err := body.NewReader()
	require.NoError(t, err, "closing the HTTP request body must not close the storage")
	replay, err := io.ReadAll(replayBody)
	require.NoError(t, err)
	require.NoError(t, replayBody.Close())
	assert.Equal(t, payload, replay)

	require.NoError(t, storage.Close())
	_, err = body.NewReader()
	require.ErrorIs(t, err, ErrStorageClosed)
}

func TestMemoryBodyStorageDoesNotExposeMutableBackingData(t *testing.T) {
	input := []byte(`{"model":"immutable"}`)
	storage, err := CreateBodyStorage(input)
	require.NoError(t, err)
	t.Cleanup(func() { _ = storage.Close() })
	require.False(t, storage.IsDisk())

	input[10] = 'X'
	first, err := storage.Bytes()
	require.NoError(t, err)
	assert.Equal(t, `{"model":"immutable"}`, string(first))

	first[10] = 'Y'
	second, err := storage.Bytes()
	require.NoError(t, err)
	assert.Equal(t, `{"model":"immutable"}`, string(second))
}
