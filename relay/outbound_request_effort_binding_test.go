package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newOpenCodeEffortBindingFixture(
	channelType int,
) (*gin.Context, *relaycommon.RelayInfo) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
	common.SetContextKey(c, constant.ContextKeyChannelId, 9000+channelType)
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	info := &relaycommon.RelayInfo{
		UsingGroup: "default",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:    channelType,
			ChannelId:      9000 + channelType,
			SelectionGroup: "default",
		},
	}
	return c, info
}

func TestBoundOpenCodeOutboundPlanRequiresExactBodyAndEffortPair(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-flash","reasoning_effort":"high"}`)

	t.Run("effort mismatch", func(t *testing.T) {
		c, info := newOpenCodeEffortBindingFixture(constant.ChannelTypeOpenCodeAPIKey)
		require.NoError(t, BindOpenCodeOutboundPlanWithEffort(c, info, body, "high"))
		info.SetReasoningEffort("low")

		err := verifyBoundOpenCodeOutboundPlan(c, info, append([]byte(nil), body...))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "effort differs")
		assert.NotContains(t, err.Error(), "high")
		assert.NotContains(t, err.Error(), "low")
	})

	t.Run("body mismatch with matching effort", func(t *testing.T) {
		c, info := newOpenCodeEffortBindingFixture(constant.ChannelTypeOpenCodeAPIKey)
		require.NoError(t, BindOpenCodeOutboundPlanWithEffort(c, info, body, "high"))

		err := verifyBoundOpenCodeOutboundPlan(
			c,
			info,
			[]byte(`{"model":"deepseek-v4-flash","reasoning_effort":"low"}`),
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "body differs")
		assert.NotContains(t, err.Error(), "deepseek-v4-flash")
	})
}

func TestOpenCodePhysicalRetryRestoresFinalizedEffort(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-flash","reasoning_effort":"high"}`)

	t.Run("type 62 replay", func(t *testing.T) {
		c, info := newOpenCodeEffortBindingFixture(constant.ChannelTypeOpenCodeGo)
		require.NoError(t, BindOpenCodeOutboundPlanWithEffort(c, info, body, "high"))
		require.NoError(t, verifyBoundOpenCodeOutboundPlan(c, info, body))

		info.SetReasoningEffort("low")
		require.NoError(t, PrepareOpenCodeGoOutboundPlanReplay(c, info))
		assert.Equal(t, "high", info.GetReasoningEffort())
		require.NoError(t, verifyBoundOpenCodeOutboundPlan(c, info, append([]byte(nil), body...)))
	})

	t.Run("type 63 rebind", func(t *testing.T) {
		c, info := newOpenCodeEffortBindingFixture(constant.ChannelTypeOpenCodeAPIKey)
		require.NoError(t, BindOpenCodeOutboundPlanWithEffort(c, info, body, "high"))
		require.NoError(t, verifyBoundOpenCodeOutboundPlan(c, info, body))

		info.SetReasoningEffort("low")
		require.NoError(t, BindOpenCodeOutboundPlanWithEffort(c, info, body, "high"))
		assert.Equal(t, "high", info.GetReasoningEffort())
		require.NoError(t, verifyBoundOpenCodeOutboundPlan(c, info, append([]byte(nil), body...)))
	})
}
