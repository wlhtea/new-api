package service

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

const openCodeGoQuotaSourceConsole = "opencode_console_authoritative"

var openCodeGoCalculatedQuotaLimits = map[string]float64{
	model.OpenCodeGoQuotaRolling: 12,
	model.OpenCodeGoQuotaWeekly:  30,
	model.OpenCodeGoQuotaMonthly: 60,
}

type OpenCodeGoPoolView struct {
	ChannelID              int                      `json:"channel_id"`
	EligibleWorkspaceCount int                      `json:"eligible_workspace_count"`
	CryptoSecretConfigured bool                     `json:"crypto_secret_configured"`
	Identities             []OpenCodeGoIdentityView `json:"identities"`
}

type OpenCodeGoIdentityView struct {
	UID           string                    `json:"uid"`
	Label         string                    `json:"label"`
	Email         string                    `json:"email"`
	Status        string                    `json:"status"`
	ManualEnabled bool                      `json:"manual_enabled"`
	HasAuthCookie bool                      `json:"has_auth_cookie"`
	LastSyncedAt  int64                     `json:"last_synced_at"`
	LastError     string                    `json:"last_error"`
	CreatedAt     int64                     `json:"created_at"`
	UpdatedAt     int64                     `json:"updated_at"`
	Workspaces    []OpenCodeGoWorkspaceView `json:"workspaces"`
}

type OpenCodeGoWorkspaceView struct {
	UID                      string                         `json:"uid"`
	Name                     string                         `json:"name"`
	Email                    string                         `json:"email"`
	HasAPIKey                bool                           `json:"has_api_key"`
	CredentialStatus         string                         `json:"credential_status"`
	MembershipStatus         string                         `json:"membership_status"`
	SubscriptionEndsAt       int64                          `json:"subscription_ends_at"`
	ManualEnabled            bool                           `json:"manual_enabled"`
	EffectiveState           string                         `json:"effective_state"`
	StateReason              string                         `json:"state_reason"`
	CooldownUntil            int64                          `json:"cooldown_until"`
	QuotaSnapshotStatus      string                         `json:"quota_snapshot_status"`
	QuotaFetchedAt           int64                          `json:"quota_fetched_at"`
	QuotaNextRefreshAt       int64                          `json:"quota_next_refresh_at"`
	QuotaParserVersion       string                         `json:"quota_parser_version"`
	QuotaError               string                         `json:"quota_error"`
	QuotaWindows             []OpenCodeGoQuotaWindowView    `json:"quota_windows"`
	Models                   []OpenCodeGoWorkspaceModelView `json:"models"`
	ChinaModelsEnabled       *bool                          `json:"china_models_enabled"`
	ChinaModelsCheckedAt     int64                          `json:"china_models_checked_at"`
	ChinaModelsError         string                         `json:"china_models_error"`
	ReferralCode             string                         `json:"referral_code"`
	AvailableReferralRewards int                            `json:"available_referral_rewards"`
	UsedReferralRewards      int                            `json:"used_referral_rewards"`
	RiskDetectedAt           int64                          `json:"risk_detected_at"`
	RiskLastCheckedAt        int64                          `json:"risk_last_checked_at"`
	LastSyncedAt             int64                          `json:"last_synced_at"`
	LastError                string                         `json:"last_error"`
	CreatedAt                int64                          `json:"created_at"`
	UpdatedAt                int64                          `json:"updated_at"`
}

type OpenCodeGoQuotaWindowView struct {
	Kind                   string  `json:"kind"`
	Source                 string  `json:"source"`
	UsedPercent            float64 `json:"used_percent"`
	RemainingPercent       float64 `json:"remaining_percent"`
	ResetSeconds           int64   `json:"reset_seconds"`
	ResetAt                int64   `json:"reset_at"`
	FetchedAt              int64   `json:"fetched_at"`
	AmountsAuthoritative   bool    `json:"amounts_authoritative"`
	CalculatedLimitUSD     float64 `json:"calculated_limit_usd"`
	CalculatedUsedUSD      float64 `json:"calculated_used_usd"`
	CalculatedRemainingUSD float64 `json:"calculated_remaining_usd"`
}

type OpenCodeGoWorkspaceModelView struct {
	Model         string `json:"model"`
	Discovered    bool   `json:"discovered"`
	State         string `json:"state"`
	DisabledUntil int64  `json:"disabled_until"`
	LastErrorCode string `json:"last_error_code"`
	LastError     string `json:"last_error"`
	UpdatedAt     int64  `json:"updated_at"`
}

func GetOpenCodeGoPoolView(channelID int) (*OpenCodeGoPoolView, error) {
	if err := validateOpenCodeGoPoolChannel(channelID); err != nil {
		return nil, err
	}
	identities, err := model.ListOpenCodeGoIdentities(channelID)
	if err != nil {
		return nil, err
	}
	view := &OpenCodeGoPoolView{
		ChannelID:              channelID,
		EligibleWorkspaceCount: openCodeGoPoolEligibleCount(channelID),
		CryptoSecretConfigured: common.CryptoSecretExplicitlyConfigured,
		Identities:             make([]OpenCodeGoIdentityView, 0, len(identities)),
	}
	for _, identity := range identities {
		identityView := OpenCodeGoIdentityView{
			UID:           identity.UID,
			Label:         identity.Label,
			Email:         identity.Email,
			Status:        identity.Status,
			ManualEnabled: identity.Status != model.OpenCodeGoIdentityStatusManualDisabled,
			HasAuthCookie: identity.AuthCookieCiphertext != "",
			LastSyncedAt:  identity.LastSyncedAt,
			LastError:     sanitizeOpenCodeGoStoredMessage(identity.LastError),
			CreatedAt:     identity.CreatedAt,
			UpdatedAt:     identity.UpdatedAt,
			Workspaces:    make([]OpenCodeGoWorkspaceView, 0, len(identity.Workspaces)),
		}
		for _, workspace := range identity.Workspaces {
			identityView.Workspaces = append(identityView.Workspaces, openCodeGoWorkspaceToView(workspace))
		}
		view.Identities = append(view.Identities, identityView)
	}
	return view, nil
}

func openCodeGoWorkspaceToView(workspace model.OpenCodeGoWorkspace) OpenCodeGoWorkspaceView {
	view := OpenCodeGoWorkspaceView{
		UID:                      workspace.UID,
		Name:                     workspace.Name,
		Email:                    workspace.Email,
		HasAPIKey:                workspace.APIKeyCiphertext != "",
		CredentialStatus:         workspace.CredentialStatus,
		MembershipStatus:         workspace.MembershipStatus,
		SubscriptionEndsAt:       workspace.SubscriptionEndsAt,
		ManualEnabled:            workspace.ManualEnabled,
		EffectiveState:           workspace.EffectiveState,
		StateReason:              sanitizeOpenCodeGoStoredMessage(workspace.StateReason),
		CooldownUntil:            workspace.CooldownUntil,
		QuotaSnapshotStatus:      workspace.QuotaSnapshotStatus,
		QuotaFetchedAt:           workspace.QuotaFetchedAt,
		QuotaNextRefreshAt:       workspace.QuotaNextRefreshAt,
		QuotaParserVersion:       workspace.QuotaParserVersion,
		QuotaError:               sanitizeOpenCodeGoStoredMessage(workspace.QuotaError),
		QuotaWindows:             make([]OpenCodeGoQuotaWindowView, 0, len(workspace.QuotaWindows)),
		Models:                   make([]OpenCodeGoWorkspaceModelView, 0, len(workspace.Models)),
		ChinaModelsEnabled:       workspace.ChinaModelsEnabled,
		ChinaModelsCheckedAt:     workspace.ChinaModelsCheckedAt,
		ChinaModelsError:         sanitizeOpenCodeGoStoredMessage(workspace.ChinaModelsError),
		ReferralCode:             workspace.ReferralCode,
		AvailableReferralRewards: workspace.AvailableReferralRewards,
		UsedReferralRewards:      workspace.UsedReferralRewards,
		RiskDetectedAt:           workspace.RiskDetectedAt,
		RiskLastCheckedAt:        workspace.RiskLastCheckedAt,
		LastSyncedAt:             workspace.LastSyncedAt,
		LastError:                sanitizeOpenCodeGoStoredMessage(workspace.LastError),
		CreatedAt:                workspace.CreatedAt,
		UpdatedAt:                workspace.UpdatedAt,
	}
	for _, window := range workspace.QuotaWindows {
		limit := openCodeGoCalculatedQuotaLimits[window.Kind]
		remainingPercent := math.Max(0, 100-window.UsedPercent)
		view.QuotaWindows = append(view.QuotaWindows, OpenCodeGoQuotaWindowView{
			Kind:                   window.Kind,
			Source:                 openCodeGoQuotaSourceConsole,
			UsedPercent:            window.UsedPercent,
			RemainingPercent:       remainingPercent,
			ResetSeconds:           window.ResetSeconds,
			ResetAt:                window.ResetAt,
			FetchedAt:              window.FetchedAt,
			AmountsAuthoritative:   false,
			CalculatedLimitUSD:     limit,
			CalculatedUsedUSD:      roundOpenCodeGoAmount(limit * window.UsedPercent / 100),
			CalculatedRemainingUSD: roundOpenCodeGoAmount(limit * remainingPercent / 100),
		})
	}
	for _, entry := range workspace.Models {
		view.Models = append(view.Models, OpenCodeGoWorkspaceModelView{
			Model:         entry.Model,
			Discovered:    entry.Discovered,
			State:         entry.State,
			DisabledUntil: entry.DisabledUntil,
			LastErrorCode: entry.LastErrorCode,
			LastError:     sanitizeOpenCodeGoStoredMessage(entry.LastError),
			UpdatedAt:     entry.UpdatedAt,
		})
	}
	sort.Slice(view.QuotaWindows, func(i, j int) bool {
		return openCodeGoQuotaKindOrder(view.QuotaWindows[i].Kind) < openCodeGoQuotaKindOrder(view.QuotaWindows[j].Kind)
	})
	sort.Slice(view.Models, func(i, j int) bool { return view.Models[i].Model < view.Models[j].Model })
	return view
}

func (service *OpenCodeGoAccountPoolService) UpdateIdentityLabel(channelID int, identityUID string, label string) error {
	if err := validateOpenCodeGoPoolChannel(channelID); err != nil {
		return err
	}
	if len([]rune(label)) > 128 {
		return errors.New("OpenCode Go identity label is too long")
	}
	result := model.DB.Model(&model.OpenCodeGoIdentity{}).
		Where("channel_id = ? AND uid = ?", channelID, identityUID).
		Updates(map[string]interface{}{
			"label":      strings.TrimSpace(label),
			"updated_at": common.GetTimestamp(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func (service *OpenCodeGoAccountPoolService) SetIdentityEnabled(channelID int, identityUID string, enabled bool) error {
	if err := validateOpenCodeGoPoolChannel(channelID); err != nil {
		return err
	}
	identity, err := model.GetOpenCodeGoIdentityPool(channelID, identityUID)
	if err != nil {
		return err
	}
	if identity == nil {
		return gorm.ErrRecordNotFound
	}
	status := model.OpenCodeGoIdentityStatusManualDisabled
	if enabled {
		status = model.OpenCodeGoIdentityStatusStale
		now := service.now().Unix()
		for _, workspace := range identity.Workspaces {
			if isOpenCodeGoWorkspaceEligibleForSnapshot(workspace, now) {
				status = model.OpenCodeGoIdentityStatusActive
				break
			}
		}
	}
	result := model.DB.Model(&model.OpenCodeGoIdentity{}).
		Where("id = ?", identity.ID).
		Updates(map[string]interface{}{
			"status":     status,
			"updated_at": common.GetTimestamp(),
		})
	if result.Error != nil {
		return result.Error
	}
	return service.rebuildChannel(channelID)
}

func (service *OpenCodeGoAccountPoolService) SetWorkspaceEnabled(channelID int, workspaceUID string, enabled bool) error {
	if err := validateOpenCodeGoPoolChannel(channelID); err != nil {
		return err
	}
	workspace, err := model.GetOpenCodeGoWorkspace(channelID, workspaceUID)
	if err != nil {
		return err
	}
	if workspace == nil {
		return gorm.ErrRecordNotFound
	}
	workspace.ManualEnabled = enabled
	state := model.OpenCodeGoStateManualDisabled
	reason := "workspace is manually disabled"
	if enabled {
		state, reason = effectiveOpenCodeGoWorkspaceState(
			*workspace,
			workspace.QuotaWindows,
			nil,
			false,
			workspace,
		)
	}
	result := model.DB.Model(&model.OpenCodeGoWorkspace{}).
		Where("id = ?", workspace.ID).
		Updates(map[string]interface{}{
			"manual_enabled":  enabled,
			"effective_state": state,
			"state_reason":    reason,
			"updated_at":      common.GetTimestamp(),
		})
	if result.Error != nil {
		return result.Error
	}
	return service.rebuildChannel(channelID)
}

func (service *OpenCodeGoAccountPoolService) RefreshWorkspace(
	ctx context.Context,
	channelID int,
	workspaceUID string,
) (*model.OpenCodeGoWorkspace, error) {
	workspace, err := model.GetOpenCodeGoWorkspace(channelID, workspaceUID)
	if err != nil {
		return nil, err
	}
	if workspace == nil {
		return nil, gorm.ErrRecordNotFound
	}
	identity, err := model.GetOpenCodeGoIdentityByID(channelID, workspace.IdentityID)
	if err != nil {
		return nil, err
	}
	if identity == nil {
		return nil, gorm.ErrRecordNotFound
	}
	if _, err := service.RefreshIdentity(ctx, channelID, identity.UID); err != nil {
		return nil, err
	}
	refreshed, err := model.GetOpenCodeGoWorkspace(channelID, workspaceUID)
	if err != nil {
		return nil, err
	}
	if refreshed == nil {
		return nil, gorm.ErrRecordNotFound
	}
	return refreshed, nil
}

func (service *OpenCodeGoAccountPoolService) DeleteIdentity(channelID int, identityUID string) error {
	if err := validateOpenCodeGoPoolChannel(channelID); err != nil {
		return err
	}
	if err := model.DeleteOpenCodeGoIdentity(channelID, identityUID); err != nil {
		return err
	}
	return service.rebuildChannel(channelID)
}

func (service *OpenCodeGoAccountPoolService) DeleteWorkspace(channelID int, workspaceUID string) error {
	if err := validateOpenCodeGoPoolChannel(channelID); err != nil {
		return err
	}
	if err := model.DeleteOpenCodeGoWorkspace(channelID, workspaceUID); err != nil {
		return err
	}
	return service.rebuildChannel(channelID)
}

func (service *OpenCodeGoAccountPoolService) DeleteNonMemberWorkspaces(channelID int) (int64, error) {
	if err := validateOpenCodeGoPoolChannel(channelID); err != nil {
		return 0, err
	}
	count, err := model.DeleteOpenCodeGoNonMemberWorkspaces(channelID)
	if err != nil {
		return 0, err
	}
	return count, service.rebuildChannel(channelID)
}

func (service *OpenCodeGoAccountPoolService) rebuildChannel(channelID int) error {
	return ReconcileOpenCodeGoPoolChannel(channelID)
}

func ReconcileOpenCodeGoPoolChannel(channelID int) error {
	if err := validateOpenCodeGoPoolChannel(channelID); err != nil {
		return err
	}
	if err := syncOpenCodeGoChannelModels(channelID); err != nil {
		return err
	}
	return RebuildOpenCodeGoPoolChannel(channelID)
}

func openCodeGoPoolEligibleCount(channelID int) int {
	value, exists := openCodeGoPoolChannels.Load(channelID)
	if !exists {
		return 0
	}
	snapshot := value.(*openCodeGoPoolChannelState).snapshot.Load()
	if snapshot == nil {
		return 0
	}
	return snapshot.candidateCount
}

func openCodeGoQuotaKindOrder(kind string) int {
	for index, candidate := range model.OpenCodeGoQuotaKinds {
		if kind == candidate {
			return index
		}
	}
	return len(model.OpenCodeGoQuotaKinds)
}

func roundOpenCodeGoAmount(value float64) float64 {
	return math.Round(value*100) / 100
}

func sanitizeOpenCodeGoStoredMessage(message string) string {
	if strings.TrimSpace(message) == "" {
		return ""
	}
	return sanitizeOpenCodeGoError(errors.New(message))
}
