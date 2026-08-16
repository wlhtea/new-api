package service

import (
	"context"
	"encoding/hex"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
)

const (
	openCodeGoCapabilityFreshAge   = 2 * time.Hour
	openCodeGoCapabilityExpiryAge  = 24 * time.Hour
	openCodeGoCapabilityWarnPeriod = time.Hour

	openCodeGoCapabilitySeedCheckedAt = int64(1786895362) // 2026-08-16T15:49:22Z
	openCodeGoCapabilitySeedRevision  = "a8f6968f0b36919050ba3700eda75b8c079868657e8bb44833ff303453501c19"
	openCodeGoCapabilitySeedETag      = `"83e016413791f14b35b1faf0dd87e8e4"`
	openCodeGoCapabilitySeedPayload   = `{"schema_version":1,"provider":"opencode-go","models":[{"id":"deepseek-v4-flash","options_known":true,"efforts":["high","low","max"]},{"id":"deepseek-v4-pro","options_known":true,"efforts":["high","max"]},{"id":"glm-5","options_known":true,"efforts":[]},{"id":"glm-5.1","options_known":true,"efforts":[]},{"id":"glm-5.2","options_known":true,"efforts":["high","max"]},{"id":"glm-5.3","options_known":true,"efforts":["high","low","max"]},{"id":"gpt-5.6-luna","options_known":true,"efforts":["high","low","max","medium","none","xhigh"]},{"id":"grok-4.5","options_known":true,"efforts":["high","low","medium"]},{"id":"hy3","options_known":true,"efforts":["high","low","none"]},{"id":"kimi-k2.5","options_known":true,"efforts":[]},{"id":"kimi-k2.6","options_known":true,"efforts":[]},{"id":"kimi-k2.7-code","options_known":true,"efforts":[]},{"id":"kimi-k3","options_known":true,"efforts":["max"]},{"id":"mimo-v2-omni","options_known":true,"efforts":[]},{"id":"mimo-v2-pro","options_known":true,"efforts":[]},{"id":"mimo-v2.5","options_known":true,"efforts":[]},{"id":"mimo-v2.5-pro","options_known":true,"efforts":[]},{"id":"minimax-m2.5","options_known":true,"efforts":[]},{"id":"minimax-m2.7","options_known":true,"efforts":[]},{"id":"minimax-m3","options_known":true,"efforts":[]},{"id":"qwen3.5-plus","options_known":true,"efforts":[]},{"id":"qwen3.6-plus","options_known":true,"efforts":[]},{"id":"qwen3.7-max","options_known":true,"efforts":[]},{"id":"qwen3.7-plus","options_known":true,"efforts":[]},{"id":"qwen3.8-max","options_known":true,"efforts":[]}]}`
)

type OpenCodeGoCapabilityFreshness string

const (
	OpenCodeGoCapabilityFreshnessMissing OpenCodeGoCapabilityFreshness = "missing"
	OpenCodeGoCapabilityFreshnessFresh   OpenCodeGoCapabilityFreshness = "fresh"
	OpenCodeGoCapabilityFreshnessWarning OpenCodeGoCapabilityFreshness = "warning"
	OpenCodeGoCapabilityFreshnessExpired OpenCodeGoCapabilityFreshness = "expired"
	OpenCodeGoCapabilityFreshnessInvalid OpenCodeGoCapabilityFreshness = "invalid"
)

type OpenCodeGoCapabilityDecision string

const (
	OpenCodeGoCapabilitySupported   OpenCodeGoCapabilityDecision = "supported"
	OpenCodeGoCapabilityUnsupported OpenCodeGoCapabilityDecision = "unsupported"
	OpenCodeGoCapabilityUnknown     OpenCodeGoCapabilityDecision = "unknown"
)

type openCodeGoCapabilityIndex struct {
	generation int64
	checkedAt  int64
	sourceETag string
	semantic   *openCodeGoCapabilitySemantic
}

// OpenCodeGoCapabilityView pins one immutable semantic revision and one
// clock-derived freshness class. Candidate planning and final validation can
// safely retain this value while a background refresh publishes a newer index.
type OpenCodeGoCapabilityView struct {
	index     *openCodeGoCapabilityIndex
	freshness OpenCodeGoCapabilityFreshness
}

var (
	openCodeGoCapabilityCurrent  atomic.Pointer[openCodeGoCapabilityIndex]
	openCodeGoCapabilityPublish  sync.Mutex
	openCodeGoCapabilitySyncOnce sync.Once
	openCodeGoCapabilityWarnedAt atomic.Int64

	openCodeGoCapabilityObservedMu sync.Mutex
	openCodeGoCapabilityObserved   *model.OpenCodeGoCapabilitySnapshotMetadata
)

func PinOpenCodeGoCapabilityView(now time.Time) OpenCodeGoCapabilityView {
	index := openCodeGoCapabilityCurrent.Load()
	freshness := openCodeGoCapabilityFreshnessAt(index, now)
	if freshness == OpenCodeGoCapabilityFreshnessWarning {
		warnOpenCodeGoCapabilityFreshness(index, now)
	}
	return OpenCodeGoCapabilityView{index: index, freshness: freshness}
}

func CurrentOpenCodeGoCapabilityView() OpenCodeGoCapabilityView {
	return PinOpenCodeGoCapabilityView(time.Now())
}

func (view OpenCodeGoCapabilityView) Freshness() OpenCodeGoCapabilityFreshness {
	return view.freshness
}

func (view OpenCodeGoCapabilityView) SemanticRevision() string {
	if view.index == nil || view.index.semantic == nil {
		return ""
	}
	return view.index.semantic.revision
}

func (view OpenCodeGoCapabilityView) CheckedAt() int64 {
	if view.index == nil {
		return 0
	}
	return view.index.checkedAt
}

func (view OpenCodeGoCapabilityView) ModelCount() int {
	if view.index == nil || view.index.semantic == nil {
		return 0
	}
	return view.index.semantic.modelCount
}

func (view OpenCodeGoCapabilityView) CheckEffort(exactModelID string, effort string) OpenCodeGoCapabilityDecision {
	if view.index == nil || view.index.semantic == nil ||
		(view.freshness != OpenCodeGoCapabilityFreshnessFresh &&
			view.freshness != OpenCodeGoCapabilityFreshnessWarning) {
		return OpenCodeGoCapabilityUnknown
	}
	capability, ok := view.index.semantic.models[exactModelID]
	if !ok || !capability.optionsKnown {
		return OpenCodeGoCapabilityUnknown
	}
	if _, ok := capability.efforts[effort]; ok {
		return OpenCodeGoCapabilitySupported
	}
	return OpenCodeGoCapabilityUnsupported
}

// StartOpenCodeGoCapabilityAuthority synchronously loads the freshest valid DB
// or bundled-seed view, then starts metadata-first DB polling for cross-node
// convergence. Call it once during startup before serving inference requests.
func StartOpenCodeGoCapabilityAuthority(syncFrequencySeconds int) {
	initializeOpenCodeGoCapabilityAuthority(time.Now())
	if syncFrequencySeconds <= 0 {
		syncFrequencySeconds = 60
	}
	openCodeGoCapabilitySyncOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(time.Duration(syncFrequencySeconds) * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				pollOpenCodeGoCapabilitySnapshot(time.Now())
			}
		}()
	})
}

func initializeOpenCodeGoCapabilityAuthority(now time.Time) {
	seed, seedErr := openCodeGoCapabilityIndexFromRow(&model.OpenCodeGoCapabilitySnapshot{
		Provider:          model.OpenCodeGoCapabilityProvider,
		Generation:        0,
		SchemaVersion:     openCodeGoCapabilitySchemaVersion,
		SemanticRevision:  openCodeGoCapabilitySeedRevision,
		SourceETag:        openCodeGoCapabilitySeedETag,
		CheckedAt:         openCodeGoCapabilitySeedCheckedAt,
		NormalizedPayload: openCodeGoCapabilitySeedPayload,
	})
	if seedErr == nil && openCodeGoCapabilityFreshnessAt(seed, now) != OpenCodeGoCapabilityFreshnessInvalid {
		publishOpenCodeGoCapabilityIndex(seed)
	}

	row, err := model.GetOpenCodeGoCapabilitySnapshot()
	if err != nil || row == nil {
		return
	}
	index, err := openCodeGoCapabilityIndexFromRow(row)
	if err != nil || openCodeGoCapabilityFreshnessAt(index, now) == OpenCodeGoCapabilityFreshnessInvalid {
		return
	}
	setOpenCodeGoCapabilityObserved(row)
	publishOpenCodeGoCapabilityIndex(index)
}

func pollOpenCodeGoCapabilitySnapshot(now time.Time) {
	metadata, err := model.GetOpenCodeGoCapabilitySnapshotMetadata()
	if err != nil || metadata == nil || isOpenCodeGoCapabilityMetadataObserved(metadata) {
		return
	}
	row, err := model.GetOpenCodeGoCapabilitySnapshot()
	if err != nil || row == nil {
		return
	}
	index, err := openCodeGoCapabilityIndexFromRow(row)
	if err != nil || openCodeGoCapabilityFreshnessAt(index, now) == OpenCodeGoCapabilityFreshnessInvalid {
		return
	}
	publishOpenCodeGoCapabilityIndex(index)
	setOpenCodeGoCapabilityObserved(row)
}

func openCodeGoCapabilityIndexFromRow(row *model.OpenCodeGoCapabilitySnapshot) (*openCodeGoCapabilityIndex, error) {
	if row == nil || row.Provider != model.OpenCodeGoCapabilityProvider ||
		row.Generation < 0 || row.SchemaVersion != openCodeGoCapabilitySchemaVersion ||
		row.CheckedAt <= 0 || !validOpenCodeGoCapabilityETag(row.SourceETag) ||
		len(row.SemanticRevision) != 64 {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	if _, err := hex.DecodeString(row.SemanticRevision); err != nil || strings.ToLower(row.SemanticRevision) != row.SemanticRevision {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	semantic, err := parseOpenCodeGoCapabilityNormalizedPayload(row.NormalizedPayload)
	if err != nil || semantic.schemaVersion != row.SchemaVersion ||
		semantic.provider != row.Provider || semantic.revision != row.SemanticRevision {
		return nil, errOpenCodeGoCapabilityInvalidCatalog
	}
	return &openCodeGoCapabilityIndex{
		generation: row.Generation,
		checkedAt:  row.CheckedAt,
		sourceETag: row.SourceETag,
		semantic:   semantic,
	}, nil
}

func openCodeGoCapabilityFreshnessAt(index *openCodeGoCapabilityIndex, now time.Time) OpenCodeGoCapabilityFreshness {
	if index == nil || index.semantic == nil {
		return OpenCodeGoCapabilityFreshnessMissing
	}
	checkedAt := time.Unix(index.checkedAt, 0)
	if index.checkedAt <= 0 || checkedAt.After(now) {
		return OpenCodeGoCapabilityFreshnessInvalid
	}
	age := now.Sub(checkedAt)
	switch {
	case age <= openCodeGoCapabilityFreshAge:
		return OpenCodeGoCapabilityFreshnessFresh
	case age <= openCodeGoCapabilityExpiryAge:
		return OpenCodeGoCapabilityFreshnessWarning
	default:
		return OpenCodeGoCapabilityFreshnessExpired
	}
}

func publishOpenCodeGoCapabilityIndex(candidate *openCodeGoCapabilityIndex) bool {
	if candidate == nil || candidate.semantic == nil {
		return false
	}
	openCodeGoCapabilityPublish.Lock()
	defer openCodeGoCapabilityPublish.Unlock()
	current := openCodeGoCapabilityCurrent.Load()
	if current != nil {
		if candidate.checkedAt < current.checkedAt {
			return false
		}
		if candidate.checkedAt == current.checkedAt && candidate.generation < current.generation {
			return false
		}
		if candidate.checkedAt == current.checkedAt && candidate.generation == current.generation {
			return false
		}
	}
	openCodeGoCapabilityCurrent.Store(candidate)
	return true
}

func validOpenCodeGoCapabilityETag(etag string) bool {
	if len(etag) > 256 {
		return false
	}
	if etag == "" {
		return true
	}
	opaque := etag
	if strings.HasPrefix(opaque, "W/") {
		opaque = strings.TrimPrefix(opaque, "W/")
	}
	if len(opaque) < 2 || opaque[0] != '"' || opaque[len(opaque)-1] != '"' {
		return false
	}
	for _, r := range opaque[1 : len(opaque)-1] {
		if r < 0x21 || r == '"' || r > 0x7e {
			return false
		}
	}
	return true
}

func observeOpenCodeGoCapabilityMetadata(metadata *model.OpenCodeGoCapabilitySnapshotMetadata) bool {
	openCodeGoCapabilityObservedMu.Lock()
	defer openCodeGoCapabilityObservedMu.Unlock()
	if openCodeGoCapabilityObserved != nil && *openCodeGoCapabilityObserved == *metadata {
		return false
	}
	copyValue := *metadata
	openCodeGoCapabilityObserved = &copyValue
	return true
}

func isOpenCodeGoCapabilityMetadataObserved(metadata *model.OpenCodeGoCapabilitySnapshotMetadata) bool {
	if metadata == nil {
		return false
	}
	openCodeGoCapabilityObservedMu.Lock()
	defer openCodeGoCapabilityObservedMu.Unlock()
	return openCodeGoCapabilityObserved != nil && *openCodeGoCapabilityObserved == *metadata
}

func setOpenCodeGoCapabilityObserved(row *model.OpenCodeGoCapabilitySnapshot) {
	if row == nil {
		return
	}
	observeOpenCodeGoCapabilityMetadata(&model.OpenCodeGoCapabilitySnapshotMetadata{
		Provider:         row.Provider,
		Generation:       row.Generation,
		SchemaVersion:    row.SchemaVersion,
		SemanticRevision: row.SemanticRevision,
		SourceETag:       row.SourceETag,
		CheckedAt:        row.CheckedAt,
		UpdatedAt:        row.UpdatedAt,
	})
}

func warnOpenCodeGoCapabilityFreshness(index *openCodeGoCapabilityIndex, now time.Time) {
	if index == nil || index.semantic == nil {
		return
	}
	nowUnix := now.Unix()
	for {
		last := openCodeGoCapabilityWarnedAt.Load()
		if last > 0 && nowUnix-last < int64(openCodeGoCapabilityWarnPeriod/time.Second) {
			return
		}
		if openCodeGoCapabilityWarnedAt.CompareAndSwap(last, nowUnix) {
			logger.LogWarn(
				context.Background(),
				"OpenCode Go capability authority freshness=warning revision="+index.semantic.revision,
			)
			return
		}
	}
}
