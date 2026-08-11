package service

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	hosttypes "github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const audioSettlementModelPrice = 0.001

func audioSettlementUsage() *dto.Usage {
	return &dto.Usage{
		PromptTokens:     12,
		CompletionTokens: 4,
		TotalTokens:      16,
		PromptTokensDetails: dto.InputTokenDetails{
			TextTokens:  2,
			AudioTokens: 10,
		},
		CompletionTokenDetails: dto.OutputTokenDetails{
			TextTokens:  1,
			AudioTokens: 3,
		},
	}
}

func audioSettlementRelayInfo(
	userID int,
	channelID int,
	tokenID int,
	tokenKey string,
	funding FundingSource,
) (*relaycommon.RelayInfo, *BillingSession) {
	relayInfo := &relaycommon.RelayInfo{
		UserId:                userID,
		TokenId:               tokenID,
		TokenKey:              tokenKey,
		OriginModelName:       "audio-settlement-model",
		StartTime:             time.Now(),
		BillingSource:         BillingSourceWallet,
		FinalPreConsumedQuota: 50,
		PriceData: hosttypes.PriceData{
			UsePrice:        true,
			ModelPrice:      audioSettlementModelPrice,
			CompletionRatio: 1,
			GroupRatioInfo:  hosttypes.GroupRatioInfo{GroupRatio: 1},
		},
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channelID},
	}
	session := &BillingSession{
		relayInfo:        relayInfo,
		funding:          funding,
		preConsumedQuota: 50,
		tokenConsumed:    50,
	}
	relayInfo.Billing = session
	return relayInfo, session
}

func TestPostAudioConsumeQuotaFundingFailureHasNoSuccessSideEffects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	truncate(t)

	const (
		userID    = 98701
		channelID = 98702
	)
	seedUser(t, userID, 1_000_000)
	seedChannel(t, channelID)

	fundingErr := errors.New("forced audio funding settlement failure")
	funding := &settlementBoundaryFunding{settleErr: fundingErr}
	relayInfo, session := audioSettlementRelayInfo(userID, channelID, 0, "", funding)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("username", "audio_funding_failure_user")

	originalScheduler := scheduleAudioRelaySuccessSample
	successSamples := 0
	scheduleAudioRelaySuccessSample = func(*relaycommon.RelayInfo, int) {
		successSamples++
	}
	t.Cleanup(func() { scheduleAudioRelaySuccessSample = originalScheduler })

	apiErr := PostAudioConsumeQuota(ctx, relayInfo, audioSettlementUsage(), "")

	require.NotNil(t, apiErr)
	assert.Equal(t, types.ErrorCodeBillingSettlementFailed, apiErr.GetErrorCode())
	assert.True(t, types.IsSkipRetryError(apiErr))
	assert.ErrorIs(t, apiErr, fundingErr)
	stage, classified := BillingSettlementStageFromError(apiErr)
	require.True(t, classified)
	assert.Equal(t, BillingSettlementStageFunding, stage)
	assert.True(t, session.NeedsRefund())
	assert.Equal(t, 1, funding.settleCalls)
	assert.Equal(t, 450, funding.settledDelta)
	assert.Zero(t, funding.refundCalls)
	assert.Zero(t, successSamples)

	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Zero(t, user.UsedQuota)
	assert.Zero(t, user.RequestCount)

	var channel model.Channel
	require.NoError(t, model.DB.First(&channel, channelID).Error)
	assert.Zero(t, channel.UsedQuota)

	var logs []model.Log
	require.NoError(t, model.LOG_DB.Where("user_id = ?", userID).Find(&logs).Error)
	assert.Empty(t, logs)
}

func TestPostAudioConsumeQuotaTokenFailureKeepsFixedPriceAndRecordsDegradation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	truncate(t)

	const (
		userID    = 98801
		channelID = 98802
		tokenID   = 98803
		tokenKey  = "sk-audio-settlement-degraded"
	)
	seedUser(t, userID, 1_000_000)
	seedChannel(t, channelID)
	seedToken(t, tokenID, userID, tokenKey, 999_950)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", tokenID).Update("used_quota", 50).Error)
	require.NoError(t, model.DB.Exec(`
		CREATE TRIGGER fail_audio_settlement_token_update
		BEFORE UPDATE ON tokens
		WHEN OLD.id = 98803
		BEGIN
			SELECT RAISE(FAIL, 'forced audio token quota adjustment failure');
		END
	`).Error)
	t.Cleanup(func() {
		model.DB.Exec("DROP TRIGGER IF EXISTS fail_audio_settlement_token_update")
	})

	funding := &settlementBoundaryFunding{}
	relayInfo, session := audioSettlementRelayInfo(userID, channelID, tokenID, tokenKey, funding)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("username", "audio_settlement_degraded_user")

	originalScheduler := scheduleAudioRelaySuccessSample
	successSamples := 0
	sampledCompletionTokens := 0
	scheduleAudioRelaySuccessSample = func(_ *relaycommon.RelayInfo, completionTokens int) {
		successSamples++
		sampledCompletionTokens = completionTokens
	}
	t.Cleanup(func() { scheduleAudioRelaySuccessSample = originalScheduler })

	apiErr := PostAudioConsumeQuota(ctx, relayInfo, audioSettlementUsage(), "")

	require.Nil(t, apiErr)
	assert.Equal(t, 1, funding.settleCalls)
	assert.Equal(t, 450, funding.settledDelta)
	assert.True(t, session.fundingSettled)
	assert.True(t, session.settled)
	assert.False(t, session.NeedsRefund())
	assert.Zero(t, funding.refundCalls)
	assert.Equal(t, 1, successSamples)
	assert.Equal(t, 4, sampledCompletionTokens)

	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Equal(t, 500, user.UsedQuota)
	assert.Equal(t, 1, user.RequestCount)

	var channel model.Channel
	require.NoError(t, model.DB.First(&channel, channelID).Error)
	assert.Equal(t, int64(500), channel.UsedQuota)

	var token model.Token
	require.NoError(t, model.DB.First(&token, tokenID).Error)
	assert.Equal(t, 999_950, token.RemainQuota)
	assert.Equal(t, 50, token.UsedQuota)

	var logs []model.Log
	require.NoError(t, model.LOG_DB.Where("user_id = ?", userID).Find(&logs).Error)
	require.Len(t, logs, 1)
	assert.Equal(t, 500, logs[0].Quota)
	assert.Contains(t, logs[0].Content, "资金结算已提交，令牌额度调整失败")

	var other map[string]any
	require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
	assert.Equal(t, true, other["billing_settlement_degraded"])
	assert.Equal(t, string(BillingSettlementStageTokenQuota), other["billing_settlement_stage"])
}

func TestPostAudioConsumeQuotaBillsFixedPriceWhenAggregateTokensAreMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	truncate(t)

	const (
		userID    = 98901
		channelID = 98902
	)
	seedUser(t, userID, 1_000_000)
	seedChannel(t, channelID)

	usage := audioSettlementUsage()
	usage.PromptTokens = 0
	usage.CompletionTokens = 0
	usage.TotalTokens = 0
	settler := &quotaConsistencyBillingSettler{}
	relayInfo := &relaycommon.RelayInfo{
		UserId:          userID,
		OriginModelName: "audio-fixed-price-model",
		StartTime:       time.Now(),
		BillingSource:   BillingSourceSubscription,
		Billing:         settler,
		PriceData: hosttypes.PriceData{
			UsePrice:        true,
			ModelPrice:      audioSettlementModelPrice,
			CompletionRatio: 1,
			GroupRatioInfo:  hosttypes.GroupRatioInfo{GroupRatio: 1},
		},
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channelID},
	}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("username", "audio_fixed_price_user")

	originalScheduler := scheduleAudioRelaySuccessSample
	successSamples := 0
	scheduleAudioRelaySuccessSample = func(*relaycommon.RelayInfo, int) {
		successSamples++
	}
	t.Cleanup(func() { scheduleAudioRelaySuccessSample = originalScheduler })

	apiErr := PostAudioConsumeQuota(ctx, relayInfo, usage, "")

	require.Nil(t, apiErr)
	assert.Equal(t, 1, settler.settleCalls)
	assert.Equal(t, 500, settler.settledQuota)
	assert.Equal(t, 1, successSamples)

	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Equal(t, 500, user.UsedQuota)
	assert.Equal(t, 1, user.RequestCount)

	var channel model.Channel
	require.NoError(t, model.DB.First(&channel, channelID).Error)
	assert.Equal(t, int64(500), channel.UsedQuota)

	var logs []model.Log
	require.NoError(t, model.LOG_DB.Where("user_id = ?", userID).Find(&logs).Error)
	require.Len(t, logs, 1)
	assert.Equal(t, 500, logs[0].Quota)
}

func TestPostAudioConsumeQuotaBillsTieredDetailsWhenAggregateTokensAreMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	truncate(t)

	const (
		userID     = 99001
		channelID  = 99002
		tieredExpr = `tier("audio", ai + ao * 2)`
	)
	seedUser(t, userID, 1_000_000)
	seedChannel(t, channelID)

	usage := audioSettlementUsage()
	usage.PromptTokens = 0
	usage.CompletionTokens = 0
	usage.TotalTokens = 0
	settler := &quotaConsistencyBillingSettler{}
	relayInfo := &relaycommon.RelayInfo{
		UserId:          userID,
		OriginModelName: "audio-tiered-model",
		StartTime:       time.Now(),
		BillingSource:   BillingSourceSubscription,
		Billing:         settler,
		PriceData: hosttypes.PriceData{
			GroupRatioInfo: hosttypes.GroupRatioInfo{GroupRatio: 1},
		},
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:  "tiered_expr",
			ExprString:   tieredExpr,
			ExprHash:     billingexpr.ExprHashString(tieredExpr),
			GroupRatio:   1,
			QuotaPerUnit: 1_000_000,
		},
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channelID},
	}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("username", "audio_tiered_user")

	originalScheduler := scheduleAudioRelaySuccessSample
	scheduleAudioRelaySuccessSample = func(*relaycommon.RelayInfo, int) {}
	t.Cleanup(func() { scheduleAudioRelaySuccessSample = originalScheduler })

	apiErr := PostAudioConsumeQuota(ctx, relayInfo, usage, "")

	require.Nil(t, apiErr)
	assert.Equal(t, 1, settler.settleCalls)
	assert.Equal(t, 16, settler.settledQuota)

	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Equal(t, 16, user.UsedQuota)
	assert.Equal(t, 1, user.RequestCount)

	var logs []model.Log
	require.NoError(t, model.LOG_DB.Where("user_id = ?", userID).Find(&logs).Error)
	require.Len(t, logs, 1)
	assert.Equal(t, 16, logs[0].Quota)
}

func TestPostAudioConsumeQuotaEmptyUsageSettlesPreConsumeToZero(t *testing.T) {
	gin.SetMode(gin.TestMode)
	truncate(t)

	const (
		userID    = 99101
		channelID = 99102
	)
	seedUser(t, userID, 1_000_000)
	seedChannel(t, channelID)

	funding := &settlementBoundaryFunding{}
	relayInfo, session := audioSettlementRelayInfo(userID, channelID, 0, "", funding)
	relayInfo.IsPlayground = true
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("username", "audio_empty_usage_user")

	originalScheduler := scheduleAudioRelaySuccessSample
	scheduleAudioRelaySuccessSample = func(*relaycommon.RelayInfo, int) {}
	t.Cleanup(func() { scheduleAudioRelaySuccessSample = originalScheduler })

	apiErr := PostAudioConsumeQuota(ctx, relayInfo, &dto.Usage{}, "")

	require.Nil(t, apiErr)
	assert.Equal(t, 1, funding.settleCalls)
	assert.Equal(t, -50, funding.settledDelta)
	assert.True(t, session.settled)
	assert.False(t, session.NeedsRefund())

	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.Zero(t, user.UsedQuota)
	assert.Zero(t, user.RequestCount)

	var logs []model.Log
	require.NoError(t, model.LOG_DB.Where("user_id = ?", userID).Find(&logs).Error)
	require.Len(t, logs, 1)
	assert.Zero(t, logs[0].Quota)
}
