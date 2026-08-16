package opencodego

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildRequestPreflightPlanRejectsMessagesStopSourceCollisionForBothChannelTypes(t *testing.T) {
	protocols := []Protocol{ProtocolChat, ProtocolMessages, ProtocolResponses}
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, protocol := range protocols {
			name := fmt.Sprintf("type-%d/%s", channelType, protocol)
			t.Run(name, func(t *testing.T) {
				config := &dto.OpenCodeGoConfig{
					ModelProtocols: map[string]string{"glm-5.2": string(protocol)},
				}
				c, info := newMessagesCollisionPreflightFixture(
					t,
					channelType,
					config,
					`,"stop_sequences":null,"stop":null`,
				)

				_, err := BuildRequestPreflightPlan(c, info)
				require.Error(t, err)
				preflightErr, ok := AsRequestPreflightError(err)
				require.True(t, ok)
				assert.Equal(t, http.StatusBadRequest, preflightErr.StatusCode)
				assert.Equal(t, types.ErrorOriginLocalValidation, preflightErr.Origin)
				assert.Equal(t, MessagesStopSourceCollisionRule, preflightErr.RuleID)
				assert.Equal(t, RequestContractPreflightStage, preflightErr.StageID)
			})
		}
	}
}

func TestBuildRequestPreflightPlanEvaluatesCollisionForEveryType63RetryCandidate(t *testing.T) {
	c, info := newMessagesCollisionPreflightFixture(
		t,
		constant.ChannelTypeOpenCodeAPIKey,
		&dto.OpenCodeGoConfig{ModelProtocols: map[string]string{"glm-5.2": dto.OpenCodeGoProtocolChat}},
		`,"stop_sequences":["END"],"stop":"fallback"`,
	)

	for index, protocol := range []string{dto.OpenCodeGoProtocolChat, dto.OpenCodeGoProtocolResponses} {
		common.SetContextKey(c, constant.ContextKeyChannelId, 6301+index)
		common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{
			OpenCodeGo: &dto.OpenCodeGoConfig{ModelProtocols: map[string]string{"glm-5.2": protocol}},
		})
		_, err := BuildRequestPreflightPlan(c, info)
		require.Error(t, err)
		preflightErr, ok := AsRequestPreflightError(err)
		require.True(t, ok)
		assert.Equal(t, MessagesStopSourceCollisionRule, preflightErr.RuleID)
		assert.Equal(t, RequestContractPreflightStage, preflightErr.StageID)
	}
}

func TestBuildRequestPreflightPlanAcceptsSingleMessagesStopSource(t *testing.T) {
	for _, extraFields := range []string{`,"stop_sequences":null`, `,"stop":null`} {
		c, info := newMessagesCollisionPreflightFixture(
			t,
			constant.ChannelTypeOpenCodeAPIKey,
			&dto.OpenCodeGoConfig{ModelProtocols: map[string]string{"glm-5.2": dto.OpenCodeGoProtocolChat}},
			extraFields,
		)

		plan, err := BuildRequestPreflightPlan(c, info)
		require.NoError(t, err)
		assert.Equal(t, ProtocolChat, plan.FinalProtocol)
	}
}

func newMessagesCollisionPreflightFixture(
	t *testing.T,
	channelType int,
	config *dto.OpenCodeGoConfig,
	extraFields string,
) (*gin.Context, *relaycommon.RelayInfo) {
	t.Helper()
	body := []byte(`{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"hello"}]` + extraFields + `}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	storage, err := common.CreateBodyStorage(body)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })
	c.Set(common.KeyBodyStorage, storage)
	common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
	common.SetContextKey(c, constant.ContextKeyChannelId, 6200+channelType)
	common.SetContextKey(c, constant.ContextKeyChannelModelMapping, "")
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{OpenCodeGo: config})
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "glm-5.2")

	request, err := helper.GetAndValidateRequest(c, types.RelayFormatClaude)
	require.NoError(t, err)
	info, err := relaycommon.GenRelayInfo(c, types.RelayFormatClaude, request, nil)
	require.NoError(t, err)
	return c, info
}
