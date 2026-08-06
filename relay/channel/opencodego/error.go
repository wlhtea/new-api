package opencodego

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

const (
	maxErrorBodyBytes    = 64 << 10
	maxErrorMessageBytes = 512
)

var observeOpenCodeGoProviderFailure = service.ObserveOpenCodeGoProviderFailure

func (a *Adaptor) HandleNon2xxResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*types.NewAPIError, *channel.Non2xxResponseObservation) {
	if resp == nil {
		err := types.NewOpenAIError(errors.New("OpenCode Go returned no response"), types.ErrorCodeBadResponseStatusCode, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
		return err, &channel.Non2xxResponseObservation{Provider: ChannelName, StatusCode: http.StatusBadGateway, ErrorCode: string(types.ErrorCodeBadResponseStatusCode)}
	}
	defer a.releaseInFlight()
	defer service.CloseResponseBodyGracefully(resp)

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes+1))
	if readErr != nil {
		failure := service.ParseOpenCodeGoProviderFailure(resp.StatusCode, resp.Header, nil)
		if failure.RetryAfter != "" && c != nil {
			c.Header("Retry-After", failure.RetryAfter)
		}
		observation := &channel.Non2xxResponseObservation{
			Provider:   ChannelName,
			StatusCode: resp.StatusCode,
			ErrorType:  failure.ErrorType,
			ErrorCode:  string(types.ErrorCodeReadResponseBodyFailed),
			Message:    failure.Message,
			RetryAfter: failure.RetryAfter,
			LimitName:  failure.LimitName,
		}
		a.logProviderFailureDiagnostics(c, resp.StatusCode, info)
		if !openCodeGoCallerCancelled(c, readErr) {
			a.persistProviderFailure(c, info, failure, observation)
		}
		err := types.NewOpenAIError(errors.New("failed to read OpenCode Go error response"), types.ErrorCodeReadResponseBodyFailed, resp.StatusCode, types.ErrOptionWithSkipRetry())
		return err, observation
	}
	if len(body) > maxErrorBodyBytes {
		body = body[:maxErrorBodyBytes]
	}

	failure := service.ParseOpenCodeGoProviderFailure(resp.StatusCode, resp.Header, body)
	a.logProviderFailureDiagnostics(c, resp.StatusCode, info)
	errorType := failure.ErrorType
	errorCode := failure.ErrorCode
	message := failure.Message
	limitName := failure.LimitName
	retryAfter := failure.RetryAfter
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
	a.persistProviderFailure(c, info, failure, observation)
	return err, observation
}

func (a *Adaptor) logProviderFailureDiagnostics(c *gin.Context, statusCode int, info *relaycommon.RelayInfo) {
	if a == nil || info == nil {
		return
	}
	workspaceRef := ""
	if a.selectedWorkspaceUID != "" {
		workspaceRef = hashCacheIdentity("diagnostic-workspace", a.selectedWorkspaceUID)
	}
	affinityRef := ""
	if a.affinityIdentity != "" {
		affinityRef = hashCacheIdentity("diagnostic-affinity", a.affinityIdentity)
	}
	logger.LogWarn(c, fmt.Sprintf(
		"OpenCode Go upstream failure diagnostics: status=%d channel_id=%d model=%q protocol=%q client_format=%q upstream_stream=%t buffered_tool=%t input_items=%d tools=%d body_bytes=%d workspace_ref=%s affinity_ref=%s",
		statusCode,
		info.ChannelId,
		info.UpstreamModelName,
		a.protocol,
		info.RelayFormat,
		a.requestUpstreamStream,
		a.bufferClaudeToolCall,
		a.requestInputItems,
		a.requestToolCount,
		info.UpstreamRequestBodySize,
		workspaceRef,
		affinityRef,
	))
}

func (a *Adaptor) persistProviderFailure(c *gin.Context, info *relaycommon.RelayInfo, failure service.OpenCodeGoProviderFailure, observation *channel.Non2xxResponseObservation) {
	if a == nil || !a.workspaceSelected || a.selectedWorkspaceUID == "" || info == nil || observation == nil || openCodeGoCallerCancelled(c, nil) {
		return
	}
	observedAt := openCodeGoHealthNow()
	_, authoritative := service.ClassifyOpenCodeGoProviderFailure(failure, observedAt)
	_, err := observeOpenCodeGoProviderFailure(
		info.ChannelId,
		a.selectedWorkspaceUID,
		info.UpstreamModelName,
		service.OpenCodeGoProviderFailure{
			StatusCode: observation.StatusCode,
			ErrorType:  observation.ErrorType,
			ErrorCode:  observation.ErrorCode,
			Message:    observation.Message,
			RetryAfter: observation.RetryAfter,
			LimitName:  observation.LimitName,
		},
		observedAt,
	)
	if err != nil {
		common.SysError(fmt.Sprintf(
			"failed to persist OpenCode Go provider health observation: channel_id=%d workspace_ref=%s error=%v",
			info.ChannelId,
			hashCacheIdentity("diagnostic-workspace", a.selectedWorkspaceUID),
			err,
		))
	}
	// Repeated persistent failures (401/403) auto-disable the workspace pending
	// manual verification, separate from model-level cooldowns.
	if disabled, bulkErr := service.ObserveOpenCodeGoBulkProviderFailure(
		info.ChannelId,
		a.selectedWorkspaceUID,
		observation.StatusCode,
		observation.Message,
		observedAt,
	); bulkErr != nil {
		common.SysError(fmt.Sprintf(
			"failed to persist OpenCode Go bulk failure observation: channel_id=%d workspace_ref=%s error=%v",
			info.ChannelId,
			hashCacheIdentity("diagnostic-workspace", a.selectedWorkspaceUID),
			bulkErr,
		))
	} else if disabled {
		logger.LogWarn(c, fmt.Sprintf(
			"OpenCode Go workspace auto-disabled after repeated provider failures: channel_id=%d workspace_ref=%s status=%d",
			info.ChannelId,
			hashCacheIdentity("diagnostic-workspace", a.selectedWorkspaceUID),
			observation.StatusCode,
		))
	}
	if !authoritative && service.IsOpenCodeGoGenericFailoverStatus(observation.StatusCode) {
		a.recordFailoverFailure(c, info, fmt.Sprintf("http_%d", observation.StatusCode))
	}
}

func sanitizeErrorMessage(message string) string {
	return service.SanitizeOpenCodeGoProviderMessage(message)
}
