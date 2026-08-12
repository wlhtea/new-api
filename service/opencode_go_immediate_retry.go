package service

import (
	"errors"
	"sync"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

const OpenCodeGoImmediateRetryLimit = 1

const openCodeGoImmediateRetryStateKey = "opencode_go_immediate_retry_state"

type openCodeGoImmediateRetryState struct {
	mu                  sync.Mutex
	selection           *OpenCodeGoPoolSelection
	replaying           bool
	deferFailover       bool
	deferredFailoverRun func()
}

// ShouldRetryOpenCodeGoRelayError accepts only failures whose provenance was
// marked at the type-62 upstream boundary. Pool exhaustion is the sole setup
// error that receives the same one-time retry.
func ShouldRetryOpenCodeGoRelayError(channelType int, relayErr *types.NewAPIError) bool {
	if channelType != constant.ChannelTypeOpenCodeGo || relayErr == nil {
		return false
	}
	if errors.Is(relayErr, ErrOpenCodeGoNoEligibleWorkspace) {
		return true
	}
	if !IsOpenCodeGoUpstreamRelayError(relayErr) || IsViolationFeeCode(relayErr.GetErrorCode()) {
		return false
	}
	upstreamStatusCode, ok := openCodeGoUpstreamRelayStatusCode(relayErr)
	if !ok {
		upstreamStatusCode = relayErr.StatusCode
	}
	return IsOpenCodeGoGenericFailoverStatus(upstreamStatusCode)
}

// BeginOpenCodeGoImmediateRetry scopes selection reuse and failover deferral
// to the first attempt of one type-62 client request.
func BeginOpenCodeGoImmediateRetry(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(openCodeGoImmediateRetryStateKey, &openCodeGoImmediateRetryState{deferFailover: true})
}

func openCodeGoImmediateRetryStateFromContext(c *gin.Context) *openCodeGoImmediateRetryState {
	if c == nil {
		return nil
	}
	value, exists := c.Get(openCodeGoImmediateRetryStateKey)
	if !exists {
		return nil
	}
	state, _ := value.(*openCodeGoImmediateRetryState)
	return state
}

// RememberOpenCodeGoImmediateRetrySelection retains the already decrypted
// selection only for the lifetime of the current request. This lets a fresh
// adaptor replay against the exact same workspace and failover generation.
func RememberOpenCodeGoImmediateRetrySelection(c *gin.Context, selection *OpenCodeGoPoolSelection) {
	state := openCodeGoImmediateRetryStateFromContext(c)
	if state == nil || selection == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.selection != nil {
		return
	}
	state.selection = selection
}

// OpenCodeGoImmediateRetrySelection returns the first attempt's selection only
// after the controller has authorized the single replay.
func OpenCodeGoImmediateRetrySelection(c *gin.Context) (*OpenCodeGoPoolSelection, bool) {
	state := openCodeGoImmediateRetryStateFromContext(c)
	if state == nil {
		return nil, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.replaying || state.selection == nil {
		return nil, false
	}
	return state.selection, true
}

// DeferOpenCodeGoImmediateRetryFailover stores the first attempt's failover
// mutation until the controller knows whether the request will be replayed.
func DeferOpenCodeGoImmediateRetryFailover(c *gin.Context, run func()) bool {
	state := openCodeGoImmediateRetryStateFromContext(c)
	if state == nil || run == nil {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.deferFailover {
		return false
	}
	if state.deferredFailoverRun == nil {
		state.deferredFailoverRun = run
	}
	return true
}

// PrepareOpenCodeGoImmediateRetry discards first-attempt failover evidence and
// arms selection reuse. The second attempt records any failure normally.
func PrepareOpenCodeGoImmediateRetry(c *gin.Context) {
	state := openCodeGoImmediateRetryStateFromContext(c)
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.deferredFailoverRun = nil
	state.deferFailover = false
	state.replaying = true
}

// FlushOpenCodeGoImmediateRetryFailover preserves normal accounting when the
// first error is not replayable.
func FlushOpenCodeGoImmediateRetryFailover(c *gin.Context) {
	state := openCodeGoImmediateRetryStateFromContext(c)
	if state == nil {
		return
	}
	state.mu.Lock()
	run := state.deferredFailoverRun
	state.deferredFailoverRun = nil
	state.deferFailover = false
	state.mu.Unlock()
	if run != nil {
		run()
	}
}

// DiscardOpenCodeGoImmediateRetryFailover drops pending upstream evidence when
// cancellation or a downstream write failure makes the first attempt local.
func DiscardOpenCodeGoImmediateRetryFailover(c *gin.Context) {
	state := openCodeGoImmediateRetryStateFromContext(c)
	if state == nil {
		return
	}
	state.mu.Lock()
	state.deferredFailoverRun = nil
	state.deferFailover = false
	state.mu.Unlock()
}

func EndOpenCodeGoImmediateRetry(c *gin.Context) {
	state := openCodeGoImmediateRetryStateFromContext(c)
	if state != nil {
		state.mu.Lock()
		state.selection = nil
		state.deferredFailoverRun = nil
		state.deferFailover = false
		state.replaying = false
		state.mu.Unlock()
	}
	if c != nil {
		c.Set(openCodeGoImmediateRetryStateKey, nil)
	}
}
