package model

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
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

func TestBackfillOfficialTaskListProjectionsPreservesAuditRow(t *testing.T) {
	truncateTables(t)
	const (
		createdAt = int64(1_700_000_000)
		updatedAt = int64(1_700_000_001)
	)
	task := &Task{
		TaskID: "task-backfill-audit", ProviderTaskID: "cgt-backfill-audit",
		UserId: 7, Platform: constant.TaskPlatform("doubao_ark_official"),
		ChannelId: 19, Group: "audit-group", Quota: 1234, Action: "video",
		Status: TaskStatusSuccess, FailReason: "historical-detail",
		CreatedAt: createdAt, UpdatedAt: updatedAt,
		SubmitTime: createdAt + 2, StartTime: createdAt + 3, FinishTime: createdAt + 4,
		Progress: "100%", ProviderDeleted: true,
		Properties:  Properties{Input: "original-input", UpstreamModelName: "fallback-model", OriginModelName: "origin-model"},
		PrivateData: TaskPrivateData{UpstreamTaskID: "cgt-backfill-audit", ResultURL: "https://cdn.example/original.mp4", PollFailures: 2},
		Data:        []byte(`{"status":"succeeded","model":"Exact-Model","service_tier":"flex","result":"original-result"}`),
	}
	require.NoError(t, DB.Create(task).Error)
	// Simulate the pre-projection schema without invoking any model hooks.
	require.NoError(t, DB.Exec("UPDATE tasks SET official_status = '', official_model = '', official_service_tier = '' WHERE id = ?", task.ID).Error)
	var before Task
	require.NoError(t, DB.First(&before, task.ID).Error)
	require.Equal(t, createdAt, before.CreatedAt)
	require.Equal(t, updatedAt, before.UpdatedAt, "the fixture must use a nonzero historical audit timestamp")

	require.NoError(t, backfillOfficialTaskListProjections())

	var after Task
	require.NoError(t, DB.First(&after, task.ID).Error)
	expected := before
	expected.OfficialStatus = "succeeded"
	expected.OfficialModel = "Exact-Model"
	expected.OfficialServiceTier = "flex"
	assert.Equal(t, expected, after, "backfill must change only its three projection columns")
	assert.Equal(t, updatedAt, after.UpdatedAt, "backfill must preserve the original audit timestamp exactly")
}

func TestListTasksForOfficialPluginMatchesModelCaseExactlyOnSQLite(t *testing.T) {
	truncateTables(t)
	now := time.Now().Unix()
	for index, modelName := range []string{"Doubao-Seedance", "doubao-seedance"} {
		task := &Task{
			TaskID:         fmt.Sprintf("task-model-case-%d", index),
			ProviderTaskID: fmt.Sprintf("cgt-model-case-%d", index),
			UserId:         7, Platform: constant.TaskPlatform("doubao_ark_official"),
			Status: TaskStatusQueued, CreatedAt: now, UpdatedAt: now,
			Data: []byte(fmt.Sprintf(`{"status":"queued","model":%q}`, modelName)),
		}
		require.NoError(t, DB.Create(task).Error)
	}

	page, total, err := ListTasksForOfficialPlugin(7, constant.TaskPlatform("doubao_ark_official"), OfficialTaskListOptions{
		PageNum: 1, PageSize: 10, Since: now - 1, Model: "Doubao-Seedance",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	require.Len(t, page, 1)
	assert.Equal(t, "Doubao-Seedance", page[0].OfficialModel)
}

func TestListTasksForOfficialPluginGeneratesCaseSensitiveDialectSQL(t *testing.T) {
	for _, testCase := range []struct {
		name               string
		dialector          gorm.Dialector
		wantModelPredicate string
	}{
		{
			name: "mysql",
			dialector: mysql.New(mysql.Config{
				DSN: "user:pass@tcp(127.0.0.1:1)/newapi?parseTime=true", SkipInitializeWithVersion: true,
			}),
			wantModelPredicate: "official_model = BINARY ?",
		},
		{
			name: "postgres",
			dialector: postgres.New(postgres.Config{
				DSN: "host=127.0.0.1 port=1 user=user dbname=newapi sslmode=disable", PreferSimpleProtocol: true,
			}),
			wantModelPredicate: `official_model = $`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			originalDB := DB
			defer func() { DB = originalDB }()
			var err error
			DB, err = gorm.Open(testCase.dialector, &gorm.Config{DryRun: true, DisableAutomaticPing: true})
			require.NoError(t, err)

			callbackName := "test:official_list_sql_" + testCase.name
			var countSQL string
			require.NoError(t, DB.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
				if tx.Statement.Table == "tasks" {
					countSQL = tx.Statement.SQL.String()
				}
			}))

			_, _, err = ListTasksForOfficialPlugin(7, constant.TaskPlatform("doubao_ark_official"), OfficialTaskListOptions{
				PageNum: 1, PageSize: 10, Model: "Doubao-Seedance",
			})
			require.NoError(t, err)
			require.NotEmpty(t, countSQL)
			assert.Contains(t, countSQL, testCase.wantModelPredicate)
			assert.NotContains(t, countSQL, "LOWER(official_model)")
		})
	}
}
