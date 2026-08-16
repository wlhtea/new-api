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

func TestBoundOpenCodeOutboundPlanRequiresExactSelectedBody(t *testing.T) {
	newFixture := func() (*gin.Context, *relaycommon.RelayInfo) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeAPIKey)
		common.SetContextKey(c, constant.ContextKeyChannelId, 63)
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		info := &relaycommon.RelayInfo{
			UsingGroup: "default",
			ChannelMeta: &relaycommon.ChannelMeta{
				ChannelType:    constant.ChannelTypeOpenCodeAPIKey,
				ChannelId:      63,
				SelectionGroup: "default",
			},
		}
		return c, info
	}

	t.Run("exact body verifies once", func(t *testing.T) {
		c, info := newFixture()
		body := []byte(`{"model":"glm-5.2","stream":false}`)
		require.NoError(t, BindOpenCodeOutboundPlan(c, info, body))
		require.NoError(t, verifyBoundOpenCodeOutboundPlan(c, info, append([]byte(nil), body...)))
		err := verifyBoundOpenCodeOutboundPlan(c, info, body)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), string(body))
	})

	t.Run("one-byte drift fails closed", func(t *testing.T) {
		c, info := newFixture()
		body := []byte(`{"model":"glm-5.2","stream":false}`)
		require.NoError(t, BindOpenCodeOutboundPlan(c, info, body))
		err := verifyBoundOpenCodeOutboundPlan(c, info, []byte(`{"model":"glm-5.2","stream":true}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "differs")
		assert.NotContains(t, err.Error(), "glm-5.2")
	})

	t.Run("selection drift fails closed", func(t *testing.T) {
		c, info := newFixture()
		body := []byte(`{"model":"glm-5.2"}`)
		require.NoError(t, BindOpenCodeOutboundPlan(c, info, body))
		common.SetContextKey(c, constant.ContextKeyChannelId, 64)
		err := verifyBoundOpenCodeOutboundPlan(c, info, body)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "selection changed")
	})
}

func TestRequiredOpenCodeOutboundPlanRejectsMissingBinding(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeAPIKey)
	common.SetContextKey(c, constant.ContextKeyChannelId, 63)
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
		ChannelType:    constant.ChannelTypeOpenCodeAPIKey,
		ChannelId:      63,
		SelectionGroup: "default",
	}}

	require.NoError(t, RequireOpenCodeOutboundPlanBinding(c))
	err := verifyBoundOpenCodeOutboundPlan(c, info, []byte(`{"model":"glm-5.2"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no bound outbound candidate")
}

func TestOpenCodeGoOutboundPlanReplayRearmsExactBodyOnly(t *testing.T) {
	newFixture := func() (*gin.Context, *relaycommon.RelayInfo) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeGo)
		common.SetContextKey(c, constant.ContextKeyChannelId, 62)
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		info := &relaycommon.RelayInfo{
			UsingGroup: "default",
			ChannelMeta: &relaycommon.ChannelMeta{
				ChannelType:    constant.ChannelTypeOpenCodeGo,
				ChannelId:      62,
				SelectionGroup: "default",
			},
		}
		return c, info
	}

	t.Run("same binding verifies both physical calls", func(t *testing.T) {
		c, info := newFixture()
		body := []byte(`{"model":"glm-5.2","stream":false}`)
		require.NoError(t, BindOpenCodeOutboundPlan(c, info, body))
		require.NoError(t, verifyBoundOpenCodeOutboundPlan(c, info, body))
		require.NoError(t, PrepareOpenCodeGoOutboundPlanReplay(c, info))
		require.NoError(t, verifyBoundOpenCodeOutboundPlan(c, info, append([]byte(nil), body...)))
	})

	t.Run("changed replay body fails before transport", func(t *testing.T) {
		c, info := newFixture()
		body := []byte(`{"model":"glm-5.2","stream":false}`)
		require.NoError(t, BindOpenCodeOutboundPlan(c, info, body))
		require.NoError(t, verifyBoundOpenCodeOutboundPlan(c, info, body))
		require.NoError(t, PrepareOpenCodeGoOutboundPlanReplay(c, info))
		err := verifyBoundOpenCodeOutboundPlan(c, info, []byte(`{"model":"glm-5.2","stream":true}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "differs")
		assert.NotContains(t, err.Error(), "glm-5.2")
	})

	t.Run("selection drift cannot rearm binding", func(t *testing.T) {
		c, info := newFixture()
		body := []byte(`{"model":"glm-5.2"}`)
		require.NoError(t, BindOpenCodeOutboundPlan(c, info, body))
		require.NoError(t, verifyBoundOpenCodeOutboundPlan(c, info, body))
		common.SetContextKey(c, constant.ContextKeyChannelId, 63)
		err := PrepareOpenCodeGoOutboundPlanReplay(c, info)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "selection changed")
	})

	t.Run("unverified binding cannot be rearmed", func(t *testing.T) {
		c, info := newFixture()
		require.NoError(t, BindOpenCodeOutboundPlan(c, info, []byte(`{"model":"glm-5.2"}`)))
		err := PrepareOpenCodeGoOutboundPlanReplay(c, info)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "binding state is invalid")
	})

	t.Run("required missing binding fails closed", func(t *testing.T) {
		c, info := newFixture()
		require.NoError(t, RequireOpenCodeOutboundPlanBinding(c))
		err := PrepareOpenCodeGoOutboundPlanReplay(c, info)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no bound candidate")
	})
}
