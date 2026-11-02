package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestDoubaoArkOfficialNativeQueryWritesZeroQuotaOperationLogs(t *testing.T) {
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
	assert.EqualValues(t, 3, total, "single-task GETs log once each; the list adds no substitute operation log")
	require.Len(t, logs, 3)
	byProviderID := make(map[string]*model.Log, len(logs))
	for _, entry := range logs {
		assert.Equal(t, model.LogTypeConsume, entry.Type)
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
		assert.Equal(t, "query", metadata["operation"])
		assert.Equal(t, "/doubao_ark_official/api/v3/contents/generations/tasks/"+providerID, metadata["request_path"])
		assert.Contains(t, entry.Content, "query")
		assert.Contains(t, entry.Content, providerID)
		assertDoubaoOfficialOperationLogSafe(t, entry, owned)
	}
	require.Len(t, byProviderID, 3)
	for providerID, wantStatus := range map[string]int{
		owned.ProviderTaskID: http.StatusOK, foreign.ProviderTaskID: http.StatusNotFound, "cgt-operation-query-missing": http.StatusNotFound,
	} {
		entry := byProviderID[providerID]
		require.NotNil(t, entry, providerID)
		var metadata map[string]any
		require.NoError(t, json.Unmarshal([]byte(entry.Other), &metadata))
		assert.Equal(t, float64(wantStatus), metadata["http_status"])
		assert.Equal(t, wantStatus == http.StatusOK, metadata["success"])
		if providerID == owned.ProviderTaskID {
			assert.Equal(t, doubaoNativeTestModel, entry.ModelName)
			assert.Equal(t, fixture.channel.Id, entry.ChannelId)
		} else {
			assert.Empty(t, entry.ModelName, "an unowned task must not contribute metadata")
			assert.Zero(t, entry.ChannelId, "an unowned task must not contribute metadata")
		}
	}
	assertDoubaoOfficialOperationBillingUnchanged(t, fixture, userBefore, channelBefore, tokenBefore)

	require.NoError(t, fixture.database.Migrator().DropTable(&model.Log{}))
	responseAfterLogFailure := invokeDoubaoOfficialOperationRoute(t, fixture, http.MethodGet, owned.ProviderTaskID)
	require.Equal(t, http.StatusOK, responseAfterLogFailure.Code, responseAfterLogFailure.Body.String())
	assert.JSONEq(t, ownedResponse.Body.String(), responseAfterLogFailure.Body.String())
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
			assert.Equal(t, doubaoNativeTestModel, entry.ModelName)
			assert.Equal(t, fixture.channel.Id, entry.ChannelId)
		case denied.ProviderTaskID:
			assert.Equal(t, doubaoNativeTestModel, entry.ModelName)
			assert.Equal(t, deniedChannel.Id, entry.ChannelId)
		default:
			assert.Empty(t, entry.ModelName, "an unowned task must not contribute metadata")
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
