package router

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListModelsSupportsOpenAIAndGeminiAuthentication(t *testing.T) {
	setupRelayRouterTestDB(t)

	user := model.User{
		Username: "models-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    100,
	}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		UserId:         user.Id,
		Key:            "modelstestkey",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}).Error)

	engine := gin.New()
	SetRelayRouter(engine)

	tests := []struct {
		name           string
		path           string
		headerName     string
		expectedObject string
		expectedField  string
	}{
		{
			name:           "OpenAI bearer token",
			path:           "/v1/models",
			headerName:     "Authorization",
			expectedObject: "list",
			expectedField:  "data",
		},
		{
			name:          "Gemini API key header",
			path:          "/v1/models",
			headerName:    "x-goog-api-key",
			expectedField: "models",
		},
		{
			name:          "Gemini API key query",
			path:          "/v1/models?key=modelstestkey",
			expectedField: "models",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.headerName != "" {
				value := "modelstestkey"
				if test.headerName == "Authorization" {
					value = "Bearer " + value
				}
				request.Header.Set(test.headerName, value)
			}

			engine.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusOK, recorder.Code)
			var payload map[string]any
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
			assert.Contains(t, payload, test.expectedField)
			assert.NotContains(t, payload, "error")
			if test.expectedObject != "" {
				assert.Equal(t, test.expectedObject, payload["object"])
			}
		})
	}
}

func TestRelayRouterValidatesClientBodiesBeforeModelAuthorizationAndDistribution(t *testing.T) {
	setupRelayRouterTestDB(t)

	user := model.User{
		Username: "relay-validation-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    100,
	}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		UserId:             user.Id,
		Key:                "relayvalidationkey",
		Status:             common.TokenStatusEnabled,
		ExpiredTime:        -1,
		UnlimitedQuota:     true,
		ModelLimitsEnabled: true,
		ModelLimits:        "allowed-model",
	}).Error)

	engine := gin.New()
	engine.Use(middleware.RequestId())
	SetRelayRouter(engine)

	tests := []struct {
		name       string
		path       string
		body       string
		wantDetail string
	}{
		{
			name:       "messages missing model is 400 instead of token 403",
			path:       "/v1/messages",
			body:       `{"messages":[{"role":"user","content":"hello"}]}`,
			wantDetail: "model is required",
		},
		{
			name:       "messages missing role is 400 instead of no-channel 503",
			path:       "/v1/messages",
			body:       `{"model":"allowed-model","messages":[{"content":"hello"}]}`,
			wantDetail: "messages[0].role",
		},
		{
			name:       "chat missing content is 400 instead of no-channel 503",
			path:       "/v1/chat/completions",
			body:       `{"model":"allowed-model","messages":[{"role":"user"}]}`,
			wantDetail: "messages[0].content",
		},
		{
			name:       "responses unknown type is 400 instead of no-channel 503",
			path:       "/v1/responses",
			body:       `{"model":"allowed-model","input":[{"type":"future_private_item"}]}`,
			wantDetail: "input[0].type",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, test.path, bytes.NewBufferString(test.body))
			request.Header.Set("Authorization", "Bearer relayvalidationkey")
			request.Header.Set("Content-Type", "application/json")

			engine.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
			require.Contains(t, recorder.Body.String(), "invalid_request_error")
			require.Contains(t, recorder.Body.String(), test.wantDetail)
			require.NotContains(t, recorder.Body.String(), "rate_limit_error")
			require.NotContains(t, recorder.Body.String(), "model_not_found")
		})
	}

	var channelCount int64
	require.NoError(t, model.DB.Model(&model.Channel{}).Count(&channelCount).Error)
	require.Zero(t, channelCount)
}

func TestRelayRouterConcurrentClientErrorsHaveNoDistributionSideEffects(t *testing.T) {
	setupRelayRouterTestDB(t)

	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	user := model.User{
		Username: "relay-load-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    1000,
	}
	require.NoError(t, model.DB.Create(&user).Error)
	token := model.Token{
		UserId:         user.Id,
		Key:            "relayloadkey",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}
	require.NoError(t, model.DB.Create(&token).Error)
	baseURL := upstream.URL
	channel := model.Channel{
		Type:    constant.ChannelTypeOpenAI,
		Key:     "mock-upstream-key",
		Status:  common.ChannelStatusEnabled,
		Name:    "relay-load-mock",
		BaseURL: &baseURL,
		Models:  "allowed-model",
		Group:   "default",
	}
	require.NoError(t, model.DB.Create(&channel).Error)
	ability := model.Ability{
		Group:     "default",
		Model:     "allowed-model",
		ChannelId: channel.Id,
		Enabled:   true,
	}
	require.NoError(t, model.DB.Create(&ability).Error)

	engine := gin.New()
	engine.Use(middleware.RequestId())
	SetRelayRouter(engine)
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)

	client := &http.Client{Timeout: 10 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	invalidRequests := []struct {
		path string
		body string
	}{
		{path: "/v1/messages", body: `{"model":"allowed-model","messages":[{"content":"hello"}]}`},
		{path: "/v1/chat/completions", body: `{"model":"allowed-model","messages":[{"role":"user"}]}`},
		{path: "/v1/responses", body: `{"model":"allowed-model","input":[{"type":"unknown-production-item"}]}`},
	}

	type requestResult struct {
		status int
		body   string
		err    error
	}
	for _, concurrency := range []int{5, 16, 32, 64, 96, 128} {
		t.Run(fmt.Sprintf("concurrency_%d", concurrency), func(t *testing.T) {
			start := make(chan struct{})
			results := make(chan requestResult, concurrency)
			var waitGroup sync.WaitGroup
			waitGroup.Add(concurrency)
			for index := 0; index < concurrency; index++ {
				fixture := invalidRequests[index%len(invalidRequests)]
				go func() {
					defer waitGroup.Done()
					<-start
					request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+fixture.path, bytes.NewBufferString(fixture.body))
					if err != nil {
						results <- requestResult{err: err}
						return
					}
					request.Header.Set("Authorization", "Bearer relayloadkey")
					request.Header.Set("Content-Type", "application/json")
					response, err := client.Do(request)
					if err != nil {
						results <- requestResult{err: err}
						return
					}
					body, readErr := io.ReadAll(response.Body)
					closeErr := response.Body.Close()
					if readErr != nil {
						err = readErr
					} else if closeErr != nil {
						err = closeErr
					}
					results <- requestResult{status: response.StatusCode, body: string(body), err: err}
				}()
			}
			close(start)
			waitGroup.Wait()
			close(results)

			for result := range results {
				require.NoError(t, result.err)
				require.Equal(t, http.StatusBadRequest, result.status, result.body)
				require.Contains(t, result.body, "invalid_request_error")
				require.NotContains(t, result.body, "rate_limit_error")
				require.NotContains(t, result.body, "model_not_found")
			}
			require.Zero(t, upstreamCalls.Load())
		})
	}

	var currentUser model.User
	require.NoError(t, model.DB.First(&currentUser, user.Id).Error)
	require.Equal(t, user.Quota, currentUser.Quota)
	require.Equal(t, user.UsedQuota, currentUser.UsedQuota)
	require.Equal(t, user.RequestCount, currentUser.RequestCount)
	require.Equal(t, user.Status, currentUser.Status)

	var currentToken model.Token
	require.NoError(t, model.DB.First(&currentToken, token.Id).Error)
	require.Equal(t, token.RemainQuota, currentToken.RemainQuota)
	require.Equal(t, token.UsedQuota, currentToken.UsedQuota)
	require.Equal(t, token.Status, currentToken.Status)

	var currentChannel model.Channel
	require.NoError(t, model.DB.First(&currentChannel, channel.Id).Error)
	require.Equal(t, channel.UsedQuota, currentChannel.UsedQuota)
	require.Equal(t, channel.Status, currentChannel.Status)

	var currentAbility model.Ability
	require.NoError(t, model.DB.First(&currentAbility, "`group` = ? AND model = ? AND channel_id = ?", ability.Group, ability.Model, ability.ChannelId).Error)
	require.True(t, currentAbility.Enabled)

	var errorLogCount int64
	require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("type = ?", model.LogTypeError).Count(&errorLogCount).Error)
	require.Zero(t, errorLogCount)
	require.Zero(t, upstreamCalls.Load())
}

func setupRelayRouterTestDB(t *testing.T) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	originalIsMasterNode := common.IsMasterNode
	originalRedisEnabled := common.RedisEnabled
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalSQLitePath := common.SQLitePath
	originalMainDatabaseType := common.MainDatabaseType()
	originalLogDatabaseType := common.LogDatabaseType()
	originalSQLDSN, hadSQLDSN := os.LookupEnv("SQL_DSN")

	common.IsMasterNode = false
	common.RedisEnabled = false
	common.MemoryCacheEnabled = false
	common.SQLitePath = fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	require.NoError(t, os.Setenv("SQL_DSN", "local"))
	require.NoError(t, model.InitDB())
	model.LOG_DB = model.DB
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Token{}, &model.Ability{}, &model.Channel{}, &model.Log{}))

	t.Cleanup(func() {
		if sqlDB, err := model.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
		common.IsMasterNode = originalIsMasterNode
		common.RedisEnabled = originalRedisEnabled
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		common.SQLitePath = originalSQLitePath
		common.SetDatabaseTypes(originalMainDatabaseType, originalLogDatabaseType)
		if hadSQLDSN {
			require.NoError(t, os.Setenv("SQL_DSN", originalSQLDSN))
		} else {
			require.NoError(t, os.Unsetenv("SQL_DSN"))
		}
	})
}
