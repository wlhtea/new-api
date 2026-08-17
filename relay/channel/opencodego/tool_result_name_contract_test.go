package opencodego

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeToolResultNameRequiresTargetRepresentation(t *testing.T) {
	messages := []any{
		map[string]any{
			"role": "assistant",
			"content": []any{map[string]any{
				"type": "tool_use", "id": "call-1", "name": "lookup", "input": map[string]any{},
			}},
		},
		map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": "call-1", "name": "lookup", "content": "done",
			}},
		},
	}
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, finalProtocol := range requestContractProtocols {
			t.Run(fmt.Sprintf("type-%d/%s", channelType, finalProtocol), func(t *testing.T) {
				c, info := newRequestContractFixture(
					t, channelType, requestPreflightEndpoints[0], finalProtocol,
					map[string]any{"messages": messages},
				)

				plan, err := BuildRequestPreflightPlan(c, info)
				if finalProtocol != ProtocolResponses {
					require.NoError(t, err)
					assert.Equal(t, finalProtocol, plan.FinalProtocol)
					return
				}
				require.Error(t, err)
				preflightErr, ok := AsRequestPreflightError(err)
				require.True(t, ok)
				assert.Equal(t, http.StatusBadRequest, preflightErr.StatusCode)
				assert.Equal(t, types.ErrorOriginLocalValidation, preflightErr.Origin)
				assert.Equal(t, RequestContractUnmappedNestedRule, preflightErr.RuleID)
				assert.Equal(t, RequestContractPublicMessage, preflightErr.Message)
				assert.NotContains(t, err.Error(), "lookup")
			})
		}
	}
}
