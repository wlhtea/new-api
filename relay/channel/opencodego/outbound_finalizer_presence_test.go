package opencodego

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFinalizeCrossProtocolPreservesStreamPresence(t *testing.T) {
	tests := []struct {
		name          string
		endpoint      requestPreflightEndpoint
		finalProtocol Protocol
	}{
		{name: "messages-to-chat", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolChat},
		{name: "messages-to-responses", endpoint: requestPreflightEndpoints[0], finalProtocol: ProtocolResponses},
		{name: "chat-to-messages", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolMessages},
		{name: "chat-to-responses", endpoint: requestPreflightEndpoints[1], finalProtocol: ProtocolResponses},
		{name: "responses-to-chat", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolChat},
		{name: "responses-to-messages", endpoint: requestPreflightEndpoints[2], finalProtocol: ProtocolMessages},
	}
	streamStates := []struct {
		name    string
		value   *bool
		present bool
	}{
		{name: "absent"},
		{name: "false", value: common.GetPointer(false), present: true},
		{name: "true", value: common.GetPointer(true), present: true},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, test := range tests {
			for _, streamState := range streamStates {
				name := fmt.Sprintf("type-%d/%s/%s", channelType, test.name, streamState.name)
				t.Run(name, func(t *testing.T) {
					extraFields := make(map[string]any)
					if streamState.value != nil {
						extraFields["stream"] = *streamState.value
					}
					c, info := newRequestContractFixture(
						t,
						channelType,
						test.endpoint,
						test.finalProtocol,
						extraFields,
					)

					result := convertAndFinalizeRequestForPresenceTest(t, c, info)
					object := decodeFinalizerTestObject(t, result)
					stream, present := object["stream"]
					assert.Equal(t, streamState.present, present)
					if streamState.present {
						assert.JSONEq(t, fmt.Sprintf("%t", *streamState.value), string(stream))
					}
				})
			}
		}
	}
}

func convertAndFinalizeRequestForPresenceTest(
	t *testing.T,
	c *gin.Context,
	info *relaycommon.RelayInfo,
) []byte {
	t.Helper()
	plan, err := BuildRequestPreflightPlan(c, info)
	require.NoError(t, err)
	require.NoError(t, StoreRequestPreflightPlan(c, plan))
	info.InitChannelMeta(c)
	info.UpstreamModelName = plan.FinalModel
	info.IsModelMapped = plan.ModelMapped

	adaptor := &Adaptor{}
	var converted any
	switch request := info.Request.(type) {
	case *dto.ClaudeRequest:
		converted, err = adaptor.ConvertClaudeRequest(c, info, request)
	case *dto.GeneralOpenAIRequest:
		converted, err = adaptor.ConvertOpenAIRequest(c, info, request)
	case *dto.OpenAIResponsesRequest:
		converted, err = adaptor.ConvertOpenAIResponsesRequest(c, info, *request)
	default:
		t.Fatalf("unsupported request type %T", info.Request)
	}
	require.NoError(t, err)
	require.Equal(t, plan.FinalProtocol.RelayFormat(), info.FinalRequestRelayFormat)

	result, err := adaptor.FinalizeOutboundRequest(c, info, converted)
	require.NoError(t, err)
	return result
}
