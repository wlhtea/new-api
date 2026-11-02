package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoubaoArkOfficialNativeAliasDispatchesListAndDeleteOnlyToOfficialPlugin(t *testing.T) {
	official, found := jsplugin.DefaultRegistry.Get("doubao_ark_official")
	require.True(t, found)
	builtin, found := jsplugin.DefaultRegistry.Get("doubao")
	require.True(t, found)
	matches := make(chan string, 4)
	handlers := func(generation *jsplugin.RoutingGeneration, binding jsplugin.RouteBinding) []gin.HandlerFunc {
		production := productionPluginRouteHandlers(generation, binding)
		return []gin.HandlerFunc{
			production[0],
			func(c *gin.Context) {
				pinned := c.MustGet(jsplugin.ContextKeyPinnedRoute).(jsplugin.PinnedRoute)
				matches <- pinned.Plugin.Meta.Key + " " + c.Request.Method
				c.JSON(http.StatusOK, gin.H{"plugin": pinned.Plugin.Meta.Key})
			},
		}
	}
	outer, registry := newPluginRouterTest(t, []*jsplugin.LoadedPlugin{official, builtin}, handlers)
	outer.NoRoute(
		(&pluginRouteDispatcher{registry: registry}).dispatch,
		func(c *gin.Context) { c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "fallback"}) },
	)

	list := httptest.NewRecorder()
	outer.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/v3/contents/generations/tasks?page_num=1&page_size=20", nil))
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	assert.JSONEq(t, `{"plugin":"doubao_ark_official"}`, list.Body.String())
	assert.Equal(t, "doubao_ark_official GET", <-matches)

	deleted := httptest.NewRecorder()
	outer.ServeHTTP(deleted, httptest.NewRequest(http.MethodDelete, "/api/v3/contents/generations/tasks/cgt-alias-delete", nil))
	require.Equal(t, http.StatusOK, deleted.Code, deleted.Body.String())
	assert.JSONEq(t, `{"plugin":"doubao_ark_official"}`, deleted.Body.String())
	assert.Equal(t, "doubao_ark_official DELETE", <-matches)
	assert.Len(t, matches, 0, "built-in doubao must not receive either official alias")
}
