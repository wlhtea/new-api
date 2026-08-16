package router

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

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

func TestRelayRouterOpenCodeClaudeBetaQueryCompatibility(t *testing.T) {
	protocols := []struct {
		name     string
		protocol opencodego.Protocol
		path     string
	}{
		{name: "chat", protocol: opencodego.ProtocolChat, path: "/zen/go/v1/chat/completions"},
		{name: "messages", protocol: opencodego.ProtocolMessages, path: "/zen/go/v1/messages"},
		{name: "responses", protocol: opencodego.ProtocolResponses, path: "/zen/go/v1/responses"},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		channelType := channelType
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			setupRelayRouterTestDB(t)
			user, _, channel, workspaceUID := setupRelayRouterOpenCodePreflightFixture(t, channelType)
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", user.Id).Update("quota", 1_000_000_000).Error)
			capture := installRelayRouterProtocolCapture(t)

			engine := gin.New()
			engine.Use(middleware.RequestId())
			SetRelayRouter(engine)

			for _, test := range protocols {
				t.Run(test.name, func(t *testing.T) {
					channel.SetOtherSettings(dto.ChannelOtherSettings{OpenCodeGo: &dto.OpenCodeGoConfig{
						ModelProtocols: map[string]string{"glm-5.2": string(test.protocol)},
					}})
					require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).
						Update("settings", channel.OtherSettings).Error)

					callsBefore := len(capture.snapshot())
					recorder := serveRelayRouterRequest(
						engine,
						"/v1/messages?beta=true",
						relayRouterProtocolMatrixRequest(t, "/v1/messages", false),
					)
					require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

					requests := capture.snapshot()
					require.Len(t, requests, callsBefore+1)
					physical := requests[len(requests)-1]
					assert.Equal(t, test.path, physical.path)
					assert.Empty(t, physical.rawQuery, "client beta marker must be consumed before OpenCode upstream I/O")
					assert.NotContains(t, physical.path, "beta")
					if channelType == constant.ChannelTypeOpenCodeGo {
						assert.Zero(t, service.OpenCodeGoWorkspaceInFlight(channel.Id, workspaceUID))
					}
				})
			}
		})
	}
}

func TestRelayRouterOpenCodeClaudeBetaQueryRejectsUnsupportedVariants(t *testing.T) {
	variants := []string{
		"/v1/messages?beta=false",
		"/v1/messages?beta=TRUE",
		"/v1/messages?beta=",
		"/v1/messages?beta=true&beta=true",
		"/v1/messages?beta=true&",
		"/v1/messages?&beta=true",
		"/v1/messages?beta=%74rue",
		"/v1/messages?api_key=client-secret",
		"/v1/messages?beta=true&client=private-value",
		"/v1/messages?beta=%ZZ",
		"/v1/chat/completions?beta=true",
		"/v1/responses?beta=true",
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		channelType := channelType
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			setupRelayRouterTestDB(t)
			user, token, channel, workspaceUID := setupRelayRouterOpenCodePreflightFixture(t, channelType)
			capture := installRelayRouterCaptureTransport(t)

			engine := gin.New()
			engine.Use(middleware.RequestId())
			SetRelayRouter(engine)

			for _, path := range variants {
				t.Run(path, func(t *testing.T) {
					basePath := strings.Split(path, "?")[0]
					before := snapshotRelayRouterPreflightSideEffects(t, user.Id, token.Id, channel.Id, workspaceUID)
					callsBefore := len(capture.snapshot())
					recorder := serveRelayRouterRequest(
						engine,
						path,
						relayRouterProtocolMatrixRequest(t, basePath, false),
					)

					require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
					assertFixedRelayRouterInvalidRequest(t, basePath, recorder.Body.Bytes())
					assert.NotContains(t, recorder.Body.String(), "client-secret")
					assert.Equal(t, callsBefore, len(capture.snapshot()))
					after := snapshotRelayRouterPreflightSideEffects(t, user.Id, token.Id, channel.Id, workspaceUID)
					assert.Equal(t, before, after)
				})
			}
		})
	}
}
