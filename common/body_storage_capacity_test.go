package common

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withDiskBodyStorageConfig(t *testing.T, thresholdMB, maxSizeMB int) {
	t.Helper()
	original := GetDiskCacheConfig()
	SetDiskCacheConfig(DiskCacheConfig{
		Enabled:     true,
		ThresholdMB: thresholdMB,
		MaxSizeMB:   maxSizeMB,
		Path:        filepath.Clean(t.TempDir()),
	})
	t.Cleanup(func() { SetDiskCacheConfig(original) })
}

func requireNoActiveDiskReservations(t *testing.T) {
	t.Helper()
	stats := GetDiskCacheStats()
	require.Zero(t, stats.ActiveDiskFiles)
	require.Zero(t, stats.CurrentDiskUsageBytes)
}

func TestDiskBodyStorageCapacityReservationIsAtomic(t *testing.T) {
	requireNoActiveDiskReservations(t)
	withDiskBodyStorageConfig(t, 0, 1)
	payload := bytes.Repeat([]byte("x"), 600<<10)
	start := make(chan struct{})
	type result struct {
		storage BodyStorage
		err     error
	}
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			storage, err := CreateBodyStorage(payload)
			results <- result{storage: storage, err: err}
		}()
	}
	close(start)
	wait.Wait()
	close(results)

	var successes, capacityFailures int
	for result := range results {
		if result.err == nil {
			successes++
			require.NoError(t, result.storage.Close())
			continue
		}
		if errors.Is(result.err, ErrDiskCacheCapacity) {
			capacityFailures++
		}
	}
	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, capacityFailures)
	requireNoActiveDiskReservations(t)
}

func TestDiskBodyStorageCapacityExactLimitAndCapPlusOne(t *testing.T) {
	requireNoActiveDiskReservations(t)
	withDiskBodyStorageConfig(t, 0, 1)

	exact, err := CreateBodyStorage(make([]byte, 1<<20))
	require.NoError(t, err)
	require.True(t, exact.IsDisk())
	stats := GetDiskCacheStats()
	assert.Equal(t, int64(1), stats.ActiveDiskFiles)
	assert.Equal(t, int64(1<<20), stats.CurrentDiskUsageBytes)
	require.NoError(t, exact.Close())

	_, err = CreateBodyStorage(make([]byte, (1<<20)+1))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDiskCacheCapacity)
	requireNoActiveDiskReservations(t)
}

func TestDiskBodyStorageSpillFailureDoesNotFallbackToHeap(t *testing.T) {
	requireNoActiveDiskReservations(t)
	original := GetDiskCacheConfig()
	blockedPath := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blockedPath, []byte("blocked"), 0600))
	SetDiskCacheConfig(DiskCacheConfig{
		Enabled:     true,
		ThresholdMB: 0,
		MaxSizeMB:   1,
		Path:        blockedPath,
	})
	t.Cleanup(func() { SetDiskCacheConfig(original) })

	storage, err := CreateBodyStorage([]byte("must-spill"))
	require.Nil(t, storage)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disk body storage creation failed")
	requireNoActiveDiskReservations(t)
}

func TestUnknownLengthBodySpillsAfterBoundedPrefix(t *testing.T) {
	requireNoActiveDiskReservations(t)
	withDiskBodyStorageConfig(t, 1, 4)
	payload := bytes.Repeat([]byte("z"), (1<<20)+1)

	storage, err := CreateBodyStorageFromReader(bytes.NewReader(payload), -1, 2<<20)
	require.NoError(t, err)
	require.True(t, storage.IsDisk())
	t.Cleanup(func() { _ = storage.Close() })
	reader, err := storage.NewReader()
	require.NoError(t, err)
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	assert.Equal(t, payload, got)
	require.NoError(t, storage.Close())
	requireNoActiveDiskReservations(t)
}

func TestActiveDiskBodyStorageIsSkippedByAgeCleanup(t *testing.T) {
	requireNoActiveDiskReservations(t)
	withDiskBodyStorageConfig(t, 0, 1)
	payload := []byte(`{"model":"still-live"}`)
	storage, err := CreateBodyStorage(payload)
	require.NoError(t, err)
	disk, ok := storage.(*diskStorage)
	require.True(t, ok)
	t.Cleanup(func() { _ = storage.Close() })
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(disk.filePath, old, old))

	require.NoError(t, CleanupOldDiskCacheFiles(time.Minute))
	_, err = os.Stat(disk.filePath)
	require.NoError(t, err)
	reader, err := storage.NewReader()
	require.NoError(t, err)
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	assert.Equal(t, payload, got)

	require.NoError(t, storage.Close())
	_, err = os.Stat(disk.filePath)
	assert.True(t, os.IsNotExist(err))
	requireNoActiveDiskReservations(t)
}

func TestDiskReaderLimitFailureReleasesReservation(t *testing.T) {
	requireNoActiveDiskReservations(t)
	withDiskBodyStorageConfig(t, 0, 4)

	storage, err := CreateBodyStorageFromReader(bytes.NewReader(make([]byte, 1025)), -1, 1024)
	require.Nil(t, storage)
	assert.ErrorIs(t, err, ErrRequestBodyTooLarge)
	requireNoActiveDiskReservations(t)
}
