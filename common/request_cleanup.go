package common

import (
	"sync"

	"github.com/gin-gonic/gin"
)

const requestCleanupStateKey = "oneapi_request_cleanup_state"

type requestCleanupState struct {
	mu       sync.Mutex
	once     sync.Once
	cleanups []func()
}

// RegisterRequestCleanup adds an idempotent request-lifetime cleanup. The
// cleanup middleware runs callbacks in reverse acquisition order on every
// exit path, including panic unwinding.
func RegisterRequestCleanup(c *gin.Context, cleanup func()) {
	if c == nil || cleanup == nil {
		return
	}
	state := getOrCreateRequestCleanupState(c)
	state.mu.Lock()
	state.cleanups = append(state.cleanups, cleanup)
	state.mu.Unlock()
}

// RunRequestCleanups executes every registered callback at most once. A
// broken cleanup cannot prevent the remaining resources from being released.
func RunRequestCleanups(c *gin.Context) {
	if c == nil {
		return
	}
	value, exists := c.Get(requestCleanupStateKey)
	if !exists {
		return
	}
	state, ok := value.(*requestCleanupState)
	if !ok || state == nil {
		return
	}
	state.once.Do(func() {
		state.mu.Lock()
		cleanups := append([]func(){}, state.cleanups...)
		state.cleanups = nil
		state.mu.Unlock()

		for i := len(cleanups) - 1; i >= 0; i-- {
			runRequestCleanup(cleanups[i])
		}
	})
}

func getOrCreateRequestCleanupState(c *gin.Context) *requestCleanupState {
	if value, exists := c.Get(requestCleanupStateKey); exists {
		if state, ok := value.(*requestCleanupState); ok && state != nil {
			return state
		}
	}
	state := &requestCleanupState{}
	c.Set(requestCleanupStateKey, state)
	return state
}

func runRequestCleanup(cleanup func()) {
	defer func() {
		if recover() != nil {
			SysError("panic recovered while releasing a request resource")
		}
	}()
	cleanup()
}
