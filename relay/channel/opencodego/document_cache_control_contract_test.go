package opencodego

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeDocumentDirectCacheControlIsNeverDroppable(t *testing.T) {
	streamStates := []struct {
		name    string
		present bool
		value   bool
	}{
		{name: "absent"},
		{name: "false", present: true},
		{name: "true", present: true, value: true},
	}
	policies := []string{
		dto.OpenCodeGoUnsupportedOptionalFieldStrict,
		dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
	}
	document := map[string]any{
		"type": "document",
		"source": map[string]any{
			"type": "text", "media_type": "text/plain", "data": "synthetic text",
		},
		"cache_control": map[string]any{"type": "ephemeral"},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, finalProtocol := range requestContractProtocols {
			for _, policy := range policies {
				for _, stream := range streamStates {
					name := fmt.Sprintf(
						"type-%d/%s/%s/stream-%s", channelType, finalProtocol, policy, stream.name,
					)
					t.Run(name, func(t *testing.T) {
						extra := map[string]any{
							"messages": []any{map[string]any{
								"role": "user", "content": []any{document},
							}},
						}
						if stream.present {
							extra["stream"] = stream.value
						}
						c, info := newRequestContractFixture(
							t, channelType, requestPreflightEndpoints[0], finalProtocol, extra,
						)
						common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{
							OpenCodeGo: &dto.OpenCodeGoConfig{
								ModelProtocols:                 map[string]string{"glm-5.2": string(finalProtocol)},
								UnsupportedOptionalFieldPolicy: policy,
							},
						})

						_, err := BuildRequestPreflightPlan(c, info)
						require.Error(t, err)
						preflightErr, ok := AsRequestPreflightError(err)
						require.True(t, ok)
						assert.Equal(t, http.StatusBadRequest, preflightErr.StatusCode)
						assert.Equal(t, types.ErrorOriginLocalValidation, preflightErr.Origin)
						assert.Equal(t, CacheControlParentRule, preflightErr.RuleID)
						assert.Equal(t, CacheControlPreflightStage, preflightErr.StageID)
						assert.Equal(t, RequestContractPublicMessage, preflightErr.Message)
						assert.NotContains(t, err.Error(), "synthetic text")
					})
				}
			}
		}
	}
}
