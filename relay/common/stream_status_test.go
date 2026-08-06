package common

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStreamStatus_SetEndReason_FirstWins(t *testing.T) {
	t.Parallel()
	s := NewStreamStatus()

	s.SetEndReason(StreamEndReasonDone, nil)
	s.SetEndReason(StreamEndReasonTimeout, nil)
	s.SetEndReason(StreamEndReasonClientGone, fmt.Errorf("context canceled"))

	assert.Equal(t, StreamEndReasonDone, s.EndReason)
	assert.Nil(t, s.EndError)
}

func TestStreamStatus_SetEndReason_WithError(t *testing.T) {
	t.Parallel()
	s := NewStreamStatus()

	expectedErr := fmt.Errorf("read: connection reset")
	s.SetEndReason(StreamEndReasonScannerErr, expectedErr)

	assert.Equal(t, StreamEndReasonScannerErr, s.EndReason)
	assert.Equal(t, expectedErr, s.EndError)
}

func TestStreamStatus_SetEndReason_NilSafe(t *testing.T) {
	t.Parallel()
	var s *StreamStatus
	s.SetEndReason(StreamEndReasonDone, nil)
}

func TestStreamStatus_SetEndReason_Concurrent(t *testing.T) {
	t.Parallel()
	s := NewStreamStatus()

	reasons := []StreamEndReason{
		StreamEndReasonDone,
		StreamEndReasonTimeout,
		StreamEndReasonClientGone,
		StreamEndReasonScannerErr,
		StreamEndReasonHandlerStop,
		StreamEndReasonEOF,
		StreamEndReasonPanic,
		StreamEndReasonPingFail,
	}

	var wg sync.WaitGroup
	for _, r := range reasons {
		wg.Add(1)
		go func(reason StreamEndReason) {
			defer wg.Done()
			s.SetEndReason(reason, nil)
		}(r)
	}
	wg.Wait()

	assert.NotEqual(t, StreamEndReasonNone, s.EndReason)
}

func TestStreamStatus_RecordError_Basic(t *testing.T) {
	t.Parallel()
	s := NewStreamStatus()

	s.RecordError("bad json")
	s.RecordError("another bad json")
	s.RecordError("client gone")

	assert.True(t, s.HasErrors())
	assert.Equal(t, 3, s.TotalErrorCount())
	assert.Len(t, s.Errors, 3)
}

func TestStreamStatus_RecordError_CapAtMax(t *testing.T) {
	t.Parallel()
	s := NewStreamStatus()

	for i := 0; i < 30; i++ {
		s.RecordError(fmt.Sprintf("error_%d", i))
	}

	assert.Equal(t, maxStreamErrorEntries, len(s.Errors))
	assert.Equal(t, 30, s.TotalErrorCount())
}

func TestStreamStatus_RecordError_NilSafe(t *testing.T) {
	t.Parallel()
	var s *StreamStatus
	s.RecordError("should not panic")
}

func TestStreamStatus_RecordError_Concurrent(t *testing.T) {
	t.Parallel()
	s := NewStreamStatus()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			s.RecordError(fmt.Sprintf("error_%d", idx))
		}(i)
	}
	wg.Wait()

	assert.Equal(t, 100, s.TotalErrorCount())
	assert.LessOrEqual(t, len(s.Errors), maxStreamErrorEntries)
}

func TestStreamStatus_HasErrors_Empty(t *testing.T) {
	t.Parallel()
	s := NewStreamStatus()
	assert.False(t, s.HasErrors())
	assert.Equal(t, 0, s.TotalErrorCount())
}

func TestStreamStatus_HasErrors_NilSafe(t *testing.T) {
	t.Parallel()
	var s *StreamStatus
	assert.False(t, s.HasErrors())
	assert.Equal(t, 0, s.TotalErrorCount())
}

func TestStreamStatus_IsNormalEnd(t *testing.T) {
	t.Parallel()
	tests := []struct {
		reason StreamEndReason
		normal bool
	}{
		{StreamEndReasonDone, true},
		{StreamEndReasonEOF, true},
		{StreamEndReasonHandlerStop, true},
		{StreamEndReasonTimeout, false},
		{StreamEndReasonClientGone, false},
		{StreamEndReasonScannerErr, false},
		{StreamEndReasonPanic, false},
		{StreamEndReasonPingFail, false},
		{StreamEndReasonNone, false},
	}
	for _, tt := range tests {
		s := NewStreamStatus()
		s.SetEndReason(tt.reason, nil)
		assert.Equal(t, tt.normal, s.IsNormalEnd(), "reason=%s", tt.reason)
	}
}

func TestStreamStatus_IsNormalEnd_NilSafe(t *testing.T) {
	t.Parallel()
	var s *StreamStatus
	assert.True(t, s.IsNormalEnd())
}

func TestStreamStatus_Summary(t *testing.T) {
	t.Parallel()

	s := NewStreamStatus()
	s.SetEndReason(StreamEndReasonDone, nil)
	summary := s.Summary()
	assert.Contains(t, summary, "reason=done")
	assert.NotContains(t, summary, "soft_errors")

	s2 := NewStreamStatus()
	s2.SetEndReason(StreamEndReasonTimeout, nil)
	s2.RecordError("bad json")
	s2.RecordError("write failed")
	summary2 := s2.Summary()
	assert.Contains(t, summary2, "reason=timeout")
	assert.Contains(t, summary2, "soft_errors=2")
}

func TestStreamStatusProtocolTerminal(t *testing.T) {
	status := NewStreamStatus()
	assert.False(t, status.ProtocolTerminalObserved())
	status.MarkProtocolTerminal()
	assert.True(t, status.ProtocolTerminalObserved())

	var nilStatus *StreamStatus
	nilStatus.MarkProtocolTerminal()
	assert.False(t, nilStatus.ProtocolTerminalObserved())
}

func TestStreamStatusProtocolTerminalRequiredRejectsEOFWithoutTerminal(t *testing.T) {
	status := NewStreamStatus()
	status.RequireProtocolTerminal()
	status.SetEndReason(StreamEndReasonEOF, nil)

	assert.True(t, status.ProtocolTerminalRequired())
	assert.False(t, status.ProtocolTerminalObserved())
	assert.False(t, status.IsNormalEnd())

	status.MarkProtocolTerminal()
	assert.True(t, status.IsNormalEnd())
}

func TestStreamStatusLocalFailureMakesCompletedStreamNonNormal(t *testing.T) {
	status := NewStreamStatus()
	status.MarkProtocolTerminal()
	status.SetEndReason(StreamEndReasonDone, nil)
	status.MarkLocalFailure()

	assert.False(t, status.IsNormalEnd())
}

func TestStreamStatusLocalFailureSurvivesTerminalMarker(t *testing.T) {
	status := NewStreamStatus()
	status.MarkDoneSentinel()
	status.SetEndReason(StreamEndReasonDone, nil)
	status.MarkLocalFailure()

	assert.True(t, status.DoneSentinelObserved())
	assert.Equal(t, StreamEndReasonDone, status.EndReason)
	assert.True(t, status.LocalFailureObserved(), "a later local failure must not be hidden by first-wins EndReason")

	var nilStatus *StreamStatus
	nilStatus.MarkLocalFailure()
	assert.False(t, nilStatus.LocalFailureObserved())
}

func TestIsTransientProviderStreamError(t *testing.T) {
	t.Parallel()
	assert.True(t, IsTransientProviderStreamError("overloaded_error", "", "", 200))
	assert.True(t, IsTransientProviderStreamError("", "", "temporary upstream timeout", 200))
	assert.True(t, IsTransientProviderStreamError("", "", "", 503))
	assert.True(t, IsTransientProviderStreamError("overloaded_error", "", "", 400), "an explicit transient type wins over a client status")
	assert.False(t, IsTransientProviderStreamError("invalid_request_error", "", "", 503), "a deterministic type must not become generic failover evidence")
	assert.False(t, IsTransientProviderStreamError("authentication_error", "", "", 500), "authentication evidence must not become generic failover evidence")
	assert.False(t, IsTransientProviderStreamError("AuthError", "", "credential rejected", 500), "OpenCode Go auth evidence must not become generic failover evidence")
	assert.False(t, IsTransientProviderStreamError("ModelError", "", "model is unavailable", 500), "OpenCode Go model evidence must not become generic failover evidence")
	assert.False(t, IsTransientProviderStreamError("MonthlyLimitError", "", "", 500), "OpenCode Go quota evidence must not become generic failover evidence")
	assert.True(t, IsTransientProviderStreamError("FreeUsageLimitError", "", "", 200), "OpenCode Go RPM evidence remains transient")
	assert.False(t, IsTransientProviderStreamError("invalid_request_error", "invalid_request", "bad tool schema", 400))
	assert.False(t, IsTransientProviderStreamError("authentication_error", "", "invalid credential", 401))
	assert.False(t, IsTransientProviderStreamError("", "invalid_prompt", "", 500), "Responses code-only prompt errors are deterministic")
	assert.False(t, IsTransientProviderStreamError("", "input_too_long", "", 500), "Responses code-only length errors are deterministic")
	assert.True(t, IsTransientProviderStreamError("", "server_error", "", 200), "Responses code-only server errors are transient")
	assert.True(t, IsTransientProviderStreamError("", "rate_limit_exceeded", "", 200), "Responses code-only rate-limit errors are transient")
	assert.False(t, IsTransientProviderStreamError("", "", "", 501), "non-generic 5xx statuses are not generic failover evidence")
	assert.False(t, IsTransientProviderStreamError("", "", "", 505), "non-generic 5xx statuses are not generic failover evidence")
}

func TestStreamStatus_Summary_NilSafe(t *testing.T) {
	t.Parallel()
	var s *StreamStatus
	assert.Equal(t, "StreamStatus<nil>", s.Summary())
}
