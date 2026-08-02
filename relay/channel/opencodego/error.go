package opencodego

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

const (
	maxErrorBodyBytes    = 64 << 10
	maxErrorMessageBytes = 512
	maxRetryAfterBytes   = 128
)

var (
	apiKeyPattern    = regexp.MustCompile(`(?i)sk-[a-z0-9_-]+`)
	workspacePattern = regexp.MustCompile(`(?i)wrk_[a-z0-9]+`)
)

type upstreamErrorEnvelope struct {
	Type     string          `json:"type"`
	Message  string          `json:"message"`
	Code     any             `json:"code"`
	Error    json.RawMessage `json:"error"`
	Metadata struct {
		LimitName string `json:"limitName"`
	} `json:"metadata"`
}

type upstreamErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    any    `json:"code"`
}

func (a *Adaptor) HandleNon2xxResponse(c *gin.Context, resp *http.Response, _ *relaycommon.RelayInfo) (*types.NewAPIError, *channel.Non2xxResponseObservation) {
	if resp == nil {
		err := types.NewOpenAIError(errors.New("OpenCode Go returned no response"), types.ErrorCodeBadResponseStatusCode, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
		return err, &channel.Non2xxResponseObservation{Provider: ChannelName, StatusCode: http.StatusBadGateway, ErrorCode: string(types.ErrorCodeBadResponseStatusCode)}
	}
	defer service.CloseResponseBodyGracefully(resp)

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes+1))
	if readErr != nil {
		err := types.NewOpenAIError(errors.New("failed to read OpenCode Go error response"), types.ErrorCodeReadResponseBodyFailed, resp.StatusCode, types.ErrOptionWithSkipRetry())
		return err, &channel.Non2xxResponseObservation{Provider: ChannelName, StatusCode: resp.StatusCode, ErrorCode: string(types.ErrorCodeReadResponseBodyFailed)}
	}
	if len(body) > maxErrorBodyBytes {
		body = body[:maxErrorBodyBytes]
	}

	errorType, errorCode, message, limitName := parseUpstreamError(body)
	message = sanitizeErrorMessage(message)
	if message == "" {
		message = fmt.Sprintf("OpenCode Go returned status %d", resp.StatusCode)
	}
	errorType = sanitizeErrorIdentifier(errorType)
	errorCode = sanitizeErrorIdentifier(errorCode)
	if errorType == "" {
		errorType = "upstream_error"
	}
	if errorCode == "" {
		errorCode = errorType
	}

	retryAfter := sanitizeRetryAfter(resp.Header.Get("Retry-After"))
	if retryAfter != "" && c != nil {
		c.Header("Retry-After", retryAfter)
	}
	var metadata json.RawMessage
	if limitName != "" || retryAfter != "" {
		metadata, _ = common.Marshal(struct {
			LimitName  string `json:"limit_name,omitempty"`
			RetryAfter string `json:"retry_after,omitempty"`
		}{LimitName: limitName, RetryAfter: retryAfter})
	}
	openAIError := types.OpenAIError{
		Message:  message,
		Type:     errorType,
		Code:     errorCode,
		Metadata: metadata,
	}
	err := types.WithOpenAIError(openAIError, resp.StatusCode, types.ErrOptionWithSkipRetry())
	observation := &channel.Non2xxResponseObservation{
		Provider:   ChannelName,
		StatusCode: resp.StatusCode,
		ErrorType:  errorType,
		ErrorCode:  errorCode,
		Message:    message,
		RetryAfter: retryAfter,
		LimitName:  limitName,
	}
	return err, observation
}

func parseUpstreamError(body []byte) (errorType string, errorCode string, message string, limitName string) {
	var envelope upstreamErrorEnvelope
	if len(body) == 0 || common.Unmarshal(body, &envelope) != nil {
		return "", "", "", ""
	}
	errorType = envelope.Type
	errorCode = stringifyErrorCode(envelope.Code)
	message = envelope.Message
	limitName = sanitizeLimitName(envelope.Metadata.LimitName)

	if len(envelope.Error) == 0 {
		return
	}
	var detail upstreamErrorDetail
	if common.Unmarshal(envelope.Error, &detail) == nil {
		if detail.Type != "" {
			errorType = detail.Type
		}
		if code := stringifyErrorCode(detail.Code); code != "" {
			errorCode = code
		}
		if detail.Message != "" {
			message = detail.Message
		}
		return
	}
	var stringError string
	if common.Unmarshal(envelope.Error, &stringError) == nil && stringError != "" {
		message = stringError
	}
	return
}

func stringifyErrorCode(value any) string {
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

func sanitizeErrorMessage(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	message = apiKeyPattern.ReplaceAllString(message, "[REDACTED]")
	message = workspacePattern.ReplaceAllString(message, "[WORKSPACE]")
	message = common.MaskSensitiveInfo(message)
	return truncateUTF8(message, maxErrorMessageBytes)
}

func sanitizeErrorIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	for i := 0; i < len(value); i++ {
		b := value[i]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') || b == '_' || b == '-' || b == '.' || b == ':' {
			continue
		}
		return ""
	}
	return value
}

func sanitizeRetryAfter(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxRetryAfterBytes || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	return value
}

func sanitizeLimitName(value string) string {
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

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
