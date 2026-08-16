package service

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

func formatNotifyType(channelId int, status int) string {
	return fmt.Sprintf("%s_%d_%d", dto.NotifyTypeChannelUpdate, channelId, status)
}

// disable & notify
func DisableChannel(channelError types.ChannelError, reason string) {
	common.SysLog(fmt.Sprintf("通道「%s」（#%d）发生错误，准备禁用，原因：%s", channelError.ChannelName, channelError.ChannelId, common.LocalLogPreview(reason)))

	// 检查是否启用自动禁用功能
	if !channelError.AutoBan {
		common.SysLog(fmt.Sprintf("通道「%s」（#%d）未启用自动禁用功能，跳过禁用操作", channelError.ChannelName, channelError.ChannelId))
		return
	}

	success := updateOpenCodeGoChannelStatus(
		channelError.ChannelId,
		channelError.ChannelType,
		channelError.UsingKey,
		common.ChannelStatusAutoDisabled,
		reason,
	)
	if success {
		subject := fmt.Sprintf("通道「%s」（#%d）已被禁用", channelError.ChannelName, channelError.ChannelId)
		content := fmt.Sprintf("通道「%s」（#%d）已被禁用，原因：%s", channelError.ChannelName, channelError.ChannelId, reason)
		NotifyRootUser(formatNotifyType(channelError.ChannelId, common.ChannelStatusAutoDisabled), subject, content)
	}
}

func EnableChannel(channelId int, channelType int, usingKey string, channelName string) {
	success := updateOpenCodeGoChannelStatus(channelId, channelType, usingKey, common.ChannelStatusEnabled, "")
	if success {
		subject := fmt.Sprintf("通道「%s」（#%d）已被启用", channelName, channelId)
		content := fmt.Sprintf("通道「%s」（#%d）已被启用", channelName, channelId)
		NotifyRootUser(formatNotifyType(channelId, common.ChannelStatusEnabled), subject, content)
	}
}

func updateOpenCodeGoChannelStatus(channelID int, channelType int, usingKey string, status int, reason string) bool {
	if channelType != constant.ChannelTypeOpenCodeGo {
		return model.UpdateChannelStatus(channelID, usingKey, status, reason)
	}

	releaseMutation := BeginOpenCodeGoPoolMutation(channelID)
	defer releaseMutation()
	if !model.UpdateChannelStatus(channelID, usingKey, status, reason) {
		return false
	}
	if status != common.ChannelStatusEnabled {
		RemoveOpenCodeGoPoolChannel(channelID)
		return true
	}
	InvalidateOpenCodeGoIdentityProxyChannel(channelID)
	if err := ReconcileOpenCodeGoPoolChannel(channelID); err != nil {
		common.SysError(fmt.Sprintf("failed to rebuild OpenCode Go account pool after channel enable: channel_id=%d error=%v", channelID, err))
	}
	return true
}

func ShouldDisableChannel(err *types.NewAPIError) bool {
	if !common.AutomaticDisableChannelEnabled {
		return false
	}
	if err == nil {
		return false
	}
	if err.Provenance().IsLocal() || err.Provenance().IsGateway() {
		return false
	}
	if IsOpenCodeGoRawInvalidRequestError(err) {
		return false
	}
	if types.IsChannelError(err) {
		return true
	}
	statusCode := OpenCodeGoRelayPolicyStatusCode(err)
	// skipRetry protects replay safety, but it must not discard authoritative
	// upstream health evidence such as a rejected API key after bytes were sent.
	provenance := err.Provenance()
	if IsOpenCodeGoUpstreamRelayError(err) && provenance.RawStatusCode > 0 &&
		operation_setting.ShouldDisableByStatusCode(statusCode) {
		return true
	}
	if types.IsSkipRetryError(err) {
		return false
	}
	if operation_setting.ShouldDisableByStatusCode(statusCode) {
		return true
	}

	lowerMessage := strings.ToLower(err.Error())
	search, _ := AcSearch(lowerMessage, operation_setting.AutomaticDisableKeywords, true)
	return search
}

func ShouldEnableChannel(newAPIError *types.NewAPIError, status int) bool {
	if !common.AutomaticEnableChannelEnabled {
		return false
	}
	if newAPIError != nil {
		return false
	}
	if status != common.ChannelStatusAutoDisabled {
		return false
	}
	return true
}
