package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestOpenCodeGoFailuresNeverEnterGenericRetry(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeGo)
	err := types.NewOpenAIError(errors.New("temporary upstream failure"), types.ErrorCodeBadResponseStatusCode, http.StatusInternalServerError)

	assert.False(t, shouldRetry(c, err, 3))
}

func TestOpenCodeGoFailuresNeverDisableWholePoolChannel(t *testing.T) {
	oldEnabled := common.AutomaticDisableChannelEnabled
	common.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() { common.AutomaticDisableChannelEnabled = oldEnabled })
	err := types.NewError(errors.New("invalid account"), types.ErrorCodeChannelInvalidKey)

	assert.True(t, shouldDisableWholeChannel(constant.ChannelTypeOpenAI, err))
	assert.False(t, shouldDisableWholeChannel(constant.ChannelTypeOpenCodeGo, err))
}
