package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/relay/channel/opencodego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func frozenOpenCodeAPIKeyScheduleSelection(group string, channelID int, priority int64, weight uint) frozenOpenCodeAPIKeySelection {
	return frozenOpenCodeAPIKeySelection{
		selectionGroup: group,
		channelID:      channelID,
		priority:       priority,
		weight:         weight,
	}
}

func frozenOpenCodeAPIKeyScheduleKeys(selections []frozenOpenCodeAPIKeySelection) []opencodego.RequestPreflightPlanKey {
	keys := make([]opencodego.RequestPreflightPlanKey, 0, len(selections))
	for _, selection := range selections {
		keys = append(keys, opencodego.RequestPreflightPlanKey{
			SelectionGroup: selection.selectionGroup,
			ChannelID:      selection.channelID,
		})
	}
	return keys
}

func TestDeriveOpenCodeAPIKeyPhysicalScheduleUsesRetryIndexedPriorityTiers(t *testing.T) {
	topology := []frozenOpenCodeAPIKeySelection{
		frozenOpenCodeAPIKeyScheduleSelection("group-a", 11, 30, 10),
		frozenOpenCodeAPIKeyScheduleSelection("group-a", 12, 30, 30),
		frozenOpenCodeAPIKeyScheduleSelection("group-a", 13, 20, 1),
		frozenOpenCodeAPIKeyScheduleSelection("group-b", 11, 40, 1),
		frozenOpenCodeAPIKeyScheduleSelection("group-b", 14, 10, 1),
	}
	initial := opencodego.RequestPreflightPlanKey{SelectionGroup: "group-a", ChannelID: 12}
	pickFirst := func(int) int { return 0 }

	schedule, err := deriveOpenCodeAPIKeyPhysicalScheduleWithPicker(topology, initial, "group-a", false, 1, true, pickFirst)
	require.NoError(t, err)
	assert.Equal(t, []opencodego.RequestPreflightPlanKey{
		{SelectionGroup: "group-a", ChannelID: 12},
		{SelectionGroup: "group-a", ChannelID: 13},
	}, frozenOpenCodeAPIKeyScheduleKeys(schedule))

	schedule, err = deriveOpenCodeAPIKeyPhysicalScheduleWithPicker(topology, initial, "auto", true, 1, true, pickFirst)
	require.NoError(t, err)
	assert.Equal(t, []opencodego.RequestPreflightPlanKey{
		{SelectionGroup: "group-a", ChannelID: 12},
		{SelectionGroup: "group-a", ChannelID: 13},
		{SelectionGroup: "group-b", ChannelID: 11},
		{SelectionGroup: "group-b", ChannelID: 14},
	}, frozenOpenCodeAPIKeyScheduleKeys(schedule))

	schedule, err = deriveOpenCodeAPIKeyPhysicalScheduleWithPicker(topology, initial, "auto", true, 0, true, pickFirst)
	require.NoError(t, err)
	assert.Equal(t, []opencodego.RequestPreflightPlanKey{
		{SelectionGroup: "group-a", ChannelID: 12},
	}, frozenOpenCodeAPIKeyScheduleKeys(schedule))
}

func TestDeriveOpenCodeAPIKeyPhysicalScheduleRejectsDuplicateTopologyKey(t *testing.T) {
	topology := []frozenOpenCodeAPIKeySelection{
		frozenOpenCodeAPIKeyScheduleSelection("group-a", 11, 10, 1),
		frozenOpenCodeAPIKeyScheduleSelection("group-a", 11, 10, 1),
	}

	_, err := deriveOpenCodeAPIKeyPhysicalSchedule(
		topology,
		opencodego.RequestPreflightPlanKey{SelectionGroup: "group-a", ChannelID: 11},
		"group-a",
		false,
		1,
	)
	require.Error(t, err)
}

func TestDeriveOpenCodeAPIKeyPhysicalScheduleRepeatsLowestTierBeforeCrossGroup(t *testing.T) {
	topology := []frozenOpenCodeAPIKeySelection{
		frozenOpenCodeAPIKeyScheduleSelection("group-a", 11, 10, 1),
		frozenOpenCodeAPIKeyScheduleSelection("group-b", 21, 20, 1),
	}

	schedule, err := deriveOpenCodeAPIKeyPhysicalScheduleWithPicker(
		topology,
		opencodego.RequestPreflightPlanKey{SelectionGroup: "group-a", ChannelID: 11},
		"auto",
		true,
		1,
		true,
		func(int) int { return 0 },
	)
	require.NoError(t, err)
	assert.Equal(t, []opencodego.RequestPreflightPlanKey{
		{SelectionGroup: "group-a", ChannelID: 11},
		{SelectionGroup: "group-a", ChannelID: 11},
		{SelectionGroup: "group-b", ChannelID: 21},
		{SelectionGroup: "group-b", ChannelID: 21},
	}, frozenOpenCodeAPIKeyScheduleKeys(schedule))
}

func TestSelectFrozenOpenCodeAPIKeyPriorityTierUsesLegacyWeights(t *testing.T) {
	tier := []frozenOpenCodeAPIKeySelection{
		frozenOpenCodeAPIKeyScheduleSelection("group-a", 11, 10, 1),
		frozenOpenCodeAPIKeyScheduleSelection("group-a", 12, 10, 3),
	}

	first, err := selectFrozenOpenCodeAPIKeyPriorityTier(tier, 0, true, func(max int) int {
		assert.Equal(t, 400, max)
		return 99
	})
	require.NoError(t, err)
	assert.Equal(t, 11, first.channelID)
	second, err := selectFrozenOpenCodeAPIKeyPriorityTier(tier, 0, true, func(max int) int {
		assert.Equal(t, 400, max)
		return 100
	})
	require.NoError(t, err)
	assert.Equal(t, 12, second.channelID)

	first, err = selectFrozenOpenCodeAPIKeyPriorityTier(tier, 0, false, func(max int) int {
		assert.Equal(t, 24, max)
		return 11
	})
	require.NoError(t, err)
	assert.Equal(t, 11, first.channelID)
	second, err = selectFrozenOpenCodeAPIKeyPriorityTier(tier, 0, false, func(max int) int {
		assert.Equal(t, 24, max)
		return 12
	})
	require.NoError(t, err)
	assert.Equal(t, 12, second.channelID)

	zeroWeights := []frozenOpenCodeAPIKeySelection{
		frozenOpenCodeAPIKeyScheduleSelection("group-a", 21, 10, 0),
		frozenOpenCodeAPIKeyScheduleSelection("group-a", 22, 10, 0),
	}
	first, err = selectFrozenOpenCodeAPIKeyPriorityTier(zeroWeights, 0, true, func(max int) int {
		assert.Equal(t, 200, max)
		return 99
	})
	require.NoError(t, err)
	assert.Equal(t, 21, first.channelID)
	second, err = selectFrozenOpenCodeAPIKeyPriorityTier(zeroWeights, 0, true, func(int) int { return 100 })
	require.NoError(t, err)
	assert.Equal(t, 22, second.channelID)

	first, err = selectFrozenOpenCodeAPIKeyPriorityTier(zeroWeights, 0, false, func(max int) int {
		assert.Equal(t, 20, max)
		return 10
	})
	require.NoError(t, err)
	assert.Equal(t, 21, first.channelID)
	second, err = selectFrozenOpenCodeAPIKeyPriorityTier(zeroWeights, 0, false, func(max int) int {
		assert.Equal(t, 20, max)
		return 11
	})
	require.NoError(t, err)
	assert.Equal(t, 22, second.channelID)
}
