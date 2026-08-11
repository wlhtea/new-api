package opencodego

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type failingResponseBodyWriter struct {
	gin.ResponseWriter
	err error
}

func (w *failingResponseBodyWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestAdaptorRejectsNonStreamResponseWriteFailureMatrix(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	protocols := []Protocol{ProtocolChat, ProtocolMessages, ProtocolResponses}
	clients := []types.RelayFormat{
		types.RelayFormatOpenAI,
		types.RelayFormatClaude,
		types.RelayFormatOpenAIResponses,
	}
	for _, protocol := range protocols {
		for _, client := range clients {
			t.Run(string(protocol)+"_to_"+string(client), func(t *testing.T) {
				info := responseTestInfo(protocol, client, false)
				c, _ := responseTestContext(client)
				writeErr := errors.New("downstream response write failed")
				c.Writer = &failingResponseBodyWriter{ResponseWriter: c.Writer, err: writeErr}
				resp := &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(responseFixture(protocol, false))),
				}
				adaptor := &Adaptor{}
				adaptor.Init(info)

				usage, apiErr := adaptor.DoResponse(c, resp, info)

				require.Nil(t, usage)
				require.NotNil(t, apiErr)
				assert.ErrorContains(t, apiErr, writeErr.Error())
				assert.True(t, types.IsSkipRetryError(apiErr))
				assert.Equal(t, types.ErrorCodeBadResponse, apiErr.GetErrorCode())
				assert.Equal(t, http.StatusInternalServerError, apiErr.StatusCode)
				assert.ErrorIs(t, service.ResponseBodyWriteError(c), writeErr)
			})
		}
	}
}

func TestAdaptorResetsStaleResponseWriteFailureBeforeDispatch(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	info := responseTestInfo(ProtocolChat, types.RelayFormatOpenAI, false)
	c, _ := responseTestContext(types.RelayFormatOpenAI)
	originalWriter := c.Writer
	staleErr := errors.New("stale response write failure")
	c.Writer = &failingResponseBodyWriter{ResponseWriter: originalWriter, err: staleErr}
	service.IOCopyBytesGracefully(c, nil, []byte("stale"))
	require.ErrorIs(t, service.ResponseBodyWriteError(c), staleErr)
	c.Writer = originalWriter
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(responseFixture(ProtocolChat, false))),
	}
	adaptor := &Adaptor{}
	adaptor.Init(info)

	usage, apiErr := adaptor.DoResponse(c, resp, info)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.NoError(t, service.ResponseBodyWriteError(c))
}
