package middleware

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoubaoArkOfficialNativeListPaginationAndDeterministicOrder(t *testing.T) {
	setupTaskPluginRouteDB(t)
	createdAt := time.Now().Add(-time.Hour).Unix()
	for _, providerID := range []string{"cgt-a", "cgt-b", "cgt-c"} {
		insertTaskPluginRouteTask(t, &model.Task{
			TaskID: "task-internal-" + providerID, ProviderTaskID: providerID, UserId: 7,
			Platform: constant.TaskPlatform("doubao_ark_official"), Status: model.TaskStatusQueued,
			CreatedAt: createdAt, Data: []byte(`{"status":"queued"}`),
		})
	}
	router := newDoubaoArkOfficialListRouter(t, 7)
	listIDs := func(target string) ([]string, int, int) {
		t.Helper()
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/doubao_ark_official/api/v3/contents/generations/tasks"+target, nil))
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		var response struct {
			Total int `json:"total"`
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
		ids := make([]string, 0, len(response.Items))
		for _, item := range response.Items {
			ids = append(ids, item.ID)
		}
		return ids, response.Total, recorder.Code
	}

	ids, total, _ := listIDs("?page_num=1&page_size=2")
	assert.Equal(t, 3, total)
	assert.Equal(t, []string{"cgt-c", "cgt-b"}, ids)
	ids, total, _ = listIDs("?page_num=2&page_size=2")
	assert.Equal(t, 3, total)
	assert.Equal(t, []string{"cgt-a"}, ids)
	ids, _, _ = listIDs("?page_num=500&page_size=500")
	assert.Empty(t, ids)

	for _, target := range []string{
		"?page_num=0", "?page_num=501", "?page_num=not-a-number",
		"?page_size=0", "?page_size=501", "?page_size=not-a-number",
	} {
		t.Run(target, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/doubao_ark_official/api/v3/contents/generations/tasks"+target, nil))
			assert.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
			assert.Contains(t, recorder.Body.String(), `"code":"invalid_request"`)
		})
	}
}

func TestDoubaoArkOfficialNativeListBreaksDuplicateSortKeyTiesByRowID(t *testing.T) {
	setupTaskPluginRouteDB(t)
	const providerID = "cgt-duplicate-pagination-tie"
	createdAt := time.Now().Add(-time.Hour).Unix()
	first := &model.Task{
		TaskID: "task-internal-tie-first", ProviderTaskID: providerID, UserId: 7,
		Platform: constant.TaskPlatform("doubao_ark_official"), Status: model.TaskStatusQueued,
		CreatedAt: createdAt, Data: []byte(`{"status":"queued","seed":101}`),
	}
	second := &model.Task{
		TaskID: "task-internal-tie-second", ProviderTaskID: providerID, UserId: 7,
		Platform: constant.TaskPlatform("doubao_ark_official"), Status: model.TaskStatusQueued,
		CreatedAt: createdAt, Data: []byte(`{"status":"queued","seed":202}`),
	}
	insertTaskPluginRouteTask(t, first)
	insertTaskPluginRouteTask(t, second)
	require.Greater(t, second.ID, first.ID)

	listPage := func(page int) (int, int, int) {
		t.Helper()
		recorder := httptest.NewRecorder()
		target := fmt.Sprintf("/doubao_ark_official/api/v3/contents/generations/tasks?page_num=%d&page_size=1", page)
		newDoubaoArkOfficialListRouter(t, 7).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		var response struct {
			Total int `json:"total"`
			Items []struct {
				ID   string `json:"id"`
				Seed int    `json:"seed"`
			} `json:"items"`
		}
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
		require.Len(t, response.Items, 1)
		assert.Equal(t, providerID, response.Items[0].ID)
		return response.Total, response.Items[0].Seed, recorder.Code
	}

	total, seed, _ := listPage(1)
	assert.Equal(t, 2, total)
	assert.Equal(t, 202, seed, "the higher unique row ID must sort first when both other keys tie")
	total, seed, _ = listPage(2)
	assert.Equal(t, 2, total)
	assert.Equal(t, 101, seed, "the second page must contain the lower unique row ID")
}
