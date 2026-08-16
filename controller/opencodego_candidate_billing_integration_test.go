package controller

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type openCodeCandidateBillingAttempt struct {
	channelID int
	body      []byte
}

type openCodeCandidateBillingTransport struct {
	mutex          sync.Mutex
	db             *gorm.DB
	userID         int
	channelAID     int
	channelBID     int
	quotaAtFirstIO int
	quotaReadErr   error
	attempts       []openCodeCandidateBillingAttempt
}

func (transport *openCodeCandidateBillingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	if err := request.Body.Close(); err != nil {
		return nil, err
	}

	channelID := 0
	switch request.Header.Get("Authorization") {
	case "Bearer candidate-billing-key-a":
		channelID = transport.channelAID
	case "Bearer candidate-billing-key-b":
		channelID = transport.channelBID
	default:
		return nil, errors.New("unexpected candidate billing credential")
	}

	quotaAtFirstIO := 0
	var quotaReadErr error
	if channelID == transport.channelAID {
		var user model.User
		quotaReadErr = transport.db.Select("quota").First(&user, transport.userID).Error
		quotaAtFirstIO = user.Quota
	}

	transport.mutex.Lock()
	transport.attempts = append(transport.attempts, openCodeCandidateBillingAttempt{
		channelID: channelID,
		body:      append([]byte(nil), body...),
	})
	if channelID == transport.channelAID {
		transport.quotaAtFirstIO = quotaAtFirstIO
		transport.quotaReadErr = quotaReadErr
	}
	transport.mutex.Unlock()

	if channelID == transport.channelAID {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Status:     "503 Service Unavailable",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewBufferString(
				`{"error":{"message":"temporary failure","type":"server_error","code":"server_error"}}`,
			)),
			Request: request,
		}, nil
	}

	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewBufferString(
			`{"id":"chatcmpl-candidate-b","object":"chat.completion","created":1,"model":"candidate-billing-model","choices":[{"index":0,"message":{"role":"assistant","content":"from-b"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
		)),
		Request: request,
	}, nil
}

func (transport *openCodeCandidateBillingTransport) snapshot() ([]openCodeCandidateBillingAttempt, int, error) {
	transport.mutex.Lock()
	defer transport.mutex.Unlock()
	attempts := make([]openCodeCandidateBillingAttempt, len(transport.attempts))
	copy(attempts, transport.attempts)
	return attempts, transport.quotaAtFirstIO, transport.quotaReadErr
}

func TestOpenCodeAPIKeyCandidateBillingReservesMaximumAndSettlesWinner(t *testing.T) {
	const (
		modelName         = "candidate-billing-model"
		requestID         = "candidate-billing-request-id"
		normalExprCost    = 100_000
		fastExprCost      = 800_000
		candidateATier    = "normal"
		candidateBTier    = "fast"
		candidateBContent = "from-b"
	)

	normalQuota, err := billingexpr.QuotaRoundStrict(
		float64(normalExprCost) / 1_000_000 * common.QuotaPerUnit,
	)
	require.NoError(t, err)
	fastQuota, err := billingexpr.QuotaRoundStrict(
		float64(fastExprCost) / 1_000_000 * common.QuotaPerUnit,
	)
	require.NoError(t, err)
	require.Greater(t, fastQuota, normalQuota)
	initialQuota := fastQuota + int(common.QuotaPerUnit)
	require.Less(t, initialQuota, common.GetTrustQuota(), "fixture must exercise real wallet reservation")

	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Token{}, &model.Log{}, &model.UserSubscription{}))

	savedConfig := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		savedConfig[key] = value
		return nil
	}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(savedConfig))
	})
	t.Cleanup(func() {
		perfmetrics.WaitForScheduledRelaySamples()
	})
	billingModes, err := common.Marshal(map[string]string{modelName: "tiered_expr"})
	require.NoError(t, err)
	billingExpressions, err := common.Marshal(map[string]string{
		modelName: `param("service_tier") == "fast" ? tier("fast", 800000) : tier("normal", 100000)`,
	})
	require.NoError(t, err)
	groupRatios, err := common.Marshal(map[string]float64{"default": 1})
	require.NoError(t, err)
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode":    string(billingModes),
		"billing_setting.billing_expr":    string(billingExpressions),
		"group_ratio_setting.group_ratio": string(groupRatios),
	}))

	originalMemoryCache := common.MemoryCacheEnabled
	originalRetryTimes := common.RetryTimes
	originalAutoDisable := common.AutomaticDisableChannelEnabled
	originalLogConsume := common.LogConsumeEnabled
	originalDataExport := common.DataExportEnabled
	originalRetryRanges := append([]operation_setting.StatusCodeRange(nil), operation_setting.AutomaticRetryStatusCodeRanges...)
	common.MemoryCacheEnabled = false
	common.RetryTimes = 1
	common.AutomaticDisableChannelEnabled = false
	common.LogConsumeEnabled = true
	common.DataExportEnabled = false
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{{
		Start: http.StatusServiceUnavailable,
		End:   http.StatusServiceUnavailable,
	}}
	t.Cleanup(func() {
		common.MemoryCacheEnabled = originalMemoryCache
		common.RetryTimes = originalRetryTimes
		common.AutomaticDisableChannelEnabled = originalAutoDisable
		common.LogConsumeEnabled = originalLogConsume
		common.DataExportEnabled = originalDataExport
		operation_setting.AutomaticRetryStatusCodeRanges = originalRetryRanges
	})

	user := model.User{
		Username: "candidate-billing-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    initialQuota,
	}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{
		UserId:         user.Id,
		Key:            "candidate-billing-client-token",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}
	require.NoError(t, db.Create(&token).Error)

	highPriority := int64(20)
	lowPriority := int64(10)
	autoBan := 0
	paramOverrideA := `{"service_tier":"normal"}`
	paramOverrideB := `{"service_tier":"fast"}`
	channelA := model.Channel{
		Type:          constant.ChannelTypeOpenCodeAPIKey,
		Key:           "candidate-billing-key-a",
		Status:        common.ChannelStatusEnabled,
		Name:          "candidate-billing-a",
		Models:        modelName,
		Group:         "default",
		Priority:      &highPriority,
		AutoBan:       &autoBan,
		ParamOverride: &paramOverrideA,
	}
	channelA.SetOtherSettings(dto.ChannelOtherSettings{
		AllowServiceTier: true,
		OpenCodeGo: &dto.OpenCodeGoConfig{
			ModelProtocols: map[string]string{modelName: dto.OpenCodeGoProtocolChat},
		},
	})
	require.NoError(t, db.Create(&channelA).Error)
	channelB := model.Channel{
		Type:          constant.ChannelTypeOpenCodeAPIKey,
		Key:           "candidate-billing-key-b",
		Status:        common.ChannelStatusEnabled,
		Name:          "candidate-billing-b",
		Models:        modelName,
		Group:         "default",
		Priority:      &lowPriority,
		AutoBan:       &autoBan,
		ParamOverride: &paramOverrideB,
	}
	channelB.SetOtherSettings(dto.ChannelOtherSettings{
		AllowServiceTier: true,
		OpenCodeGo: &dto.OpenCodeGoConfig{
			ModelProtocols: map[string]string{modelName: dto.OpenCodeGoProtocolChat},
		},
	})
	require.NoError(t, db.Create(&channelB).Error)
	for _, ability := range []model.Ability{
		{Group: "default", Model: modelName, ChannelId: channelA.Id, Enabled: true, Priority: &highPriority},
		{Group: "default", Model: modelName, ChannelId: channelB.Id, Enabled: true, Priority: &lowPriority},
	} {
		require.NoError(t, db.Create(&ability).Error)
	}

	transport := &openCodeCandidateBillingTransport{
		db:         db,
		userID:     user.Id,
		channelAID: channelA.Id,
		channelBID: channelB.Id,
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

	body := []byte(`{"model":"candidate-billing-model","messages":[{"role":"user","content":"hello"}],"max_tokens":16,"stream":false}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(common.RequestIdKey, requestID)
	c.Set("username", user.Username)
	common.SetContextKey(c, constant.ContextKeyRequestStartTime, time.Now())
	common.SetContextKey(c, constant.ContextKeyUserId, user.Id)
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserQuota, initialQuota)
	common.SetContextKey(c, constant.ContextKeyTokenId, token.Id)
	common.SetContextKey(c, constant.ContextKeyTokenKey, token.Key)
	common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
	common.SetContextKey(c, constant.ContextKeyTokenUnlimited, true)
	common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: "wallet_only"})
	require.Nil(t, middleware.SetupContextForSelectedChannel(c, &channelA, modelName))
	t.Cleanup(func() {
		common.RunRequestCleanups(c)
		common.CleanupBodyStorage(c)
	})

	Relay(c, types.RelayFormatOpenAI)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), candidateBContent)
	assert.Equal(t, channelB.Id, common.GetContextKeyInt(c, constant.ContextKeyChannelId))
	assert.Equal(t, []string{strconv.Itoa(channelA.Id), strconv.Itoa(channelB.Id)}, c.GetStringSlice("use_channel"))

	attempts, quotaAtFirstIO, quotaReadErr := transport.snapshot()
	require.NoError(t, quotaReadErr)
	require.Len(t, attempts, 2)
	assert.Equal(t, channelA.Id, attempts[0].channelID)
	assert.Equal(t, channelB.Id, attempts[1].channelID)
	assert.Equal(t, initialQuota-fastQuota, quotaAtFirstIO,
		"candidate B's maximum must be reserved before candidate A performs I/O")
	planValue, found := c.Get(openCodeFinalizedCandidatePlansContextKey)
	require.True(t, found)
	plans, ok := planValue.(*openCodeFinalizedCandidatePlans)
	require.True(t, ok)
	plannedBodies := make(map[int][]byte, len(plans.plans))
	for _, plan := range plans.plans {
		plannedBodies[plan.key.ChannelID] = plan.body
	}
	for _, attempt := range attempts {
		planned, exists := plannedBodies[attempt.channelID]
		require.True(t, exists)
		assert.True(t, bytes.Equal(planned, attempt.body),
			"physical body for channel %d differed from its pre-billing candidate", attempt.channelID)
	}

	var bodyA map[string]interface{}
	require.NoError(t, common.Unmarshal(attempts[0].body, &bodyA))
	assert.Equal(t, candidateATier, bodyA["service_tier"])
	var bodyB map[string]interface{}
	require.NoError(t, common.Unmarshal(attempts[1].body, &bodyB))
	assert.Equal(t, candidateBTier, bodyB["service_tier"])

	var settledUser model.User
	require.NoError(t, db.First(&settledUser, user.Id).Error)
	assert.Equal(t, initialQuota-fastQuota, settledUser.Quota,
		"winner B's finalized tier must settle without inheriting failed A's cheaper tier")

	var consumeLogs []model.Log
	require.NoError(t, db.Where("type = ? AND request_id = ?", model.LogTypeConsume, requestID).Find(&consumeLogs).Error)
	require.Len(t, consumeLogs, 1)
	assert.Equal(t, channelB.Id, consumeLogs[0].ChannelId)
	assert.Equal(t, fastQuota, consumeLogs[0].Quota)
	var other map[string]interface{}
	require.NoError(t, common.Unmarshal([]byte(consumeLogs[0].Other), &other))
	assert.Equal(t, "tiered_expr", other["billing_mode"])
	assert.Equal(t, candidateBTier, other["matched_tier"])
}
