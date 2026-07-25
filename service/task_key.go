package service

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// ResolveStoredTaskKey returns only the credential captured when the Task was
// submitted. Polling never substitutes the channel's current/random key.
func ResolveStoredTaskKey(channel *model.Channel, storedKey string) (string, error) {
	if channel == nil || storedKey == "" {
		return "", errors.New("stored task credential is unavailable")
	}
	if !channel.ChannelInfo.IsMultiKey {
		if channel.Key != storedKey {
			return "", errors.New("stored task credential is no longer configured")
		}
		return storedKey, nil
	}
	keys := channel.GetKeys()
	for index, key := range keys {
		if key != storedKey {
			continue
		}
		status := common.ChannelStatusEnabled
		if configured, ok := channel.ChannelInfo.MultiKeyStatusList[index]; ok {
			status = configured
		}
		if status == common.ChannelStatusEnabled {
			return storedKey, nil
		}
	}
	return "", errors.New("stored task credential is disabled or removed")
}
