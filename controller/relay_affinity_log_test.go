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
		http.StatusUnauthorized,
	))
	service.ResetStatusCode(internalErr, `{"401":429}`)
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
	require.Equal(t, float64(http.StatusUnauthorized), adminInfo["upstream_status_code"])
	require.Equal(t, float64(http.StatusTooManyRequests), other["status_code"])
}

func TestProcessChannelErrorRedactsOpenCodeAPIKeyAdminDiagnostic(t *testing.T) {
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousErrorLogEnabled := constant.ErrorLogEnabled
	previousRedisEnabled := common.RedisEnabled
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf(
		"file:relay-api-key-log-%d?mode=memory&cache=shared",
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
		Id:       4201,
		Username: "api-key-log-user",
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
	c.Set("id", 4201)
	c.Set("username", "api-key-log-user")
	c.Set("token_name", "client-token")
	c.Set("original_model", "glm-5.2")
	c.Set("token_id", 5201)
	c.Set("group", "opencode-keys")
	c.Set("channel_id", 6301)
	c.Set("channel_name", "OpenCode API Key")
	c.Set("channel_type", constant.ChannelTypeOpenCodeAPIKey)
	common.SetContextKey(c, constant.ContextKeyRequestStartTime, time.Now())

	internalErr := relaytypes.WithOpenAIError(relaytypes.OpenAIError{
		Message: "invalid request",
		Type:    "invalid_request_error",
		Code:    "invalid_request_error",
	}, http.StatusBadGateway)
	internalErr.SetMessage(
		`{"error":{"message":"Authorization: Bearer private-bearer; x-api-key=private-key; Cookie: session=private-cookie; session_id=private-session; proxy=socks5://proxy-user:proxy-password@10.0.0.8:1080; endpoint=http://internal-control.local/v1/private"}}`,
	)
	service.MarkOpenCodeGoUpstreamRelayErrorWithStatus(internalErr, http.StatusOK)
	processChannelError(
		c,
		*relaytypes.NewChannelError(
			6301,
			constant.ChannelTypeOpenCodeAPIKey,
			"OpenCode API Key",
			false,
			"",
			false,
		),
		internalErr,
	)

	var stored model.Log
	require.NoError(t, db.Where("type = ?", model.LogTypeError).First(&stored).Error)
	for _, secret := range []string{
		"private-bearer",
		"private-key",
		"private-cookie",
		"private-session",
		"proxy-user",
		"proxy-password",
		"10.0.0.8",
		"internal-control.local",
		"/v1/private",
	} {
		require.NotContains(t, stored.Content, secret)
	}
	require.Contains(t, stored.Content, "[redacted]")
	other, err := common.StrToMap(stored.Other)
	require.NoError(t, err)
	adminInfo, ok := other["admin_info"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, float64(http.StatusOK), adminInfo["upstream_status_code"])
}
