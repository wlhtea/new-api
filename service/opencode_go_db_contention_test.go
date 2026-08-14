package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	gosqlite "github.com/glebarez/go-sqlite"
	"github.com/glebarez/sqlite"
	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type openCodeGoContentionTestRow struct {
	ID    int64 `gorm:"primaryKey;autoIncrement"`
	Value int
}

func captureOpenCodeGoSQLiteBusyError(t *testing.T) error {
	t.Helper()
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(0)",
		filepath.ToSlash(filepath.Join(t.TempDir(), "captured-busy.db")),
	)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(2)
	require.NoError(t, db.AutoMigrate(&openCodeGoContentionTestRow{}))
	row := openCodeGoContentionTestRow{Value: 1}
	require.NoError(t, db.Create(&row).Error)

	locker := db.Begin()
	require.NoError(t, locker.Error)
	require.NoError(t, locker.Model(&openCodeGoContentionTestRow{}).
		Where("id = ?", row.ID).
		Update("value", 2).Error)
	busyErr := db.Model(&openCodeGoContentionTestRow{}).
		Where("id = ?", row.ID).
		Update("value", 3).Error
	require.Error(t, busyErr)
	var sqliteErr *gosqlite.Error
	require.ErrorAs(t, busyErr, &sqliteErr)
	require.Equal(t, 5, sqliteErr.Code()&0xff)
	require.NoError(t, locker.Rollback().Error)
	require.NoError(t, sqlDB.Close())
	return busyErr
}

func TestIsRetryableOpenCodeGoSQLiteCode(t *testing.T) {
	for _, code := range []int{5, 6, 5 | (2 << 8), 6 | (1 << 8)} {
		assert.True(t, isRetryableOpenCodeGoSQLiteCode(code), "code %d should be retryable", code)
	}
	for _, code := range []int{0, 1, 9, 19, 263} {
		assert.False(t, isRetryableOpenCodeGoSQLiteCode(code), "code %d should be terminal", code)
	}
}

func TestRunOpenCodeGoContentionTransactionStopsOnCancellation(t *testing.T) {
	previousDatabaseType := common.MainDatabaseType()
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	t.Cleanup(func() { common.SetMainDatabaseType(previousDatabaseType) })

	openCodeGoSQLiteWriteSemaphore <- struct{}{}
	semaphoreHeld := true
	t.Cleanup(func() {
		if semaphoreHeld {
			<-openCodeGoSQLiteWriteSemaphore
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := runOpenCodeGoContentionTransaction(ctx, "test_cancelled", func(*gorm.DB) error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, called)

	<-openCodeGoSQLiteWriteSemaphore
	semaphoreHeld = false
}

func TestRunOpenCodeGoContentionTransactionDoesNotRetryTerminalError(t *testing.T) {
	db, _, _ := setupOpenCodeGoPoolTestDB(t)
	previousBusyTimeout := 0
	require.NoError(t, db.Raw("PRAGMA busy_timeout").Scan(&previousBusyTimeout).Error)
	wantErr := errors.New("terminal database failure")
	attempts := 0
	err := runOpenCodeGoContentionTransaction(context.Background(), "test_terminal", func(*gorm.DB) error {
		attempts++
		return wantErr
	})
	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, 1, attempts)
	busyTimeoutAfter := 0
	require.NoError(t, db.Raw("PRAGMA busy_timeout").Scan(&busyTimeoutAfter).Error)
	assert.Equal(t, previousBusyTimeout, busyTimeoutAfter)
}

func TestRunOpenCodeGoContentionTransactionStopsDuringBackoff(t *testing.T) {
	setupOpenCodeGoPoolTestDB(t)
	previousDatabaseType := common.MainDatabaseType()
	common.SetMainDatabaseType(common.DatabaseTypeMySQL)
	t.Cleanup(func() { common.SetMainDatabaseType(previousDatabaseType) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstAttempt := make(chan struct{})
	go func() {
		<-firstAttempt
		time.Sleep(2 * time.Millisecond)
		cancel()
	}()
	attempts := 0
	err := runOpenCodeGoContentionTransaction(ctx, "test_cancel_backoff", func(*gorm.DB) error {
		attempts++
		close(firstAttempt)
		return &mysqldriver.MySQLError{
			Number:  openCodeGoMySQLDeadlockErrno,
			Message: "synthetic deadlock",
		}
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, attempts)
}

func TestRunOpenCodeGoContentionTransactionExhaustsThreeAttempts(t *testing.T) {
	setupOpenCodeGoPoolTestDB(t)
	previousDatabaseType := common.MainDatabaseType()
	common.SetMainDatabaseType(common.DatabaseTypeMySQL)
	t.Cleanup(func() { common.SetMainDatabaseType(previousDatabaseType) })

	wantErr := &mysqldriver.MySQLError{
		Number:  openCodeGoMySQLDeadlockErrno,
		Message: "synthetic persistent deadlock",
	}
	attempts := 0
	err := runOpenCodeGoContentionTransaction(context.Background(), "test_exhausted", func(*gorm.DB) error {
		attempts++
		return wantErr
	})
	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, 3, attempts)
}

func TestRunOpenCodeGoContentionTransactionInterruptsSQLiteBusyWait(t *testing.T) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(30000)",
		filepath.ToSlash(filepath.Join(t.TempDir(), "busy-cancel.db")),
	)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	require.NoError(t, db.AutoMigrate(&openCodeGoContentionTestRow{}))
	row := openCodeGoContentionTestRow{Value: 1}
	require.NoError(t, db.Create(&row).Error)

	previousDB := model.DB
	previousDatabaseType := common.MainDatabaseType()
	model.DB = db
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	t.Cleanup(func() {
		model.DB = previousDB
		common.SetMainDatabaseType(previousDatabaseType)
		require.NoError(t, sqlDB.Close())
	})

	locker := db.Begin()
	require.NoError(t, locker.Error)
	require.NoError(t, locker.Model(&openCodeGoContentionTestRow{}).
		Where("id = ?", row.ID).
		Update("value", 2).Error)
	lockerReleased := false
	t.Cleanup(func() {
		if !lockerReleased {
			_ = locker.Rollback().Error
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	err = runOpenCodeGoContentionTransaction(ctx, "test_busy_cancel", func(tx *gorm.DB) error {
		return tx.Model(&openCodeGoContentionTestRow{}).
			Where("id = ?", row.ID).
			Update("value", 3).Error
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(startedAt), 2*time.Second)

	require.NoError(t, locker.Rollback().Error)
	lockerReleased = true
}
