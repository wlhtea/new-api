package perfmetrics

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/perf_metrics_setting"
	"github.com/bytedance/gopkg/util/gopool"
)

func TestWaitForScheduledRelaySamplesObservesAllScheduledSamples(t *testing.T) {
	WaitForScheduledRelaySamples()

	const (
		modelName   = "relay-metrics-wait-boundary"
		groupName   = "relay-metrics-test"
		sampleCount = 64
	)
	deleteRelayMetricBuckets(modelName, groupName)
	t.Cleanup(func() {
		WaitForScheduledRelaySamples()
		deleteRelayMetricBuckets(modelName, groupName)
	})

	info := &relaycommon.RelayInfo{
		OriginModelName: modelName,
		UsingGroup:      groupName,
		StartTime:       time.Now().Add(-time.Second),
	}
	for i := 0; i < sampleCount; i++ {
		ScheduleRelaySample(info, true, 1)
	}

	WaitForScheduledRelaySamples()

	got := relayMetricCounters(modelName, groupName)
	if got.requestCount != sampleCount {
		t.Fatalf("scheduled request count = %d, want %d", got.requestCount, sampleCount)
	}
	if got.successCount != sampleCount {
		t.Fatalf("scheduled success count = %d, want %d", got.successCount, sampleCount)
	}
	if got.outputTokens != sampleCount {
		t.Fatalf("scheduled output tokens = %d, want %d", got.outputTokens, sampleCount)
	}
}

func TestWaitForScheduledRelaySamplesIgnoresUnrelatedPoolWork(t *testing.T) {
	WaitForScheduledRelaySamples()

	workerStarted := make(chan struct{})
	releaseWorker := make(chan struct{})
	workerDone := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseWorker) })
	}
	t.Cleanup(func() {
		release()
		awaitSignal(t, workerDone, "unrelated pool worker cleanup")
	})

	gopool.Go(func() {
		close(workerStarted)
		<-releaseWorker
		close(workerDone)
	})
	awaitSignal(t, workerStarted, "unrelated pool worker start")

	waitReturned := make(chan struct{})
	go func() {
		WaitForScheduledRelaySamples()
		close(waitReturned)
	}()
	awaitSignal(t, waitReturned, "relay sample wait while unrelated work is running")

	select {
	case <-workerDone:
		t.Fatal("unrelated pool work completed before it was released")
	default:
	}

	release()
	awaitSignal(t, workerDone, "unrelated pool worker completion")
}

func TestScheduleRelaySampleCompletesTrackingAfterPanic(t *testing.T) {
	WaitForScheduledRelaySamples()

	const (
		modelName = "relay-metrics-panic-boundary"
		groupName = "relay-metrics-test"
	)
	deleteRelayMetricBuckets(modelName, groupName)
	poisonRelayMetricBuckets(modelName, groupName)
	t.Cleanup(func() {
		WaitForScheduledRelaySamples()
		deleteRelayMetricBuckets(modelName, groupName)
	})

	panicSeen := make(chan any, 1)
	gopool.SetPanicHandler(func(_ context.Context, recovered any) {
		panicSeen <- recovered
	})
	t.Cleanup(func() { gopool.SetPanicHandler(nil) })

	ScheduleRelaySample(&relaycommon.RelayInfo{
		OriginModelName: modelName,
		UsingGroup:      groupName,
		StartTime:       time.Now().Add(-time.Second),
	}, true, 1)

	waitReturned := make(chan struct{})
	go func() {
		WaitForScheduledRelaySamples()
		close(waitReturned)
	}()
	awaitSignal(t, waitReturned, "relay sample wait after callback panic")

	var recovered any
	select {
	case recovered = <-panicSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduled relay sample did not reach the pool panic handler")
	}
	if _, ok := recovered.(*runtime.TypeAssertionError); !ok {
		t.Fatalf("scheduled relay sample panic = %T, want *runtime.TypeAssertionError", recovered)
	}
}

func relayMetricCounters(modelName string, groupName string) counters {
	total := counters{}
	hotBuckets.Range(func(key, value any) bool {
		metricKey, ok := key.(bucketKey)
		if !ok || metricKey.model != modelName || metricKey.group != groupName {
			return true
		}
		bucket, ok := value.(*atomicBucket)
		if !ok {
			return true
		}
		snapshot := bucket.snapshot()
		total.requestCount += snapshot.requestCount
		total.successCount += snapshot.successCount
		total.outputTokens += snapshot.outputTokens
		return true
	})
	return total
}

func poisonRelayMetricBuckets(modelName string, groupName string) {
	bucketSeconds := perf_metrics_setting.GetBucketSeconds()
	now := time.Now().Unix()
	for offset := int64(-1); offset <= 1; offset++ {
		hotBuckets.Store(bucketKey{
			model:    modelName,
			group:    groupName,
			bucketTs: bucketStart(now + offset*bucketSeconds),
		}, struct{}{})
	}
}

func deleteRelayMetricBuckets(modelName string, groupName string) {
	hotBuckets.Range(func(key, _ any) bool {
		metricKey, ok := key.(bucketKey)
		if ok && metricKey.model == modelName && metricKey.group == groupName {
			hotBuckets.Delete(key)
		}
		return true
	})
}

func awaitSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}
