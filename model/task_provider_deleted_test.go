package model

import (
	"reflect"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestTaskProviderDeletedIsPersistentPrivateTombstone(t *testing.T) {
	field, found := reflect.TypeOf(Task{}).FieldByName("ProviderDeleted")
	require.True(t, found, "Task must retain an explicit provider-deletion tombstone")
	assert.Equal(t, "-", field.Tag.Get("json"))

	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&Task{}))
	assert.True(t, database.Migrator().HasColumn(&Task{}, "provider_deleted"))
}
