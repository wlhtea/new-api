package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func invokeDoubaoOfficialOperationRoute(t *testing.T, fixture *doubaoNativeFixture, method, providerID string) *httptest.ResponseRecorder {
	t.Helper()
	return invokeDoubaoOfficialOperationRouteWithBody(t, fixture, method, providerID, "", "")
}

func invokeDoubaoOfficialOperationRouteWithBody(t *testing.T, fixture *doubaoNativeFixture, method, providerID, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	binding, found := fixture.generation.LookupDeclaredRoute(method, doubaoOfficialDeleteDeclaredPath)
	require.True(t, found, "official native task route is not registered")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	actualPath := strings.Replace(doubaoOfficialDeleteDeclaredPath, ":task_id", providerID, 1)
	ctx.Request = httptest.NewRequest(method, actualPath+"?private_query=must-not-leak", strings.NewReader(body))
	if contentType != "" {
		ctx.Request.Header.Set("Content-Type", contentType)
	}
	ctx.Params = gin.Params{{Key: "task_id", Value: providerID}}
	ctx.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{
		Generation: fixture.generation, Plugin: binding.Plugin, Route: binding.Route,
	})
	common.SetContextKey(ctx, constant.ContextKeyUserId, 7)
	common.SetContextKey(ctx, constant.ContextKeyTokenId, 41)
	common.SetContextKey(ctx, constant.ContextKeyUsingGroup, "operation-group")
	ctx.Set("id", 7)
	ctx.Set("username", "doubao-native-user")
	ctx.Set("role", common.RoleCommonUser)
	ctx.Set("token_name", "operation-token")
	ctx.Set(common.RequestIdKey, "req-operation-"+strings.ToLower(method)+"-"+providerID)
	middleware.PrepareTaskPluginRoute()(ctx)
	return recorder
}

func invokeDoubaoOfficialOperationRouteThroughGin(t *testing.T, fixture *doubaoNativeFixture, method, encodedProviderID string) *httptest.ResponseRecorder {
	t.Helper()
	binding, found := fixture.generation.LookupDeclaredRoute(method, doubaoOfficialDeleteDeclaredPath)
	require.True(t, found, "official native task route is not registered")
	engine := gin.New()
	engine.Handle(method, doubaoOfficialDeleteDeclaredPath,
		func(ctx *gin.Context) {
			ctx.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{
				Generation: fixture.generation, Plugin: binding.Plugin, Route: binding.Route,
			})
			common.SetContextKey(ctx, constant.ContextKeyUserId, 7)
			common.SetContextKey(ctx, constant.ContextKeyTokenId, 41)
			common.SetContextKey(ctx, constant.ContextKeyUsingGroup, "operation-group")
			ctx.Set("id", 7)
			ctx.Set("username", "doubao-native-user")
			ctx.Set("role", common.RoleCommonUser)
			ctx.Set("token_name", "operation-token")
			ctx.Set(common.RequestIdKey, "req-operation-invalid-provider-id")
		},
		middleware.PrepareTaskPluginRoute(),
	)
	recorder := httptest.NewRecorder()
	requestPath := strings.Replace(doubaoOfficialDeleteDeclaredPath, ":task_id", encodedProviderID, 1)
	engine.ServeHTTP(recorder, httptest.NewRequest(method, requestPath, nil))
	return recorder
}

func setupDoubaoOfficialOperationIdentity(t *testing.T, fixture *doubaoNativeFixture) (model.User, model.Channel, model.Token) {
	t.Helper()
	require.NoError(t, fixture.database.AutoMigrate(&model.Token{}))
	require.NoError(t, fixture.database.Model(&model.User{}).Where("id = ?", 7).Updates(map[string]any{
		"used_quota": 123, "request_count": 456,
	}).Error)
	fixture.channel.UsedQuota = 789
	require.NoError(t, fixture.database.Save(fixture.channel).Error)
	token := model.Token{
		Id: 41, UserId: 7, Key: "sk-current-token-secret", Name: "operation-token",
		RemainQuota: 4321, UsedQuota: 54, Group: "operation-group",
	}
	require.NoError(t, fixture.database.Create(&token).Error)
	var user model.User
	require.NoError(t, fixture.database.First(&user, 7).Error)
	var channel model.Channel
	require.NoError(t, fixture.database.First(&channel, fixture.channel.Id).Error)
	return user, channel, token
}

func assertDoubaoOfficialOperationBillingUnchanged(t *testing.T, fixture *doubaoNativeFixture, userBefore model.User, channelBefore model.Channel, tokenBefore model.Token) {
	t.Helper()
	var userAfter model.User
	require.NoError(t, fixture.database.First(&userAfter, 7).Error)
	assert.Equal(t, userBefore.Quota, userAfter.Quota)
	assert.Equal(t, userBefore.UsedQuota, userAfter.UsedQuota)
	assert.Equal(t, userBefore.RequestCount, userAfter.RequestCount)
	var channelAfter model.Channel
	require.NoError(t, fixture.database.First(&channelAfter, fixture.channel.Id).Error)
	assert.Equal(t, channelBefore.UsedQuota, channelAfter.UsedQuota)
	var tokenAfter model.Token
	require.NoError(t, fixture.database.First(&tokenAfter, tokenBefore.Id).Error)
	assert.Equal(t, tokenBefore.RemainQuota, tokenAfter.RemainQuota)
	assert.Equal(t, tokenBefore.UsedQuota, tokenAfter.UsedQuota)
}

func assertDoubaoOfficialOperationLogSafe(t *testing.T, entry *model.Log, task *model.Task) {
	t.Helper()
	raw := entry.Content + " " + entry.Other
	for _, forbidden := range []string{
		task.TaskID, "sk-current-token-secret", "sk-task-private", "private_query",
		"must-not-leak", "https://private.example/result.mp4", "private-request-body",
		"admin_info", "root_info", "audit_info",
	} {
		assert.NotContains(t, raw, forbidden)
	}
	assert.Empty(t, entry.ChannelName)
}

func TestDoubaoArkOfficialNativeQueryWritesNoOperationLogs(t *testing.T) {
	fixture := setupDoubaoNativeFixture(t, "cgt-operation-query-upstream")
	common.LogConsumeEnabled = true
	userBefore, channelBefore, tokenBefore := setupDoubaoOfficialOperationIdentity(t, fixture)
	pluginSetting := `{"task_plugin_key":"doubao_ark_official","allow_official_task_list":true}`
	fixture.channel.Type = constant.ChannelTypeTaskPlugin
	fixture.channel.Setting = &pluginSetting
	require.NoError(t, fixture.database.Save(fixture.channel).Error)
	channelBefore = *fixture.channel

	now := time.Now().Unix()
	owned := &model.Task{
		TaskID: "task-operation-query-internal", ProviderTaskID: "cgt-operation-query-owned",
		UserId: 7, ChannelId: fixture.channel.Id, Platform: "doubao_ark_official",
		Status: model.TaskStatusSuccess, OfficialModel: doubaoNativeTestModel,
		Properties: model.Properties{OriginModelName: doubaoNativeTestModel},
		CreatedAt:  now, UpdatedAt: now,
		Data:        []byte(`{"id":"cgt-operation-query-owned","status":"succeeded","private_request_body":"private-request-body"}`),
		PrivateData: model.TaskPrivateData{Key: "sk-task-private", ResultURL: "https://private.example/result.mp4"},
	}
	foreign := &model.Task{
		TaskID: "task-operation-query-foreign-internal", ProviderTaskID: "cgt-operation-query-foreign",
		UserId: 8, ChannelId: fixture.channel.Id, Platform: "doubao_ark_official",
		Status: model.TaskStatusSuccess, OfficialModel: "foreign-model-secret",
		Data: []byte(`{"id":"cgt-operation-query-foreign","status":"succeeded"}`),
	}
	require.NoError(t, fixture.database.Create(owned).Error)
	require.NoError(t, fixture.database.Create(foreign).Error)

	ownedResponse := invokeDoubaoOfficialOperationRoute(t, fixture, http.MethodGet, owned.ProviderTaskID)
	require.Equal(t, http.StatusOK, ownedResponse.Code, ownedResponse.Body.String())
	assert.JSONEq(t, `{"id":"cgt-operation-query-owned","status":"succeeded"}`, ownedResponse.Body.String())
	for _, providerID := range []string{foreign.ProviderTaskID, "cgt-operation-query-missing"} {
		response := invokeDoubaoOfficialOperationRoute(t, fixture, http.MethodGet, providerID)
		require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
		assert.JSONEq(t, `{"error":{"code":"task_not_found","message":"Task not found"}}`, response.Body.String())
		assert.NotContains(t, response.Body.String(), providerID)
	}

	listResponse := invokeDoubaoOfficialListRoute(t, fixture, 7)
	require.Equal(t, http.StatusOK, listResponse.Code, listResponse.Body.String())

	logs, total, err := model.GetUserLogs(7, model.LogFilter{Type: model.LogTypeConsume}, 0, 10)
	require.NoError(t, err)
	assert.Zero(t, total, "single-task GETs and list requests must not write operation logs")
	assert.Empty(t, logs)
	assertDoubaoOfficialOperationBillingUnchanged(t, fixture, userBefore, channelBefore, tokenBefore)

	require.NoError(t, fixture.database.Migrator().DropTable(&model.Log{}))
	responseAfterLogFailure := invokeDoubaoOfficialOperationRoute(t, fixture, http.MethodGet, owned.ProviderTaskID)
	require.Equal(t, http.StatusOK, responseAfterLogFailure.Code, responseAfterLogFailure.Body.String())
	assert.JSONEq(t, ownedResponse.Body.String(), responseAfterLogFailure.Body.String())
}

func TestDoubaoArkOfficialNativeSingleTaskValidatesProviderTaskID(t *testing.T) {
	testCases := []struct {
		name              string
		encodedProviderID string
		decodedBytes      int
		wantStatus        int
		expectLookup      bool
	}{
		{name: "191 ASCII bytes accepted", encodedProviderID: strings.Repeat("a", 191), decodedBytes: 191, wantStatus: http.StatusNotFound, expectLookup: true},
		{name: "192 ASCII bytes rejected", encodedProviderID: strings.Repeat("a", 192), decodedBytes: 192, wantStatus: http.StatusBadRequest},
		{name: "191 UTF-8 bytes accepted", encodedProviderID: strings.Repeat("é", 95) + "a", decodedBytes: 191, wantStatus: http.StatusNotFound, expectLookup: true},
		{name: "192 UTF-8 bytes rejected", encodedProviderID: strings.Repeat("é", 96), decodedBytes: 192, wantStatus: http.StatusBadRequest},
		{name: "percent-encoded newline rejected", encodedProviderID: "cgt%0Aforged", wantStatus: http.StatusBadRequest},
		{name: "percent-encoded unicode control rejected", encodedProviderID: "cgt%C2%85forged", wantStatus: http.StatusBadRequest},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := setupDoubaoNativeFixture(t, "cgt-operation-validation")
			common.LogConsumeEnabled = true
			setupDoubaoOfficialOperationIdentity(t, fixture)
			if testCase.decodedBytes > 0 {
				require.Len(t, testCase.encodedProviderID, testCase.decodedBytes, "test input must exercise the byte boundary")
			}

			providerLookupQueries := 0
			const callbackName = "test:count_official_provider_lookups"
			require.NoError(t, fixture.database.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
				if tx.Statement != nil && tx.Statement.Table == "tasks" && strings.Contains(tx.Statement.SQL.String(), "provider_task_id") {
					providerLookupQueries++
				}
			}))

			response := invokeDoubaoOfficialOperationRouteThroughGin(t, fixture, http.MethodGet, testCase.encodedProviderID)
			assert.Equal(t, testCase.wantStatus, response.Code, response.Body.String())
			if testCase.expectLookup {
				assert.Positive(t, providerLookupQueries)
			} else {
				assert.Zero(t, providerLookupQueries, "invalid provider IDs must be rejected before task lookup")
			}
			logs, total, err := model.GetUserLogs(7, model.LogFilter{Type: model.LogTypeConsume}, 0, 10)
			require.NoError(t, err)
			assert.Zero(t, total, "GET validation results must not write operation logs")
			assert.Empty(t, logs)
		})
	}
}

func TestDoubaoArkOfficialOperationLogsBoundSanitizedRejectedRequestValues(t *testing.T) {
	testCases := []struct {
		name                 string
		encodedProviderID    string
		decodedProviderID    string
		applicationForbidden string
	}{
		{
			name:                 "oversized provider ID",
			encodedProviderID:    strings.Repeat("a", 4096),
			decodedProviderID:    strings.Repeat("a", 4096),
			applicationForbidden: strings.Repeat("a", 4096),
		},
		{
			name:                 "percent-encoded newline",
			encodedProviderID:    "cgt%0Aforged",
			decodedProviderID:    "cgt\nforged",
			applicationForbidden: `cgt\nforged`,
		},
		{
			name:                 "percent-encoded unicode control",
			encodedProviderID:    "cgt%C2%85forged",
			decodedProviderID:    "cgt\u0085forged",
			applicationForbidden: "cgt\u0085forged",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := setupDoubaoNativeFixture(t, "cgt-operation-log-sanitization")
			common.LogConsumeEnabled = true
			setupDoubaoOfficialOperationIdentity(t, fixture)
			var applicationLogs bytes.Buffer
			common.LogWriterMu.Lock()
			previousWriter, previousErrorWriter := gin.DefaultWriter, gin.DefaultErrorWriter
			gin.DefaultWriter, gin.DefaultErrorWriter = &applicationLogs, &applicationLogs
			common.LogWriterMu.Unlock()
			t.Cleanup(func() {
				common.LogWriterMu.Lock()
				gin.DefaultWriter, gin.DefaultErrorWriter = previousWriter, previousErrorWriter
				common.LogWriterMu.Unlock()
			})

			response := invokeDoubaoOfficialOperationRouteThroughGin(t, fixture, http.MethodDelete, testCase.encodedProviderID)
			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			logs, total, err := model.GetUserLogs(7, model.LogFilter{Type: model.LogTypeConsume}, 0, 10)
			require.NoError(t, err)
			assert.EqualValues(t, 1, total)
			require.Len(t, logs, 1)
			entry := logs[0]
			assert.Zero(t, entry.Quota)
			assert.Equal(t, "删除任务", entry.ModelName)
			var metadata map[string]any
			require.NoError(t, json.Unmarshal([]byte(entry.Other), &metadata))
			assert.Equal(t, "delete", metadata["operation"])
			assert.NotContains(t, metadata, "task_model")
			providerID, ok := metadata["provider_task_id"].(string)
			require.True(t, ok)
			requestPath, ok := metadata["request_path"].(string)
			require.True(t, ok)
			assert.LessOrEqual(t, len(providerID), 191)
			assert.LessOrEqual(t, len(requestPath), 256)
			assert.NotEqual(t, testCase.decodedProviderID, providerID)
			assert.NotContains(t, entry.Content, testCase.decodedProviderID)
			assert.NotContains(t, applicationLogs.String(), testCase.applicationForbidden)
			assert.LessOrEqual(t, applicationLogs.Len(), 8192)
			for _, value := range []string{providerID, requestPath, entry.Content, entry.Other} {
				for _, r := range value {
					assert.False(t, unicode.IsControl(r), "log value contains an unsanitized control rune: %q", value)
				}
			}
		})
	}
}

func TestDoubaoArkOfficialNativeOperationLogsDoNotChangeQuotaData(t *testing.T) {
	fixture := setupDoubaoNativeFixture(t, "cgt-operation-quota-data")
	require.NoError(t, fixture.database.AutoMigrate(&model.AuditLog{}))
	common.LogConsumeEnabled = true
	previousDataExport := common.DataExportEnabled
	common.DataExportEnabled = true
	model.CacheQuotaDataLock.Lock()
	previousQuotaData := model.CacheQuotaData
	model.CacheQuotaData = make(map[string]*model.QuotaData)
	model.CacheQuotaDataLock.Unlock()
	t.Cleanup(func() {
		common.DataExportEnabled = previousDataExport
		model.CacheQuotaDataLock.Lock()
		model.CacheQuotaData = previousQuotaData
		model.CacheQuotaDataLock.Unlock()
	})
	setupDoubaoOfficialOperationIdentity(t, fixture)

	response := invokeDoubaoOfficialOperationRoute(t, fixture, http.MethodDelete, "cgt-operation-quota-data-missing")
	require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())

	logs, total, err := model.GetUserLogs(7, model.LogFilter{Type: model.LogTypeConsume}, 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	require.Len(t, logs, 1)
	assert.Zero(t, logs[0].Quota)
	assert.Equal(t, "删除任务", logs[0].ModelName)
	assert.Contains(t, logs[0].Content, "cgt-operation-quota-data-missing")
	assert.Contains(t, logs[0].Other, `"provider_task_id"`)

	model.CacheQuotaDataLock.Lock()
	assert.Empty(t, model.CacheQuotaData, "zero-quota operation logs must not increment dashboard request counts")
	model.CacheQuotaDataLock.Unlock()
}

func TestDoubaoArkOfficialNativeRepeatedDeleteRetainsLogMetadata(t *testing.T) {
	fixture := setupDoubaoNativeFixture(t, "cgt-operation-repeated-delete-owned")
	require.NoError(t, fixture.database.AutoMigrate(&model.AuditLog{}))
	common.LogConsumeEnabled = true
	userBefore, channelBefore, tokenBefore := setupDoubaoOfficialOperationIdentity(t, fixture)
	pluginSetting := `{"task_plugin_key":"doubao_ark_official","allow_official_task_delete":true}`
	fixture.channel.Type = constant.ChannelTypeTaskPlugin
	fixture.channel.Setting = &pluginSetting
	require.NoError(t, fixture.database.Save(fixture.channel).Error)
	channelBefore = *fixture.channel
	useRealDoubaoTaskAdaptors(t)

	now := time.Now().Unix()
	owned := &model.Task{
		TaskID: "task-operation-repeated-delete-internal", ProviderTaskID: "cgt-operation-repeated-delete-owned",
		UserId: 7, ChannelId: fixture.channel.Id, Platform: "doubao_ark_official",
		Status: model.TaskStatusSuccess, OfficialModel: doubaoNativeTestModel,
		Properties: model.Properties{OriginModelName: doubaoNativeTestModel}, CreatedAt: now, UpdatedAt: now,
		Data:        []byte(`{"id":"cgt-operation-repeated-delete-owned","status":"succeeded"}`),
		PrivateData: model.TaskPrivateData{Key: "sk-task-private", ResultURL: "https://private.example/result.mp4"},
	}
	wrongPlugin := &model.Task{
		TaskID: "task-operation-repeated-delete-wrong-plugin", ProviderTaskID: owned.ProviderTaskID,
		UserId: 7, ChannelId: 999, Platform: "another_official_plugin", ProviderDeleted: true,
		Status: model.TaskStatusSuccess, OfficialModel: "wrong-plugin-model-secret", CreatedAt: now, UpdatedAt: now,
	}
	foreign := &model.Task{
		TaskID: "task-operation-repeated-delete-foreign", ProviderTaskID: "cgt-operation-repeated-delete-foreign",
		UserId: 8, ChannelId: fixture.channel.Id, Platform: "doubao_ark_official", ProviderDeleted: true,
		Status: model.TaskStatusSuccess, OfficialModel: "foreign-model-secret", CreatedAt: now, UpdatedAt: now,
	}
	for _, task := range []*model.Task{owned, wrongPlugin, foreign} {
		require.NoError(t, fixture.database.Create(task).Error)
	}

	firstResponse := invokeDoubaoOfficialOperationRoute(t, fixture, http.MethodDelete, owned.ProviderTaskID)
	require.Equal(t, http.StatusOK, firstResponse.Code, firstResponse.Body.String())
	var tombstoned model.Task
	require.NoError(t, fixture.database.First(&tombstoned, owned.ID).Error)
	require.True(t, tombstoned.ProviderDeleted)

	secondResponse := invokeDoubaoOfficialOperationRoute(t, fixture, http.MethodDelete, owned.ProviderTaskID)
	require.Equal(t, http.StatusNotFound, secondResponse.Code, secondResponse.Body.String())
	assert.JSONEq(t, `{"error":{"code":"task_not_found","message":"Task not found"}}`, secondResponse.Body.String())
	assert.NotContains(t, secondResponse.Body.String(), owned.ProviderTaskID)
	assert.Equal(t, int32(1), fixture.deleteCalls.Load(), "a tombstoned retry must not reach upstream DELETE")

	getResponse := invokeDoubaoOfficialOperationRoute(t, fixture, http.MethodGet, owned.ProviderTaskID)
	require.Equal(t, http.StatusNotFound, getResponse.Code, getResponse.Body.String())
	foreignResponse := invokeDoubaoOfficialOperationRoute(t, fixture, http.MethodDelete, foreign.ProviderTaskID)
	require.Equal(t, http.StatusNotFound, foreignResponse.Code, foreignResponse.Body.String())

	logs, total, err := model.GetUserLogs(7, model.LogFilter{Type: model.LogTypeConsume}, 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 3, total, "two owned DELETEs plus one foreign DELETE; the tombstoned GET must not log")
	require.Len(t, logs, 3)
	ownedStatuses := make(map[int]int)
	foreignLogs := 0
	for _, entry := range logs {
		assert.Equal(t, "删除任务", entry.ModelName)
		assert.Zero(t, entry.Quota)
		var metadata map[string]any
		require.NoError(t, json.Unmarshal([]byte(entry.Other), &metadata))
		providerID, ok := metadata["provider_task_id"].(string)
		require.True(t, ok)
		statusCode, ok := metadata["http_status"].(float64)
		require.True(t, ok)
		switch providerID {
		case owned.ProviderTaskID:
			ownedStatuses[int(statusCode)]++
			assert.Equal(t, doubaoNativeTestModel, metadata["task_model"],
				"both the successful DELETE and the tombstoned retry must retain the original task model")
			assert.Equal(t, fixture.channel.Id, entry.ChannelId,
				"both DELETE logs must retain the original channel")
			assertDoubaoOfficialOperationLogSafe(t, entry, owned)
			assert.NotContains(t, entry.Content+entry.Other, "wrong-plugin-model-secret")
		case foreign.ProviderTaskID:
			foreignLogs++
			assert.NotContains(t, metadata, "task_model", "a foreign tombstone must not contribute metadata")
			assert.Zero(t, entry.ChannelId, "a foreign tombstone must not contribute a channel")
			assert.NotContains(t, entry.Content+entry.Other, "foreign-model-secret")
		default:
			t.Fatalf("unexpected operation log provider ID %q", providerID)
		}
	}
	assert.Equal(t, map[int]int{http.StatusOK: 1, http.StatusNotFound: 1}, ownedStatuses,
		"each DELETE attempt must write exactly one log")
	assert.Equal(t, 1, foreignLogs)
	assertDoubaoOfficialOperationBillingUnchanged(t, fixture, userBefore, channelBefore, tokenBefore)
}

func TestDoubaoArkOfficialDeleteOperationLogsExcludedFromUsageStats(t *testing.T) {
	fixture := setupDoubaoNativeFixture(t, "cgt-operation-usage-stats")
	require.NoError(t, fixture.database.AutoMigrate(&model.AuditLog{}))
	common.LogConsumeEnabled = true
	userBefore, channelBefore, tokenBefore := setupDoubaoOfficialOperationIdentity(t, fixture)

	now := time.Now().Unix()
	response := invokeDoubaoOfficialOperationRoute(t, fixture, http.MethodDelete, "cgt-operation-usage-stats-missing")
	require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/billing", nil)
	ctx.Set("username", "doubao-native-user")
	common.SetContextKey(ctx, constant.ContextKeyTokenId, 41)
	common.SetContextKey(ctx, constant.ContextKeyUsingGroup, "operation-group")
	model.RecordConsumeLog(ctx, 7, model.RecordConsumeLogParams{
		ChannelId: fixture.channel.Id, ModelName: doubaoNativeTestModel,
		TokenName: "operation-token", TokenId: 41, Group: "operation-group",
		Quota: 125, PromptTokens: 3, CompletionTokens: 4,
		Other: model.NewLogOther(), SkipDataExport: true,
	})

	stat, err := model.SumUsedQuota(model.LogTypeConsume, now-1, now+1, "", "doubao-native-user", "", 0, "")
	require.NoError(t, err)
	assert.Equal(t, model.Stat{Quota: 125, Rpm: 1, Tpm: 7}, stat,
		"delete operation logs must not affect quota, RPM, or TPM; ordinary billing rows remain included")

	hasUsageFlagColumn := fixture.database.Migrator().HasColumn(&model.Log{}, "exclude_from_usage_stats")
	assert.True(t, hasUsageFlagColumn, "usage exclusion must be persisted rather than inferred from display metadata")
	if hasUsageFlagColumn {
		var stored []struct {
			ModelName             string
			ExcludeFromUsageStats bool
		}
		require.NoError(t, fixture.database.Table("logs").
			Select("model_name, exclude_from_usage_stats").Order("id").Scan(&stored).Error)
		require.Len(t, stored, 2)
		for _, row := range stored {
			assert.Equal(t, row.ModelName == "删除任务", row.ExcludeFromUsageStats)
		}
	}

	logs, total, err := model.GetUserLogs(7, model.LogFilter{Type: model.LogTypeConsume}, 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 2, total)
	require.Len(t, logs, 2)
	serialized, err := json.Marshal(logs)
	require.NoError(t, err)
	assert.NotContains(t, string(serialized), "exclude_from_usage_stats", "internal usage-stat flags must stay out of log JSON")
	assertDoubaoOfficialOperationBillingUnchanged(t, fixture, userBefore, channelBefore, tokenBefore)
}

func TestDoubaoArkOfficialNativeDeleteWritesZeroQuotaOperationLogs(t *testing.T) {
	fixture := setupDoubaoNativeFixture(t, "cgt-operation-delete-owned")
	common.LogConsumeEnabled = true
	require.NoError(t, fixture.database.AutoMigrate(&model.AuditLog{}))
	userBefore, channelBefore, tokenBefore := setupDoubaoOfficialOperationIdentity(t, fixture)
	pluginSetting := `{"task_plugin_key":"doubao_ark_official","allow_official_task_delete":true}`
	fixture.channel.Type = constant.ChannelTypeTaskPlugin
	fixture.channel.Setting = &pluginSetting
	require.NoError(t, fixture.database.Save(fixture.channel).Error)
	channelBefore = *fixture.channel
	useRealDoubaoTaskAdaptors(t)

	deniedSetting := `{"task_plugin_key":"doubao_ark_official","allow_official_task_delete":false}`
	deniedChannel := &model.Channel{
		Type: constant.ChannelTypeTaskPlugin, Name: "operation-delete-denied", Key: "sk-denied-secret",
		BaseURL: fixture.channel.BaseURL, Status: common.ChannelStatusEnabled, Setting: &deniedSetting,
	}
	require.NoError(t, fixture.database.Create(deniedChannel).Error)
	now := time.Now().Unix()
	owned := &model.Task{
		TaskID: "task-operation-delete-internal", ProviderTaskID: "cgt-operation-delete-owned",
		UserId: 7, ChannelId: fixture.channel.Id, Platform: "doubao_ark_official",
		Status: model.TaskStatusSuccess, OfficialModel: doubaoNativeTestModel, Quota: 1000,
		Properties: model.Properties{OriginModelName: doubaoNativeTestModel},
		CreatedAt:  now, UpdatedAt: now,
		Data:        []byte(`{"id":"cgt-operation-delete-owned","status":"succeeded","private_request_body":"private-request-body"}`),
		PrivateData: model.TaskPrivateData{Key: "sk-task-private", ResultURL: "https://private.example/result.mp4"},
	}
	denied := &model.Task{
		TaskID: "task-operation-delete-denied-internal", ProviderTaskID: "cgt-operation-delete-denied",
		UserId: 7, ChannelId: deniedChannel.Id, Platform: "doubao_ark_official",
		Status: model.TaskStatusQueued, OfficialModel: doubaoNativeTestModel,
		Properties: model.Properties{OriginModelName: doubaoNativeTestModel}, CreatedAt: now, UpdatedAt: now,
		Data: []byte(`{"id":"cgt-operation-delete-denied","status":"queued"}`),
	}
	foreign := &model.Task{
		TaskID: "task-operation-delete-foreign-internal", ProviderTaskID: "cgt-operation-delete-foreign",
		UserId: 8, ChannelId: fixture.channel.Id, Platform: "doubao_ark_official",
		Status: model.TaskStatusSuccess, OfficialModel: "foreign-model-secret",
		Data: []byte(`{"id":"cgt-operation-delete-foreign","status":"succeeded"}`),
	}
	for _, task := range []*model.Task{owned, denied, foreign} {
		require.NoError(t, fixture.database.Create(task).Error)
	}

	ownedResponse := invokeDoubaoOfficialOperationRoute(t, fixture, http.MethodDelete, owned.ProviderTaskID)
	require.Equal(t, http.StatusOK, ownedResponse.Code, ownedResponse.Body.String())
	assert.JSONEq(t, `{}`, ownedResponse.Body.String())
	assert.Equal(t, int32(1), fixture.deleteCalls.Load())
	deniedResponse := invokeDoubaoOfficialOperationRoute(t, fixture, http.MethodDelete, denied.ProviderTaskID)
	require.Equal(t, http.StatusForbidden, deniedResponse.Code, deniedResponse.Body.String())
	assert.JSONEq(t, `{"error":{"code":"official_task_delete_disabled","message":"Official task deletion is disabled for this channel"}}`, deniedResponse.Body.String())
	for _, providerID := range []string{foreign.ProviderTaskID, "cgt-operation-delete-missing"} {
		response := invokeDoubaoOfficialOperationRoute(t, fixture, http.MethodDelete, providerID)
		require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
		assert.JSONEq(t, `{"error":{"code":"task_not_found","message":"Task not found"}}`, response.Body.String())
		assert.NotContains(t, response.Body.String(), providerID)
	}
	malformedResponse := invokeDoubaoOfficialOperationRouteWithBody(t, fixture, http.MethodDelete, "cgt-operation-delete-malformed", `{`, "application/json")
	require.Equal(t, http.StatusBadRequest, malformedResponse.Code, malformedResponse.Body.String())
	assert.Equal(t, int32(1), fixture.deleteCalls.Load(), "denials must not reach upstream DELETE")

	logs, total, err := model.GetUserLogs(7, model.LogFilter{Type: model.LogTypeConsume}, 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 5, total)
	require.Len(t, logs, 5)
	byProviderID := make(map[string]*model.Log, len(logs))
	for _, entry := range logs {
		assert.Equal(t, model.LogTypeConsume, entry.Type)
		assert.Equal(t, "删除任务", entry.ModelName)
		assert.Zero(t, entry.Quota)
		assert.Zero(t, entry.PromptTokens)
		assert.Zero(t, entry.CompletionTokens)
		assert.Equal(t, 7, entry.UserId)
		assert.Equal(t, "doubao-native-user", entry.Username)
		assert.Equal(t, "operation-token", entry.TokenName)
		assert.Equal(t, 41, entry.TokenId)
		assert.Equal(t, "operation-group", entry.Group)
		var metadata map[string]any
		require.NoError(t, json.Unmarshal([]byte(entry.Other), &metadata))
		providerID, ok := metadata["provider_task_id"].(string)
		require.True(t, ok, "operation log must expose the official cgt ID: %s", entry.Other)
		byProviderID[providerID] = entry
		assert.Equal(t, "delete", metadata["operation"])
		assert.Equal(t, "/doubao_ark_official/api/v3/contents/generations/tasks/"+providerID, metadata["request_path"])
		assert.Contains(t, entry.Content, "delete")
		assert.Contains(t, entry.Content, providerID)
		assertDoubaoOfficialOperationLogSafe(t, entry, owned)
		assert.NotContains(t, entry.Content+entry.Other, "sk-denied-secret")
		assert.NotContains(t, entry.Content+entry.Other, "task-operation-delete-denied-internal")
		assert.NotContains(t, entry.Content+entry.Other, "task-operation-delete-foreign-internal")
		assert.NotContains(t, entry.Content+entry.Other, "foreign-model-secret")
	}
	require.Len(t, byProviderID, 5)
	for providerID, wantStatus := range map[string]int{
		owned.ProviderTaskID: http.StatusOK, denied.ProviderTaskID: http.StatusForbidden,
		foreign.ProviderTaskID: http.StatusNotFound, "cgt-operation-delete-missing": http.StatusNotFound,
		"cgt-operation-delete-malformed": http.StatusBadRequest,
	} {
		entry := byProviderID[providerID]
		require.NotNil(t, entry, providerID)
		var metadata map[string]any
		require.NoError(t, json.Unmarshal([]byte(entry.Other), &metadata))
		assert.Equal(t, float64(wantStatus), metadata["http_status"])
		assert.Equal(t, wantStatus == http.StatusOK, metadata["success"])
		switch providerID {
		case owned.ProviderTaskID:
			assert.Equal(t, doubaoNativeTestModel, metadata["task_model"])
			assert.Equal(t, fixture.channel.Id, entry.ChannelId)
		case denied.ProviderTaskID:
			assert.Equal(t, doubaoNativeTestModel, metadata["task_model"])
			assert.Equal(t, deniedChannel.Id, entry.ChannelId)
		default:
			assert.NotContains(t, metadata, "task_model", "an unowned task must not contribute metadata")
			assert.Zero(t, entry.ChannelId, "an unowned task must not contribute metadata")
		}
	}

	var audits int64
	require.NoError(t, fixture.database.Model(&model.AuditLog{}).Where("action = ?", "task_provider_delete").Count(&audits).Error)
	assert.EqualValues(t, 4, audits, "the independent audit trail remains intact")
	var ownedAfter model.Task
	require.NoError(t, fixture.database.First(&ownedAfter, owned.ID).Error)
	assert.True(t, ownedAfter.ProviderDeleted)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), ownedAfter.Status)
	var deniedAfter model.Task
	require.NoError(t, fixture.database.First(&deniedAfter, denied.ID).Error)
	assert.Equal(t, *denied, deniedAfter)
	assertDoubaoOfficialOperationBillingUnchanged(t, fixture, userBefore, channelBefore, tokenBefore)

	require.NoError(t, fixture.database.Migrator().DropTable(&model.Log{}))
	responseAfterLogFailure := invokeDoubaoOfficialOperationRoute(t, fixture, http.MethodDelete, denied.ProviderTaskID)
	require.Equal(t, deniedResponse.Code, responseAfterLogFailure.Code, responseAfterLogFailure.Body.String())
	assert.JSONEq(t, deniedResponse.Body.String(), responseAfterLogFailure.Body.String())
}
