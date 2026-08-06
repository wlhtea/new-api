package model

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"gorm.io/gorm"
)

const (
	OpenCodeGoIdentityStatusPending        = "pending"
	OpenCodeGoIdentityStatusActive         = "active"
	OpenCodeGoIdentityStatusStale          = "stale"
	OpenCodeGoIdentityStatusAuthError      = "auth_error"
	OpenCodeGoIdentityStatusManualDisabled = "manual_disabled"

	OpenCodeGoStatePending           = "pending"
	OpenCodeGoStateEligible          = "eligible"
	OpenCodeGoStateManualDisabled    = "manual_disabled"
	OpenCodeGoStateStale             = "stale"
	OpenCodeGoStateAuthError         = "auth_error"
	OpenCodeGoStateKeyError          = "key_error"
	OpenCodeGoStateMembershipExpired = "membership_expired"
	OpenCodeGoStateQuotaExhausted    = "quota_exhausted"
	OpenCodeGoStateRiskBlocked       = "risk_blocked"
	OpenCodeGoStateBulkDisabled      = "bulk_disabled"
	OpenCodeGoStateCooldown          = "cooldown"

	OpenCodeGoQuotaSnapshotPending  = "pending"
	OpenCodeGoQuotaSnapshotComplete = "complete"
	OpenCodeGoQuotaSnapshotStale    = "stale"
	OpenCodeGoQuotaSnapshotError    = "error"

	OpenCodeGoQuotaRolling = "rolling"
	OpenCodeGoQuotaWeekly  = "weekly"
	OpenCodeGoQuotaMonthly = "monthly"

	OpenCodeGoCredentialPending = "pending"
	OpenCodeGoCredentialValid   = "valid"
	OpenCodeGoCredentialMissing = "missing"
	OpenCodeGoCredentialError   = "error"

	OpenCodeGoMembershipUnknown  = "unknown"
	OpenCodeGoMembershipActive   = "active"
	OpenCodeGoMembershipInactive = "inactive"

	OpenCodeGoModelAvailable     = "available"
	OpenCodeGoModelRegionBlocked = "region_blocked"
	OpenCodeGoModelRPMCooldown   = "rpm_cooldown"
	OpenCodeGoModelTransient     = "transient_cooldown"
	OpenCodeGoModelDisabled      = "disabled"
)

var OpenCodeGoQuotaKinds = []string{
	OpenCodeGoQuotaRolling,
	OpenCodeGoQuotaWeekly,
	OpenCodeGoQuotaMonthly,
}

type OpenCodeGoIdentity struct {
	ID                    int64                 `json:"-" gorm:"primaryKey;autoIncrement"`
	UID                   string                `json:"uid" gorm:"type:varchar(64);uniqueIndex"`
	ChannelID             int                   `json:"-" gorm:"index;uniqueIndex:idx_ocg_identity_channel_fingerprint,priority:1"`
	Label                 string                `json:"label" gorm:"type:varchar(128)"`
	Email                 string                `json:"email" gorm:"type:varchar(254)"`
	AuthCookieCiphertext  string                `json:"-" gorm:"type:text;not null"`
	AuthCookieFingerprint string                `json:"-" gorm:"type:varchar(64);uniqueIndex:idx_ocg_identity_channel_fingerprint,priority:2"`
	Status                string                `json:"status" gorm:"type:varchar(32);index"`
	LastSyncedAt          int64                 `json:"last_synced_at" gorm:"bigint;index"`
	LastError             string                `json:"last_error" gorm:"type:varchar(512)"`
	CreatedAt             int64                 `json:"created_at" gorm:"bigint;index"`
	UpdatedAt             int64                 `json:"updated_at" gorm:"bigint;index"`
	Workspaces            []OpenCodeGoWorkspace `json:"-" gorm:"foreignKey:IdentityID"`
}

type OpenCodeGoWorkspace struct {
	ID                       int64                      `json:"-" gorm:"primaryKey;autoIncrement"`
	UID                      string                     `json:"uid" gorm:"type:varchar(64);uniqueIndex"`
	ChannelID                int                        `json:"-" gorm:"index;uniqueIndex:idx_ocg_workspace_channel_upstream,priority:1"`
	IdentityID               int64                      `json:"-" gorm:"index"`
	UpstreamWorkspaceID      string                     `json:"-" gorm:"column:upstream_workspace_id;type:varchar(96);uniqueIndex:idx_ocg_workspace_channel_upstream,priority:2"`
	Name                     string                     `json:"name" gorm:"type:varchar(191)"`
	Email                    string                     `json:"email" gorm:"type:varchar(254)"`
	APIKeyCiphertext         string                     `json:"-" gorm:"column:api_key_ciphertext;type:text"`
	APIKeyFingerprint        string                     `json:"-" gorm:"column:api_key_fingerprint;type:varchar(64);index"`
	APIKeyPrefix             string                     `json:"api_key_prefix" gorm:"column:api_key_prefix;type:varchar(16)"`
	CredentialStatus         string                     `json:"credential_status" gorm:"type:varchar(32);index"`
	MembershipStatus         string                     `json:"membership_status" gorm:"type:varchar(32);index"`
	SubscriptionReference    string                     `json:"-" gorm:"type:varchar(128)"`
	SubscriptionEndsAt       int64                      `json:"subscription_ends_at" gorm:"bigint;index"`
	RenewalCancelledAt       int64                      `json:"renewal_cancelled_at" gorm:"bigint"`
	RenewalCheckedAt         int64                      `json:"renewal_checked_at" gorm:"bigint"`
	RenewalCancelError       string                     `json:"renewal_cancel_error" gorm:"type:varchar(512)"`
	ManualEnabled            bool                       `json:"manual_enabled" gorm:"not null;index"`
	EffectiveState           string                     `json:"effective_state" gorm:"type:varchar(32);index"`
	StateReason              string                     `json:"state_reason" gorm:"type:varchar(191)"`
	HealthObservation        string                     `json:"health_observation" gorm:"type:varchar(48)"`
	HealthObservedAt         int64                      `json:"health_observed_at" gorm:"bigint;index"`
	CooldownUntil            int64                      `json:"cooldown_until" gorm:"bigint;index"`
	QuotaSnapshotStatus      string                     `json:"quota_snapshot_status" gorm:"type:varchar(32);index"`
	QuotaFetchedAt           int64                      `json:"quota_fetched_at" gorm:"bigint;index"`
	QuotaNextRefreshAt       int64                      `json:"quota_next_refresh_at" gorm:"bigint;index"`
	QuotaRecoveryAt          int64                      `json:"quota_recovery_at" gorm:"bigint;index"`
	QuotaParserVersion       string                     `json:"quota_parser_version" gorm:"type:varchar(32)"`
	QuotaError               string                     `json:"quota_error" gorm:"type:varchar(512)"`
	ChinaModelsEnabled       *bool                      `json:"china_models_enabled"`
	ChinaModelsCheckedAt     int64                      `json:"china_models_checked_at" gorm:"bigint"`
	ChinaModelsError         string                     `json:"china_models_error" gorm:"type:varchar(512)"`
	ReferralCode             string                     `json:"referral_code" gorm:"type:varchar(96)"`
	AvailableReferralRewards int                        `json:"available_referral_rewards"`
	UsedReferralRewards      int                        `json:"used_referral_rewards"`
	ReferralRewardAppliedAt  int64                      `json:"referral_reward_applied_at" gorm:"bigint"`
	RiskDetectedAt           int64                      `json:"risk_detected_at" gorm:"bigint;index"`
	RiskLastCheckedAt        int64                      `json:"risk_last_checked_at" gorm:"bigint"`
	// BulkFailureDetectedAt marks a workspace auto-disabled by repeated
	// persistent provider failures (401/403), awaiting manual verification.
	BulkFailureDetectedAt    int64                      `json:"-" gorm:"bigint;index"`
	LastSyncedAt             int64                      `json:"last_synced_at" gorm:"bigint;index"`
	LastError                string                     `json:"last_error" gorm:"type:varchar(512)"`
	CreatedAt                int64                      `json:"created_at" gorm:"bigint;index"`
	UpdatedAt                int64                      `json:"updated_at" gorm:"bigint;index"`
	QuotaWindows             []OpenCodeGoQuotaWindow    `json:"-" gorm:"foreignKey:WorkspaceID"`
	Models                   []OpenCodeGoWorkspaceModel `json:"-" gorm:"foreignKey:WorkspaceID"`
}

type OpenCodeGoQuotaWindow struct {
	ID           int64   `json:"-" gorm:"primaryKey;autoIncrement"`
	WorkspaceID  int64   `json:"-" gorm:"index;uniqueIndex:idx_ocg_quota_workspace_kind,priority:1"`
	Kind         string  `json:"kind" gorm:"type:varchar(16);uniqueIndex:idx_ocg_quota_workspace_kind,priority:2"`
	UsedPercent  float64 `json:"used_percent"`
	ResetSeconds int64   `json:"reset_seconds" gorm:"bigint"`
	ResetAt      int64   `json:"reset_at" gorm:"bigint;index"`
	FetchedAt    int64   `json:"fetched_at" gorm:"bigint;index"`
}

type OpenCodeGoWorkspaceModel struct {
	ID                int64  `json:"-" gorm:"primaryKey;autoIncrement"`
	WorkspaceID       int64  `json:"-" gorm:"index;uniqueIndex:idx_ocg_workspace_model,priority:1"`
	Model             string `json:"model" gorm:"type:varchar(191);uniqueIndex:idx_ocg_workspace_model,priority:2"`
	Discovered        bool   `json:"discovered" gorm:"not null;index"`
	State             string `json:"state" gorm:"type:varchar(32);index"`
	DisabledUntil     int64  `json:"disabled_until" gorm:"bigint;index"`
	LastErrorCode     string `json:"last_error_code" gorm:"type:varchar(96)"`
	LastError         string `json:"last_error" gorm:"type:varchar(512)"`
	HealthObservation string `json:"health_observation" gorm:"type:varchar(48)"`
	HealthObservedAt  int64  `json:"health_observed_at" gorm:"bigint;index"`
	UpdatedAt         int64  `json:"updated_at" gorm:"bigint;index"`
}

type OpenCodeGoOperation struct {
	ID           int64  `json:"id" gorm:"primaryKey;autoIncrement"`
	UID          string `json:"uid" gorm:"type:varchar(64);uniqueIndex"`
	ChannelID    int    `json:"-" gorm:"index"`
	WorkspaceID  int64  `json:"-" gorm:"index"`
	WorkspaceUID string `json:"workspace_uid" gorm:"type:varchar(64);index"`
	Action       string `json:"action" gorm:"type:varchar(64);index"`
	Source       string `json:"source" gorm:"type:varchar(32);index"`
	Status       string `json:"status" gorm:"type:varchar(32);index"`
	StartedAt    int64  `json:"started_at" gorm:"bigint;index"`
	FinishedAt   int64  `json:"finished_at" gorm:"bigint"`
	Result       string `json:"result" gorm:"type:varchar(512)"`
	Error        string `json:"error" gorm:"type:varchar(512)"`
	CreatedAt    int64  `json:"created_at" gorm:"bigint;index"`
	UpdatedAt    int64  `json:"updated_at" gorm:"bigint;index"`
}

type OpenCodeGoRefreshTarget struct {
	ChannelID   int    `json:"channel_id"`
	IdentityUID string `json:"identity_uid"`
}

type OpenCodeGoModelRecoveryTarget struct {
	ChannelID    int    `json:"channel_id"`
	WorkspaceUID string `json:"workspace_uid"`
	Model        string `json:"model"`
}

type OpenCodeGoRiskRecheckTarget struct {
	ChannelID    int    `json:"channel_id"`
	WorkspaceUID string `json:"workspace_uid"`
}

func setOpenCodeGoCreateTimestamps(createdAt *int64, updatedAt *int64) {
	now := common.GetTimestamp()
	if *createdAt == 0 {
		*createdAt = now
	}
	if *updatedAt == 0 {
		*updatedAt = now
	}
}

func (identity *OpenCodeGoIdentity) BeforeCreate(_ *gorm.DB) error {
	setOpenCodeGoCreateTimestamps(&identity.CreatedAt, &identity.UpdatedAt)
	if identity.Status == "" {
		identity.Status = OpenCodeGoIdentityStatusPending
	}
	return nil
}

func (identity *OpenCodeGoIdentity) BeforeUpdate(_ *gorm.DB) error {
	identity.UpdatedAt = common.GetTimestamp()
	return nil
}

func (workspace *OpenCodeGoWorkspace) BeforeCreate(_ *gorm.DB) error {
	setOpenCodeGoCreateTimestamps(&workspace.CreatedAt, &workspace.UpdatedAt)
	if workspace.EffectiveState == "" {
		workspace.EffectiveState = OpenCodeGoStatePending
	}
	if workspace.QuotaSnapshotStatus == "" {
		workspace.QuotaSnapshotStatus = OpenCodeGoQuotaSnapshotPending
	}
	return nil
}

func (workspace *OpenCodeGoWorkspace) BeforeUpdate(_ *gorm.DB) error {
	workspace.UpdatedAt = common.GetTimestamp()
	return nil
}

func (entry *OpenCodeGoWorkspaceModel) BeforeCreate(_ *gorm.DB) error {
	if entry.State == "" {
		entry.State = OpenCodeGoModelAvailable
	}
	if entry.UpdatedAt == 0 {
		entry.UpdatedAt = common.GetTimestamp()
	}
	return nil
}

func (entry *OpenCodeGoWorkspaceModel) BeforeUpdate(_ *gorm.DB) error {
	entry.UpdatedAt = common.GetTimestamp()
	return nil
}

func (operation *OpenCodeGoOperation) BeforeCreate(_ *gorm.DB) error {
	setOpenCodeGoCreateTimestamps(&operation.CreatedAt, &operation.UpdatedAt)
	return nil
}

func (operation *OpenCodeGoOperation) BeforeUpdate(_ *gorm.DB) error {
	operation.UpdatedAt = common.GetTimestamp()
	return nil
}

func ListOpenCodeGoIdentities(channelID int) ([]OpenCodeGoIdentity, error) {
	var identities []OpenCodeGoIdentity
	err := DB.Where("channel_id = ?", channelID).
		Order("id asc").
		Preload("Workspaces", func(tx *gorm.DB) *gorm.DB { return tx.Order("id asc") }).
		Preload("Workspaces.QuotaWindows", func(tx *gorm.DB) *gorm.DB { return tx.Order("kind asc") }).
		Preload("Workspaces.Models", func(tx *gorm.DB) *gorm.DB { return tx.Order("model asc") }).
		Find(&identities).Error
	return identities, err
}

func GetOpenCodeGoIdentity(channelID int, uid string) (*OpenCodeGoIdentity, error) {
	var identity OpenCodeGoIdentity
	err := DB.Where("channel_id = ? AND uid = ?", channelID, uid).First(&identity).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &identity, err
}

func GetOpenCodeGoIdentityPool(channelID int, uid string) (*OpenCodeGoIdentity, error) {
	var identity OpenCodeGoIdentity
	err := DB.Where("channel_id = ? AND uid = ?", channelID, uid).
		Preload("Workspaces", func(tx *gorm.DB) *gorm.DB { return tx.Order("id asc") }).
		Preload("Workspaces.QuotaWindows", func(tx *gorm.DB) *gorm.DB { return tx.Order("kind asc") }).
		Preload("Workspaces.Models", func(tx *gorm.DB) *gorm.DB { return tx.Order("model asc") }).
		First(&identity).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &identity, err
}

func ListOpenCodeGoChannelIDs() ([]int, error) {
	var channelIDs []int
	err := DB.Model(&Channel{}).
		Where("type = ?", constant.ChannelTypeOpenCodeGo).
		Order("id asc").
		Pluck("id", &channelIDs).Error
	return channelIDs, err
}

func ListOpenCodeGoIdentityUIDs(channelID int) ([]string, error) {
	var identityUIDs []string
	err := DB.Model(&OpenCodeGoIdentity{}).
		Where("channel_id = ?", channelID).
		Order("id asc").
		Pluck("uid", &identityUIDs).Error
	return identityUIDs, err
}

func ListOpenCodeGoOperations(channelID int, limit int) ([]OpenCodeGoOperation, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var operations []OpenCodeGoOperation
	err := DB.Where("channel_id = ?", channelID).
		Order("id desc").
		Limit(limit).
		Find(&operations).Error
	return operations, err
}

func ListOpenCodeGoDueRefreshTargets(now int64, staleBefore int64, limit int) ([]OpenCodeGoRefreshTarget, error) {
	if limit <= 0 {
		limit = 500
	}
	if limit > 5000 {
		limit = 5000
	}
	var targets []OpenCodeGoRefreshTarget
	err := DB.Model(&OpenCodeGoIdentity{}).
		Select("DISTINCT open_code_go_identities.channel_id, open_code_go_identities.uid AS identity_uid").
		Joins("JOIN open_code_go_workspaces ON open_code_go_workspaces.identity_id = open_code_go_identities.id").
		Where("open_code_go_identities.status IN ?", []string{
			OpenCodeGoIdentityStatusActive,
			OpenCodeGoIdentityStatusStale,
		}).
		Where("open_code_go_workspaces.manual_enabled = ?", true).
		Where(
			"open_code_go_workspaces.quota_snapshot_status <> ? OR open_code_go_workspaces.quota_next_refresh_at <= ? OR open_code_go_workspaces.last_synced_at <= ?",
			OpenCodeGoQuotaSnapshotComplete,
			now,
			staleBefore,
		).
		Order("open_code_go_identities.channel_id asc, open_code_go_identities.uid asc").
		Limit(limit).
		Scan(&targets).Error
	return targets, err
}

func ListOpenCodeGoDueModelRecoveryTargets(channelID int, now int64, limit int) ([]OpenCodeGoModelRecoveryTarget, error) {
	if limit <= 0 {
		limit = 500
	}
	if limit > 5000 {
		limit = 5000
	}
	query := DB.Model(&OpenCodeGoWorkspaceModel{}).
		Select("open_code_go_workspaces.channel_id, open_code_go_workspaces.uid AS workspace_uid, open_code_go_workspace_models.model").
		Joins("JOIN open_code_go_workspaces ON open_code_go_workspaces.id = open_code_go_workspace_models.workspace_id").
		Where("open_code_go_workspace_models.discovered = ?", true).
		Where("open_code_go_workspace_models.state IN ?", []string{
			OpenCodeGoModelRegionBlocked,
			OpenCodeGoModelRPMCooldown,
			OpenCodeGoModelTransient,
			OpenCodeGoModelDisabled,
		}).
		Where("open_code_go_workspace_models.disabled_until > 0 AND open_code_go_workspace_models.disabled_until <= ?", now)
	if channelID > 0 {
		query = query.Where("open_code_go_workspaces.channel_id = ?", channelID)
	}
	var targets []OpenCodeGoModelRecoveryTarget
	err := query.
		Order("open_code_go_workspaces.channel_id asc, open_code_go_workspaces.uid asc, open_code_go_workspace_models.model asc").
		Limit(limit).
		Scan(&targets).Error
	return targets, err
}

func ListOpenCodeGoRiskRecheckTargets(channelID int, limit int) ([]OpenCodeGoRiskRecheckTarget, error) {
	if limit <= 0 {
		limit = 500
	}
	if limit > 5000 {
		limit = 5000
	}
	query := DB.Model(&OpenCodeGoWorkspace{}).
		Select("channel_id, uid AS workspace_uid").
		Where("effective_state = ?", OpenCodeGoStateRiskBlocked).
		Where("manual_enabled = ?", true).
		Where("credential_status = ? AND api_key_ciphertext <> ''", OpenCodeGoCredentialValid)
	if channelID > 0 {
		query = query.Where("channel_id = ?", channelID)
	}
	var targets []OpenCodeGoRiskRecheckTarget
	err := query.
		Order("channel_id asc, uid asc").
		Limit(limit).
		Scan(&targets).Error
	return targets, err
}

func GetOpenCodeGoIdentityByFingerprint(channelID int, fingerprint string) (*OpenCodeGoIdentity, error) {
	var identity OpenCodeGoIdentity
	err := DB.Where("channel_id = ? AND auth_cookie_fingerprint = ?", channelID, fingerprint).First(&identity).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &identity, err
}

func GetOpenCodeGoIdentityByID(channelID int, identityID int64) (*OpenCodeGoIdentity, error) {
	var identity OpenCodeGoIdentity
	err := DB.Where("channel_id = ? AND id = ?", channelID, identityID).First(&identity).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &identity, err
}

func GetOpenCodeGoWorkspace(channelID int, uid string) (*OpenCodeGoWorkspace, error) {
	var workspace OpenCodeGoWorkspace
	err := DB.Where("channel_id = ? AND uid = ?", channelID, uid).
		Preload("QuotaWindows").
		Preload("Models").
		First(&workspace).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &workspace, err
}

func DeleteOpenCodeGoIdentity(channelID int, uid string) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		var identity OpenCodeGoIdentity
		if err := tx.Where("channel_id = ? AND uid = ?", channelID, uid).First(&identity).Error; err != nil {
			return err
		}
		return deleteOpenCodeGoIdentitiesTx(tx, []int64{identity.ID})
	})
}

func DeleteOpenCodeGoWorkspace(channelID int, uid string) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		var workspace OpenCodeGoWorkspace
		if err := tx.Where("channel_id = ? AND uid = ?", channelID, uid).First(&workspace).Error; err != nil {
			return err
		}
		return deleteOpenCodeGoWorkspacesTx(tx, []int64{workspace.ID})
	})
}

func DeleteOpenCodeGoNonMemberWorkspaces(channelID int) (int64, error) {
	var deleted int64
	err := DB.Transaction(func(tx *gorm.DB) error {
		var workspaceIDs []int64
		if err := tx.Model(&OpenCodeGoWorkspace{}).
			Where("channel_id = ?", channelID).
			Where("membership_status IS NULL OR membership_status <> ?", OpenCodeGoMembershipActive).
			Pluck("id", &workspaceIDs).Error; err != nil {
			return err
		}
		if len(workspaceIDs) == 0 {
			return nil
		}
		if err := deleteOpenCodeGoWorkspacesTx(tx, workspaceIDs); err != nil {
			return err
		}
		deleted = int64(len(workspaceIDs))

		var identityIDs []int64
		if err := tx.Model(&OpenCodeGoIdentity{}).Where("channel_id = ?", channelID).Pluck("id", &identityIDs).Error; err != nil {
			return err
		}
		for _, identityID := range identityIDs {
			var count int64
			if err := tx.Model(&OpenCodeGoWorkspace{}).Where("identity_id = ?", identityID).Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				if err := tx.Where("id = ?", identityID).Delete(&OpenCodeGoIdentity{}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	return deleted, err
}

func DeleteOpenCodeGoPoolByChannelTx(tx *gorm.DB, channelIDs []int) error {
	if tx == nil || len(channelIDs) == 0 {
		return nil
	}
	var identityIDs []int64
	if err := tx.Model(&OpenCodeGoIdentity{}).Where("channel_id IN ?", channelIDs).Pluck("id", &identityIDs).Error; err != nil {
		return err
	}
	if err := deleteOpenCodeGoIdentitiesTx(tx, identityIDs); err != nil {
		return err
	}
	return tx.Where("channel_id IN ?", channelIDs).Delete(&OpenCodeGoOperation{}).Error
}

func deleteOpenCodeGoIdentitiesTx(tx *gorm.DB, identityIDs []int64) error {
	if len(identityIDs) == 0 {
		return nil
	}
	var workspaceIDs []int64
	if err := tx.Model(&OpenCodeGoWorkspace{}).Where("identity_id IN ?", identityIDs).Pluck("id", &workspaceIDs).Error; err != nil {
		return err
	}
	if err := deleteOpenCodeGoWorkspacesTx(tx, workspaceIDs); err != nil {
		return err
	}
	return tx.Where("id IN ?", identityIDs).Delete(&OpenCodeGoIdentity{}).Error
}

func deleteOpenCodeGoWorkspacesTx(tx *gorm.DB, workspaceIDs []int64) error {
	if len(workspaceIDs) == 0 {
		return nil
	}
	if err := tx.Where("workspace_id IN ?", workspaceIDs).Delete(&OpenCodeGoQuotaWindow{}).Error; err != nil {
		return err
	}
	if err := tx.Where("workspace_id IN ?", workspaceIDs).Delete(&OpenCodeGoWorkspaceModel{}).Error; err != nil {
		return err
	}
	if err := tx.Where("workspace_id IN ?", workspaceIDs).Delete(&OpenCodeGoOperation{}).Error; err != nil {
		return err
	}
	return tx.Where("id IN ?", workspaceIDs).Delete(&OpenCodeGoWorkspace{}).Error
}
