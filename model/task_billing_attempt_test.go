package model

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func billingSnapshot(requestID string, amount int) TaskBillingAttemptSnapshot {
	return TaskBillingAttemptSnapshot{
		RequestID:     requestID,
		PublicTaskID:  "task_" + requestID,
		SubmitTime:    1_750_000_000,
		UserID:        501,
		FundingSource: "wallet",
		FundingAmount: amount,
		TokenID:       601,
		TokenAmount:   amount,
	}
}

func seedBillingBalances(t *testing.T, userID, tokenID, wallet, token int) {
	t.Helper()
	require.NoError(t, DB.Create(&User{
		Id:       userID,
		Username: fmt.Sprintf("billing-user-%d", userID),
		Quota:    wallet,
	}).Error)
	require.NoError(t, DB.Create(&Token{
		Id:          tokenID,
		UserId:      userID,
		Key:         fmt.Sprintf("billing-token-%d", tokenID),
		RemainQuota: token,
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
	}).Error)
}

func loadBillingAttempt(t *testing.T, requestID string) TaskBillingAttempt {
	t.Helper()
	var attempt TaskBillingAttempt
	require.NoError(t, DB.Where("request_id = ?", requestID).First(&attempt).Error)
	return attempt
}

func loadBillingUserQuota(t *testing.T, userID int) int {
	t.Helper()
	var quota int
	require.NoError(t, DB.Model(&User{}).Where("id = ?", userID).Select("quota").Scan(&quota).Error)
	return quota
}

func loadBillingToken(t *testing.T, tokenID int) Token {
	t.Helper()
	var token Token
	require.NoError(t, DB.Unscoped().Where("id = ?", tokenID).First(&token).Error)
	return token
}

func TestBeginTaskBillingAttemptPrecedesAnyBalanceMutation(t *testing.T) {
	truncateTables(t)
	snapshot := billingSnapshot("begin-before-balance", 120)
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)

	attempt, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	require.NotZero(t, attempt.ID)
	assert.Equal(t, 1_000, loadBillingUserQuota(t, snapshot.UserID))
	assert.Equal(t, 1_000, loadBillingToken(t, snapshot.TokenID).RemainQuota)
	assert.Zero(t, attempt.FundingConsumedAt)
	assert.Zero(t, attempt.TokenConsumedAt)
}

func TestBeginTaskBillingAttemptIsUniqueByRequestID(t *testing.T) {
	truncateTables(t)
	snapshot := billingSnapshot("unique-request", 10)
	first, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	second, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID)

	var count int64
	require.NoError(t, DB.Model(&TaskBillingAttempt{}).
		Where("request_id = ?", snapshot.RequestID).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestBeginTaskBillingAttemptRejectsImmutableDrift(t *testing.T) {
	truncateTables(t)
	snapshot := billingSnapshot("immutable-drift", 10)
	first, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)

	drifted := snapshot
	drifted.FundingAmount++
	_, err = BeginTaskBillingAttempt(drifted)
	require.Error(t, err)
	assert.Equal(t, snapshot.FundingAmount, loadBillingAttempt(t, snapshot.RequestID).FundingAmount)
	assert.Equal(t, TaskBillingOwnerRequest, first.Owner)
}

func TestFreeAndPaidZeroAttemptShapes(t *testing.T) {
	truncateTables(t)

	free := billingSnapshot("free-shape", 0)
	free.IsFree = true
	free.UserID = 0
	free.FundingSource = ""
	free.SubscriptionID = 0
	free.TokenAmount = 0
	free.TokenID = 777
	freeAttempt, err := BeginTaskBillingAttempt(free)
	require.NoError(t, err)
	assert.NotZero(t, freeAttempt.FundingConsumedAt)
	assert.NotZero(t, freeAttempt.TokenConsumedAt)
	assert.NotZero(t, freeAttempt.PreconsumeCompletedAt)
	assert.Equal(t, 777, freeAttempt.TokenID)

	paidZero := billingSnapshot("paid-zero-shape", 0)
	paidZero.TokenAmount = 0
	seedBillingBalances(t, paidZero.UserID, paidZero.TokenID, 1_000, 1_000)
	paidAttempt, err := BeginTaskBillingAttempt(paidZero)
	require.NoError(t, err)
	assert.False(t, paidAttempt.IsFree)
	assert.Equal(t, "wallet", paidAttempt.FundingSource)
	assert.Zero(t, paidAttempt.FundingConsumedAt)
	assert.Zero(t, paidAttempt.TokenConsumedAt)

	fundingResult, err := ApplyTaskFundingPreconsume(paidZero.RequestID)
	require.NoError(t, err)
	tokenResult, err := ApplyTaskTokenPreconsume(paidZero.RequestID)
	require.NoError(t, err)
	assert.True(t, fundingResult.Applied)
	assert.True(t, tokenResult.Applied)
	completed, err := VerifyTaskBillingAttemptPreconsumed(paidZero.RequestID)
	require.NoError(t, err)
	assert.NotZero(t, completed.PreconsumeCompletedAt)
}

func TestFundingPreconsumeMutationAndMarkerAreAtomic(t *testing.T) {
	truncateTables(t)
	snapshot := billingSnapshot("funding-preconsume-atomic", 125)
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)

	restore := setTaskBillingFailpointForTest(func(operation, point string) error {
		if operation == "funding_preconsume" && point == "after_balance_mutation" {
			return errors.New("crash after wallet update")
		}
		return nil
	})
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	restore()
	require.Error(t, err)
	assert.Equal(t, 1_000, loadBillingUserQuota(t, snapshot.UserID))
	assert.Zero(t, loadBillingAttempt(t, snapshot.RequestID).FundingConsumedAt)

	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	assert.Equal(t, 875, loadBillingUserQuota(t, snapshot.UserID))
	assert.NotZero(t, loadBillingAttempt(t, snapshot.RequestID).FundingConsumedAt)
}

func TestTokenPreconsumeMutationAndMarkerAreAtomic(t *testing.T) {
	truncateTables(t)
	snapshot := billingSnapshot("token-preconsume-atomic", 125)
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)

	restore := setTaskBillingFailpointForTest(func(operation, point string) error {
		if operation == "token_preconsume" && point == "after_balance_mutation" {
			return errors.New("crash after token update")
		}
		return nil
	})
	_, err = ApplyTaskTokenPreconsume(snapshot.RequestID)
	restore()
	require.Error(t, err)
	token := loadBillingToken(t, snapshot.TokenID)
	assert.Equal(t, 1_000, token.RemainQuota)
	assert.Zero(t, token.UsedQuota)
	assert.Zero(t, loadBillingAttempt(t, snapshot.RequestID).TokenConsumedAt)

	_, err = ApplyTaskTokenPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	token = loadBillingToken(t, snapshot.TokenID)
	assert.Equal(t, 875, token.RemainQuota)
	assert.Equal(t, 125, token.UsedQuota)
	assert.NotZero(t, loadBillingAttempt(t, snapshot.RequestID).TokenConsumedAt)
}

func TestSubscriptionPreconsumeRecordBalanceAndMarkerAreAtomic(t *testing.T) {
	truncateTables(t)
	now := time.Now().Unix()
	snapshot := billingSnapshot("subscription-preconsume-atomic", 125)
	snapshot.FundingSource = "subscription"
	snapshot.SubscriptionID = 701
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	require.NoError(t, DB.Create(&UserSubscription{
		Id:          snapshot.SubscriptionID,
		UserId:      snapshot.UserID,
		AmountTotal: 1_000,
		AmountUsed:  100,
		Status:      "active",
		StartTime:   now - 10,
		EndTime:     now + 3_600,
	}).Error)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)

	restore := setTaskBillingFailpointForTest(func(operation, point string) error {
		if operation == "funding_preconsume" && point == "after_balance_mutation" {
			return errors.New("crash after subscription update")
		}
		return nil
	})
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	restore()
	require.Error(t, err)

	var sub UserSubscription
	require.NoError(t, DB.First(&sub, snapshot.SubscriptionID).Error)
	assert.Equal(t, int64(100), sub.AmountUsed)
	var count int64
	require.NoError(t, DB.Model(&SubscriptionPreConsumeRecord{}).
		Where("request_id = ?", snapshot.RequestID).Count(&count).Error)
	assert.Zero(t, count)
	assert.Zero(t, loadBillingAttempt(t, snapshot.RequestID).FundingConsumedAt)

	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	require.NoError(t, DB.First(&sub, snapshot.SubscriptionID).Error)
	assert.Equal(t, int64(225), sub.AmountUsed)
	assert.NotZero(t, loadBillingAttempt(t, snapshot.RequestID).FundingConsumedAt)
}

func TestDurableSubscriptionPlanningAndApplyHonorDueReset(t *testing.T) {
	truncateTables(t)
	now := time.Now().Unix()
	const planID = 711
	require.NoError(t, DB.Create(&SubscriptionPlan{
		Id:               planID,
		Title:            "durable reset fixture",
		QuotaResetPeriod: SubscriptionResetDaily,
	}).Error)

	snapshot := billingSnapshot("subscription-due-reset", 80)
	snapshot.FundingSource = "subscription"
	snapshot.SubscriptionID = 712
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	lastReset := now - 86_401
	nextReset := now - 1
	require.NoError(t, DB.Create(&UserSubscription{
		Id:                snapshot.SubscriptionID,
		UserId:            snapshot.UserID,
		PlanId:            planID,
		AmountTotal:       100,
		AmountUsed:        100,
		Status:            "active",
		StartTime:         now - 2*86_400,
		EndTime:           now + 7*86_400,
		LastResetTime:     lastReset,
		NextResetTime:     nextReset,
		QuotaResetVersion: 7,
	}).Error)

	planned, err := PlanUserSubscriptionForTaskBilling(snapshot.UserID, int64(snapshot.FundingAmount))
	require.NoError(t, err)
	assert.Equal(t, snapshot.SubscriptionID, planned.Id)
	assert.Zero(t, planned.AmountUsed, "planning projects the due reset without mutating primary state")
	assert.Equal(t, int64(8), planned.QuotaResetVersion)
	var subscription UserSubscription
	require.NoError(t, DB.First(&subscription, snapshot.SubscriptionID).Error)
	assert.Equal(t, int64(100), subscription.AmountUsed)
	assert.Equal(t, lastReset, subscription.LastResetTime)
	assert.Equal(t, int64(7), subscription.QuotaResetVersion)

	_, err = BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	restore := setTaskBillingFailpointForTest(func(operation, point string) error {
		if operation == "funding_preconsume" && point == "after_balance_mutation" {
			return errors.New("crash after reset and subscription consume")
		}
		return nil
	})
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	restore()
	require.Error(t, err)
	require.NoError(t, DB.First(&subscription, snapshot.SubscriptionID).Error)
	assert.Equal(t, int64(100), subscription.AmountUsed)
	assert.Equal(t, lastReset, subscription.LastResetTime)
	assert.Equal(t, int64(7), subscription.QuotaResetVersion)
	assert.Zero(t, loadBillingAttempt(t, snapshot.RequestID).FundingConsumedAt)

	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	require.NoError(t, DB.First(&subscription, snapshot.SubscriptionID).Error)
	assert.Equal(t, int64(80), subscription.AmountUsed)
	assert.Greater(t, subscription.LastResetTime, lastReset)
	assert.Equal(t, int64(8), subscription.QuotaResetVersion)
	var record SubscriptionPreConsumeRecord
	require.NoError(t, DB.Where("request_id = ?", snapshot.RequestID).First(&record).Error)
	require.NotNil(t, record.SubscriptionResetVersion)
	assert.Equal(t, subscription.QuotaResetVersion, *record.SubscriptionResetVersion)
	assert.NotZero(t, loadBillingAttempt(t, snapshot.RequestID).FundingConsumedAt)
}

func TestSubscriptionResetScheduleInitializationDoesNotAdvanceVersion(t *testing.T) {
	now := time.Now().Unix()
	plan := &SubscriptionPlan{QuotaResetPeriod: SubscriptionResetDaily}
	subscription := &UserSubscription{
		AmountUsed:        25,
		StartTime:         now,
		EndTime:           now + 7*86_400,
		QuotaResetVersion: 3,
	}

	changed, err := projectUserSubscriptionReset(subscription, plan, now)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, int64(25), subscription.AmountUsed)
	assert.Equal(t, int64(3), subscription.QuotaResetVersion)
	assert.Greater(t, subscription.NextResetTime, now)
}

func TestSubscriptionResetVersionOverflowFailsClosed(t *testing.T) {
	t.Run("due reset projection", func(t *testing.T) {
		now := time.Now().Unix()
		plan := &SubscriptionPlan{
			QuotaResetPeriod: SubscriptionResetDaily,
		}
		subscription := &UserSubscription{
			AmountUsed:        100,
			StartTime:         now - 2*86_400,
			EndTime:           now + 7*86_400,
			LastResetTime:     now - 86_401,
			NextResetTime:     now - 1,
			QuotaResetVersion: math.MaxInt64,
		}
		before := *subscription

		changed, err := projectUserSubscriptionReset(subscription, plan, now)
		require.Error(t, err)
		assert.False(t, changed)
		assert.Equal(t, before, *subscription)
	})

	t.Run("admin reset transaction", func(t *testing.T) {
		truncateTables(t)
		now := time.Now().Unix()
		const planID = 715
		require.NoError(t, DB.Create(&SubscriptionPlan{
			Id:               planID,
			Title:            "reset overflow fixture",
			QuotaResetPeriod: SubscriptionResetDaily,
		}).Error)
		require.NoError(t, DB.Create(&UserSubscription{
			Id:                716,
			UserId:            717,
			PlanId:            planID,
			AmountTotal:       1_000,
			AmountUsed:        100,
			Status:            "active",
			StartTime:         now - 3_600,
			EndTime:           now + 7*86_400,
			QuotaResetVersion: math.MaxInt64,
		}).Error)

		_, err := AdminResetUserSubscriptionsByPlan(717, planID, false)
		require.Error(t, err)
		var subscription UserSubscription
		require.NoError(t, DB.First(&subscription, 716).Error)
		assert.Equal(t, int64(100), subscription.AmountUsed)
		assert.Equal(t, int64(math.MaxInt64), subscription.QuotaResetVersion)
	})
}

func TestSubscriptionRefundAfterQuotaResetDoesNotDebitNewPeriod(t *testing.T) {
	for _, newPeriodUsed := range []int64{0, 50} {
		t.Run(fmt.Sprintf("new-period-used-%d", newPeriodUsed), func(t *testing.T) {
			truncateTables(t)
			now := time.Now().Unix()
			const planID = 713
			require.NoError(t, DB.Create(&SubscriptionPlan{
				Id:               planID,
				Title:            "refund generation fixture",
				QuotaResetPeriod: SubscriptionResetDaily,
			}).Error)

			snapshot := billingSnapshot(fmt.Sprintf("subscription-reset-refund-%d", newPeriodUsed), 40)
			snapshot.FundingSource = "subscription"
			snapshot.SubscriptionID = 714
			seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
			require.NoError(t, DB.Create(&UserSubscription{
				Id:          snapshot.SubscriptionID,
				UserId:      snapshot.UserID,
				PlanId:      planID,
				AmountTotal: 1_000,
				Status:      "active",
				StartTime:   now - 3_600,
				EndTime:     now + 7*86_400,
			}).Error)

			_, err := BeginTaskBillingAttempt(snapshot)
			require.NoError(t, err)
			_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
			require.NoError(t, err)

			_, err = AdminResetUserSubscriptionsByPlan(snapshot.UserID, planID, false)
			require.NoError(t, err)
			if newPeriodUsed > 0 {
				require.NoError(t, PostConsumeUserSubscriptionDelta(snapshot.SubscriptionID, newPeriodUsed))
			}

			_, err = ApplyTaskFundingRefund(snapshot.RequestID)
			require.NoError(t, err)

			var subscription UserSubscription
			require.NoError(t, DB.First(&subscription, snapshot.SubscriptionID).Error)
			assert.Equal(t, newPeriodUsed, subscription.AmountUsed)
			assert.Equal(t, int64(1), subscription.QuotaResetVersion)
			var record SubscriptionPreConsumeRecord
			require.NoError(t, DB.Where("request_id = ?", snapshot.RequestID).First(&record).Error)
			assert.Equal(t, "refunded", record.Status)
			require.NotNil(t, record.SubscriptionResetVersion)
			assert.Zero(t, *record.SubscriptionResetVersion)
			firstAttempt := loadBillingAttempt(t, snapshot.RequestID)
			assert.NotZero(t, firstAttempt.RefundStartedAt)
			assert.NotZero(t, firstAttempt.FundingRefundedAt)
			assert.Zero(t, firstAttempt.TokenRefundedAt)
			assert.Zero(t, firstAttempt.RefundCompletedAt)

			_, err = ApplyTaskFundingRefund(snapshot.RequestID)
			require.NoError(t, err)
			require.NoError(t, DB.First(&subscription, snapshot.SubscriptionID).Error)
			assert.Equal(t, newPeriodUsed, subscription.AmountUsed)
			assert.Equal(t, firstAttempt.FundingRefundedAt,
				loadBillingAttempt(t, snapshot.RequestID).FundingRefundedAt)
		})
	}
}

func TestSubscriptionRefundRejectsFutureRecordGeneration(t *testing.T) {
	truncateTables(t)
	now := time.Now().Unix()
	snapshot := billingSnapshot("subscription-future-generation", 40)
	snapshot.FundingSource = "subscription"
	snapshot.SubscriptionID = 721
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	require.NoError(t, DB.Create(&UserSubscription{
		Id:                snapshot.SubscriptionID,
		UserId:            snapshot.UserID,
		AmountTotal:       1_000,
		Status:            "active",
		StartTime:         now - 10,
		EndTime:           now + 3_600,
		QuotaResetVersion: 3,
	}).Error)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	require.NoError(t, DB.Model(&SubscriptionPreConsumeRecord{}).
		Where("request_id = ?", snapshot.RequestID).
		Update("subscription_reset_version", 4).Error)

	_, err = ApplyTaskFundingRefund(snapshot.RequestID)
	assert.ErrorIs(t, err, ErrTaskBillingIdentityDrift)

	var subscription UserSubscription
	require.NoError(t, DB.First(&subscription, snapshot.SubscriptionID).Error)
	assert.Equal(t, int64(40), subscription.AmountUsed)
	var record SubscriptionPreConsumeRecord
	require.NoError(t, DB.Where("request_id = ?", snapshot.RequestID).First(&record).Error)
	assert.Equal(t, "consumed", record.Status)
	attempt := loadBillingAttempt(t, snapshot.RequestID)
	assert.Zero(t, attempt.RefundStartedAt)
	assert.Zero(t, attempt.FundingRefundedAt)
}

func TestLegacySubscriptionRefundAfterQuotaResetDoesNotDebitNewPeriod(t *testing.T) {
	truncateTables(t)
	now := time.Now().Unix()
	const (
		planID         = 718
		subscriptionID = 719
		userID         = 720
	)
	require.NoError(t, DB.Create(&SubscriptionPlan{
		Id:               planID,
		Title:            "legacy refund generation fixture",
		QuotaResetPeriod: SubscriptionResetDaily,
	}).Error)
	require.NoError(t, DB.Create(&UserSubscription{
		Id:                subscriptionID,
		UserId:            userID,
		PlanId:            planID,
		AmountTotal:       1_000,
		Status:            "active",
		StartTime:         now - 3_600,
		EndTime:           now + 7*86_400,
		QuotaResetVersion: 4,
	}).Error)

	const requestID = "legacy-subscription-reset-refund"
	_, err := PreConsumeUserSubscription(requestID, userID, "", 0, 40)
	require.NoError(t, err)
	var record SubscriptionPreConsumeRecord
	require.NoError(t, DB.Where("request_id = ?", requestID).First(&record).Error)
	require.NotNil(t, record.SubscriptionResetVersion)
	assert.Equal(t, int64(4), *record.SubscriptionResetVersion)

	_, err = AdminResetUserSubscriptionsByPlan(userID, planID, false)
	require.NoError(t, err)
	require.NoError(t, PostConsumeUserSubscriptionDelta(subscriptionID, 50))
	require.NoError(t, RefundSubscriptionPreConsume(requestID))

	var subscription UserSubscription
	require.NoError(t, DB.First(&subscription, subscriptionID).Error)
	assert.Equal(t, int64(5), subscription.QuotaResetVersion)
	assert.Equal(t, int64(50), subscription.AmountUsed)
	require.NoError(t, DB.Where("request_id = ?", requestID).First(&record).Error)
	assert.Equal(t, "refunded", record.Status)
}

func TestLegacySubscriptionRefundFailsClosedForUnknownResetGeneration(t *testing.T) {
	truncateTables(t)
	now := time.Now().Unix()
	const (
		requestID      = "legacy-unknown-reset-generation"
		subscriptionID = 722
		userID         = 723
	)
	require.NoError(t, DB.Create(&UserSubscription{
		Id:                subscriptionID,
		UserId:            userID,
		AmountTotal:       1_000,
		AmountUsed:        50,
		Status:            "active",
		StartTime:         now - 3_600,
		EndTime:           now + 3_600,
		QuotaResetVersion: 0,
	}).Error)
	require.NoError(t, DB.Exec(`
		INSERT INTO subscription_pre_consume_records
			(request_id, user_id, user_subscription_id, pre_consumed, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, requestID, userID, subscriptionID, 40, "consumed", now, now).Error)

	err := RefundSubscriptionPreConsume(requestID)
	require.Error(t, err)

	var subscription UserSubscription
	require.NoError(t, DB.First(&subscription, subscriptionID).Error)
	assert.Equal(t, int64(50), subscription.AmountUsed)
	var record SubscriptionPreConsumeRecord
	require.NoError(t, DB.Where("request_id = ?", requestID).First(&record).Error)
	assert.Equal(t, "consumed", record.Status)
}

func TestSubscriptionResetGenerationMigrationLeavesLegacyRecordUnknown(t *testing.T) {
	type legacyPreConsumeRecord struct {
		Id                 int    `gorm:"primaryKey"`
		RequestId          string `gorm:"type:varchar(64);uniqueIndex"`
		UserId             int
		UserSubscriptionId int
		PreConsumed        int64
		Status             string
		CreatedAt          int64
		UpdatedAt          int64
	}

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	const tableName = "subscription_pre_consume_generation_migration_fixture"
	require.NoError(t, db.Table(tableName).AutoMigrate(&legacyPreConsumeRecord{}))
	require.NoError(t, db.Table(tableName).Create(&legacyPreConsumeRecord{
		RequestId:          "pre-migration-record",
		UserId:             724,
		UserSubscriptionId: 725,
		PreConsumed:        40,
		Status:             "consumed",
		CreatedAt:          time.Now().Unix(),
		UpdatedAt:          time.Now().Unix(),
	}).Error)

	require.NoError(t, db.Table(tableName).AutoMigrate(&SubscriptionPreConsumeRecord{}))
	var version sql.NullInt64
	require.NoError(t, db.Table(tableName).
		Select("subscription_reset_version").
		Where("request_id = ?", "pre-migration-record").
		Scan(&version).Error)
	assert.False(t, version.Valid, "pre-migration records must remain explicitly unknown")
}

func TestDurablePreconsumeBypassesBatchQueue(t *testing.T) {
	truncateTables(t)
	previousBatch := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = true
	t.Cleanup(func() { common.BatchUpdateEnabled = previousBatch })

	snapshot := billingSnapshot("batch-bypass", 200)
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	_, err = ApplyTaskTokenPreconsume(snapshot.RequestID)
	require.NoError(t, err)

	assert.Equal(t, 800, loadBillingUserQuota(t, snapshot.UserID))
	token := loadBillingToken(t, snapshot.TokenID)
	assert.Equal(t, 800, token.RemainQuota)
	assert.Equal(t, 200, token.UsedQuota)
	attempt := loadBillingAttempt(t, snapshot.RequestID)
	assert.NotZero(t, attempt.FundingConsumedAt)
	assert.NotZero(t, attempt.TokenConsumedAt)
	assert.NotZero(t, attempt.PreconsumeCompletedAt)
}

func TestPreconsumeCommitReadAfterErrorDoesNotReplay(t *testing.T) {
	truncateTables(t)
	snapshot := billingSnapshot("preconsume-readback", 200)
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)

	var once sync.Once
	restore := setTaskBillingFailpointForTest(func(operation, point string) error {
		var err error
		if operation == "funding_preconsume" && point == "after_commit" {
			once.Do(func() { err = errors.New("ambiguous commit result") })
		}
		return err
	})
	result, err := ApplyTaskFundingPreconsume(snapshot.RequestID)
	restore()
	require.NoError(t, err)
	assert.False(t, result.Completed)
	assert.Equal(t, 800, loadBillingUserQuota(t, snapshot.UserID))

	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	assert.Equal(t, 800, loadBillingUserQuota(t, snapshot.UserID))

	tokenResult, err := ApplyTaskTokenPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	assert.True(t, tokenResult.Completed)
}

func TestPaidZeroApplyValidatesPrimaryIdentity(t *testing.T) {
	truncateTables(t)
	snapshot := billingSnapshot("paid-zero-missing-identity", 0)
	snapshot.TokenAmount = 0
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)

	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.Error(t, err)
	_, err = ApplyTaskTokenPreconsume(snapshot.RequestID)
	require.Error(t, err)
	attempt := loadBillingAttempt(t, snapshot.RequestID)
	assert.Zero(t, attempt.FundingConsumedAt)
	assert.Zero(t, attempt.TokenConsumedAt)
}

func TestPreconsumeFailsClosedAfterRefundStarts(t *testing.T) {
	truncateTables(t)
	snapshot := billingSnapshot("preconsume-after-refund", 100)
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	_, err = ApplyTaskFundingRefund(snapshot.RequestID)
	require.NoError(t, err)

	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	assert.ErrorIs(t, err, ErrTaskBillingAttemptState)
	_, err = ApplyTaskTokenPreconsume(snapshot.RequestID)
	assert.ErrorIs(t, err, ErrTaskBillingAttemptState)
	assert.Equal(t, 1_000, loadBillingUserQuota(t, snapshot.UserID))
	assert.Equal(t, 1_000, loadBillingToken(t, snapshot.TokenID).RemainQuota)
}

func TestVerifyTaskBillingAttemptPreconsumedRejectsPrimaryIdentityDrift(t *testing.T) {
	truncateTables(t)
	snapshot := billingSnapshot("verify-primary-drift", 100)
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	_, err = ApplyTaskTokenPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", snapshot.TokenID).
		Update("user_id", snapshot.UserID+1).Error)

	_, err = VerifyTaskBillingAttemptPreconsumed(snapshot.RequestID)
	assert.ErrorIs(t, err, ErrTaskBillingIdentityDrift)
}

func TestFundingRefundMutationAndMarkerAreAtomic(t *testing.T) {
	truncateTables(t)
	snapshot := billingSnapshot("funding-refund-atomic", 125)
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)

	restore := setTaskBillingFailpointForTest(func(operation, point string) error {
		if operation == "funding_refund" && point == "after_balance_mutation" {
			return errors.New("crash after funding refund")
		}
		return nil
	})
	_, err = ApplyTaskFundingRefund(snapshot.RequestID)
	restore()
	require.Error(t, err)
	assert.Equal(t, 875, loadBillingUserQuota(t, snapshot.UserID))
	assert.Zero(t, loadBillingAttempt(t, snapshot.RequestID).FundingRefundedAt)

	_, err = ApplyTaskFundingRefund(snapshot.RequestID)
	require.NoError(t, err)
	assert.Equal(t, 1_000, loadBillingUserQuota(t, snapshot.UserID))
	assert.NotZero(t, loadBillingAttempt(t, snapshot.RequestID).FundingRefundedAt)
}

func TestTokenRefundMutationAndMarkerAreAtomic(t *testing.T) {
	truncateTables(t)
	snapshot := billingSnapshot("token-refund-atomic", 125)
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	_, err = ApplyTaskTokenPreconsume(snapshot.RequestID)
	require.NoError(t, err)

	restore := setTaskBillingFailpointForTest(func(operation, point string) error {
		if operation == "token_refund" && point == "after_balance_mutation" {
			return errors.New("crash after token refund")
		}
		return nil
	})
	_, err = ApplyTaskTokenRefund(snapshot.RequestID)
	restore()
	require.Error(t, err)
	token := loadBillingToken(t, snapshot.TokenID)
	assert.Equal(t, 875, token.RemainQuota)
	assert.Equal(t, 125, token.UsedQuota)
	assert.Zero(t, loadBillingAttempt(t, snapshot.RequestID).TokenRefundedAt)

	_, err = ApplyTaskTokenRefund(snapshot.RequestID)
	require.NoError(t, err)
	token = loadBillingToken(t, snapshot.TokenID)
	assert.Equal(t, 1_000, token.RemainQuota)
	assert.Zero(t, token.UsedQuota)
	assert.NotZero(t, loadBillingAttempt(t, snapshot.RequestID).TokenRefundedAt)
}

func TestRefundCommitReadAfterErrorDoesNotReplay(t *testing.T) {
	truncateTables(t)
	snapshot := billingSnapshot("refund-readback", 100)
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)

	var once sync.Once
	restore := setTaskBillingFailpointForTest(func(operation, point string) error {
		var err error
		if operation == "funding_refund" && point == "after_commit" {
			once.Do(func() { err = errors.New("ambiguous refund commit") })
		}
		return err
	})
	_, err = ApplyTaskFundingRefund(snapshot.RequestID)
	restore()
	require.NoError(t, err)
	_, err = ApplyTaskFundingRefund(snapshot.RequestID)
	require.NoError(t, err)
	assert.Equal(t, 1_000, loadBillingUserQuota(t, snapshot.UserID))
}

func TestRefundCompletionRequiresBothMarkers(t *testing.T) {
	truncateTables(t)
	snapshot := billingSnapshot("refund-needs-both", 100)
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	_, err = ApplyTaskTokenPreconsume(snapshot.RequestID)
	require.NoError(t, err)

	_, err = ApplyTaskFundingRefund(snapshot.RequestID)
	require.NoError(t, err)
	assert.Zero(t, loadBillingAttempt(t, snapshot.RequestID).RefundCompletedAt)
	_, err = ApplyTaskTokenRefund(snapshot.RequestID)
	require.NoError(t, err)
	assert.NotZero(t, loadBillingAttempt(t, snapshot.RequestID).RefundCompletedAt)
}

func TestConcurrentAttemptRefundAppliesEachComponentOnce(t *testing.T) {
	truncateTables(t)
	snapshot := billingSnapshot("concurrent-refund", 100)
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	_, err = ApplyTaskTokenPreconsume(snapshot.RequestID)
	require.NoError(t, err)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, applyErr := ApplyTaskFundingRefund(snapshot.RequestID)
			errs <- applyErr
		}()
		go func() {
			defer wg.Done()
			_, applyErr := ApplyTaskTokenRefund(snapshot.RequestID)
			errs <- applyErr
		}()
	}
	wg.Wait()
	close(errs)
	for applyErr := range errs {
		require.NoError(t, applyErr)
	}
	assert.Equal(t, 1_000, loadBillingUserQuota(t, snapshot.UserID))
	token := loadBillingToken(t, snapshot.TokenID)
	assert.Equal(t, 1_000, token.RemainQuota)
	assert.Zero(t, token.UsedQuota)
	assert.NotZero(t, loadBillingAttempt(t, snapshot.RequestID).RefundCompletedAt)
}

func TestBillingComponentCrashPointMatrix(t *testing.T) {
	testCases := []struct {
		operation string
		apply     func(string) (TaskBillingApplyResult, error)
		prepare   func(t *testing.T, requestID string)
		marker    func(TaskBillingAttempt) int64
	}{
		{
			operation: "funding_preconsume",
			apply:     ApplyTaskFundingPreconsume,
			prepare:   func(*testing.T, string) {},
			marker:    func(attempt TaskBillingAttempt) int64 { return attempt.FundingConsumedAt },
		},
		{
			operation: "token_preconsume",
			apply:     ApplyTaskTokenPreconsume,
			prepare:   func(*testing.T, string) {},
			marker:    func(attempt TaskBillingAttempt) int64 { return attempt.TokenConsumedAt },
		},
		{
			operation: "funding_refund",
			apply:     ApplyTaskFundingRefund,
			prepare: func(t *testing.T, requestID string) {
				_, err := ApplyTaskFundingPreconsume(requestID)
				require.NoError(t, err)
			},
			marker: func(attempt TaskBillingAttempt) int64 { return attempt.FundingRefundedAt },
		},
		{
			operation: "token_refund",
			apply:     ApplyTaskTokenRefund,
			prepare: func(t *testing.T, requestID string) {
				_, err := ApplyTaskTokenPreconsume(requestID)
				require.NoError(t, err)
			},
			marker: func(attempt TaskBillingAttempt) int64 { return attempt.TokenRefundedAt },
		},
	}
	crashPoints := []string{
		"before_balance_update",
		"after_balance_mutation",
		"before_marker",
		"before_commit",
		"after_commit",
	}

	fixtureID := 0
	for _, testCase := range testCases {
		for _, crashPoint := range crashPoints {
			fixtureID++
			t.Run(testCase.operation+"/"+crashPoint, func(t *testing.T) {
				truncateTables(t)
				requestID := fmt.Sprintf("crash-matrix-%d", fixtureID)
				snapshot := billingSnapshot(requestID, 100)
				snapshot.UserID = 1_000 + fixtureID
				snapshot.TokenID = 2_000 + fixtureID
				seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
				_, err := BeginTaskBillingAttempt(snapshot)
				require.NoError(t, err)
				testCase.prepare(t, requestID)

				beforeUser := loadBillingUserQuota(t, snapshot.UserID)
				beforeToken := loadBillingToken(t, snapshot.TokenID)
				restore := setTaskBillingFailpointForTest(func(operation, point string) error {
					if operation == testCase.operation && point == crashPoint {
						return errors.New("injected component crash")
					}
					return nil
				})
				defer restore()
				_, err = testCase.apply(requestID)
				restore()

				if crashPoint == "after_commit" {
					require.NoError(t, err)
					assert.NotZero(t, testCase.marker(loadBillingAttempt(t, requestID)))
				} else {
					require.Error(t, err)
					assert.Zero(t, testCase.marker(loadBillingAttempt(t, requestID)))
					assert.Equal(t, beforeUser, loadBillingUserQuota(t, snapshot.UserID))
					afterToken := loadBillingToken(t, snapshot.TokenID)
					assert.Equal(t, beforeToken.RemainQuota, afterToken.RemainQuota)
					assert.Equal(t, beforeToken.UsedQuota, afterToken.UsedQuota)
				}

				_, err = testCase.apply(requestID)
				require.NoError(t, err)
				assert.NotZero(t, testCase.marker(loadBillingAttempt(t, requestID)))
			})
		}
	}
}

func TestTaskQuotaClearAndSecondRefundMarkerAreAtomic(t *testing.T) {
	truncateTables(t)
	snapshot := billingSnapshot("task-quota-refund-atomic", 100)
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	_, err = ApplyTaskTokenPreconsume(snapshot.RequestID)
	require.NoError(t, err)

	candidate := &Task{
		TaskID:     snapshot.PublicTaskID,
		UserId:     snapshot.UserID,
		Quota:      snapshot.FundingAmount,
		Status:     TaskStatusSubmitting,
		SubmitTime: snapshot.SubmitTime,
		Progress:   "0%",
		PrivateData: TaskPrivateData{
			BillingSource: snapshot.FundingSource,
			TokenId:       snapshot.TokenID,
		},
	}
	task, _, err := PrepareTaskSubmissionAttempt(candidate, 0, snapshot.RequestID)
	require.NoError(t, err)
	task, err = TransitionTaskSubmissionToFailure(
		task.ID, task.TaskID, "", "fixture_failure", "fixture failure",
	)
	require.NoError(t, err)
	_, err = ApplyTaskFundingRefund(snapshot.RequestID)
	require.NoError(t, err)

	restore := setTaskBillingFailpointForTest(func(operation, point string) error {
		if operation == "token_refund" && point == "before_marker" {
			return errors.New("crash before second refund marker")
		}
		return nil
	})
	_, err = ApplyTaskTokenRefund(snapshot.RequestID)
	restore()
	require.Error(t, err)
	var reloadedTask Task
	require.NoError(t, DB.First(&reloadedTask, task.ID).Error)
	assert.Equal(t, snapshot.FundingAmount, reloadedTask.Quota)
	attempt := loadBillingAttempt(t, snapshot.RequestID)
	assert.NotZero(t, attempt.FundingRefundedAt)
	assert.Zero(t, attempt.TokenRefundedAt)
	assert.Zero(t, attempt.RefundCompletedAt)

	_, err = ApplyTaskTokenRefund(snapshot.RequestID)
	require.NoError(t, err)
	require.NoError(t, DB.First(&reloadedTask, task.ID).Error)
	assert.Zero(t, reloadedTask.Quota)
	assert.NotZero(t, loadBillingAttempt(t, snapshot.RequestID).RefundCompletedAt)
}

func TestPaidZeroTaskRefundCompletesWhenQuotaUpdateReportsNoChangedRow(t *testing.T) {
	truncateTables(t)
	snapshot := billingSnapshot("task-paid-zero-refund", 0)
	snapshot.TokenAmount = 0
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 0, 0)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	_, err = ApplyTaskTokenPreconsume(snapshot.RequestID)
	require.NoError(t, err)

	task, _, err := PrepareTaskSubmissionAttempt(&Task{
		TaskID:     snapshot.PublicTaskID,
		UserId:     snapshot.UserID,
		Quota:      0,
		Status:     TaskStatusSubmitting,
		SubmitTime: snapshot.SubmitTime,
		Progress:   "0%",
		PrivateData: TaskPrivateData{
			BillingSource: snapshot.FundingSource,
			TokenId:       snapshot.TokenID,
		},
	}, 0, snapshot.RequestID)
	require.NoError(t, err)
	task, err = TransitionTaskSubmissionToFailure(
		task.ID, task.TaskID, "", "fixture_failure", "fixture failure",
	)
	require.NoError(t, err)
	_, err = ApplyTaskFundingRefund(snapshot.RequestID)
	require.NoError(t, err)

	const callbackName = "task_billing_test_zero_quota_changed_rows"
	emulateChangedRows := true
	require.NoError(t, DB.Callback().Update().After("gorm:update").Register(
		callbackName,
		func(tx *gorm.DB) {
			if emulateChangedRows && tx.Statement != nil && tx.Statement.Table == "tasks" {
				tx.RowsAffected = 0
			}
		},
	))
	t.Cleanup(func() {
		emulateChangedRows = false
	})

	_, err = ApplyTaskTokenRefund(snapshot.RequestID)
	require.NoError(t, err)
	reloaded := loadBillingAttempt(t, snapshot.RequestID)
	assert.NotZero(t, reloaded.TokenRefundedAt)
	assert.NotZero(t, reloaded.RefundCompletedAt)
	require.NoError(t, DB.First(task, task.ID).Error)
	assert.Zero(t, task.Quota)
}

func TestTaskOwnedRefundRejectsFinancialDrift(t *testing.T) {
	truncateTables(t)
	snapshot := billingSnapshot("task-refund-financial-drift", 100)
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	_, err = ApplyTaskTokenPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	task, _, err := PrepareTaskSubmissionAttempt(&Task{
		TaskID:     snapshot.PublicTaskID,
		UserId:     snapshot.UserID,
		Quota:      snapshot.FundingAmount,
		Status:     TaskStatusSubmitting,
		SubmitTime: snapshot.SubmitTime,
		Progress:   "0%",
		PrivateData: TaskPrivateData{
			BillingSource: snapshot.FundingSource,
			TokenId:       snapshot.TokenID,
		},
	}, 0, snapshot.RequestID)
	require.NoError(t, err)
	task, err = TransitionTaskSubmissionToFailure(
		task.ID, task.TaskID, "", "fixture_failure", "fixture failure",
	)
	require.NoError(t, err)
	driftedPrivate := task.PrivateData
	driftedPrivate.TokenId++
	require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).
		Update("private_data", driftedPrivate).Error)

	_, err = ApplyTaskFundingRefund(snapshot.RequestID)
	assert.ErrorIs(t, err, ErrTaskBillingIdentityDrift)
	assert.Equal(t, 900, loadBillingUserQuota(t, snapshot.UserID))
	assert.Zero(t, loadBillingAttempt(t, snapshot.RequestID).FundingRefundedAt)
}

func TestSubscriptionRefundRejectsRecordIdentityDrift(t *testing.T) {
	truncateTables(t)
	now := time.Now().Unix()
	snapshot := billingSnapshot("subscription-record-drift", 100)
	snapshot.FundingSource = "subscription"
	snapshot.SubscriptionID = 706
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	require.NoError(t, DB.Create(&UserSubscription{
		Id:          snapshot.SubscriptionID,
		UserId:      snapshot.UserID,
		AmountTotal: 1_000,
		AmountUsed:  50,
		Status:      "active",
		StartTime:   now - 10,
		EndTime:     now + 3_600,
	}).Error)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	require.NoError(t, DB.Model(&SubscriptionPreConsumeRecord{}).
		Where("request_id = ?", snapshot.RequestID).
		Update("pre_consumed", 99).Error)

	_, err = ApplyTaskFundingRefund(snapshot.RequestID)
	assert.ErrorIs(t, err, ErrTaskBillingIdentityDrift)
	var subscription UserSubscription
	require.NoError(t, DB.First(&subscription, snapshot.SubscriptionID).Error)
	assert.Equal(t, int64(150), subscription.AmountUsed)
	assert.Zero(t, loadBillingAttempt(t, snapshot.RequestID).FundingRefundedAt)
}

func TestDeletedTokenRefundMarksComponentNotApplicable(t *testing.T) {
	truncateTables(t)
	snapshot := billingSnapshot("deleted-token-refund", 100)
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	_, err = ApplyTaskTokenPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	require.NoError(t, DB.Delete(&Token{}, snapshot.TokenID).Error)

	result, err := ApplyTaskTokenRefund(snapshot.RequestID)
	require.NoError(t, err)
	assert.True(t, result.Applied)
	token := loadBillingToken(t, snapshot.TokenID)
	assert.Equal(t, 900, token.RemainQuota)
	assert.Equal(t, 100, token.UsedQuota)
	assert.NotZero(t, loadBillingAttempt(t, snapshot.RequestID).TokenRefundedAt)
}

func TestSubscriptionRecordOlderThanSevenDaysRetainedForActiveAttempt(t *testing.T) {
	truncateTables(t)
	now := time.Now().Unix()
	snapshot := billingSnapshot("subscription-retention", 100)
	snapshot.FundingSource = "subscription"
	snapshot.SubscriptionID = 702
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	require.NoError(t, DB.Create(&UserSubscription{
		Id:          snapshot.SubscriptionID,
		UserId:      snapshot.UserID,
		AmountTotal: 1_000,
		Status:      "active",
		StartTime:   now - 10,
		EndTime:     now + 3_600,
	}).Error)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	require.NoError(t, DB.Model(&SubscriptionPreConsumeRecord{}).
		Where("request_id = ?", snapshot.RequestID).
		Updates(map[string]any{"created_at": now - 8*86_400, "updated_at": now - 8*86_400}).Error)
	require.NoError(t, DB.Model(&TaskBillingAttempt{}).
		Where("request_id = ?", snapshot.RequestID).
		Updates(map[string]any{"created_at": now - 8*86_400, "updated_at": now - 8*86_400}).Error)

	deleted, err := CleanupSubscriptionPreConsumeRecords(7 * 86_400)
	require.NoError(t, err)
	assert.Zero(t, deleted)
	var count int64
	require.NoError(t, DB.Model(&SubscriptionPreConsumeRecord{}).
		Where("request_id = ?", snapshot.RequestID).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestSubscriptionRecordOlderThanSevenDaysRetainedForActiveTaskAttempt(t *testing.T) {
	truncateTables(t)
	now := time.Now().Unix()
	snapshot := billingSnapshot("subscription-task-retention", 100)
	snapshot.FundingSource = "subscription"
	snapshot.SubscriptionID = 705
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	require.NoError(t, DB.Create(&UserSubscription{
		Id:          snapshot.SubscriptionID,
		UserId:      snapshot.UserID,
		AmountTotal: 1_000,
		Status:      "active",
		StartTime:   now - 10,
		EndTime:     now + 3_600,
	}).Error)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	_, err = ApplyTaskTokenPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	_, _, err = PrepareTaskSubmissionAttempt(&Task{
		TaskID:     snapshot.PublicTaskID,
		UserId:     snapshot.UserID,
		Quota:      snapshot.FundingAmount,
		Status:     TaskStatusSubmitting,
		SubmitTime: snapshot.SubmitTime,
		Progress:   "0%",
		PrivateData: TaskPrivateData{
			BillingSource:  snapshot.FundingSource,
			SubscriptionId: snapshot.SubscriptionID,
			TokenId:        snapshot.TokenID,
		},
	}, 0, snapshot.RequestID)
	require.NoError(t, err)
	require.NoError(t, DB.Model(&SubscriptionPreConsumeRecord{}).
		Where("request_id = ?", snapshot.RequestID).
		Updates(map[string]any{"created_at": now - 8*86_400, "updated_at": now - 8*86_400}).Error)
	require.NoError(t, DB.Model(&TaskBillingAttempt{}).
		Where("request_id = ?", snapshot.RequestID).
		Updates(map[string]any{"created_at": now - 8*86_400, "updated_at": now - 8*86_400}).Error)

	deleted, err := CleanupSubscriptionPreConsumeRecords(7 * 86_400)
	require.NoError(t, err)
	assert.Zero(t, deleted)
}

func TestSubscriptionRefundAfterSevenDaysStillCompletes(t *testing.T) {
	truncateTables(t)
	now := time.Now().Unix()
	snapshot := billingSnapshot("subscription-late-refund", 100)
	snapshot.FundingSource = "subscription"
	snapshot.SubscriptionID = 703
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	require.NoError(t, DB.Create(&UserSubscription{
		Id:          snapshot.SubscriptionID,
		UserId:      snapshot.UserID,
		AmountTotal: 1_000,
		AmountUsed:  50,
		Status:      "active",
		StartTime:   now - 10,
		EndTime:     now + 3_600,
	}).Error)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	require.NoError(t, DB.Model(&SubscriptionPreConsumeRecord{}).
		Where("request_id = ?", snapshot.RequestID).
		Updates(map[string]any{"created_at": now - 8*86_400, "updated_at": now - 8*86_400}).Error)

	_, err = CleanupSubscriptionPreConsumeRecords(7 * 86_400)
	require.NoError(t, err)
	_, err = ApplyTaskFundingRefund(snapshot.RequestID)
	require.NoError(t, err)
	var sub UserSubscription
	require.NoError(t, DB.First(&sub, snapshot.SubscriptionID).Error)
	assert.Equal(t, int64(50), sub.AmountUsed)
}

func TestCompletedSubscriptionRefundDoesNotNeedCleanedRecord(t *testing.T) {
	truncateTables(t)
	now := time.Now().Unix()
	snapshot := billingSnapshot("subscription-clean-record", 100)
	snapshot.FundingSource = "subscription"
	snapshot.SubscriptionID = 704
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	require.NoError(t, DB.Create(&UserSubscription{
		Id:          snapshot.SubscriptionID,
		UserId:      snapshot.UserID,
		AmountTotal: 1_000,
		Status:      "active",
		StartTime:   now - 10,
		EndTime:     now + 3_600,
	}).Error)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	_, err = ApplyTaskFundingRefund(snapshot.RequestID)
	require.NoError(t, err)
	require.NoError(t, DB.Where("request_id = ?", snapshot.RequestID).
		Delete(&SubscriptionPreConsumeRecord{}).Error)

	_, err = ApplyTaskFundingRefund(snapshot.RequestID)
	require.NoError(t, err)
}

func TestTaskBillingAttemptLookupNotFoundIsDistinct(t *testing.T) {
	truncateTables(t)
	_, err := GetTaskBillingAttemptByRequestID("missing-attempt")
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestInvalidTaskBillingAttemptLookupDoesNotMasqueradeAsNotFound(t *testing.T) {
	truncateTables(t)
	_, err := GetTaskBillingAttemptByRequestID("")
	require.Error(t, err)
	assert.NotErrorIs(t, err, gorm.ErrRecordNotFound)
	_, err = GetTaskBillingAttemptByTaskID(0)
	require.Error(t, err)
	assert.NotErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestCacheInvalidationFailureDoesNotReplayDurableMutation(t *testing.T) {
	truncateTables(t)
	server := useUserCacheMiniRedis(t)
	snapshot := billingSnapshot("cache-failure-no-replay", 100)
	seedBillingBalances(t, snapshot.UserID, snapshot.TokenID, 1_000, 1_000)
	_, err := BeginTaskBillingAttempt(snapshot)
	require.NoError(t, err)
	server.Close()

	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	_, err = ApplyTaskTokenPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	_, err = ApplyTaskFundingPreconsume(snapshot.RequestID)
	require.NoError(t, err)
	_, err = ApplyTaskTokenPreconsume(snapshot.RequestID)
	require.NoError(t, err)

	assert.Equal(t, 900, loadBillingUserQuota(t, snapshot.UserID))
	token := loadBillingToken(t, snapshot.TokenID)
	assert.Equal(t, 900, token.RemainQuota)
	assert.Equal(t, 100, token.UsedQuota)
	assert.NotZero(t, loadBillingAttempt(t, snapshot.RequestID).PreconsumeCompletedAt)
}
