package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestDoubaoArkOfficialNativeListRequiresEnabledChannelOptIn(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		setting       string
		channelType   int
		channelStatus int
	}{
		{
			name:          "missing setting denies by default",
			setting:       `{"task_plugin_key":"doubao_ark_official"}`,
			channelType:   constant.ChannelTypeTaskPlugin,
			channelStatus: common.ChannelStatusEnabled,
		},
		{
			name:          "explicit false denies",
			setting:       `{"task_plugin_key":"doubao_ark_official","allow_official_task_list":false}`,
			channelType:   constant.ChannelTypeTaskPlugin,
			channelStatus: common.ChannelStatusEnabled,
		},
		{
			name:          "disabled channel denies even when opted in",
			setting:       `{"task_plugin_key":"doubao_ark_official","allow_official_task_list":true}`,
			channelType:   constant.ChannelTypeTaskPlugin,
			channelStatus: common.ChannelStatusManuallyDisabled,
		},
		{
			name:          "another plugin cannot grant official list access",
			setting:       `{"task_plugin_key":"doubao","allow_official_task_list":true}`,
			channelType:   constant.ChannelTypeTaskPlugin,
			channelStatus: common.ChannelStatusEnabled,
		},
		{
			name:          "legacy type 54 channel cannot grant official list access",
			setting:       `{"task_plugin_key":"doubao_ark_official","allow_official_task_list":true}`,
			channelType:   constant.ChannelTypeDoubaoVideo,
			channelStatus: common.ChannelStatusEnabled,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			setupTaskPluginRouteDB(t)
			var channel model.Channel
			require.NoError(t, model.DB.First(&channel, 1).Error)
			channel.Setting = &testCase.setting
			channel.Type = testCase.channelType
			channel.Status = testCase.channelStatus
			require.NoError(t, model.DB.Save(&channel).Error)
			insertTaskPluginRouteTask(t, &model.Task{
				TaskID: "task-list-denied-internal", ProviderTaskID: "cgt-list-denied", UserId: 7,
				Platform: constant.TaskPlatform("doubao_ark_official"), Status: model.TaskStatusQueued,
				CreatedAt: time.Now().Unix(), Data: []byte(`{"status":"queued"}`),
			})

			recorder := httptest.NewRecorder()
			newDoubaoArkOfficialListRouter(t, 7).ServeHTTP(recorder,
				httptest.NewRequest(http.MethodGet, "/doubao_ark_official/api/v3/contents/generations/tasks", nil))

			assert.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
			assert.JSONEq(t, `{"error":{"code":"official_task_list_disabled","message":"Official task listing is disabled"}}`, recorder.Body.String())
			assert.NotContains(t, recorder.Body.String(), "cgt-list-denied")
			assert.NotContains(t, recorder.Body.String(), "task-list-denied-internal")
		})
	}
}

func TestDoubaoArkOfficialNativeListSwitchDoesNotDisableSingleTaskQuery(t *testing.T) {
	setupTaskPluginRouteDB(t)
	deniedSetting := `{"task_plugin_key":"doubao_ark_official","allow_official_task_list":false}`
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 1).Update("setting", deniedSetting).Error)
	insertTaskPluginRouteTask(t, &model.Task{
		TaskID: "task-list-switch-query-internal", ProviderTaskID: "cgt-list-switch-query", UserId: 7,
		Platform: constant.TaskPlatform("doubao_ark_official"), Status: model.TaskStatusSuccess,
		CreatedAt: time.Now().Unix(), Data: []byte(`{"status":"succeeded"}`),
	})

	list := httptest.NewRecorder()
	newDoubaoArkOfficialListRouter(t, 7).ServeHTTP(list,
		httptest.NewRequest(http.MethodGet, "/doubao_ark_official/api/v3/contents/generations/tasks", nil))
	assert.Equal(t, http.StatusForbidden, list.Code, list.Body.String())
	assert.Contains(t, list.Body.String(), `"code":"official_task_list_disabled"`)

	plugin, routeIndex := doubaoArkOfficialRoute(t, http.MethodGet, "/doubao_ark_official/api/v3/contents/generations/tasks/:task_id")
	queryRouter := gin.New()
	queryRouter.GET("/doubao_ark_official/api/v3/contents/generations/tasks/:task_id",
		pinTaskPluginRoute(plugin, routeIndex),
		func(c *gin.Context) { c.Set("id", 7); c.Next() },
		PrepareTaskPluginRoute(),
	)
	query := httptest.NewRecorder()
	queryRouter.ServeHTTP(query, httptest.NewRequest(http.MethodGet,
		"/doubao_ark_official/api/v3/contents/generations/tasks/cgt-list-switch-query", nil))
	require.Equal(t, http.StatusOK, query.Code, query.Body.String())
	assert.JSONEq(t, `{"id":"cgt-list-switch-query","status":"succeeded"}`, query.Body.String())
}

func TestDoubaoArkOfficialNativeListFiltersAllowedChannelsInSQL(t *testing.T) {
	setupTaskPluginRouteDB(t)
	enabled := common.ChannelStatusEnabled
	for _, channel := range []*model.Channel{
		{Id: 2, Type: constant.ChannelTypeTaskPlugin, Status: enabled, Name: "list-false", Key: "sk-list-false", Setting: stringPointer(`{"task_plugin_key":"doubao_ark_official","allow_official_task_list":false}`)},
		{Id: 3, Type: constant.ChannelTypeTaskPlugin, Status: enabled, Name: "list-missing", Key: "sk-list-missing", Setting: stringPointer(`{"task_plugin_key":"doubao_ark_official"}`)},
		{Id: 4, Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusManuallyDisabled, Name: "list-disabled", Key: "sk-list-disabled", Setting: stringPointer(`{"task_plugin_key":"doubao_ark_official","allow_official_task_list":true}`)},
		{Id: 5, Type: constant.ChannelTypeTaskPlugin, Status: enabled, Name: "list-other-plugin", Key: "sk-list-other-plugin", Setting: stringPointer(`{"task_plugin_key":"doubao","allow_official_task_list":true}`)},
		{Id: 6, Type: constant.ChannelTypeNewAPI, Status: enabled, Name: "list-gateway", Key: "sk-list-gateway", Setting: stringPointer(`{"task_plugin_key":"doubao_ark_official","allow_official_task_list":true}`)},
	} {
		require.NoError(t, model.DB.Create(channel).Error)
	}

	now := time.Now().Unix()
	for index, task := range []*model.Task{
		{TaskID: "task-list-allowed-old-internal", ProviderTaskID: "cgt-list-allowed-old", UserId: 7, ChannelId: 1, Platform: "doubao_ark_official", Status: model.TaskStatusQueued, CreatedAt: now - 1, Data: []byte(`{"status":"queued"}`)},
		{TaskID: "task-list-allowed-new-internal", ProviderTaskID: "cgt-list-allowed-new", UserId: 7, ChannelId: 1, Platform: "doubao_ark_official", Status: model.TaskStatusSuccess, CreatedAt: now, Data: []byte(`{"status":"succeeded","private_data":{"key":"must-not-leak"}}`)},
		{TaskID: "task-list-denied-false-internal", ProviderTaskID: "cgt-list-denied-false", UserId: 7, ChannelId: 2, Platform: "doubao_ark_official", Status: model.TaskStatusQueued, CreatedAt: now + 1, Data: []byte(`{"status":"queued"}`)},
		{TaskID: "task-list-denied-missing-internal", ProviderTaskID: "cgt-list-denied-missing", UserId: 7, ChannelId: 3, Platform: "doubao_ark_official", Status: model.TaskStatusQueued, CreatedAt: now + 2, Data: []byte(`{"status":"queued"}`)},
		{TaskID: "task-list-denied-disabled-internal", ProviderTaskID: "cgt-list-denied-disabled", UserId: 7, ChannelId: 4, Platform: "doubao_ark_official", Status: model.TaskStatusQueued, CreatedAt: now + 3, Data: []byte(`{"status":"queued"}`)},
		{TaskID: "task-list-denied-plugin-internal", ProviderTaskID: "cgt-list-denied-plugin", UserId: 7, ChannelId: 5, Platform: "doubao_ark_official", Status: model.TaskStatusQueued, CreatedAt: now + 4, Data: []byte(`{"status":"queued"}`)},
		{TaskID: "task-list-denied-gateway-internal", ProviderTaskID: "cgt-list-denied-gateway", UserId: 7, ChannelId: 6, Platform: "doubao_ark_official", Status: model.TaskStatusQueued, CreatedAt: now + 5, Data: []byte(`{"status":"queued"}`)},
		{TaskID: "task-list-foreign-internal", ProviderTaskID: "cgt-list-foreign", UserId: 8, ChannelId: 1, Platform: "doubao_ark_official", Status: model.TaskStatusQueued, CreatedAt: now + 6, Data: []byte(`{"status":"queued"}`)},
		{TaskID: "task-list-wrong-plugin-internal", ProviderTaskID: "cgt-list-wrong-plugin", UserId: 7, ChannelId: 1, Platform: "doubao", Status: model.TaskStatusQueued, CreatedAt: now + 7, Data: []byte(`{"status":"queued"}`)},
	} {
		task.UpdatedAt = task.CreatedAt + int64(index)
		insertTaskPluginRouteTask(t, task)
	}

	const callbackName = "test:official_list_channel_permissions"
	var countSQL []string
	var dataSQL []string
	var maxSelectedRows int64
	require.NoError(t, model.DB.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table != "tasks" {
			return
		}
		sql := tx.Statement.SQL.String()
		switch {
		case strings.Contains(sql, "SELECT count("):
			countSQL = append(countSQL, sql)
		case strings.Contains(sql, "SELECT *"):
			dataSQL = append(dataSQL, sql)
			if tx.RowsAffected > maxSelectedRows {
				maxSelectedRows = tx.RowsAffected
			}
		}
	}))
	t.Cleanup(func() { _ = model.DB.Callback().Query().Remove(callbackName) })

	router := newDoubaoArkOfficialListRouter(t, 7)
	status, body := officialListResponse(t, router, "page_num=1&page_size=1")
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, float64(2), body["total"])
	assert.Equal(t, []string{"cgt-list-allowed-new"}, officialListIDs(body))
	assert.NotContains(t, fmt.Sprint(body["items"]), "must-not-leak")

	status, body = officialListResponse(t, router, "page_num=2&page_size=1")
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, float64(2), body["total"])
	assert.Equal(t, []string{"cgt-list-allowed-old"}, officialListIDs(body))

	status, body = officialListResponse(t, router, "filter.task_ids=cgt-list-allowed-new&filter.task_ids=cgt-list-denied-false&filter.task_ids=cgt-list-foreign")
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, float64(1), body["total"])
	assert.Equal(t, []string{"cgt-list-allowed-new"}, officialListIDs(body))

	status, body = officialListResponse(t, router, "filter.task_ids=cgt-list-denied-false&filter.task_ids=cgt-list-denied-missing&filter.task_ids=cgt-list-foreign")
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, float64(0), body["total"])
	assert.Empty(t, officialListIDs(body))

	require.Len(t, countSQL, 4)
	require.Len(t, dataSQL, 3, "each non-empty page must use one bounded task query, with no per-row queries")
	for _, sql := range append(countSQL, dataSQL...) {
		assert.Contains(t, sql, "channel_id", "both total and page queries must filter allowed channels in SQL")
	}
	assert.LessOrEqual(t, maxSelectedRows, int64(1), "task rows must remain bounded by page_size")
}

func stringPointer(value string) *string {
	return &value
}
