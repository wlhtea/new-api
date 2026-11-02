package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoubaoArkOfficialNativeTombstonesAreHiddenWithoutDeletingAuditRow(t *testing.T) {
	setupTaskPluginRouteDB(t)
	now := time.Now().Unix()
	for _, task := range []*model.Task{
		{TaskID: "task-active", ProviderTaskID: "cgt-active", UserId: 7, Platform: "doubao_ark_official", Status: model.TaskStatusSuccess, CreatedAt: now, Data: []byte(`{"status":"succeeded"}`)},
		{TaskID: "task-tombstoned", ProviderTaskID: "cgt-tombstoned", UserId: 7, Platform: "doubao_ark_official", Status: model.TaskStatusSuccess, CreatedAt: now - 1, ProviderDeleted: true, Data: []byte(`{"status":"succeeded"}`)},
	} {
		insertTaskPluginRouteTask(t, task)
	}

	status, body := officialListResponse(t, newDoubaoArkOfficialListRouter(t, 7), "")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, float64(1), body["total"])
	assert.Equal(t, []string{"cgt-active"}, officialListIDs(body))

	plugin, queryRoute := doubaoArkOfficialRoute(t, http.MethodGet, "/doubao_ark_official/api/v3/contents/generations/tasks/:task_id")
	router := gin.New()
	router.GET("/doubao_ark_official/api/v3/contents/generations/tasks/:task_id",
		pinTaskPluginRoute(plugin, queryRoute),
		func(c *gin.Context) { c.Set("id", 7); c.Next() },
		PrepareTaskPluginRoute(),
	)
	for providerID, expected := range map[string]int{"cgt-active": http.StatusOK, "cgt-tombstoned": http.StatusNotFound} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/doubao_ark_official/api/v3/contents/generations/tasks/"+providerID, nil))
		assert.Equal(t, expected, recorder.Code, recorder.Body.String())
	}

	var persisted model.Task
	require.NoError(t, model.DB.Where("task_id = ?", "task-tombstoned").First(&persisted).Error)
	assert.True(t, persisted.ProviderDeleted)
	assert.Equal(t, "cgt-tombstoned", persisted.ProviderTaskID)
}
