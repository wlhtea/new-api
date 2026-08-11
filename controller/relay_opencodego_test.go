package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

type postResponseSettlementBilling struct {
	refundCalls int
}

func (*postResponseSettlementBilling) Settle(int) error { return nil }

func (b *postResponseSettlementBilling) Refund(*gin.Context) { b.refundCalls++ }

func (*postResponseSettlementBilling) NeedsRefund() bool { return true }

func (*postResponseSettlementBilling) GetPreConsumedQuota() int { return 50 }

func (*postResponseSettlementBilling) Reserve(int) error { return nil }

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

func TestPostResponseSettlementFailureRefundsWithoutRetryDisableOrResponseAppend(t *testing.T) {
	oldEnabled := common.AutomaticDisableChannelEnabled
	common.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() { common.AutomaticDisableChannelEnabled = oldEnabled })

	apiErr := types.NewErrorWithStatusCode(
		errors.New("forced funding settlement failure"),
		types.ErrorCodeBillingSettlementFailed,
		http.StatusInternalServerError,
		types.ErrOptionWithSkipRetry(),
	)

	for _, test := range []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "json", contentType: "application/json", body: `{"id":"completed"}`},
		{name: "sse", contentType: "text/event-stream", body: "data: [DONE]\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			c.Data(http.StatusOK, test.contentType, []byte(test.body))

			renderRelayError(c, types.RelayFormatOpenAI, nil, apiErr, "request-id")

			assert.Equal(t, http.StatusOK, recorder.Code)
			assert.Equal(t, test.body, recorder.Body.String())
			assert.NotContains(t, recorder.Body.String(), `"error"`)
		})
	}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	assert.False(t, shouldRetry(c, apiErr, 3))
	assert.False(t, shouldDisableWholeChannel(constant.ChannelTypeOpenAI, apiErr))

	billing := &postResponseSettlementBilling{}
	refundRelayBilling(c, &relaycommon.RelayInfo{Billing: billing})
	assert.Equal(t, 1, billing.refundCalls)
}
