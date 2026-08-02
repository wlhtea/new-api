package service

import (
	"errors"
	"sort"
	"time"

	"github.com/QuantumNous/new-api/model"
)

type OpenCodeGoModelRecoverySummary struct {
	Total     int `json:"total"`
	Recovered int `json:"recovered"`
	Failed    int `json:"failed"`
}

func RecoverOpenCodeGoModelCooldowns(
	channelID int,
	now time.Time,
	limit int,
) (OpenCodeGoModelRecoverySummary, error) {
	return recoverOpenCodeGoModelCooldowns(channelID, now, limit, ReconcileOpenCodeGoPoolChannel)
}

func recoverOpenCodeGoModelCooldowns(
	channelID int,
	now time.Time,
	limit int,
	rebuild func(int) error,
) (OpenCodeGoModelRecoverySummary, error) {
	if now.IsZero() {
		return OpenCodeGoModelRecoverySummary{}, errors.New("OpenCode Go model recovery time is required")
	}
	targets, err := model.ListOpenCodeGoDueModelRecoveryTargets(channelID, now.Unix(), limit)
	if err != nil {
		return OpenCodeGoModelRecoverySummary{}, err
	}
	summary := OpenCodeGoModelRecoverySummary{Total: len(targets)}
	channels := make(map[int]struct{})
	var resultErr error
	for _, target := range targets {
		applied, applyErr := applyOpenCodeGoClassifiedFailure(
			target.ChannelID,
			target.WorkspaceUID,
			target.Model,
			OpenCodeGoClassifiedFailure{
				Scope: OpenCodeGoHealthScopeModel,
				Observation: OpenCodeGoHealthObservation{
					Kind:       OpenCodeGoObservationCooldownExpired,
					ObservedAt: now,
				},
			},
			nil,
		)
		if applyErr != nil {
			summary.Failed++
			resultErr = errors.Join(resultErr, applyErr)
			continue
		}
		if applied {
			summary.Recovered++
			channels[target.ChannelID] = struct{}{}
		}
	}

	channelIDs := make([]int, 0, len(channels))
	for affectedChannelID := range channels {
		channelIDs = append(channelIDs, affectedChannelID)
	}
	sort.Ints(channelIDs)
	for _, affectedChannelID := range channelIDs {
		if rebuild == nil {
			continue
		}
		if rebuildErr := rebuild(affectedChannelID); rebuildErr != nil {
			resultErr = errors.Join(resultErr, rebuildErr)
		}
	}
	return summary, resultErr
}
