package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func multiKeyChannel(keys []string, statuses map[int]int) *model.Channel {
	return &model.Channel{
		Key:  "configured-multi-key",
		Keys: append([]string(nil), keys...),
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:         true,
			MultiKeyStatusList: statuses,
		},
	}
}

func TestResolveStoredTaskKeySingleKey(t *testing.T) {
	channel := &model.Channel{
		Key: "KEY_A",
		ChannelInfo: model.ChannelInfo{
			IsMultiKey: false,
		},
	}
	got, err := ResolveStoredTaskKey(channel, "KEY_A")
	require.NoError(t, err)
	assert.Equal(t, "KEY_A", got)
}

func TestResolveStoredTaskKeySingleKeyChangedAfterSubmit(t *testing.T) {
	channel := &model.Channel{
		Key: "KEY_B",
		ChannelInfo: model.ChannelInfo{
			IsMultiKey: false,
		},
	}
	_, err := ResolveStoredTaskKey(channel, "KEY_A")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "KEY_A")
	assert.NotContains(t, err.Error(), "KEY_B")
}

func TestResolveStoredTaskKeyRejectsUnavailableCredential(t *testing.T) {
	tests := []struct {
		name      string
		channel   *model.Channel
		storedKey string
	}{
		{name: "nil channel", channel: nil, storedKey: "KEY_A"},
		{name: "empty stored key", channel: &model.Channel{Key: "KEY_A"}},
		{
			name: "empty multi key channel",
			channel: multiKeyChannel(
				nil,
				map[int]int{},
			),
			storedKey: "KEY_A",
		},
		{
			name:      "removed multi key",
			channel:   multiKeyChannel([]string{"KEY_B"}, nil),
			storedKey: "KEY_A",
		},
		{
			name: "disabled multi key",
			channel: multiKeyChannel(
				[]string{"KEY_A", "KEY_B"},
				map[int]int{0: common.ChannelStatusManuallyDisabled},
			),
			storedKey: "KEY_A",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ResolveStoredTaskKey(test.channel, test.storedKey)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "KEY_A")
			assert.NotContains(t, err.Error(), "KEY_B")
		})
	}
}

func TestResolveStoredTaskKeyMultiKeyEnabledRules(t *testing.T) {
	tests := []struct {
		name     string
		channel  *model.Channel
		expected string
	}{
		{
			name:     "missing status defaults enabled",
			channel:  multiKeyChannel([]string{"KEY_A"}, nil),
			expected: "KEY_A",
		},
		{
			name: "explicit enabled",
			channel: multiKeyChannel(
				[]string{"KEY_A"},
				map[int]int{0: common.ChannelStatusEnabled},
			),
			expected: "KEY_A",
		},
		{
			name: "duplicate has one enabled entry",
			channel: multiKeyChannel(
				[]string{"KEY_A", "KEY_A"},
				map[int]int{
					0: common.ChannelStatusManuallyDisabled,
					1: common.ChannelStatusEnabled,
				},
			),
			expected: "KEY_A",
		},
		{
			name: "duplicate missing status is enabled",
			channel: multiKeyChannel(
				[]string{"KEY_A", "KEY_A"},
				map[int]int{0: common.ChannelStatusManuallyDisabled},
			),
			expected: "KEY_A",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveStoredTaskKey(test.channel, "KEY_A")
			require.NoError(t, err)
			assert.Equal(t, test.expected, got)
		})
	}
}

func TestResolveStoredTaskKeyNeverFallsBack(t *testing.T) {
	channel := multiKeyChannel(
		[]string{"KEY_A", "KEY_B"},
		map[int]int{0: common.ChannelStatusManuallyDisabled, 1: common.ChannelStatusEnabled},
	)
	_, err := ResolveStoredTaskKey(channel, "KEY_A")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "KEY_A")
	assert.NotContains(t, err.Error(), "KEY_B")
}
