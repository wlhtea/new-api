package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/opencodego"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newOpenCodeAPIKeySnapshotTestChannel(
	name string,
	modelName string,
	priority int64,
	protocol string,
) model.Channel {
	weight := uint(1)
	autoBan := 0
	channel := model.Channel{
		Type:     constant.ChannelTypeOpenCodeAPIKey,
		Key:      name + "-key",
		Status:   common.ChannelStatusEnabled,
		Name:     name,
		Models:   modelName,
		Group:    "default",
		Priority: &priority,
		Weight:   &weight,
		AutoBan:  &autoBan,
	}
	channel.SetOtherSettings(dto.ChannelOtherSettings{OpenCodeGo: &dto.OpenCodeGoConfig{
		ModelProtocols: map[string]string{modelName: protocol},
	}})
	return channel
}

func persistOpenCodeAPIKeySnapshotTestChannel(
	t *testing.T,
	db *gorm.DB,
	channel *model.Channel,
) {
	t.Helper()
	require.NoError(t, db.Create(channel).Error)
	require.NoError(t, db.Create(&model.Ability{
		Group:     "default",
		Model:     channel.Models,
		ChannelId: channel.Id,
		Enabled:   true,
		Priority:  channel.Priority,
		Weight:    uint(channel.GetWeight()),
	}).Error)
}

func newOpenCodeAPIKeySnapshotTestFixture(
	t *testing.T,
	selected *model.Channel,
	modelName string,
) (*gin.Context, *relaycommon.RelayInfo) {
	return newOpenCodeAPIKeySnapshotTestFixtureWithExtra(t, selected, modelName, "")
}

func newOpenCodeAPIKeySnapshotTestFixtureWithExtra(
	t *testing.T,
	selected *model.Channel,
	modelName string,
	extraFields string,
) (*gin.Context, *relaycommon.RelayInfo) {
	t.Helper()
	body := []byte(`{"model":"` + modelName + `","messages":[{"role":"user","content":"hello"}],"stream":false` + extraFields + `}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	storage, err := common.CreateBodyStorage(body)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })
	c.Set(common.KeyBodyStorage, storage)
	common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	require.Nil(t, middleware.SetupContextForSelectedChannel(c, selected, modelName))

	request, err := helper.GetAndValidateRequest(c, types.RelayFormatOpenAI)
	require.NoError(t, err)
	info, err := relaycommon.GenRelayInfo(c, types.RelayFormatOpenAI, request, nil)
	require.NoError(t, err)
	return c, info
}

func TestPrepareOpenCodeCandidatePlansSkipsIncompatibleInitialCandidate(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	const modelName = "retry-snapshot-compatible-fallback-model"

	incompatible := newOpenCodeAPIKeySnapshotTestChannel(
		"snapshot-incompatible",
		modelName,
		20,
		dto.OpenCodeGoProtocolResponses,
	)
	persistOpenCodeAPIKeySnapshotTestChannel(t, db, &incompatible)
	compatible := newOpenCodeAPIKeySnapshotTestChannel(
		"snapshot-compatible",
		modelName,
		10,
		dto.OpenCodeGoProtocolChat,
	)
	persistOpenCodeAPIKeySnapshotTestChannel(t, db, &compatible)

	c, info := newOpenCodeAPIKeySnapshotTestFixtureWithExtra(
		t,
		&incompatible,
		modelName,
		`,"thinking":{"type":"enabled"}`,
	)

	plans, relayErr := prepareAndFreezeOpenCodeCandidatePlans(c, info)
	require.Nil(t, relayErr)
	require.NotNil(t, plans)
	require.Len(t, plans.plans, 1)
	assert.Equal(t, compatible.Id, plans.plans[0].key.ChannelID)
	assert.Equal(t, compatible.Id, common.GetContextKeyInt(c, constant.ContextKeyChannelId))

	_, incompatibleFound, err := opencodego.GetRequestPreflightPlanForSelection(c, "default", incompatible.Id)
	require.NoError(t, err)
	assert.False(t, incompatibleFound)
	compatiblePlan, compatibleFound, err := opencodego.GetRequestPreflightPlanForSelection(c, "default", compatible.Id)
	require.NoError(t, err)
	require.True(t, compatibleFound)
	assert.Equal(t, opencodego.ProtocolChat, compatiblePlan.FinalProtocol)

	snapshot, found, err := getOpenCodeAPIKeyRetrySnapshot(c)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, snapshot.topology, 1)
	require.NotEmpty(t, snapshot.selections)
	assert.Equal(t, compatible.Id, snapshot.selections[0].channelID)
}

func TestPrepareOpenCodeCandidatePlansRejectsStaleInitialSelection(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	const modelName = "retry-snapshot-stale-initial-model"

	available := newOpenCodeAPIKeySnapshotTestChannel("snapshot-available", modelName, 10, dto.OpenCodeGoProtocolChat)
	persistOpenCodeAPIKeySnapshotTestChannel(t, db, &available)
	stale := newOpenCodeAPIKeySnapshotTestChannel("snapshot-stale", modelName, 20, dto.OpenCodeGoProtocolChat)
	stale.Id = available.Id + 1000

	c, info := newOpenCodeAPIKeySnapshotTestFixture(t, &stale, modelName)
	plans, relayErr := prepareAndFreezeOpenCodeCandidatePlans(c, info)
	require.NotNil(t, relayErr)
	assert.Nil(t, plans)
	assert.Equal(t, types.ErrorOriginGatewayInvariant, relayErr.Provenance().Origin)
	assert.Equal(t, opencodego.PreflightPlanMismatchRule, relayErr.Provenance().Subtype)
	_, found, snapshotErr := getOpenCodeAPIKeyRetrySnapshot(c)
	require.NoError(t, snapshotErr)
	assert.False(t, found)
}

func TestPrepareOpenCodeCandidatePlansReplacesStaleInitialConfigFromDatabase(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	const modelName = "retry-snapshot-refresh-initial-model"

	persisted := newOpenCodeAPIKeySnapshotTestChannel("snapshot-database", modelName, 20, dto.OpenCodeGoProtocolChat)
	persisted.Key = "snapshot-database-key"
	databaseHeader := `{"X-Snapshot-Config":"database"}`
	persisted.HeaderOverride = &databaseHeader
	persistOpenCodeAPIKeySnapshotTestChannel(t, db, &persisted)

	stale := persisted
	stale.Key = "snapshot-stale-key"
	staleHeader := `{"X-Snapshot-Config":"stale"}`
	stale.HeaderOverride = &staleHeader
	stale.SetOtherSettings(dto.ChannelOtherSettings{OpenCodeGo: &dto.OpenCodeGoConfig{
		ModelProtocols: map[string]string{modelName: dto.OpenCodeGoProtocolResponses},
	}})

	c, info := newOpenCodeAPIKeySnapshotTestFixture(t, &stale, modelName)
	plans, relayErr := prepareAndFreezeOpenCodeCandidatePlans(c, info)
	require.Nil(t, relayErr)
	require.NotNil(t, plans)
	require.Len(t, plans.plans, 1)
	assert.Equal(t, persisted.Id, plans.plans[0].key.ChannelID)
	assert.Empty(t, common.GetContextKeyString(c, constant.ContextKeyChannelKey))
	assert.Equal(t, "database", common.GetContextKeyStringMap(c, constant.ContextKeyChannelHeaderOverride)["X-Snapshot-Config"])

	plan, found, err := opencodego.GetRequestPreflightPlanForSelection(c, "default", persisted.Id)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, opencodego.ProtocolChat, plan.FinalProtocol)

	snapshot, found, err := getOpenCodeAPIKeyRetrySnapshot(c)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, snapshot.topology, 1)
	assert.Empty(t, snapshot.topology[0].channelKey)
	assert.Equal(t, "database", snapshot.topology[0].headerOverride["X-Snapshot-Config"])
	selected, err := snapshot.selectAttempt(c, 0)
	require.NoError(t, err)
	require.NotNil(t, selected)
	assert.Equal(t, "snapshot-database-key", common.GetContextKeyString(c, constant.ContextKeyChannelKey))
	assert.Equal(t, "snapshot-database-key", snapshot.topology[0].channelKey)
}

func TestPrepareOpenCodeCandidatePlansFiltersInvalidCandidateConfig(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	const modelName = "retry-snapshot-invalid-candidate-model"

	initial := newOpenCodeAPIKeySnapshotTestChannel("snapshot-valid", modelName, 20, dto.OpenCodeGoProtocolChat)
	persistOpenCodeAPIKeySnapshotTestChannel(t, db, &initial)
	invalid := newOpenCodeAPIKeySnapshotTestChannel("snapshot-invalid", modelName, 10, dto.OpenCodeGoProtocolChat)
	invalidSetting := `{`
	invalid.Setting = &invalidSetting
	persistOpenCodeAPIKeySnapshotTestChannel(t, db, &invalid)

	c, info := newOpenCodeAPIKeySnapshotTestFixture(t, &initial, modelName)
	plans, relayErr := prepareAndFreezeOpenCodeCandidatePlans(c, info)
	require.Nil(t, relayErr)
	require.NotNil(t, plans)
	require.Len(t, plans.plans, 1)
	assert.Equal(t, initial.Id, plans.plans[0].key.ChannelID)

	_, invalidFound, err := opencodego.GetRequestPreflightPlanForSelection(c, "default", invalid.Id)
	require.NoError(t, err)
	assert.False(t, invalidFound)
	_, initialFound, err := opencodego.GetRequestPreflightPlanForSelection(c, "default", initial.Id)
	require.NoError(t, err)
	assert.True(t, initialFound)

	snapshot, found, snapshotErr := getOpenCodeAPIKeyRetrySnapshot(c)
	require.NoError(t, snapshotErr)
	require.True(t, found)
	require.Len(t, snapshot.topology, 1)
	assert.Equal(t, initial.Id, snapshot.topology[0].channelID)
	for _, selection := range snapshot.selections {
		assert.Equal(t, initial.Id, selection.channelID)
	}
}

func TestOpenCodeAPIKeyRetrySnapshotMaterializesPollingKeyOncePerCandidate(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	previousMemoryCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = previousMemoryCache })

	channel := newOpenCodeAPIKeySnapshotTestChannel(
		"snapshot-key-materialization",
		"snapshot-key-materialization-model",
		10,
		dto.OpenCodeGoProtocolChat,
	)
	channel.Key = "materialized-key-a\nmaterialized-key-b"
	channel.ChannelInfo = model.ChannelInfo{
		IsMultiKey:           true,
		MultiKeySize:         2,
		MultiKeyMode:         constant.MultiKeyModePolling,
		MultiKeyPollingIndex: 0,
		MultiKeyStatusList: map[int]int{
			0: common.ChannelStatusEnabled,
			1: common.ChannelStatusEnabled,
		},
	}
	require.NoError(t, db.Create(&channel).Error)
	key := opencodego.RequestPreflightPlanKey{SelectionGroup: "default", ChannelID: channel.Id}
	selection := frozenOpenCodeAPIKeySelection{
		selectionGroup: "default",
		channelID:      channel.Id,
		channelName:    channel.Name,
	}
	snapshot := &openCodeAPIKeyRetrySnapshot{
		version:      openCodeAPIKeyRetrySnapshotVersion,
		topology:     []frozenOpenCodeAPIKeySelection{selection},
		selections:   []frozenOpenCodeAPIKeySelection{selection, selection},
		keySources:   map[opencodego.RequestPreflightPlanKey]*model.Channel{key: &channel},
		materialized: make(map[opencodego.RequestPreflightPlanKey]frozenOpenCodeAPIKeyCredential),
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	first, err := snapshot.selectAttempt(c, 0)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, "materialized-key-a", common.GetContextKeyString(c, constant.ContextKeyChannelKey))
	assert.True(t, common.GetContextKeyBool(c, constant.ContextKeyChannelIsMultiKey))
	assert.Zero(t, common.GetContextKeyInt(c, constant.ContextKeyChannelMultiKeyIndex))

	second, err := snapshot.selectAttempt(c, 1)
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.Equal(t, "materialized-key-a", common.GetContextKeyString(c, constant.ContextKeyChannelKey))
	var persisted model.Channel
	require.NoError(t, db.First(&persisted, channel.Id).Error)
	assert.Equal(t, 1, persisted.ChannelInfo.MultiKeyPollingIndex)
}
