package controller

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/opencodego"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func captureOpenCodeDiagnosticLogs(t *testing.T) (*bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	infoLogs := &bytes.Buffer{}
	errorLogs := &bytes.Buffer{}
	common.LogWriterMu.Lock()
	previousWriter := gin.DefaultWriter
	previousErrorWriter := gin.DefaultErrorWriter
	gin.DefaultWriter = infoLogs
	gin.DefaultErrorWriter = errorLogs
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultWriter = previousWriter
		gin.DefaultErrorWriter = previousErrorWriter
		common.LogWriterMu.Unlock()
	})
	return infoLogs, errorLogs
}

func TestOpenCodePreflightRejectionDiagnosticIsBoundedAndPrivate(t *testing.T) {
	_, errorLogs := captureOpenCodeDiagnosticLogs(t)
	body := []byte(`{
		"model":"diagnostic-model-private-glm-5.2",
		"max_tokens":16,
		"system":[{
			"type":"text",
			"text":"diagnostic-body-private",
			"cache_control":{"type":"ephemeral","ttl":"diagnostic-ttl-private"}
		}],
		"messages":[{"role":"user","content":"hello"}],
		"metadata":{"user_id":"diagnostic-metadata-private"}
	}`)
	c, info, _ := newOpenCodeControllerPreflightFixture(
		t,
		constant.ChannelTypeOpenCodeAPIKey,
		"/v1/messages",
		types.RelayFormatClaude,
		"diagnostic-model-private-glm-5.2",
		`{"diagnostic-model-private-glm-5.2":"glm-5.2"}`,
		body,
	)
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{
		OpenCodeGo: &dto.OpenCodeGoConfig{
			UnsupportedOptionalFieldPolicy: dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
		},
	})
	common.SetContextKey(c, constant.ContextKeyChannelName, "diagnostic-channel-private")
	common.SetContextKey(c, constant.ContextKeyChannelKey, "diagnostic-credential-private")
	c.Set(common.RequestIdKey, "diagnostic-request-id-private")
	c.Request.URL.RawQuery = "diagnostic-url-private=true"

	relayErr := preflightOpenCodeRequest(c, info)

	require.NotNil(t, relayErr)
	assert.Equal(t, http.StatusBadRequest, relayErr.StatusCode)
	assert.Equal(t, opencodego.CacheControlShapeRule, relayErr.Provenance().Subtype)
	logText := errorLogs.String()
	assert.Equal(t, 1, strings.Count(logText, "OpenCode request preflight rejected:"))
	assert.Contains(t, logText, "rule_id="+opencodego.CacheControlShapeRule)
	assert.Contains(t, logText, "stage="+opencodego.CacheControlPreflightStage)
	assert.Contains(t, logText, "status=400")
	assert.Contains(t, logText, "client_protocol=messages")
	assert.Contains(t, logText, "final_protocol=chat")
	assert.Contains(t, logText, "channel_type=63")
	assert.Contains(t, logText, "policy=drop_known_optional")
	assert.Contains(t, logText, "candidate_count=1")
	assert.Contains(t, logText, "preserve_count=0")
	assert.Contains(t, logText, "drop_count=0")
	requireOpenCodeDiagnosticFieldAllowlist(t, logText, "OpenCode request preflight rejected:")
	requireOpenCodeDiagnosticPrivacy(t, logText,
		"diagnostic-model-private",
		"diagnostic-body-private",
		"diagnostic-ttl-private",
		"diagnostic-metadata-private",
		"diagnostic-channel-private",
		"diagnostic-credential-private",
		"diagnostic-request-id-private",
		"diagnostic-url-private",
	)
}

func TestOpenCodeAllRejectedAPIKeyCandidatesLogMixedPolicyOnly(t *testing.T) {
	_, errorLogs := captureOpenCodeDiagnosticLogs(t)
	db := setupModelListControllerTestDB(t)
	const modelName = "diagnostic-candidate-model-private"

	strict := newOpenCodeAPIKeySnapshotTestChannel(
		"diagnostic-strict-channel-private",
		modelName,
		20,
		dto.OpenCodeGoProtocolChat,
	)
	strict.Key = "diagnostic-strict-credential-private"
	setCandidateCachePolicy(&strict, dto.OpenCodeGoUnsupportedOptionalFieldStrict)
	persistOpenCodeAPIKeySnapshotTestChannel(t, db, &strict)
	compatible := newOpenCodeAPIKeySnapshotTestChannel(
		"diagnostic-compatible-channel-private",
		modelName,
		10,
		dto.OpenCodeGoProtocolChat,
	)
	compatible.Key = "diagnostic-compatible-credential-private"
	setCandidateCachePolicy(&compatible, dto.OpenCodeGoUnsupportedOptionalFieldDropKnown)
	persistOpenCodeAPIKeySnapshotTestChannel(t, db, &compatible)
	c, info := newMalformedOpenCodeCacheCandidateFixture(t, &strict, modelName)

	plans, relayErr := prepareAndFreezeOpenCodeCandidatePlans(c, info)

	assert.Nil(t, plans)
	require.NotNil(t, relayErr)
	assert.Equal(t, http.StatusBadRequest, relayErr.StatusCode)
	assert.Equal(t, opencodego.CacheControlShapeRule, relayErr.Provenance().Subtype)
	logText := errorLogs.String()
	assert.Equal(t, 1, strings.Count(logText, "OpenCode request preflight rejected:"))
	assert.Contains(t, logText, "rule_id="+opencodego.CacheControlShapeRule)
	assert.Contains(t, logText, "stage="+opencodego.CacheControlPreflightStage)
	assert.Contains(t, logText, "status=400")
	assert.Contains(t, logText, "client_protocol=messages")
	assert.Contains(t, logText, "final_protocol=chat")
	assert.Contains(t, logText, "channel_type=63")
	assert.Contains(t, logText, "policy=mixed")
	assert.Contains(t, logText, "candidate_count=2")
	assert.Contains(t, logText, "preserve_count=0")
	assert.Contains(t, logText, "drop_count=0")
	requireOpenCodeDiagnosticFieldAllowlist(t, logText, "OpenCode request preflight rejected:")
	requireOpenCodeDiagnosticPrivacy(t, logText,
		modelName,
		"diagnostic-strict-channel-private",
		"diagnostic-compatible-channel-private",
		"diagnostic-strict-credential-private",
		"diagnostic-compatible-credential-private",
		"diagnostic-candidate-body-private",
		"diagnostic-candidate-ttl-private",
		"diagnostic-candidate-metadata-private",
		"diagnostic-candidate-request-id-private",
		"diagnostic-candidate-url-private",
	)
}

func TestOpenCodeCompatibilitySummaryPrecedesAttemptAndDeduplicatesByPlan(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			infoLogs, _ := captureOpenCodeDiagnosticLogs(t)
			body := []byte(`{
				"model":"diagnostic-attempt-model-private",
				"max_tokens":16,
				"system":[{
					"type":"text",
					"text":"diagnostic-attempt-body-private",
					"cache_control":{"type":"ephemeral"}
				}],
				"messages":[{"role":"user","content":"hello"}],
				"metadata":{"user_id":"diagnostic-attempt-metadata-private"}
			}`)
			c, info, _ := newOpenCodeControllerPreflightFixture(
				t,
				channelType,
				"/v1/messages",
				types.RelayFormatClaude,
				"diagnostic-attempt-model-private",
				`{"diagnostic-attempt-model-private":"glm-5.2"}`,
				body,
			)
			common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{
				OpenCodeGo: &dto.OpenCodeGoConfig{
					UnsupportedOptionalFieldPolicy: dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
				},
			})
			common.SetContextKey(c, constant.ContextKeyChannelName, "diagnostic-attempt-channel-private")
			common.SetContextKey(c, constant.ContextKeyChannelKey, "diagnostic-attempt-credential-private")
			c.Set(common.RequestIdKey, "diagnostic-attempt-request-id-private")
			c.Request.URL.RawQuery = "diagnostic-attempt-url-private=true"
			require.Nil(t, preflightOpenCodeRequest(c, info))
			plan, found, err := opencodego.GetRequestPreflightPlan(c)
			require.NoError(t, err)
			require.True(t, found)
			require.NotEmpty(t, plan.CacheControlPlanFingerprint)

			attempts := 0
			physicalAttempt := func() *types.NewAPIError {
				return relayOpenCodePhysicalAttempt(c, func() *types.NewAPIError {
					attempts++
					assert.Contains(t, infoLogs.String(), "rule_id="+openCodeCacheControlCompatibilityRule)
					if channelType == constant.ChannelTypeOpenCodeGo && attempts == 1 {
						return markedOpenCodeGoImmediateRetryError("retryable diagnostic test failure", http.StatusServiceUnavailable)
					}
					return nil
				})
			}
			if channelType == constant.ChannelTypeOpenCodeGo {
				relayErr := relaySelectedChannelWithOpenCodeGoRetry(
					c,
					types.RelayFormatClaude,
					info,
					channelType,
					physicalAttempt,
				)
				require.Nil(t, relayErr)
			} else {
				for attemptIndex := 0; attemptIndex < 2; attemptIndex++ {
					relayErr := relaySelectedChannelWithOpenCodeGoRetry(
						c,
						types.RelayFormatClaude,
						info,
						channelType,
						physicalAttempt,
					)
					require.Nil(t, relayErr)
				}
			}

			assert.Equal(t, 2, attempts)
			logText := infoLogs.String()
			assert.Equal(t, 1, strings.Count(logText, "OpenCode request compatibility attempt:"))
			assert.Contains(t, logText, "rule_id="+openCodeCacheControlCompatibilityRule)
			assert.Contains(t, logText, "stage="+openCodeCacheControlPhysicalAttemptStage)
			assert.Contains(t, logText, "status=attempt")
			assert.Contains(t, logText, "registry_version="+opencodego.CacheControlRegistryVersion)
			assert.Contains(t, logText, "client_protocol=messages")
			assert.Contains(t, logText, "final_protocol=chat")
			assert.Contains(t, logText, fmt.Sprintf("channel_type=%d", channelType))
			assert.Contains(t, logText, "policy=drop_known_optional")
			assert.Contains(t, logText, "candidate_count=1")
			assert.Contains(t, logText, "preserve_count=0")
			assert.Contains(t, logText, "drop_count=1")
			assert.NotContains(t, logText, plan.CacheControlPlanFingerprint)
			requireOpenCodeDiagnosticFieldAllowlist(t, logText, "OpenCode request compatibility attempt:")
			requireOpenCodeDiagnosticPrivacy(t, logText,
				"diagnostic-attempt-model-private",
				"diagnostic-attempt-body-private",
				"diagnostic-attempt-metadata-private",
				"diagnostic-attempt-channel-private",
				"diagnostic-attempt-credential-private",
				"diagnostic-attempt-request-id-private",
				"diagnostic-attempt-url-private",
			)
		})
	}
}

func TestOpenCodeDiagnosticRejectsDynamicRuleAndStageValues(t *testing.T) {
	_, errorLogs := captureOpenCodeDiagnosticLogs(t)
	c, info, _ := newOpenCodeControllerPreflightFixture(
		t,
		constant.ChannelTypeOpenCodeGo,
		"/v1/chat/completions",
		types.RelayFormatOpenAI,
		"glm-5.2",
		"",
		openCodeControllerPreflightBody(t, types.RelayFormatOpenAI, "glm-5.2", false),
	)
	initializeOpenCodeCacheControlDiagnostics(c, info)
	recordOpenCodeDiagnosticCandidateFromContext(c)
	state, found := getOpenCodeCacheControlDiagnosticState(c)
	require.True(t, found)
	state.mu.Lock()
	state.candidateCount = openCodeDiagnosticMaxCandidates + 1000
	state.preserveCount = openCodeDiagnosticMaxDispositions + 1000
	state.dropCount = openCodeDiagnosticMaxDispositions + 1000
	state.policyMask = openCodeDiagnosticPolicyInvalidMask
	state.mu.Unlock()

	logOpenCodeRequestPreflightRejection(
		c,
		"diagnostic-dynamic-rule-private",
		"diagnostic-dynamic-stage-private",
		http.StatusOK,
	)

	logText := errorLogs.String()
	assert.Contains(t, logText, "rule_id="+openCodeDiagnosticUnknownRule)
	assert.Contains(t, logText, "stage="+openCodeDiagnosticUnknownStage)
	assert.Contains(t, logText, "status=500")
	assert.Contains(t, logText, "policy=mixed")
	assert.Contains(t, logText, fmt.Sprintf("candidate_count=%d", openCodeDiagnosticMaxCandidates))
	assert.Contains(t, logText, fmt.Sprintf("preserve_count=%d", openCodeDiagnosticMaxDispositions))
	assert.Contains(t, logText, fmt.Sprintf("drop_count=%d", openCodeDiagnosticMaxDispositions))
	requireOpenCodeDiagnosticFieldAllowlist(t, logText, "OpenCode request preflight rejected:")
	requireOpenCodeDiagnosticPrivacy(t, logText,
		"diagnostic-dynamic-rule-private",
		"diagnostic-dynamic-stage-private",
	)
}

func TestOpenCodeDiagnosticRegistryVersionIsBounded(t *testing.T) {
	assert.Equal(
		t,
		opencodego.CacheControlRegistryVersion,
		boundedOpenCodeCacheControlRegistryVersion(opencodego.CacheControlRegistryVersion),
	)
	assert.Equal(
		t,
		openCodeDiagnosticProtocolUnknown,
		boundedOpenCodeCacheControlRegistryVersion("diagnostic-registry-private"),
	)
}

func newMalformedOpenCodeCacheCandidateFixture(
	t *testing.T,
	selected *model.Channel,
	modelName string,
) (*gin.Context, *relaycommon.RelayInfo) {
	t.Helper()
	body := []byte(`{"model":"` + modelName + `","max_tokens":32,` +
		`"system":[{"type":"text","text":"diagnostic-candidate-body-private",` +
		`"cache_control":{"type":"ephemeral","ttl":"diagnostic-candidate-ttl-private"}}],` +
		`"messages":[{"role":"user","content":"hello"}],` +
		`"metadata":{"user_id":"diagnostic-candidate-metadata-private"},"stream":false}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.URL.RawQuery = "diagnostic-candidate-url-private=true"
	storage, err := common.CreateBodyStorage(body)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })
	c.Set(common.KeyBodyStorage, storage)
	c.Set(common.RequestIdKey, "diagnostic-candidate-request-id-private")
	common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	require.Nil(t, middleware.SetupContextForSelectedChannel(c, selected, modelName))

	request, err := helper.GetAndValidateRequest(c, types.RelayFormatClaude)
	require.NoError(t, err)
	info, err := relaycommon.GenRelayInfo(c, types.RelayFormatClaude, request, nil)
	require.NoError(t, err)
	return c, info
}

func requireOpenCodeDiagnosticPrivacy(t *testing.T, logText string, sentinels ...string) {
	t.Helper()
	for _, sentinel := range sentinels {
		assert.NotContains(t, logText, sentinel)
	}
	for _, forbiddenField := range []string{
		"channel_id=",
		"group=",
		"model=",
		"path=",
		"index=",
		"value=",
		"body=",
		"credential=",
		"url=",
		"metadata=",
		"request_id=",
		"upstream=",
		"fingerprint=",
	} {
		assert.NotContains(t, logText, forbiddenField)
	}
}

func requireOpenCodeDiagnosticFieldAllowlist(t *testing.T, logText string, marker string) {
	t.Helper()
	markerIndex := strings.Index(logText, marker)
	require.NotEqual(t, -1, markerIndex)
	payload := strings.TrimSpace(logText[markerIndex+len(marker):])
	allowed := map[string]struct{}{
		"rule_id":         {},
		"stage":           {},
		"status":          {},
		"client_protocol": {},
		"final_protocol":  {},
		"channel_type":    {},
		"policy":          {},
		"candidate_count": {},
		"preserve_count":  {},
		"drop_count":      {},
	}
	if marker == "OpenCode request compatibility attempt:" {
		allowed["registry_version"] = struct{}{}
	}
	fields := strings.Fields(payload)
	require.Len(t, fields, len(allowed))
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		key, _, found := strings.Cut(field, "=")
		require.True(t, found, field)
		_, accepted := allowed[key]
		assert.True(t, accepted, key)
		_, duplicate := seen[key]
		assert.False(t, duplicate, key)
		seen[key] = struct{}{}
	}
}
