package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestProcessChannelErrorPersistsOpenCodeGoAffinity(t *testing.T) {
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousErrorLogEnabled := constant.ErrorLogEnabled
	previousRedisEnabled := common.RedisEnabled
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf(
		"file:relay-affinity-log-%d?mode=memory&cache=shared",
		time.Now().UnixNano(),
	)), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	model.DB, model.LOG_DB = db, db
	constant.ErrorLogEnabled = true
	common.RedisEnabled = false
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Log{}))
	require.NoError(t, db.Create(&model.User{
		Id:       4101,
		Username: "affinity-log-user",
		Setting:  "{}",
	}).Error)
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		constant.ErrorLogEnabled = previousErrorLogEnabled
		common.RedisEnabled = previousRedisEnabled
		_ = sqlDB.Close()
	})

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set("id", 4101)
	c.Set("username", "affinity-log-user")
	c.Set("token_name", "client-token")
	c.Set("original_model", "glm-5.2")
	c.Set("token_id", 5101)
	c.Set("group", "default")
	c.Set("channel_id", 6201)
	c.Set("channel_name", "OpenCode Go pool")
	c.Set("channel_type", constant.ChannelTypeOpenCodeGo)
	common.SetContextKey(c, constant.ContextKeyRequestStartTime, time.Now())
	common.SetContextKey(c, constant.ContextKeyOpenCodeGoWorkspaceUID, "workspace_error_target")
	common.SetContextKey(c, constant.ContextKeyOpenCodeGoAffinitySource, "opencode-session")
	common.SetContextKey(c, constant.ContextKeyOpenCodeGoAffinityKey, "private-affinity-key")

	internalErr := service.MarkOpenCodeGoUpstreamRelayError(relaytypes.NewOpenAIError(
		fmt.Errorf("internal shard zen-primary failed"),
		relaytypes.ErrorCodeBadResponseStatusCode,
		http.StatusBadGateway,
	))
	processChannelError(
		c,
		*relaytypes.NewChannelError(
			6201,
			constant.ChannelTypeOpenCodeGo,
			"OpenCode Go pool",
			false,
			"",
			false,
		),
		internalErr,
	)

	var stored model.Log
	require.NoError(t, db.Where("type = ?", model.LogTypeError).First(&stored).Error)
	require.Contains(t, stored.Content, "internal shard zen-primary failed")
	require.Equal(t, "internal shard zen-primary failed", internalErr.Error())
	require.True(t, service.IsOpenCodeGoUpstreamRelayError(internalErr))
	other, err := common.StrToMap(stored.Other)
	require.NoError(t, err)
	adminInfo, ok := other["admin_info"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, "workspace_error_target", adminInfo["opencode_go_workspace_uid"])
	require.Equal(t, "opencode-session", adminInfo["opencode_go_affinity_source"])
	require.Equal(t, "private-affinity-key", adminInfo["opencode_go_affinity_key"])
}
