package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	gosqlite "github.com/glebarez/go-sqlite"
	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

const (
	openCodeGoContentionTimeout        = 10 * time.Second
	openCodeGoSQLiteAttemptBusyTimeout = time.Second
)

var (
	openCodeGoContentionRetryDelays = [...]time.Duration{
		10 * time.Millisecond,
		25 * time.Millisecond,
	}
	openCodeGoSQLiteWriteSemaphore = make(chan struct{}, 1)
)

const (
	openCodeGoMySQLLockWaitTimeoutErrno = 1205 // ER_LOCK_WAIT_TIMEOUT
	openCodeGoMySQLDeadlockErrno        = 1213 // ER_LOCK_DEADLOCK
)

func runOpenCodeGoContentionTransaction(
	ctx context.Context,
	operation string,
	transaction func(*gorm.DB) error,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, openCodeGoContentionTimeout)
	defer cancel()

	release, err := acquireOpenCodeGoSQLiteWrite(ctx)
	if err != nil {
		return err
	}
	defer release()

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := executeOpenCodeGoContentionTransaction(ctx, transaction)
		if err == nil {
			if attempt > 0 {
				common.SysLog(fmt.Sprintf(
					"OpenCode Go database contention recovered: operation=%s attempts=%d",
					operation,
					attempt+1,
				))
			}
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if !isRetryableOpenCodeGoContentionError(err) {
			return err
		}
		if attempt == len(openCodeGoContentionRetryDelays) {
			common.SysError(fmt.Sprintf(
				"OpenCode Go database contention exhausted: operation=%s attempts=%d",
				operation,
				attempt+1,
			))
			return err
		}
		if err := waitForOpenCodeGoContentionRetry(ctx, openCodeGoContentionRetryDelays[attempt]); err != nil {
			return err
		}
	}
}

func executeOpenCodeGoContentionTransaction(
	ctx context.Context,
	transaction func(*gorm.DB) error,
) error {
	if !common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		return model.DB.WithContext(ctx).Transaction(transaction)
	}
	return model.DB.WithContext(ctx).Connection(func(connection *gorm.DB) error {
		return runOpenCodeGoSQLiteTransaction(connection, ctx, transaction)
	})
}

// The modernc SQLite busy handler does not observe Go context cancellation.
// Bound it per pinned connection, then restore that connection before reuse.
func runOpenCodeGoSQLiteTransaction(
	connection *gorm.DB,
	ctx context.Context,
	transaction func(*gorm.DB) error,
) error {
	control := connection.Session(&gorm.Session{Context: context.Background()})
	previousBusyTimeout := 0
	if err := control.Raw("PRAGMA busy_timeout").Scan(&previousBusyTimeout).Error; err != nil {
		return err
	}
	attemptMilliseconds := openCodeGoSQLiteAttemptBusyTimeout.Milliseconds()
	if err := control.Exec(fmt.Sprintf("PRAGMA busy_timeout = %d", attemptMilliseconds)).Error; err != nil {
		return err
	}
	defer func() {
		_, restoreErr := control.Statement.ConnPool.ExecContext(
			context.Background(),
			fmt.Sprintf("PRAGMA busy_timeout = %d", previousBusyTimeout),
		)
		if restoreErr != nil {
			common.SysError("OpenCode Go SQLite busy timeout restoration failed")
		}
	}()
	return connection.WithContext(ctx).Transaction(transaction)
}

func acquireOpenCodeGoSQLiteWrite(ctx context.Context) (func(), error) {
	if !common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		return func() {}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case openCodeGoSQLiteWriteSemaphore <- struct{}{}:
		return func() { <-openCodeGoSQLiteWriteSemaphore }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func waitForOpenCodeGoContentionRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func isRetryableOpenCodeGoContentionError(err error) bool {
	switch common.MainDatabaseType() {
	case common.DatabaseTypeSQLite:
		var sqliteErr *gosqlite.Error
		if !errors.As(err, &sqliteErr) {
			return false
		}
		return isRetryableOpenCodeGoSQLiteCode(sqliteErr.Code())
	case common.DatabaseTypeMySQL:
		var mysqlErr *mysqldriver.MySQLError
		if !errors.As(err, &mysqlErr) {
			return false
		}
		return mysqlErr.Number == openCodeGoMySQLLockWaitTimeoutErrno ||
			mysqlErr.Number == openCodeGoMySQLDeadlockErrno
	default:
		return false
	}
}

func isRetryableOpenCodeGoSQLiteCode(code int) bool {
	baseCode := code & 0xff
	return baseCode == 5 || baseCode == 6
}
