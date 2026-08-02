package service

import (
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyOpenCodeGoProviderFailureMatrix(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	tests := []struct {
		name      string
		failure   OpenCodeGoProviderFailure
		wantOK    bool
		wantScope OpenCodeGoHealthScope
		wantKind  OpenCodeGoHealthObservationKind
		wantQuota string
		wantDelay time.Duration
	}{
		{name: "risk control", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "AuthError", Message: "Request blocked by upstream provider."}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationRiskBlocked},
		{name: "invalid key is not risk", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "AuthError", Message: "Invalid API key"}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationCredentialFailure},
		{name: "auth model denial", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "AuthError", Message: "Model alpha-test is not supported"}, wantOK: true, wantScope: OpenCodeGoHealthScopeModel, wantKind: OpenCodeGoObservationModelBlocked, wantDelay: openCodeGoDefaultModelCooldown},
		{name: "credits", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "CreditsError"}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationQuotaExhausted},
		{name: "monthly", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "MonthlyLimitError"}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationQuotaExhausted, wantQuota: model.OpenCodeGoQuotaMonthly},
		{name: "user monthly", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "UserLimitError"}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationQuotaExhausted, wantQuota: model.OpenCodeGoQuotaMonthly},
		{name: "go rolling", failure: OpenCodeGoProviderFailure{StatusCode: 429, ErrorType: "GoUsageLimitError", LimitName: "5 hour", RetryAfter: "90"}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationQuotaExhausted, wantQuota: model.OpenCodeGoQuotaRolling, wantDelay: 90 * time.Second},
		{name: "go weekly", failure: OpenCodeGoProviderFailure{StatusCode: 429, ErrorType: "GoUsageLimitError", LimitName: "weekly"}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationQuotaExhausted, wantQuota: model.OpenCodeGoQuotaWeekly},
		{name: "go monthly", failure: OpenCodeGoProviderFailure{StatusCode: 429, ErrorType: "GoUsageLimitError", LimitName: "monthly"}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationQuotaExhausted, wantQuota: model.OpenCodeGoQuotaMonthly},
		{name: "black quota", failure: OpenCodeGoProviderFailure{StatusCode: 429, ErrorType: "BlackUsageLimitError", RetryAfter: "120"}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationQuotaExhausted, wantDelay: 2 * time.Minute},
		{name: "region", failure: OpenCodeGoProviderFailure{StatusCode: 403, ErrorType: "RegionError"}, wantOK: true, wantScope: OpenCodeGoHealthScopeModel, wantKind: OpenCodeGoObservationRegionBlocked, wantDelay: openCodeGoDefaultRegionCooldown},
		{name: "model", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "ModelError"}, wantOK: true, wantScope: OpenCodeGoHealthScopeModel, wantKind: OpenCodeGoObservationModelBlocked, wantDelay: openCodeGoDefaultModelCooldown},
		{name: "rpm", failure: OpenCodeGoProviderFailure{StatusCode: 429, ErrorType: "RateLimitError", RetryAfter: "17"}, wantOK: true, wantScope: OpenCodeGoHealthScopeModel, wantKind: OpenCodeGoObservationRPMThrottled, wantDelay: 17 * time.Second},
		{name: "free rpm", failure: OpenCodeGoProviderFailure{StatusCode: 429, ErrorType: "FreeUsageLimitError"}, wantOK: true, wantScope: OpenCodeGoHealthScopeModel, wantKind: OpenCodeGoObservationRPMThrottled, wantDelay: openCodeGoDefaultRPMCooldown},
		{name: "unknown 401", failure: OpenCodeGoProviderFailure{StatusCode: 401}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationCredentialFailure},
		{name: "unknown 403", failure: OpenCodeGoProviderFailure{StatusCode: 403}, wantOK: true, wantScope: OpenCodeGoHealthScopeModel, wantKind: OpenCodeGoObservationRegionBlocked, wantDelay: openCodeGoDefaultRegionCooldown},
		{name: "unknown 429", failure: OpenCodeGoProviderFailure{StatusCode: 429}, wantOK: true, wantScope: OpenCodeGoHealthScopeModel, wantKind: OpenCodeGoObservationRPMThrottled, wantDelay: openCodeGoDefaultRPMCooldown},
		{name: "upstream 500", failure: OpenCodeGoProviderFailure{StatusCode: 500}, wantOK: true, wantScope: OpenCodeGoHealthScopeModel, wantKind: OpenCodeGoObservationTransientFailure, wantDelay: openCodeGoDefaultTransientCooldown},
		{name: "caller abort", failure: OpenCodeGoProviderFailure{StatusCode: 499, ErrorType: "error"}},
		{name: "client request error", failure: OpenCodeGoProviderFailure{StatusCode: 400, ErrorType: "invalid_request_error"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			classified, ok := ClassifyOpenCodeGoProviderFailure(test.failure, now)
			require.Equal(t, test.wantOK, ok)
			if !ok {
				return
			}
			assert.Equal(t, test.wantScope, classified.Scope)
			assert.Equal(t, test.wantKind, classified.Observation.Kind)
			assert.Equal(t, test.wantQuota, classified.Observation.QuotaKind)
			if test.wantDelay == 0 {
				assert.True(t, classified.Observation.Deadline.IsZero())
			} else {
				assert.Equal(t, now.Add(test.wantDelay), classified.Observation.Deadline)
			}
		})
	}
}

func TestParseOpenCodeGoRetryAfterSupportsHTTPDateAndBoundsValues(t *testing.T) {
	now := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	deadline := now.Add(75 * time.Second).Format(http.TimeFormat)
	duration, ok := parseOpenCodeGoRetryAfter(deadline, now)
	require.True(t, ok)
	assert.Equal(t, 75*time.Second, duration)

	duration, ok = parseOpenCodeGoRetryAfter("999999999999", now)
	require.True(t, ok)
	assert.Equal(t, openCodeGoMaxRetryAfter, duration)

	_, ok = parseOpenCodeGoRetryAfter("-1", now)
	assert.False(t, ok)
}

func TestClassifyOpenCodeGoTransportFailureIsModelScoped(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	classified := ClassifyOpenCodeGoTransportFailure("connection reset", now)
	assert.Equal(t, OpenCodeGoHealthScopeModel, classified.Scope)
	assert.Equal(t, OpenCodeGoObservationTransientFailure, classified.Observation.Kind)
	assert.Equal(t, now.Add(openCodeGoDefaultTransientCooldown), classified.Observation.Deadline)
}
