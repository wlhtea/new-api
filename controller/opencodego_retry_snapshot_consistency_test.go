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

func TestFreezeOpenCodeAPIKeyRetrySnapshotSkipsIncompatibleInitialCandidate(t *testing.T) {
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

	relayErr := freezeOpenCodeAPIKeyRetrySnapshot(c, info)
	require.Nil(t, relayErr)
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

func TestFreezeOpenCodeAPIKeyRetrySnapshotRejectsStaleInitialSelection(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	const modelName = "retry-snapshot-stale-initial-model"

	available := newOpenCodeAPIKeySnapshotTestChannel("snapshot-available", modelName, 10, dto.OpenCodeGoProtocolChat)
	persistOpenCodeAPIKeySnapshotTestChannel(t, db, &available)
	stale := newOpenCodeAPIKeySnapshotTestChannel("snapshot-stale", modelName, 20, dto.OpenCodeGoProtocolChat)
	stale.Id = available.Id + 1000

	c, info := newOpenCodeAPIKeySnapshotTestFixture(t, &stale, modelName)
	relayErr := freezeOpenCodeAPIKeyRetrySnapshot(c, info)
	require.NotNil(t, relayErr)
	assert.Equal(t, types.ErrorOriginGatewayInvariant, relayErr.Provenance().Origin)
	assert.Equal(t, opencodego.PreflightPlanMismatchRule, relayErr.Provenance().Subtype)
	_, found, snapshotErr := getOpenCodeAPIKeyRetrySnapshot(c)
	require.NoError(t, snapshotErr)
	assert.False(t, found)
}

func TestFreezeOpenCodeAPIKeyRetrySnapshotReplacesStaleInitialConfigFromDatabase(t *testing.T) {
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
	require.Nil(t, freezeOpenCodeAPIKeyRetrySnapshot(c, info))
	assert.Equal(t, "snapshot-database-key", common.GetContextKeyString(c, constant.ContextKeyChannelKey))
	assert.Equal(t, "database", common.GetContextKeyStringMap(c, constant.ContextKeyChannelHeaderOverride)["X-Snapshot-Config"])

	plan, found, err := opencodego.GetRequestPreflightPlanForSelection(c, "default", persisted.Id)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, opencodego.ProtocolChat, plan.FinalProtocol)

	snapshot, found, err := getOpenCodeAPIKeyRetrySnapshot(c)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, snapshot.topology, 1)
	assert.Equal(t, "snapshot-database-key", snapshot.topology[0].channelKey)
	assert.Equal(t, "database", snapshot.topology[0].headerOverride["X-Snapshot-Config"])
}

func TestFreezeOpenCodeAPIKeyRetrySnapshotRejectsInvalidCandidateConfig(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	const modelName = "retry-snapshot-invalid-candidate-model"

	initial := newOpenCodeAPIKeySnapshotTestChannel("snapshot-valid", modelName, 20, dto.OpenCodeGoProtocolChat)
	persistOpenCodeAPIKeySnapshotTestChannel(t, db, &initial)
	invalid := newOpenCodeAPIKeySnapshotTestChannel("snapshot-invalid", modelName, 10, dto.OpenCodeGoProtocolChat)
	invalidSetting := `{`
	invalid.Setting = &invalidSetting
	persistOpenCodeAPIKeySnapshotTestChannel(t, db, &invalid)

	c, info := newOpenCodeAPIKeySnapshotTestFixture(t, &initial, modelName)
	relayErr := freezeOpenCodeAPIKeyRetrySnapshot(c, info)
	require.NotNil(t, relayErr)
	assert.Equal(t, types.ErrorOriginGatewayConfig, relayErr.Provenance().Origin)
	assert.Equal(t, opencodego.PreflightCandidateConfigInvalidRule, relayErr.Provenance().Subtype)
	_, found, snapshotErr := getOpenCodeAPIKeyRetrySnapshot(c)
	require.NoError(t, snapshotErr)
	assert.False(t, found)
}
