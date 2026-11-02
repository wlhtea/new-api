package router

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type doubaoArkAliasIntegrationFixture struct {
	engine       *gin.Engine
	database     *gorm.DB
	channel      *model.Channel
	deleteCalls  atomic.Int32
	deletePaths  chan string
	deleteAuth   chan string
	otherCalls   atomic.Int32
	providerID   string
	internalID   string
	officialData string
}

func setupDoubaoArkAliasIntegration(t *testing.T) *doubaoArkAliasIntegrationFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()
	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousMemoryCache := common.MemoryCacheEnabled
	previousBatchUpdate := common.BatchUpdateEnabled
	previousLogConsume := common.LogConsumeEnabled
	previousRedis := common.RedisEnabled
	previousRateLimit := setting.ModelRequestRateLimitEnabled
	previousAdaptorFactory := service.GetTaskAdaptorFunc
	previousSQLitePath := common.SQLitePath
	previousMasterNode := common.IsMasterNode
	previousDatabaseType := common.MainDatabaseType()
	previousLogDatabaseType := common.LogDatabaseType()
	common.SQLitePath = t.TempDir() + "/ark-alias-integration.db"
	common.IsMasterNode = false
	t.Setenv("SQL_DSN", "")
	require.NoError(t, model.InitDB())
	database := model.DB
	require.NoError(t, database.AutoMigrate(
		&model.User{}, &model.Token{}, &model.Channel{}, &model.Task{},
		&model.Ability{}, &model.AuditLog{}, &model.Log{},
	))
	model.DB = database
	model.LOG_DB = database
	common.MemoryCacheEnabled = false
	common.BatchUpdateEnabled = false
	common.LogConsumeEnabled = false
	common.RedisEnabled = false
	setting.ModelRequestRateLimitEnabled = false
	service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor {
		return relay.GetTaskAdaptor(platform)
	}
	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.SQLitePath = previousSQLitePath
		common.IsMasterNode = previousMasterNode
		common.SetDatabaseTypes(previousDatabaseType, previousLogDatabaseType)
		common.MemoryCacheEnabled = previousMemoryCache
		common.BatchUpdateEnabled = previousBatchUpdate
		common.LogConsumeEnabled = previousLogConsume
		common.RedisEnabled = previousRedis
		setting.ModelRequestRateLimitEnabled = previousRateLimit
		service.GetTaskAdaptorFunc = previousAdaptorFactory
	})

	fixture := &doubaoArkAliasIntegrationFixture{
		database: database, deletePaths: make(chan string, 4), deleteAuth: make(chan string, 4),
		providerID: "cgt-alias-production-integration", internalID: "task_alias_production_internal",
		officialData: `{"id":"task_alias_production_internal","status":"succeeded","seed":42,"resolution":"720p","content":{"video_url":"https://cdn.example/alias.mp4","last_frame_url":"https://cdn.example/alias-last.jpg"},"private_data":{"key":"must-not-leak"}}`,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			fixture.deleteCalls.Add(1)
			fixture.deletePaths <- r.URL.Path
			fixture.deleteAuth <- r.Header.Get("Authorization")
			if r.URL.Path != "/api/v3/contents/generations/tasks/"+fixture.providerID {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"wrong cgt"}}`))
				return
			}
			_, _ = w.Write([]byte(`{}`))
			return
		}
		fixture.otherCalls.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)

	require.NoError(t, database.Create(&model.User{
		Id: 7, Username: "ark-alias-integration-user", Role: common.RoleCommonUser,
		Status: common.UserStatusEnabled, Group: "default", Quota: 10000,
	}).Error)
	require.NoError(t, database.Create(&model.Token{
		Id: 1, UserId: 7, Key: "aliasintegration", Name: "alias integration",
		Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true,
	}).Error)
	pluginSetting := `{"task_plugin_key":"doubao_ark_official","allow_official_task_list":true,"allow_official_task_delete":true}`
	channel := &model.Channel{
		Type: constant.ChannelTypeTaskPlugin, Name: "ark-alias-integration", Key: "sk-upstream-alias",
		BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled,
		Models: doubaoArkAliasTestModel, Group: "default", Setting: &pluginSetting,
	}
	require.NoError(t, database.Create(channel).Error)
	fixture.channel = channel

	official, found := jsplugin.DefaultRegistry.Get("doubao_ark_official")
	require.True(t, found)
	builtin, found := jsplugin.DefaultRegistry.Get("doubao")
	require.True(t, found)
	outer, registry := newPluginRouterTest(t, []*jsplugin.LoadedPlugin{official, builtin}, productionPluginRouteHandlers)
	outer.NoRoute(
		(&pluginRouteDispatcher{registry: registry}).dispatch,
		func(c *gin.Context) { c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "fallback"}) },
	)
	fixture.engine = outer
	return fixture
}

func (fixture *doubaoArkAliasIntegrationFixture) createTask(t *testing.T, platform constant.TaskPlatform, internalID, data string) *model.Task {
	t.Helper()
	now := time.Now().Unix()
	task := &model.Task{
		TaskID: internalID, ProviderTaskID: fixture.providerID, UserId: 7, ChannelId: fixture.channel.Id,
		Platform: platform, Status: model.TaskStatusSuccess, Progress: "100%",
		CreatedAt: now, UpdatedAt: now, Data: []byte(data),
	}
	require.NoError(t, fixture.database.Create(task).Error)
	return task
}

func performDoubaoArkAliasRequest(fixture *doubaoArkAliasIntegrationFixture, method, target string, authenticated bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	if authenticated {
		request.Header.Set("Authorization", "Bearer sk-aliasintegration")
	}
	recorder := httptest.NewRecorder()
	fixture.engine.ServeHTTP(recorder, request)
	return recorder
}

func TestDoubaoArkOfficialExactAliasListUsesProductionChain(t *testing.T) {
	fixture := setupDoubaoArkAliasIntegration(t)
	fixture.createTask(t, constant.TaskPlatform("doubao_ark_official"), fixture.internalID, fixture.officialData)
	fixture.createTask(t, constant.TaskPlatform("doubao"), "task_builtin_doubao_must_not_be_used", `{"id":"cgt-builtin-doubao","status":"failed","builtin_doubao_sentinel":true}`)

	unauthenticated := performDoubaoArkAliasRequest(fixture, http.MethodGet, "/api/v3/contents/generations/tasks", false)
	assert.Equal(t, http.StatusUnauthorized, unauthenticated.Code, unauthenticated.Body.String())

	response := performDoubaoArkAliasRequest(fixture, http.MethodGet, "/api/v3/contents/generations/tasks?page_num=1&page_size=20", true)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.JSONEq(t, `{"total":1,"items":[{"id":"cgt-alias-production-integration","status":"succeeded","seed":42,"resolution":"720p","content":{"video_url":"https://cdn.example/alias.mp4","last_frame_url":"https://cdn.example/alias-last.jpg"}}]}`, response.Body.String())
	assert.NotContains(t, response.Body.String(), fixture.internalID)
	assert.NotContains(t, response.Body.String(), "must-not-leak")
	assert.NotContains(t, response.Body.String(), "builtin_doubao_sentinel")
	assert.NotContains(t, response.Body.String(), "task_builtin_doubao")
	assert.Equal(t, int32(0), fixture.otherCalls.Load(), "a local list must not call Ark or the built-in doubao adaptor")
	assert.Equal(t, int32(0), fixture.deleteCalls.Load())
}

func TestDoubaoArkOfficialExactAliasDeleteUsesProductionChainAndCGT(t *testing.T) {
	fixture := setupDoubaoArkAliasIntegration(t)
	official := fixture.createTask(t, constant.TaskPlatform("doubao_ark_official"), fixture.internalID, fixture.officialData)
	builtin := fixture.createTask(t, constant.TaskPlatform("doubao"), "task_builtin_doubao_must_not_be_used", `{"id":"cgt-builtin-doubao","status":"succeeded"}`)

	response := performDoubaoArkAliasRequest(fixture, http.MethodDelete, "/api/v3/contents/generations/tasks/"+fixture.providerID, true)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.JSONEq(t, `{}`, response.Body.String())
	require.Equal(t, int32(1), fixture.deleteCalls.Load())
	assert.Equal(t, "/api/v3/contents/generations/tasks/"+fixture.providerID, <-fixture.deletePaths)
	assert.Equal(t, "Bearer sk-upstream-alias", <-fixture.deleteAuth)
	assert.Equal(t, int32(0), fixture.otherCalls.Load())

	var officialAfter model.Task
	require.NoError(t, fixture.database.Where("id = ?", official.ID).First(&officialAfter).Error)
	assert.True(t, officialAfter.ProviderDeleted)
	var builtinAfter model.Task
	require.NoError(t, fixture.database.Where("id = ?", builtin.ID).First(&builtinAfter).Error)
	assert.False(t, builtinAfter.ProviderDeleted, "the built-in doubao row must not be mutated by the official alias")

	list := performDoubaoArkAliasRequest(fixture, http.MethodGet, "/api/v3/contents/generations/tasks", true)
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	assert.JSONEq(t, `{"total":0,"items":[]}`, list.Body.String())
}
