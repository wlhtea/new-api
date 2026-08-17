package controller

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel/opencodego"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeDocumentPreflightReturnsFixedPrivate400Matrix(t *testing.T) {
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
	finalProtocols := []opencodego.Protocol{opencodego.ProtocolChat, opencodego.ProtocolResponses}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, finalProtocol := range finalProtocols {
			for _, policy := range policies {
				for _, stream := range streamStates {
					name := fmt.Sprintf(
						"type-%d/%s/%s/stream-%s", channelType, finalProtocol, policy, stream.name,
					)
					t.Run(name, func(t *testing.T) {
						body := map[string]any{
							"model":      "glm-5.2",
							"max_tokens": 32,
							"messages": []any{map[string]any{
								"role": "user",
								"content": []any{map[string]any{
									"type": "document",
									"source": map[string]any{
										"type": "url", "url": "https://example.invalid/private-document?token=private-marker",
									},
									"title": "private-title", "context": "private-context",
								}},
							}},
						}
						if stream.present {
							body["stream"] = stream.value
						}
						encoded, err := common.Marshal(body)
						require.NoError(t, err)
						c, info, recorder := newOpenCodeControllerPreflightFixture(
							t,
							channelType,
							"/v1/messages",
							types.RelayFormatClaude,
							"glm-5.2",
							"",
							encoded,
						)
						common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{
							OpenCodeGo: &dto.OpenCodeGoConfig{
								ModelProtocols:                 map[string]string{"glm-5.2": string(finalProtocol)},
								UnsupportedOptionalFieldPolicy: policy,
							},
						})

						relayErr := preflightOpenCodeRequest(c, info)
						require.NotNil(t, relayErr)
						assert.Equal(t, http.StatusBadRequest, relayErr.StatusCode)
						assert.Equal(t, types.ErrorOriginLocalValidation, relayErr.Provenance().Origin)
						assert.Equal(t, opencodego.RequestContractUnmappedNestedRule, relayErr.Provenance().Subtype)
						_, planFound, planErr := opencodego.GetRequestPreflightPlan(c)
						require.NoError(t, planErr)
						assert.False(t, planFound)

						renderRelayError(c, types.RelayFormatClaude, nil, relayErr, "document-request-id-private")
						assert.Equal(t, http.StatusBadRequest, recorder.Code)
						response := recorder.Body.String()
						assert.Contains(t, response, constant.OpenCodeGoPublicInvalidRequestMessage)
						assert.Contains(t, response, `"type":"invalid_request_error"`)
						assert.NotContains(t, response, `"code"`)
						for _, privateValue := range []string{
							"private-document", "private-marker", "private-title", "private-context",
							"document-request-id-private", opencodego.RequestContractPublicMessage,
						} {
							assert.NotContains(t, response, privateValue)
						}
					})
				}
			}
		}
	}
}

func TestClaudeToolResultUnsupportedShapeReturnsFixedPrivate400Matrix(t *testing.T) {
	shapes := []struct {
		name    string
		result  map[string]any
		private string
	}{
		{
			name: "image",
			result: map[string]any{
				"type": "tool_result", "tool_use_id": "call-private", "content": []any{map[string]any{
					"type": "image", "source": map[string]any{
						"type": "base64", "media_type": "image/png", "data": "aGVsbG8=",
					},
				}},
			},
			private: "image-private-marker",
		},
		{
			name: "document",
			result: map[string]any{
				"type": "tool_result", "tool_use_id": "call-private", "content": []any{map[string]any{
					"type": "document", "source": map[string]any{
						"type": "text", "media_type": "text/plain", "data": "document-private-marker",
					},
				}},
			},
			private: "document-private-marker",
		},
		{
			name: "is_error",
			result: map[string]any{
				"type": "tool_result", "tool_use_id": "call-private", "content": "failure-private-marker", "is_error": true,
			},
			private: "failure-private-marker",
		},
	}
	streamStates := []struct {
		name    string
		present bool
		value   bool
	}{
		{name: "absent"},
		{name: "false", present: true},
		{name: "true", present: true, value: true},
	}
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, finalProtocol := range []opencodego.Protocol{opencodego.ProtocolChat, opencodego.ProtocolResponses} {
			for _, policy := range []string{
				dto.OpenCodeGoUnsupportedOptionalFieldStrict,
				dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
			} {
				for _, stream := range streamStates {
					for _, shape := range shapes {
						name := fmt.Sprintf("type-%d/%s/%s/stream-%s/%s", channelType, finalProtocol, policy, stream.name, shape.name)
						t.Run(name, func(t *testing.T) {
							body := map[string]any{
								"model": "glm-5.2", "max_tokens": 32,
								"messages": []any{
									map[string]any{"role": "assistant", "content": []any{map[string]any{
										"type": "tool_use", "id": "call-private", "name": "lookup", "input": map[string]any{},
									}}},
									map[string]any{"role": "user", "content": []any{shape.result}},
								},
							}
							if stream.present {
								body["stream"] = stream.value
							}
							encoded, err := common.Marshal(body)
							require.NoError(t, err)
							c, info, recorder := newOpenCodeControllerPreflightFixture(
								t, channelType, "/v1/messages", types.RelayFormatClaude, "glm-5.2", "", encoded,
							)
							common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{
								OpenCodeGo: &dto.OpenCodeGoConfig{
									ModelProtocols:                 map[string]string{"glm-5.2": string(finalProtocol)},
									UnsupportedOptionalFieldPolicy: policy,
								},
							})
							relayErr := preflightOpenCodeRequest(c, info)
							require.NotNil(t, relayErr)
							assert.Equal(t, http.StatusBadRequest, relayErr.StatusCode)
							assert.Equal(t, types.ErrorOriginLocalValidation, relayErr.Provenance().Origin)
							renderRelayError(c, types.RelayFormatClaude, nil, relayErr, "tool-result-request-id-private")
							assert.Equal(t, http.StatusBadRequest, recorder.Code)
							response := recorder.Body.String()
							assert.Contains(t, response, constant.OpenCodeGoPublicInvalidRequestMessage)
							assert.Contains(t, response, `"type":"invalid_request_error"`)
							for _, privateValue := range []string{
								shape.private, "call-private", "tool-result-request-id-private", opencodego.RequestContractPublicMessage,
							} {
								assert.NotContains(t, response, privateValue)
							}
						})
					}
				}
			}
		}
	}
}
