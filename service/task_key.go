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
	lock := model.GetChannelPollingLock(channel.Id)
	lock.Lock()
	isMultiKey := channel.ChannelInfo.IsMultiKey
	configuredKey := channel.Key
	keys := append([]string(nil), channel.GetKeys()...)
	statuses := make(map[int]int, len(channel.ChannelInfo.MultiKeyStatusList))
	for index, status := range channel.ChannelInfo.MultiKeyStatusList {
		statuses[index] = status
	}
	lock.Unlock()

	if !isMultiKey {
		if configuredKey != storedKey {
			return "", errors.New("stored task credential is no longer configured")
		}
		return storedKey, nil
	}
	for index, key := range keys {
		if key != storedKey {
			continue
		}
		status := common.ChannelStatusEnabled
		if configured, ok := statuses[index]; ok {
			status = configured
		}
		if status == common.ChannelStatusEnabled {
			return storedKey, nil
		}
	}
	return "", errors.New("stored task credential is disabled or removed")
}
