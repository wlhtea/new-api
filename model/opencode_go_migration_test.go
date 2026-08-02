package model

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func testOpenCodeGoMigrationAndQueries(t *testing.T, db *gorm.DB, databaseType common.DatabaseType) {
	t.Helper()
	previousDB := DB
	previousMainType := common.MainDatabaseType()
	previousLogType := common.LogDatabaseType()
	DB = db
	common.SetDatabaseTypes(databaseType, previousLogType)
	initCol()
	t.Cleanup(func() {
		DB = previousDB
		common.SetDatabaseTypes(previousMainType, previousLogType)
		initCol()
	})

	models := []any{
		&Channel{},
		&Ability{},
		&OpenCodeGoIdentity{},
		&OpenCodeGoWorkspace{},
		&OpenCodeGoQuotaWindow{},
		&OpenCodeGoWorkspaceModel{},
		&OpenCodeGoOperation{},
	}
	require.NoError(t, db.AutoMigrate(models...))
	require.NoError(t, db.AutoMigrate(models...), "OpenCode Go migration must be idempotent")

	now := time.Now().Unix()
	unique := fmt.Sprintf("%d", time.Now().UnixNano())
	channel := &Channel{
		Type:   constant.ChannelTypeOpenCodeGo,
		Name:   "OpenCode Go migration " + unique,
		Status: common.ChannelStatusEnabled,
		Models: "model-a",
		Group:  "default",
	}
	require.NoError(t, db.Create(channel).Error)
	t.Cleanup(func() {
		_ = DeleteOpenCodeGoPoolByChannelTx(db, []int{channel.Id})
		_ = db.Where("channel_id = ?", channel.Id).Delete(&Ability{}).Error
		_ = db.Where("id = ?", channel.Id).Delete(&Channel{}).Error
	})

	identity := &OpenCodeGoIdentity{
		UID:                   "identity-" + unique,
		ChannelID:             channel.Id,
		AuthCookieCiphertext:  "synthetic-cookie-ciphertext",
		AuthCookieFingerprint: fmt.Sprintf("%064s", unique),
		Status:                OpenCodeGoIdentityStatusActive,
		LastSyncedAt:          now,
	}
	require.NoError(t, db.Create(identity).Error)
	workspace := &OpenCodeGoWorkspace{
		UID:                 "workspace-" + unique,
		ChannelID:           channel.Id,
		IdentityID:          identity.ID,
		UpstreamWorkspaceID: "wrk_" + unique,
		APIKeyCiphertext:    "synthetic-key-ciphertext",
		CredentialStatus:    OpenCodeGoCredentialValid,
		MembershipStatus:    OpenCodeGoMembershipActive,
		ManualEnabled:       true,
		EffectiveState:      OpenCodeGoStateEligible,
		QuotaSnapshotStatus: OpenCodeGoQuotaSnapshotComplete,
		QuotaFetchedAt:      now,
		QuotaNextRefreshAt:  now - 1,
		LastSyncedAt:        now,
	}
	require.NoError(t, db.Create(workspace).Error)
	for index, kind := range OpenCodeGoQuotaKinds {
		require.NoError(t, db.Create(&OpenCodeGoQuotaWindow{
			WorkspaceID:  workspace.ID,
			Kind:         kind,
			UsedPercent:  float64(10 + index),
			ResetSeconds: int64((index + 1) * 3600),
			ResetAt:      now + int64((index+1)*3600),
			FetchedAt:    now,
		}).Error)
	}
	require.NoError(t, db.Create(&OpenCodeGoWorkspaceModel{
		WorkspaceID: workspace.ID,
		Model:       "model-a",
		Discovered:  true,
		State:       OpenCodeGoModelAvailable,
	}).Error)
	require.NoError(t, db.Create(&OpenCodeGoOperation{
		UID:          "operation-" + unique,
		ChannelID:    channel.Id,
		WorkspaceID:  workspace.ID,
		WorkspaceUID: workspace.UID,
		Action:       "migration_test",
		Status:       "succeeded",
	}).Error)

	identities, err := ListOpenCodeGoIdentities(channel.Id)
	require.NoError(t, err)
	require.Len(t, identities, 1)
	require.Len(t, identities[0].Workspaces, 1)
	assert.True(t, identities[0].Workspaces[0].ManualEnabled)
	require.Len(t, identities[0].Workspaces[0].QuotaWindows, len(OpenCodeGoQuotaKinds))
	require.Len(t, identities[0].Workspaces[0].Models, 1)
	assert.True(t, identities[0].Workspaces[0].Models[0].Discovered)

	targets, err := ListOpenCodeGoDueRefreshTargets(now, now-900, 5000)
	require.NoError(t, err)
	assert.Contains(t, targets, OpenCodeGoRefreshTarget{
		ChannelID:   channel.Id,
		IdentityUID: identity.UID,
	})

	require.NoError(t, channel.Delete())
	for _, table := range []any{
		&Channel{},
		&Ability{},
		&OpenCodeGoIdentity{},
		&OpenCodeGoWorkspace{},
		&OpenCodeGoOperation{},
	} {
		var count int64
		query := db.Model(table)
		switch table.(type) {
		case *Channel:
			query = query.Where("id = ?", channel.Id)
		default:
			query = query.Where("channel_id = ?", channel.Id)
		}
		require.NoError(t, query.Count(&count).Error)
		assert.Zero(t, count)
	}
	for _, table := range []any{&OpenCodeGoQuotaWindow{}, &OpenCodeGoWorkspaceModel{}} {
		var count int64
		require.NoError(t, db.Model(table).Where("workspace_id = ?", workspace.ID).Count(&count).Error)
		assert.Zero(t, count)
	}
}

func TestOpenCodeGoMigrationAndQueriesSQLite(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	testOpenCodeGoMigrationAndQueries(t, db, common.DatabaseTypeSQLite)
}

func TestOpenCodeGoMigrationAndQueriesConfiguredDatabases(t *testing.T) {
	tests := []struct {
		name         string
		env          string
		databaseType common.DatabaseType
		dialector    func(string) gorm.Dialector
	}{
		{name: "mysql", env: "TEST_MYSQL_DSN", databaseType: common.DatabaseTypeMySQL, dialector: mysql.Open},
		{
			name:         "postgres",
			env:          "TEST_POSTGRES_DSN",
			databaseType: common.DatabaseTypePostgreSQL,
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
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = sqlDB.Close() })
			testOpenCodeGoMigrationAndQueries(t, db, test.databaseType)
		})
	}
}
