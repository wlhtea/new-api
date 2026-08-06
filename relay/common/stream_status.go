package common

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type StreamEndReason string

const (
	StreamEndReasonNone        StreamEndReason = ""
	StreamEndReasonDone        StreamEndReason = "done"
	StreamEndReasonTimeout     StreamEndReason = "timeout"
	StreamEndReasonClientGone  StreamEndReason = "client_gone"
	StreamEndReasonScannerErr  StreamEndReason = "scanner_error"
	StreamEndReasonHandlerStop StreamEndReason = "handler_stop"
	StreamEndReasonEOF         StreamEndReason = "eof"
	StreamEndReasonPanic       StreamEndReason = "panic"
	StreamEndReasonPingFail    StreamEndReason = "ping_fail"
)

const maxStreamErrorEntries = 20

type StreamErrorEntry struct {
	Message   string
	Timestamp time.Time
}

type StreamStatus struct {
	EndReason StreamEndReason
	EndError  error
	endOnce   sync.Once

	mu               sync.Mutex
	Errors           []StreamErrorEntry
	ErrorCount       int
	terminal         bool
	terminalRequired bool
	done             bool
	upstream         bool
	localFailure     bool
}

func NewStreamStatus() *StreamStatus {
	return &StreamStatus{}
}

func (s *StreamStatus) SetEndReason(reason StreamEndReason, err error) {
	if s == nil {
		return
	}
	s.endOnce.Do(func() {
		s.mu.Lock()
		s.EndReason = reason
		s.EndError = err
		s.mu.Unlock()
	})
}

func (s *StreamStatus) RecordError(msg string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ErrorCount++
	if len(s.Errors) < maxStreamErrorEntries {
		s.Errors = append(s.Errors, StreamErrorEntry{
			Message:   msg,
			Timestamp: time.Now(),
		})
	}
}

func (s *StreamStatus) HasErrors() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ErrorCount > 0
}

func (s *StreamStatus) TotalErrorCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ErrorCount
}

func (s *StreamStatus) MarkProtocolTerminal() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.terminal = true
	s.mu.Unlock()
}

// RequireProtocolTerminal marks streams whose protocol has an explicit
// terminal event (for example Responses `response.completed` or Messages
// `message_stop`). EOF by itself is not a successful end for those streams.
func (s *StreamStatus) RequireProtocolTerminal() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.terminalRequired = true
	s.mu.Unlock()
}

func (s *StreamStatus) ProtocolTerminalRequired() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminalRequired
}

func (s *StreamStatus) ProtocolTerminalObserved() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal
}

func (s *StreamStatus) MarkDoneSentinel() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
}

func (s *StreamStatus) DoneSentinelObserved() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

func (s *StreamStatus) MarkUpstreamFailure() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.upstream = true
	s.mu.Unlock()
}

func (s *StreamStatus) UpstreamFailureObserved() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upstream
}

// MarkLocalFailure records a failure in the relay itself (for example a
// client write, ping, or handler panic). It is kept separately from the
// terminal reason because the terminal marker may have won the first-wins
// race before the local failure is observed.
func (s *StreamStatus) MarkLocalFailure() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.localFailure = true
	s.mu.Unlock()
}

func (s *StreamStatus) LocalFailureObserved() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.localFailure
}

var deterministicStreamErrorMarkers = [...]string{
	"invalid_request",
	"invalid request",
	"invalidargument",
	"invalid_prompt",
	"invalid prompt",
	"invalid_parameter",
	"invalid parameter",
	"invalid_value",
	"invalid value",
	"missing_required",
	"missing required",
	"bad_request",
	"bad request",
	"badrequest",
	"validation_error",
	"validation error",
	"validationerror",
	"authentication",
	"authenticationerror",
	"autherror",
	"auth_error",
	"unauthorized",
	"invalid_api_key",
	"invalid api key",
	"credential",
	"credentialerror",
	"permission",
	"permissionerror",
	"forbidden",
	"forbiddenerror",
	"not_found",
	"not found",
	"notfounderror",
	"model_not_found",
	"modelnotfound",
	"modelerror",
	"model_error",
	"context_length",
	"context length",
	"input_too_long",
	"input too long",
	"content_filter",
	"content policy",
	"safety",
	"region",
	"regionerror",
	"region_error",
	"risk",
	"quota",
	"quotaerror",
	"credit",
	"creditserror",
	"credits_error",
	"monthlylimiterror",
	"monthly_limit_error",
	"userlimiterror",
	"user_limit_error",
	"gousagelimiterror",
	"go_usage_limit_error",
	"blackusagelimiterror",
	"black_usage_limit_error",
	"subscription",
	"tool schema",
	"unsupported",
}

var transientStreamErrorMarkers = [...]string{
	"overload",
	"overloadederror",
	"rate_limit",
	"rate-limit",
	"ratelimit",
	"ratelimiterror",
	"freeusagelimiterror",
	"free_usage_limit_error",
	"server_error",
	"servererror",
	"internal_error",
	"internalerror",
	"internal server",
	"service_unavailable",
	"serviceunavailable",
	"temporar",
	"timeout",
	"timeouterror",
	"capacity",
	"too_many_requests",
	"try again",
	"api_error",
	"apierror",
	"gateway_error",
	"gatewayerror",
}

func hasStreamErrorMarker(value string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

// IsTransientProviderStreamError reports whether a structured upstream stream
// error is suitable as failover evidence. Request/auth/model errors must be
// surfaced to the caller without rotating an otherwise healthy workspace;
// overload, rate-limit, timeout, and server-side errors may be transient.
func IsTransientProviderStreamError(errorType, errorCode, message string, statusCode int) bool {
	typeCode := strings.ToLower(strings.Join([]string{errorType, errorCode}, " "))
	message = strings.ToLower(message)

	// A structured provider classification is stronger than the transport
	// status. A deterministic error can be wrapped in an HTTP 500 by a gateway;
	// that must not become generic failover evidence.
	if hasStreamErrorMarker(typeCode, deterministicStreamErrorMarkers[:]) {
		return false
	}
	if hasStreamErrorMarker(typeCode, transientStreamErrorMarkers[:]) {
		return true
	}
	if hasStreamErrorMarker(message, deterministicStreamErrorMarkers[:]) {
		return false
	}
	if hasStreamErrorMarker(message, transientStreamErrorMarkers[:]) {
		return true
	}
	return statusCode == http.StatusTooManyRequests || statusCode == http.StatusRequestTimeout ||
		statusCode == http.StatusTooEarly || statusCode == http.StatusInternalServerError ||
		statusCode == http.StatusBadGateway || statusCode == http.StatusServiceUnavailable ||
		statusCode == http.StatusGatewayTimeout
}

func (s *StreamStatus) IsNormalEnd() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	reason := s.EndReason
	terminalRequired := s.terminalRequired
	terminal := s.terminal
	localFailure := s.localFailure
	s.mu.Unlock()
	if localFailure || (terminalRequired && !terminal) {
		return false
	}
	return reason == StreamEndReasonDone ||
		reason == StreamEndReasonEOF ||
		reason == StreamEndReasonHandlerStop
}

func (s *StreamStatus) Summary() string {
	if s == nil {
		return "StreamStatus<nil>"
	}
	b := &strings.Builder{}
	s.mu.Lock()
	reason := s.EndReason
	endErr := s.EndError
	errorCount := s.ErrorCount
	s.mu.Unlock()
	fmt.Fprintf(b, "reason=%s", reason)
	if endErr != nil {
		fmt.Fprintf(b, " end_error=%q", endErr.Error())
	}
	if errorCount > 0 {
		fmt.Fprintf(b, " soft_errors=%d", errorCount)
	}
	return b.String()
}
