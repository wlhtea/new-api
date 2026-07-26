package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVideoRouterRegistersNewAndLegacyRoutesExactlyOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	require.NotPanics(t, func() {
		SetVideoRouter(engine)
	})

	type routeKey struct {
		method string
		path   string
	}
	counts := make(map[routeKey]int)
	for _, route := range engine.Routes() {
		counts[routeKey{method: route.Method, path: route.Path}]++
	}

	for _, expected := range []routeKey{
		{method: http.MethodPost, path: "/v1/videos"},
		{method: http.MethodGet, path: "/v1/videos/:task_id"},
		{method: http.MethodGet, path: "/v1/videos/:task_id/content"},
		{method: http.MethodPost, path: "/v1/video/generations"},
		{method: http.MethodGet, path: "/v1/video/generations/:task_id"},
		{method: http.MethodPost, path: "/v1/videos/:video_id/remix"},
	} {
		assert.Equalf(t, 1, counts[expected], "%s %s", expected.method, expected.path)
	}
}

func TestVideoContentRouteRejectsClientWithoutToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetVideoRouter(engine)

	request := httptest.NewRequest(http.MethodGet, "/v1/videos/task_public/content", nil)
	request.Header.Set("Authorization", "Bearer ")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	assert.Equal(t, http.StatusUnauthorized, response.Code)
}
