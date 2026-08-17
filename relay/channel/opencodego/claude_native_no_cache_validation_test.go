package opencodego

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeNativeMessagesValidatesNestedContentWithoutCacheMarkers(t *testing.T) {
	tests := []struct {
		name     string
		messages []any
	}{
		{
			name: "assistant document",
			messages: []any{map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "document", "source": map[string]any{
					"type": "text", "media_type": "text/plain", "data": "private-document",
				}},
			}}},
		},
		{
			name: "assistant tool result",
			messages: []any{map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "call-1", "content": "private-result"},
			}}},
		},
		{
			name: "invalid error flag",
			messages: []any{
				map[string]any{"role": "assistant", "content": []any{map[string]any{
					"type": "tool_use", "id": "call-1", "name": "lookup", "input": map[string]any{},
				}}},
				map[string]any{"role": "user", "content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": "call-1", "content": "private-result", "is_error": "yes",
				}}},
			},
		},
	}
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("type-%d/%s", channelType, test.name), func(t *testing.T) {
				c, info := newRequestContractFixture(
					t, channelType, requestPreflightEndpoints[0], ProtocolMessages,
					map[string]any{"messages": test.messages},
				)
				_, err := BuildRequestPreflightPlan(c, info)
				require.Error(t, err)
				preflightErr, ok := AsRequestPreflightError(err)
				require.True(t, ok)
				assert.Equal(t, http.StatusBadRequest, preflightErr.StatusCode)
				assert.Equal(t, RequestContractUnmappedNestedRule, preflightErr.RuleID)
				assert.Equal(t, RequestContractPublicMessage, preflightErr.Message)
				assert.NotContains(t, err.Error(), "private-")
			})
		}
	}
}
