package opencodego

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

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
)

var observeOpenCodeGoProviderFailure = service.ObserveOpenCodeGoProviderFailure

func (a *Adaptor) HandleNon2xxResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*types.NewAPIError, *channel.Non2xxResponseObservation) {
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

	failure := service.ParseOpenCodeGoProviderFailure(resp.StatusCode, resp.Header, body)
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
	a.persistProviderFailure(info, observation)
	return err, observation
}

func (a *Adaptor) persistProviderFailure(info *relaycommon.RelayInfo, observation *channel.Non2xxResponseObservation) {
	if a == nil || !a.workspaceSelected || a.selectedWorkspaceUID == "" || info == nil || observation == nil {
		return
	}
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
		openCodeGoHealthNow(),
	)
	if err != nil {
		common.SysError(fmt.Sprintf(
			"failed to persist OpenCode Go provider health observation: channel_id=%d workspace_uid=%s error=%v",
			info.ChannelId,
			a.selectedWorkspaceUID,
			err,
		))
	}
}

func sanitizeErrorMessage(message string) string {
	return service.SanitizeOpenCodeGoProviderMessage(message)
}
