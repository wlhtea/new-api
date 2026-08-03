package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
)

const openCodeGoNoEligibleWorkspaceReason = "opencode_go:no_eligible_workspace"

const openCodeGoAffinityDomain = "new-api/opencode-go/workspace-affinity/v1"

var ErrOpenCodeGoNoEligibleWorkspace = errors.New("OpenCode Go channel has no eligible workspace for the requested model")

var ErrOpenCodeGoSelectedCredentialUnavailable = errors.New("selected OpenCode Go workspace credential is unavailable")

type OpenCodeGoPoolSelection struct {
	WorkspaceID  int64
	WorkspaceUID string
	APIKey       string
}

type openCodeGoPoolCandidate struct {
	workspaceID      int64
	workspaceUID     string
	apiKeyCiphertext string
}

type openCodeGoPoolSnapshot struct {
	byModel        map[string][]openCodeGoPoolCandidate
	candidateCount int
	codec          *OpenCodeGoCredentialCodec
}

type openCodeGoPoolChannelState struct {
	snapshot atomic.Pointer[openCodeGoPoolSnapshot]
	cursors  sync.Map
}

var openCodeGoPoolChannels sync.Map

// PrepareOpenCodeGoPoolContainer removes derived routing state from a newly
// created channel. Credentials and model availability are populated only after
// an account-pool import succeeds.
func PrepareOpenCodeGoPoolContainer(channel *model.Channel) {
	if channel == nil || channel.Type != constant.ChannelTypeOpenCodeGo {
		return
	}
	channel.Key = ""
	channel.Models = ""
	if channel.Status == common.ChannelStatusManuallyDisabled {
		return
	}
	channel.Status = common.ChannelStatusAutoDisabled
	info := channel.GetOtherInfo()
	info["status_reason"] = openCodeGoNoEligibleWorkspaceReason
	info["status_time"] = common.GetTimestamp()
	channel.SetOtherInfo(info)
}

func InitOpenCodeGoPools() {
	channelIDs, err := model.ListOpenCodeGoChannelIDs()
	if err != nil {
		common.SysError("failed to list OpenCode Go account pools: " + err.Error())
		return
	}
	for _, channelID := range channelIDs {
		if err := ReconcileOpenCodeGoPoolChannel(channelID); err != nil {
			common.SysError(fmt.Sprintf("failed to rebuild OpenCode Go account pool: channel_id=%d error=%v", channelID, err))
		}
	}
}

func RebuildOpenCodeGoPoolChannel(channelID int) error {
	state := openCodeGoPoolStateForChannel(channelID)
	emptySnapshot := &openCodeGoPoolSnapshot{byModel: make(map[string][]openCodeGoPoolCandidate)}
	identities, err := model.ListOpenCodeGoIdentities(channelID)
	if err != nil {
		state.snapshot.Store(emptySnapshot)
		return err
	}
	if len(identities) == 0 {
		state.snapshot.Store(emptySnapshot)
		updateOpenCodeGoChannelAvailability(channelID, false)
		return nil
	}

	codec, err := NewConfiguredOpenCodeGoCredentialCodec()
	if err != nil {
		state.snapshot.Store(emptySnapshot)
		updateOpenCodeGoChannelAvailability(channelID, false)
		return err
	}

	now := time.Now().Unix()
	snapshot := &openCodeGoPoolSnapshot{byModel: make(map[string][]openCodeGoPoolCandidate)}
	for _, identity := range identities {
		if identity.Status != model.OpenCodeGoIdentityStatusActive {
			continue
		}
		for _, workspace := range identity.Workspaces {
			if !isOpenCodeGoWorkspaceEligibleForSnapshot(workspace, now) {
				continue
			}
			candidate := openCodeGoPoolCandidate{
				workspaceID:      workspace.ID,
				workspaceUID:     workspace.UID,
				apiKeyCiphertext: workspace.APIKeyCiphertext,
			}
			added := false
			seenModels := make(map[string]struct{})
			for _, workspaceModel := range workspace.Models {
				if !workspaceModel.Discovered || workspaceModel.State != model.OpenCodeGoModelAvailable || workspaceModel.DisabledUntil > now {
					continue
				}
				modelID := strings.TrimSpace(workspaceModel.Model)
				if modelID == "" {
					continue
				}
				if _, exists := seenModels[modelID]; exists {
					continue
				}
				seenModels[modelID] = struct{}{}
				snapshot.byModel[modelID] = append(snapshot.byModel[modelID], candidate)
				added = true
			}
			if added {
				snapshot.candidateCount++
			}
		}
	}

	snapshot.codec = codec
	state.snapshot.Store(snapshot)
	updateOpenCodeGoChannelAvailability(channelID, snapshot.candidateCount > 0)
	return nil
}

func SelectOpenCodeGoWorkspace(channelID int, upstreamModel string) (*OpenCodeGoPoolSelection, error) {
	return SelectOpenCodeGoWorkspaceWithAffinity(channelID, upstreamModel, "")
}

func SelectOpenCodeGoWorkspaceWithAffinity(channelID int, upstreamModel string, affinityKey string) (*OpenCodeGoPoolSelection, error) {
	value, exists := openCodeGoPoolChannels.Load(channelID)
	if !exists {
		return nil, ErrOpenCodeGoNoEligibleWorkspace
	}
	state := value.(*openCodeGoPoolChannelState)
	snapshot := state.snapshot.Load()
	if snapshot == nil {
		return nil, ErrOpenCodeGoNoEligibleWorkspace
	}
	candidates := snapshot.byModel[strings.TrimSpace(upstreamModel)]
	if len(candidates) == 0 {
		return nil, ErrOpenCodeGoNoEligibleWorkspace
	}
	index := 0
	if strings.TrimSpace(affinityKey) == "" {
		cursorValue, _ := state.cursors.LoadOrStore(strings.TrimSpace(upstreamModel), &atomic.Uint64{})
		cursor := cursorValue.(*atomic.Uint64)
		index = int((cursor.Add(1) - 1) % uint64(len(candidates)))
	} else {
		index = openCodeGoAffinityCandidateIndex(channelID, upstreamModel, affinityKey, candidates)
	}
	candidate := candidates[index]
	if snapshot.codec == nil {
		return nil, ErrOpenCodeGoSelectedCredentialUnavailable
	}
	apiKey, err := snapshot.codec.Decrypt(
		OpenCodeGoCredentialAPIKey,
		channelID,
		candidate.workspaceUID,
		candidate.apiKeyCiphertext,
	)
	if err != nil || strings.TrimSpace(apiKey) == "" {
		return nil, ErrOpenCodeGoSelectedCredentialUnavailable
	}
	return &OpenCodeGoPoolSelection{
		WorkspaceID:  candidate.workspaceID,
		WorkspaceUID: candidate.workspaceUID,
		APIKey:       apiKey,
	}, nil
}

func openCodeGoAffinityCandidateIndex(channelID int, upstreamModel string, affinityKey string, candidates []openCodeGoPoolCandidate) int {
	selectedIndex := 0
	var selectedScore uint64
	for index, candidate := range candidates {
		hash := hmac.New(sha256.New, []byte(common.CryptoSecret))
		_, _ = fmt.Fprintf(
			hash,
			"%s\x00%d\x00%s\x00%s\x00%s",
			openCodeGoAffinityDomain,
			channelID,
			strings.TrimSpace(upstreamModel),
			affinityKey,
			candidate.workspaceUID,
		)
		score := binary.BigEndian.Uint64(hash.Sum(nil)[:8])
		if index == 0 || score > selectedScore || (score == selectedScore && candidate.workspaceUID < candidates[selectedIndex].workspaceUID) {
			selectedIndex = index
			selectedScore = score
		}
	}
	return selectedIndex
}

func RemoveOpenCodeGoPoolChannel(channelID int) {
	openCodeGoPoolChannels.Delete(channelID)
}

func ReloadOpenCodeGoPools() {
	openCodeGoPoolChannels.Range(func(key, _ interface{}) bool {
		openCodeGoPoolChannels.Delete(key)
		return true
	})
	InitOpenCodeGoPools()
}

func openCodeGoPoolStateForChannel(channelID int) *openCodeGoPoolChannelState {
	value, _ := openCodeGoPoolChannels.LoadOrStore(channelID, &openCodeGoPoolChannelState{})
	return value.(*openCodeGoPoolChannelState)
}

func isOpenCodeGoWorkspaceEligibleForSnapshot(workspace model.OpenCodeGoWorkspace, now int64) bool {
	if !workspace.ManualEnabled ||
		workspace.MembershipStatus != model.OpenCodeGoMembershipActive ||
		(workspace.SubscriptionEndsAt > 0 && workspace.SubscriptionEndsAt <= now) ||
		workspace.CredentialStatus != model.OpenCodeGoCredentialValid ||
		workspace.EffectiveState != model.OpenCodeGoStateEligible ||
		workspace.QuotaSnapshotStatus != model.OpenCodeGoQuotaSnapshotComplete ||
		workspace.QuotaFetchedAt <= 0 ||
		workspace.CooldownUntil > 0 ||
		workspace.APIKeyCiphertext == "" {
		return false
	}

	seenKinds := make(map[string]struct{}, len(model.OpenCodeGoQuotaKinds))
	for _, window := range workspace.QuotaWindows {
		if window.FetchedAt != workspace.QuotaFetchedAt || window.UsedPercent >= 100 || window.UsedPercent < 0 {
			return false
		}
		seenKinds[window.Kind] = struct{}{}
	}
	if len(seenKinds) != len(model.OpenCodeGoQuotaKinds) {
		return false
	}
	for _, kind := range model.OpenCodeGoQuotaKinds {
		if _, exists := seenKinds[kind]; !exists {
			return false
		}
	}
	return true
}

func updateOpenCodeGoChannelAvailability(channelID int, eligible bool) {
	channel, err := model.GetChannelById(channelID, true)
	if err != nil || channel.Type != constant.ChannelTypeOpenCodeGo {
		return
	}
	statusReason, _ := channel.GetOtherInfo()["status_reason"].(string)
	if !eligible {
		if channel.Status == common.ChannelStatusEnabled {
			model.UpdateChannelStatus(channelID, "", common.ChannelStatusAutoDisabled, openCodeGoNoEligibleWorkspaceReason)
		}
		return
	}
	if channel.Status == common.ChannelStatusAutoDisabled && strings.HasPrefix(statusReason, openCodeGoNoEligibleWorkspaceReason) {
		model.UpdateChannelStatus(channelID, "", common.ChannelStatusEnabled, "opencode_go:eligible_workspace_available")
	}
}
