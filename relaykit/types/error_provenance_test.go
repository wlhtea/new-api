package types

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewAPIErrorProvenanceIsFirstWriteImmutable(t *testing.T) {
	err := NewOpenAIError(errors.New("upstream rejected request"), ErrorCodeBadResponse, http.StatusBadRequest)
	first := ErrorProvenance{
		Origin:        ErrorOriginUpstreamHTTP,
		Subtype:       "non_2xx",
		RawStatusCode: http.StatusBadRequest,
	}

	require.True(t, err.SetProvenance(first))
	assert.True(t, err.SetProvenance(first), "the same assignment is idempotent")
	assert.False(t, err.SetProvenance(ErrorProvenance{
		Origin:        ErrorOriginLocalValidation,
		Subtype:       "forged",
		RawStatusCode: http.StatusUnprocessableEntity,
	}))
	assert.Equal(t, first, err.Provenance())
	assert.True(t, err.Provenance().IsUpstream())
}

func TestErrOptionWithProvenanceDoesNotSerializeOrChangeMessage(t *testing.T) {
	provenance := ErrorProvenance{
		Origin:        ErrorOriginLocalCancel,
		Subtype:       "downstream_context",
		RawStatusCode: 0,
	}
	err := NewOpenAIError(
		errors.New("request canceled"),
		ErrorCodeBadResponse,
		499,
		ErrOptionWithProvenance(provenance),
	)

	assert.Equal(t, provenance, err.Provenance())
	assert.Equal(t, "request canceled", err.Error())
	assert.False(t, err.Provenance().IsUpstream())
	assert.True(t, err.Provenance().IsLocal())
	assert.NotContains(t, err.ToOpenAIError().Message, string(provenance.Origin))
}

func TestErrorProvenanceLocalAndUpstreamAxesAreDisjoint(t *testing.T) {
	for _, origin := range []ErrorOrigin{
		ErrorOriginLocalValidation,
		ErrorOriginLocalCancel,
		ErrorOriginLocalDeadline,
		ErrorOriginLocalWriter,
		ErrorOriginLocalPanic,
	} {
		provenance := ErrorProvenance{Origin: origin}
		assert.True(t, provenance.IsLocal(), origin)
		assert.False(t, provenance.IsUpstream(), origin)
	}
	provenance := ErrorProvenance{Origin: ErrorOriginUpstreamTransport}
	assert.True(t, provenance.IsUpstream())
	assert.False(t, provenance.IsLocal())
}
