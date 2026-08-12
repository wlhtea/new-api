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

func TestWrapAsViolationFeeGrokCSAMPreservesOpenCodeGoUpstreamOrigin(t *testing.T) {
	message := "provider rejected content: " + CSAMViolationMarker
	upstreamErr := types.NewOpenAIError(errors.New(message), types.ErrorCodeBadResponse, http.StatusBadRequest)
	MarkOpenCodeGoUpstreamRelayError(upstreamErr)

	normalized := WrapAsViolationFeeGrokCSAM(upstreamErr)

	require.NotNil(t, normalized)
	assert.Equal(t, message, normalized.Error())
	assert.True(t, IsOpenCodeGoUpstreamRelayError(normalized))
	assert.Equal(t, types.ErrorCodeViolationFeeGrokCSAM, normalized.GetErrorCode())
	assert.True(t, types.IsSkipRetryError(normalized))
	assert.Equal(t, http.StatusTooManyRequests, PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, normalized).StatusCode)
	assert.Equal(t, message, upstreamErr.Error())
}

func TestNormalizeViolationFeeErrorPreservesOpenCodeGoUpstreamOrigin(t *testing.T) {
	tests := []struct {
		name string
		err  *types.NewAPIError
	}{
		{
			name: "CSAM marker",
			err: types.NewOpenAIError(
				errors.New("provider rejected content: "+CSAMViolationMarker),
				types.ErrorCodeBadResponse,
				http.StatusBadRequest,
			),
		},
		{
			name: "existing violation fee code",
			err: types.NewOpenAIError(
				errors.New("upstream policy rejection"),
				types.ErrorCode(ViolationFeeCodePrefix+"provider_policy"),
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
			assert.Equal(t, http.StatusTooManyRequests, PublicOpenCodeGoRelayError(constant.ChannelTypeOpenCodeGo, normalized).StatusCode)
			assert.Equal(t, originalMessage, test.err.Error())
		})
	}
}
