package model

import (
	"errors"
	"strings"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

const OpenCodeGoCapabilityProvider = "opencode-go"

var ErrOpenCodeGoCapabilityStaleGeneration = errors.New("OpenCode Go capability snapshot generation is stale")

// OpenCodeGoCapabilitySnapshot is internal control-plane state. It deliberately
// lives outside Option so older nodes cannot expose or mutate capability data
// during a rolling upgrade.
type OpenCodeGoCapabilitySnapshot struct {
	Provider          string `json:"-" gorm:"type:varchar(64);primaryKey"`
	Generation        int64  `json:"-" gorm:"bigint;not null"`
	SchemaVersion     int    `json:"-" gorm:"not null"`
	SemanticRevision  string `json:"-" gorm:"type:varchar(64);not null"`
	SourceETag        string `json:"-" gorm:"type:varchar(256);not null"`
	CheckedAt         int64  `json:"-" gorm:"bigint;not null;index"`
	NormalizedPayload string `json:"-" gorm:"type:text;not null"`
	UpdatedAt         int64  `json:"-" gorm:"bigint;not null"`
}

type OpenCodeGoCapabilitySnapshotMetadata struct {
	Provider         string
	Generation       int64
	SchemaVersion    int
	SemanticRevision string
	SourceETag       string
	CheckedAt        int64
	UpdatedAt        int64
}

func GetOpenCodeGoCapabilitySnapshot() (*OpenCodeGoCapabilitySnapshot, error) {
	var snapshot OpenCodeGoCapabilitySnapshot
	err := DB.Where("provider = ?", OpenCodeGoCapabilityProvider).First(&snapshot).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func GetOpenCodeGoCapabilitySnapshotMetadata() (*OpenCodeGoCapabilitySnapshotMetadata, error) {
	var metadata OpenCodeGoCapabilitySnapshotMetadata
	err := DB.Model(&OpenCodeGoCapabilitySnapshot{}).
		Select("provider", "generation", "schema_version", "semantic_revision", "source_e_tag", "checked_at", "updated_at").
		Where("provider = ?", OpenCodeGoCapabilityProvider).
		Take(&metadata).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &metadata, nil
}

// PersistOpenCodeGoCapabilitySnapshotForTask stores a last-known-good snapshot
// only while the caller still owns the live capability-refresh lease. The task
// row, task lock, generation fence, and singleton snapshot are checked in the
// same transaction; publication to process memory must happen after this call
// commits successfully.
func PersistOpenCodeGoCapabilitySnapshotForTask(
	task *SystemTask,
	runnerID string,
	snapshot *OpenCodeGoCapabilitySnapshot,
) error {
	if task == nil || snapshot == nil || task.ID <= 0 || task.TaskID == "" || runnerID == "" {
		return ErrSystemTaskLockLost
	}
	if task.Type != SystemTaskTypeOpenCodeGoCapabilityRefresh {
		return ErrSystemTaskLockLost
	}
	if snapshot.Provider != OpenCodeGoCapabilityProvider ||
		snapshot.SchemaVersion <= 0 ||
		len(snapshot.SemanticRevision) != 64 ||
		len(snapshot.SourceETag) > 256 ||
		snapshot.CheckedAt <= 0 ||
		strings.TrimSpace(snapshot.NormalizedPayload) == "" ||
		len(snapshot.NormalizedPayload) > 60<<10 {
		return errors.New("invalid OpenCode Go capability snapshot")
	}

	stored := *snapshot
	stored.Generation = task.ID

	err := DB.Transaction(func(tx *gorm.DB) error {
		now := common.GetTimestamp()
		if stored.CheckedAt > now {
			return errors.New("OpenCode Go capability snapshot timestamp is in the future")
		}
		stored.UpdatedAt = now
		var persistedTask SystemTask
		if err := lockForUpdate(tx).
			Where(
				"id = ? AND task_id = ? AND type = ? AND status = ? AND locked_by = ?",
				task.ID,
				task.TaskID,
				SystemTaskTypeOpenCodeGoCapabilityRefresh,
				SystemTaskStatusRunning,
				runnerID,
			).
			First(&persistedTask).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrSystemTaskLockLost
			}
			return err
		}

		var lock SystemTaskLock
		if err := lockForUpdate(tx).
			Where(
				"type = ? AND task_id = ? AND locked_by = ? AND locked_until >= ?",
				SystemTaskTypeOpenCodeGoCapabilityRefresh,
				task.TaskID,
				runnerID,
				now,
			).
			First(&lock).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrSystemTaskLockLost
			}
			return err
		}

		var current OpenCodeGoCapabilitySnapshot
		err := lockForUpdate(tx).
			Where("provider = ?", OpenCodeGoCapabilityProvider).
			First(&current).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			return tx.Create(&stored).Error
		case err != nil:
			return err
		case current.CheckedAt > stored.CheckedAt:
			return ErrOpenCodeGoCapabilityStaleGeneration
		case current.Generation >= task.ID:
			return ErrOpenCodeGoCapabilityStaleGeneration
		default:
			result := tx.Model(&OpenCodeGoCapabilitySnapshot{}).
				Where("provider = ? AND generation < ?", OpenCodeGoCapabilityProvider, task.ID).
				Updates(map[string]any{
					"generation":         stored.Generation,
					"schema_version":     stored.SchemaVersion,
					"semantic_revision":  stored.SemanticRevision,
					"source_e_tag":       stored.SourceETag,
					"checked_at":         stored.CheckedAt,
					"normalized_payload": stored.NormalizedPayload,
					"updated_at":         stored.UpdatedAt,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrOpenCodeGoCapabilityStaleGeneration
			}
			return nil
		}
	})
	if err != nil {
		return err
	}
	*snapshot = stored
	return nil
}
