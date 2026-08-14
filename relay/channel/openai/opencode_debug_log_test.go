package openai

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenaiHandlerOmitsOpenCodeErrorBodyFromDebugLog(t *testing.T) {
	previousDebug := common.DebugEnabled
	previousWriter := gin.DefaultErrorWriter
	var logs bytes.Buffer
	common.DebugEnabled = true
	common.LogWriterMu.Lock()
	gin.DefaultErrorWriter = &logs
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.DebugEnabled = previousDebug
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = previousWriter
		common.LogWriterMu.Unlock()
	})

	body := `{"error":{"type":"upstream_error","code":"upstream_error","message":"Authorization: Bearer private-key; x-api-key=private-api-key; endpoint=http://internal-control.local/v1/private"}}`
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenCodeAPIKey},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	usage, apiErr := OpenaiHandler(c, info, resp)

	require.Nil(t, usage)
	require.NotNil(t, apiErr)
	assert.Contains(t, logs.String(), "upstream response body omitted")
	assert.NotContains(t, logs.String(), "OpenCode")
	assert.NotContains(t, logs.String(), "private-key")
	assert.NotContains(t, logs.String(), "private-api-key")
	assert.NotContains(t, logs.String(), "internal-control.local")
}
