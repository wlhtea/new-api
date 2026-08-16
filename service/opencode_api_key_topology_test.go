package service

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func createOpenCodeAPIKeyTopologyChannel(
	t *testing.T,
	db *gorm.DB,
	id int,
	channelType int,
	status int,
	groups string,
	modelName string,
) model.Channel {
	t.Helper()
	priority := int64(0)
	weight := uint(1)
	header := fmt.Sprintf(`{"X-Topology":"%d"}`, id)
	channel := model.Channel{
		Id:             id,
		Type:           channelType,
		Key:            fmt.Sprintf("topology-key-%d", id),
		Status:         status,
		Name:           fmt.Sprintf("topology-channel-%d", id),
		Models:         modelName,
		Group:          groups,
		Priority:       &priority,
		Weight:         &weight,
		HeaderOverride: &header,
		ChannelInfo: model.ChannelInfo{
			MultiKeyStatusList: map[int]int{0: common.ChannelStatusEnabled},
		},
	}
	require.NoError(t, db.Create(&channel).Error)
	return channel
}

func createOpenCodeAPIKeyTopologyAbility(
	t *testing.T,
	db *gorm.DB,
	group string,
	modelName string,
	channelID int,
	enabled bool,
	priority int64,
	weight uint,
) {
	t.Helper()
	require.NoError(t, db.Create(&model.Ability{
		Group:     group,
		Model:     modelName,
		ChannelId: channelID,
		Enabled:   enabled,
		Priority:  &priority,
		Weight:    weight,
	}).Error)
}

func setOpenCodeAPIKeyTopologyChannelSchedule(
	t *testing.T,
	db *gorm.DB,
	channelID int,
	priority int64,
	weight uint,
) {
	t.Helper()
	require.NoError(t, db.Model(&model.Channel{}).
		Where("id = ?", channelID).
		Updates(map[string]interface{}{
			"priority": priority,
			"weight":   weight,
		}).Error)
}

func TestSnapshotOpenCodeAPIKeyCandidateTopologyIsCompleteUniqueAndDeterministic(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	const modelName = "topology-complete-model"

	shared := createOpenCodeAPIKeyTopologyChannel(t, db, 3100, constant.ChannelTypeOpenCodeAPIKey, common.ChannelStatusEnabled, "vip,default", modelName)
	createOpenCodeAPIKeyTopologyChannel(t, db, 3101, constant.ChannelTypeOpenCodeAPIKey, common.ChannelStatusEnabled, "vip", modelName)
	createOpenCodeAPIKeyTopologyChannel(t, db, 3102, constant.ChannelTypeOpenCodeAPIKey, common.ChannelStatusEnabled, "vip", modelName)
	createOpenCodeAPIKeyTopologyChannel(t, db, 3103, constant.ChannelTypeOpenCodeAPIKey, common.ChannelStatusEnabled, "vip", modelName)
	createOpenCodeAPIKeyTopologyChannel(t, db, 3104, constant.ChannelTypeOpenCodeAPIKey, common.ChannelStatusEnabled, "default", modelName)
	createOpenCodeAPIKeyTopologyChannel(t, db, 3105, constant.ChannelTypeOpenCodeAPIKey, common.ChannelStatusManuallyDisabled, "vip", modelName)
	createOpenCodeAPIKeyTopologyChannel(t, db, 3106, constant.ChannelTypeOpenCodeAPIKey, common.ChannelStatusEnabled, "vip", modelName)
	setOpenCodeAPIKeyTopologyChannelSchedule(t, db, 3100, 30, 20)
	setOpenCodeAPIKeyTopologyChannelSchedule(t, db, 3101, 30, 30)
	setOpenCodeAPIKeyTopologyChannelSchedule(t, db, 3102, 30, 30)
	setOpenCodeAPIKeyTopologyChannelSchedule(t, db, 3103, 20, 100)
	setOpenCodeAPIKeyTopologyChannelSchedule(t, db, 3104, 40, 1)
	setOpenCodeAPIKeyTopologyChannelSchedule(t, db, 3105, 100, 100)
	setOpenCodeAPIKeyTopologyChannelSchedule(t, db, 3106, 100, 100)

	createOpenCodeAPIKeyTopologyAbility(t, db, "vip", modelName, 3100, true, 30, 20)
	createOpenCodeAPIKeyTopologyAbility(t, db, "default", modelName, 3100, true, 30, 20)
	createOpenCodeAPIKeyTopologyAbility(t, db, "vip", modelName, 3101, true, 30, 30)
	createOpenCodeAPIKeyTopologyAbility(t, db, "vip", modelName, 3102, true, 30, 30)
	createOpenCodeAPIKeyTopologyAbility(t, db, "vip", modelName, 3103, true, 20, 100)
	createOpenCodeAPIKeyTopologyAbility(t, db, "default", modelName, 3104, true, 40, 1)
	createOpenCodeAPIKeyTopologyAbility(t, db, "vip", modelName, 3105, false, 100, 100)
	createOpenCodeAPIKeyTopologyAbility(t, db, "vip", modelName, 3106, false, 100, 100)

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyTokenAutoGroups, []string{"vip", "default"})
	common.SetContextKey(c, constant.ContextKeyAutoGroup, "unchanged")
	common.SetContextKey(c, constant.ContextKeyAutoGroupIndex, 7)

	var channelCountBefore int64
	var abilityCountBefore int64
	require.NoError(t, db.Model(&model.Channel{}).Count(&channelCountBefore).Error)
	require.NoError(t, db.Model(&model.Ability{}).Count(&abilityCountBefore).Error)

	topology, err := SnapshotOpenCodeAPIKeyCandidateTopology(
		c,
		"auto",
		"default",
		modelName,
		"/v1/chat/completions",
	)
	require.NoError(t, err)

	type topologyKey struct {
		group     string
		channelID int
	}
	actual := make([]topologyKey, 0, len(topology))
	unique := make(map[topologyKey]struct{}, len(topology))
	for _, candidate := range topology {
		require.NotNil(t, candidate.Channel)
		key := topologyKey{group: candidate.SelectionGroup, channelID: candidate.Channel.Id}
		actual = append(actual, key)
		unique[key] = struct{}{}
	}
	assert.Equal(t, []topologyKey{
		{group: "vip", channelID: 3101},
		{group: "vip", channelID: 3102},
		{group: "vip", channelID: 3100},
		{group: "vip", channelID: 3103},
		{group: "default", channelID: 3104},
		{group: "default", channelID: 3100},
	}, actual)
	assert.Len(t, unique, len(topology), "one group/channel pair must never be repeated")
	assert.Equal(t, "unchanged", common.GetContextKeyString(c, constant.ContextKeyAutoGroup))
	assert.Equal(t, 7, common.GetContextKeyInt(c, constant.ContextKeyAutoGroupIndex))

	var channelCountAfter int64
	var abilityCountAfter int64
	require.NoError(t, db.Model(&model.Channel{}).Count(&channelCountAfter).Error)
	require.NoError(t, db.Model(&model.Ability{}).Count(&abilityCountAfter).Error)
	assert.Equal(t, channelCountBefore, channelCountAfter)
	assert.Equal(t, abilityCountBefore, abilityCountAfter)

	var sharedCopies []*model.Channel
	for _, candidate := range topology {
		if candidate.Channel.Id == shared.Id {
			sharedCopies = append(sharedCopies, candidate.Channel)
		}
	}
	require.Len(t, sharedCopies, 2)
	require.NotSame(t, sharedCopies[0], sharedCopies[1])
	sharedCopies[0].ChannelInfo.MultiKeyStatusList[0] = common.ChannelStatusManuallyDisabled
	require.NotNil(t, sharedCopies[0].HeaderOverride)
	require.NotNil(t, sharedCopies[1].HeaderOverride)
	*sharedCopies[0].HeaderOverride = `{"X-Topology":"mutated"}`
	assert.Equal(t, common.ChannelStatusEnabled, sharedCopies[1].ChannelInfo.MultiKeyStatusList[0])
	assert.Equal(t, fmt.Sprintf(`{"X-Topology":"%d"}`, shared.Id), *sharedCopies[1].HeaderOverride)
}

func TestSnapshotOpenCodeAPIKeyCandidateTopologyRejectsMixedProviderGroup(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	const modelName = "topology-mixed-provider-model"

	createOpenCodeAPIKeyTopologyChannel(t, db, 3150, constant.ChannelTypeOpenCodeAPIKey, common.ChannelStatusEnabled, "vip", modelName)
	createOpenCodeAPIKeyTopologyChannel(t, db, 3151, constant.ChannelTypeOpenCodeGo, common.ChannelStatusEnabled, "vip", modelName)
	createOpenCodeAPIKeyTopologyAbility(t, db, "vip", modelName, 3150, true, 0, 1)
	createOpenCodeAPIKeyTopologyAbility(t, db, "vip", modelName, 3151, true, 0, 1)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	topology, err := SnapshotOpenCodeAPIKeyCandidateTopology(c, "vip", "default", modelName, "/v1/chat/completions")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrOpenCodeAPIKeyMixedTopology)
	assert.Nil(t, topology)
}

func TestSnapshotOpenCodeAPIKeyCandidateTopologyRejectsTornChannelAbilityState(t *testing.T) {
	const requestModel = "topology-torn-model"
	tests := []struct {
		name            string
		channelStatus   int
		channelGroup    string
		channelModel    string
		abilityPriority int64
		abilityWeight   uint
	}{
		{
			name:            "status",
			channelStatus:   common.ChannelStatusManuallyDisabled,
			channelGroup:    "vip",
			channelModel:    requestModel,
			abilityPriority: 0,
			abilityWeight:   1,
		},
		{
			name:            "group",
			channelStatus:   common.ChannelStatusEnabled,
			channelGroup:    "other",
			channelModel:    requestModel,
			abilityPriority: 0,
			abilityWeight:   1,
		},
		{
			name:            "model",
			channelStatus:   common.ChannelStatusEnabled,
			channelGroup:    "vip",
			channelModel:    "topology-other-model",
			abilityPriority: 0,
			abilityWeight:   1,
		},
		{
			name:            "priority",
			channelStatus:   common.ChannelStatusEnabled,
			channelGroup:    "vip",
			channelModel:    requestModel,
			abilityPriority: 1,
			abilityWeight:   1,
		},
		{
			name:            "weight",
			channelStatus:   common.ChannelStatusEnabled,
			channelGroup:    "vip",
			channelModel:    requestModel,
			abilityPriority: 0,
			abilityWeight:   2,
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := setupChannelSelectAutoGroupsTest(t)
			channelID := 3160 + index
			createOpenCodeAPIKeyTopologyChannel(
				t,
				db,
				channelID,
				constant.ChannelTypeOpenCodeAPIKey,
				test.channelStatus,
				test.channelGroup,
				test.channelModel,
			)
			createOpenCodeAPIKeyTopologyAbility(
				t,
				db,
				"vip",
				requestModel,
				channelID,
				true,
				test.abilityPriority,
				test.abilityWeight,
			)

			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			topology, err := SnapshotOpenCodeAPIKeyCandidateTopology(
				c,
				"vip",
				"default",
				requestModel,
				"/v1/chat/completions",
			)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrOpenCodeAPIKeyInconsistentTopology))
			assert.Nil(t, topology)
		})
	}
}

func TestSnapshotOpenCodeAPIKeyCandidateTopologyUsesNormalizedModelOnlyAsFallback(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	const (
		requestModel    = "gpt-4-gizmo-topology"
		normalizedModel = "gpt-4-gizmo-*"
	)
	createOpenCodeAPIKeyTopologyChannel(t, db, 3200, constant.ChannelTypeOpenCodeAPIKey, common.ChannelStatusEnabled, "vip", requestModel)
	createOpenCodeAPIKeyTopologyChannel(t, db, 3201, constant.ChannelTypeOpenCodeAPIKey, common.ChannelStatusEnabled, "vip", normalizedModel)
	setOpenCodeAPIKeyTopologyChannelSchedule(t, db, 3200, 1, 1)
	setOpenCodeAPIKeyTopologyChannelSchedule(t, db, 3201, 100, 100)
	createOpenCodeAPIKeyTopologyAbility(t, db, "vip", requestModel, 3200, true, 1, 1)
	createOpenCodeAPIKeyTopologyAbility(t, db, "vip", normalizedModel, 3201, true, 100, 100)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	topology, err := SnapshotOpenCodeAPIKeyCandidateTopology(c, "vip", "default", requestModel, "/v1/responses")
	require.NoError(t, err)
	require.Len(t, topology, 1)
	assert.Equal(t, 3200, topology[0].Channel.Id)

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.Channel{}).
			Where("id = ?", 3200).
			Update("status", common.ChannelStatusManuallyDisabled).Error; err != nil {
			return err
		}
		return tx.Model(&model.Ability{}).
			Where("channel_id = ? AND `group` = ? AND model = ?", 3200, "vip", requestModel).
			Update("enabled", false).Error
	}))
	topology, err = SnapshotOpenCodeAPIKeyCandidateTopology(c, "vip", "default", requestModel, "/v1/responses")
	require.NoError(t, err)
	require.Len(t, topology, 1)
	assert.Equal(t, 3201, topology[0].Channel.Id)
}
