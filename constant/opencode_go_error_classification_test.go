package constant

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOpenCodeGoSafeUpstreamClientRequestError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		errorType  string
		errorCode  string
		want       bool
	}{
		{
			name:       "raw 400 invalid request",
			statusCode: http.StatusBadRequest,
			errorType:  "invalid_request_error",
			errorCode:  "invalid_prompt",
			want:       true,
		},
		{
			name:       "raw 422 validation error",
			statusCode: http.StatusUnprocessableEntity,
			errorType:  "validation_error",
			errorCode:  "invalid_value",
			want:       true,
		},
		{
			name:       "http 200 envelope fails closed",
			statusCode: http.StatusOK,
			errorType:  "invalid_request_error",
			errorCode:  "invalid_prompt",
			want:       false,
		},
		{
			name:       "raw 401 fails closed",
			statusCode: http.StatusUnauthorized,
			errorType:  "invalid_request_error",
			errorCode:  "invalid_prompt",
			want:       false,
		},
		{
			name:       "raw 429 fails closed",
			statusCode: http.StatusTooManyRequests,
			errorType:  "validation_error",
			errorCode:  "invalid_value",
			want:       false,
		},
		{
			name:       "invalid api key vetoes invalid request",
			statusCode: http.StatusBadRequest,
			errorType:  "invalid_request_error",
			errorCode:  "invalid_api_key",
			want:       false,
		},
		{
			name:       "rate limit vetoes validation",
			statusCode: http.StatusBadRequest,
			errorType:  "validation_error",
			errorCode:  "rate_limit_exceeded",
			want:       false,
		},
		{
			name:       "quota vetoes invalid request",
			statusCode: http.StatusBadRequest,
			errorType:  "invalid_request_error",
			errorCode:  "quota_exhausted",
			want:       false,
		},
		{
			name:       "policy vetoes invalid request",
			statusCode: http.StatusBadRequest,
			errorType:  "invalid_request_error",
			errorCode:  "policy_violation",
			want:       false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, IsOpenCodeGoSafeUpstreamClientRequestError(
				test.statusCode,
				test.errorType,
				test.errorCode,
				"request rejected",
			))
		})
	}
}

func TestClassifyOpenCodeGoPublicErrorPrioritizesOperatorClassifications(t *testing.T) {
	for _, test := range []struct {
		name      string
		errorType string
		errorCode string
	}{
		{name: "authentication", errorType: "invalid_request_error", errorCode: "invalid_api_key"},
		{name: "rate limit", errorType: "validation_error", errorCode: "rate_limit_exceeded"},
		{name: "quota", errorType: "invalid_request_error", errorCode: "quota_exhausted"},
		{name: "policy", errorType: "invalid_request_error", errorCode: "policy_violation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			projection := ClassifyOpenCodeGoPublicError(http.StatusBadRequest, test.errorType, test.errorCode, "request rejected")

			assert.Equal(t, http.StatusTooManyRequests, projection.StatusCode)
			assert.Equal(t, OpenCodeGoPublicRateLimitErrorCode, projection.Code)
		})
	}
}
