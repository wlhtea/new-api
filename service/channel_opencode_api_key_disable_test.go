package service

import (
	"fmt"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestOpenCodeAPIKeyAutoDisableOnlyAffectsFailedChannelRow(t *testing.T) {
	originalDB := model.DB
	originalLogDB := model.LOG_DB
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	model.DB = db
	model.LOG_DB = db
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		model.DB = originalDB
		model.LOG_DB = originalLogDB
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		sqlDB, sqlErr := db.DB()
		if sqlErr == nil {
			require.NoError(t, sqlDB.Close())
		}
	})

	priority := int64(0)
	weight := uint(100)
	tag := "opencode-api-key-pool"
	channels := []model.Channel{
		{
			Type: constant.ChannelTypeOpenCodeAPIKey, Key: "test-key-one",
			Status: common.ChannelStatusEnabled, Name: "api-key-row-1",
			Models: "test-model", Group: "default", Tag: &tag,
			Priority: &priority, Weight: &weight,
		},
		{
			Type: constant.ChannelTypeOpenCodeAPIKey, Key: "test-key-two",
			Status: common.ChannelStatusEnabled, Name: "api-key-row-2",
			Models: "test-model", Group: "default", Tag: &tag,
			Priority: &priority, Weight: &weight,
		},
	}
	for index := range channels {
		require.NoError(t, db.Create(&channels[index]).Error)
		require.NoError(t, db.Create(&model.Ability{
			Group: "default", Model: "test-model", ChannelId: channels[index].Id,
			Enabled: true, Priority: &priority, Weight: weight, Tag: &tag,
		}).Error)
	}

	require.True(t, updateOpenCodeGoChannelStatus(
		channels[0].Id,
		constant.ChannelTypeOpenCodeAPIKey,
		channels[0].Key,
		common.ChannelStatusAutoDisabled,
		"upstream rejected this channel credential",
	))

	var storedChannels []model.Channel
	require.NoError(t, db.Order("id ASC").Find(&storedChannels).Error)
	require.Len(t, storedChannels, 2)
	assert.Equal(t, common.ChannelStatusAutoDisabled, storedChannels[0].Status)
	assert.Equal(t, common.ChannelStatusEnabled, storedChannels[1].Status)

	var abilities []model.Ability
	require.NoError(t, db.Order("channel_id ASC").Find(&abilities).Error)
	require.Len(t, abilities, 2)
	assert.False(t, abilities[0].Enabled)
	assert.True(t, abilities[1].Enabled)
}
