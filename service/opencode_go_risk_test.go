package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeOpenCodeGoRiskProbe struct {
	mu        sync.Mutex
	response  OpenCodeGoRiskProbeResponse
	err       error
	delay     time.Duration
	active    int
	maxActive int
	apiKeys   []string
	models    []string
}

func (fake *fakeOpenCodeGoRiskProbe) Probe(
	ctx context.Context,
	apiKey string,
	modelID string,
) (OpenCodeGoRiskProbeResponse, error) {
	fake.mu.Lock()
	fake.active++
	if fake.active > fake.maxActive {
		fake.maxActive = fake.active
	}
	fake.apiKeys = append(fake.apiKeys, apiKey)
	fake.models = append(fake.models, modelID)
	fake.mu.Unlock()

	if fake.delay > 0 {
		select {
		case <-ctx.Done():
			fake.finish()
			return OpenCodeGoRiskProbeResponse{}, ctx.Err()
		case <-time.After(fake.delay):
		}
	}
	fake.finish()
	return fake.response, fake.err
}

func (fake *fakeOpenCodeGoRiskProbe) finish() {
	fake.mu.Lock()
	fake.active--
	fake.mu.Unlock()
}

func seedOpenCodeGoRiskWorkspace(
	t *testing.T,
	codec *OpenCodeGoCredentialCodec,
	channelID int,
	identityID int64,
	uid string,
	apiKey string,
) model.OpenCodeGoWorkspace {
	t.Helper()
	workspace := seedOpenCodeGoHealthWorkspace(t, channelID, identityID, uid, openCodeGoDefaultRiskProbeModel)
	ciphertext, err := codec.Encrypt(OpenCodeGoCredentialAPIKey, channelID, workspace.UID, apiKey)
	require.NoError(t, err)
	detectedAt := time.Unix(1_900_000_000, 0)
	require.NoError(t, model.DB.Model(&model.OpenCodeGoWorkspace{}).
		Where("id = ?", workspace.ID).
		Updates(map[string]interface{}{
			"api_key_ciphertext":   ciphertext,
			"effective_state":      model.OpenCodeGoStateRiskBlocked,
			"state_reason":         "request blocked by upstream provider",
			"health_observation":   string(OpenCodeGoObservationRiskBlocked),
			"health_observed_at":   detectedAt.UnixNano(),
			"risk_detected_at":     detectedAt.Unix(),
			"risk_last_checked_at": detectedAt.Unix(),
		}).Error)
	workspace.APIKeyCiphertext = ciphertext
	workspace.EffectiveState = model.OpenCodeGoStateRiskBlocked
	return workspace
}

func TestOpenCodeGoRiskRecheckSuccessIsRequiredForRecovery(t *testing.T) {
	_, channel, codec := setupOpenCodeGoPoolTestDB(t)
	identity := model.OpenCodeGoIdentity{
		UID:                   "identity-risk-success",
		ChannelID:             channel.Id,
		AuthCookieCiphertext:  "encrypted-cookie-risk-success",
		AuthCookieFingerprint: "fingerprint-risk-success",
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, model.DB.Create(&identity).Error)
	workspace := seedOpenCodeGoRiskWorkspace(t, codec, channel.Id, identity.ID, "workspace-risk-success", "risk-probe-key")

	fake := &fakeOpenCodeGoRiskProbe{response: OpenCodeGoRiskProbeResponse{StatusCode: http.StatusOK}}
	riskService := newOpenCodeGoRiskRecheckService(fake, codec)
	riskService.now = func() time.Time { return time.Unix(1_900_000_100, 0) }
	rebuilds := 0
	riskService.rebuild = func(gotChannelID int) error {
		rebuilds++
		assert.Equal(t, channel.Id, gotChannelID)
		return nil
	}

	result, err := riskService.RecheckWorkspace(context.Background(), channel.Id, workspace.UID, "manual")
	require.NoError(t, err)
	assert.Equal(t, "recovered", result.Status)
	assert.False(t, result.Blocked)
	assert.Equal(t, 1, rebuilds)
	assert.Equal(t, []string{"risk-probe-key"}, fake.apiKeys)
	assert.Equal(t, []string{openCodeGoDefaultRiskProbeModel}, fake.models)

	after, err := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
	require.NoError(t, err)
	assert.Equal(t, model.OpenCodeGoStateEligible, after.EffectiveState)
	assert.Zero(t, after.RiskDetectedAt)

	var operations []model.OpenCodeGoOperation
	require.NoError(t, model.DB.Where("workspace_id = ?", workspace.ID).Find(&operations).Error)
	require.Len(t, operations, 1)
	assert.Equal(t, OpenCodeGoOperationStatusSucceeded, operations[0].Status)
	assert.Equal(t, OpenCodeGoOperationRiskRecheck, operations[0].Action)
}

func TestOpenCodeGoRiskRecheckDoesNotClearOnUnrelatedAuthError(t *testing.T) {
	_, channel, codec := setupOpenCodeGoPoolTestDB(t)
	identity := model.OpenCodeGoIdentity{
		UID:                   "identity-risk-auth",
		ChannelID:             channel.Id,
		AuthCookieCiphertext:  "encrypted-cookie-risk-auth",
		AuthCookieFingerprint: "fingerprint-risk-auth",
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, model.DB.Create(&identity).Error)
	workspace := seedOpenCodeGoRiskWorkspace(t, codec, channel.Id, identity.ID, "workspace-risk-auth", "risk-auth-key")
	failure := OpenCodeGoProviderFailure{
		StatusCode: http.StatusUnauthorized,
		ErrorType:  "AuthError",
		Message:    "Invalid API key",
	}
	fake := &fakeOpenCodeGoRiskProbe{response: OpenCodeGoRiskProbeResponse{StatusCode: http.StatusUnauthorized, Failure: &failure}}
	riskService := newOpenCodeGoRiskRecheckService(fake, codec)
	checkedAt := time.Unix(1_900_000_200, 0)
	riskService.now = func() time.Time { return checkedAt }
	riskService.rebuild = func(int) error { return nil }

	result, err := riskService.RecheckWorkspace(context.Background(), channel.Id, workspace.UID, "manual")
	require.NoError(t, err)
	assert.Equal(t, "not_recovered", result.Status)
	assert.False(t, result.Blocked)

	after, err := model.GetOpenCodeGoWorkspace(channel.Id, workspace.UID)
	require.NoError(t, err)
	assert.Equal(t, model.OpenCodeGoStateRiskBlocked, after.EffectiveState)
	assert.Equal(t, checkedAt.Unix(), after.RiskLastCheckedAt)
}

func TestOpenCodeGoRiskBatchSerializesWorkspacesSharingIdentity(t *testing.T) {
	_, channel, codec := setupOpenCodeGoPoolTestDB(t)
	identity := model.OpenCodeGoIdentity{
		UID:                   "identity-risk-batch",
		ChannelID:             channel.Id,
		AuthCookieCiphertext:  "encrypted-cookie-risk-batch",
		AuthCookieFingerprint: "fingerprint-risk-batch",
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, model.DB.Create(&identity).Error)
	seedOpenCodeGoRiskWorkspace(t, codec, channel.Id, identity.ID, "workspace-risk-batch-a", "risk-batch-key-a")
	seedOpenCodeGoRiskWorkspace(t, codec, channel.Id, identity.ID, "workspace-risk-batch-b", "risk-batch-key-b")

	fake := &fakeOpenCodeGoRiskProbe{
		response: OpenCodeGoRiskProbeResponse{StatusCode: http.StatusOK},
		delay:    20 * time.Millisecond,
	}
	riskService := newOpenCodeGoRiskRecheckService(fake, codec)
	riskService.now = func() time.Time { return time.Unix(1_900_000_300, 0) }
	riskService.rebuild = func(int) error { return nil }

	summary, err := riskService.RecheckRiskWorkspaces(
		context.Background(),
		channel.Id,
		2,
		100,
		"task",
		nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 2, summary.Total)
	assert.Equal(t, 2, summary.Recovered)
	assert.Equal(t, 1, fake.maxActive)
}

func TestOpenCodeGoRiskProbeClientSendsMinimalNonStreamingRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/chat/completions", request.URL.Path)
		assert.Equal(t, "Bearer probe-key", request.Header.Get("Authorization"))
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		var payload openCodeGoRiskProbeRequest
		require.NoError(t, common.Unmarshal(body, &payload))
		assert.Equal(t, openCodeGoDefaultRiskProbeModel, payload.Model)
		assert.Equal(t, 1, payload.MaxTokens)
		assert.False(t, payload.Stream)
		require.Len(t, payload.Messages, 1)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"id":"probe"}`))
	}))
	defer server.Close()

	client, err := newOpenCodeGoHTTPRiskProbeClient(server.URL, server.Client())
	require.NoError(t, err)
	response, err := client.Probe(context.Background(), "probe-key", openCodeGoDefaultRiskProbeModel)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Nil(t, response.Failure)
}

func TestOpenCodeGoRiskProbeTransportErrorIsSanitized(t *testing.T) {
	_, channel, codec := setupOpenCodeGoPoolTestDB(t)
	identity := model.OpenCodeGoIdentity{
		UID:                   "identity-risk-error",
		ChannelID:             channel.Id,
		AuthCookieCiphertext:  "encrypted-cookie-risk-error",
		AuthCookieFingerprint: "fingerprint-risk-error",
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, model.DB.Create(&identity).Error)
	workspace := seedOpenCodeGoRiskWorkspace(t, codec, channel.Id, identity.ID, "workspace-risk-error", "private-probe-key")
	fake := &fakeOpenCodeGoRiskProbe{err: errors.New("transport failed with private-probe-key")}
	riskService := newOpenCodeGoRiskRecheckService(fake, codec)
	riskService.now = func() time.Time { return time.Unix(1_900_000_400, 0) }
	riskService.rebuild = func(int) error { return nil }

	result, err := riskService.RecheckWorkspace(context.Background(), channel.Id, workspace.UID, "manual")
	require.Error(t, err)
	assert.NotContains(t, result.Error, "private-probe-key")
	assert.NotContains(t, err.Error(), "private-probe-key")
}
