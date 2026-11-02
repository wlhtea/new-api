package middleware

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func officialListResponse(t *testing.T, router *gin.Engine, rawQuery string) (int, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	target := "/doubao_ark_official/api/v3/contents/generations/tasks"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body), recorder.Body.String())
	return recorder.Code, body
}

func officialListIDs(body map[string]any) []string {
	items, _ := body["items"].([]any)
	ids := make([]string, 0, len(items))
	for _, value := range items {
		item, _ := value.(map[string]any)
		id, _ := item["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func TestDoubaoArkOfficialNativeListTaskIDsCap(t *testing.T) {
	setupTaskPluginRouteDB(t)
	router := newDoubaoArkOfficialListRouter(t, 7)
	for _, testCase := range []struct {
		count      int
		wantStatus int
	}{
		{count: 100, wantStatus: http.StatusOK},
		{count: 101, wantStatus: http.StatusBadRequest},
	} {
		t.Run(fmt.Sprintf("ids_%d", testCase.count), func(t *testing.T) {
			query := url.Values{}
			for index := 0; index < testCase.count; index++ {
				query.Add("filter.task_ids", fmt.Sprintf("cgt-filter-%03d", index))
			}
			status, body := officialListResponse(t, router, query.Encode())
			assert.Equal(t, testCase.wantStatus, status, body)
			if testCase.wantStatus == http.StatusBadRequest {
				errorObject, _ := body["error"].(map[string]any)
				assert.Equal(t, "invalid_request", errorObject["code"])
			}
		})
	}
}

func TestDoubaoArkOfficialNativeListScopesAndFilters(t *testing.T) {
	setupTaskPluginRouteDB(t)
	now := time.Now()
	modelAlpha := "doubao-seedance-2-0-260128"
	modelBeta := "doubao-seedance-2-5-260628"
	for _, task := range []*model.Task{
		{TaskID: "task-a-queued", ProviderTaskID: "cgt-a-queued", UserId: 7, Platform: "doubao_ark_official", Status: model.TaskStatusQueued, CreatedAt: now.Add(-5 * time.Hour).Unix(), Data: []byte(`{"status":"queued","model":"` + modelAlpha + `","service_tier":"default"}`)},
		{TaskID: "task-a-succeeded", ProviderTaskID: "cgt-a-succeeded", UserId: 7, Platform: "doubao_ark_official", Status: model.TaskStatusSuccess, CreatedAt: now.Add(-4 * time.Hour).Unix(), Data: []byte(`{"status":"succeeded","model":"` + modelBeta + `","service_tier":"flex"}`)},
		{TaskID: "task-a-failed", ProviderTaskID: "cgt-a-failed", UserId: 7, Platform: "doubao_ark_official", Status: model.TaskStatusFailure, CreatedAt: now.Add(-3 * time.Hour).Unix(), Data: []byte(`{"status":"failed","model":"` + modelAlpha + `","service_tier":"default"}`)},
		{TaskID: "task-a-cancelled", ProviderTaskID: "cgt-a-cancelled", UserId: 7, Platform: "doubao_ark_official", Status: model.TaskStatusFailure, CreatedAt: now.Add(-2 * time.Hour).Unix(), Data: []byte(`{"status":"cancelled","model":"` + modelBeta + `"}`)},
		{TaskID: "task-a-expired", ProviderTaskID: "cgt-a-expired", UserId: 7, Platform: "doubao_ark_official", Status: model.TaskStatusFailure, CreatedAt: now.Add(-time.Hour).Unix(), Data: []byte(`{"status":"expired","model":"` + modelBeta + `"}`)},
		{TaskID: "task-a-old", ProviderTaskID: "cgt-a-old", UserId: 7, Platform: "doubao_ark_official", Status: model.TaskStatusQueued, CreatedAt: now.Add(-8 * 24 * time.Hour).Unix(), Data: []byte(`{"status":"queued","model":"` + modelAlpha + `"}`)},
		{TaskID: "task-b", ProviderTaskID: "cgt-b", UserId: 8, Platform: "doubao_ark_official", Status: model.TaskStatusQueued, CreatedAt: now.Unix(), Data: []byte(`{"status":"queued","model":"` + modelAlpha + `"}`)},
		{TaskID: "task-wrong-plugin", ProviderTaskID: "cgt-wrong-plugin", UserId: 7, Platform: constant.TaskPlatform("doubao"), Status: model.TaskStatusQueued, CreatedAt: now.Unix(), Data: []byte(`{"status":"queued","model":"` + modelAlpha + `"}`)},
	} {
		insertTaskPluginRouteTask(t, task)
	}

	routerA := newDoubaoArkOfficialListRouter(t, 7)
	status, body := officialListResponse(t, routerA, "")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, float64(5), body["total"])
	assert.NotContains(t, officialListIDs(body), "cgt-b")
	assert.NotContains(t, officialListIDs(body), "cgt-wrong-plugin")
	assert.NotContains(t, officialListIDs(body), "cgt-a-old")

	status, body = officialListResponse(t, newDoubaoArkOfficialListRouter(t, 8), "")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, []string{"cgt-b"}, officialListIDs(body))
	status, body = officialListResponse(t, newDoubaoArkOfficialListRouter(t, 9), "")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, float64(0), body["total"])
	assert.Empty(t, officialListIDs(body))

	query := url.Values{}
	for _, id := range []string{"cgt-a-queued", "cgt-a-succeeded", "cgt-b", "cgt-wrong-plugin", "cgt-a-old"} {
		query.Add("filter.task_ids", id)
	}
	status, body = officialListResponse(t, routerA, query.Encode())
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, []string{"cgt-a-succeeded", "cgt-a-queued"}, officialListIDs(body))
	assert.Equal(t, float64(2), body["total"])

	for rawQuery, expected := range map[string][]string{
		"filter.model=" + url.QueryEscape(modelAlpha): {"cgt-a-failed", "cgt-a-queued"},
		"filter.model=alpha":                          {},
		"filter.status=queued":                        {"cgt-a-queued"},
		"filter.status=cancelled":                     {"cgt-a-cancelled"},
		"filter.status=expired":                       {"cgt-a-expired"},
		"filter.service_tier=flex":                    {"cgt-a-succeeded"},
		"filter.service_tier=default":                 {"cgt-a-failed", "cgt-a-queued"},
	} {
		status, body = officialListResponse(t, routerA, rawQuery)
		require.Equal(t, http.StatusOK, status, rawQuery)
		assert.Equal(t, expected, officialListIDs(body), rawQuery)
	}

	for _, rawQuery := range []string{"filter.status=unknown", "filter.service_tier=premium"} {
		status, body = officialListResponse(t, routerA, rawQuery)
		assert.Equal(t, http.StatusBadRequest, status, body)
		errorObject, _ := body["error"].(map[string]any)
		assert.Equal(t, "invalid_request", errorObject["code"])
	}
}
