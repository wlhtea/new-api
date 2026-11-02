package controller

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const doubaoNativeTestModel = "doubao-seedance-2-0-260128"

type doubaoNativeFixture struct {
	database      *gorm.DB
	channel       *model.Channel
	generation    *pluginruntime.RoutingGeneration
	upstream      *httptest.Server
	submitBodies  chan []byte
	submitCalls   atomic.Int32
	queryCalls    atomic.Int32
	deleteCalls   atomic.Int32
	deleteStatus  atomic.Int32
	deleteAuth    chan string
	queryBody     string
	queryStarted  chan struct{}
	queryRelease  chan struct{}
	deleteStarted chan struct{}
	deleteRelease chan struct{}
}

func setupDoubaoNativeFixture(t *testing.T, upstreamTaskID string) *doubaoNativeFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()

	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousMemoryCache := common.MemoryCacheEnabled
	previousBatchUpdate := common.BatchUpdateEnabled
	previousLogConsume := common.LogConsumeEnabled
	previousRedisEnabled := common.RedisEnabled
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.User{}, &model.Channel{}, &model.Task{}, &model.Log{}))
	model.DB = database
	model.LOG_DB = database
	common.MemoryCacheEnabled = false
	common.BatchUpdateEnabled = false
	common.LogConsumeEnabled = false
	common.RedisEnabled = false
	previousModelRatios := ratio_setting.ModelRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"`+doubaoNativeTestModel+`":1}`))
	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.MemoryCacheEnabled = previousMemoryCache
		common.BatchUpdateEnabled = previousBatchUpdate
		common.LogConsumeEnabled = previousLogConsume
		common.RedisEnabled = previousRedisEnabled
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(previousModelRatios))
	})
	require.NoError(t, database.Create(&model.User{
		Id: 7, Username: "doubao-native-user", Group: "default", Quota: 10_000_000,
	}).Error)

	fixture := &doubaoNativeFixture{database: database, submitBodies: make(chan []byte, 8), deleteAuth: make(chan string, 16)}
	fixture.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/contents/generations/tasks":
			call := fixture.submitCalls.Add(1)
			body, readErr := io.ReadAll(r.Body)
			if !assert.NoError(t, readErr) {
				http.Error(w, "read request", http.StatusInternalServerError)
				return
			}
			fixture.submitBodies <- body
			responseTaskID := upstreamTaskID
			if call > 1 {
				responseTaskID += "-continuation"
			}
			_, _ = w.Write([]byte(`{"id":"` + responseTaskID + `"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/contents/generations/tasks/"+upstreamTaskID:
			fixture.queryCalls.Add(1)
			if fixture.queryStarted != nil {
				close(fixture.queryStarted)
			}
			if fixture.queryRelease != nil {
				<-fixture.queryRelease
			}
			responseBody := fixture.queryBody
			if responseBody == "" {
				responseBody = `{"id":"` + upstreamTaskID + `","status":"succeeded","content":{"video_url":"https://cdn.example/video.mp4","resolution":"1080p"},"usage":{"total_tokens":1}}`
			}
			_, _ = w.Write([]byte(responseBody))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v3/contents/generations/tasks/"):
			fixture.deleteCalls.Add(1)
			if fixture.deleteStarted != nil {
				close(fixture.deleteStarted)
			}
			if fixture.deleteRelease != nil {
				<-fixture.deleteRelease
			}
			fixture.deleteAuth <- r.Header.Get("Authorization")
			status := int(fixture.deleteStatus.Load())
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			if status >= 200 && status < 300 {
				_, _ = w.Write([]byte(`{}`))
			} else {
				_, _ = w.Write([]byte(`{"error":{"message":"upstream-sensitive-detail"}}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fixture.upstream.Close)

	channel := &model.Channel{
		Type: constant.ChannelTypeDoubaoVideo, Name: "doubao-native-test", Key: "sk-test",
		BaseURL: &fixture.upstream.URL, Status: common.ChannelStatusEnabled,
		Models: doubaoNativeTestModel, Group: "default",
	}
	require.NoError(t, database.Create(channel).Error)
	fixture.channel = channel
	fixture.generation = pluginruntime.DefaultRegistry.Generation()
	require.NotNil(t, fixture.generation)
	return fixture
}

func newDoubaoNativeSubmitContext(t *testing.T, fixture *doubaoNativeFixture, routePath, body string, userID int) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	binding, found := fixture.generation.LookupDeclaredRoute(http.MethodPost, routePath)
	require.True(t, found, "native submit route is not registered")
	require.Equal(t, strings.Split(strings.Trim(routePath, "/"), "/")[0], binding.Plugin.Meta.Key)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, routePath, bytes.NewBufferString(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{
		Generation: fixture.generation, Plugin: binding.Plugin, Route: binding.Route,
	})
	common.SetContextKey(ctx, constant.ContextKeyUserId, userID)
	common.SetContextKey(ctx, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(ctx, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(ctx, constant.ContextKeyTokenGroup, "default")
	common.SetContextKey(ctx, constant.ContextKeyUserQuota, 10_000_000)
	return ctx, recorder
}

func submitDoubaoNativeTaskBody(t *testing.T, fixture *doubaoNativeFixture, routePath, publicTaskID, body string, userID int) (*httptest.ResponseRecorder, *model.Task) {
	t.Helper()
	ctx, recorder := newDoubaoNativeSubmitContext(t, fixture, routePath, body, userID)
	middleware.PrepareTaskPluginRoute()(ctx)
	require.False(t, ctx.IsAborted(), recorder.Body.String())
	require.Nil(t, middleware.SetupContextForSelectedChannel(ctx, fixture.channel, doubaoNativeTestModel))
	billing := &nativeRouteBilling{userID: userID}
	relayInfo := &relaycommon.RelayInfo{
		UserId: userID, UserGroup: "default", UsingGroup: "default", UserQuota: 10_000_000,
		TokenGroup: "default", OriginModelName: doubaoNativeTestModel, Billing: billing,
		TaskRelayInfo: &relaycommon.TaskRelayInfo{
			Action: ctx.GetString("task_action"), PublicTaskID: publicTaskID, LockedChannel: fixture.channel,
		},
	}
	require.Nil(t, relay.ApplyOriginTaskAffinity(ctx, relayInfo))
	outcome, taskErr := executeTaskSubmissionWith(ctx, relayInfo, relay.RelayTaskSubmit)
	require.Nil(t, taskErr)
	require.NotNil(t, outcome)
	presentTaskSubmission(ctx, outcome)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var persisted model.Task
	require.NoError(t, fixture.database.Where("task_id = ?", publicTaskID).First(&persisted).Error)
	return recorder, &persisted
}

func submitDoubaoNativeTask(t *testing.T, fixture *doubaoNativeFixture, routePath, publicTaskID string) (*httptest.ResponseRecorder, *model.Task) {
	t.Helper()
	return submitDoubaoNativeTaskBody(t, fixture, routePath, publicTaskID,
		`{"model":"`+doubaoNativeTestModel+`","content":[{"type":"text","text":"a lighthouse"}],"duration":5}`, 7)
}

func TestDoubaoNativeSubmitPersistsProviderIDWithoutChangingLegacyPublicID(t *testing.T) {
	const (
		providerTaskID = "cgt-legacy-provider-1"
		publicTaskID   = "task_doubao_legacy_public"
	)
	fixture := setupDoubaoNativeFixture(t, providerTaskID)
	recorder, persisted := submitDoubaoNativeTask(t, fixture, "/doubao/api/v3/contents/generations/tasks", publicTaskID)

	assert.JSONEq(t, `{"id":"`+publicTaskID+`"}`, recorder.Body.String())
	assert.Equal(t, publicTaskID, persisted.TaskID)
	assert.Equal(t, providerTaskID, persisted.PrivateData.UpstreamTaskID)

	field, exists := reflect.TypeOf(model.Task{}).FieldByName("ProviderTaskID")
	require.True(t, exists, "Task must persist the parsed provider task ID separately")
	assert.Equal(t, "-", field.Tag.Get("json"))
	assert.Contains(t, field.Tag.Get("gorm"), "index")
	providerValue := reflect.ValueOf(persisted).Elem().FieldByName("ProviderTaskID")
	require.True(t, providerValue.IsValid())
	assert.Equal(t, providerTaskID, providerValue.String())
	assert.True(t, fixture.database.Migrator().HasIndex(&model.Task{}, "idx_tasks_provider_task_id"))

	encoded, err := json.Marshal(persisted)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(encoded, &payload))
	assert.NotContains(t, payload, "provider_task_id")
	assert.Equal(t, int32(1), fixture.submitCalls.Load())
}

func TestDoubaoArkOfficialNativeCreateReturnsProviderID(t *testing.T) {
	const (
		providerTaskID = "cgt-official-native-1"
		publicTaskID   = "task_official_internal"
	)
	fixture := setupDoubaoNativeFixture(t, providerTaskID)
	pluginSetting := `{"task_plugin_key":"doubao_ark_official"}`
	fixture.channel.Type = constant.ChannelTypeTaskPlugin
	fixture.channel.Setting = &pluginSetting
	recorder, persisted := submitDoubaoNativeTask(t, fixture, "/doubao_ark_official/api/v3/contents/generations/tasks", publicTaskID)

	assert.JSONEq(t, `{"id":"`+providerTaskID+`"}`, recorder.Body.String())
	assert.Equal(t, publicTaskID, persisted.TaskID)
	assert.Equal(t, constant.TaskPlatform("doubao_ark_official"), persisted.Platform)
	assert.Equal(t, providerTaskID, persisted.ProviderTaskID)
	assert.Equal(t, providerTaskID, persisted.PrivateData.UpstreamTaskID)
	assert.Equal(t, int32(1), fixture.submitCalls.Load())

	previousAdaptorFactory := service.GetTaskAdaptorFunc
	service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor {
		return relay.GetTaskAdaptor(platform)
	}
	t.Cleanup(func() { service.GetTaskAdaptorFunc = previousAdaptorFactory })
	service.DispatchPlatformUpdate(
		t.Context(), persisted.Platform,
		map[int][]string{fixture.channel.Id: {providerTaskID}},
		map[string]*model.Task{providerTaskID: persisted},
	)
	require.NoError(t, fixture.database.Where("task_id = ?", publicTaskID).First(persisted).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), persisted.Status)
	assert.Equal(t, int32(1), fixture.queryCalls.Load())
}

func TestDoubaoArkOfficialNativeDraftTaskContinuationReusesProviderID(t *testing.T) {
	const routePath = "/doubao_ark_official/api/v3/contents/generations/tasks"
	fixture := setupDoubaoNativeFixture(t, "cgt-official-native-origin")
	pluginSetting := `{"task_plugin_key":"doubao_ark_official"}`
	fixture.channel.Type = constant.ChannelTypeTaskPlugin
	fixture.channel.Setting = &pluginSetting

	created, origin := submitDoubaoNativeTask(t, fixture, routePath, "task_official_origin_internal")
	var createdBody map[string]string
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &createdBody))
	providerTaskID := createdBody["id"]
	require.Equal(t, "cgt-official-native-origin", providerTaskID)
	require.Equal(t, providerTaskID, origin.ProviderTaskID)
	require.NotEqual(t, providerTaskID, origin.TaskID)

	draftBody := `{"model":"` + doubaoNativeTestModel + `","content":[{"type":"draft_task","draft_task":{"id":"` + providerTaskID + `"}}],"duration":5}`
	continued, continuation := submitDoubaoNativeTaskBody(t, fixture, routePath, "task_official_continuation_internal", draftBody, 7)
	assert.JSONEq(t, `{"id":"cgt-official-native-origin-continuation"}`, continued.Body.String())
	assert.Equal(t, "cgt-official-native-origin-continuation", continuation.ProviderTaskID)

	<-fixture.submitBodies
	var outgoing struct {
		Content []struct {
			Type      string `json:"type"`
			DraftTask struct {
				ID string `json:"id"`
			} `json:"draft_task"`
		} `json:"content"`
	}
	require.NoError(t, json.Unmarshal(<-fixture.submitBodies, &outgoing))
	require.Len(t, outgoing.Content, 1)
	assert.Equal(t, "draft_task", outgoing.Content[0].Type)
	assert.Equal(t, providerTaskID, outgoing.Content[0].DraftTask.ID,
		"the native public cgt must resolve to the origin's provider ID, not its internal task_* ID")

	wrongPluginProviderID := "cgt-official-wrong-plugin-scope"
	require.NoError(t, fixture.database.Create(&model.Task{
		TaskID: "task_official_wrong_plugin_internal", ProviderTaskID: wrongPluginProviderID,
		UserId: 7, ChannelId: fixture.channel.Id, Platform: constant.TaskPlatform("doubao"),
		Status:      model.TaskStatusSuccess,
		PrivateData: model.TaskPrivateData{UpstreamTaskID: wrongPluginProviderID},
	}).Error)
	for _, testCase := range []struct {
		name       string
		userID     int
		providerID string
	}{
		{name: "another user cannot reuse the returned cgt", userID: 8, providerID: providerTaskID},
		{name: "same user cannot cross plugin scope", userID: 7, providerID: wrongPluginProviderID},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body := `{"model":"` + doubaoNativeTestModel + `","content":[{"type":"draft_task","draft_task":{"id":"` + testCase.providerID + `"}}],"duration":5}`
			ctx, recorder := newDoubaoNativeSubmitContext(t, fixture, routePath, body, testCase.userID)
			middleware.PrepareTaskPluginRoute()(ctx)
			require.True(t, ctx.IsAborted())
			assert.Equal(t, http.StatusBadRequest, recorder.Code)
			assert.Contains(t, recorder.Body.String(), `"code":"invalid_request"`)
			assert.Contains(t, recorder.Body.String(), "origin task not found or not owned by you")
		})
	}
	assert.Equal(t, int32(2), fixture.submitCalls.Load(), "rejected continuations never reach Ark")
}
