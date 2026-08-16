package controller

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/opencodego"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type openCodeRetryCapturedAttempt struct {
	channelID int
	path      string
	header    http.Header
	body      []byte
}

type openCodeRetrySnapshotTransport struct {
	mutex         sync.Mutex
	db            *gorm.DB
	channelAID    int
	channelBID    int
	mutatedOther  string
	mutatedHeader string
	mutationErr   error
	captured      []openCodeRetryCapturedAttempt
}

func (transport *openCodeRetrySnapshotTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	if err := request.Body.Close(); err != nil {
		return nil, err
	}

	channelID := 0
	switch request.Header.Get("Authorization") {
	case "Bearer retry-snapshot-key-a":
		channelID = transport.channelAID
	case "Bearer retry-snapshot-key-b":
		channelID = transport.channelBID
	}
	transport.mutex.Lock()
	transport.captured = append(transport.captured, openCodeRetryCapturedAttempt{
		channelID: channelID,
		path:      request.URL.Path,
		header:    request.Header.Clone(),
		body:      append([]byte(nil), body...),
	})
	transport.mutex.Unlock()

	if channelID == transport.channelAID {
		transport.mutex.Lock()
		transport.mutationErr = transport.db.Model(&model.Channel{}).
			Where("id = ?", transport.channelBID).
			Updates(map[string]interface{}{
				"key":             "retry-snapshot-mutated-key-b",
				"settings":        transport.mutatedOther,
				"header_override": transport.mutatedHeader,
			}).Error
		transport.mutex.Unlock()
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Status:     "503 Service Unavailable",
			Header: http.Header{
				"Content-Type":         []string{"application/json"},
				"Retry-After":          []string{"11"},
				"X-Upstream-Attempt-A": []string{"private-a"},
			},
			Body:    io.NopCloser(strings.NewReader(`{"error":{"message":"temporary private failure","type":"server_error","code":"server_error"}}`)),
			Request: request,
		}, nil
	}

	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header: http.Header{
			"Content-Type":         []string{"application/json"},
			"X-Upstream-Attempt-B": []string{"private-b"},
		},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl-snapshot-b","object":"chat.completion","created":1,"model":"retry-snapshot-model","choices":[{"index":0,"message":{"role":"assistant","content":"from-b"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		)),
		Request: request,
	}, nil
}

func (transport *openCodeRetrySnapshotTransport) snapshot() ([]openCodeRetryCapturedAttempt, error) {
	transport.mutex.Lock()
	defer transport.mutex.Unlock()
	captured := make([]openCodeRetryCapturedAttempt, len(transport.captured))
	copy(captured, transport.captured)
	return captured, transport.mutationErr
}

// This is deterministic E1/E2 evidence only: it proves controller routing and
// the exact mock wire requests, not real-provider model capability.
func TestOpenCodeAPIKeyControllerRetryUsesFrozenPhysicalScheduleE1E2(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Token{}, &model.Log{}, &model.UserSubscription{}))

	originalMemoryCache := common.MemoryCacheEnabled
	originalRetryTimes := common.RetryTimes
	originalAutoDisable := common.AutomaticDisableChannelEnabled
	originalRetryRanges := append([]operation_setting.StatusCodeRange(nil), operation_setting.AutomaticRetryStatusCodeRanges...)
	originalRatios := ratio_setting.ModelRatio2JSONString()
	common.MemoryCacheEnabled = false
	common.RetryTimes = 1
	common.AutomaticDisableChannelEnabled = false
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{{
		Start: http.StatusServiceUnavailable,
		End:   http.StatusServiceUnavailable,
	}}
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"retry-snapshot-model":0}`))
	t.Cleanup(func() {
		common.MemoryCacheEnabled = originalMemoryCache
		common.RetryTimes = originalRetryTimes
		common.AutomaticDisableChannelEnabled = originalAutoDisable
		operation_setting.AutomaticRetryStatusCodeRanges = originalRetryRanges
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalRatios))
	})

	user := model.User{
		Username: "retry-snapshot-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    1000,
	}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{
		UserId:         user.Id,
		Key:            "retry-snapshot-client-token",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}
	require.NoError(t, db.Create(&token).Error)

	highPriority := int64(20)
	lowPriority := int64(10)
	lowerPriority := int64(5)
	autoBan := 0
	headerA := `{"X-Snapshot-Channel":"a"}`
	headerB := `{"X-Snapshot-Channel":"b-frozen"}`
	channelA := model.Channel{
		Type:           constant.ChannelTypeOpenCodeAPIKey,
		Key:            "retry-snapshot-key-a",
		Status:         common.ChannelStatusEnabled,
		Name:           "retry-snapshot-a",
		Models:         "retry-snapshot-model",
		Group:          "default",
		Priority:       &highPriority,
		AutoBan:        &autoBan,
		HeaderOverride: &headerA,
	}
	channelA.SetOtherSettings(dto.ChannelOtherSettings{OpenCodeGo: &dto.OpenCodeGoConfig{
		ModelProtocols: map[string]string{"retry-snapshot-model": dto.OpenCodeGoProtocolResponses},
	}})
	require.NoError(t, db.Create(&channelA).Error)
	channelB := model.Channel{
		Type:           constant.ChannelTypeOpenCodeAPIKey,
		Key:            "retry-snapshot-key-b",
		Status:         common.ChannelStatusEnabled,
		Name:           "retry-snapshot-b",
		Models:         "retry-snapshot-model",
		Group:          "default",
		Priority:       &lowPriority,
		AutoBan:        &autoBan,
		HeaderOverride: &headerB,
	}
	channelB.SetOtherSettings(dto.ChannelOtherSettings{OpenCodeGo: &dto.OpenCodeGoConfig{
		ModelProtocols: map[string]string{"retry-snapshot-model": dto.OpenCodeGoProtocolChat},
	}})
	require.NoError(t, db.Create(&channelB).Error)
	channelC := model.Channel{
		Type:     constant.ChannelTypeOpenCodeAPIKey,
		Key:      "retry-snapshot-key-c",
		Status:   common.ChannelStatusEnabled,
		Name:     "retry-snapshot-c",
		Models:   "retry-snapshot-model",
		Group:    "default",
		Priority: &lowerPriority,
		AutoBan:  &autoBan,
	}
	channelC.SetOtherSettings(dto.ChannelOtherSettings{OpenCodeGo: &dto.OpenCodeGoConfig{
		ModelProtocols: map[string]string{"retry-snapshot-model": dto.OpenCodeGoProtocolMessages},
	}})
	require.NoError(t, db.Create(&channelC).Error)
	for _, ability := range []model.Ability{
		{Group: "default", Model: "retry-snapshot-model", ChannelId: channelA.Id, Enabled: true, Priority: &highPriority},
		{Group: "default", Model: "retry-snapshot-model", ChannelId: channelB.Id, Enabled: true, Priority: &lowPriority},
		{Group: "default", Model: "retry-snapshot-model", ChannelId: channelC.Id, Enabled: true, Priority: &lowerPriority},
	} {
		require.NoError(t, db.Create(&ability).Error)
	}

	mutatedHeader := `{"X-Snapshot-Channel":"b-mutated"}`
	mutatedOtherBytes, err := common.Marshal(dto.ChannelOtherSettings{OpenCodeGo: &dto.OpenCodeGoConfig{
		ModelProtocols: map[string]string{"retry-snapshot-model": dto.OpenCodeGoProtocolMessages},
	}})
	require.NoError(t, err)
	transport := &openCodeRetrySnapshotTransport{
		db:            db,
		channelAID:    channelA.Id,
		channelBID:    channelB.Id,
		mutatedOther:  string(mutatedOtherBytes),
		mutatedHeader: mutatedHeader,
	}
	service.InitHttpClient()
	client := service.GetHttpClient()
	require.NotNil(t, client)
	originalTransport := client.Transport
	client.Transport = transport
	service.ResetProxyClientCache()
	t.Cleanup(func() {
		client.Transport = originalTransport
		service.ResetProxyClientCache()
	})

	body := []byte(`{"model":"retry-snapshot-model","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Authorization", "Bearer retry-snapshot-client-token")
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(common.RequestIdKey, "retry-snapshot-request-id")
	common.SetContextKey(c, constant.ContextKeyRequestStartTime, time.Now())
	common.SetContextKey(c, constant.ContextKeyUserId, user.Id)
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserQuota, user.Quota)
	common.SetContextKey(c, constant.ContextKeyTokenId, token.Id)
	common.SetContextKey(c, constant.ContextKeyTokenKey, token.Key)
	common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
	common.SetContextKey(c, constant.ContextKeyTokenUnlimited, true)
	require.Nil(t, middleware.SetupContextForSelectedChannel(c, &channelA, "retry-snapshot-model"))
	t.Cleanup(func() {
		common.RunRequestCleanups(c)
		common.CleanupBodyStorage(c)
	})

	Relay(c, types.RelayFormatOpenAI)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), "from-b")
	assert.Equal(t, channelB.Id, common.GetContextKeyInt(c, constant.ContextKeyChannelId))
	assert.Equal(t, []string{strconv.Itoa(channelA.Id), strconv.Itoa(channelB.Id)}, c.GetStringSlice("use_channel"))
	assert.Empty(t, recorder.Header().Get("Retry-After"))
	assert.Empty(t, recorder.Header().Get("X-Upstream-Attempt-A"))
	assert.Empty(t, recorder.Header().Get("X-Upstream-Attempt-B"))

	captured, mutationErr := transport.snapshot()
	require.NoError(t, mutationErr)
	require.Len(t, captured, 2)
	assert.Equal(t, channelA.Id, captured[0].channelID)
	assert.Equal(t, channelB.Id, captured[1].channelID)
	assert.Equal(t, "/zen/go/v1/responses", captured[0].path)
	assert.Equal(t, "/zen/go/v1/chat/completions", captured[1].path)
	assert.Equal(t, "a", captured[0].header.Get("X-Snapshot-Channel"))
	assert.Equal(t, "b-frozen", captured[1].header.Get("X-Snapshot-Channel"))
	assert.Equal(t, "Bearer retry-snapshot-key-b", captured[1].header.Get("Authorization"))

	var bodyA map[string]interface{}
	require.NoError(t, common.Unmarshal(captured[0].body, &bodyA))
	assert.Contains(t, bodyA, "input")
	assert.NotContains(t, bodyA, "messages")
	var bodyB map[string]interface{}
	require.NoError(t, common.Unmarshal(captured[1].body, &bodyB))
	assert.Contains(t, bodyB, "messages")
	assert.NotContains(t, bodyB, "input")
	assert.Equal(t, false, bodyB["stream"])

	planB, found, err := opencodego.GetRequestPreflightPlanForSelection(c, "default", channelB.Id)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, opencodego.ProtocolChat, planB.FinalProtocol)
	planC, found, err := opencodego.GetRequestPreflightPlanForSelection(c, "default", channelC.Id)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, opencodego.ProtocolMessages, planC.FinalProtocol)
	snapshot, found, err := getOpenCodeAPIKeyRetrySnapshot(c)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, snapshot.topology, 3, "same-priority candidates must all be frozen before billing")
	require.Len(t, snapshot.selections, 2, "the physical schedule must retain the configured retry budget")
	var mutatedB model.Channel
	require.NoError(t, db.First(&mutatedB, channelB.Id).Error)
	assert.Equal(t, "retry-snapshot-mutated-key-b", mutatedB.Key)
	require.NotNil(t, mutatedB.HeaderOverride)
	assert.Equal(t, mutatedHeader, *mutatedB.HeaderOverride)
}
