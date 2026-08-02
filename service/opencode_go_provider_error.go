package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

type OpenCodeGoHealthScope string

const (
	OpenCodeGoHealthScopeWorkspace OpenCodeGoHealthScope = "workspace"
	OpenCodeGoHealthScopeModel     OpenCodeGoHealthScope = "model"

	openCodeGoDefaultRPMCooldown       = time.Minute
	openCodeGoDefaultTransientCooldown = 30 * time.Second
	openCodeGoDefaultRegionCooldown    = 30 * time.Minute
	openCodeGoDefaultModelCooldown     = 15 * time.Minute
	openCodeGoMaxRetryAfter            = 31 * 24 * time.Hour
)

type OpenCodeGoProviderFailure struct {
	StatusCode int
	ErrorType  string
	ErrorCode  string
	Message    string
	RetryAfter string
	LimitName  string
}

type OpenCodeGoClassifiedFailure struct {
	Scope       OpenCodeGoHealthScope
	Observation OpenCodeGoHealthObservation
}

type openCodeGoProviderErrorEnvelope struct {
	Type     string          `json:"type"`
	Message  string          `json:"message"`
	Code     any             `json:"code"`
	Error    json.RawMessage `json:"error"`
	Metadata struct {
		LimitName string `json:"limitName"`
	} `json:"metadata"`
}

type openCodeGoProviderErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    any    `json:"code"`
}

func ParseOpenCodeGoProviderFailure(statusCode int, header http.Header, body []byte) OpenCodeGoProviderFailure {
	failure := OpenCodeGoProviderFailure{StatusCode: statusCode}
	var envelope openCodeGoProviderErrorEnvelope
	if len(body) > 0 && common.Unmarshal(body, &envelope) == nil {
		failure.ErrorType = envelope.Type
		failure.ErrorCode = stringifyOpenCodeGoProviderErrorCode(envelope.Code)
		failure.Message = envelope.Message
		failure.LimitName = sanitizeOpenCodeGoLimitName(envelope.Metadata.LimitName)
		if len(envelope.Error) > 0 {
			var detail openCodeGoProviderErrorDetail
			if common.Unmarshal(envelope.Error, &detail) == nil {
				if detail.Type != "" {
					failure.ErrorType = detail.Type
				}
				if code := stringifyOpenCodeGoProviderErrorCode(detail.Code); code != "" {
					failure.ErrorCode = code
				}
				if detail.Message != "" {
					failure.Message = detail.Message
				}
			} else {
				var stringError string
				if common.Unmarshal(envelope.Error, &stringError) == nil && stringError != "" {
					failure.Message = stringError
				}
			}
		}
	}

	failure.ErrorType = sanitizeOpenCodeGoErrorIdentifier(failure.ErrorType)
	failure.ErrorCode = sanitizeOpenCodeGoErrorIdentifier(failure.ErrorCode)
	failure.Message = SanitizeOpenCodeGoProviderMessage(failure.Message)
	if failure.ErrorType == "" {
		failure.ErrorType = "upstream_error"
	}
	if failure.ErrorCode == "" {
		failure.ErrorCode = failure.ErrorType
	}
	if failure.Message == "" {
		failure.Message = fmt.Sprintf("OpenCode Go returned status %d", statusCode)
	}
	if header != nil {
		failure.RetryAfter = sanitizeOpenCodeGoRetryAfter(header.Get("Retry-After"))
	}
	return failure
}

func SanitizeOpenCodeGoProviderMessage(message string) string {
	if strings.TrimSpace(message) == "" {
		return ""
	}
	return sanitizeOpenCodeGoError(errors.New(message))
}

func stringifyOpenCodeGoProviderErrorCode(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		return fmt.Sprintf("%v", typed)
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func sanitizeOpenCodeGoRetryAfter(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	return value
}

func sanitizeOpenCodeGoLimitName(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "5 hour":
		return "5 hour"
	case "weekly":
		return "weekly"
	case "monthly":
		return "monthly"
	default:
		return ""
	}
}

func ClassifyOpenCodeGoProviderFailure(
	failure OpenCodeGoProviderFailure,
	observedAt time.Time,
) (OpenCodeGoClassifiedFailure, bool) {
	if observedAt.IsZero() || failure.StatusCode == 499 {
		return OpenCodeGoClassifiedFailure{}, false
	}

	errorType := strings.ToLower(strings.TrimSpace(failure.ErrorType))
	message := strings.TrimSpace(failure.Message)
	observation := OpenCodeGoHealthObservation{
		ObservedAt: observedAt,
		Reason:     message,
		ErrorCode:  firstNonEmptyOpenCodeGoMessage(failure.ErrorCode, failure.ErrorType),
	}

	workspace := func(kind OpenCodeGoHealthObservationKind) (OpenCodeGoClassifiedFailure, bool) {
		observation.Kind = kind
		return OpenCodeGoClassifiedFailure{Scope: OpenCodeGoHealthScopeWorkspace, Observation: observation}, true
	}
	modelScope := func(kind OpenCodeGoHealthObservationKind, fallback time.Duration) (OpenCodeGoClassifiedFailure, bool) {
		observation.Kind = kind
		observation.Deadline = openCodeGoFailureDeadline(observedAt, failure.RetryAfter, fallback)
		return OpenCodeGoClassifiedFailure{Scope: OpenCodeGoHealthScopeModel, Observation: observation}, true
	}
	quota := func(kind string) (OpenCodeGoClassifiedFailure, bool) {
		observation.Kind = OpenCodeGoObservationQuotaExhausted
		observation.QuotaKind = kind
		observation.Deadline = openCodeGoFailureDeadline(observedAt, failure.RetryAfter, 0)
		return OpenCodeGoClassifiedFailure{Scope: OpenCodeGoHealthScopeWorkspace, Observation: observation}, true
	}

	switch errorType {
	case "autherror":
		if failure.StatusCode == http.StatusUnauthorized && strings.Contains(strings.ToLower(message), "request blocked by upstream provider") {
			return workspace(OpenCodeGoObservationRiskBlocked)
		}
		if openCodeGoAuthErrorIsModelScoped(message) {
			return modelScope(OpenCodeGoObservationModelBlocked, openCodeGoDefaultModelCooldown)
		}
		return workspace(OpenCodeGoObservationCredentialFailure)
	case "creditserror":
		return quota("")
	case "monthlylimiterror", "userlimiterror":
		return quota(model.OpenCodeGoQuotaMonthly)
	case "gousagelimiterror":
		return quota(openCodeGoQuotaKindForLimitName(failure.LimitName))
	case "blackusagelimiterror":
		return quota("")
	case "regionerror":
		return modelScope(OpenCodeGoObservationRegionBlocked, openCodeGoDefaultRegionCooldown)
	case "modelerror":
		return modelScope(OpenCodeGoObservationModelBlocked, openCodeGoDefaultModelCooldown)
	case "ratelimiterror", "freeusagelimiterror":
		return modelScope(OpenCodeGoObservationRPMThrottled, openCodeGoDefaultRPMCooldown)
	}

	switch {
	case failure.StatusCode == http.StatusUnauthorized:
		return workspace(OpenCodeGoObservationCredentialFailure)
	case failure.StatusCode == http.StatusForbidden:
		return modelScope(OpenCodeGoObservationRegionBlocked, openCodeGoDefaultRegionCooldown)
	case failure.StatusCode == http.StatusTooManyRequests:
		return modelScope(OpenCodeGoObservationRPMThrottled, openCodeGoDefaultRPMCooldown)
	case failure.StatusCode == http.StatusRequestTimeout,
		failure.StatusCode == http.StatusTooEarly,
		failure.StatusCode >= http.StatusInternalServerError:
		return modelScope(OpenCodeGoObservationTransientFailure, openCodeGoDefaultTransientCooldown)
	default:
		return OpenCodeGoClassifiedFailure{}, false
	}
}

func ClassifyOpenCodeGoTransportFailure(message string, observedAt time.Time) OpenCodeGoClassifiedFailure {
	return OpenCodeGoClassifiedFailure{
		Scope: OpenCodeGoHealthScopeModel,
		Observation: OpenCodeGoHealthObservation{
			Kind:       OpenCodeGoObservationTransientFailure,
			ObservedAt: observedAt,
			Reason:     message,
			ErrorCode:  "transport_error",
			Deadline:   observedAt.Add(openCodeGoDefaultTransientCooldown),
		},
	}
}

func openCodeGoAuthErrorIsModelScoped(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "model") &&
		(strings.Contains(message, "not supported") || strings.Contains(message, "unsupported"))
}

func openCodeGoQuotaKindForLimitName(limitName string) string {
	switch strings.ToLower(strings.TrimSpace(limitName)) {
	case "5 hour":
		return model.OpenCodeGoQuotaRolling
	case "weekly":
		return model.OpenCodeGoQuotaWeekly
	case "monthly":
		return model.OpenCodeGoQuotaMonthly
	default:
		return ""
	}
}

func openCodeGoFailureDeadline(observedAt time.Time, retryAfter string, fallback time.Duration) time.Time {
	if duration, ok := parseOpenCodeGoRetryAfter(retryAfter, observedAt); ok {
		return observedAt.Add(duration)
	}
	if fallback <= 0 {
		return time.Time{}
	}
	return observedAt.Add(fallback)
}

func parseOpenCodeGoRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			return 0, false
		}
		maxSeconds := int64(openCodeGoMaxRetryAfter / time.Second)
		if seconds > maxSeconds {
			seconds = maxSeconds
		}
		return time.Duration(seconds) * time.Second, true
	}
	deadline, err := http.ParseTime(value)
	if err != nil || !deadline.After(now) {
		return 0, false
	}
	duration := deadline.Sub(now)
	if duration > openCodeGoMaxRetryAfter {
		duration = openCodeGoMaxRetryAfter
	}
	return duration, true
}
