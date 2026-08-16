package helper

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
	if constant.StreamingTimeout == 0 {
		constant.StreamingTimeout = 30
	}
}

func setupStreamTest(t *testing.T, body io.Reader) (*gin.Context, *http.Response, *relaycommon.RelayInfo) {
	t.Helper()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	responseBody, ok := body.(io.ReadCloser)
	if !ok {
		responseBody = io.NopCloser(body)
	}
	resp := &http.Response{Body: responseBody}

	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{},
	}

	return c, resp, info
}

type deadlineTrackingWriter struct {
	header   http.Header
	deadline time.Time
}

func (w *deadlineTrackingWriter) Header() http.Header {
	return w.header
}

func (w *deadlineTrackingWriter) Write(data []byte) (int, error) {
	return len(data), nil
}

func (w *deadlineTrackingWriter) WriteHeader(int) {}

func (w *deadlineTrackingWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func TestExtendWriteDeadlineUsesBoundedPerWriteDeadline(t *testing.T) {
	writer := &deadlineTrackingWriter{header: make(http.Header)}
	c, _ := gin.CreateTestContext(writer)
	startedAt := time.Now()

	ExtendWriteDeadline(c)

	require.False(t, writer.deadline.IsZero())
	assert.GreaterOrEqual(t, writer.deadline.Sub(startedAt), streamWriteTimeout-time.Second)
	assert.LessOrEqual(t, writer.deadline.Sub(startedAt), streamWriteTimeout+time.Second)
}

func buildSSEBody(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "data: {\"id\":%d,\"choices\":[{\"delta\":{\"content\":\"token_%d\"}}]}\n", i, i)
	}
	b.WriteString("data: [DONE]\n")
	return b.String()
}

// ---------- Basic correctness ----------

func TestStreamScannerHandler_NilInputs(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}

	StreamScannerHandler(c, nil, info, func(data string, sr *StreamResult) {})
	StreamScannerHandler(c, &http.Response{Body: io.NopCloser(strings.NewReader(""))}, info, nil)
}

func TestNewStreamScanner_AllowsLargeStreamLine(t *testing.T) {
	oldBufferMB := constant.StreamScannerMaxBufferMB
	constant.StreamScannerMaxBufferMB = 1
	t.Cleanup(func() {
		constant.StreamScannerMaxBufferMB = oldBufferMB
	})

	payload := strings.Repeat("x", 128<<10)
	scanner := NewStreamScanner(strings.NewReader("data: " + payload + "\n"))
	scanner.Split(bufio.ScanLines)

	require.True(t, scanner.Scan())
	assert.Equal(t, "data: "+payload, scanner.Text())
	require.NoError(t, scanner.Err())
}

func TestNewStreamScannerWithLimitRejectsOneByteOver(t *testing.T) {
	for _, test := range []struct {
		name     string
		lineSize int
		wantScan bool
	}{
		{name: "at limit", lineSize: 32, wantScan: true},
		{name: "one byte over", lineSize: 33, wantScan: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			scanner := newStreamScannerWithLimit(strings.NewReader(strings.Repeat("x", test.lineSize)), 32)
			scanner.Split(bufio.ScanLines)

			assert.Equal(t, test.wantScan, scanner.Scan())
			if test.wantScan {
				require.NoError(t, scanner.Err())
				assert.Len(t, scanner.Text(), test.lineSize)
				return
			}
			require.Error(t, scanner.Err())
		})
	}
}

func TestOpenCodeStreamQueueHasExplicitByteCeiling(t *testing.T) {
	options := normalizedStreamScannerOptions(streamScannerOptions{
		maxLineBytes:   64,
		maxEventBytes:  16,
		maxQueuedBytes: 32,
		queueItems:     10,
		idleTimeout:    time.Second,
	})

	assert.Equal(t, 2, options.queueItems)
	assert.LessOrEqual(t, options.queueItems*options.maxEventBytes, options.maxQueuedBytes)
}

func TestStreamScannerHandler_EmptyBody(t *testing.T) {
	t.Parallel()

	c, resp, info := setupStreamTest(t, strings.NewReader(""))

	var called atomic.Bool
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		called.Store(true)
	})

	assert.False(t, called.Load(), "handler should not be called for empty body")
}

func TestStreamScannerHandler_1000Chunks(t *testing.T) {
	t.Parallel()

	const numChunks = 1000
	body := buildSSEBody(numChunks)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	var count atomic.Int64
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		count.Add(1)
	})

	assert.Equal(t, int64(numChunks), count.Load())
	assert.Equal(t, numChunks, info.ReceivedResponseCount)
}

func TestStreamScannerHandler_OrderPreserved(t *testing.T) {
	t.Parallel()

	const numChunks = 500
	body := buildSSEBody(numChunks)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	var mu sync.Mutex
	received := make([]string, 0, numChunks)

	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		mu.Lock()
		received = append(received, data)
		mu.Unlock()
	})

	require.Equal(t, numChunks, len(received))
	for i := 0; i < numChunks; i++ {
		expected := fmt.Sprintf("{\"id\":%d,\"choices\":[{\"delta\":{\"content\":\"token_%d\"}}]}", i, i)
		assert.Equal(t, expected, received[i], "chunk %d out of order", i)
	}
}

func TestStreamScannerHandler_DoneStopsScanner(t *testing.T) {
	t.Parallel()

	body := buildSSEBody(50) + "data: should_not_appear\n"
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	var count atomic.Int64
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		count.Add(1)
	})

	assert.Equal(t, int64(50), count.Load(), "data after [DONE] must not be processed")
}

func TestStreamScannerHandler_StopStopsStream(t *testing.T) {
	t.Parallel()

	const numChunks = 200
	body := buildSSEBody(numChunks)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	const stopAt int64 = 50
	var count atomic.Int64
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		n := count.Add(1)
		if n >= stopAt {
			sr.Stop(fmt.Errorf("fatal at %d", n))
		}
	})

	assert.Equal(t, stopAt, count.Load())
	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonHandlerStop, info.StreamStatus.EndReason)
}

func TestStreamScannerHandler_SkipsNonDataLines(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	b.WriteString(": comment line\n")
	b.WriteString("event: message\n")
	b.WriteString("id: 12345\n")
	b.WriteString("retry: 5000\n")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "data: payload_%d\n", i)
		b.WriteString(": interleaved comment\n")
	}
	b.WriteString("data: [DONE]\n")

	c, resp, info := setupStreamTest(t, strings.NewReader(b.String()))

	var count atomic.Int64
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		count.Add(1)
	})

	assert.Equal(t, int64(100), count.Load())
}

func TestStreamScannerHandler_DataWithExtraSpaces(t *testing.T) {
	t.Parallel()

	body := "data:   {\"trimmed\":true}  \ndata: [DONE]\n"
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	var got string
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		got = data
	})

	assert.Equal(t, "{\"trimmed\":true}", got)
}

func TestStreamScannerHandler_BareDoneSentinel(t *testing.T) {
	t.Parallel()

	body := "data: {\"payload\":true}\n[DONE]\n"
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	var received []string
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		received = append(received, data)
	})

	require.Equal(t, []string{"{\"payload\":true}"}, received)
	require.NotNil(t, info.StreamStatus)
	assert.True(t, info.StreamStatus.DoneSentinelObserved())
	assert.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
}

func TestStreamScannerHandler_DoesNotAcceptSentinelPrefix(t *testing.T) {
	t.Parallel()

	c, resp, info := setupStreamTest(t, strings.NewReader("data: [DONE]garbage\n"))
	var received string
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		received = data
	})

	assert.Equal(t, "[DONE]garbage", received)
	require.NotNil(t, info.StreamStatus)
	assert.False(t, info.StreamStatus.DoneSentinelObserved())
}

// TestStreamScannerHandler_ClientCancelAbortsUpstreamAndReturns pins the
// disconnect contract: when the client goes away, the handler must return
// promptly (all goroutines joined, so the gin.Context can never leak into a
// pooled reuse), the upstream body must be closed to stop token generation,
// and no data received after the disconnect may be processed or written.
func TestStreamScannerHandler_ClientCancelAbortsUpstreamAndReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pr, pw := io.Pipe()
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)

	resp := &http.Response{Body: pr}
	info := &relaycommon.RelayInfo{
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{},
	}

	var count atomic.Int64
	firstHandled := make(chan struct{})
	done := make(chan struct{})
	go func() {
		StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
			count.Add(1)
			_ = StringData(c, data)
			if data == "first" {
				close(firstHandled)
			}
		})
		close(done)
	}()

	_, err := fmt.Fprint(pw, "data: first\n")
	require.NoError(t, err)

	select {
	case <-firstHandled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first chunk")
	}

	cancel()

	// The handler must return without any further upstream input: cleanup
	// closes resp.Body, which unblocks the scanner goroutine.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after client disconnect")
	}

	// Upstream read side must be closed so the provider stops generating
	// (and billing) for a request nobody is listening to.
	_, err = fmt.Fprint(pw, "data: second\n")
	require.ErrorIs(t, err, io.ErrClosedPipe, "upstream body should be closed after client disconnect")

	assert.Equal(t, int64(1), count.Load(), "no chunk after disconnect should be processed")
	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonClientGone, info.StreamStatus.EndReason)

	body := recorder.Body.String()
	assert.Contains(t, body, "first")
	assert.NotContains(t, body, "second")
}

// ---------- Ping tests ----------

func TestStreamScannerHandler_PingSentDuringSlowUpstream(t *testing.T) {
	setting := operation_setting.GetGeneralSetting()
	oldEnabled := setting.PingIntervalEnabled
	oldSeconds := setting.PingIntervalSeconds
	setting.PingIntervalEnabled = true
	setting.PingIntervalSeconds = 1
	t.Cleanup(func() {
		setting.PingIntervalEnabled = oldEnabled
		setting.PingIntervalSeconds = oldSeconds
	})

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for i := 0; i < 4; i++ {
			fmt.Fprintf(pw, "data: chunk_%d\n", i)
			time.Sleep(400 * time.Millisecond)
		}
		fmt.Fprint(pw, "data: [DONE]\n")
	}()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	resp := &http.Response{Body: pr}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}

	var count atomic.Int64
	done := make(chan struct{})
	go func() {
		StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
			count.Add(1)
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stream to finish")
	}

	assert.Equal(t, int64(4), count.Load())

	body := recorder.Body.String()
	pingCount := strings.Count(body, ": PING")
	assert.GreaterOrEqual(t, pingCount, 1,
		"expected at least 1 ping during slow stream with 1s interval; got %d", pingCount)
}

func TestStreamScannerHandler_PingDisabledByRelayInfo(t *testing.T) {
	setting := operation_setting.GetGeneralSetting()
	oldEnabled := setting.PingIntervalEnabled
	oldSeconds := setting.PingIntervalSeconds
	setting.PingIntervalEnabled = true
	setting.PingIntervalSeconds = 1
	t.Cleanup(func() {
		setting.PingIntervalEnabled = oldEnabled
		setting.PingIntervalSeconds = oldSeconds
	})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	resp := &http.Response{Body: io.NopCloser(strings.NewReader(buildSSEBody(5)))}
	info := &relaycommon.RelayInfo{
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{},
	}

	var count atomic.Int64
	done := make(chan struct{})
	go func() {
		StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
			count.Add(1)
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}

	assert.Equal(t, int64(5), count.Load())

	body := recorder.Body.String()
	pingCount := strings.Count(body, ": PING")
	assert.Equal(t, 0, pingCount, "pings should be disabled when DisablePing=true")
}

// ---------- StreamStatus integration ----------

func TestStreamScannerHandler_StreamStatus_DoneReason(t *testing.T) {
	t.Parallel()

	body := buildSSEBody(10)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {})

	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	assert.Nil(t, info.StreamStatus.EndError)
	assert.True(t, info.StreamStatus.IsNormalEnd())
	assert.False(t, info.StreamStatus.HasErrors())
}

func TestStreamScannerHandler_StreamStatus_EOFWithoutDone(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&b, "data: {\"id\":%d}\n", i)
	}
	c, resp, info := setupStreamTest(t, strings.NewReader(b.String()))

	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {})

	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonEOF, info.StreamStatus.EndReason)
	assert.True(t, info.StreamStatus.IsNormalEnd())
}

func TestStreamScannerHandler_RequiredProtocolTerminalRejectsDoneSentinelOnly(t *testing.T) {
	t.Parallel()

	c, resp, info := setupStreamTest(t, strings.NewReader("data: {\"id\":1}\ndata: [DONE]\n"))
	info.StreamProtocolTerminalRequired = true

	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {})

	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	assert.True(t, info.StreamStatus.ProtocolTerminalRequired())
	assert.False(t, info.StreamStatus.ProtocolTerminalObserved())
	assert.False(t, info.StreamStatus.IsNormalEnd())
}

func TestStreamScannerHandler_RequiredProtocolTerminalAcceptsTerminalEvent(t *testing.T) {
	t.Parallel()

	c, resp, info := setupStreamTest(t, strings.NewReader("data: {\"type\":\"response.completed\"}\ndata: [DONE]\n"))
	info.StreamProtocolTerminalRequired = true

	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		info.StreamStatus.MarkProtocolTerminal()
	})

	require.NotNil(t, info.StreamStatus)
	assert.True(t, info.StreamStatus.ProtocolTerminalObserved())
	assert.True(t, info.StreamStatus.IsNormalEnd())
}

func TestStreamScannerHandler_StreamStatus_HandlerStop(t *testing.T) {
	t.Parallel()

	body := buildSSEBody(100)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	var count atomic.Int64
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		n := count.Add(1)
		if n >= 10 {
			sr.Stop(fmt.Errorf("stop at 10"))
		}
	})

	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonHandlerStop, info.StreamStatus.EndReason)
	assert.True(t, info.StreamStatus.HasErrors())
	assert.True(t, info.StreamStatus.LocalFailureObserved())
}

func TestStreamScannerHandler_StreamStatus_HandlerDone(t *testing.T) {
	t.Parallel()

	body := buildSSEBody(20)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	var count atomic.Int64
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		n := count.Add(1)
		if n >= 5 {
			sr.Done()
		}
	})

	assert.Equal(t, int64(5), count.Load())
	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	assert.False(t, info.StreamStatus.HasErrors())
}

func TestStreamScannerHandler_StreamStatus_Timeout(t *testing.T) {
	// Not parallel: modifies global constant.StreamingTimeout
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 1
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	pr, pw := io.Pipe()
	go func() {
		fmt.Fprint(pw, "data: {\"id\":1}\n")
		time.Sleep(2 * time.Second)
		pw.Close()
	}()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	resp := &http.Response{Body: pr}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}

	done := make(chan struct{})
	go func() {
		StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stream timeout")
	}

	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonTimeout, info.StreamStatus.EndReason)
	assert.False(t, info.StreamStatus.IsNormalEnd())
}

func TestOpenCodeStreamIdleIgnoresBlankAndCommentLines(t *testing.T) {
	pr, pw := io.Pipe()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer pw.Close()
		for i := 0; i < 30; i++ {
			if _, err := fmt.Fprint(pw, ": keepalive\n\n"); err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	c, resp, info := setupStreamTest(t, pr)
	info.ChannelMeta.ChannelType = constant.ChannelTypeOpenCodeAPIKey
	started := time.Now()
	streamScannerHandlerWithOptions(c, resp, info, func(data string, sr *StreamResult) {
		t.Fatalf("comment traffic must not reach the semantic handler: %q", data)
	}, streamScannerOptions{
		maxLineBytes:   128,
		maxEventBytes:  64,
		maxQueuedBytes: 64,
		queueItems:     10,
		idleTimeout:    35 * time.Millisecond,
		semanticIdle:   true,
	})

	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonTimeout, info.StreamStatus.EndReason)
	assert.Less(t, time.Since(started), 120*time.Millisecond)
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("comment writer did not stop after stream timeout")
	}
}

func TestOpenCodeStreamSemanticEventsRenewIdleDeadline(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for i := 0; i < 4; i++ {
			_, _ = fmt.Fprintf(pw, "data: {\"id\":%d}\n", i)
			time.Sleep(15 * time.Millisecond)
		}
		_, _ = fmt.Fprint(pw, "data: [DONE]\n")
	}()

	c, resp, info := setupStreamTest(t, pr)
	info.ChannelMeta.ChannelType = constant.ChannelTypeOpenCodeAPIKey
	var count atomic.Int64
	streamScannerHandlerWithOptions(c, resp, info, func(data string, sr *StreamResult) {
		count.Add(1)
	}, streamScannerOptions{
		maxLineBytes:   128,
		maxEventBytes:  64,
		maxQueuedBytes: 64,
		queueItems:     10,
		idleTimeout:    30 * time.Millisecond,
		semanticIdle:   true,
	})

	assert.Equal(t, int64(4), count.Load())
	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
}

func TestOpenCodeStreamEventLimitExactAndOneByteOver(t *testing.T) {
	for _, test := range []struct {
		name          string
		eventBytes    int
		wantHandled   bool
		wantEndReason relaycommon.StreamEndReason
	}{
		{name: "at limit", eventBytes: 8, wantHandled: true, wantEndReason: relaycommon.StreamEndReasonDone},
		{name: "one byte over", eventBytes: 9, wantEndReason: relaycommon.StreamEndReasonScannerErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := "data: " + strings.Repeat("x", test.eventBytes) + "\ndata: [DONE]\n"
			c, resp, info := setupStreamTest(t, strings.NewReader(body))
			info.ChannelMeta.ChannelType = constant.ChannelTypeOpenCodeAPIKey
			var handled atomic.Bool

			streamScannerHandlerWithOptions(c, resp, info, func(data string, sr *StreamResult) {
				handled.Store(true)
			}, streamScannerOptions{
				maxLineBytes:   64,
				maxEventBytes:  8,
				maxQueuedBytes: 8,
				queueItems:     10,
				idleTimeout:    time.Second,
				semanticIdle:   true,
			})

			assert.Equal(t, test.wantHandled, handled.Load())
			require.NotNil(t, info.StreamStatus)
			assert.Equal(t, test.wantEndReason, info.StreamStatus.EndReason)
		})
	}
}

func TestStreamScannerHandler_StreamStatus_SoftErrors(t *testing.T) {
	t.Parallel()

	body := buildSSEBody(10)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		sr.Error(fmt.Errorf("soft error for chunk"))
	})

	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	assert.True(t, info.StreamStatus.HasErrors())
	assert.Equal(t, 10, info.StreamStatus.TotalErrorCount())
}

func TestStreamScannerHandler_StreamStatus_MultipleErrorsPerChunk(t *testing.T) {
	t.Parallel()

	body := buildSSEBody(5)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		sr.Error(fmt.Errorf("error A"))
		sr.Error(fmt.Errorf("error B"))
	})

	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	assert.Equal(t, 10, info.StreamStatus.TotalErrorCount())
}

func TestStreamScannerHandler_StreamStatus_ErrorThenStop(t *testing.T) {
	t.Parallel()

	// Use a large body without [DONE] to avoid race between scanner's [DONE]
	// and handler's Stop on the sync.Once EndReason.
	var b strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "data: {\"id\":%d}\n", i)
	}
	c, resp, info := setupStreamTest(t, strings.NewReader(b.String()))

	var count atomic.Int64
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		count.Add(1)
		sr.Error(fmt.Errorf("soft error"))
		sr.Stop(fmt.Errorf("fatal"))
	})

	assert.Equal(t, int64(1), count.Load())
	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonHandlerStop, info.StreamStatus.EndReason)
	assert.Equal(t, 2, info.StreamStatus.TotalErrorCount())
}

func TestStreamScannerHandler_StreamStatus_InitializedIfNil(t *testing.T) {
	t.Parallel()

	body := buildSSEBody(1)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	assert.Nil(t, info.StreamStatus)

	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {})

	assert.NotNil(t, info.StreamStatus)
}

func TestStreamScannerHandler_StreamStatus_ReplacesPreInitialized(t *testing.T) {
	t.Parallel()

	body := buildSSEBody(5)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	info.StreamStatus = relaycommon.NewStreamStatus()
	info.StreamStatus.RecordError("pre-existing error")

	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {})

	assert.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	assert.Equal(t, 0, info.StreamStatus.TotalErrorCount())
}

func TestStreamScannerHandlerOpenCodeGoUsesOnlyLocalSSEHeaders(t *testing.T) {
	tests := []struct {
		name               string
		channelType        int
		wantCodexExtension bool
	}{
		{name: "open_code_go", channelType: constant.ChannelTypeOpenCodeGo},
		{name: "other_channel", channelType: constant.ChannelTypeOpenAI, wantCodexExtension: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, resp, info := setupStreamTest(t, strings.NewReader("data: [DONE]\n"))
			common.SetContextKey(c, constant.ContextKeyChannelType, test.channelType)
			resp.Header = http.Header{
				"Content-Type":         []string{"text/event-stream; workspace=wrk_private"},
				"X-Codex-Turn-State":   []string{"internal-turn-state"},
				"X-Reasoning-Included": []string{"workspace=wrk_private"},
				"X-Internal-Shard":     []string{"zen-primary"},
			}

			StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {})

			header := c.Writer.Header()
			assert.Equal(t, "text/event-stream", header.Get("Content-Type"))
			assert.Equal(t, "no-cache", header.Get("Cache-Control"))
			assert.Equal(t, "keep-alive", header.Get("Connection"))
			assert.Equal(t, "chunked", header.Get("Transfer-Encoding"))
			assert.Equal(t, "no", header.Get("X-Accel-Buffering"))
			assert.Empty(t, header.Get("X-Internal-Shard"))
			if test.wantCodexExtension {
				assert.Equal(t, "internal-turn-state", header.Get("X-Codex-Turn-State"))
				assert.Equal(t, "workspace=wrk_private", header.Get("X-Reasoning-Included"))
			} else {
				assert.Empty(t, header.Get("X-Codex-Turn-State"))
				assert.Empty(t, header.Get("X-Reasoning-Included"))
			}
		})
	}
}

func TestStreamScannerHandlerOmitsOpenCodeSSEDataFromDebugLog(t *testing.T) {
	previousDebug := common.DebugEnabled
	previousWriter := gin.DefaultErrorWriter
	var logs bytes.Buffer
	common.DebugEnabled = true
	common.LogWriterMu.Lock()
	gin.DefaultErrorWriter = &logs
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.DebugEnabled = previousDebug
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = previousWriter
		common.LogWriterMu.Unlock()
	})

	const payload = `{"type":"error","error":{"message":"Console Go Authorization: Bearer stream-private-key; Cookie: session=private-session; x-opencode-session=private-session; proxy=socks5://proxy-user:proxy-pass@proxy.internal:1080; endpoint=http://internal-control.local/private"}}`
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		t.Run(constant.GetChannelTypeName(channelType), func(t *testing.T) {
			logs.Reset()
			c, resp, info := setupStreamTest(t, strings.NewReader("data: "+payload+"\n"))
			info.ChannelMeta.ChannelType = channelType
			var received string
			StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
				received = data
			})

			assert.Contains(t, received, "stream-private-key")
			assert.Contains(t, received, "private-session")
			assert.Contains(t, logs.String(), "stream scanner data omitted")
			assert.NotContains(t, logs.String(), "Console Go")
			assert.NotContains(t, logs.String(), "stream-private-key")
			assert.NotContains(t, logs.String(), "private-session")
			assert.NotContains(t, logs.String(), "proxy-user")
			assert.NotContains(t, logs.String(), "proxy.internal")
			assert.NotContains(t, logs.String(), "internal-control.local")
		})
	}

	logs.Reset()
	c, resp, info := setupStreamTest(t, strings.NewReader("data: "+payload+"\n"))
	info.ChannelMeta.ChannelType = constant.ChannelTypeOpenAI
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {})
	assert.Contains(t, logs.String(), payload, "non-OpenCode debug logging must remain unchanged")
}
