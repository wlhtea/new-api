package model

import (
	"bytes"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestRecordConsumeLogKeepsAdminDataInDBButNotProcessLog(t *testing.T) {
	previousDB, previousLogDB := DB, LOG_DB
	previousWriter := gin.DefaultWriter
	previousConsumeEnabled := common.LogConsumeEnabled
	db, err := gorm.Open(sqlite.Open("file:consume_log_privacy?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&User{}, &Log{}))
	DB, LOG_DB = db, db
	common.LogConsumeEnabled = true
	user := User{Username: "consume-log-user", Status: common.UserStatusEnabled, Group: "default"}
	require.NoError(t, db.Create(&user).Error)

	var output bytes.Buffer
	common.LogWriterMu.Lock()
	gin.DefaultWriter = &output
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultWriter = previousWriter
		common.LogWriterMu.Unlock()
		DB, LOG_DB = previousDB, previousLogDB
		common.LogConsumeEnabled = previousConsumeEnabled
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	c, _ := gin.CreateTestContext(nil)
	c.Set("username", user.Username)
	const workspaceUID = "workspace-private-value"
	const affinityKey = "session-private-value"
	const content = "request-body-private-value"
	RecordConsumeLog(c, user.Id, RecordConsumeLogParams{
		ChannelId: 72,
		ModelName: "public-model",
		Content:   content,
		Other: map[string]interface{}{
			"admin_info": map[string]interface{}{
				"opencode_go_workspace_uid": workspaceUID,
				"opencode_go_affinity_key":  affinityKey,
			},
		},
	})

	processLog := output.String()
	require.Contains(t, processLog, "record consume log")
	require.NotContains(t, processLog, workspaceUID)
	require.NotContains(t, processLog, affinityKey)
	require.NotContains(t, processLog, content)

	var stored Log
	require.NoError(t, db.Where("user_id = ? AND type = ?", user.Id, LogTypeConsume).First(&stored).Error)
	require.Contains(t, stored.Other, workspaceUID)
	require.Contains(t, stored.Other, affinityKey)
	require.Equal(t, content, stored.Content)
}
