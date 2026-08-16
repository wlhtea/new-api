package helper

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/bytedance/gopkg/util/gopool"

	"github.com/gin-gonic/gin"
)

const (
	InitialScannerBufferSize    = 64 << 10  // 64KB (64*1024)
	DefaultMaxScannerBufferSize = 128 << 20 // 64MB (64*1024*1024) default SSE buffer size
	DefaultPingInterval         = 10 * time.Second
	// streamWriteTimeout bounds a single blocked write to a slow client so the
	// unconditional wg.Wait() in cleanup can always finish. Without it, a slow
	// but connected client (full TCP buffer, no server WriteTimeout) could hang
	// the handler forever.
	streamWriteTimeout = 30 * time.Second
)

type streamScannerOptions struct {
	maxLineBytes   int
	maxEventBytes  int
	maxQueuedBytes int
	queueItems     int
	idleTimeout    time.Duration
	semanticIdle   bool
}

func getScannerBufferSize() int {
	if constant.StreamScannerMaxBufferMB > 0 {
		return constant.StreamScannerMaxBufferMB << 20
	}
	return DefaultMaxScannerBufferSize
}

func NewStreamScanner(reader io.Reader) *bufio.Scanner {
	return newStreamScannerWithLimit(reader, getScannerBufferSize())
}

func newStreamScannerWithLimit(reader io.Reader, maxBytes int) *bufio.Scanner {
	if maxBytes <= 0 {
		maxBytes = getScannerBufferSize()
	}
	// Scanner's max token size includes the byte needed to discover a delimiter
	// or EOF. Reserve that one probe byte so a token exactly at the semantic
	// limit is accepted while limit+1 is still rejected.
	scannerMaxBytes := maxBytes
	if scannerMaxBytes < int(^uint(0)>>1) {
		scannerMaxBytes++
	}
	scanner := bufio.NewScanner(reader)
	initialBytes := min(InitialScannerBufferSize, scannerMaxBytes)
	scanner.Buffer(make([]byte, initialBytes), scannerMaxBytes)
	return scanner
}

func streamScannerOptionsForInfo(info *relaycommon.RelayInfo) streamScannerOptions {
	options := streamScannerOptions{
		maxLineBytes: getScannerBufferSize(),
		queueItems:   10,
	}
	if info != nil && constant.IsOpenCodeChannelType(info.GetChannelType()) {
		options.maxLineBytes = min(options.maxLineBytes, constant.OpenCodeGoMaxSSEEventBytes)
		options.maxEventBytes = constant.OpenCodeGoMaxSSEEventBytes
		options.maxQueuedBytes = constant.OpenCodeGoMaxSSEQueuedBytes
		options.semanticIdle = true
	}
	return options
}

func normalizedStreamScannerOptions(options streamScannerOptions) streamScannerOptions {
	if options.maxLineBytes <= 0 {
		options.maxLineBytes = getScannerBufferSize()
	}
	if options.queueItems <= 0 {
		options.queueItems = 10
	}
	if options.maxEventBytes > 0 && options.maxQueuedBytes > 0 {
		options.maxEventBytes = min(options.maxEventBytes, options.maxQueuedBytes)
		byteBoundedItems := options.maxQueuedBytes / options.maxEventBytes
		if byteBoundedItems < 1 {
			byteBoundedItems = 1
		}
		options.queueItems = min(options.queueItems, byteBoundedItems)
	}
	if options.idleTimeout <= 0 {
		options.idleTimeout = time.Duration(constant.StreamingTimeout) * time.Second
	}
	if options.idleTimeout <= 0 {
		options.idleTimeout = 5 * time.Minute
	}
	return options
}

func copyCodexSSEHeaders(c *gin.Context, resp *http.Response) {
	if c == nil || c.Writer == nil || resp == nil {
		return
	}
	// codex
	for _, name := range []string{"X-Reasoning-Included", "X-Codex-Turn-State"} {
		values := resp.Header.Values(name)
		if !service.ShouldCopyUpstreamHeader(c, name, values) {
			continue
		}
		for _, value := range values {
			if value != "" {
				c.Writer.Header().Add(name, value)
			}
		}
	}
}

// ExtendWriteDeadline pushes the connection write deadline forward before each
// stream write. Best-effort: writers that don't support deadlines (e.g.
// httptest recorders) are silently ignored.
func ExtendWriteDeadline(c *gin.Context) {
	if c == nil || c.Writer == nil {
		return
	}
	_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Now().Add(streamWriteTimeout))
}

func StreamScannerHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo, dataHandler func(data string, sr *StreamResult)) {
	streamScannerHandlerWithOptions(c, resp, info, dataHandler, streamScannerOptionsForInfo(info))
}

func streamScannerHandlerWithOptions(
	c *gin.Context,
	resp *http.Response,
	info *relaycommon.RelayInfo,
	dataHandler func(data string, sr *StreamResult),
	options streamScannerOptions,
) {

	if c == nil || info == nil || resp == nil || dataHandler == nil {
		return
	}
	options = normalizedStreamScannerOptions(options)

	// 无条件新建 StreamStatus
	info.StreamStatus = relaycommon.NewStreamStatus()
	if info.StreamProtocolTerminalRequired {
		info.StreamStatus.RequireProtocolTerminal()
	}

	ctx, cancel := context.WithCancel(context.Background())

	streamingTimeout := options.idleTimeout

	var (
		stopChan    = make(chan bool, 3) // 增加缓冲区避免阻塞
		scanner     = newStreamScannerWithLimit(resp.Body, options.maxLineBytes)
		ticker      = time.NewTicker(streamingTimeout)
		pingTicker  *time.Ticker
		writeMutex  sync.Mutex     // Mutex to protect concurrent writes
		wg          sync.WaitGroup // 用于等待所有 goroutine 退出
		cleanupOnce sync.Once
		stopOnce    sync.Once
	)

	stop := func() {
		stopOnce.Do(func() {
			close(stopChan)
		})
	}

	generalSettings := operation_setting.GetGeneralSetting()
	pingEnabled := generalSettings.PingIntervalEnabled && !info.DisablePing
	pingInterval := time.Duration(generalSettings.PingIntervalSeconds) * time.Second
	if pingInterval <= 0 {
		pingInterval = DefaultPingInterval
	}

	if pingEnabled {
		pingTicker = time.NewTicker(pingInterval)
	}

	logger.LogDebug(c, "relay timeout seconds: %d", common.RelayTimeout)
	logger.LogDebug(c, "relay max idle conns: %d", common.RelayMaxIdleConns)
	logger.LogDebug(c, "relay max idle conns per host: %d", common.RelayMaxIdleConnsPerHost)
	logger.LogDebug(c, "streaming timeout seconds: %d", int64(streamingTimeout.Seconds()))
	logger.LogDebug(c, "ping interval seconds: %d", int64(pingInterval.Seconds()))

	cleanup := func() {
		cleanupOnce.Do(func() {
			cancel()
			stop()
			if resp.Body != nil {
				_ = resp.Body.Close()
			}

			ticker.Stop()
			if pingTicker != nil {
				pingTicker.Stop()
			}

			wg.Wait()
		})
	}
	// Ensure gin.Context is not returned to Gin's pool while any stream goroutine can still use it.
	defer cleanup()

	scanner.Split(bufio.ScanLines)
	copyCodexSSEHeaders(c, resp)
	SetEventStreamHeaders(c)

	ctx = context.WithValue(ctx, "stop_chan", stopChan)

	// Handle ping data sending with improved error handling
	if pingEnabled && pingTicker != nil {
		wg.Add(1)
		gopool.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					logger.LogError(c, fmt.Sprintf("ping goroutine panic: %v", r))
					info.StreamStatus.MarkLocalFailure()
					info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonPanic, fmt.Errorf("ping panic: %v", r))
					stop()
				}
				logger.LogDebug(c, "ping goroutine exited")
				wg.Done()
			}()

			// 添加超时保护，防止 goroutine 无限运行
			maxPingDuration := 30 * time.Minute // 最大 ping 持续时间
			pingTimeout := time.NewTimer(maxPingDuration)
			defer pingTimeout.Stop()

			for {
				select {
				case <-pingTicker.C:
					var err error
					func() {
						writeMutex.Lock()
						defer writeMutex.Unlock()
						ExtendWriteDeadline(c)
						err = PingData(c)
					}()
					if err != nil {
						logger.LogError(c, "ping data error: "+err.Error())
						info.StreamStatus.MarkLocalFailure()
						info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonPingFail, err)
						return
					}
					logger.LogDebug(c, "ping data sent")
				case <-ctx.Done():
					return
				case <-stopChan:
					return
				case <-c.Request.Context().Done():
					// 监听客户端断开连接
					return
				case <-pingTimeout.C:
					logger.LogError(c, "ping goroutine max duration reached")
					return
				}
			}
		})
	}

	dataChan := make(chan string, options.queueItems)

	wg.Add(1)
	gopool.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				logger.LogError(c, fmt.Sprintf("data handler goroutine panic: %v", r))
				info.StreamStatus.MarkLocalFailure()
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonPanic, fmt.Errorf("handler panic: %v", r))
			}
			stop()
			wg.Done()
		}()
		sr := newStreamResult(info.StreamStatus)
		for data := range dataChan {
			sr.reset()
			func() {
				writeMutex.Lock()
				defer writeMutex.Unlock()
				ExtendWriteDeadline(c)
				dataHandler(data, sr)
			}()
			if sr.IsStopped() {
				return
			}
		}
	})

	// Scanner goroutine with improved error handling
	wg.Add(1)
	common.RelayCtxGo(ctx, func() {
		defer func() {
			close(dataChan)
			if r := recover(); r != nil {
				logger.LogError(c, fmt.Sprintf("scanner goroutine panic: %v", r))
				info.StreamStatus.MarkLocalFailure()
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonPanic, fmt.Errorf("scanner panic: %v", r))
			}
			stop()
			logger.LogDebug(c, "scanner goroutine exited")
			wg.Done()
		}()

		for scanner.Scan() {
			// 检查是否需要停止
			select {
			case <-stopChan:
				return
			case <-ctx.Done():
				return
			default:
			}

			rawData := strings.TrimSpace(scanner.Text())
			if !options.semanticIdle {
				ticker.Reset(streamingTimeout)
			}
			if info != nil && constant.IsOpenCodeChannelType(info.GetChannelType()) {
				// OpenCode response envelopes can contain upstream credentials,
				// proxy URLs, session identifiers, or private endpoints. The
				// protocol-specific handler classifies and sanitizes them later;
				// never write the raw SSE line to DEBUG logs here.
				logger.LogDebug(c, "stream scanner data omitted (bytes=%d)", len(rawData))
			} else {
				logger.LogDebug(c, "stream scanner data: %s", rawData)
			}

			var data string
			switch {
			case strings.HasPrefix(rawData, "data:"):
				data = strings.TrimSpace(rawData[len("data:"):])
			case rawData == "[DONE]":
				// Some gateways emit the sentinel without the SSE `data:`
				// prefix. Do not slice it as if the prefix were present.
				data = rawData
			default:
				continue
			}
			if data == "" {
				continue
			}
			if options.maxEventBytes > 0 && len(data) > options.maxEventBytes {
				err := fmt.Errorf("stream event exceeds %d-byte relay limit", options.maxEventBytes)
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, err)
				return
			}
			if options.semanticIdle {
				ticker.Reset(streamingTimeout)
			}
			if data != "[DONE]" {
				info.SetFirstResponseTime()
				info.ReceivedResponseCount++

				select {
				case dataChan <- data:
				case <-ctx.Done():
					return
				case <-stopChan:
					return
				}
			} else {
				info.StreamStatus.MarkDoneSentinel()
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
				logger.LogDebug(c, "received [DONE], stopping scanner")
				return
			}
		}

		if err := scanner.Err(); err != nil {
			if err != io.EOF {
				logger.LogError(c, "scanner error: "+err.Error())
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, err)
			}
		}
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonEOF, nil)
	})

	// 主循环等待完成或超时
	select {
	case <-ticker.C:
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonTimeout, nil)
	case <-stopChan:
		// EndReason already set by the goroutine that triggered stopChan
	case <-c.Request.Context().Done():
		// 客户端断开：立即 cleanup 关闭上游 resp.Body，解除 scanner 阻塞并让上游停止生成，
		// 避免为已放弃的请求继续消费上游 token。
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, c.Request.Context().Err())
	}

	cleanup()
	if info.StreamStatus.IsNormalEnd() && !info.StreamStatus.HasErrors() {
		logger.LogInfo(c, fmt.Sprintf("stream ended: %s", info.StreamStatus.Summary()))
	} else {
		logger.LogError(c, fmt.Sprintf("stream ended: %s, received=%d", info.StreamStatus.Summary(), info.ReceivedResponseCount))
	}
}
