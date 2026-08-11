package relay

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupOpenCodeGoOriginEndpointGuardTest(t *testing.T) *gorm.DB {
	t.Helper()
	previousDB := model.DB
	previousDatabaseType := common.MainDatabaseType()
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:origin-endpoint-guard-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Midjourney{}, &model.Task{}))
	model.DB = db
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		model.DB = previousDB
		common.SetMainDatabaseType(previousDatabaseType)
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})
	return db
}

func seedOpenCodeGoOriginChannel(t *testing.T, db *gorm.DB) *model.Channel {
	t.Helper()
	channel := &model.Channel{
		Type:   constant.ChannelTypeOpenCodeGo,
		Status: common.ChannelStatusEnabled,
		Name:   "origin endpoint guard",
	}
	require.NoError(t, db.Create(channel).Error)
	return channel
}

func TestMidjourneyImageSeedRejectsOpenCodeGoOriginBeforeAuthorization(t *testing.T) {
	db := setupOpenCodeGoOriginEndpointGuardTest(t)
	channel := seedOpenCodeGoOriginChannel(t, db)
	require.NoError(t, db.Create(&model.Midjourney{
		UserId:    4201,
		MjId:      "mj-origin-endpoint-guard",
		ChannelId: channel.Id,
	}).Error)

	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodGet, "/mj/task/mj-origin-endpoint-guard/image-seed", nil)
	c.Params = gin.Params{{Key: "id", Value: "mj-origin-endpoint-guard"}}
	c.Set("id", 4201)

	errResponse := RelayMidjourneyTaskImageSeed(c)

	require.NotNil(t, errResponse)
	assert.Empty(t, c.Request.Header.Get("Authorization"))
	assert.Zero(t, c.GetInt("channel_id"))
}

func TestMidjourneyImageRejectsOpenCodeGoOriginBeforeExternalFetch(t *testing.T) {
	db := setupOpenCodeGoOriginEndpointGuardTest(t)
	channel := seedOpenCodeGoOriginChannel(t, db)
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	require.NoError(t, db.Create(&model.Midjourney{
		UserId:    4203,
		MjId:      "mj-image-origin-endpoint-guard",
		ChannelId: channel.Id,
		ImageUrl:  server.URL + "/image.png",
	}).Error)

	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodGet, "/mj/image/mj-image-origin-endpoint-guard", nil)
	c.Params = gin.Params{{Key: "id", Value: "mj-image-origin-endpoint-guard"}}

	RelayMidjourneyImage(c)

	assert.Equal(t, http.StatusBadRequest, writer.Code)
	assert.JSONEq(t, `{"error":"task_channel_unsupported_endpoint"}`, writer.Body.String())
	assert.Zero(t, requestCount.Load())
}

func TestResolveOriginTaskRejectsOpenCodeGoBeforeLockingChannel(t *testing.T) {
	db := setupOpenCodeGoOriginEndpointGuardTest(t)
	channel := seedOpenCodeGoOriginChannel(t, db)
	require.NoError(t, db.Create(&model.Task{
		TaskID:    "video-origin-endpoint-guard",
		UserId:    4202,
		ChannelId: channel.Id,
		Properties: model.Properties{
			OriginModelName: "video-model",
		},
	}).Error)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos/video-origin-endpoint-guard/remix", nil)
	c.Params = gin.Params{{Key: "video_id", Value: "video-origin-endpoint-guard"}}
	info := &relaycommon.RelayInfo{
		UserId:        4202,
		TaskRelayInfo: &relaycommon.TaskRelayInfo{},
	}

	taskErr := ResolveOriginTask(c, info)

	require.NotNil(t, taskErr)
	assert.Equal(t, "task_channel_unsupported_endpoint", taskErr.Code)
	assert.Nil(t, info.LockedChannel)
	assert.Zero(t, common.GetContextKeyInt(c, constant.ContextKeyChannelId))
}
