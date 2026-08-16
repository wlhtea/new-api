package model

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestOpenCodeGoCapabilitySnapshotMigrationIsDedicatedAndIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Option{}, &OpenCodeGoCapabilitySnapshot{}))
	require.NoError(t, db.AutoMigrate(&Option{}, &OpenCodeGoCapabilitySnapshot{}))
	assert.True(t, db.Migrator().HasTable(&OpenCodeGoCapabilitySnapshot{}))

	snapshot := OpenCodeGoCapabilitySnapshot{
		Provider:          OpenCodeGoCapabilityProvider,
		Generation:        7,
		SchemaVersion:     1,
		SemanticRevision:  strings.Repeat("a", 64),
		SourceETag:        `"fixture"`,
		CheckedAt:         100,
		NormalizedPayload: `{"schema_version":1}`,
		UpdatedAt:         101,
	}
	require.NoError(t, db.Create(&snapshot).Error)

	var optionCount int64
	require.NoError(t, db.Model(&Option{}).Count(&optionCount).Error)
	assert.Zero(t, optionCount, "capability state must never enter the generic Option table")

	var reloaded OpenCodeGoCapabilitySnapshot
	require.NoError(t, db.First(&reloaded, "provider = ?", OpenCodeGoCapabilityProvider).Error)
	assert.Equal(t, snapshot.SourceETag, reloaded.SourceETag)
}

func TestOpenCodeGoCapabilitySnapshotMigrationConfiguredDatabases(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		dialector func(string) gorm.Dialector
	}{
		{name: "mysql", env: "TEST_MYSQL_DSN", dialector: mysql.Open},
		{
			name: "postgres",
			env:  "TEST_POSTGRES_DSN",
			dialector: func(dsn string) gorm.Dialector {
				return postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(test.env))
			if dsn == "" {
				t.Skip(test.env + " is not configured")
			}
			db, err := gorm.Open(test.dialector(dsn), &gorm.Config{})
			require.NoError(t, err)
			require.NoError(t, db.AutoMigrate(&OpenCodeGoCapabilitySnapshot{}))
			require.NoError(t, db.AutoMigrate(&OpenCodeGoCapabilitySnapshot{}))
			assert.True(t, db.Migrator().HasTable(&OpenCodeGoCapabilitySnapshot{}))
		})
	}
}

func TestPersistOpenCodeGoCapabilitySnapshotForTaskRequiresLiveLeaseAndFencesGeneration(t *testing.T) {
	truncateTables(t)
	require.NoError(t, DB.AutoMigrate(&OpenCodeGoCapabilitySnapshot{}))
	require.NoError(t, DB.Where("provider = ?", OpenCodeGoCapabilityProvider).Delete(&OpenCodeGoCapabilitySnapshot{}).Error)
	t.Cleanup(func() {
		_ = DB.Where("provider = ?", OpenCodeGoCapabilityProvider).Delete(&OpenCodeGoCapabilitySnapshot{}).Error
	})

	task, err := CreateSystemTask(SystemTaskTypeOpenCodeGoCapabilityRefresh, struct{}{}, nil)
	require.NoError(t, err)
	runnerID := "capability-runner"
	claimed, ok, err := ClaimSystemTask(
		task.ID,
		task.Type,
		runnerID,
		common.GetTimestamp()+60,
	)
	require.NoError(t, err)
	require.True(t, ok)

	snapshot := &OpenCodeGoCapabilitySnapshot{
		Provider:          OpenCodeGoCapabilityProvider,
		SchemaVersion:     1,
		SemanticRevision:  strings.Repeat("b", 64),
		SourceETag:        `"fixture"`,
		CheckedAt:         common.GetTimestamp(),
		NormalizedPayload: `{"schema_version":1}`,
	}
	require.NoError(t, PersistOpenCodeGoCapabilitySnapshotForTask(claimed, runnerID, snapshot))
	assert.Equal(t, claimed.ID, snapshot.Generation)
	assert.NotZero(t, snapshot.UpdatedAt)

	reloaded, err := GetOpenCodeGoCapabilitySnapshot()
	require.NoError(t, err)
	require.NotNil(t, reloaded)
	assert.Equal(t, claimed.ID, reloaded.Generation)
	assert.Equal(t, snapshot.SemanticRevision, reloaded.SemanticRevision)

	err = PersistOpenCodeGoCapabilitySnapshotForTask(claimed, runnerID, snapshot)
	assert.ErrorIs(t, err, ErrOpenCodeGoCapabilityStaleGeneration)

	require.NoError(t, DB.Model(&SystemTaskLock{}).
		Where("task_id = ?", claimed.TaskID).
		Update("locked_until", common.GetTimestamp()-1).Error)
	snapshot.SemanticRevision = strings.Repeat("c", 64)
	err = PersistOpenCodeGoCapabilitySnapshotForTask(claimed, runnerID, snapshot)
	assert.ErrorIs(t, err, ErrSystemTaskLockLost)

	reloaded, err = GetOpenCodeGoCapabilitySnapshot()
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("b", 64), reloaded.SemanticRevision)
}

func TestPersistOpenCodeGoCapabilitySnapshotRejectsWrongTaskType(t *testing.T) {
	snapshot := &OpenCodeGoCapabilitySnapshot{
		Provider:          OpenCodeGoCapabilityProvider,
		SchemaVersion:     1,
		SemanticRevision:  strings.Repeat("a", 64),
		CheckedAt:         common.GetTimestamp(),
		NormalizedPayload: `{}`,
	}
	err := PersistOpenCodeGoCapabilitySnapshotForTask(
		&SystemTask{ID: 1, TaskID: "wrong", Type: SystemTaskTypeLogCleanup},
		"runner",
		snapshot,
	)
	assert.True(t, errors.Is(err, ErrSystemTaskLockLost))
}

func TestPersistOpenCodeGoCapabilitySnapshotRejectsClockRegressionAndFuture(t *testing.T) {
	truncateTables(t)
	require.NoError(t, DB.AutoMigrate(&OpenCodeGoCapabilitySnapshot{}))
	require.NoError(t, DB.Where("provider = ?", OpenCodeGoCapabilityProvider).Delete(&OpenCodeGoCapabilitySnapshot{}).Error)

	first, err := CreateSystemTask(SystemTaskTypeOpenCodeGoCapabilityRefresh, struct{}{}, nil)
	require.NoError(t, err)
	firstRunner := "capability-clock-runner-1"
	claimedFirst, ok, err := ClaimSystemTask(first.ID, first.Type, firstRunner, common.GetTimestamp()+60)
	require.NoError(t, err)
	require.True(t, ok)
	checkedAt := common.GetTimestamp()
	base := &OpenCodeGoCapabilitySnapshot{
		Provider:          OpenCodeGoCapabilityProvider,
		SchemaVersion:     1,
		SemanticRevision:  strings.Repeat("d", 64),
		SourceETag:        `"clock-fixture"`,
		CheckedAt:         checkedAt,
		NormalizedPayload: `{"schema_version":1}`,
	}
	require.NoError(t, PersistOpenCodeGoCapabilitySnapshotForTask(claimedFirst, firstRunner, base))
	require.NoError(t, FinishSystemTask(
		claimedFirst.TaskID,
		firstRunner,
		SystemTaskStatusSucceeded,
		struct{}{},
		"",
	))

	second, err := CreateSystemTask(SystemTaskTypeOpenCodeGoCapabilityRefresh, struct{}{}, nil)
	require.NoError(t, err)
	secondRunner := "capability-clock-runner-2"
	claimedSecond, ok, err := ClaimSystemTask(second.ID, second.Type, secondRunner, common.GetTimestamp()+60)
	require.NoError(t, err)
	require.True(t, ok)

	regressed := *base
	regressed.CheckedAt = checkedAt - 1
	regressed.SemanticRevision = strings.Repeat("e", 64)
	err = PersistOpenCodeGoCapabilitySnapshotForTask(claimedSecond, secondRunner, &regressed)
	assert.ErrorIs(t, err, ErrOpenCodeGoCapabilityStaleGeneration)

	future := *base
	future.CheckedAt = common.GetTimestamp() + 3600
	future.SemanticRevision = strings.Repeat("f", 64)
	err = PersistOpenCodeGoCapabilitySnapshotForTask(claimedSecond, secondRunner, &future)
	assert.Error(t, err)
	assert.NotErrorIs(t, err, ErrOpenCodeGoCapabilityStaleGeneration)

	reloaded, err := GetOpenCodeGoCapabilitySnapshot()
	require.NoError(t, err)
	require.NotNil(t, reloaded)
	assert.Equal(t, base.SemanticRevision, reloaded.SemanticRevision)
}
