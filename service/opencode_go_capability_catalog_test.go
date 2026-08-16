package service

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const openCodeGoCapabilityCatalogFixture = `{
  "opencode-go": {
    "id": "opencode-go",
    "models": {
      "Case/Model": {
        "id": "Case/Model",
        "reasoning_options": [
          {"type":"effort","values":["low",null,"high","low"]}
        ]
      },
      "empty": {"id":"empty","reasoning_options":[]},
      "fallback": {"id":"fallback"},
      "toggle": {"id":"toggle","reasoning_options":[{"type":"toggle"}]}
    }
  }
}`

func TestNormalizeOpenCodeGoCapabilityCatalogPreservesAuthoritySemantics(t *testing.T) {
	semantic, err := normalizeOpenCodeGoCapabilityCatalog([]byte(openCodeGoCapabilityCatalogFixture))
	require.NoError(t, err)
	assert.Equal(t, 4, semantic.modelCount)
	assert.Len(t, semantic.revision, 64)
	assert.LessOrEqual(t, len(semantic.payload), openCodeGoCapabilityNormalizedMaxBytes)

	caseModel := semantic.models["Case/Model"]
	assert.True(t, caseModel.optionsKnown)
	assert.Equal(t, map[string]struct{}{
		"high": {},
		"low":  {},
		"none": {},
	}, caseModel.efforts)
	assert.True(t, semantic.models["empty"].optionsKnown)
	assert.Empty(t, semantic.models["empty"].efforts)
	assert.False(t, semantic.models["fallback"].optionsKnown)
	assert.True(t, semantic.models["toggle"].optionsKnown)
	assert.Empty(t, semantic.models["toggle"].efforts)

	reparsed, err := parseOpenCodeGoCapabilityNormalizedPayload(semantic.payload)
	require.NoError(t, err)
	assert.Equal(t, semantic.revision, reparsed.revision)
	assert.Equal(t, semantic.modelCount, reparsed.modelCount)
}

func TestNormalizeOpenCodeGoCapabilityCatalogRevisionIsCanonical(t *testing.T) {
	first := `{"opencode-go":{"id":"opencode-go","models":{"b":{"id":"b","reasoning_options":[]},"a":{"id":"a","reasoning_options":[{"type":"effort","values":["max","low","max"]}]}}}}`
	second := `{"opencode-go":{"models":{"a":{"reasoning_options":[{"values":["low","max"],"type":"effort"}],"id":"a"},"b":{"reasoning_options":[],"id":"b"}},"id":"opencode-go"}}`
	firstSemantic, err := normalizeOpenCodeGoCapabilityCatalog([]byte(first))
	require.NoError(t, err)
	secondSemantic, err := normalizeOpenCodeGoCapabilityCatalog([]byte(second))
	require.NoError(t, err)
	assert.Equal(t, firstSemantic.revision, secondSemantic.revision)
	assert.Equal(t, firstSemantic.payload, secondSemantic.payload)
}

func TestNormalizeOpenCodeGoCapabilityCatalogRejectsAmbiguousOrMalformedSources(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "duplicate provider",
			body: `{"opencode-go":{"id":"opencode-go","models":{"a":{"id":"a"}}},"opencode-go":{"id":"opencode-go","models":{"b":{"id":"b"}}}}`,
		},
		{
			name: "duplicate model",
			body: `{"opencode-go":{"id":"opencode-go","models":{"a":{"id":"a"},"a":{"id":"a"}}}}`,
		},
		{
			name: "duplicate effort entries",
			body: `{"opencode-go":{"id":"opencode-go","models":{"a":{"id":"a","reasoning_options":[{"type":"effort","values":["low"]},{"type":"effort","values":["high"]}]}}}}`,
		},
		{
			name: "null options",
			body: `{"opencode-go":{"id":"opencode-go","models":{"a":{"id":"a","reasoning_options":null}}}}`,
		},
		{
			name: "malformed option type",
			body: `{"opencode-go":{"id":"opencode-go","models":{"a":{"id":"a","reasoning_options":[{"type":null}]}}}}`,
		},
		{
			name: "empty effort id",
			body: `{"opencode-go":{"id":"opencode-go","models":{"a":{"id":"a","reasoning_options":[{"type":"effort","values":[""]}]}}}}`,
		},
		{
			name: "model id mismatch",
			body: `{"opencode-go":{"id":"opencode-go","models":{"a":{"id":"A"}}}}`,
		},
		{
			name: "trailing data",
			body: `{"opencode-go":{"id":"opencode-go","models":{"a":{"id":"a"}}}} true`,
		},
		{
			name: "unpaired surrogate",
			body: `{"opencode-go":{"id":"opencode-go","models":{"\uD800":{"id":"\uD800"}}}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := normalizeOpenCodeGoCapabilityCatalog([]byte(test.body))
			assert.ErrorIs(t, err, errOpenCodeGoCapabilityInvalidCatalog)
		})
	}

	oversized := make([]byte, openCodeGoCapabilityCatalogMaxBytes+1)
	_, err := normalizeOpenCodeGoCapabilityCatalog(oversized)
	assert.ErrorIs(t, err, errOpenCodeGoCapabilityInvalidCatalog)
}

func TestNormalizeOpenCodeGoCapabilityCatalogIgnoresNonEffortReasoningOptions(t *testing.T) {
	semantic, err := normalizeOpenCodeGoCapabilityCatalog([]byte(`{
		"opencode-go":{"id":"opencode-go","models":{
			"future":{"id":"future","reasoning_options":[
				{"type":"future_control","new_member":{"nested":true}},
				{"type":"effort","values":["low"]}
			]}
		}}
	}`))
	require.NoError(t, err)
	assert.True(t, semantic.models["future"].optionsKnown)
	assert.Equal(t, map[string]struct{}{"low": {}}, semantic.models["future"].efforts)
}

func TestNormalizeOpenCodeGoCapabilityCatalogEnforcesIdentifierAndCountBounds(t *testing.T) {
	longModelID := strings.Repeat("m", openCodeGoCapabilityMaxModelIDBytes+1)
	longEffortID := strings.Repeat("e", openCodeGoCapabilityMaxEffortIDBytes+1)
	tooManyOptions := strings.TrimSuffix(strings.Repeat(`{"type":"toggle"},`, openCodeGoCapabilityMaxOptionsPerModel+1), ",")
	tooManyEfforts := strings.TrimSuffix(strings.Repeat(`"low",`, openCodeGoCapabilityMaxEffortsPerOption+1), ",")
	tests := []string{
		fmt.Sprintf(`{"opencode-go":{"id":"opencode-go","models":{%q:{"id":%q}}}}`, longModelID, longModelID),
		fmt.Sprintf(`{"opencode-go":{"id":"opencode-go","models":{"a":{"id":"a","reasoning_options":[{"type":"effort","values":[%q]}]}}}}`, longEffortID),
		fmt.Sprintf(`{"opencode-go":{"id":"opencode-go","models":{"a":{"id":"a","reasoning_options":[%s]}}}}`, tooManyOptions),
		fmt.Sprintf(`{"opencode-go":{"id":"opencode-go","models":{"a":{"id":"a","reasoning_options":[{"type":"effort","values":[%s]}]}}}}`, tooManyEfforts),
	}
	for index, body := range tests {
		t.Run(fmt.Sprintf("bound_%d", index), func(t *testing.T) {
			_, err := normalizeOpenCodeGoCapabilityCatalog([]byte(body))
			assert.ErrorIs(t, err, errOpenCodeGoCapabilityInvalidCatalog)
		})
	}

	var tooManyModels strings.Builder
	tooManyModels.WriteString(`{"opencode-go":{"id":"opencode-go","models":{`)
	for index := 0; index < openCodeGoCapabilityMaxModels+1; index++ {
		if index > 0 {
			tooManyModels.WriteByte(',')
		}
		_, _ = fmt.Fprintf(&tooManyModels, `"m%d":{"id":"m%d"}`, index, index)
	}
	tooManyModels.WriteString(`}}}`)
	_, err := normalizeOpenCodeGoCapabilityCatalog([]byte(tooManyModels.String()))
	assert.ErrorIs(t, err, errOpenCodeGoCapabilityInvalidCatalog)

	_, err = parseOpenCodeGoCapabilityNormalizedPayload(strings.Repeat(" ", openCodeGoCapabilityNormalizedMaxBytes+1))
	assert.ErrorIs(t, err, errOpenCodeGoCapabilityInvalidCatalog)
}

func TestOpenCodeGoCapabilityViewUsesExactModelIDsAndFreshExplicitOptions(t *testing.T) {
	semantic, err := normalizeOpenCodeGoCapabilityCatalog([]byte(openCodeGoCapabilityCatalogFixture))
	require.NoError(t, err)
	now := time.Unix(100_000, 0)
	view := OpenCodeGoCapabilityView{
		index: &openCodeGoCapabilityIndex{
			generation: 1,
			checkedAt:  now.Unix(),
			semantic:   semantic,
		},
		freshness: OpenCodeGoCapabilityFreshnessFresh,
	}
	assert.Equal(t, OpenCodeGoCapabilitySupported, view.CheckEffort("Case/Model", "low"))
	assert.Equal(t, OpenCodeGoCapabilitySupported, view.CheckEffort("Case/Model", "none"))
	assert.Equal(t, OpenCodeGoCapabilityUnsupported, view.CheckEffort("Case/Model", "max"))
	assert.Equal(t, OpenCodeGoCapabilityUnsupported, view.CheckEffort("empty", "low"))
	assert.Equal(t, OpenCodeGoCapabilityUnsupported, view.CheckEffort("toggle", "low"))
	assert.Equal(t, OpenCodeGoCapabilityUnknown, view.CheckEffort("fallback", "low"))
	assert.Equal(t, OpenCodeGoCapabilityUnknown, view.CheckEffort("case/model", "low"))
	assert.Equal(t, OpenCodeGoCapabilityUnknown, view.CheckEffort("prefix/Case/Model", "low"))
}

func TestOpenCodeGoCapabilityFreshnessBoundaries(t *testing.T) {
	semantic, err := normalizeOpenCodeGoCapabilityCatalog([]byte(openCodeGoCapabilityCatalogFixture))
	require.NoError(t, err)
	checked := time.Unix(50_000, 0)
	index := &openCodeGoCapabilityIndex{checkedAt: checked.Unix(), semantic: semantic}

	assert.Equal(t, OpenCodeGoCapabilityFreshnessFresh, openCodeGoCapabilityFreshnessAt(index, checked.Add(2*time.Hour)))
	assert.Equal(t, OpenCodeGoCapabilityFreshnessWarning, openCodeGoCapabilityFreshnessAt(index, checked.Add(2*time.Hour+time.Second)))
	assert.Equal(t, OpenCodeGoCapabilityFreshnessWarning, openCodeGoCapabilityFreshnessAt(index, checked.Add(24*time.Hour)))
	assert.Equal(t, OpenCodeGoCapabilityFreshnessExpired, openCodeGoCapabilityFreshnessAt(index, checked.Add(24*time.Hour+time.Second)))
	assert.Equal(t, OpenCodeGoCapabilityFreshnessInvalid, openCodeGoCapabilityFreshnessAt(index, checked.Add(-time.Second)))

	expired := OpenCodeGoCapabilityView{index: index, freshness: OpenCodeGoCapabilityFreshnessExpired}
	assert.Equal(t, OpenCodeGoCapabilityUnknown, expired.CheckEffort("Case/Model", "low"))
}

func TestBundledOpenCodeGoCapabilitySeedIsNormalizedAndTimestamped(t *testing.T) {
	semantic, err := parseOpenCodeGoCapabilityNormalizedPayload(openCodeGoCapabilitySeedPayload)
	require.NoError(t, err)
	assert.Equal(t, openCodeGoCapabilitySeedRevision, semantic.revision)
	assert.Greater(t, openCodeGoCapabilitySeedCheckedAt, int64(0))
	assert.Equal(t, OpenCodeGoCapabilitySupported, OpenCodeGoCapabilityView{
		index: &openCodeGoCapabilityIndex{
			checkedAt: openCodeGoCapabilitySeedCheckedAt,
			semantic:  semantic,
		},
		freshness: OpenCodeGoCapabilityFreshnessFresh,
	}.CheckEffort("deepseek-v4-flash", "high"))
	assert.Equal(t, OpenCodeGoCapabilityUnsupported, OpenCodeGoCapabilityView{
		index: &openCodeGoCapabilityIndex{
			checkedAt: openCodeGoCapabilitySeedCheckedAt,
			semantic:  semantic,
		},
		freshness: OpenCodeGoCapabilityFreshnessFresh,
	}.CheckEffort("deepseek-v4-flash", "xhigh"))
	assert.True(t, strings.HasPrefix(openCodeGoCapabilitySeedETag, `"`))
}

func TestPinnedOpenCodeGoCapabilityViewDoesNotMixConcurrentRevision(t *testing.T) {
	oldCurrent := openCodeGoCapabilityCurrent.Load()
	oldObserved := openCodeGoCapabilityObserved
	t.Cleanup(func() {
		openCodeGoCapabilityCurrent.Store(oldCurrent)
		openCodeGoCapabilityObserved = oldObserved
	})
	openCodeGoCapabilityCurrent.Store(nil)

	oldSemantic, err := normalizeOpenCodeGoCapabilityCatalog([]byte(`{"opencode-go":{"id":"opencode-go","models":{"m":{"id":"m","reasoning_options":[{"type":"effort","values":["low"]}]}}}}`))
	require.NoError(t, err)
	newSemantic, err := normalizeOpenCodeGoCapabilityCatalog([]byte(`{"opencode-go":{"id":"opencode-go","models":{"m":{"id":"m","reasoning_options":[{"type":"effort","values":["high"]}]}}}}`))
	require.NoError(t, err)
	now := time.Unix(200_000, 0)
	require.True(t, publishOpenCodeGoCapabilityIndex(&openCodeGoCapabilityIndex{
		generation: 1,
		checkedAt:  now.Unix(),
		semantic:   oldSemantic,
	}))
	pinned := PinOpenCodeGoCapabilityView(now)
	require.True(t, publishOpenCodeGoCapabilityIndex(&openCodeGoCapabilityIndex{
		generation: 2,
		checkedAt:  now.Add(time.Second).Unix(),
		semantic:   newSemantic,
	}))

	assert.Equal(t, oldSemantic.revision, pinned.SemanticRevision())
	assert.Equal(t, OpenCodeGoCapabilitySupported, pinned.CheckEffort("m", "low"))
	assert.Equal(t, OpenCodeGoCapabilityUnsupported, pinned.CheckEffort("m", "high"))
	current := PinOpenCodeGoCapabilityView(now.Add(time.Second))
	assert.Equal(t, newSemantic.revision, current.SemanticRevision())
	assert.Equal(t, OpenCodeGoCapabilitySupported, current.CheckEffort("m", "high"))
}

func TestOpenCodeGoCapabilityIndexRejectsRevisionOrPayloadDrift(t *testing.T) {
	semantic, err := parseOpenCodeGoCapabilityNormalizedPayload(openCodeGoCapabilitySeedPayload)
	require.NoError(t, err)
	row := &model.OpenCodeGoCapabilitySnapshot{
		Provider:          model.OpenCodeGoCapabilityProvider,
		Generation:        1,
		SchemaVersion:     openCodeGoCapabilitySchemaVersion,
		SemanticRevision:  semantic.revision,
		SourceETag:        `"fixture"`,
		CheckedAt:         100,
		NormalizedPayload: semantic.payload,
	}
	_, err = openCodeGoCapabilityIndexFromRow(row)
	require.NoError(t, err)
	row.SemanticRevision = strings.Repeat("0", 64)
	_, err = openCodeGoCapabilityIndexFromRow(row)
	assert.ErrorIs(t, err, errOpenCodeGoCapabilityInvalidCatalog)
	row.SemanticRevision = semantic.revision
	row.SourceETag = strings.Repeat("e", 257)
	_, err = openCodeGoCapabilityIndexFromRow(row)
	assert.ErrorIs(t, err, errOpenCodeGoCapabilityInvalidCatalog)
}

func TestOpenCodeGoCapabilityAtomicPublicationIsRaceSafe(t *testing.T) {
	oldCurrent := openCodeGoCapabilityCurrent.Load()
	t.Cleanup(func() { openCodeGoCapabilityCurrent.Store(oldCurrent) })
	openCodeGoCapabilityCurrent.Store(nil)
	semantic, err := normalizeOpenCodeGoCapabilityCatalog([]byte(openCodeGoCapabilityCatalogFixture))
	require.NoError(t, err)
	base := time.Unix(300_000, 0)

	var wait sync.WaitGroup
	for generation := int64(1); generation <= 32; generation++ {
		generation := generation
		wait.Add(1)
		go func() {
			defer wait.Done()
			publishOpenCodeGoCapabilityIndex(&openCodeGoCapabilityIndex{
				generation: generation,
				checkedAt:  base.Add(time.Duration(generation) * time.Second).Unix(),
				semantic:   semantic,
			})
		}()
	}
	for reader := 0; reader < 32; reader++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < 100; iteration++ {
				view := PinOpenCodeGoCapabilityView(base.Add(time.Minute))
				if view.SemanticRevision() != "" {
					assert.Equal(t, semantic.revision, view.SemanticRevision())
				}
			}
		}()
	}
	wait.Wait()
	assert.Equal(t, base.Add(32*time.Second).Unix(), openCodeGoCapabilityCurrent.Load().checkedAt)
}
