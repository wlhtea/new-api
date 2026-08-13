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
		{name: "legacy generic block is credential failure", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "AuthError", Message: "Request blocked by upstream provider."}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationCredentialFailure},
		{name: "production fraud block", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "AuthError", Message: "This account has found to be committing fraud or is in breach of terms of services and has been blocked."}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationRiskBlocked},
		{name: "production fraud block normalized", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: " AUTHERROR ", Message: "  THIS account has found to be committing fraud\n or is in breach of terms of services and has been blocked!  "}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationRiskBlocked},
		{name: "production fraud block terminal punctuation", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "AuthError", Message: "This account has found to be committing fraud or is in breach of terms of services and has been blocked?!"}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationRiskBlocked},
		{name: "fraud wording comma is not normalized", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "AuthError", Message: "This account has found to be committing fraud or is in breach of terms of services and has been blocked,"}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationCredentialFailure},
		{name: "generic blocked is credential failure", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "AuthError", Message: "This account has been blocked."}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationCredentialFailure},
		{name: "fraud wording suffix is not authoritative", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "AuthError", Message: "This account has found to be committing fraud or is in breach of terms of services and has been blocked temporarily."}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationCredentialFailure},
		{name: "fraud wording requires 401", failure: OpenCodeGoProviderFailure{StatusCode: 403, ErrorType: "AuthError", Message: "This account has found to be committing fraud or is in breach of terms of services and has been blocked."}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationCredentialFailure},
		{name: "fraud wording requires AuthError", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "Error", Message: "This account has found to be committing fraud or is in breach of terms of services and has been blocked."}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationCredentialFailure},
		{name: "invalid key is not risk", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "AuthError", Message: "Invalid API key"}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationCredentialFailure},
		{name: "auth model denial does not cool model", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "AuthError", Message: "Model alpha-test is not supported"}, wantOK: false},
		{name: "credits", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "CreditsError"}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationQuotaExhausted},
		{name: "monthly", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "MonthlyLimitError"}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationQuotaExhausted, wantQuota: model.OpenCodeGoQuotaMonthly},
		{name: "user monthly", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "UserLimitError"}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationQuotaExhausted, wantQuota: model.OpenCodeGoQuotaMonthly},
		{name: "go rolling does not change workspace availability", failure: OpenCodeGoProviderFailure{StatusCode: 429, ErrorType: "GoUsageLimitError", LimitName: "5 hour", RetryAfter: "90"}, wantOK: false},
		{name: "go weekly does not change workspace availability", failure: OpenCodeGoProviderFailure{StatusCode: 429, ErrorType: "GoUsageLimitError", LimitName: "weekly"}, wantOK: false},
		{name: "go monthly does not change workspace availability", failure: OpenCodeGoProviderFailure{StatusCode: 429, ErrorType: "GoUsageLimitError", LimitName: "monthly"}, wantOK: false},
		{name: "black quota does not change workspace availability", failure: OpenCodeGoProviderFailure{StatusCode: 429, ErrorType: "BlackUsageLimitError", RetryAfter: "120"}, wantOK: false},
		{name: "region error does not cool model", failure: OpenCodeGoProviderFailure{StatusCode: 403, ErrorType: "RegionError"}, wantOK: false},
		{name: "model error does not cool model", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "ModelError"}, wantOK: false},
		{name: "401 rate limit does not cool model", failure: OpenCodeGoProviderFailure{StatusCode: 401, ErrorType: "RateLimitError", RetryAfter: "17"}, wantOK: false},
		{name: "rpm does not cool model", failure: OpenCodeGoProviderFailure{StatusCode: 429, ErrorType: "RateLimitError", RetryAfter: "17"}, wantOK: false},
		{name: "free rpm does not cool model", failure: OpenCodeGoProviderFailure{StatusCode: 429, ErrorType: "FreeUsageLimitError"}, wantOK: false},
		{name: "unknown 401", failure: OpenCodeGoProviderFailure{StatusCode: 401}, wantOK: true, wantScope: OpenCodeGoHealthScopeWorkspace, wantKind: OpenCodeGoObservationCredentialFailure},
		{name: "unknown 403 is not enough health evidence", failure: OpenCodeGoProviderFailure{StatusCode: 403}, wantOK: false},
		{name: "unknown 429 does not cool model", failure: OpenCodeGoProviderFailure{StatusCode: 429}, wantOK: false},
		{name: "request timeout does not cool model", failure: OpenCodeGoProviderFailure{StatusCode: 408}, wantOK: false},
		{name: "too early does not cool model", failure: OpenCodeGoProviderFailure{StatusCode: 425}, wantOK: false},
		{name: "upstream 500", failure: OpenCodeGoProviderFailure{StatusCode: 500}, wantOK: false},
		{name: "caller abort", failure: OpenCodeGoProviderFailure{StatusCode: 499, ErrorType: "error"}},
		{name: "client request error", failure: OpenCodeGoProviderFailure{StatusCode: 400, ErrorType: "invalid_request_error"}},
		{name: "400 auth error does not change workspace availability", failure: OpenCodeGoProviderFailure{StatusCode: 400, ErrorType: "AuthError", Message: "invalid request"}, wantOK: false},
		{name: "400 credits error does not change workspace availability", failure: OpenCodeGoProviderFailure{StatusCode: 400, ErrorType: "CreditsError"}, wantOK: false},
		{name: "400 region error does not cool model", failure: OpenCodeGoProviderFailure{StatusCode: 400, ErrorType: "RegionError"}, wantOK: false},
		{name: "400 model error does not cool model", failure: OpenCodeGoProviderFailure{StatusCode: 400, ErrorType: "ModelError"}, wantOK: false},
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

func TestObserveOpenCodeGoProviderFailureDoesNotPersistProvider5xx(t *testing.T) {
	applied, err := ObserveOpenCodeGoProviderFailure(
		999,
		"workspace-does-not-exist",
		"glm-5.2",
		OpenCodeGoProviderFailure{StatusCode: http.StatusInternalServerError},
		time.Unix(1_900_000_000, 0),
	)

	require.NoError(t, err)
	assert.False(t, applied)
}

func TestParseAndClassifyOpenCodeGoProductionFraudBlock(t *testing.T) {
	failure := ParseOpenCodeGoProviderFailure(
		http.StatusUnauthorized,
		nil,
		[]byte(`{"type":"AuthError","message":"This account has found to be committing fraud or is in breach of terms of services and has been blocked."}`),
	)

	classified, ok := ClassifyOpenCodeGoProviderFailure(failure, time.Unix(1_900_000_000, 0))
	require.True(t, ok)
	assert.Equal(t, OpenCodeGoHealthScopeWorkspace, classified.Scope)
	assert.Equal(t, OpenCodeGoObservationRiskBlocked, classified.Observation.Kind)
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

func TestParseOpenCodeGoProviderFailureRejectsPrivateRetryAfterText(t *testing.T) {
	httpDate := time.Date(2030, time.January, 2, 3, 5, 20, 0, time.UTC).Format(http.TimeFormat)
	for _, test := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "delta seconds", value: "321", want: "321"},
		{name: "http date", value: httpDate, want: httpDate},
		{name: "private text", value: "workspace=wrk_private", want: ""},
		{name: "signed seconds", value: "+30", want: ""},
		{name: "newline", value: "30\r\nworkspace=wrk_private", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := ParseOpenCodeGoProviderFailure(
				http.StatusTooManyRequests,
				http.Header{"Retry-After": []string{test.value}},
				nil,
			)
			assert.Equal(t, test.want, failure.RetryAfter)
		})
	}
}

func TestParseOpenCodeGoProviderFailurePreservesSafeValidationPath(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "top level zod issues",
			body: `[{"code":"too_big","path":["tools",0,"input_schema"],"message":"Array must contain at most 20 element(s)"}]`,
		},
		{
			name: "nested fastapi detail",
			body: `{"detail":[{"code":"invalid_type","path":["body","thinking"],"message":"Expected string"}]}`,
		},
		{
			name: "server sent event validation error",
			body: "event: error\ndata: [{\"code\":\"invalid_value\",\"path\":[\"messages\",1,\"role\"],\"message\":\"Unsupported role\"}]\n\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure := ParseOpenCodeGoProviderFailure(http.StatusUnprocessableEntity, nil, []byte(test.body))

			assert.Equal(t, "validation_error", failure.ErrorType)
			assert.NotEqual(t, "validation_error", failure.ErrorCode)
			assert.Contains(t, failure.Message, "OpenCode Go rejected")
			assert.NotContains(t, failure.Message, "[")
		})
	}
}

func TestClassifyOpenCodeGoTransportFailureDoesNotChangeHealth(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	classified := ClassifyOpenCodeGoTransportFailure("connection reset", now)
	assert.Empty(t, classified.Scope)
	assert.Empty(t, classified.Observation.Kind)
	assert.Equal(t, now, classified.Observation.ObservedAt)
}
