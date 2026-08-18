package router

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/opencodego"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const relayRouterCachePrivacySentinel = "cache-private-body-sentinel"

func TestRelayRouterOpenCodeCacheControlStrictMatrixHasFixed400AndZeroSideEffects(t *testing.T) {
	clients := []struct {
		name          string
		path          string
		finalProtocol opencodego.Protocol
		wantRule      string
		wantStage     string
	}{
		{
			name:          "messages-registered-marker",
			path:          "/v1/messages",
			finalProtocol: opencodego.ProtocolChat,
			wantRule:      opencodego.CacheControlUnsupportedRule,
			wantStage:     opencodego.CacheControlPreflightStage,
		},
		{
			name:          "chat-same-name-negative-control",
			path:          "/v1/chat/completions",
			finalProtocol: opencodego.ProtocolResponses,
			wantRule:      opencodego.RequestContractUnmappedNestedRule,
			wantStage:     opencodego.RequestContractPreflightStage,
		},
		{
			name:          "responses-same-name-negative-control",
			path:          "/v1/responses",
			finalProtocol: opencodego.ProtocolChat,
			wantRule:      opencodego.RequestContractUnmappedNestedRule,
			wantStage:     opencodego.RequestContractPreflightStage,
		},
	}
	streamStates := []struct {
		name  string
		value *bool
	}{
		{name: "absent"},
		{name: "false", value: common.GetPointer(false)},
		{name: "true", value: common.GetPointer(true)},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		channelType := channelType
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			setupRelayRouterTestDB(t)
			user, token, channel, workspaceUID := setupRelayRouterOpenCodePreflightFixture(t, channelType)
			capture := installRelayRouterProtocolCapture(t)
			var observation relayRouterPreflightObservation
			engine := gin.New()
			engine.Use(middleware.RequestId())
			engine.Use(func(c *gin.Context) {
				c.Next()
				observation.channelType = common.GetContextKeyInt(c, constant.ContextKeyChannelType)
				observation.channelID = common.GetContextKeyInt(c, constant.ContextKeyChannelId)
				observation.rejection, observation.rejected = opencodego.GetRequestPreflightRejection(c)
			})
			SetRelayRouter(engine)

			for _, client := range clients {
				policies := []string{dto.OpenCodeGoUnsupportedOptionalFieldStrict}
				if client.path != "/v1/messages" {
					policies = append(policies, dto.OpenCodeGoUnsupportedOptionalFieldDropKnown)
				}
				for _, policy := range policies {
					setRelayRouterCachePolicy(t, &channel, client.finalProtocol, policy)
					for _, stream := range streamStates {
						name := fmt.Sprintf("%s/%s/stream-%s", client.name, policy, stream.name)
						t.Run(name, func(t *testing.T) {
							before := snapshotRelayRouterPreflightSideEffects(t, user.Id, token.Id, channel.Id, workspaceUID)
							callsBefore := len(capture.snapshot())
							observation = relayRouterPreflightObservation{}

							body := relayRouterCacheControlBody(t, client.path, stream.value)
							recorder := serveRelayRouterRequest(engine, client.path, body)

							require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
							assertFixedRelayRouterInvalidRequest(t, client.path, recorder.Body.Bytes())
							assert.NotContains(t, recorder.Body.String(), relayRouterCachePrivacySentinel)
							assert.Equal(t, channelType, observation.channelType)
							assert.Equal(t, channel.Id, observation.channelID)
							require.True(t, observation.rejected)
							assert.Equal(t, client.wantRule, observation.rejection.RuleID)
							assert.Equal(t, client.wantStage, observation.rejection.StageID)
							assert.Len(t, capture.snapshot(), callsBefore)
							after := snapshotRelayRouterPreflightSideEffects(t, user.Id, token.Id, channel.Id, workspaceUID)
							assert.Equal(t, before, after)
						})
					}
				}
			}
		})
	}
}

func TestRelayRouterOpenCodeCacheControlCompatibilityDropsBeforePhysicalWire(t *testing.T) {
	streamStates := []struct {
		name  string
		value *bool
	}{
		{name: "absent"},
		{name: "false", value: common.GetPointer(false)},
		{name: "true", value: common.GetPointer(true)},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		channelType := channelType
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			setupRelayRouterTestDB(t)
			user, _, channel, workspaceUID := setupRelayRouterOpenCodePreflightFixture(t, channelType)
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", user.Id).Update("quota", 1_000_000_000).Error)
			capture := installRelayRouterProtocolCapture(t)
			var observation relayRouterProtocolObservation
			engine := gin.New()
			engine.Use(middleware.RequestId())
			engine.Use(func(c *gin.Context) {
				c.Next()
				observation.channelType = common.GetContextKeyInt(c, constant.ContextKeyChannelType)
				observation.channelID = common.GetContextKeyInt(c, constant.ContextKeyChannelId)
				observation.plan, observation.planFound, observation.planLookupErr = opencodego.GetRequestPreflightPlan(c)
			})
			SetRelayRouter(engine)

			for _, finalProtocol := range []opencodego.Protocol{opencodego.ProtocolChat, opencodego.ProtocolResponses} {
				setRelayRouterCachePolicy(t, &channel, finalProtocol, dto.OpenCodeGoUnsupportedOptionalFieldDropKnown)
				for _, stream := range streamStates {
					name := fmt.Sprintf("messages-to-%s/stream-%s", finalProtocol, stream.name)
					t.Run(name, func(t *testing.T) {
						callsBefore := len(capture.snapshot())
						observation = relayRouterProtocolObservation{}
						body := relayRouterCacheControlBody(t, "/v1/messages", stream.value)

						recorder := serveRelayRouterRequest(engine, "/v1/messages", body)

						require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
						require.NoError(t, observation.planLookupErr)
						require.True(t, observation.planFound)
						assert.Equal(t, channelType, observation.channelType)
						assert.Equal(t, channel.Id, observation.channelID)
						assert.Equal(t, finalProtocol, observation.plan.FinalProtocol)
						assert.Equal(t, dto.OpenCodeGoUnsupportedOptionalFieldDropKnown, observation.plan.UnsupportedOptionalFieldPolicy)
						assert.Equal(t, 1, observation.plan.CacheControlDropCount)
						assert.Zero(t, observation.plan.CacheControlPreserveCount)

						requests := capture.snapshot()
						require.Len(t, requests, callsBefore+1)
						physical := requests[len(requests)-1]
						assert.NotContains(t, string(physical.body), "cache_control")
						assert.Contains(t, string(physical.body), relayRouterCachePrivacySentinel)
						if finalProtocol == opencodego.ProtocolChat {
							assert.Equal(t, "/zen/go/v1/chat/completions", physical.path)
						} else {
							assert.Equal(t, "/zen/go/v1/responses", physical.path)
						}
						if channelType == constant.ChannelTypeOpenCodeGo {
							assert.Zero(t, serviceOpenCodeWorkspaceInFlight(channel.Id, workspaceUID))
						}
					})
				}
			}
		})
	}
}

func TestRelayRouterOpenCodeCapturedSystemRoleCacheControlMatrix(t *testing.T) {
	streamStates := []struct {
		name  string
		value *bool
	}{
		{name: "absent"},
		{name: "false", value: common.GetPointer(false)},
		{name: "true", value: common.GetPointer(true)},
	}
	protocols := []struct {
		protocol opencodego.Protocol
		path     string
	}{
		{protocol: opencodego.ProtocolMessages, path: "/zen/go/v1/messages"},
		{protocol: opencodego.ProtocolChat, path: "/zen/go/v1/chat/completions"},
		{protocol: opencodego.ProtocolResponses, path: "/zen/go/v1/responses"},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		channelType := channelType
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			setupRelayRouterTestDB(t)
			user, token, channel, workspaceUID := setupRelayRouterOpenCodePreflightFixture(t, channelType)
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", user.Id).
				Update("quota", 1_000_000_000).Error)
			capture := installRelayRouterProtocolCapture(t)
			var observation relayRouterProtocolObservation
			var rejection opencodego.RequestPreflightRejection
			var rejected bool
			engine := gin.New()
			engine.Use(middleware.RequestId())
			engine.Use(func(c *gin.Context) {
				c.Next()
				observation.channelType = common.GetContextKeyInt(c, constant.ContextKeyChannelType)
				observation.channelID = common.GetContextKeyInt(c, constant.ContextKeyChannelId)
				observation.plan, observation.planFound, observation.planLookupErr = opencodego.GetRequestPreflightPlan(c)
				rejection, rejected = opencodego.GetRequestPreflightRejection(c)
			})
			SetRelayRouter(engine)

			for _, target := range protocols {
				for _, policy := range []string{
					dto.OpenCodeGoUnsupportedOptionalFieldStrict,
					dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
				} {
					setRelayRouterCachePolicy(t, &channel, target.protocol, policy)
					for _, stream := range streamStates {
						name := fmt.Sprintf("%s/%s/stream-%s", target.protocol, policy, stream.name)
						t.Run(name, func(t *testing.T) {
							callsBefore := len(capture.snapshot())
							observation = relayRouterProtocolObservation{}
							rejection = opencodego.RequestPreflightRejection{}
							rejected = false
							body := relayRouterCapturedSystemRoleCacheControlBody(t, stream.value)
							unsupportedStrict := target.protocol != opencodego.ProtocolMessages &&
								policy == dto.OpenCodeGoUnsupportedOptionalFieldStrict
							var before relayRouterPreflightSideEffects
							if unsupportedStrict {
								before = snapshotRelayRouterPreflightSideEffects(
									t, user.Id, token.Id, channel.Id, workspaceUID,
								)
							}

							recorder := serveRelayRouterRequest(engine, "/v1/messages", body)

							if unsupportedStrict {
								require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
								assertFixedRelayRouterInvalidRequest(t, "/v1/messages", recorder.Body.Bytes())
								assert.NotContains(t, recorder.Body.String(), relayRouterCachePrivacySentinel)
								require.True(t, rejected)
								assert.Equal(t, opencodego.CacheControlUnsupportedRule, rejection.RuleID)
								assert.Equal(t, opencodego.CacheControlPreflightStage, rejection.StageID)
								assert.Len(t, capture.snapshot(), callsBefore)
								after := snapshotRelayRouterPreflightSideEffects(
									t, user.Id, token.Id, channel.Id, workspaceUID,
								)
								assert.Equal(t, before, after)
								return
							}

							require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
							assert.False(t, rejected)
							require.NoError(t, observation.planLookupErr)
							require.True(t, observation.planFound)
							assert.Equal(t, channelType, observation.channelType)
							assert.Equal(t, channel.Id, observation.channelID)
							assert.Equal(t, target.protocol, observation.plan.FinalProtocol)
							assert.Equal(t, policy, observation.plan.UnsupportedOptionalFieldPolicy)

							requests := capture.snapshot()
							require.Len(t, requests, callsBefore+1)
							physical := requests[len(requests)-1]
							assert.Equal(t, target.path, physical.path)
							assert.Contains(t, string(physical.body), relayRouterCachePrivacySentinel)
							if target.protocol == opencodego.ProtocolMessages {
								assert.Equal(t, 3, observation.plan.CacheControlPreserveCount)
								assert.Zero(t, observation.plan.CacheControlDropCount)
								assertRelayRouterCapturedSystemMarkers(t, physical.body)
							} else {
								assert.Zero(t, observation.plan.CacheControlPreserveCount)
								assert.Equal(t, 3, observation.plan.CacheControlDropCount)
								assert.NotContains(t, string(physical.body), "cache_control")
							}
							if channelType == constant.ChannelTypeOpenCodeGo {
								assert.Zero(t, serviceOpenCodeWorkspaceInFlight(channel.Id, workspaceUID))
							}
						})
					}
				}
			}
		})
	}
}

func TestRelayRouterOpenCodeMalformedCacheControlFailsClosedInBothPolicies(t *testing.T) {
	streamStates := []struct {
		name  string
		value *bool
	}{
		{name: "absent"},
		{name: "false", value: common.GetPointer(false)},
		{name: "true", value: common.GetPointer(true)},
	}
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		channelType := channelType
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			setupRelayRouterTestDB(t)
			user, token, channel, workspaceUID := setupRelayRouterOpenCodePreflightFixture(t, channelType)
			capture := installRelayRouterProtocolCapture(t)
			engine := gin.New()
			engine.Use(middleware.RequestId())
			SetRelayRouter(engine)

			for _, policy := range []string{
				dto.OpenCodeGoUnsupportedOptionalFieldStrict,
				dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
			} {
				setRelayRouterCachePolicy(t, &channel, opencodego.ProtocolChat, policy)
				for _, stream := range streamStates {
					name := policy + "/stream-" + stream.name
					t.Run(name, func(t *testing.T) {
						before := snapshotRelayRouterPreflightSideEffects(t, user.Id, token.Id, channel.Id, workspaceUID)
						callsBefore := len(capture.snapshot())
						body := relayRouterMalformedCacheControlBody(t, stream.value)

						recorder := serveRelayRouterRequest(engine, "/v1/messages", body)

						require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
						assertFixedRelayRouterInvalidRequest(t, "/v1/messages", recorder.Body.Bytes())
						assert.NotContains(t, recorder.Body.String(), relayRouterCachePrivacySentinel)
						assert.Len(t, capture.snapshot(), callsBefore)
						after := snapshotRelayRouterPreflightSideEffects(t, user.Id, token.Id, channel.Id, workspaceUID)
						assert.Equal(t, before, after)
					})
				}
			}
		})
	}
}

func setRelayRouterCachePolicy(t *testing.T, channel *model.Channel, protocol opencodego.Protocol, policy string) {
	t.Helper()
	channel.SetOtherSettings(dto.ChannelOtherSettings{OpenCodeGo: &dto.OpenCodeGoConfig{
		ModelProtocols:                 map[string]string{"glm-5.2": string(protocol)},
		UnsupportedOptionalFieldPolicy: policy,
	}})
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).
		Update("settings", channel.OtherSettings).Error)
}

func relayRouterCacheControlBody(t *testing.T, path string, stream *bool) []byte {
	t.Helper()
	marker := map[string]any{"type": "ephemeral"}
	body := map[string]any{"model": "glm-5.2"}
	switch path {
	case "/v1/messages":
		body["max_tokens"] = 16
		body["system"] = []any{map[string]any{
			"type": "text", "text": relayRouterCachePrivacySentinel, "cache_control": marker,
		}}
		body["messages"] = []any{map[string]any{"role": "user", "content": relayRouterCachePrivacySentinel}}
	case "/v1/chat/completions":
		body["messages"] = []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "text", "text": relayRouterCachePrivacySentinel, "cache_control": marker,
			}},
		}}
	case "/v1/responses":
		body["input"] = []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{
				"type": "input_text", "text": relayRouterCachePrivacySentinel, "cache_control": marker,
			}},
		}}
	default:
		t.Fatalf("unsupported relay path %q", path)
	}
	if stream != nil {
		body["stream"] = *stream
	}
	encoded, err := common.Marshal(body)
	require.NoError(t, err)
	return encoded
}

func relayRouterMalformedCacheControlBody(t *testing.T, stream *bool) []byte {
	t.Helper()
	body := map[string]any{
		"model":      "glm-5.2",
		"max_tokens": 16,
		"system": []any{map[string]any{
			"type": "text", "text": relayRouterCachePrivacySentinel,
			"cache_control": map[string]any{"type": "ephemeral", "ttl": "private-invalid-ttl"},
		}},
		"messages": []any{map[string]any{"role": "user", "content": relayRouterCachePrivacySentinel}},
	}
	if stream != nil {
		body["stream"] = *stream
	}
	encoded, err := common.Marshal(body)
	require.NoError(t, err)
	return encoded
}

func relayRouterCapturedSystemRoleCacheControlBody(t *testing.T, stream *bool) []byte {
	t.Helper()
	marker := func() map[string]any {
		return map[string]any{"type": "ephemeral"}
	}
	body := map[string]any{
		"model":      "glm-5.2",
		"max_tokens": 16,
		"system": []any{
			map[string]any{"type": "text", "text": "unmarked system"},
			map[string]any{
				"type": "text", "text": "top-level system one", "cache_control": marker(),
			},
			map[string]any{
				"type": "text", "text": "top-level system two", "cache_control": marker(),
			},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": relayRouterCachePrivacySentinel},
			map[string]any{
				"role": "system",
				"content": []any{map[string]any{
					"type": "text", "text": "message-level system", "cache_control": marker(),
				}},
			},
		},
	}
	if stream != nil {
		body["stream"] = *stream
	}
	encoded, err := common.Marshal(body)
	require.NoError(t, err)
	return encoded
}

func assertRelayRouterCapturedSystemMarkers(t *testing.T, body []byte) {
	t.Helper()
	var outbound map[string]any
	require.NoError(t, common.Unmarshal(body, &outbound))
	system, ok := outbound["system"].([]any)
	require.True(t, ok)
	require.Len(t, system, 4)
	for _, index := range []int{1, 2, 3} {
		part, ok := system[index].(map[string]any)
		require.True(t, ok)
		marker, present := part["cache_control"].(map[string]any)
		require.True(t, present)
		assert.Equal(t, "ephemeral", marker["type"])
	}
	messages, ok := outbound["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 1)
	assert.Equal(t, "user", messages[0].(map[string]any)["role"])
}

func serviceOpenCodeWorkspaceInFlight(channelID int, workspaceUID string) int64 {
	if strings.TrimSpace(workspaceUID) == "" {
		return 0
	}
	return service.OpenCodeGoWorkspaceInFlight(channelID, workspaceUID)
}
