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

func serviceOpenCodeWorkspaceInFlight(channelID int, workspaceUID string) int64 {
	if strings.TrimSpace(workspaceUID) == "" {
		return 0
	}
	return service.OpenCodeGoWorkspaceInFlight(channelID, workspaceUID)
}
