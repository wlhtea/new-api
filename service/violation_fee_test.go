package service

import (
	"errors"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWrapAsViolationFeeGrokCSAMRejectsFreeTextEvidence(t *testing.T) {
	message := "provider rejected content: " + CSAMViolationMarker
	upstreamErr := types.NewOpenAIError(errors.New(message), types.ErrorCodeBadResponse, http.StatusBadRequest)
	MarkOpenCodeGoUpstreamRelayError(upstreamErr)

	normalized := WrapAsViolationFeeGrokCSAM(upstreamErr)

	require.NotNil(t, normalized)
	assert.Equal(t, message, normalized.Error())
	assert.True(t, IsOpenCodeGoUpstreamRelayError(normalized))
	assert.Equal(t, types.ErrorCodeBadResponse, normalized.GetErrorCode())
	assert.False(t, types.IsSkipRetryError(normalized))
	assert.Equal(t, http.StatusBadRequest, PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, normalized).StatusCode)
	assert.Equal(t, message, upstreamErr.Error())
}

func TestNormalizeViolationFeeErrorPreservesOpenCodeGoUpstreamOrigin(t *testing.T) {
	tests := []struct {
		name string
		err  *types.NewAPIError
	}{
		{
			name: "trusted stable violation fee code",
			err: types.NewOpenAIError(
				errors.New("upstream policy rejection"),
				types.ErrorCodeViolationFeeGrokCSAM,
				http.StatusBadRequest,
			),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			originalMessage := test.err.Error()
			MarkOpenCodeGoUpstreamRelayError(test.err)

			normalized := NormalizeViolationFeeError(test.err)

			require.NotNil(t, normalized)
			assert.Equal(t, originalMessage, normalized.Error())
			assert.True(t, IsOpenCodeGoUpstreamRelayError(normalized))
			assert.True(t, types.IsSkipRetryError(normalized))
			assert.Equal(t, http.StatusBadRequest, PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, normalized).StatusCode)
			assert.Equal(t, originalMessage, test.err.Error())
		})
	}
}

func TestNormalizeViolationFeeErrorPreservesRawOpenCodeGoUpstreamStatus(t *testing.T) {
	upstreamErr := types.NewOpenAIError(
		errors.New("provider rejected content: "+CSAMViolationMarker),
		types.ErrorCodeBadResponse,
		http.StatusServiceUnavailable,
	)
	MarkOpenCodeGoUpstreamRelayError(upstreamErr)
	ResetStatusCode(upstreamErr, `{"503":400}`)

	normalized := NormalizeViolationFeeError(upstreamErr)

	require.NotNil(t, normalized)
	statusCode, ok := openCodeGoUpstreamRelayStatusCode(normalized)
	require.True(t, ok)
	assert.Equal(t, http.StatusServiceUnavailable, statusCode)
	assert.Equal(t, http.StatusBadRequest, normalized.StatusCode)
	assert.True(t, ShouldRetryOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, normalized))
}

func TestViolationFeeRequiresTrustedUpstreamOriginAndExactCode(t *testing.T) {
	localStableCode := types.NewOpenAIError(
		errors.New("local validation"),
		types.ErrorCodeViolationFeeGrokCSAM,
		http.StatusBadRequest,
	)
	upstreamTextOnly := MarkOpenCodeGoUpstreamRelayError(types.NewOpenAIError(
		errors.New("provider rejected content: "+CSAMViolationMarker),
		types.ErrorCodeBadResponse,
		http.StatusBadRequest,
	))
	trusted := MarkOpenCodeGoUpstreamRelayError(types.NewOpenAIError(
		errors.New("upstream policy rejection"),
		types.ErrorCodeViolationFeeGrokCSAM,
		http.StatusBadRequest,
	))

	assert.False(t, shouldChargeViolationFee(localStableCode))
	assert.False(t, shouldChargeViolationFee(upstreamTextOnly))
	assert.True(t, shouldChargeViolationFee(trusted))
}
