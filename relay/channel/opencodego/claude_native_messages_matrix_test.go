package opencodego

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeNativeMessagesMatrixPreservesValidatedDocumentAndToolResultWire(t *testing.T) {
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
	fixtures := []struct {
		name          string
		content       map[string]any
		wantNested    bool
		wantErrorFlag bool
	}{
		{
			name: "document base64 pdf",
			content: map[string]any{
				"type": "document", "source": map[string]any{
					"type": "base64", "media_type": "application/pdf", "data": "AA==",
				},
				"citations": map[string]any{"enabled": true},
				"context":   "native-context", "title": "native-title",
			},
		},
		{
			name: "document plain text",
			content: map[string]any{
				"type": "document", "source": map[string]any{
					"type": "text", "media_type": "text/plain", "data": "native document",
				},
			},
		},
		{
			name: "document url",
			content: map[string]any{
				"type": "document", "source": map[string]any{
					"type": "url", "url": "https://example.invalid/native-document.pdf",
				},
			},
		},
		{
			name: "tool result text",
			content: map[string]any{
				"type": "tool_result", "tool_use_id": "native-call-1", "content": []any{
					map[string]any{"type": "text", "text": "native result"},
				},
			},
			wantNested: true,
		},
		{
			name: "tool result image",
			content: map[string]any{
				"type": "tool_result", "tool_use_id": "native-call-1", "content": []any{
					map[string]any{"type": "image", "source": map[string]any{
						"type": "base64", "media_type": "image/png", "data": "aGVsbG8=",
					}},
				},
			},
			wantNested: true,
		},
		{
			name: "tool result document",
			content: map[string]any{
				"type": "tool_result", "tool_use_id": "native-call-1", "content": []any{
					map[string]any{"type": "document", "source": map[string]any{
						"type": "text", "media_type": "text/plain", "data": "nested native document",
					}},
				},
			},
			wantNested: true,
		},
		{
			name: "tool result is error",
			content: map[string]any{
				"type": "tool_result", "tool_use_id": "native-call-1", "content": "native failure", "is_error": true,
			},
			wantErrorFlag: true,
		},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, policy := range policies {
			for _, stream := range streamStates {
				for _, fixture := range fixtures {
					name := fmt.Sprintf("type-%d/%s/stream-%s/%s", channelType, policy, stream.name, fixture.name)
					t.Run(name, func(t *testing.T) {
						messages := []any{map[string]any{
							"role": "user", "content": []any{fixture.content},
						}}
						if fixture.wantNested || fixture.wantErrorFlag {
							messages = []any{
								map[string]any{"role": "assistant", "content": []any{map[string]any{
									"type": "tool_use", "id": "native-call-1", "name": "lookup", "input": map[string]any{},
								}}},
								messages[0],
							}
						}
						extra := map[string]any{"messages": messages}
						if stream.present {
							extra["stream"] = stream.value
						}
						c, info := newRequestContractFixture(
							t, channelType, requestPreflightEndpoints[0], ProtocolMessages, extra,
						)
						setClaudeReplayPolicy(c, ProtocolMessages, policy)
						wire := convertAndFinalizeRequestForPresenceTest(t, c, info)

						var root map[string]any
						require.NoError(t, common.Unmarshal(wire, &root))
						actualStream, actualStreamPresent := root["stream"]
						assert.Equal(t, stream.present, actualStreamPresent)
						if stream.present {
							assert.Equal(t, stream.value, actualStream)
						}
						gotMessages, ok := root["messages"].([]any)
						require.True(t, ok)
						if fixture.wantNested || fixture.wantErrorFlag {
							require.Len(t, gotMessages, 2)
							resultMessage := gotMessages[1].(map[string]any)
							gotContent := resultMessage["content"].([]any)[0].(map[string]any)
							assert.Equal(t, fixture.content, gotContent)
							if fixture.wantErrorFlag {
								assert.Equal(t, true, gotContent["is_error"])
							}
						} else {
							require.Len(t, gotMessages, 1)
							gotContent := gotMessages[0].(map[string]any)["content"].([]any)[0].(map[string]any)
							assert.Equal(t, fixture.content, gotContent)
						}
					})
				}
			}
		}
	}
}

func TestClaudeNativeMessagesMatrixPreservesAssistantTextThinkingAndToolUse(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, policy := range []string{
			dto.OpenCodeGoUnsupportedOptionalFieldStrict,
			dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
		} {
			name := fmt.Sprintf("type-%d/%s", channelType, policy)
			t.Run(name, func(t *testing.T) {
				extra := map[string]any{"messages": []any{
					map[string]any{"role": "user", "content": "read the reference"},
					map[string]any{"role": "assistant", "content": []any{
						map[string]any{"type": "thinking", "thinking": "inspect", "signature": "native-signature"},
						map[string]any{"type": "text", "text": "I will inspect it."},
						map[string]any{"type": "tool_use", "id": "native-call-2", "name": "Bash", "input": map[string]any{
							"command": "cat -- <native-reference>",
						}},
					}},
				}}
				c, info := newRequestContractFixture(
					t, channelType, requestPreflightEndpoints[0], ProtocolMessages, extra,
				)
				setClaudeReplayPolicy(c, ProtocolMessages, policy)
				wire := convertAndFinalizeRequestForPresenceTest(t, c, info)
				var root map[string]any
				require.NoError(t, common.Unmarshal(wire, &root))
				messages := root["messages"].([]any)
				require.Len(t, messages, 2)
				assistant := messages[1].(map[string]any)
				content := assistant["content"].([]any)
				require.Len(t, content, 3)
				assert.Equal(t, "thinking", content[0].(map[string]any)["type"])
				assert.Equal(t, "text", content[1].(map[string]any)["type"])
				toolUse := content[2].(map[string]any)
				assert.Equal(t, "tool_use", toolUse["type"])
				assert.Equal(t, "native-call-2", toolUse["id"])
			})
		}
	}
}
