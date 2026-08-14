package common

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestDefaultSQLitePathConfiguresBusyTimeout(t *testing.T) {
	queryIndex := strings.IndexByte(SQLitePath, '?')
	require.NotEqual(t, -1, queryIndex)
	testPath := filepath.ToSlash(filepath.Join(t.TempDir(), "busy-timeout.db")) + SQLitePath[queryIndex:]
	db, err := gorm.Open(sqlite.Open(testPath), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })

	var busyTimeout int
	require.NoError(t, db.Raw("PRAGMA busy_timeout").Scan(&busyTimeout).Error)
	require.Equal(t, 30_000, busyTimeout)
}
