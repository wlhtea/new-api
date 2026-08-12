package opencodego

import (
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdaptorImmediateRetryReusesFirstWorkspaceSelection(t *testing.T) {
	originalSelector := selectOpenCodeGoWorkspace
	selectorCalls := 0
	selection := &service.OpenCodeGoPoolSelection{
		WorkspaceID:     1,
		WorkspaceUID:    "workspace-pinned",
		IdentityUID:     "identity-pinned",
		APIKey:          "key-a",
		FailoverAttempt: &service.OpenCodeGoFailoverAttempt{},
	}
	selectOpenCodeGoWorkspace = func(_ int, _ string, _ service.OpenCodeGoPoolSelectOptions) (*service.OpenCodeGoPoolSelection, error) {
		selectorCalls++
		return selection, nil
	}
	t.Cleanup(func() { selectOpenCodeGoWorkspace = originalSelector })

	c := newAdaptorTestContext()
	info := newAdaptorTestInfo("glm-5.2", false)
	info.ChannelId = 42
	service.BeginOpenCodeGoImmediateRetry(c)
	t.Cleanup(func() { service.EndOpenCodeGoImmediateRetry(c) })

	first := &Adaptor{}
	first.Init(info)
	_, err := first.ConvertOpenAIRequest(c, info, requestForFormat(types.RelayFormatOpenAI).(*dto.GeneralOpenAIRequest))
	require.NoError(t, err)
	firstHeader := make(http.Header)
	require.NoError(t, first.SetupRequestHeader(c, &firstHeader, info))
	assert.Equal(t, 1, selectorCalls)
	assert.Equal(t, "workspace-pinned", first.selectedWorkspaceUID)
	assert.Same(t, selection.FailoverAttempt, first.failoverAttempt)
	first.releaseInFlight()

	service.PrepareOpenCodeGoImmediateRetry(c)
	second := &Adaptor{}
	second.Init(info)
	_, err = second.ConvertOpenAIRequest(c, info, requestForFormat(types.RelayFormatOpenAI).(*dto.GeneralOpenAIRequest))
	require.NoError(t, err)
	secondHeader := make(http.Header)
	require.NoError(t, second.SetupRequestHeader(c, &secondHeader, info))
	t.Cleanup(second.releaseInFlight)

	assert.Equal(t, 1, selectorCalls)
	assert.Equal(t, "workspace-pinned", second.selectedWorkspaceUID)
	assert.Equal(t, "identity-pinned", second.selectedIdentityUID)
	assert.Same(t, selection.FailoverAttempt, second.failoverAttempt)
	assert.Equal(t, "Bearer key-a", secondHeader.Get("Authorization"))
}

func TestAdaptorImmediateRetryReselectsAfterPoolExhaustion(t *testing.T) {
	originalSelector := selectOpenCodeGoWorkspace
	selectorCalls := 0
	selectOpenCodeGoWorkspace = func(_ int, _ string, _ service.OpenCodeGoPoolSelectOptions) (*service.OpenCodeGoPoolSelection, error) {
		selectorCalls++
		if selectorCalls == 1 {
			return nil, service.ErrOpenCodeGoNoEligibleWorkspace
		}
		return &service.OpenCodeGoPoolSelection{
			WorkspaceID:  2,
			WorkspaceUID: "workspace-recovered",
			IdentityUID:  "identity-recovered",
			APIKey:       "key-b",
		}, nil
	}
	t.Cleanup(func() { selectOpenCodeGoWorkspace = originalSelector })

	c := newAdaptorTestContext()
	info := newAdaptorTestInfo("glm-5.2", false)
	info.ChannelId = 42
	service.BeginOpenCodeGoImmediateRetry(c)
	t.Cleanup(func() { service.EndOpenCodeGoImmediateRetry(c) })

	first := &Adaptor{}
	first.Init(info)
	_, err := first.ConvertOpenAIRequest(c, info, requestForFormat(types.RelayFormatOpenAI).(*dto.GeneralOpenAIRequest))
	require.NoError(t, err)
	firstHeader := make(http.Header)
	err = first.SetupRequestHeader(c, &firstHeader, info)
	require.ErrorIs(t, err, service.ErrOpenCodeGoNoEligibleWorkspace)
	assert.Equal(t, 1, selectorCalls)

	service.PrepareOpenCodeGoImmediateRetry(c)
	second := &Adaptor{}
	second.Init(info)
	_, err = second.ConvertOpenAIRequest(c, info, requestForFormat(types.RelayFormatOpenAI).(*dto.GeneralOpenAIRequest))
	require.NoError(t, err)
	secondHeader := make(http.Header)
	require.NoError(t, second.SetupRequestHeader(c, &secondHeader, info))
	t.Cleanup(second.releaseInFlight)

	assert.Equal(t, 2, selectorCalls)
	assert.Equal(t, "workspace-recovered", second.selectedWorkspaceUID)
	assert.Equal(t, "Bearer key-b", secondHeader.Get("Authorization"))
}

func TestAdaptorImmediateRetryCountsOnlyFinalFailoverFailure(t *testing.T) {
	originalObserver := observeOpenCodeGoFailoverFailure
	observerCalls := 0
	observeOpenCodeGoFailoverFailure = func(_ *service.OpenCodeGoFailoverAttempt, _ time.Time) (service.OpenCodeGoFailoverObservation, error) {
		observerCalls++
		return service.OpenCodeGoFailoverObservation{}, nil
	}
	t.Cleanup(func() { observeOpenCodeGoFailoverFailure = originalObserver })

	c := newAdaptorTestContext()
	info := newAdaptorTestInfo("glm-5.2", false)
	info.ChannelId = 42
	attempt := &service.OpenCodeGoFailoverAttempt{}
	service.BeginOpenCodeGoImmediateRetry(c)
	t.Cleanup(func() { service.EndOpenCodeGoImmediateRetry(c) })

	first := &Adaptor{
		protocol:             ProtocolChat,
		workspaceSelected:    true,
		selectedWorkspaceUID: "workspace-pinned",
		failoverAttempt:      attempt,
	}
	first.recordFailoverFailure(c, info, "http_503")
	assert.Zero(t, observerCalls)

	service.PrepareOpenCodeGoImmediateRetry(c)
	second := &Adaptor{
		protocol:             ProtocolChat,
		workspaceSelected:    true,
		selectedWorkspaceUID: "workspace-pinned",
		failoverAttempt:      attempt,
	}
	second.recordFailoverFailure(c, info, "http_502")
	assert.Equal(t, 1, observerCalls)
}

func TestAdaptorImmediateRetryFlushesFirstFailureWhenRetryIsRejected(t *testing.T) {
	originalObserver := observeOpenCodeGoFailoverFailure
	observerCalls := 0
	observeOpenCodeGoFailoverFailure = func(_ *service.OpenCodeGoFailoverAttempt, _ time.Time) (service.OpenCodeGoFailoverObservation, error) {
		observerCalls++
		return service.OpenCodeGoFailoverObservation{}, nil
	}
	t.Cleanup(func() { observeOpenCodeGoFailoverFailure = originalObserver })

	c := newAdaptorTestContext()
	info := newAdaptorTestInfo("glm-5.2", false)
	info.ChannelId = 42
	service.BeginOpenCodeGoImmediateRetry(c)
	t.Cleanup(func() { service.EndOpenCodeGoImmediateRetry(c) })

	adaptor := &Adaptor{
		protocol:             ProtocolChat,
		workspaceSelected:    true,
		selectedWorkspaceUID: "workspace-pinned",
		failoverAttempt:      &service.OpenCodeGoFailoverAttempt{},
	}
	adaptor.recordFailoverFailure(c, info, "http_503")
	assert.Zero(t, observerCalls)

	service.FlushOpenCodeGoImmediateRetryFailover(c)
	service.FlushOpenCodeGoImmediateRetryFailover(c)
	assert.Equal(t, 1, observerCalls)
}

func TestAdaptorImmediateRetryDoesNotReuseSelectionOutsideRetryScope(t *testing.T) {
	c := newAdaptorTestContext()
	selection := &service.OpenCodeGoPoolSelection{WorkspaceUID: "workspace-private"}

	service.RememberOpenCodeGoImmediateRetrySelection(c, selection)
	got, replaying := service.OpenCodeGoImmediateRetrySelection(c)
	assert.Nil(t, got)
	assert.False(t, replaying)

	service.BeginOpenCodeGoImmediateRetry(c)
	service.RememberOpenCodeGoImmediateRetrySelection(c, selection)
	service.EndOpenCodeGoImmediateRetry(c)
	got, replaying = service.OpenCodeGoImmediateRetrySelection(c)
	assert.Nil(t, got)
	assert.False(t, replaying)
}
