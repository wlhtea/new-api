package common

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// BodyStorage 请求体存储接口
type BodyStorage interface {
	io.ReadSeeker
	io.Closer
	// Bytes 获取全部内容
	Bytes() ([]byte, error)
	// Size 获取数据大小
	Size() int64
	// IsDisk 是否是磁盘存储
	IsDisk() bool
	// NewReader returns an independent reader positioned at the start of the
	// stored payload. Each call returns a reader with its own cursor, so
	// callers (e.g. http.Request.GetBody) can replay the body concurrently
	// with, or after, other readers without sharing seek state. Closing the
	// returned reader releases only that reader, never the storage itself;
	// after the storage has been closed, NewReader returns ErrStorageClosed.
	NewReader() (io.ReadCloser, error)
}

// ReplayableBody is an outbound request body that can report its byte size and
// create independent readers for transport-level retries.
type ReplayableBody interface {
	io.Reader
	Size() int64
	NewReader() (io.ReadCloser, error)
}

// ErrStorageClosed 存储已关闭错误
var ErrStorageClosed = fmt.Errorf("body storage is closed")

var ErrDiskCacheCapacity = errors.New("disk body cache capacity exceeded")

// memoryStorage 内存存储实现
type memoryStorage struct {
	data   []byte
	reader *bytes.Reader
	size   int64
	closed int32
	mu     sync.Mutex
}

func newMemoryStorage(data []byte) *memoryStorage {
	immutableData := append([]byte(nil), data...)
	return newMemoryStorageOwned(immutableData)
}

func newMemoryStorageOwned(immutableData []byte) *memoryStorage {
	size := int64(len(immutableData))
	IncrementMemoryBuffers(size)
	return &memoryStorage{
		data:   immutableData,
		reader: bytes.NewReader(immutableData),
		size:   size,
	}
}

func (m *memoryStorage) Read(p []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if atomic.LoadInt32(&m.closed) == 1 {
		return 0, ErrStorageClosed
	}
	return m.reader.Read(p)
}

func (m *memoryStorage) ReadAt(p []byte, offset int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if atomic.LoadInt32(&m.closed) == 1 {
		return 0, ErrStorageClosed
	}
	return bytes.NewReader(m.data).ReadAt(p, offset)
}

func (m *memoryStorage) Seek(offset int64, whence int) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if atomic.LoadInt32(&m.closed) == 1 {
		return 0, ErrStorageClosed
	}
	return m.reader.Seek(offset, whence)
}

func (m *memoryStorage) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if atomic.CompareAndSwapInt32(&m.closed, 0, 1) {
		DecrementMemoryBuffers(m.size)
	}
	return nil
}

func (m *memoryStorage) Bytes() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if atomic.LoadInt32(&m.closed) == 1 {
		return nil, ErrStorageClosed
	}
	return append([]byte(nil), m.data...), nil
}

func (m *memoryStorage) NewReader() (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if atomic.LoadInt32(&m.closed) == 1 {
		return nil, ErrStorageClosed
	}
	// A fresh bytes.Reader over the shared immutable backing array: an
	// independent cursor at zero copy cost. NopCloser keeps Close a no-op, so
	// the storage lifecycle stays owned by whoever holds the storage itself.
	return io.NopCloser(bytes.NewReader(m.data)), nil
}

func (m *memoryStorage) Size() int64 {
	return m.size
}

func (m *memoryStorage) IsDisk() bool {
	return false
}

// diskStorage 磁盘存储实现
type diskStorage struct {
	file     *os.File
	filePath string
	size     int64
	closed   int32
	mu       sync.Mutex
}

func newDiskStorage(data []byte, cachePath string) (*diskStorage, error) {
	size := int64(len(data))
	if !ReserveDiskCacheBytes(size) {
		return nil, ErrDiskCacheCapacity
	}
	committed := false
	defer func() {
		if !committed {
			releaseDiskCacheBytes(size)
		}
	}()
	// 使用统一的缓存目录管理
	filePath, file, err := CreateDiskCacheFile(DiskCacheTypeBody)
	if err != nil {
		return nil, err
	}

	registerActiveDiskCacheFile(filePath)
	cleanupFile := true
	defer func() {
		if cleanupFile {
			_ = file.Close()
			_ = os.Remove(filePath)
			unregisterActiveDiskCacheFile(filePath)
		}
	}()

	// 写入数据
	n, err := file.Write(data)
	if err != nil {
		return nil, fmt.Errorf("failed to write to temp file: %w", err)
	}
	if n != len(data) {
		return nil, io.ErrShortWrite
	}

	// 重置文件指针
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("failed to seek temp file: %w", err)
	}

	commitReservedDiskFile()
	committed = true
	cleanupFile = false

	return &diskStorage{
		file:     file,
		filePath: filePath,
		size:     size,
	}, nil
}

func newDiskStorageFromReader(reader io.Reader, expectedBytes int64, maxBytes int64, cachePath string) (*diskStorage, error) {
	// 使用统一的缓存目录管理
	filePath, file, err := CreateDiskCacheFile(DiskCacheTypeBody)
	if err != nil {
		return nil, err
	}
	registerActiveDiskCacheFile(filePath)
	var reserved int64
	committed := false
	if expectedBytes >= 0 {
		if !ReserveDiskCacheBytes(expectedBytes) {
			_ = file.Close()
			_ = os.Remove(filePath)
			unregisterActiveDiskCacheFile(filePath)
			return nil, ErrDiskCacheCapacity
		}
		reserved = expectedBytes
	}
	defer func() {
		if !committed {
			_ = file.Close()
			_ = os.Remove(filePath)
			unregisterActiveDiskCacheFile(filePath)
			releaseDiskCacheBytes(reserved)
		}
	}()

	buffer := make([]byte, 32<<10)
	var written int64
	for {
		count, readErr := reader.Read(buffer)
		if count > 0 {
			if written > maxBytes-int64(count) {
				return nil, ErrRequestBodyTooLarge
			}
			required := written + int64(count)
			if required > reserved {
				additional := required - reserved
				if !ReserveDiskCacheBytes(additional) {
					return nil, ErrDiskCacheCapacity
				}
				reserved += additional
			}
			writeCount, writeErr := file.Write(buffer[:count])
			written += int64(writeCount)
			if writeErr != nil {
				return nil, fmt.Errorf("failed to write to temp file: %w", writeErr)
			}
			if writeCount != count {
				return nil, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("failed to read request body: %w", readErr)
		}
	}
	if reserved > written {
		releaseDiskCacheBytes(reserved - written)
		reserved = written
	}

	// 重置文件指针
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("failed to seek temp file: %w", err)
	}

	commitReservedDiskFile()
	committed = true

	return &diskStorage{
		file:     file,
		filePath: filePath,
		size:     written,
	}, nil
}

func (d *diskStorage) Read(p []byte) (n int, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if atomic.LoadInt32(&d.closed) == 1 {
		return 0, ErrStorageClosed
	}
	return d.file.Read(p)
}

func (d *diskStorage) ReadAt(p []byte, offset int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if atomic.LoadInt32(&d.closed) == 1 {
		return 0, ErrStorageClosed
	}
	return d.file.ReadAt(p, offset)
}

func (d *diskStorage) Seek(offset int64, whence int) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if atomic.LoadInt32(&d.closed) == 1 {
		return 0, ErrStorageClosed
	}
	return d.file.Seek(offset, whence)
}

func (d *diskStorage) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if atomic.CompareAndSwapInt32(&d.closed, 0, 1) {
		closeErr := d.file.Close()
		removeErr := os.Remove(d.filePath)
		unregisterActiveDiskCacheFile(d.filePath)
		releaseReservedDiskFile(d.size)
		if closeErr != nil {
			return closeErr
		}
		if removeErr != nil && !os.IsNotExist(removeErr) {
			return removeErr
		}
	}
	return nil
}

func (d *diskStorage) Bytes() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if atomic.LoadInt32(&d.closed) == 1 {
		return nil, ErrStorageClosed
	}

	// 保存当前位置
	currentPos, err := d.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}

	// 移动到开头
	if _, err := d.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	// 读取全部内容
	data := make([]byte, d.size)
	_, err = io.ReadFull(d.file, data)
	if err != nil {
		return nil, err
	}

	// 恢复位置
	if _, err := d.file.Seek(currentPos, io.SeekStart); err != nil {
		return nil, err
	}

	return data, nil
}

func (d *diskStorage) NewReader() (io.ReadCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if atomic.LoadInt32(&d.closed) == 1 {
		return nil, ErrStorageClosed
	}
	// A separate file descriptor over the same cache file: an independent
	// cursor at zero copy cost. Closing the returned reader closes only that
	// descriptor; the storage keeps owning the primary descriptor and the
	// file's lifetime. Readers opened before Close stay usable even after the
	// file is unlinked, as the descriptor keeps the inode alive.
	file, err := os.Open(d.filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open body cache file for replay: %w", err)
	}
	return file, nil
}

func (d *diskStorage) Size() int64 {
	return d.size
}

func (d *diskStorage) IsDisk() bool {
	return true
}

// CreateBodyStorage 根据数据大小创建合适的存储
func CreateBodyStorage(data []byte) (BodyStorage, error) {
	size := int64(len(data))
	threshold := GetDiskCacheThresholdBytes()

	// 检查是否应该使用磁盘缓存
	if IsDiskCacheEnabled() && size >= threshold {
		storage, err := newDiskStorage(data, GetDiskCachePath())
		if err != nil {
			return nil, fmt.Errorf("disk body storage creation failed: %w", err)
		}
		return storage, nil
	}

	return newMemoryStorage(data), nil
}

// CreateBodyStorageFromReader 从 Reader 创建存储（用于大请求的流式处理）
func CreateBodyStorageFromReader(reader io.Reader, contentLength int64, maxBytes int64) (BodyStorage, error) {
	if reader == nil {
		return nil, errors.New("request body reader is nil")
	}
	if maxBytes < 0 {
		return nil, errors.New("request body limit is invalid")
	}
	if contentLength > maxBytes {
		return nil, ErrRequestBodyTooLarge
	}
	threshold := GetDiskCacheThresholdBytes()

	// Known large bodies stream directly to disk. Unknown or nominally small
	// bodies use a threshold-sized prefix and spill without ever reading the
	// max request size into heap first.
	if IsDiskCacheEnabled() && contentLength >= threshold && contentLength >= 0 {
		storage, err := newDiskStorageFromReader(reader, contentLength, maxBytes, GetDiskCachePath())
		if err != nil {
			return nil, fmt.Errorf("disk storage creation failed: %w", err)
		}
		IncrementDiskCacheHits()
		return storage, nil
	}
	if IsDiskCacheEnabled() {
		prefixLimit := threshold + 1
		if threshold >= maxBytes {
			prefixLimit = maxBytes + 1
		}
		prefix, err := io.ReadAll(io.LimitReader(reader, prefixLimit))
		if err != nil {
			return nil, err
		}
		if int64(len(prefix)) > maxBytes {
			return nil, ErrRequestBodyTooLarge
		}
		if int64(len(prefix)) > threshold {
			storage, err := newDiskStorageFromReader(io.MultiReader(bytes.NewReader(prefix), reader), -1, maxBytes, GetDiskCachePath())
			if err != nil {
				return nil, fmt.Errorf("disk storage creation failed: %w", err)
			}
			IncrementDiskCacheHits()
			return storage, nil
		}
		IncrementMemoryCacheHits()
		return newMemoryStorageOwned(prefix), nil
	}

	// 使用内存读取
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrRequestBodyTooLarge
	}

	IncrementMemoryCacheHits()
	return newMemoryStorageOwned(data), nil
}

type replayableBodyReader struct {
	storage BodyStorage
}

func (r replayableBodyReader) Read(p []byte) (int, error) {
	return r.storage.Read(p)
}

func (r replayableBodyReader) Size() int64 {
	return r.storage.Size()
}

func (r replayableBodyReader) NewReader() (io.ReadCloser, error) {
	return r.storage.NewReader()
}

// NewReplayableBodyReader exposes the replay capabilities of storage without
// exposing io.Closer. This keeps ownership of the storage lifecycle with the
// caller instead of allowing net/http to close it as the request body.
func NewReplayableBodyReader(storage BodyStorage) ReplayableBody {
	return replayableBodyReader{storage: storage}
}

// CleanupOldCacheFiles 清理旧的缓存文件（用于启动时清理残留）
func CleanupOldCacheFiles() {
	// 使用统一的缓存管理
	CleanupOldDiskCacheFiles(5 * time.Minute)
}
