package opencodego

import (
	"errors"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyChannelIgnoresResidualPoolSettings(t *testing.T) {
	originalSelector := selectOpenCodeGoWorkspace
	selectorCalled := false
	selectOpenCodeGoWorkspace = func(_ int, _ string, _ service.OpenCodeGoPoolSelectOptions) (*service.OpenCodeGoPoolSelection, error) {
		selectorCalled = true
		return nil, errors.New("workspace selector must not run for an API-key channel")
	}
	t.Cleanup(func() { selectOpenCodeGoWorkspace = originalSelector })

	info := newAPIKeyAdaptorTestInfo("glm-5.2", false)
	info.ApiKey = "row-api-key"
	info.ChannelOtherSettings.OpenCodeGo = &dto.OpenCodeGoConfig{
		GenericFailoverEnabled: true,
		AffinityFallback:       "token",
		LoadAwareEnabled:       true,
		IdentityProxyEnabled:   true,
	}
	adaptor := &Adaptor{}
	adaptor.Init(info)

	_, err := adaptor.ConvertOpenAIRequest(
		newAdaptorTestContext(),
		info,
		requestForFormat(types.RelayFormatOpenAI).(*dto.GeneralOpenAIRequest),
	)
	require.NoError(t, err)

	header := http.Header{}
	require.NoError(t, adaptor.SetupRequestHeader(newAdaptorTestContext(), &header, info))
	assert.False(t, selectorCalled)
	assert.False(t, adaptor.workspaceSelected)
	assert.Equal(t, "row-api-key", info.ApiKey)
	assert.Equal(t, "Bearer row-api-key", header.Get("Authorization"))
}
