package controller

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newOpenCodeSecurityTestContext(t *testing.T, body string, channelType int) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	storage, err := common.CreateBodyStorage([]byte(body))
	require.NoError(t, err)
	c.Set(common.KeyBodyStorage, storage)
	common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
	t.Cleanup(func() { _ = storage.Close() })
	_, err = helper.GetAndValidateRequest(c, types.RelayFormatOpenAI)
	require.NoError(t, err)
	return c
}

func TestScanOpenCodeRequestSensitiveValuesIncludesUnknownExtensions(t *testing.T) {
	c := newOpenCodeSecurityTestContext(t,
		`{"model":"test-model","messages":[{"role":"user","content":"ordinary"}],"provider_extension":{"opaque":"test_sensitive"}}`,
		constant.ChannelTypeOpenCodeAPIKey,
	)

	matched, count, err := scanOpenCodeRequestSensitiveValues(c, types.RelayFormatOpenAI)

	require.NoError(t, err)
	assert.True(t, matched)
	assert.Positive(t, count)
}

func TestScanOpenCodeRequestSensitiveValuesPreservesNonOpenCodeBehavior(t *testing.T) {
	c := newOpenCodeSecurityTestContext(t,
		`{"model":"test-model","messages":[{"role":"user","content":"ordinary"}],"provider_extension":{"opaque":"test_sensitive"}}`,
		constant.ChannelTypeOpenAI,
	)

	matched, count, err := scanOpenCodeRequestSensitiveValues(c, types.RelayFormatOpenAI)

	require.NoError(t, err)
	assert.False(t, matched)
	assert.Zero(t, count)
}

func TestScanOpenCodeRequestSensitiveValuesFailsClosedWithoutEnvelope(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeAPIKey)

	matched, count, err := scanOpenCodeRequestSensitiveValues(c, types.RelayFormatOpenAI)

	require.Error(t, err)
	assert.False(t, matched)
	assert.Zero(t, count)
}

func TestPreflightOpenCodeRequestSensitiveValuesReturnsSafeTypedRejection(t *testing.T) {
	var logs bytes.Buffer
	common.LogWriterMu.Lock()
	previousErrorWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logs
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = previousErrorWriter
		common.LogWriterMu.Unlock()
	})

	previousCheckSensitive := setting.CheckSensitiveEnabled
	previousCheckPrompt := setting.CheckSensitiveOnPromptEnabled
	previousWords := append([]string(nil), setting.SensitiveWords...)
	setting.CheckSensitiveEnabled = true
	setting.CheckSensitiveOnPromptEnabled = true
	setting.SensitiveWords = []string{"raw-only-secret"}
	t.Cleanup(func() {
		setting.CheckSensitiveEnabled = previousCheckSensitive
		setting.CheckSensitiveOnPromptEnabled = previousCheckPrompt
		setting.SensitiveWords = previousWords
	})

	c := newOpenCodeSecurityTestContext(t,
		`{"model":"test-model","messages":[{"role":"user","content":"ordinary"}],"provider_extension":{"opaque":"raw-only-secret"}}`,
		constant.ChannelTypeOpenCodeAPIKey,
	)

	relayErr := preflightOpenCodeRequestSensitiveValues(c, types.RelayFormatOpenAI)

	require.NotNil(t, relayErr)
	assert.NotNil(t, relayErr.Err)
	assert.Equal(t, http.StatusBadRequest, relayErr.StatusCode)
	assert.Equal(t, types.ErrorCodeSensitiveWordsDetected, relayErr.GetErrorCode())
	assert.True(t, types.IsSkipRetryError(relayErr))
	assert.False(t, types.IsRecordErrorLog(relayErr))
	assert.Equal(t, types.ErrorOriginLocalValidation, relayErr.Provenance().Origin)
	assert.Equal(t, openCodeRawSensitiveScanRuleID, relayErr.Provenance().Subtype)
	assert.NotContains(t, relayErr.Error(), "raw-only-secret")
	assert.Contains(t, logs.String(), "rule_id="+openCodeRawSensitiveScanRuleID)
	assert.Contains(t, logs.String(), "match_count=1")
	assert.NotContains(t, logs.String(), "raw-only-secret")
	assert.NotContains(t, logs.String(), "provider_extension")
}

func TestPreflightOpenCodeRequestSensitiveValuesFailsClosedWithoutLeakingScanError(t *testing.T) {
	previousCheckSensitive := setting.CheckSensitiveEnabled
	previousCheckPrompt := setting.CheckSensitiveOnPromptEnabled
	setting.CheckSensitiveEnabled = true
	setting.CheckSensitiveOnPromptEnabled = true
	t.Cleanup(func() {
		setting.CheckSensitiveEnabled = previousCheckSensitive
		setting.CheckSensitiveOnPromptEnabled = previousCheckPrompt
	})

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeAPIKey)

	relayErr := preflightOpenCodeRequestSensitiveValues(c, types.RelayFormatOpenAI)

	require.NotNil(t, relayErr)
	assert.NotNil(t, relayErr.Err)
	assert.Equal(t, http.StatusInternalServerError, relayErr.StatusCode)
	assert.True(t, types.IsSkipRetryError(relayErr))
	assert.False(t, types.IsRecordErrorLog(relayErr))
	assert.Equal(t, types.ErrorOriginGatewayInvariant, relayErr.Provenance().Origin)
	assert.Equal(t, openCodeRawSensitiveScanRuleID+".scan-failed", relayErr.Provenance().Subtype)
	assert.Equal(t, "request security validation failed", relayErr.Error())
}

func TestPreflightOpenCodeRequestSensitiveValuesClassifiesCancellationLocally(t *testing.T) {
	previousCheckSensitive := setting.CheckSensitiveEnabled
	previousCheckPrompt := setting.CheckSensitiveOnPromptEnabled
	setting.CheckSensitiveEnabled = true
	setting.CheckSensitiveOnPromptEnabled = true
	t.Cleanup(func() {
		setting.CheckSensitiveEnabled = previousCheckSensitive
		setting.CheckSensitiveOnPromptEnabled = previousCheckPrompt
	})

	c := newOpenCodeSecurityTestContext(t,
		`{"model":"test-model","messages":[{"role":"user","content":"ordinary"}]}`,
		constant.ChannelTypeOpenCodeAPIKey,
	)
	cancelled, cancel := context.WithCancel(c.Request.Context())
	cancel()
	c.Request = c.Request.WithContext(cancelled)

	relayErr := preflightOpenCodeRequestSensitiveValues(c, types.RelayFormatOpenAI)

	require.NotNil(t, relayErr)
	assert.Equal(t, 499, relayErr.StatusCode)
	assert.Equal(t, types.ErrorOriginLocalCancel, relayErr.Provenance().Origin)
	assert.Equal(t, openCodeRawSensitiveScanRuleID+".cancelled", relayErr.Provenance().Subtype)
	assert.Equal(t, "request was canceled", relayErr.Error())
}

func TestShouldRunTypedSensitiveScanHonorsSharedRawScan(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	assert.True(t, shouldRunTypedSensitiveScan(c))

	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeAPIKey)
	assert.False(t, shouldRunTypedSensitiveScan(c))
}

func TestRelayScansRawOpenCodeStringsBeforeRetrySnapshot(t *testing.T) {
	var logs bytes.Buffer
	common.LogWriterMu.Lock()
	previousErrorWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logs
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = previousErrorWriter
		common.LogWriterMu.Unlock()
	})

	previousCheckSensitive := setting.CheckSensitiveEnabled
	previousCheckPrompt := setting.CheckSensitiveOnPromptEnabled
	previousWords := append([]string(nil), setting.SensitiveWords...)
	setting.CheckSensitiveEnabled = true
	setting.CheckSensitiveOnPromptEnabled = true
	setting.SensitiveWords = []string{"controller-raw-secret"}
	t.Cleanup(func() {
		setting.CheckSensitiveEnabled = previousCheckSensitive
		setting.CheckSensitiveOnPromptEnabled = previousCheckPrompt
		setting.SensitiveWords = previousWords
	})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		bytes.NewBufferString(`{"model":"test-model","messages":[{"role":"user","content":"ordinary"}],"provider_extension":{"opaque":"controller-raw-secret"}}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(common.RequestIdKey, "raw-security-request-id")
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeAPIKey)
	storage, err := common.CreateBodyStorage([]byte(`{"model":"test-model","messages":[{"role":"user","content":"ordinary"}],"provider_extension":{"opaque":"controller-raw-secret"}}`))
	require.NoError(t, err)
	c.Set(common.KeyBodyStorage, storage)
	t.Cleanup(func() { _ = storage.Close() })

	Relay(c, types.RelayFormatOpenAI)

	assert.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), "request contains sensitive content")
	assert.NotContains(t, recorder.Body.String(), "controller-raw-secret")
	assert.NotContains(t, recorder.Body.String(), "provider_extension")
	_, snapshotFound, snapshotErr := getOpenCodeAPIKeyRetrySnapshot(c)
	require.NoError(t, snapshotErr)
	assert.False(t, snapshotFound)
	assert.Contains(t, logs.String(), "rule_id="+openCodeRawSensitiveScanRuleID)
	assert.NotContains(t, logs.String(), "controller-raw-secret")
	assert.NotContains(t, logs.String(), "provider_extension")
}
