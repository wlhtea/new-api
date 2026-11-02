package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoubaoArkOfficialNativeProjectionPreservesPublicFieldsAndDropsPrivateData(t *testing.T) {
	setupTaskPluginRouteDB(t)
	const (
		providerTaskID = "cgt-official-full-projection"
		internalTaskID = "task_internal_full_projection"
	)
	officialSnapshot := map[string]any{
		"id":                      internalTaskID,
		"model":                   "doubao-seedance-2-5-260628",
		"status":                  "succeeded",
		"content":                 map[string]any{"video_url": "https://cdn.example/video.mov", "last_frame_url": "https://cdn.example/last.jpg", "file_url": "https://cdn.example/source.mov", "private_content": "content-sentinel"},
		"usage":                   map[string]any{"completion_tokens": 1234, "total_tokens": 1234, "tool_usage": map[string]any{"web_search": 2, "private_tool": "tool-sentinel"}, "private_usage": "usage-sentinel"},
		"error":                   map[string]any{"code": "", "message": "", "private_error": "error-sentinel"},
		"created_at":              1779348818,
		"updated_at":              1779348874,
		"seed":                    78674,
		"resolution":              "720p",
		"ratio":                   "16:9",
		"duration":                5,
		"frames":                  120,
		"framespersecond":         24,
		"service_tier":            "default",
		"execution_expires_after": 172800,
		"generate_audio":          true,
		"draft":                   false,
		"draft_task_id":           "cgt-official-draft-origin",
		"priority":                3,
		"output_format":           "mov",
		"return_last_frame":       true,
		"safety_identifier":       "official-end-user",
		"tools":                   []any{map[string]any{"type": "web_search", "private_tool_config": "tool-config-sentinel"}},
		"private_data":            map[string]any{"key": "sk-private-sentinel"},
		"channel_id":              91,
		"billing_snapshot":        map[string]any{"quota": 12},
		"api_key":                 "sk-top-level-sentinel",
		"upstream_task_id":        "private-upstream-sentinel",
	}
	encoded, err := common.Marshal(officialSnapshot)
	require.NoError(t, err)
	insertTaskPluginRouteTask(t, &model.Task{
		TaskID: internalTaskID, ProviderTaskID: providerTaskID, UserId: 7,
		Platform: constant.TaskPlatform("doubao_ark_official"), Status: model.TaskStatusSuccess,
		Data:        encoded,
		PrivateData: model.TaskPrivateData{UpstreamTaskID: providerTaskID, Key: "sk-private-column-sentinel"},
	})

	expectedItem := map[string]any{
		"id":                      providerTaskID,
		"model":                   "doubao-seedance-2-5-260628",
		"status":                  "succeeded",
		"content":                 map[string]any{"video_url": "https://cdn.example/video.mov", "last_frame_url": "https://cdn.example/last.jpg", "file_url": "https://cdn.example/source.mov"},
		"usage":                   map[string]any{"completion_tokens": 1234, "total_tokens": 1234, "tool_usage": map[string]any{"web_search": 2}},
		"error":                   map[string]any{"code": "", "message": ""},
		"created_at":              1779348818,
		"updated_at":              1779348874,
		"seed":                    78674,
		"resolution":              "720p",
		"ratio":                   "16:9",
		"duration":                5,
		"frames":                  120,
		"framespersecond":         24,
		"service_tier":            "default",
		"execution_expires_after": 172800,
		"generate_audio":          true,
		"draft":                   false,
		"draft_task_id":           "cgt-official-draft-origin",
		"priority":                3,
		"output_format":           "mov",
		"return_last_frame":       true,
		"safety_identifier":       "official-end-user",
		"tools":                   []any{map[string]any{"type": "web_search"}},
	}
	expectedJSON, err := common.Marshal(expectedItem)
	require.NoError(t, err)

	plugin, found := jsplugin.DefaultRegistry.Get("doubao_ark_official")
	require.True(t, found)
	queryRouter := gin.New()
	queryRouter.GET("/doubao_ark_official/api/v3/contents/generations/tasks/:task_id",
		pinTaskPluginRoute(plugin, 1),
		func(c *gin.Context) { c.Set("id", 7); c.Next() },
		PrepareTaskPluginRoute(),
	)
	query := httptest.NewRecorder()
	queryRouter.ServeHTTP(query, httptest.NewRequest(http.MethodGet, "/doubao_ark_official/api/v3/contents/generations/tasks/"+providerTaskID, nil))
	require.Equal(t, http.StatusOK, query.Code, query.Body.String())
	assert.JSONEq(t, string(expectedJSON), query.Body.String())

	list := httptest.NewRecorder()
	newDoubaoArkOfficialListRouter(t, 7).ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/doubao_ark_official/api/v3/contents/generations/tasks", nil))
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	expectedList, err := common.Marshal(map[string]any{"total": 1, "items": []any{expectedItem}})
	require.NoError(t, err)
	assert.JSONEq(t, string(expectedList), list.Body.String())

	for _, responseBody := range []string{query.Body.String(), list.Body.String()} {
		for _, sentinel := range []string{
			internalTaskID, "private_data", "sk-private-sentinel", "sk-private-column-sentinel",
			"channel_id", "billing_snapshot", "api_key", "sk-top-level-sentinel", "upstream_task_id",
			"private-upstream-sentinel", "private_content", "content-sentinel", "private_usage",
			"usage-sentinel", "private_tool", "tool-sentinel", "private_error", "error-sentinel",
			"private_tool_config", "tool-config-sentinel",
		} {
			assert.NotContains(t, responseBody, sentinel)
		}
	}
}
