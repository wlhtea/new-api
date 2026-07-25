package model

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const (
	TaskBillingOwnerRequest = "request"
	TaskBillingOwnerTask    = "task"

	taskBillingFundingWallet       = "wallet"
	taskBillingFundingSubscription = "subscription"

	TaskBillingRequestRecoveryGraceSeconds int64 = 30
)

var (
	ErrTaskBillingAttemptConflict = errors.New("task billing attempt immutable identity conflict")
	ErrTaskBillingAttemptState    = errors.New("task billing attempt state conflict")
	ErrTaskBillingIdentityDrift   = errors.New("task billing identity drift")
	ErrTaskBillingSubjectInUse    = errors.New("task billing subject has an active attempt")
)

// TaskBillingAttemptSnapshot is the immutable financial identity captured before
// any durable task balance mutation.
type TaskBillingAttemptSnapshot struct {
	RequestID      string
	PublicTaskID   string
	SubmitTime     int64
	IsFree         bool
	UserID         int
	FundingSource  string
	SubscriptionID int
	FundingAmount  int
	TokenID        int
	TokenAmount    int
	BillingContext *TaskBillingContext
}

// TaskBillingAttempt is the main-database component ledger for one task
// submission. It deliberately contains no credentials or provider payloads.
type TaskBillingAttempt struct {
	ID        int64  `json:"-" gorm:"primaryKey"`
	RequestID string `json:"-" gorm:"type:varchar(64);uniqueIndex"`
	TaskID    *int64 `json:"-" gorm:"uniqueIndex"`
	Owner     string `json:"-" gorm:"type:varchar(16);index"`

	PublicTaskID   string `json:"-" gorm:"type:varchar(64);index"`
	SubmitTime     int64  `json:"-"`
	IsFree         bool   `json:"-"`
	UserID         int    `json:"-" gorm:"index"`
	FundingSource  string `json:"-" gorm:"type:varchar(32)"`
	SubscriptionID int    `json:"-" gorm:"index"`
	FundingAmount  int    `json:"-"`
	TokenID        int    `json:"-" gorm:"index"`
	TokenAmount    int    `json:"-"`
	// BillingContextDigest is a canonical SHA-256 identity. The ledger never
	// stores Task credentials, prompt/media input, or provider payloads.
	BillingContextDigest string `json:"-" gorm:"type:char(64)"`
	PrepareVersion       int64  `json:"-"`

	FundingConsumedAt     int64 `json:"-" gorm:"index"`
	TokenConsumedAt       int64 `json:"-" gorm:"index"`
	PreconsumeCompletedAt int64 `json:"-" gorm:"index"`
	FundingRefundedAt     int64 `json:"-" gorm:"index"`
	TokenRefundedAt       int64 `json:"-" gorm:"index"`
	RefundStartedAt       int64 `json:"-" gorm:"index"`
	RefundCompletedAt     int64 `json:"-" gorm:"index"`
	OwnerTransferredAt    int64 `json:"-"`
	SubmissionSettledAt   int64 `json:"-" gorm:"index"`
	SucceededAt           int64 `json:"-" gorm:"index"`
	CreatedAt             int64 `json:"-"`
	UpdatedAt             int64 `json:"-" gorm:"index"`
}

type TaskBillingApplyResult struct {
	Applied   bool
	Completed bool
	Owner     string
	TaskID    int64
	UserID    int
	TokenID   int
}

type taskBillingFailpointFunc func(operation, point string) error

var taskBillingFailpointState struct {
	sync.RWMutex
	fn taskBillingFailpointFunc
}

// setTaskBillingFailpointForTest installs a deterministic crash point used by
// transaction integration tests. Production callers never install one.
func setTaskBillingFailpointForTest(fn taskBillingFailpointFunc) func() {
	taskBillingFailpointState.Lock()
	previous := taskBillingFailpointState.fn
	taskBillingFailpointState.fn = fn
	taskBillingFailpointState.Unlock()
	return func() {
		taskBillingFailpointState.Lock()
		taskBillingFailpointState.fn = previous
		taskBillingFailpointState.Unlock()
	}
}

func runTaskBillingFailpoint(operation, point string) error {
	taskBillingFailpointState.RLock()
	fn := taskBillingFailpointState.fn
	taskBillingFailpointState.RUnlock()
	if fn == nil {
		return nil
	}
	return fn(operation, point)
}

func taskBillingTimestamp() int64 {
	now := common.GetTimestamp()
	if now <= 0 {
		return 1
	}
	return now
}

type taskBillingAttemptIdentity struct {
	RequestID            string
	PublicTaskID         string
	SubmitTime           int64
	IsFree               bool
	UserID               int
	FundingSource        string
	SubscriptionID       int
	FundingAmount        int
	TokenID              int
	TokenAmount          int
	BillingContextDigest string
}

const taskBillingContextDigestDomain = "new-api/task-billing-context/v1"

// DigestTaskBillingContext returns a canonical SHA-256 identity over the
// non-secret billing fields needed to verify a durable Task link. It does not
// accept a Task or TaskPrivateData, so credentials and provider/request
// payloads cannot enter the digest input.
func DigestTaskBillingContext(context *TaskBillingContext) (string, error) {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(taskBillingContextDigestDomain))
	_, _ = hasher.Write([]byte{0})

	writeUint64 := func(value uint64) {
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], value)
		_, _ = hasher.Write(encoded[:])
	}
	writeString := func(value string) {
		writeUint64(uint64(len(value)))
		_, _ = hasher.Write([]byte(value))
	}
	canonicalFloat := func(name string, value float64) (uint64, error) {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, fmt.Errorf("%w: %s is not finite", ErrTaskBillingIdentityDrift, name)
		}
		if value == 0 {
			value = 0 // normalize negative zero to the sole zero representation
		}
		return math.Float64bits(value), nil
	}

	if context == nil {
		_, _ = hasher.Write([]byte{0})
		return hex.EncodeToString(hasher.Sum(nil)), nil
	}
	_, _ = hasher.Write([]byte{1})

	modelPrice, err := canonicalFloat("billing model price", context.ModelPrice)
	if err != nil {
		return "", err
	}
	groupRatio, err := canonicalFloat("billing group ratio", context.GroupRatio)
	if err != nil {
		return "", err
	}
	modelRatio, err := canonicalFloat("billing model ratio", context.ModelRatio)
	if err != nil {
		return "", err
	}
	writeUint64(modelPrice)
	writeUint64(groupRatio)
	writeUint64(modelRatio)
	writeString(context.OriginModelName)
	if context.PerCallBilling {
		_, _ = hasher.Write([]byte{1})
	} else {
		_, _ = hasher.Write([]byte{0})
	}

	keys := make([]string, 0, len(context.OtherRatios))
	for key := range context.OtherRatios {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	writeUint64(uint64(len(keys)))
	for _, key := range keys {
		ratio, err := canonicalFloat("billing other ratio "+key, context.OtherRatios[key])
		if err != nil {
			return "", err
		}
		writeString(key)
		writeUint64(ratio)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func validateTaskBillingContextDigest(digest string) error {
	if len(digest) != sha256.Size*2 || strings.ToLower(digest) != digest {
		return fmt.Errorf("%w: invalid billing context digest", ErrTaskBillingIdentityDrift)
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("%w: invalid billing context digest", ErrTaskBillingIdentityDrift)
	}
	return nil
}

func taskBillingIdentityFromSnapshot(
	snapshot TaskBillingAttemptSnapshot,
) (taskBillingAttemptIdentity, error) {
	digest, err := DigestTaskBillingContext(snapshot.BillingContext)
	if err != nil {
		return taskBillingAttemptIdentity{}, err
	}
	identity := taskBillingAttemptIdentity{
		RequestID:            snapshot.RequestID,
		PublicTaskID:         snapshot.PublicTaskID,
		SubmitTime:           snapshot.SubmitTime,
		IsFree:               snapshot.IsFree,
		UserID:               snapshot.UserID,
		FundingSource:        snapshot.FundingSource,
		SubscriptionID:       snapshot.SubscriptionID,
		FundingAmount:        snapshot.FundingAmount,
		TokenID:              snapshot.TokenID,
		TokenAmount:          snapshot.TokenAmount,
		BillingContextDigest: digest,
	}
	if err := validateTaskBillingIdentity(identity); err != nil {
		return taskBillingAttemptIdentity{}, err
	}
	return identity, nil
}

func taskBillingIdentityFromAttempt(attempt *TaskBillingAttempt) taskBillingAttemptIdentity {
	if attempt == nil {
		return taskBillingAttemptIdentity{}
	}
	return taskBillingAttemptIdentity{
		RequestID:            attempt.RequestID,
		PublicTaskID:         attempt.PublicTaskID,
		SubmitTime:           attempt.SubmitTime,
		IsFree:               attempt.IsFree,
		UserID:               attempt.UserID,
		FundingSource:        attempt.FundingSource,
		SubscriptionID:       attempt.SubscriptionID,
		FundingAmount:        attempt.FundingAmount,
		TokenID:              attempt.TokenID,
		TokenAmount:          attempt.TokenAmount,
		BillingContextDigest: attempt.BillingContextDigest,
	}
}

func validateTaskBillingIdentity(identity taskBillingAttemptIdentity) error {
	if identity.RequestID == "" || strings.TrimSpace(identity.RequestID) != identity.RequestID {
		return fmt.Errorf("%w: request id is empty or not canonical", ErrTaskBillingIdentityDrift)
	}
	if len(identity.RequestID) > 64 {
		return fmt.Errorf("%w: request id is too long", ErrTaskBillingIdentityDrift)
	}
	if identity.PublicTaskID == "" || strings.TrimSpace(identity.PublicTaskID) != identity.PublicTaskID {
		return fmt.Errorf("%w: public task id is empty or not canonical", ErrTaskBillingIdentityDrift)
	}
	if len(identity.PublicTaskID) > 64 || identity.SubmitTime <= 0 {
		return fmt.Errorf("%w: invalid public task identity", ErrTaskBillingIdentityDrift)
	}
	if identity.FundingAmount < 0 || identity.TokenAmount < 0 || identity.TokenID < 0 {
		return fmt.Errorf("%w: negative amount or token identity", ErrTaskBillingIdentityDrift)
	}
	if identity.FundingAmount > common.MaxQuota || identity.TokenAmount > common.MaxQuota {
		return fmt.Errorf("%w: amount exceeds database quota range", ErrTaskBillingIdentityDrift)
	}
	if err := validateTaskBillingContextDigest(identity.BillingContextDigest); err != nil {
		return err
	}
	if identity.IsFree {
		if identity.UserID < 0 || identity.FundingSource != "" || identity.SubscriptionID != 0 ||
			identity.FundingAmount != 0 || identity.TokenAmount != 0 {
			return fmt.Errorf("%w: invalid free attempt shape", ErrTaskBillingIdentityDrift)
		}
		return nil
	}
	if identity.UserID <= 0 {
		return fmt.Errorf("%w: paid attempt has no user", ErrTaskBillingIdentityDrift)
	}
	switch identity.FundingSource {
	case taskBillingFundingWallet:
		if identity.SubscriptionID != 0 {
			return fmt.Errorf("%w: wallet attempt has a subscription", ErrTaskBillingIdentityDrift)
		}
	case taskBillingFundingSubscription:
		if identity.SubscriptionID <= 0 {
			return fmt.Errorf("%w: subscription attempt has no subscription", ErrTaskBillingIdentityDrift)
		}
	default:
		return fmt.Errorf("%w: invalid funding source", ErrTaskBillingIdentityDrift)
	}
	if identity.TokenAmount > 0 && identity.TokenID <= 0 {
		return fmt.Errorf("%w: token amount has no token", ErrTaskBillingIdentityDrift)
	}
	return nil
}

func validateTaskBillingAttempt(attempt *TaskBillingAttempt) error {
	if attempt == nil {
		return fmt.Errorf("%w: attempt is nil", ErrTaskBillingIdentityDrift)
	}
	if err := validateTaskBillingIdentity(taskBillingIdentityFromAttempt(attempt)); err != nil {
		return err
	}
	switch attempt.Owner {
	case TaskBillingOwnerRequest:
		if attempt.TaskID != nil || attempt.OwnerTransferredAt != 0 || attempt.PrepareVersion != 0 {
			return fmt.Errorf("%w: request owner has a task link", ErrTaskBillingIdentityDrift)
		}
	case TaskBillingOwnerTask:
		if attempt.TaskID == nil || *attempt.TaskID <= 0 || attempt.OwnerTransferredAt == 0 ||
			attempt.PrepareVersion <= 0 {
			return fmt.Errorf("%w: task owner has no task link", ErrTaskBillingIdentityDrift)
		}
	default:
		return fmt.Errorf("%w: invalid owner", ErrTaskBillingIdentityDrift)
	}
	preconsumeComplete := attempt.FundingConsumedAt != 0 && attempt.TokenConsumedAt != 0
	if (attempt.PreconsumeCompletedAt != 0) != preconsumeComplete {
		return fmt.Errorf("%w: invalid preconsume completion marker", ErrTaskBillingIdentityDrift)
	}
	refundComplete := attempt.FundingRefundedAt != 0 && attempt.TokenRefundedAt != 0
	if (attempt.RefundCompletedAt != 0) != refundComplete {
		return fmt.Errorf("%w: invalid refund completion marker", ErrTaskBillingIdentityDrift)
	}
	if attempt.SucceededAt != 0 &&
		(attempt.RefundStartedAt != 0 || attempt.FundingRefundedAt != 0 ||
			attempt.TokenRefundedAt != 0 || attempt.RefundCompletedAt != 0) {
		return fmt.Errorf("%w: succeeded and refunded markers conflict", ErrTaskBillingIdentityDrift)
	}
	return nil
}

func sameTaskBillingSnapshot(attempt *TaskBillingAttempt, snapshot TaskBillingAttemptSnapshot) error {
	identity, err := taskBillingIdentityFromSnapshot(snapshot)
	if err != nil {
		return err
	}
	if taskBillingIdentityFromAttempt(attempt) != identity {
		return ErrTaskBillingAttemptConflict
	}
	return nil
}

// lockTaskBillingBeginSubjects runs after the new ledger row has been inserted
// in the same transaction. This preserves the global ledger-before-balance-row
// lock order while synchronizing with hard deletion gates. If deletion wins the
// subject lock first, the lookup fails and the uncommitted ledger is rolled
// back; if Begin wins, deletion observes the committed active attempt.
func lockTaskBillingBeginSubjects(tx *gorm.DB, snapshot TaskBillingAttemptSnapshot) error {
	if tx == nil {
		return ErrTaskBillingIdentityDrift
	}
	if snapshot.UserID > 0 {
		var user User
		if err := lockForUpdate(tx).
			Select("id").
			Where("id = ?", snapshot.UserID).
			First(&user).Error; err != nil {
			return err
		}
	}
	if snapshot.FundingSource == taskBillingFundingSubscription {
		var subscription UserSubscription
		if err := lockForUpdate(tx).
			Select("id", "user_id").
			Where("id = ? AND user_id = ?", snapshot.SubscriptionID, snapshot.UserID).
			First(&subscription).Error; err != nil {
			return err
		}
	}
	return nil
}

func hasActiveTaskBillingAttempt(
	tx *gorm.DB,
	column string,
	subjectID int,
) (bool, error) {
	if tx == nil || subjectID <= 0 {
		return false, ErrTaskBillingIdentityDrift
	}
	switch column {
	case "user_id", "subscription_id":
	default:
		return false, ErrTaskBillingIdentityDrift
	}
	var attemptID int64
	query := tx.Model(&TaskBillingAttempt{}).
		Select("id").
		Where(column+" = ?", subjectID).
		Where("succeeded_at = ? AND refund_completed_at = ?", 0, 0).
		Order("id").
		Limit(1).
		Scan(&attemptID)
	if query.Error != nil {
		return false, query.Error
	}
	return attemptID != 0, nil
}

func BeginTaskBillingAttempt(snapshot TaskBillingAttemptSnapshot) (*TaskBillingAttempt, error) {
	identity, err := taskBillingIdentityFromSnapshot(snapshot)
	if err != nil {
		return nil, err
	}

	var result TaskBillingAttempt
	err = DB.Transaction(func(tx *gorm.DB) error {
		query := lockForUpdate(tx).
			Where("request_id = ?", snapshot.RequestID).
			Limit(1).
			Find(&result)
		if query.Error != nil {
			return query.Error
		}
		if query.RowsAffected > 0 {
			if err := validateTaskBillingAttempt(&result); err != nil {
				return err
			}
			return sameTaskBillingSnapshot(&result, snapshot)
		}

		now := taskBillingTimestamp()
		result = TaskBillingAttempt{
			RequestID:            snapshot.RequestID,
			Owner:                TaskBillingOwnerRequest,
			PublicTaskID:         snapshot.PublicTaskID,
			SubmitTime:           snapshot.SubmitTime,
			IsFree:               snapshot.IsFree,
			UserID:               snapshot.UserID,
			FundingSource:        snapshot.FundingSource,
			SubscriptionID:       snapshot.SubscriptionID,
			FundingAmount:        snapshot.FundingAmount,
			TokenID:              snapshot.TokenID,
			TokenAmount:          snapshot.TokenAmount,
			BillingContextDigest: identity.BillingContextDigest,
			CreatedAt:            now,
			UpdatedAt:            now,
		}
		if snapshot.IsFree {
			result.FundingConsumedAt = now
			result.TokenConsumedAt = now
			result.PreconsumeCompletedAt = now
		}
		if err := tx.Create(&result).Error; err != nil {
			return err
		}
		return lockTaskBillingBeginSubjects(tx, snapshot)
	})
	if err == nil {
		return &result, nil
	}

	// A concurrent INSERT may have won the unique RequestID. Resolve only from
	// the primary ledger row and still reject any immutable drift.
	existing, readErr := GetTaskBillingAttemptByRequestID(snapshot.RequestID)
	if readErr != nil {
		return nil, err
	}
	if validationErr := validateTaskBillingAttempt(existing); validationErr != nil {
		return nil, validationErr
	}
	if conflictErr := sameTaskBillingSnapshot(existing, snapshot); conflictErr != nil {
		return nil, conflictErr
	}
	return existing, nil
}

func GetTaskBillingAttemptByRequestID(requestID string) (*TaskBillingAttempt, error) {
	if requestID == "" {
		return nil, fmt.Errorf("%w: empty request id", ErrTaskBillingIdentityDrift)
	}
	var attempt TaskBillingAttempt
	if err := DB.Where("request_id = ?", requestID).First(&attempt).Error; err != nil {
		return nil, err
	}
	return &attempt, nil
}

func GetTaskBillingAttemptByTaskID(taskID int64) (*TaskBillingAttempt, error) {
	if taskID <= 0 {
		return nil, fmt.Errorf("%w: invalid task id", ErrTaskBillingIdentityDrift)
	}
	var attempt TaskBillingAttempt
	if err := DB.Where("task_id = ?", taskID).First(&attempt).Error; err != nil {
		return nil, err
	}
	return &attempt, nil
}

func taskBillingRecoveryQuery(requestStaleBefore, submittingBefore int64) *gorm.DB {
	return DB.Table("task_billing_attempts").
		Joins("LEFT JOIN tasks ON tasks.id = task_billing_attempts.task_id").
		Where("task_billing_attempts.refund_completed_at = ? AND task_billing_attempts.succeeded_at = ?", 0, 0).
		Where(
			"(task_billing_attempts.owner = ? AND task_billing_attempts.task_id IS NULL AND task_billing_attempts.updated_at <= ?) OR "+
				"(task_billing_attempts.owner = ? AND task_billing_attempts.task_id IS NOT NULL AND tasks.status = ?) OR "+
				"(task_billing_attempts.owner = ? AND task_billing_attempts.task_id IS NOT NULL AND tasks.status = ? AND tasks.submit_time <= ?)",
			TaskBillingOwnerRequest,
			requestStaleBefore,
			TaskBillingOwnerTask,
			TaskStatusFailure,
			TaskBillingOwnerTask,
			TaskStatusSubmitting,
			submittingBefore,
		)
}

func ListRecoverableTaskBillingAttempts(
	requestStaleBefore int64,
	submittingBefore int64,
	limit int,
) ([]TaskBillingAttempt, error) {
	if limit <= 0 {
		return nil, nil
	}
	var attempts []TaskBillingAttempt
	err := taskBillingRecoveryQuery(requestStaleBefore, submittingBefore).
		Select("task_billing_attempts.*").
		Order("task_billing_attempts.id").
		Limit(limit).
		Find(&attempts).Error
	return attempts, err
}

func HasRecoverableTaskBillingAttempts(
	requestStaleBefore int64,
	submittingBefore int64,
) (bool, error) {
	var id int64
	query := taskBillingRecoveryQuery(requestStaleBefore, submittingBefore).
		Select("task_billing_attempts.id").
		Limit(1).
		Scan(&id)
	if query.Error != nil {
		return false, query.Error
	}
	return id != 0, nil
}

func taskBillingApplyResult(attempt *TaskBillingAttempt, applied, completed bool) TaskBillingApplyResult {
	result := TaskBillingApplyResult{
		Applied:   applied,
		Completed: completed,
	}
	if attempt == nil {
		return result
	}
	result.Owner = attempt.Owner
	result.UserID = attempt.UserID
	result.TokenID = attempt.TokenID
	if attempt.TaskID != nil {
		result.TaskID = *attempt.TaskID
	}
	return result
}

func updatePreconsumeCompletion(attempt *TaskBillingAttempt, now int64) {
	if attempt.FundingConsumedAt == 0 || attempt.TokenConsumedAt == 0 ||
		attempt.PreconsumeCompletedAt != 0 {
		return
	}
	attempt.PreconsumeCompletedAt = now
}

func applyTaskFundingPreconsumeTx(requestID string) (*TaskBillingAttempt, bool, bool, error) {
	const operation = "funding_preconsume"
	var result TaskBillingAttempt
	applied := false
	mayHaveCommitted := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where("request_id = ?", requestID).First(&result).Error; err != nil {
			return err
		}
		if err := validateTaskBillingAttempt(&result); err != nil {
			return err
		}
		if result.RefundStartedAt != 0 || result.RefundCompletedAt != 0 || result.SucceededAt != 0 {
			return ErrTaskBillingAttemptState
		}
		if result.FundingConsumedAt != 0 {
			return nil
		}
		if result.Owner != TaskBillingOwnerRequest || result.TaskID != nil {
			return ErrTaskBillingAttemptState
		}
		if err := runTaskBillingFailpoint(operation, "before_balance_update"); err != nil {
			return err
		}

		switch result.FundingSource {
		case taskBillingFundingWallet:
			var user User
			if err := lockForUpdate(tx).Where("id = ?", result.UserID).First(&user).Error; err != nil {
				return err
			}
			if result.FundingAmount > 0 {
				if user.Quota < result.FundingAmount {
					return fmt.Errorf("wallet quota insufficient, remain=%d need=%d", user.Quota, result.FundingAmount)
				}
				update := tx.Model(&User{}).Where("id = ?", result.UserID).
					Update("quota", gorm.Expr("quota - ?", result.FundingAmount))
				if update.Error != nil {
					return update.Error
				}
				if update.RowsAffected != 1 {
					return fmt.Errorf("%w: wallet row disappeared", ErrTaskBillingIdentityDrift)
				}
			}
		case taskBillingFundingSubscription:
			var existing SubscriptionPreConsumeRecord
			query := lockForUpdate(tx).Where("request_id = ?", result.RequestID).Limit(1).Find(&existing)
			if query.Error != nil {
				return query.Error
			}
			if query.RowsAffected != 0 {
				return fmt.Errorf("%w: preconsume record predates ledger marker", ErrTaskBillingIdentityDrift)
			}
			var subscription UserSubscription
			if err := lockForUpdate(tx).
				Where("id = ? AND user_id = ?", result.SubscriptionID, result.UserID).
				First(&subscription).Error; err != nil {
				return err
			}
			now := taskBillingTimestamp()
			if subscription.Status != "active" ||
				(subscription.EndTime > 0 && subscription.EndTime <= now) {
				return errors.New("subscription is not active")
			}
			if subscription.PlanId > 0 {
				plan, err := getSubscriptionPlanForTaskBilling(tx, subscription.PlanId)
				if err != nil {
					return err
				}
				if err := maybeResetUserSubscriptionWithPlanTx(tx, &subscription, plan, now); err != nil {
					return err
				}
			}
			if subscription.QuotaResetVersion < 0 {
				return ErrTaskBillingIdentityDrift
			}
			if subscription.AmountTotal > 0 &&
				subscription.AmountTotal-subscription.AmountUsed < int64(result.FundingAmount) {
				return fmt.Errorf("subscription quota insufficient, need=%d", result.FundingAmount)
			}
			record := SubscriptionPreConsumeRecord{
				RequestId:                result.RequestID,
				UserId:                   result.UserID,
				UserSubscriptionId:       result.SubscriptionID,
				PreConsumed:              int64(result.FundingAmount),
				SubscriptionResetVersion: common.GetPointer(subscription.QuotaResetVersion),
				Status:                   "consumed",
			}
			if err := tx.Create(&record).Error; err != nil {
				return err
			}
			if result.FundingAmount > 0 {
				update := tx.Model(&UserSubscription{}).
					Where("id = ? AND user_id = ?", result.SubscriptionID, result.UserID).
					Updates(map[string]any{
						"amount_used": gorm.Expr("amount_used + ?", result.FundingAmount),
						"updated_at":  taskBillingTimestamp(),
					})
				if update.Error != nil {
					return update.Error
				}
				if update.RowsAffected != 1 {
					return fmt.Errorf("%w: subscription row disappeared", ErrTaskBillingIdentityDrift)
				}
			}
		default:
			return fmt.Errorf("%w: invalid funding source", ErrTaskBillingIdentityDrift)
		}
		if err := runTaskBillingFailpoint(operation, "after_balance_mutation"); err != nil {
			return err
		}
		if err := runTaskBillingFailpoint(operation, "before_marker"); err != nil {
			return err
		}
		now := taskBillingTimestamp()
		result.FundingConsumedAt = now
		updatePreconsumeCompletion(&result, now)
		result.UpdatedAt = now
		update := tx.Model(&TaskBillingAttempt{}).
			Where("id = ? AND funding_consumed_at = ?", result.ID, 0).
			Updates(map[string]any{
				"funding_consumed_at":     result.FundingConsumedAt,
				"preconsume_completed_at": result.PreconsumeCompletedAt,
				"updated_at":              result.UpdatedAt,
			})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrTaskBillingAttemptState
		}
		applied = true
		if err := runTaskBillingFailpoint(operation, "before_commit"); err != nil {
			return err
		}
		mayHaveCommitted = true
		return nil
	})
	if err == nil {
		mayHaveCommitted = true
		if hookErr := runTaskBillingFailpoint(operation, "after_commit"); hookErr != nil {
			err = hookErr
		}
	}
	return &result, applied, mayHaveCommitted, err
}

func ApplyTaskFundingPreconsume(requestID string) (TaskBillingApplyResult, error) {
	attempt, applied, mayHaveCommitted, err := applyTaskFundingPreconsumeTx(requestID)
	if err != nil {
		if !mayHaveCommitted {
			return TaskBillingApplyResult{}, err
		}
		primary, readErr := GetTaskBillingAttemptByRequestID(requestID)
		if readErr != nil || primary.FundingConsumedAt == 0 {
			return TaskBillingApplyResult{}, err
		}
		if validationErr := validateTaskBillingAttempt(primary); validationErr != nil {
			return TaskBillingApplyResult{}, validationErr
		}
		attempt = primary
		applied = false
	}
	if attempt.FundingSource == taskBillingFundingWallet && attempt.FundingAmount > 0 {
		invalidateTaskBillingUserCache(attempt.UserID)
	}
	return taskBillingApplyResult(attempt, applied, attempt.PreconsumeCompletedAt != 0), nil
}

func applyTaskTokenPreconsumeTx(requestID string) (*TaskBillingAttempt, bool, bool, string, error) {
	const operation = "token_preconsume"
	var result TaskBillingAttempt
	applied := false
	mayHaveCommitted := false
	tokenKey := ""
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where("request_id = ?", requestID).First(&result).Error; err != nil {
			return err
		}
		if err := validateTaskBillingAttempt(&result); err != nil {
			return err
		}
		if result.RefundStartedAt != 0 || result.RefundCompletedAt != 0 || result.SucceededAt != 0 {
			return ErrTaskBillingAttemptState
		}
		if result.TokenConsumedAt != 0 {
			return nil
		}
		if result.Owner != TaskBillingOwnerRequest || result.TaskID != nil {
			return ErrTaskBillingAttemptState
		}
		if err := runTaskBillingFailpoint(operation, "before_balance_update"); err != nil {
			return err
		}
		if result.TokenID > 0 {
			var token Token
			if err := lockForUpdate(tx).
				Where("id = ? AND user_id = ?", result.TokenID, result.UserID).
				First(&token).Error; err != nil {
				return err
			}
			tokenKey = token.Key
			if token.Status != common.TokenStatusEnabled ||
				(token.ExpiredTime != -1 && token.ExpiredTime < taskBillingTimestamp()) {
				return errors.New("token is not active")
			}
			if result.TokenAmount > 0 {
				if !token.UnlimitedQuota && token.RemainQuota < result.TokenAmount {
					return fmt.Errorf("token quota insufficient, remain=%d need=%d", token.RemainQuota, result.TokenAmount)
				}
				if int64(token.UsedQuota)+int64(result.TokenAmount) > int64(common.MaxQuota) {
					return fmt.Errorf("%w: token used quota overflow", ErrTaskBillingIdentityDrift)
				}
				update := tx.Model(&Token{}).
					Where("id = ? AND user_id = ?", result.TokenID, result.UserID).
					Updates(map[string]any{
						"remain_quota":  gorm.Expr("remain_quota - ?", result.TokenAmount),
						"used_quota":    gorm.Expr("used_quota + ?", result.TokenAmount),
						"accessed_time": taskBillingTimestamp(),
					})
				if update.Error != nil {
					return update.Error
				}
				if update.RowsAffected != 1 {
					return fmt.Errorf("%w: token row disappeared", ErrTaskBillingIdentityDrift)
				}
			}
		} else if result.TokenAmount != 0 {
			return ErrTaskBillingIdentityDrift
		}
		if err := runTaskBillingFailpoint(operation, "after_balance_mutation"); err != nil {
			return err
		}
		if err := runTaskBillingFailpoint(operation, "before_marker"); err != nil {
			return err
		}
		now := taskBillingTimestamp()
		result.TokenConsumedAt = now
		updatePreconsumeCompletion(&result, now)
		result.UpdatedAt = now
		update := tx.Model(&TaskBillingAttempt{}).
			Where("id = ? AND token_consumed_at = ?", result.ID, 0).
			Updates(map[string]any{
				"token_consumed_at":       result.TokenConsumedAt,
				"preconsume_completed_at": result.PreconsumeCompletedAt,
				"updated_at":              result.UpdatedAt,
			})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrTaskBillingAttemptState
		}
		applied = true
		if err := runTaskBillingFailpoint(operation, "before_commit"); err != nil {
			return err
		}
		mayHaveCommitted = true
		return nil
	})
	if err == nil {
		mayHaveCommitted = true
		if hookErr := runTaskBillingFailpoint(operation, "after_commit"); hookErr != nil {
			err = hookErr
		}
	}
	return &result, applied, mayHaveCommitted, tokenKey, err
}

func ApplyTaskTokenPreconsume(requestID string) (TaskBillingApplyResult, error) {
	attempt, applied, mayHaveCommitted, tokenKey, err := applyTaskTokenPreconsumeTx(requestID)
	if err != nil {
		if !mayHaveCommitted {
			return TaskBillingApplyResult{}, err
		}
		primary, readErr := GetTaskBillingAttemptByRequestID(requestID)
		if readErr != nil || primary.TokenConsumedAt == 0 {
			return TaskBillingApplyResult{}, err
		}
		if validationErr := validateTaskBillingAttempt(primary); validationErr != nil {
			return TaskBillingApplyResult{}, validationErr
		}
		attempt = primary
		applied = false
	}
	invalidateTaskBillingTokenCacheForID(attempt.TokenID, tokenKey)
	return taskBillingApplyResult(attempt, applied, attempt.PreconsumeCompletedAt != 0), nil
}

func validateTaskBillingAttemptPreconsumed(attempt *TaskBillingAttempt) error {
	if err := validateTaskBillingAttempt(attempt); err != nil {
		return err
	}
	if attempt.FundingConsumedAt == 0 || attempt.TokenConsumedAt == 0 ||
		attempt.PreconsumeCompletedAt == 0 {
		return ErrTaskBillingAttemptState
	}
	if attempt.RefundStartedAt != 0 || attempt.RefundCompletedAt != 0 ||
		attempt.SucceededAt != 0 || attempt.SubmissionSettledAt != 0 {
		return ErrTaskBillingAttemptState
	}
	return nil
}

func verifyTaskBillingAttemptPrimarySubjects(
	db *gorm.DB,
	attempt *TaskBillingAttempt,
) error {
	if db == nil || attempt == nil {
		return ErrTaskBillingIdentityDrift
	}
	if !attempt.IsFree {
		switch attempt.FundingSource {
		case taskBillingFundingWallet:
			var user User
			if err := lockForUpdate(db).
				Where("id = ?", attempt.UserID).
				First(&user).Error; err != nil {
				return err
			}
		case taskBillingFundingSubscription:
			var record SubscriptionPreConsumeRecord
			if err := lockForUpdate(db).
				Where("request_id = ?", attempt.RequestID).
				First(&record).Error; err != nil {
				return err
			}
			if record.UserId != attempt.UserID ||
				record.UserSubscriptionId != attempt.SubscriptionID ||
				record.PreConsumed != int64(attempt.FundingAmount) ||
				record.Status != "consumed" {
				return ErrTaskBillingIdentityDrift
			}
			var subscription UserSubscription
			if err := lockForUpdate(db).
				Where("id = ? AND user_id = ?", attempt.SubscriptionID, attempt.UserID).
				First(&subscription).Error; err != nil {
				return err
			}
		default:
			return ErrTaskBillingIdentityDrift
		}
	}
	if attempt.TokenID > 0 {
		var token Token
		if err := lockForUpdate(db).
			Where("id = ?", attempt.TokenID).
			First(&token).Error; err != nil {
			return err
		}
		if token.UserId != attempt.UserID {
			return ErrTaskBillingIdentityDrift
		}
	} else if attempt.TokenAmount != 0 {
		return ErrTaskBillingIdentityDrift
	}
	return nil
}

func VerifyTaskBillingAttemptPreconsumed(requestID string) (*TaskBillingAttempt, error) {
	attempt, err := GetTaskBillingAttemptByRequestID(requestID)
	if err != nil {
		return nil, err
	}
	if err := validateTaskBillingAttemptPreconsumed(attempt); err != nil {
		return nil, err
	}
	if attempt.Owner != TaskBillingOwnerRequest || attempt.TaskID != nil {
		return nil, ErrTaskBillingAttemptState
	}
	if err := verifyTaskBillingAttemptPrimarySubjects(DB, attempt); err != nil {
		return nil, err
	}
	return attempt, nil
}

// VerifyTaskBillingAttemptPreconsumedForSubmit performs a fresh, uncached
// primary-database proof immediately before a provider POST. It accepts the
// initial request owner and a linked Task owner, but a linked Task must still be
// the exact immutable SUBMITTING/0% provisional record with no upstream ID.
func VerifyTaskBillingAttemptPreconsumedForSubmit(
	requestID string,
) (*TaskBillingAttempt, error) {
	if requestID == "" {
		return nil, ErrTaskBillingIdentityDrift
	}
	var verified TaskBillingAttempt
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).
			Where("request_id = ?", requestID).
			First(&verified).Error; err != nil {
			return err
		}
		if err := validateTaskBillingAttemptPreconsumed(&verified); err != nil {
			return err
		}
		switch verified.Owner {
		case TaskBillingOwnerRequest:
			if verified.TaskID != nil {
				return ErrTaskBillingIdentityDrift
			}
		case TaskBillingOwnerTask:
			if verified.TaskID == nil || *verified.TaskID <= 0 {
				return ErrTaskBillingIdentityDrift
			}
			var task Task
			if err := lockForUpdate(tx).
				Where("id = ? AND task_id = ?", *verified.TaskID, verified.PublicTaskID).
				First(&task).Error; err != nil {
				return err
			}
			if task.Status != TaskStatusSubmitting ||
				task.Progress != "0%" ||
				task.PrivateData.UpstreamTaskID != "" ||
				task.UserId != verified.UserID ||
				task.SubmitTime != verified.SubmitTime ||
				task.Quota != verified.FundingAmount ||
				task.PrivateData.BillingSource != verified.FundingSource ||
				task.PrivateData.SubscriptionId != verified.SubscriptionID ||
				task.PrivateData.TokenId != verified.TokenID {
				return ErrTaskBillingIdentityDrift
			}
			digest, err := DigestTaskBillingContext(task.PrivateData.BillingContext)
			if err != nil {
				return err
			}
			if digest != verified.BillingContextDigest {
				return ErrTaskBillingIdentityDrift
			}
		default:
			return ErrTaskBillingIdentityDrift
		}
		return verifyTaskBillingAttemptPrimarySubjects(tx, &verified)
	})
	if err != nil {
		return nil, err
	}
	return &verified, nil
}

func validateTaskBillingRefundOwner(
	tx *gorm.DB,
	attempt *TaskBillingAttempt,
) (*Task, error) {
	if err := validateTaskBillingAttempt(attempt); err != nil {
		return nil, err
	}
	if attempt.SucceededAt != 0 {
		return nil, ErrTaskBillingAttemptState
	}
	switch attempt.Owner {
	case TaskBillingOwnerRequest:
		if attempt.TaskID != nil {
			return nil, ErrTaskBillingIdentityDrift
		}
		return nil, nil
	case TaskBillingOwnerTask:
		if attempt.TaskID == nil {
			return nil, ErrTaskBillingIdentityDrift
		}
		var task Task
		if err := lockForUpdate(tx).
			Where("id = ? AND task_id = ?", *attempt.TaskID, attempt.PublicTaskID).
			First(&task).Error; err != nil {
			return nil, err
		}
		if task.Status != TaskStatusFailure || task.Progress != "100%" {
			return nil, ErrTaskBillingAttemptState
		}
		if task.UserId != attempt.UserID ||
			task.SubmitTime != attempt.SubmitTime ||
			task.PrivateData.BillingSource != attempt.FundingSource ||
			task.PrivateData.SubscriptionId != attempt.SubscriptionID ||
			task.PrivateData.TokenId != attempt.TokenID {
			return nil, ErrTaskBillingIdentityDrift
		}
		if attempt.RefundCompletedAt == 0 && task.Quota != attempt.FundingAmount {
			return nil, ErrTaskBillingIdentityDrift
		}
		return &task, nil
	default:
		return nil, ErrTaskBillingIdentityDrift
	}
}

func completeTaskBillingRefund(
	tx *gorm.DB,
	attempt *TaskBillingAttempt,
	task *Task,
	now int64,
) error {
	if attempt.FundingRefundedAt == 0 || attempt.TokenRefundedAt == 0 {
		return nil
	}
	if attempt.RefundCompletedAt == 0 {
		attempt.RefundCompletedAt = now
	}
	if task == nil {
		return nil
	}
	if task.Quota == 0 {
		return nil
	}
	update := tx.Model(&Task{}).Where("id = ? AND task_id = ?", task.ID, task.TaskID).
		Update("quota", 0)
	if update.Error != nil {
		return update.Error
	}
	if update.RowsAffected != 1 {
		return ErrTaskBillingIdentityDrift
	}
	task.Quota = 0
	return nil
}

func persistTaskBillingRefundMarkers(tx *gorm.DB, attempt *TaskBillingAttempt) error {
	update := tx.Model(&TaskBillingAttempt{}).
		Where("id = ?", attempt.ID).
		Updates(map[string]any{
			"refund_started_at":   attempt.RefundStartedAt,
			"funding_refunded_at": attempt.FundingRefundedAt,
			"token_refunded_at":   attempt.TokenRefundedAt,
			"refund_completed_at": attempt.RefundCompletedAt,
			"updated_at":          attempt.UpdatedAt,
		})
	if update.Error != nil {
		return update.Error
	}
	if update.RowsAffected != 1 {
		return ErrTaskBillingAttemptState
	}
	return nil
}

func applyTaskFundingRefundTx(requestID string) (*TaskBillingAttempt, bool, bool, error) {
	const operation = "funding_refund"
	var result TaskBillingAttempt
	applied := false
	mayHaveCommitted := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where("request_id = ?", requestID).First(&result).Error; err != nil {
			return err
		}
		task, err := validateTaskBillingRefundOwner(tx, &result)
		if err != nil {
			return err
		}
		if result.FundingRefundedAt != 0 {
			return nil
		}
		now := taskBillingTimestamp()
		if result.RefundStartedAt == 0 {
			result.RefundStartedAt = now
		}
		if err := runTaskBillingFailpoint(operation, "before_balance_update"); err != nil {
			return err
		}

		if result.FundingConsumedAt != 0 && !result.IsFree {
			switch result.FundingSource {
			case taskBillingFundingWallet:
				if result.FundingAmount > 0 {
					var user User
					if err := lockForUpdate(tx.Unscoped()).
						Where("id = ?", result.UserID).
						First(&user).Error; err != nil {
						return err
					}
					if int64(user.Quota)+int64(result.FundingAmount) > int64(common.MaxQuota) {
						return ErrTaskBillingIdentityDrift
					}
					update := tx.Unscoped().Model(&User{}).Where("id = ?", result.UserID).
						Update("quota", gorm.Expr("quota + ?", result.FundingAmount))
					if update.Error != nil {
						return update.Error
					}
					if update.RowsAffected != 1 {
						return ErrTaskBillingIdentityDrift
					}
				}
			case taskBillingFundingSubscription:
				var record SubscriptionPreConsumeRecord
				if err := lockForUpdate(tx).
					Where("request_id = ?", result.RequestID).
					First(&record).Error; err != nil {
					return err
				}
				if record.UserId != result.UserID ||
					record.UserSubscriptionId != result.SubscriptionID ||
					record.PreConsumed != int64(result.FundingAmount) ||
					record.Status != "consumed" {
					return ErrTaskBillingIdentityDrift
				}
				var subscription UserSubscription
				if err := lockForUpdate(tx).
					Where("id = ? AND user_id = ?", result.SubscriptionID, result.UserID).
					First(&subscription).Error; err != nil {
					return err
				}
				if record.SubscriptionResetVersion == nil ||
					*record.SubscriptionResetVersion < 0 ||
					subscription.QuotaResetVersion < 0 ||
					subscription.QuotaResetVersion < *record.SubscriptionResetVersion {
					return ErrTaskBillingIdentityDrift
				}
				if subscription.QuotaResetVersion == *record.SubscriptionResetVersion &&
					result.FundingAmount > 0 {
					if subscription.AmountUsed < int64(result.FundingAmount) {
						return ErrTaskBillingIdentityDrift
					}
					update := tx.Model(&UserSubscription{}).
						Where("id = ? AND user_id = ?", result.SubscriptionID, result.UserID).
						Updates(map[string]any{
							"amount_used": gorm.Expr("amount_used - ?", result.FundingAmount),
							"updated_at":  now,
						})
					if update.Error != nil {
						return update.Error
					}
					if update.RowsAffected != 1 {
						return ErrTaskBillingIdentityDrift
					}
				}
				record.Status = "refunded"
				record.UpdatedAt = now
				update := tx.Model(&SubscriptionPreConsumeRecord{}).
					Where("id = ? AND status = ?", record.Id, "consumed").
					Updates(map[string]any{
						"status":     record.Status,
						"updated_at": record.UpdatedAt,
					})
				if update.Error != nil {
					return update.Error
				}
				if update.RowsAffected != 1 {
					return ErrTaskBillingIdentityDrift
				}
			default:
				return ErrTaskBillingIdentityDrift
			}
		}
		if err := runTaskBillingFailpoint(operation, "after_balance_mutation"); err != nil {
			return err
		}
		if err := runTaskBillingFailpoint(operation, "before_marker"); err != nil {
			return err
		}
		result.FundingRefundedAt = now
		result.UpdatedAt = now
		if err := completeTaskBillingRefund(tx, &result, task, now); err != nil {
			return err
		}
		if err := persistTaskBillingRefundMarkers(tx, &result); err != nil {
			return err
		}
		applied = true
		if err := runTaskBillingFailpoint(operation, "before_commit"); err != nil {
			return err
		}
		mayHaveCommitted = true
		return nil
	})
	if err == nil {
		mayHaveCommitted = true
		if hookErr := runTaskBillingFailpoint(operation, "after_commit"); hookErr != nil {
			err = hookErr
		}
	}
	return &result, applied, mayHaveCommitted, err
}

func ApplyTaskFundingRefund(requestID string) (TaskBillingApplyResult, error) {
	attempt, applied, mayHaveCommitted, err := applyTaskFundingRefundTx(requestID)
	if err != nil {
		if !mayHaveCommitted {
			return TaskBillingApplyResult{}, err
		}
		primary, readErr := GetTaskBillingAttemptByRequestID(requestID)
		if readErr != nil || primary.FundingRefundedAt == 0 {
			return TaskBillingApplyResult{}, err
		}
		if validationErr := validateTaskBillingAttempt(primary); validationErr != nil {
			return TaskBillingApplyResult{}, validationErr
		}
		attempt = primary
		applied = false
	}
	if attempt.FundingSource == taskBillingFundingWallet &&
		attempt.FundingConsumedAt != 0 && attempt.FundingAmount > 0 {
		invalidateTaskBillingUserCache(attempt.UserID)
	}
	return taskBillingApplyResult(attempt, applied, attempt.RefundCompletedAt != 0), nil
}

func applyTaskTokenRefundTx(requestID string) (*TaskBillingAttempt, bool, bool, string, error) {
	const operation = "token_refund"
	var result TaskBillingAttempt
	applied := false
	mayHaveCommitted := false
	tokenKey := ""
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where("request_id = ?", requestID).First(&result).Error; err != nil {
			return err
		}
		task, err := validateTaskBillingRefundOwner(tx, &result)
		if err != nil {
			return err
		}
		if result.TokenRefundedAt != 0 {
			return nil
		}
		now := taskBillingTimestamp()
		if result.RefundStartedAt == 0 {
			result.RefundStartedAt = now
		}
		if err := runTaskBillingFailpoint(operation, "before_balance_update"); err != nil {
			return err
		}
		if result.TokenConsumedAt != 0 && result.TokenAmount > 0 {
			var token Token
			query := lockForUpdate(tx).
				Where("id = ? AND user_id = ?", result.TokenID, result.UserID).
				Limit(1).
				Find(&token)
			if query.Error != nil {
				return query.Error
			}
			if query.RowsAffected > 0 {
				if token.UsedQuota < result.TokenAmount {
					return ErrTaskBillingIdentityDrift
				}
				if int64(token.RemainQuota)+int64(result.TokenAmount) > int64(common.MaxQuota) {
					return ErrTaskBillingIdentityDrift
				}
				update := tx.Model(&Token{}).
					Where("id = ? AND user_id = ?", result.TokenID, result.UserID).
					Updates(map[string]any{
						"remain_quota":  gorm.Expr("remain_quota + ?", result.TokenAmount),
						"used_quota":    gorm.Expr("used_quota - ?", result.TokenAmount),
						"accessed_time": now,
					})
				if update.Error != nil {
					return update.Error
				}
				if update.RowsAffected != 1 {
					return ErrTaskBillingIdentityDrift
				}
				tokenKey = token.Key
			}
		}
		if err := runTaskBillingFailpoint(operation, "after_balance_mutation"); err != nil {
			return err
		}
		if err := runTaskBillingFailpoint(operation, "before_marker"); err != nil {
			return err
		}
		result.TokenRefundedAt = now
		result.UpdatedAt = now
		if err := completeTaskBillingRefund(tx, &result, task, now); err != nil {
			return err
		}
		if err := persistTaskBillingRefundMarkers(tx, &result); err != nil {
			return err
		}
		applied = true
		if err := runTaskBillingFailpoint(operation, "before_commit"); err != nil {
			return err
		}
		mayHaveCommitted = true
		return nil
	})
	if err == nil {
		mayHaveCommitted = true
		if hookErr := runTaskBillingFailpoint(operation, "after_commit"); hookErr != nil {
			err = hookErr
		}
	}
	return &result, applied, mayHaveCommitted, tokenKey, err
}

func ApplyTaskTokenRefund(requestID string) (TaskBillingApplyResult, error) {
	attempt, applied, mayHaveCommitted, tokenKey, err := applyTaskTokenRefundTx(requestID)
	if err != nil {
		if !mayHaveCommitted {
			return TaskBillingApplyResult{}, err
		}
		primary, readErr := GetTaskBillingAttemptByRequestID(requestID)
		if readErr != nil || primary.TokenRefundedAt == 0 {
			return TaskBillingApplyResult{}, err
		}
		if validationErr := validateTaskBillingAttempt(primary); validationErr != nil {
			return TaskBillingApplyResult{}, validationErr
		}
		attempt = primary
		applied = false
	}
	invalidateTaskBillingTokenCacheForID(attempt.TokenID, tokenKey)
	return taskBillingApplyResult(attempt, applied, attempt.RefundCompletedAt != 0), nil
}

func invalidateTaskBillingUserCache(userID int) {
	if userID <= 0 {
		return
	}
	if err := invalidateUserCache(userID); err != nil {
		common.SysLog(fmt.Sprintf("failed to invalidate durable task user cache (userId=%d): %v", userID, err))
	}
}

func invalidateTaskBillingTokenCache(tokenKey string) {
	if tokenKey == "" || !common.RedisEnabled {
		return
	}
	if err := cacheDeleteToken(tokenKey); err != nil {
		common.SysLog("failed to invalidate durable task token cache: " + err.Error())
	}
}

func invalidateTaskBillingTokenCacheForID(tokenID int, tokenKey string) {
	if tokenID <= 0 || !common.RedisEnabled {
		return
	}
	if tokenKey == "" {
		var token Token
		err := DB.Unscoped().
			Select("id", commonKeyCol).
			Where("id = ?", tokenID).
			First(&token).Error
		if err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				common.SysLog(fmt.Sprintf(
					"failed to resolve durable task token cache identity (tokenId=%d): %v",
					tokenID,
					err,
				))
			}
			return
		}
		tokenKey = token.Key
	}
	invalidateTaskBillingTokenCache(tokenKey)
}
