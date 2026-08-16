package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
)

const (
	openCodeGoCapabilityCatalogURL      = "https://models.opencode.ai/api.json"
	openCodeGoCapabilityRefreshInterval = time.Hour
	openCodeGoCapabilityFetchTimeout    = 20 * time.Second
)

var (
	errOpenCodeGoCapabilityFetchFailed    = errors.New("OpenCode Go capability fetch failed")
	errOpenCodeGoCapabilityHTTPStatus     = errors.New("OpenCode Go capability HTTP status invalid")
	errOpenCodeGoCapabilityResponseTooBig = errors.New("OpenCode Go capability response too large")
)

type openCodeGoCapabilityCatalogClient struct {
	httpClient *http.Client
	endpoint   string
}

type openCodeGoCapabilityFetchResult struct {
	notModified bool
	etag        string
	body        []byte
}

type openCodeGoCapabilityRefreshResult struct {
	Outcome          string `json:"outcome"`
	ModelCount       int    `json:"model_count,omitempty"`
	SemanticRevision string `json:"semantic_revision,omitempty"`
	CheckedAt        int64  `json:"checked_at,omitempty"`
}

type openCodeGoCapabilityRefreshHandler struct {
	client *openCodeGoCapabilityCatalogClient
	now    func() time.Time
}

func newDefaultOpenCodeGoCapabilityCatalogClient() *openCodeGoCapabilityCatalogClient {
	return &openCodeGoCapabilityCatalogClient{
		httpClient: &http.Client{
			Timeout: openCodeGoCapabilityFetchTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		endpoint: openCodeGoCapabilityCatalogURL,
	}
}

func newOpenCodeGoCapabilityRefreshHandler() openCodeGoCapabilityRefreshHandler {
	return openCodeGoCapabilityRefreshHandler{
		client: newDefaultOpenCodeGoCapabilityCatalogClient(),
		now:    time.Now,
	}
}

func init() {
	handler := newOpenCodeGoCapabilityRefreshHandler()
	RegisterSystemTaskHandler(handler)
}

func (openCodeGoCapabilityRefreshHandler) Type() string {
	return model.SystemTaskTypeOpenCodeGoCapabilityRefresh
}

func (openCodeGoCapabilityRefreshHandler) Enabled() bool { return true }

func (openCodeGoCapabilityRefreshHandler) Interval() time.Duration {
	return openCodeGoCapabilityRefreshInterval
}

func (openCodeGoCapabilityRefreshHandler) NewPayload() any { return struct{}{} }

func (handler openCodeGoCapabilityRefreshHandler) Run(
	ctx context.Context,
	task *model.SystemTask,
	runnerID string,
) {
	if handler.client == nil || handler.now == nil {
		finishOpenCodeGoCapabilityRefreshFailure(task, runnerID, "configuration_failed")
		return
	}

	persisted, err := model.GetOpenCodeGoCapabilitySnapshot()
	if err != nil {
		finishOpenCodeGoCapabilityRefreshFailure(task, runnerID, "snapshot_read_failed")
		return
	}
	var persistedIndex *openCodeGoCapabilityIndex
	if persisted != nil {
		persistedIndex, err = openCodeGoCapabilityIndexFromRow(persisted)
		if err != nil {
			persisted = nil
			persistedIndex = nil
		}
	}
	etag := ""
	if persisted != nil && persistedIndex != nil {
		etag = persistedIndex.sourceETag
	}

	fetched, err := handler.client.fetch(ctx, etag)
	if err != nil {
		category := "fetch_failed"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			category = "cancelled"
		} else if errors.Is(err, errOpenCodeGoCapabilityHTTPStatus) {
			category = "http_status_failed"
		} else if errors.Is(err, errOpenCodeGoCapabilityResponseTooBig) {
			category = "response_too_large"
		}
		finishOpenCodeGoCapabilityRefreshFailure(task, runnerID, category)
		return
	}

	checkedAt := handler.now().Unix()
	var semantic *openCodeGoCapabilitySemantic
	sourceETag := fetched.etag
	if fetched.notModified {
		if persisted == nil || persistedIndex == nil {
			finishOpenCodeGoCapabilityRefreshFailure(task, runnerID, "not_modified_without_snapshot")
			return
		}
		semantic = persistedIndex.semantic
		if sourceETag == "" {
			sourceETag = persisted.SourceETag
		}
	} else {
		semantic, err = normalizeOpenCodeGoCapabilityCatalog(fetched.body)
		if err != nil {
			finishOpenCodeGoCapabilityRefreshFailure(task, runnerID, "catalog_invalid")
			return
		}
	}

	row := &model.OpenCodeGoCapabilitySnapshot{
		Provider:          model.OpenCodeGoCapabilityProvider,
		SchemaVersion:     semantic.schemaVersion,
		SemanticRevision:  semantic.revision,
		SourceETag:        sourceETag,
		CheckedAt:         checkedAt,
		NormalizedPayload: semantic.payload,
	}
	if err := model.PersistOpenCodeGoCapabilitySnapshotForTask(task, runnerID, row); err != nil {
		category := "snapshot_write_failed"
		if errors.Is(err, model.ErrSystemTaskLockLost) {
			category = "lease_lost"
		} else if errors.Is(err, model.ErrOpenCodeGoCapabilityStaleGeneration) {
			category = "stale_generation"
		}
		finishOpenCodeGoCapabilityRefreshFailure(task, runnerID, category)
		return
	}
	row.Generation = task.ID
	index, err := openCodeGoCapabilityIndexFromRow(row)
	if err != nil {
		finishOpenCodeGoCapabilityRefreshFailure(task, runnerID, "snapshot_validation_failed")
		return
	}
	publishOpenCodeGoCapabilityIndex(index)
	setOpenCodeGoCapabilityObserved(row)

	outcome := "updated"
	if fetched.notModified {
		outcome = "not_modified"
	}
	result := openCodeGoCapabilityRefreshResult{
		Outcome:          outcome,
		ModelCount:       semantic.modelCount,
		SemanticRevision: semantic.revision,
		CheckedAt:        checkedAt,
	}
	if err := model.FinishSystemTask(
		task.TaskID,
		runnerID,
		model.SystemTaskStatusSucceeded,
		result,
		"",
	); err != nil {
		logger.LogWarn(context.Background(), "OpenCode Go capability refresh finish failed category=lease_or_database")
	}
}

func (client *openCodeGoCapabilityCatalogClient) fetch(
	ctx context.Context,
	etag string,
) (*openCodeGoCapabilityFetchResult, error) {
	if client == nil || client.httpClient == nil || client.endpoint == "" || !validOpenCodeGoCapabilityETag(etag) {
		return nil, errOpenCodeGoCapabilityFetchFailed
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.endpoint, nil)
	if err != nil {
		return nil, errOpenCodeGoCapabilityFetchFailed
	}
	request.Header.Set("Accept", "application/json")
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errOpenCodeGoCapabilityFetchFailed
	}
	defer response.Body.Close()
	responseETag := response.Header.Get("ETag")
	if !validOpenCodeGoCapabilityETag(responseETag) {
		return nil, errOpenCodeGoCapabilityFetchFailed
	}
	if response.StatusCode == http.StatusNotModified {
		if strings.TrimSpace(etag) == "" {
			return nil, errOpenCodeGoCapabilityHTTPStatus
		}
		return &openCodeGoCapabilityFetchResult{notModified: true, etag: responseETag}, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, errOpenCodeGoCapabilityHTTPStatus
	}
	if response.ContentLength > openCodeGoCapabilityCatalogMaxBytes {
		return nil, errOpenCodeGoCapabilityResponseTooBig
	}
	limited := io.LimitReader(response.Body, openCodeGoCapabilityCatalogMaxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, errOpenCodeGoCapabilityFetchFailed
	}
	if len(body) > openCodeGoCapabilityCatalogMaxBytes {
		return nil, errOpenCodeGoCapabilityResponseTooBig
	}
	return &openCodeGoCapabilityFetchResult{etag: responseETag, body: body}, nil
}

func finishOpenCodeGoCapabilityRefreshFailure(task *model.SystemTask, runnerID string, category string) {
	if task == nil || category == "" {
		return
	}
	result := openCodeGoCapabilityRefreshResult{Outcome: category}
	if err := model.FinishSystemTask(
		task.TaskID,
		runnerID,
		model.SystemTaskStatusFailed,
		result,
		category,
	); err != nil {
		logger.LogWarn(context.Background(), "OpenCode Go capability refresh failure finalization failed category=lease_or_database")
	}
}
