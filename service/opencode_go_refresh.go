package service

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/QuantumNous/new-api/model"
)

const (
	OpenCodeGoDefaultRefreshConcurrency = 4
	OpenCodeGoMaxRefreshConcurrency     = 16
)

type OpenCodeGoRefreshResult struct {
	ChannelID   int    `json:"channel_id"`
	IdentityUID string `json:"identity_uid"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
}

type OpenCodeGoRefreshSummary struct {
	Total         int                             `json:"total"`
	Processed     int                             `json:"processed"`
	Succeeded     int                             `json:"succeeded"`
	Failed        int                             `json:"failed"`
	Results       []OpenCodeGoRefreshResult       `json:"results"`
	ModelRecovery OpenCodeGoModelRecoverySummary  `json:"model_recovery"`
	Lifecycle     OpenCodeGoLifecycleBatchSummary `json:"lifecycle"`
}

type openCodeGoIndexedRefreshTarget struct {
	index  int
	target model.OpenCodeGoRefreshTarget
}

type openCodeGoIndexedRefreshResult struct {
	index  int
	result OpenCodeGoRefreshResult
}

func (service *OpenCodeGoAccountPoolService) RefreshAllIdentities(
	ctx context.Context,
	channelID int,
	concurrency int,
	reportProgress func(processed, total int),
) (OpenCodeGoRefreshSummary, error) {
	if err := validateOpenCodeGoPoolChannel(channelID); err != nil {
		return OpenCodeGoRefreshSummary{}, err
	}
	identityUIDs, err := model.ListOpenCodeGoIdentityUIDs(channelID)
	if err != nil {
		return OpenCodeGoRefreshSummary{}, err
	}
	targets := make([]model.OpenCodeGoRefreshTarget, 0, len(identityUIDs))
	for _, identityUID := range identityUIDs {
		targets = append(targets, model.OpenCodeGoRefreshTarget{
			ChannelID:   channelID,
			IdentityUID: identityUID,
		})
	}
	return service.RefreshIdentityTargets(ctx, targets, concurrency, reportProgress)
}

func (service *OpenCodeGoAccountPoolService) RefreshIdentityTargets(
	ctx context.Context,
	targets []model.OpenCodeGoRefreshTarget,
	concurrency int,
	reportProgress func(processed, total int),
) (OpenCodeGoRefreshSummary, error) {
	if service == nil || service.console == nil || service.codec == nil {
		return OpenCodeGoRefreshSummary{}, errors.New("OpenCode Go refresh service is not configured")
	}
	targets = uniqueOpenCodeGoRefreshTargets(targets)
	summary := OpenCodeGoRefreshSummary{
		Total:   len(targets),
		Results: make([]OpenCodeGoRefreshResult, 0, len(targets)),
	}
	if reportProgress != nil {
		reportProgress(0, summary.Total)
	}
	if len(targets) == 0 {
		return summary, nil
	}
	concurrency = normalizeOpenCodeGoRefreshConcurrency(concurrency, len(targets))

	jobs := make(chan openCodeGoIndexedRefreshTarget)
	outcomes := make(chan openCodeGoIndexedRefreshResult)
	var workers sync.WaitGroup
	workers.Add(concurrency)
	for workerIndex := 0; workerIndex < concurrency; workerIndex++ {
		go func() {
			defer workers.Done()
			for job := range jobs {
				result := OpenCodeGoRefreshResult{
					ChannelID:   job.target.ChannelID,
					IdentityUID: job.target.IdentityUID,
					Status:      "refreshed",
				}
				if _, err := service.RefreshIdentity(ctx, job.target.ChannelID, job.target.IdentityUID); err != nil {
					result.Status = "error"
					result.Error = sanitizeOpenCodeGoError(err)
				}
				outcomes <- openCodeGoIndexedRefreshResult{index: job.index, result: result}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for index, target := range targets {
			select {
			case <-ctx.Done():
				return
			case jobs <- openCodeGoIndexedRefreshTarget{index: index, target: target}:
			}
		}
	}()
	go func() {
		workers.Wait()
		close(outcomes)
	}()

	indexedResults := make([]*OpenCodeGoRefreshResult, len(targets))
	for outcome := range outcomes {
		result := outcome.result
		indexedResults[outcome.index] = &result
		summary.Processed++
		if result.Status == "refreshed" {
			summary.Succeeded++
		} else {
			summary.Failed++
		}
		if reportProgress != nil {
			reportProgress(summary.Processed, summary.Total)
		}
	}
	for _, result := range indexedResults {
		if result != nil {
			summary.Results = append(summary.Results, *result)
		}
	}
	if err := ctx.Err(); err != nil {
		return summary, fmt.Errorf("OpenCode Go refresh cancelled: %w", err)
	}
	return summary, nil
}

func uniqueOpenCodeGoRefreshTargets(targets []model.OpenCodeGoRefreshTarget) []model.OpenCodeGoRefreshTarget {
	result := make([]model.OpenCodeGoRefreshTarget, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if target.ChannelID <= 0 || target.IdentityUID == "" {
			continue
		}
		key := fmt.Sprintf("%d:%s", target.ChannelID, target.IdentityUID)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, target)
	}
	return result
}

func normalizeOpenCodeGoRefreshConcurrency(concurrency int, targetCount int) int {
	if concurrency <= 0 {
		concurrency = OpenCodeGoDefaultRefreshConcurrency
	}
	if concurrency > OpenCodeGoMaxRefreshConcurrency {
		concurrency = OpenCodeGoMaxRefreshConcurrency
	}
	if concurrency > targetCount {
		concurrency = targetCount
	}
	if concurrency < 1 {
		concurrency = 1
	}
	return concurrency
}
