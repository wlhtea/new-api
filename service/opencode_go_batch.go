package service

import (
	"context"

	"github.com/QuantumNous/new-api/model"
)

type OpenCodeGoBatchWorkspaceResult struct {
	WorkspaceUID string `json:"workspace_uid"`
	Status       string `json:"status"` // ok | skipped | error
	Message      string `json:"message,omitempty"`
}

type OpenCodeGoBatchSummary struct {
	Attempted int                              `json:"attempted"`
	Succeeded int                              `json:"succeeded"`
	Skipped   int                              `json:"skipped"`
	Failed    int                              `json:"failed"`
	Results   []OpenCodeGoBatchWorkspaceResult `json:"results"`
}

func (summary *OpenCodeGoBatchSummary) append(uid string, status string, message string) {
	summary.Attempted++
	switch status {
	case "ok":
		summary.Succeeded++
	case "skipped":
		summary.Skipped++
	default:
		summary.Failed++
	}
	summary.Results = append(summary.Results, OpenCodeGoBatchWorkspaceResult{
		WorkspaceUID: uid,
		Status:       status,
		Message:      message,
	})
}

// openCodeGoBatchTargetWorkspaces lists the channel workspaces to operate on.
// When workspaceUIDs is non-empty it is filtered to those UIDs.
func openCodeGoBatchTargetWorkspaces(channelID int, workspaceUIDs []string) ([]model.OpenCodeGoWorkspace, error) {
	identities, err := model.ListOpenCodeGoIdentities(channelID)
	if err != nil {
		return nil, err
	}
	want := make(map[string]struct{}, len(workspaceUIDs))
	for _, uid := range workspaceUIDs {
		if uid != "" {
			want[uid] = struct{}{}
		}
	}
	seen := make(map[string]struct{})
	var workspaces []model.OpenCodeGoWorkspace
	for _, identity := range identities {
		for _, workspace := range identity.Workspaces {
			if len(want) > 0 {
				if _, ok := want[workspace.UID]; !ok {
					continue
				}
			}
			if _, duplicate := seen[workspace.UID]; duplicate {
				continue
			}
			seen[workspace.UID] = struct{}{}
			workspaces = append(workspaces, workspace)
		}
	}
	return workspaces, nil
}

func openCodeGoBatchWorkspaceEligible(workspace *model.OpenCodeGoWorkspace) bool {
	return workspace != nil && workspace.ManualEnabled &&
		workspace.EffectiveState != model.OpenCodeGoStateRiskBlocked &&
		workspace.EffectiveState != model.OpenCodeGoStateAuthError
}

// BatchSetChinaModels enables or disables China-deployed models across the
// channel's eligible workspaces. An empty workspaceUIDs list targets every
// eligible workspace. Renewal and referral automations are not touched.
func (service *OpenCodeGoLifecycleService) BatchSetChinaModels(
	ctx context.Context,
	channelID int,
	workspaceUIDs []string,
	enabled bool,
	source string,
) (*OpenCodeGoBatchSummary, error) {
	scoped, err := service.scopedForChannel(channelID)
	if err != nil {
		return nil, err
	}
	service = scoped
	workspaces, err := openCodeGoBatchTargetWorkspaces(channelID, workspaceUIDs)
	if err != nil {
		return nil, err
	}
	summary := &OpenCodeGoBatchSummary{}
	for index := range workspaces {
		workspace := &workspaces[index]
		if !openCodeGoBatchWorkspaceEligible(workspace) {
			summary.append(workspace.UID, "skipped", "workspace is not eligible")
			continue
		}
		if enabled && workspace.ChinaModelsEnabled != nil && *workspace.ChinaModelsEnabled {
			summary.append(workspace.UID, "skipped", "China-deployed models already enabled")
			continue
		}
		if !enabled && (workspace.ChinaModelsEnabled == nil || !*workspace.ChinaModelsEnabled) {
			summary.append(workspace.UID, "skipped", "China-deployed models already disabled")
			continue
		}
		var operation *model.OpenCodeGoOperation
		var actionErr error
		if enabled {
			operation, actionErr = service.EnableChinaModels(ctx, channelID, workspace.UID, source)
		} else {
			operation, actionErr = service.DisableChinaModels(ctx, channelID, workspace.UID, source)
		}
		if actionErr != nil {
			summary.append(workspace.UID, "error", sanitizeOpenCodeGoError(actionErr))
			continue
		}
		status := operation.Status
		if status == "" {
			status = "ok"
		}
		summary.append(workspace.UID, "ok", status)
	}
	return summary, nil
}

// BatchCancelSubscriptionRenewal cancels subscription renewal across the
// channel's eligible active workspaces that have not been cancelled yet.
// Cancellation is one-way: there is no batch re-enable of renewal.
func (service *OpenCodeGoLifecycleService) BatchCancelSubscriptionRenewal(
	ctx context.Context,
	channelID int,
	workspaceUIDs []string,
	source string,
) (*OpenCodeGoBatchSummary, error) {
	scoped, err := service.scopedForChannel(channelID)
	if err != nil {
		return nil, err
	}
	service = scoped
	workspaces, err := openCodeGoBatchTargetWorkspaces(channelID, workspaceUIDs)
	if err != nil {
		return nil, err
	}
	summary := &OpenCodeGoBatchSummary{}
	for index := range workspaces {
		workspace := &workspaces[index]
		if !openCodeGoBatchWorkspaceEligible(workspace) {
			summary.append(workspace.UID, "skipped", "workspace is not eligible")
			continue
		}
		if workspace.MembershipStatus != model.OpenCodeGoMembershipActive ||
			workspace.SubscriptionReference == "" || workspace.RenewalCancelledAt > 0 {
			summary.append(workspace.UID, "skipped", "no active renewal to cancel")
			continue
		}
		_, _, actionErr := service.CancelSubscriptionRenewal(ctx, channelID, workspace.UID, source)
		if actionErr != nil {
			summary.append(workspace.UID, "error", sanitizeOpenCodeGoError(actionErr))
			continue
		}
		summary.append(workspace.UID, "ok", "subscription renewal cancelled")
	}
	return summary, nil
}
