package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"

	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelayTaskFetchUnknownModeReturnsErrorWithoutCallingBuilder(t *testing.T) {
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodGet, "/unknown/task/fetch", nil)

	taskErr := RelayTaskFetch(c, relayconstant.RelayModeUnknown)

	require.NotNil(t, taskErr)
	assert.Equal(t, "invalid_relay_mode", taskErr.Code)
	assert.Equal(t, http.StatusBadRequest, taskErr.StatusCode)
	assert.True(t, taskErr.LocalError)
	assert.Empty(t, writer.Body.String())
}
