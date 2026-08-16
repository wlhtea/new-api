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
	projecti18n "github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type distributorEndpointGuardFixture struct {
	modelName      string
	openCodeGoID   int
	fallbackID     int
	affinityHeader string
}

func setupDistributorEndpointGuardTest(t *testing.T) distributorEndpointGuardFixture {
	t.Helper()
	previousDB := model.DB
	previousDatabaseType := common.MainDatabaseType()
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	previousRedisEnabled := common.RedisEnabled
	require.NoError(t, projecti18n.Init())
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:distributor-endpoint-guard-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	model.DB = db
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	common.MemoryCacheEnabled = true
	common.RedisEnabled = false

	modelName := fmt.Sprintf("endpoint-guard-%d", time.Now().UnixNano())
	highPriority := int64(100)
	lowPriority := int64(1)
	weight := uint(100)
	openCodeGo := &model.Channel{
		Type:     constant.ChannelTypeOpenCodeGo,
		Status:   common.ChannelStatusEnabled,
		Name:     "affinity OpenCode Go",
		Models:   modelName,
		Group:    "default",
		Priority: &highPriority,
		Weight:   &weight,
	}
	fallback := &model.Channel{
		Type:     constant.ChannelTypeOpenAI,
		Key:      "fallback-key",
		Status:   common.ChannelStatusEnabled,
		Name:     "affinity fallback",
		Models:   modelName,
		Group:    "default",
		Priority: &lowPriority,
		Weight:   &weight,
	}
	require.NoError(t, db.Create(openCodeGo).Error)
	require.NoError(t, db.Create(fallback).Error)
	require.NoError(t, openCodeGo.AddAbilities(nil))
	require.NoError(t, fallback.AddAbilities(nil))
	model.InitChannelCache()

	affinityHeader := fmt.Sprintf("endpoint-affinity-%d", time.Now().UnixNano())
	setting := operation_setting.GetChannelAffinitySetting()
	previousSetting := *setting
	*setting = operation_setting.ChannelAffinitySetting{
		Enabled:           true,
		DefaultTTLSeconds: 60,
		Rules: []operation_setting.ChannelAffinityRule{
			{
				Name:       "endpoint guard",
				ModelRegex: []string{"^" + modelName + "$"},
				PathRegex:  []string{"^/v1/moderations$"},
				KeySources: []operation_setting.ChannelAffinityKeySource{
					{Type: "request_header", Key: "X-Endpoint-Affinity"},
				},
				IncludeRuleName:  true,
				IncludeModelName: true,
			},
		},
	}

	seedContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	seedContext.Request = httptest.NewRequest(http.MethodPost, "/v1/moderations", strings.NewReader(`{"model":"`+modelName+`"}`))
	seedContext.Request.Header.Set("Content-Type", "application/json")
	seedContext.Request.Header.Set("X-Endpoint-Affinity", affinityHeader)
	_, _ = service.GetPreferredChannelByAffinity(seedContext, modelName, "default")
	service.RecordChannelAffinity(seedContext, openCodeGo.Id)

	t.Cleanup(func() {
		service.ClearCurrentChannelAffinityCache(seedContext)
		*setting = previousSetting
		common.MemoryCacheEnabled = false
		model.DB = previousDB
		common.SetMainDatabaseType(previousDatabaseType)
		common.RedisEnabled = previousRedisEnabled
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
		if previousMemoryCacheEnabled {
			model.InitChannelCache()
		}
	})

	return distributorEndpointGuardFixture{
		modelName:      modelName,
		openCodeGoID:   openCodeGo.Id,
		fallbackID:     fallback.Id,
		affinityHeader: affinityHeader,
	}
}

func runDistributorEndpointRequest(t *testing.T, path string, modelName string, configure func(*gin.Context)) (int, int, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	selectedChannelID := 0
	nextCalled := false
	router := gin.New()
	router.POST(path,
		func(c *gin.Context) {
			common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
			common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
			if configure != nil {
				configure(c)
			}
			c.Next()
		},
		Distribute(),
		func(c *gin.Context) {
			nextCalled = true
			selectedChannelID = common.GetContextKeyInt(c, constant.ContextKeyChannelId)
			c.Status(http.StatusNoContent)
		},
	)
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"`+modelName+`"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response.Code, selectedChannelID, nextCalled
}

func TestDistributorRejectsUnsupportedSpecificOpenCodeGoChannel(t *testing.T) {
	fixture := setupDistributorEndpointGuardTest(t)

	status, selectedChannelID, nextCalled := runDistributorEndpointRequest(t, "/v1/moderations", fixture.modelName, func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, fmt.Sprintf("%d", fixture.openCodeGoID))
	})

	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Zero(t, selectedChannelID)
	assert.False(t, nextCalled)
}

func TestDistributorAllowsSupportedSpecificOpenCodeGoChannel(t *testing.T) {
	fixture := setupDistributorEndpointGuardTest(t)

	status, selectedChannelID, nextCalled := runDistributorEndpointRequest(t, "/v1/messages", fixture.modelName, func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, fmt.Sprintf("%d", fixture.openCodeGoID))
	})

	assert.Equal(t, http.StatusNoContent, status)
	assert.Equal(t, fixture.openCodeGoID, selectedChannelID)
	assert.True(t, nextCalled)
}

func TestDistributorFailsClosedWithoutOverwritingMalformedOpenCodeGoSettings(t *testing.T) {
	fixture := setupDistributorEndpointGuardTest(t)
	const malformedSettings = `{"opencode_go":{"identity_proxy_enabled":true}`
	require.NoError(t, model.DB.Model(&model.Channel{}).
		Where("id = ?", fixture.openCodeGoID).
		Update("settings", malformedSettings).Error)
	model.InitChannelCache()

	status, selectedChannelID, nextCalled := runDistributorEndpointRequest(t, "/v1/messages", fixture.modelName, func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, fmt.Sprintf("%d", fixture.openCodeGoID))
	})

	assert.Equal(t, http.StatusTooManyRequests, status)
	assert.Zero(t, selectedChannelID)
	assert.False(t, nextCalled)
	persisted, err := model.GetChannelById(fixture.openCodeGoID, true)
	require.NoError(t, err)
	assert.Equal(t, malformedSettings, persisted.OtherSettings)
}

func TestDistributorFailsClosedWithoutOverwritingMalformedOpenCodeGoTransportSettings(t *testing.T) {
	fixture := setupDistributorEndpointGuardTest(t)
	const malformedSettings = `{"proxy":"http://proxy.example:8080"`
	require.NoError(t, model.DB.Model(&model.Channel{}).
		Where("id = ?", fixture.openCodeGoID).
		Update("setting", malformedSettings).Error)
	model.InitChannelCache()

	status, selectedChannelID, nextCalled := runDistributorEndpointRequest(t, "/v1/messages", fixture.modelName, func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, fmt.Sprintf("%d", fixture.openCodeGoID))
	})

	assert.Equal(t, http.StatusTooManyRequests, status)
	assert.Zero(t, selectedChannelID)
	assert.False(t, nextCalled)
	persisted, err := model.GetChannelById(fixture.openCodeGoID, true)
	require.NoError(t, err)
	require.NotNil(t, persisted.Setting)
	assert.Equal(t, malformedSettings, *persisted.Setting)
}

func TestDistributorSkipsUnsupportedOpenCodeGoAffinityHit(t *testing.T) {
	fixture := setupDistributorEndpointGuardTest(t)

	status, selectedChannelID, nextCalled := runDistributorEndpointRequest(t, "/v1/moderations", fixture.modelName, func(c *gin.Context) {
		c.Request.Header.Set("X-Endpoint-Affinity", fixture.affinityHeader)
	})

	assert.Equal(t, http.StatusNoContent, status)
	assert.Equal(t, fixture.fallbackID, selectedChannelID)
	assert.True(t, nextCalled)
}

func TestSetupContextForSelectedChannelPlanningDefersAPIKeyPolling(t *testing.T) {
	channel := &model.Channel{
		Id:   6301,
		Type: constant.ChannelTypeOpenCodeAPIKey,
		Key:  "planning-key-a\nplanning-key-b",
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:           true,
			MultiKeySize:         2,
			MultiKeyMode:         constant.MultiKeyModePolling,
			MultiKeyPollingIndex: 1,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusEnabled,
				1: common.ChannelStatusEnabled,
			},
		},
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	relayErr := SetupContextForSelectedChannelPlanning(c, channel, "planning-model")

	require.Nil(t, relayErr)
	assert.Empty(t, common.GetContextKeyString(c, constant.ContextKeyChannelKey))
	assert.False(t, common.GetContextKeyBool(c, constant.ContextKeyChannelIsMultiKey))
	assert.Equal(t, 1, channel.ChannelInfo.MultiKeyPollingIndex)
	source, found := SelectedChannelPlanningSource(c)
	require.True(t, found)
	require.NotSame(t, channel, source)
	assert.Equal(t, channel.ChannelInfo, source.ChannelInfo)
}
