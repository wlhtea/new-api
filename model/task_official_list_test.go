package model

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestListTasksForOfficialPluginBoundsLargeLegacyDatasetInSQL(t *testing.T) {
	truncateTables(t)
	now := time.Now().Unix()
	tasks := make([]*Task, 0, 1200)
	for index := 0; index < 1200; index++ {
		officialStatus := "failed"
		hostStatus := TaskStatus(TaskStatusFailure)
		if index%2 == 0 {
			officialStatus = "succeeded"
			hostStatus = TaskStatusSuccess
		}
		modelName := "model-beta"
		if index%3 == 0 {
			modelName = "model-alpha"
		}
		serviceTier := "default"
		if index%4 == 0 {
			serviceTier = "flex"
		}
		tasks = append(tasks, &Task{
			TaskID:         fmt.Sprintf("task-large-%04d", index),
			ProviderTaskID: fmt.Sprintf("cgt-large-%04d", index),
			UserId:         7,
			Platform:       constant.TaskPlatform("doubao_ark_official"),
			Status:         hostStatus,
			CreatedAt:      now - int64(index),
			UpdatedAt:      now - int64(index),
			Data: []byte(fmt.Sprintf(
				`{"status":%q,"model":%q,"service_tier":%q,"payload":%q}`,
				officialStatus, modelName, serviceTier, strings.Repeat("x", 256),
			)),
		})
	}
	require.NoError(t, DB.CreateInBatches(tasks, 100).Error)
	// Simulate rows written before query projections existed. The old schema
	// rejects this statement; the migrated schema clears the fields so the
	// ordinary migration entry point must backfill them from Data.
	_ = DB.Exec("UPDATE tasks SET official_status = '', official_model = '', official_service_tier = ''").Error
	require.NoError(t, migrateDB())
	assert.True(t, DB.Migrator().HasIndex(&Task{}, "idx_tasks_official_list"))

	const callbackName = "test:official_list_row_bound"
	var maxSelectedRows int64
	var dataQuerySQL string
	require.NoError(t, DB.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table != "tasks" {
			return
		}
		sql := tx.Statement.SQL.String()
		if strings.Contains(sql, "SELECT *") {
			dataQuerySQL = sql
			if tx.RowsAffected > maxSelectedRows {
				maxSelectedRows = tx.RowsAffected
			}
		}
	}))
	t.Cleanup(func() { _ = DB.Callback().Query().Remove(callbackName) })

	options := OfficialTaskListOptions{
		PageNum: 2, PageSize: 25,
		Status: "succeeded", Model: "model-alpha", ServiceTier: "flex",
		Since: now - 7*24*60*60,
	}
	page, total, err := ListTasksForOfficialPlugin(7, constant.TaskPlatform("doubao_ark_official"), options)
	require.NoError(t, err)
	assert.Equal(t, int64(100), total, "every 12th row matches all three exact filters")
	assert.Len(t, page, 25)

	options.PageNum = 4
	lastPage, total, err := ListTasksForOfficialPlugin(7, constant.TaskPlatform("doubao_ark_official"), options)
	require.NoError(t, err)
	assert.Equal(t, int64(100), total)
	assert.Len(t, lastPage, 25)

	options.PageNum = 5
	beyondLast, total, err := ListTasksForOfficialPlugin(7, constant.TaskPlatform("doubao_ark_official"), options)
	require.NoError(t, err)
	assert.Equal(t, int64(100), total)
	assert.Empty(t, beyondLast)

	assert.LessOrEqual(t, maxSelectedRows, int64(25), "each page read must be bounded by page_size, never the seven-day table scan")
	assert.Contains(t, dataQuerySQL, "LIMIT")
	assert.Contains(t, dataQuerySQL, "official_status")
	assert.Contains(t, dataQuerySQL, "official_model")
	assert.Contains(t, dataQuerySQL, "official_service_tier")
}
