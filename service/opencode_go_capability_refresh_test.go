package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func testOpenCodeGoCapabilityHTTPClient(endpoint string) *openCodeGoCapabilityCatalogClient {
	return &openCodeGoCapabilityCatalogClient{
		httpClient: &http.Client{
			Timeout: time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		endpoint: endpoint,
	}
}

func TestOpenCodeGoCapabilityCatalogClientHandles200And304WithoutFollowingRedirects(t *testing.T) {
	var seenIfNoneMatch atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		seenIfNoneMatch.Store(request.Header.Get("If-None-Match"))
		w.Header().Set("ETag", `"fixture-v1"`)
		if request.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(openCodeGoCapabilityCatalogFixture))
	}))
	defer server.Close()
	client := testOpenCodeGoCapabilityHTTPClient(server.URL)

	result, err := client.fetch(context.Background(), "")
	require.NoError(t, err)
	assert.False(t, result.notModified)
	assert.Equal(t, `"fixture-v1"`, result.etag)
	assert.Equal(t, openCodeGoCapabilityCatalogFixture, string(result.body))
	assert.Equal(t, "", seenIfNoneMatch.Load())

	result, err = client.fetch(context.Background(), `"fixture-v1"`)
	require.NoError(t, err)
	assert.True(t, result.notModified)
	assert.Empty(t, result.body)
	assert.Equal(t, `"fixture-v1"`, seenIfNoneMatch.Load())

	serverWithoutValidator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"fixture-v1"`)
		w.WriteHeader(http.StatusNotModified)
	}))
	defer serverWithoutValidator.Close()
	_, err = testOpenCodeGoCapabilityHTTPClient(serverWithoutValidator.URL).fetch(context.Background(), "")
	assert.ErrorIs(t, err, errOpenCodeGoCapabilityHTTPStatus)

	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("redirect target must not be reached")
	}))
	defer redirectTarget.Close()
	redirectSource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, redirectTarget.URL, http.StatusFound)
	}))
	defer redirectSource.Close()
	_, err = testOpenCodeGoCapabilityHTTPClient(redirectSource.URL).fetch(context.Background(), "")
	assert.ErrorIs(t, err, errOpenCodeGoCapabilityHTTPStatus)
}

func TestPollOpenCodeGoCapabilitySnapshotRetriesAfterInvalidPayload(t *testing.T) {
	semantic, err := normalizeOpenCodeGoCapabilityCatalog([]byte(openCodeGoCapabilityCatalogFixture))
	require.NoError(t, err)
	oldCurrent := openCodeGoCapabilityCurrent.Load()
	openCodeGoCapabilityObservedMu.Lock()
	oldObserved := openCodeGoCapabilityObserved
	openCodeGoCapabilityObserved = nil
	openCodeGoCapabilityObservedMu.Unlock()
	openCodeGoCapabilityCurrent.Store(nil)
	t.Cleanup(func() {
		openCodeGoCapabilityCurrent.Store(oldCurrent)
		openCodeGoCapabilityObservedMu.Lock()
		openCodeGoCapabilityObserved = oldObserved
		openCodeGoCapabilityObservedMu.Unlock()
	})

	now := time.Unix(openCodeGoCapabilitySeedCheckedAt+7200, 0)
	valid := &model.OpenCodeGoCapabilitySnapshot{
		Provider:          model.OpenCodeGoCapabilityProvider,
		Generation:        99,
		SchemaVersion:     semantic.schemaVersion,
		SemanticRevision:  semantic.revision,
		SourceETag:        `"retry-fixture"`,
		CheckedAt:         now.Unix(),
		NormalizedPayload: semantic.payload,
		UpdatedAt:         now.Unix(),
	}
	require.NoError(t, model.DB.AutoMigrate(&model.OpenCodeGoCapabilitySnapshot{}))
	require.NoError(t, model.DB.Where("provider = ?", model.OpenCodeGoCapabilityProvider).Delete(&model.OpenCodeGoCapabilitySnapshot{}).Error)
	invalid := *valid
	invalid.NormalizedPayload = "{}"
	require.NoError(t, model.DB.Create(&invalid).Error)

	pollOpenCodeGoCapabilitySnapshot(now)
	assert.Nil(t, openCodeGoCapabilityCurrent.Load())

	require.NoError(t, model.DB.Model(&model.OpenCodeGoCapabilitySnapshot{}).
		Where("provider = ?", model.OpenCodeGoCapabilityProvider).
		UpdateColumn("normalized_payload", semantic.payload).Error)
	pollOpenCodeGoCapabilitySnapshot(now)
	current := openCodeGoCapabilityCurrent.Load()
	require.NotNil(t, current)
	assert.Equal(t, semantic.revision, current.semantic.revision)
}

func TestOpenCodeGoCapabilityCatalogClientBoundsResponseBeforeDecode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(openCodeGoCapabilityCatalogMaxBytes+1))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	_, err := testOpenCodeGoCapabilityHTTPClient(server.URL).fetch(context.Background(), "")
	assert.ErrorIs(t, err, errOpenCodeGoCapabilityResponseTooBig)
}

func TestOpenCodeGoCapabilityRefreshIsScheduledImmediatelyAndDeduplicated(t *testing.T) {
	handler := newOpenCodeGoCapabilityRefreshHandler()
	withSystemTaskRegistry(t, handler)
	require.NoError(t, model.DB.Where("type = ?", model.SystemTaskTypeOpenCodeGoCapabilityRefresh).
		Delete(&model.SystemTask{}).Error)
	t.Cleanup(func() {
		_ = model.DB.Where("type = ?", model.SystemTaskTypeOpenCodeGoCapabilityRefresh).
			Delete(&model.SystemTask{}).Error
	})

	runSystemTaskScheduler()
	latest, err := model.GetLatestSystemTask(model.SystemTaskTypeOpenCodeGoCapabilityRefresh)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, model.SystemTaskStatusPending, latest.Status)
	assert.Equal(t, time.Hour, handler.Interval())

	runSystemTaskScheduler()
	var count int64
	require.NoError(t, model.DB.Model(&model.SystemTask{}).
		Where("type = ?", model.SystemTaskTypeOpenCodeGoCapabilityRefresh).
		Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestOpenCodeGoCapabilityRefreshHandlerPersistsPublishesAndRetainsLKG(t *testing.T) {
	truncate(t)
	require.NoError(t, model.DB.AutoMigrate(&model.OpenCodeGoCapabilitySnapshot{}))
	require.NoError(t, model.DB.Where("provider = ?", model.OpenCodeGoCapabilityProvider).
		Delete(&model.OpenCodeGoCapabilitySnapshot{}).Error)
	oldCurrent := openCodeGoCapabilityCurrent.Load()
	openCodeGoCapabilityObservedMu.Lock()
	oldObserved := openCodeGoCapabilityObserved
	openCodeGoCapabilityObserved = nil
	openCodeGoCapabilityObservedMu.Unlock()
	openCodeGoCapabilityCurrent.Store(nil)
	t.Cleanup(func() {
		_ = model.DB.Where("provider = ?", model.OpenCodeGoCapabilityProvider).
			Delete(&model.OpenCodeGoCapabilitySnapshot{}).Error
		openCodeGoCapabilityCurrent.Store(oldCurrent)
		openCodeGoCapabilityObservedMu.Lock()
		openCodeGoCapabilityObserved = oldObserved
		openCodeGoCapabilityObservedMu.Unlock()
	})

	var invalidResponse atomic.Bool
	var requestCount atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestCount.Add(1)
		w.Header().Set("ETag", `"fixture-v1"`)
		if invalidResponse.Load() {
			_, _ = w.Write([]byte(`{"sentinel_upstream_text":"must_not_persist"}`))
			return
		}
		if request.Header.Get("If-None-Match") == `"fixture-v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(openCodeGoCapabilityCatalogFixture))
	}))
	defer server.Close()

	now := time.Unix(openCodeGoCapabilitySeedCheckedAt+120, 0)
	handler := openCodeGoCapabilityRefreshHandler{
		client: testOpenCodeGoCapabilityHTTPClient(server.URL),
		now:    func() time.Time { return now },
	}
	assert.Equal(t, time.Hour, handler.Interval())
	assert.True(t, handler.Enabled())
	var _ ScheduledSystemTaskHandler = handler

	first := claimOpenCodeGoCapabilityRefreshTask(t, "capability-runner-1")
	handler.Run(context.Background(), first, "capability-runner-1")
	firstFinished, err := model.GetSystemTaskByTaskID(first.TaskID)
	require.NoError(t, err)
	require.NotNil(t, firstFinished)
	assert.Equal(t, model.SystemTaskStatusSucceeded, firstFinished.Status)
	assert.NotContains(t, firstFinished.Result, server.URL)
	assert.NotContains(t, firstFinished.Result, "sentinel_upstream_text")

	firstSnapshot, err := model.GetOpenCodeGoCapabilitySnapshot()
	require.NoError(t, err)
	require.NotNil(t, firstSnapshot)
	assert.Equal(t, first.ID, firstSnapshot.Generation)
	assert.Equal(t, now.Unix(), firstSnapshot.CheckedAt)
	assert.Equal(t, `"fixture-v1"`, firstSnapshot.SourceETag)
	firstView := PinOpenCodeGoCapabilityView(now)
	assert.Equal(t, OpenCodeGoCapabilitySupported, firstView.CheckEffort("Case/Model", "low"))
	assert.Equal(t, OpenCodeGoCapabilityUnsupported, firstView.CheckEffort("Case/Model", "max"))

	now = now.Add(time.Hour)
	second := claimOpenCodeGoCapabilityRefreshTask(t, "capability-runner-2")
	handler.Run(context.Background(), second, "capability-runner-2")
	secondSnapshot, err := model.GetOpenCodeGoCapabilitySnapshot()
	require.NoError(t, err)
	require.NotNil(t, secondSnapshot)
	assert.Greater(t, secondSnapshot.Generation, firstSnapshot.Generation)
	assert.Equal(t, now.Unix(), secondSnapshot.CheckedAt)
	assert.Equal(t, firstSnapshot.SemanticRevision, secondSnapshot.SemanticRevision)
	assert.Equal(t, firstSnapshot.NormalizedPayload, secondSnapshot.NormalizedPayload)

	invalidResponse.Store(true)
	now = now.Add(time.Hour)
	third := claimOpenCodeGoCapabilityRefreshTask(t, "capability-runner-3")
	handler.Run(context.Background(), third, "capability-runner-3")
	thirdFinished, err := model.GetSystemTaskByTaskID(third.TaskID)
	require.NoError(t, err)
	require.NotNil(t, thirdFinished)
	assert.Equal(t, model.SystemTaskStatusFailed, thirdFinished.Status)
	assert.Equal(t, "catalog_invalid", thirdFinished.Error)
	assert.NotContains(t, thirdFinished.Result, "sentinel_upstream_text")
	assert.NotContains(t, thirdFinished.Error, server.URL)

	afterFailure, err := model.GetOpenCodeGoCapabilitySnapshot()
	require.NoError(t, err)
	require.NotNil(t, afterFailure)
	assert.Equal(t, secondSnapshot.Generation, afterFailure.Generation)
	assert.Equal(t, secondSnapshot.CheckedAt, afterFailure.CheckedAt)
	assert.Equal(t, secondSnapshot.SemanticRevision, afterFailure.SemanticRevision)
	assert.Equal(t, int64(3), requestCount.Load())
}

func claimOpenCodeGoCapabilityRefreshTask(t *testing.T, runnerID string) *model.SystemTask {
	t.Helper()
	task, err := model.CreateSystemTask(model.SystemTaskTypeOpenCodeGoCapabilityRefresh, struct{}{}, nil)
	require.NoError(t, err)
	claimed, ok, err := model.ClaimSystemTask(
		task.ID,
		task.Type,
		runnerID,
		common.GetTimestamp()+60,
	)
	require.NoError(t, err)
	require.True(t, ok)
	return claimed
}

func TestInitializeAndPollOpenCodeGoCapabilityAuthorityUsesFreshestValidView(t *testing.T) {
	require.NoError(t, model.DB.AutoMigrate(&model.OpenCodeGoCapabilitySnapshot{}))
	require.NoError(t, model.DB.Where("provider = ?", model.OpenCodeGoCapabilityProvider).
		Delete(&model.OpenCodeGoCapabilitySnapshot{}).Error)
	oldCurrent := openCodeGoCapabilityCurrent.Load()
	openCodeGoCapabilityObservedMu.Lock()
	oldObserved := openCodeGoCapabilityObserved
	openCodeGoCapabilityObserved = nil
	openCodeGoCapabilityObservedMu.Unlock()
	openCodeGoCapabilityCurrent.Store(nil)
	t.Cleanup(func() {
		_ = model.DB.Where("provider = ?", model.OpenCodeGoCapabilityProvider).
			Delete(&model.OpenCodeGoCapabilitySnapshot{}).Error
		openCodeGoCapabilityCurrent.Store(oldCurrent)
		openCodeGoCapabilityObservedMu.Lock()
		openCodeGoCapabilityObserved = oldObserved
		openCodeGoCapabilityObservedMu.Unlock()
	})

	now := time.Unix(openCodeGoCapabilitySeedCheckedAt+3600, 0)
	initializeOpenCodeGoCapabilityAuthority(now)
	seedView := PinOpenCodeGoCapabilityView(now)
	assert.Equal(t, openCodeGoCapabilitySeedRevision, seedView.SemanticRevision())

	semantic, err := normalizeOpenCodeGoCapabilityCatalog([]byte(openCodeGoCapabilityCatalogFixture))
	require.NoError(t, err)
	dbRow := &model.OpenCodeGoCapabilitySnapshot{
		Provider:          model.OpenCodeGoCapabilityProvider,
		Generation:        10,
		SchemaVersion:     semantic.schemaVersion,
		SemanticRevision:  semantic.revision,
		SourceETag:        `"db-v1"`,
		CheckedAt:         now.Add(time.Minute).Unix(),
		NormalizedPayload: semantic.payload,
		UpdatedAt:         now.Unix(),
	}
	require.NoError(t, model.DB.Create(dbRow).Error)
	pollOpenCodeGoCapabilitySnapshot(now.Add(time.Minute))
	dbView := PinOpenCodeGoCapabilityView(now.Add(time.Minute))
	assert.Equal(t, semantic.revision, dbView.SemanticRevision())

	require.NoError(t, model.DB.Model(&model.OpenCodeGoCapabilitySnapshot{}).
		Where("provider = ?", model.OpenCodeGoCapabilityProvider).
		Updates(map[string]any{
			"generation":        11,
			"semantic_revision": strings.Repeat("0", 64),
			"checked_at":        now.Add(2 * time.Minute).Unix(),
			"updated_at":        now.Add(2 * time.Minute).Unix(),
		}).Error)
	pollOpenCodeGoCapabilitySnapshot(now.Add(2 * time.Minute))
	afterInvalid := PinOpenCodeGoCapabilityView(now.Add(2 * time.Minute))
	assert.Equal(t, semantic.revision, afterInvalid.SemanticRevision(), "invalid DB data must not clear the working LKG")

	require.NoError(t, model.DB.Model(&model.OpenCodeGoCapabilitySnapshot{}).
		Where("provider = ?", model.OpenCodeGoCapabilityProvider).
		Updates(map[string]any{
			"generation":        1,
			"semantic_revision": semantic.revision,
			"checked_at":        openCodeGoCapabilitySeedCheckedAt - 1,
			"updated_at":        now.Add(3 * time.Minute).Unix(),
		}).Error)
	pollOpenCodeGoCapabilitySnapshot(now.Add(3 * time.Minute))
	afterRestore := PinOpenCodeGoCapabilityView(now.Add(3 * time.Minute))
	assert.Equal(t, semantic.revision, afterRestore.SemanticRevision())
	assert.Equal(t, dbRow.CheckedAt, afterRestore.CheckedAt(), "an older restored row must not regress checked_at")
}

func TestInitializeOpenCodeGoCapabilityAuthorityUsesSeedBeforeSlaveMigration(t *testing.T) {
	dbWithoutCapabilityTable, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	oldDB := model.DB
	oldCurrent := openCodeGoCapabilityCurrent.Load()
	openCodeGoCapabilityObservedMu.Lock()
	oldObserved := openCodeGoCapabilityObserved
	openCodeGoCapabilityObserved = nil
	openCodeGoCapabilityObservedMu.Unlock()
	model.DB = dbWithoutCapabilityTable
	openCodeGoCapabilityCurrent.Store(nil)
	t.Cleanup(func() {
		model.DB = oldDB
		openCodeGoCapabilityCurrent.Store(oldCurrent)
		openCodeGoCapabilityObservedMu.Lock()
		openCodeGoCapabilityObserved = oldObserved
		openCodeGoCapabilityObservedMu.Unlock()
	})

	now := time.Unix(openCodeGoCapabilitySeedCheckedAt, 0).Add(time.Hour)
	initializeOpenCodeGoCapabilityAuthority(now)
	view := PinOpenCodeGoCapabilityView(now)
	assert.Equal(t, openCodeGoCapabilitySeedRevision, view.SemanticRevision())
	assert.Equal(t, OpenCodeGoCapabilityFreshnessFresh, view.Freshness())

	pollOpenCodeGoCapabilitySnapshot(now.Add(time.Minute))
	assert.Equal(t, openCodeGoCapabilitySeedRevision, PinOpenCodeGoCapabilityView(now.Add(time.Minute)).SemanticRevision())
}

func TestCapabilityRefreshFailurePersistsOnlyStableCategory(t *testing.T) {
	truncate(t)
	require.NoError(t, model.DB.AutoMigrate(&model.OpenCodeGoCapabilitySnapshot{}))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprint(w, "sensitive-provider-error-marker")
	}))
	defer server.Close()
	task := claimOpenCodeGoCapabilityRefreshTask(t, "capability-error-runner")
	handler := openCodeGoCapabilityRefreshHandler{
		client: testOpenCodeGoCapabilityHTTPClient(server.URL),
		now:    time.Now,
	}
	handler.Run(context.Background(), task, "capability-error-runner")
	finished, err := model.GetSystemTaskByTaskID(task.TaskID)
	require.NoError(t, err)
	require.NotNil(t, finished)
	assert.Equal(t, model.SystemTaskStatusFailed, finished.Status)
	assert.Equal(t, "http_status_failed", finished.Error)
	assert.NotContains(t, finished.Result, "sensitive-provider-error-marker")
	assert.NotContains(t, finished.Result, server.URL)
}
