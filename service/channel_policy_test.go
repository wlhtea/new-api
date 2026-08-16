package service

import (
	"errors"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
)

func TestShouldDisableChannelRequiresRawOpenCodeHTTPEvidence(t *testing.T) {
	oldEnabled := common.AutomaticDisableChannelEnabled
	oldRanges := append([]operation_setting.StatusCodeRange(nil), operation_setting.AutomaticDisableStatusCodeRanges...)
	common.AutomaticDisableChannelEnabled = true
	operation_setting.AutomaticDisableStatusCodeRanges = []operation_setting.StatusCodeRange{{
		Start: http.StatusBadGateway,
		End:   http.StatusBadGateway,
	}}
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = oldEnabled
		operation_setting.AutomaticDisableStatusCodeRanges = oldRanges
	})

	responseLimit := types.NewOpenAIError(
		errors.New("response exceeded local limit"),
		types.ErrorCodeReadResponseBodyFailed,
		http.StatusBadGateway,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithProvenance(types.ErrorProvenance{
			Origin:  types.ErrorOriginGatewayInvariant,
			Subtype: "response_limit",
		}),
	)
	assert.False(t, ShouldDisableChannel(responseLimit))
	assert.False(t, IsOpenCodeGoUpstreamRelayError(responseLimit))
	assert.Zero(t, responseLimit.Provenance().RawStatusCode)

	rawBadGateway := MarkOpenCodeGoUpstreamRelayErrorWithStatus(types.NewOpenAIError(
		errors.New("trusted upstream HTTP failure"),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusBadGateway,
	), http.StatusBadGateway)
	assert.True(t, ShouldDisableChannel(rawBadGateway), "raw HTTP evidence remains an eligible disable signal")
}
