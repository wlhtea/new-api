package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/opencodego"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelayRouterOpenCodeMessagesFinalizerCapturesExactChatJSON(t *testing.T) {
	streamStates := []struct {
		name  string
		value *bool
	}{
		{name: "absent"},
		{name: "false", value: common.GetPointer(false)},
		{name: "true", value: common.GetPointer(true)},
	}
	// This is a successful wire-preservation control, so keep the known output
	// amplifier within a reservable bound. Oversized/non-integral amplifier
	// shapes are covered by the finalized-candidate rejection tests.
	const thinkingRaw = `{ "type":"enabled", "budget_tokens":128 }`
	const stopRaw = `[ "END", "DONE" ]`

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		channelType := channelType
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			setupRelayRouterTestDB(t)
			_, _, channel, workspaceUID := setupRelayRouterOpenCodePreflightFixture(t, channelType)
			capture := installRelayRouterCaptureTransport(t)
			engine := gin.New()
			engine.Use(middleware.RequestId())
			SetRelayRouter(engine)

			for _, stream := range streamStates {
				t.Run("stream-"+stream.name, func(t *testing.T) {
					callsBefore := len(capture.snapshot())
					body := relayRouterMessagesFinalizerBody(thinkingRaw, stopRaw, stream.value, false)
					recorder := serveRelayRouterRequest(engine, "/v1/messages", body)
					require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

					requests := capture.snapshot()
					require.Len(t, requests, callsBefore+1)
					captured := requests[len(requests)-1]
					assert.Equal(t, "/zen/go/v1/chat/completions", captured.path)
					assert.Equal(t, "application/json", captured.header.Get("Content-Type"))
					assert.NotEmpty(t, captured.header.Get("Authorization"))

					outbound := decodeRelayRouterRawObject(t, captured.body)
					assert.JSONEq(t, `"glm-5.2"`, string(outbound["model"]))
					assert.True(t, bytes.Equal([]byte(thinkingRaw), outbound["thinking"]), "thinking changed: %s", outbound["thinking"])
					assert.True(t, bytes.Equal([]byte(stopRaw), outbound["stop"]), "stop changed: %s", outbound["stop"])
					_, hasStopSequences := outbound["stop_sequences"]
					assert.False(t, hasStopSequences)
					streamRaw, streamPresent := outbound["stream"]
					if stream.value == nil {
						assert.False(t, streamPresent)
					} else {
						require.True(t, streamPresent)
						assert.JSONEq(t, fmt.Sprintf("%t", *stream.value), string(streamRaw))
					}
					if channelType == constant.ChannelTypeOpenCodeGo {
						assert.Zero(t, service.OpenCodeGoWorkspaceInFlight(channel.Id, workspaceUID))
					}
				})
			}
		})
	}
}

func TestRelayRouterOpenCodeMessagesStopCollisionHasZeroSideEffects(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		channelType := channelType
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			setupRelayRouterTestDB(t)
			user, token, channel, workspaceUID := setupRelayRouterOpenCodePreflightFixture(t, channelType)
			capture := installRelayRouterCaptureTransport(t)
			var observation relayRouterPreflightObservation
			engine := gin.New()
			engine.Use(middleware.RequestId())
			engine.Use(func(c *gin.Context) {
				c.Next()
				observation.rejection, observation.rejected = opencodego.GetRequestPreflightRejection(c)
			})
			SetRelayRouter(engine)

			before := snapshotRelayRouterPreflightSideEffects(t, user.Id, token.Id, channel.Id, workspaceUID)
			body := relayRouterMessagesFinalizerBody(`null`, `null`, nil, true)
			recorder := serveRelayRouterRequest(engine, "/v1/messages", body)

			require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
			assert.Equal(t, "application/json; charset=utf-8", recorder.Header().Get("Content-Type"))
			assert.NotContains(t, recorder.Body.String(), "Messages stop_sequences and stop cannot both be provided")
			assertFixedRelayRouterInvalidRequest(t, "/v1/messages", recorder.Body.Bytes())
			require.True(t, observation.rejected)
			assert.Equal(t, opencodego.MessagesStopSourceCollisionRule, observation.rejection.RuleID)
			assert.Equal(t, opencodego.RequestContractPreflightStage, observation.rejection.StageID)
			assert.Empty(t, capture.snapshot())
			after := snapshotRelayRouterPreflightSideEffects(t, user.Id, token.Id, channel.Id, workspaceUID)
			assert.Equal(t, before, after)
		})
	}
}

func TestRelayRouterOpenCodeFinalizedSystemPromptSensitiveValueHasZeroCalls(t *testing.T) {
	previousCheckSensitive := setting.CheckSensitiveEnabled
	previousCheckPrompt := setting.CheckSensitiveOnPromptEnabled
	previousWords := append([]string(nil), setting.SensitiveWords...)
	setting.CheckSensitiveEnabled = true
	setting.CheckSensitiveOnPromptEnabled = true
	setting.SensitiveWords = []string{"finalized-system-secret"}
	t.Cleanup(func() {
		setting.CheckSensitiveEnabled = previousCheckSensitive
		setting.CheckSensitiveOnPromptEnabled = previousCheckPrompt
		setting.SensitiveWords = previousWords
	})

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		channelType := channelType
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			setupRelayRouterTestDB(t)
			_, _, channel, workspaceUID := setupRelayRouterOpenCodePreflightFixture(t, channelType)
			channel.SetSetting(dto.ChannelSettings{SystemPrompt: "finalized-system-secret"})
			require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("setting", channel.Setting).Error)
			capture := installRelayRouterCaptureTransport(t)
			engine := gin.New()
			engine.Use(middleware.RequestId())
			SetRelayRouter(engine)

			body := relayRouterMessagesFinalizerBody(`null`, `null`, nil, false)
			recorder := serveRelayRouterRequest(engine, "/v1/messages", body)

			require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
			assert.Equal(t, "application/json; charset=utf-8", recorder.Header().Get("Content-Type"))
			assert.NotContains(t, recorder.Body.String(), "request contains sensitive content")
			assertFixedRelayRouterInvalidRequest(t, "/v1/messages", recorder.Body.Bytes())
			assert.NotContains(t, recorder.Body.String(), "finalized-system-secret")
			assert.Empty(t, capture.snapshot())
			if channelType == constant.ChannelTypeOpenCodeGo {
				assert.Zero(t, service.OpenCodeGoWorkspaceInFlight(channel.Id, workspaceUID))
			}
		})
	}
}

func relayRouterMessagesFinalizerBody(thinkingRaw, stopSequencesRaw string, stream *bool, includeStopFallback bool) []byte {
	body := `{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"hello"}],` +
		`"thinking":` + thinkingRaw + `,"stop_sequences":` + stopSequencesRaw
	if includeStopFallback {
		body += `,"stop":null`
	}
	if stream != nil {
		body += fmt.Sprintf(`,"stream":%t`, *stream)
	}
	return []byte(body + `}`)
}

func decodeRelayRouterRawObject(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	require.NoError(t, common.Unmarshal(data, &object))
	require.NotNil(t, object)
	return object
}
