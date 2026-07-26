package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type trackingWriter struct {
	httptest.ResponseRecorder
	wroteHeader bool
	statusCodes []int
}

func newTrackingWriter() *trackingWriter {
	return &trackingWriter{ResponseRecorder: *httptest.NewRecorder()}
}

func (w *trackingWriter) WriteHeader(code int) {
	w.wroteHeader = true
	w.statusCodes = append(w.statusCodes, code)
	w.ResponseRecorder.WriteHeader(code)
}

func (w *trackingWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseRecorder.Write(p)
}

type videoProxyReadCloser struct {
	io.Reader
	closeCalls int
}

func (r *videoProxyReadCloser) Close() error {
	r.closeCalls++
	return nil
}

// videoProxyFetcherFixture embeds the ordinary task contract and supplies only
// the optional content-fetch operation exercised by the controller.
type videoProxyFetcherFixture struct {
	channel.TaskAdaptor
	fetch func(
		context.Context,
		string,
		string,
		string,
		string,
	) (*channel.VideoContent, error)
}

type videoProxyTaskAdaptorLookupFunc func(constant.TaskPlatform) channel.TaskAdaptor

func (f videoProxyTaskAdaptorLookupFunc) TaskAdaptor(platform constant.TaskPlatform) channel.TaskAdaptor {
	return f(platform)
}

func (f *videoProxyFetcherFixture) FetchVideoContent(
	ctx context.Context,
	baseURL string,
	key string,
	upstreamTaskID string,
	proxy string,
) (*channel.VideoContent, error) {
	return f.fetch(ctx, baseURL, key, upstreamTaskID, proxy)
}

func setupVideoProxyDB(t *testing.T) *gorm.DB {
	t.Helper()

	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousMainDatabaseType := common.MainDatabaseType()
	previousLogDatabaseType := common.LogDatabaseType()
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	previousRedisEnabled := common.RedisEnabled

	db, err := gorm.Open(sqlite.Open(fmt.Sprintf(
		"file:video-proxy-%d?mode=memory&cache=shared",
		time.Now().UnixNano(),
	)), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	model.DB = db
	model.LOG_DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.MemoryCacheEnabled = false
	common.RedisEnabled = false
	require.NoError(t, db.AutoMigrate(
		&model.Task{},
		&model.Channel{},
		&model.User{},
		&model.Token{},
	))

	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.SetDatabaseTypes(previousMainDatabaseType, previousLogDatabaseType)
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
		common.RedisEnabled = previousRedisEnabled
		require.NoError(t, sqlDB.Close())
	})
	return db
}

func createVideoProxyTask(
	t *testing.T,
	db *gorm.DB,
	publicTaskID string,
	userID int,
	channelID int,
	platform constant.TaskPlatform,
	status model.TaskStatus,
	storedKey string,
	upstreamTaskID string,
	resultURL string,
) *model.Task {
	t.Helper()
	task := &model.Task{
		TaskID:    publicTaskID,
		UserId:    userID,
		ChannelId: channelID,
		Platform:  platform,
		Status:    status,
		PrivateData: model.TaskPrivateData{
			Key:            storedKey,
			UpstreamTaskID: upstreamTaskID,
			ResultURL:      resultURL,
		},
	}
	require.NoError(t, db.Create(task).Error)
	return task
}

func createVideoProxyChannel(
	t *testing.T,
	db *gorm.DB,
	channelID int,
	channelType int,
	key string,
	baseURL string,
	proxy string,
) *model.Channel {
	t.Helper()
	settingBytes, err := common.Marshal(dto.ChannelSettings{Proxy: proxy})
	require.NoError(t, err)
	channel := &model.Channel{
		Id:      channelID,
		Type:    channelType,
		Key:     key,
		BaseURL: common.GetPointer(baseURL),
		Setting: common.GetPointer(string(settingBytes)),
	}
	require.NoError(t, db.Create(channel).Error)
	return channel
}

func newVideoProxyContext(
	writer http.ResponseWriter,
	path string,
	taskID string,
	userID int,
) *gin.Context {
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodGet, path, nil)
	ctx.Params = gin.Params{{Key: "task_id", Value: taskID}}
	ctx.Set("id", userID)
	return ctx
}

type openAIVideoErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func decodeOpenAIVideoError(t *testing.T, body []byte) openAIVideoErrorResponse {
	t.Helper()
	var response openAIVideoErrorResponse
	require.NoError(t, common.Unmarshal(body, &response))
	return response
}

func captureVideoProxyErrorLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	buffer := &bytes.Buffer{}
	common.LogWriterMu.Lock()
	previous := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = buffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = previous
		common.LogWriterMu.Unlock()
	})
	return buffer
}

func TestVideoProxyMissingTaskUsesNestedNotFound(t *testing.T) {
	setupVideoProxyDB(t)
	writer := newTrackingWriter()
	ctx := newVideoProxyContext(writer, "/v1/videos/task_missing/content", "task_missing", 41)

	VideoProxy(ctx)

	require.Equal(t, http.StatusNotFound, writer.Code)
	response := decodeOpenAIVideoError(t, writer.Body.Bytes())
	assert.Equal(t, "invalid_request_error", response.Error.Type)
	assert.Equal(t, "task_not_found", response.Error.Code)
	assert.NotEmpty(t, response.Error.Message)
}

func TestVideoProxyOtherUsersTaskUsesNestedNotFound(t *testing.T) {
	db := setupVideoProxyDB(t)
	createVideoProxyTask(
		t, db, "task_other_user", 42, 501, constant.TaskPlatform("59"),
		model.TaskStatusSuccess, "STORED_KEY", "UPSTREAM_PRIVATE", "",
	)
	writer := newTrackingWriter()
	ctx := newVideoProxyContext(writer, "/v1/videos/task_other_user/content", "task_other_user", 41)

	VideoProxy(ctx)

	require.Equal(t, http.StatusNotFound, writer.Code)
	response := decodeOpenAIVideoError(t, writer.Body.Bytes())
	assert.Equal(t, "task_not_found", response.Error.Code)
	assert.NotContains(t, writer.Body.String(), "UPSTREAM_PRIVATE")
}

func TestVideoProxyContentStateErrorsUseOpenAICodes(t *testing.T) {
	tests := []struct {
		name     string
		status   model.TaskStatus
		wantCode string
	}{
		{name: "submitted", status: model.TaskStatusSubmitted, wantCode: "task_not_completed"},
		{name: "in progress", status: model.TaskStatusInProgress, wantCode: "task_not_completed"},
		{name: "failure", status: model.TaskStatusFailure, wantCode: "task_failed"},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := setupVideoProxyDB(t)
			taskID := "task_state_" + strconv.Itoa(index)
			createVideoProxyTask(
				t, db, taskID, 41, 501, constant.TaskPlatform("59"), test.status,
				"STORED_KEY", "UPSTREAM_PRIVATE", "",
			)
			writer := newTrackingWriter()
			ctx := newVideoProxyContext(writer, "/v1/videos/"+taskID+"/content", taskID, 41)

			VideoProxy(ctx)

			require.Equal(t, http.StatusBadRequest, writer.Code)
			response := decodeOpenAIVideoError(t, writer.Body.Bytes())
			assert.Equal(t, "invalid_request_error", response.Error.Type)
			assert.Equal(t, test.wantCode, response.Error.Code)
		})
	}
}

func TestVideoProxySeedDanceContentUsesPersistedTaskData(t *testing.T) {
	db := setupVideoProxyDB(t)
	const (
		publicTaskID   = "task_public"
		storedKey      = "STORED_KEY"
		upstreamTaskID = "UPSTREAM_PRIVATE"
		baseURL        = "https://seedance.fixture"
		proxyURL       = "http://proxy.fixture:8080"
	)
	createVideoProxyChannel(
		t, db, 501, constant.ChannelTypeOpenAI, storedKey, baseURL, proxyURL,
	)
	createVideoProxyTask(
		t, db, publicTaskID, 41, 501, constant.TaskPlatform("59"),
		model.TaskStatusSuccess, storedKey, upstreamTaskID, "",
	)

	var gotPlatform constant.TaskPlatform
	var gotBaseURL, gotKey, gotUpstreamTaskID, gotProxy string
	body := &videoProxyReadCloser{Reader: strings.NewReader("MP4_BYTES")}
	fetcher := &videoProxyFetcherFixture{
		fetch: func(
			_ context.Context,
			fetchBaseURL string,
			fetchKey string,
			fetchUpstreamTaskID string,
			fetchProxy string,
		) (*channel.VideoContent, error) {
			gotBaseURL = fetchBaseURL
			gotKey = fetchKey
			gotUpstreamTaskID = fetchUpstreamTaskID
			gotProxy = fetchProxy
			return &channel.VideoContent{
				ContentType:   "application/provider-video",
				ContentLength: int64(len("MP4_BYTES")),
				Body:          body,
			}, nil
		},
	}
	writer := newTrackingWriter()
	ctx := newVideoProxyContext(writer, "/v1/videos/"+publicTaskID+"/content", publicTaskID, 41)
	ctx.Set(videoProxyTaskAdaptorLookupContextKey, videoProxyTaskAdaptorLookupFunc(
		func(platform constant.TaskPlatform) channel.TaskAdaptor {
			gotPlatform = platform
			return fetcher
		},
	))

	VideoProxy(ctx)

	require.Equal(t, http.StatusOK, writer.Code)
	assert.Equal(t, constant.TaskPlatform("59"), gotPlatform)
	assert.Equal(t, baseURL, gotBaseURL)
	assert.Equal(t, storedKey, gotKey)
	assert.Equal(t, upstreamTaskID, gotUpstreamTaskID)
	assert.Equal(t, proxyURL, gotProxy)
	assert.Equal(t, 1, body.closeCalls)
	assert.Equal(t, "MP4_BYTES", writer.Body.String())
	assert.Equal(t, "video/mp4", writer.Header().Get("Content-Type"))
	assert.Equal(t, `inline; filename="task_public.mp4"`, writer.Header().Get("Content-Disposition"))
	assert.Equal(t, "private, max-age=3600", writer.Header().Get("Cache-Control"))
	assert.Equal(t, "nosniff", writer.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, strconv.Itoa(len("MP4_BYTES")), writer.Header().Get("Content-Length"))
}

func TestVideoProxyStructuredFetchErrorsKeepTheirContractBeforeAny200(t *testing.T) {
	tests := []struct {
		name       string
		fetchError *channel.VideoContentError
	}{
		{
			name: "rate limited",
			fetchError: &channel.VideoContentError{
				StatusCode: http.StatusTooManyRequests,
				Type:       "upstream_rate_limit_error",
				Code:       "upstream_rate_limit_error",
				Message:    "upstream rate limit exceeded",
				Cause:      errors.New("PROVIDER_PRIVATE_DETAIL"),
			},
		},
		{
			name: "invalid upstream response",
			fetchError: &channel.VideoContentError{
				StatusCode: http.StatusBadGateway,
				Type:       "invalid_upstream_response",
				Code:       "invalid_upstream_response",
				Message:    "upstream returned invalid video content",
				Cause:      errors.New("PROVIDER_PRIVATE_DETAIL"),
			},
		},
		{
			name: "timeout",
			fetchError: &channel.VideoContentError{
				StatusCode: http.StatusGatewayTimeout,
				Type:       "upstream_timeout_error",
				Code:       "upstream_timeout_error",
				Message:    "upstream request timed out",
				Cause:      errors.New("PROVIDER_PRIVATE_DETAIL"),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := setupVideoProxyDB(t)
			createVideoProxyChannel(t, db, 501, constant.ChannelTypeSeedDance, "STORED_KEY", "https://seedance.fixture", "")
			createVideoProxyTask(
				t, db, "task_fetch_error", 41, 501, constant.TaskPlatform("59"),
				model.TaskStatusSuccess, "STORED_KEY", "UPSTREAM_PRIVATE", "",
			)
			fetcher := &videoProxyFetcherFixture{
				fetch: func(context.Context, string, string, string, string) (*channel.VideoContent, error) {
					return nil, test.fetchError
				},
			}
			writer := newTrackingWriter()
			ctx := newVideoProxyContext(writer, "/v1/videos/task_fetch_error/content", "task_fetch_error", 41)
			ctx.Set(videoProxyTaskAdaptorLookupContextKey, videoProxyTaskAdaptorLookupFunc(
				func(constant.TaskPlatform) channel.TaskAdaptor { return fetcher },
			))

			VideoProxy(ctx)

			require.Equal(t, test.fetchError.StatusCode, writer.Code)
			require.Equal(t, []int{test.fetchError.StatusCode}, writer.statusCodes)
			response := decodeOpenAIVideoError(t, writer.Body.Bytes())
			assert.Equal(t, test.fetchError.Type, response.Error.Type)
			assert.Equal(t, test.fetchError.Code, response.Error.Code)
			assert.Equal(t, test.fetchError.Message, response.Error.Message)
			assert.NotContains(t, writer.Body.String(), "PROVIDER_PRIVATE_DETAIL")
		})
	}
}

func TestVideoProxyPlainFetchErrorIsSanitizedBeforeAny200(t *testing.T) {
	db := setupVideoProxyDB(t)
	createVideoProxyChannel(t, db, 501, constant.ChannelTypeSeedDance, "STORED_KEY", "https://seedance.fixture", "")
	createVideoProxyTask(
		t, db, "task_plain_error", 41, 501, constant.TaskPlatform("59"),
		model.TaskStatusSuccess, "STORED_KEY", "UPSTREAM_PRIVATE", "",
	)
	fetcher := &videoProxyFetcherFixture{
		fetch: func(context.Context, string, string, string, string) (*channel.VideoContent, error) {
			return nil, errors.New("PROVIDER_PRIVATE_DETAIL")
		},
	}
	writer := newTrackingWriter()
	ctx := newVideoProxyContext(writer, "/v1/videos/task_plain_error/content", "task_plain_error", 41)
	ctx.Set(videoProxyTaskAdaptorLookupContextKey, videoProxyTaskAdaptorLookupFunc(
		func(constant.TaskPlatform) channel.TaskAdaptor { return fetcher },
	))

	VideoProxy(ctx)

	require.Equal(t, http.StatusBadGateway, writer.Code)
	require.Equal(t, []int{http.StatusBadGateway}, writer.statusCodes)
	response := decodeOpenAIVideoError(t, writer.Body.Bytes())
	assert.Equal(t, "upstream_error", response.Error.Type)
	assert.Equal(t, "upstream_connection_error", response.Error.Code)
	assert.Equal(t, "failed to fetch video content", response.Error.Message)
	assert.NotContains(t, writer.Body.String(), "PROVIDER_PRIVATE_DETAIL")
	assert.NotContains(t, writer.Body.String(), "STORED_KEY")
	assert.NotContains(t, writer.Body.String(), "UPSTREAM_PRIVATE")
}

func TestVideoProxyFetchErrorsDoNotLogStoredCredentialsOrUpstreamTaskIDs(t *testing.T) {
	tests := []struct {
		name       string
		fetchError error
	}{
		{
			name: "structured error",
			fetchError: &channel.VideoContentError{
				StatusCode: http.StatusBadGateway,
				Type:       "upstream_error",
				Code:       "upstream_connection_error",
				Message:    "failed to fetch video content",
				Cause: errors.New(
					`Get "https://seedance.fixture/video/UPSTREAM_PRIVATE?key=STORED_KEY": connection reset`,
				),
			},
		},
		{
			name: "plain error",
			fetchError: errors.New(
				`Get "https://seedance.fixture/video/UPSTREAM_PRIVATE?key=STORED_KEY": connection reset`,
			),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := setupVideoProxyDB(t)
			createVideoProxyChannel(
				t, db, 501, constant.ChannelTypeSeedDance,
				"STORED_KEY", "https://seedance.fixture", "",
			)
			createVideoProxyTask(
				t, db, "task_private_log", 41, 501, constant.TaskPlatform("59"),
				model.TaskStatusSuccess, "STORED_KEY", "UPSTREAM_PRIVATE", "",
			)
			fetcher := &videoProxyFetcherFixture{
				fetch: func(context.Context, string, string, string, string) (*channel.VideoContent, error) {
					return nil, test.fetchError
				},
			}
			logs := captureVideoProxyErrorLogs(t)
			writer := newTrackingWriter()
			ctx := newVideoProxyContext(
				writer,
				"/v1/videos/task_private_log/content",
				"task_private_log",
				41,
			)
			ctx.Set(videoProxyTaskAdaptorLookupContextKey, videoProxyTaskAdaptorLookupFunc(
				func(constant.TaskPlatform) channel.TaskAdaptor { return fetcher },
			))

			VideoProxy(ctx)

			require.Equal(t, http.StatusBadGateway, writer.Code)
			assert.NotContains(t, logs.String(), "STORED_KEY")
			assert.NotContains(t, logs.String(), "UPSTREAM_PRIVATE")
			assert.NotContains(t, writer.Body.String(), "STORED_KEY")
			assert.NotContains(t, writer.Body.String(), "UPSTREAM_PRIVATE")
		})
	}
}

func TestVideoProxyMalformedFetcherResultsFailClosedBeforeAny200(t *testing.T) {
	tests := []struct {
		name        string
		fetchResult func(*videoProxyReadCloser) (*channel.VideoContent, error)
	}{
		{
			name: "zero structured status",
			fetchResult: func(body *videoProxyReadCloser) (*channel.VideoContent, error) {
				return &channel.VideoContent{Body: body}, &channel.VideoContentError{
					Type:    "upstream_error",
					Code:    "upstream_connection_error",
					Message: "failed to fetch video content",
				}
			},
		},
		{
			name: "non-error structured status",
			fetchResult: func(body *videoProxyReadCloser) (*channel.VideoContent, error) {
				return &channel.VideoContent{Body: body}, &channel.VideoContentError{
					StatusCode: http.StatusOK,
					Type:       "upstream_error",
					Code:       "upstream_connection_error",
					Message:    "failed to fetch video content",
				}
			},
		},
		{
			name: "blank structured type",
			fetchResult: func(body *videoProxyReadCloser) (*channel.VideoContent, error) {
				return &channel.VideoContent{Body: body}, &channel.VideoContentError{
					StatusCode: http.StatusBadGateway,
					Code:       "upstream_connection_error",
					Message:    "failed to fetch video content",
				}
			},
		},
		{
			name: "blank structured code",
			fetchResult: func(body *videoProxyReadCloser) (*channel.VideoContent, error) {
				return &channel.VideoContent{Body: body}, &channel.VideoContentError{
					StatusCode: http.StatusBadGateway,
					Type:       "upstream_error",
					Message:    "failed to fetch video content",
				}
			},
		},
		{
			name: "blank structured message",
			fetchResult: func(body *videoProxyReadCloser) (*channel.VideoContent, error) {
				return &channel.VideoContent{Body: body}, &channel.VideoContentError{
					StatusCode: http.StatusBadGateway,
					Type:       "upstream_error",
					Code:       "upstream_connection_error",
				}
			},
		},
		{
			name: "negative content length",
			fetchResult: func(body *videoProxyReadCloser) (*channel.VideoContent, error) {
				return &channel.VideoContent{
					ContentLength: -1,
					Body:          body,
				}, nil
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := setupVideoProxyDB(t)
			createVideoProxyChannel(
				t, db, 501, constant.ChannelTypeSeedDance,
				"STORED_KEY", "https://seedance.fixture", "",
			)
			createVideoProxyTask(
				t, db, "task_malformed_fetch", 41, 501, constant.TaskPlatform("59"),
				model.TaskStatusSuccess, "STORED_KEY", "UPSTREAM_PRIVATE", "",
			)
			body := &videoProxyReadCloser{Reader: strings.NewReader("unused")}
			fetcher := &videoProxyFetcherFixture{
				fetch: func(context.Context, string, string, string, string) (*channel.VideoContent, error) {
					return test.fetchResult(body)
				},
			}
			writer := newTrackingWriter()
			ctx := newVideoProxyContext(
				writer,
				"/v1/videos/task_malformed_fetch/content",
				"task_malformed_fetch",
				41,
			)
			ctx.Set(videoProxyTaskAdaptorLookupContextKey, videoProxyTaskAdaptorLookupFunc(
				func(constant.TaskPlatform) channel.TaskAdaptor { return fetcher },
			))

			VideoProxy(ctx)

			require.Equal(t, http.StatusBadGateway, writer.Code)
			require.Equal(t, []int{http.StatusBadGateway}, writer.statusCodes)
			response := decodeOpenAIVideoError(t, writer.Body.Bytes())
			assert.Equal(t, "upstream_error", response.Error.Type)
			assert.Equal(t, "upstream_connection_error", response.Error.Code)
			assert.Equal(t, "failed to fetch video content", response.Error.Message)
			assert.Equal(t, 1, body.closeCalls)
		})
	}
}

func TestVideoProxyFetchErrorClosesReturnedBody(t *testing.T) {
	db := setupVideoProxyDB(t)
	createVideoProxyChannel(t, db, 501, constant.ChannelTypeSeedDance, "STORED_KEY", "https://seedance.fixture", "")
	createVideoProxyTask(
		t, db, "task_error_body", 41, 501, constant.TaskPlatform("59"),
		model.TaskStatusSuccess, "STORED_KEY", "UPSTREAM_PRIVATE", "",
	)
	body := &videoProxyReadCloser{Reader: strings.NewReader("unused")}
	fetcher := &videoProxyFetcherFixture{
		fetch: func(context.Context, string, string, string, string) (*channel.VideoContent, error) {
			return &channel.VideoContent{Body: body}, &channel.VideoContentError{
				StatusCode: http.StatusBadGateway,
				Type:       "invalid_upstream_response",
				Code:       "invalid_upstream_response",
				Message:    "upstream returned invalid video content",
			}
		},
	}
	writer := newTrackingWriter()
	ctx := newVideoProxyContext(writer, "/v1/videos/task_error_body/content", "task_error_body", 41)
	ctx.Set(videoProxyTaskAdaptorLookupContextKey, videoProxyTaskAdaptorLookupFunc(
		func(constant.TaskPlatform) channel.TaskAdaptor { return fetcher },
	))

	VideoProxy(ctx)

	require.Equal(t, http.StatusBadGateway, writer.Code)
	assert.Equal(t, 1, body.closeCalls)
}

func TestVideoProxyChannelAndStoredCredentialErrorsAreNested(t *testing.T) {
	t.Run("channel unavailable", func(t *testing.T) {
		db := setupVideoProxyDB(t)
		createVideoProxyTask(
			t, db, "task_missing_channel", 41, 501, constant.TaskPlatform("59"),
			model.TaskStatusSuccess, "STORED_KEY", "UPSTREAM_PRIVATE", "",
		)
		writer := newTrackingWriter()
		ctx := newVideoProxyContext(writer, "/v1/videos/task_missing_channel/content", "task_missing_channel", 41)
		ctx.Set(videoProxyTaskAdaptorLookupContextKey, videoProxyTaskAdaptorLookupFunc(
			func(constant.TaskPlatform) channel.TaskAdaptor { return &videoProxyFetcherFixture{} },
		))

		VideoProxy(ctx)

		require.Equal(t, http.StatusBadGateway, writer.Code)
		response := decodeOpenAIVideoError(t, writer.Body.Bytes())
		assert.Equal(t, "upstream_error", response.Error.Type)
		assert.Equal(t, "channel_unavailable", response.Error.Code)
		assert.Equal(t, "video channel is unavailable", response.Error.Message)
	})

	t.Run("stored credential unavailable", func(t *testing.T) {
		db := setupVideoProxyDB(t)
		createVideoProxyChannel(t, db, 501, constant.ChannelTypeSeedDance, "CURRENT_KEY", "https://seedance.fixture", "")
		createVideoProxyTask(
			t, db, "task_missing_credential", 41, 501, constant.TaskPlatform("59"),
			model.TaskStatusSuccess, "STORED_KEY", "UPSTREAM_PRIVATE", "",
		)
		writer := newTrackingWriter()
		ctx := newVideoProxyContext(writer, "/v1/videos/task_missing_credential/content", "task_missing_credential", 41)
		ctx.Set(videoProxyTaskAdaptorLookupContextKey, videoProxyTaskAdaptorLookupFunc(
			func(constant.TaskPlatform) channel.TaskAdaptor { return &videoProxyFetcherFixture{} },
		))

		VideoProxy(ctx)

		require.Equal(t, http.StatusBadGateway, writer.Code)
		response := decodeOpenAIVideoError(t, writer.Body.Bytes())
		assert.Equal(t, "upstream_error", response.Error.Type)
		assert.Equal(t, "stored_credential_unavailable", response.Error.Code)
		assert.Equal(t, "video channel credential is unavailable", response.Error.Message)
		assert.NotContains(t, writer.Body.String(), "STORED_KEY")
	})
}

func TestOpenAIVideoNotFoundIsNestedButLegacyVideoGenerationStaysFlat(t *testing.T) {
	db := setupVideoProxyDB(t)

	t.Run("new status route", func(t *testing.T) {
		writer := newTrackingWriter()
		ctx, _ := gin.CreateTestContext(writer)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/task_missing", nil)
		ctx.Params = gin.Params{{Key: "task_id", Value: "task_missing"}}
		ctx.Set("id", 41)
		ctx.Set("relay_mode", relayconstant.RelayModeVideoFetchByID)

		RelayTaskFetch(ctx)

		require.Equal(t, http.StatusNotFound, writer.Code)
		response := decodeOpenAIVideoError(t, writer.Body.Bytes())
		assert.Equal(t, "invalid_request_error", response.Error.Type)
		assert.Equal(t, "task_not_found", response.Error.Code)
	})

	t.Run("other user on new status route", func(t *testing.T) {
		createVideoProxyTask(
			t, db, "task_status_other_user", 42, 501, constant.TaskPlatform("59"),
			model.TaskStatusSuccess, "STORED_KEY", "UPSTREAM_PRIVATE", "",
		)
		writer := newTrackingWriter()
		ctx, _ := gin.CreateTestContext(writer)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/task_status_other_user", nil)
		ctx.Params = gin.Params{{Key: "task_id", Value: "task_status_other_user"}}
		ctx.Set("id", 41)
		ctx.Set("relay_mode", relayconstant.RelayModeVideoFetchByID)

		RelayTaskFetch(ctx)

		require.Equal(t, http.StatusNotFound, writer.Code)
		response := decodeOpenAIVideoError(t, writer.Body.Bytes())
		assert.Equal(t, "task_not_found", response.Error.Code)
	})

	t.Run("legacy route", func(t *testing.T) {
		writer := newTrackingWriter()
		ctx, _ := gin.CreateTestContext(writer)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/video/generations/task_missing", nil)
		ctx.Params = gin.Params{{Key: "task_id", Value: "task_missing"}}
		ctx.Set("id", 41)
		ctx.Set("relay_mode", relayconstant.RelayModeVideoFetchByID)

		RelayTaskFetch(ctx)

		require.Equal(t, http.StatusBadRequest, writer.Code)
		var response struct {
			Code string `json:"code"`
		}
		require.NoError(t, common.Unmarshal(writer.Body.Bytes(), &response))
		assert.Equal(t, "task_not_exist", response.Code)
	})
}

func TestOpenAIVideoErrorSelectionExcludesRemix(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantNested bool
	}{
		{name: "create", path: "/v1/videos", wantNested: true},
		{name: "status", path: "/v1/videos/task_public", wantNested: true},
		{name: "content", path: "/v1/videos/task_public/content", wantNested: true},
		{name: "remix", path: "/v1/videos/task_public/remix", wantNested: false},
		{name: "legacy", path: "/v1/video/generations/task_public", wantNested: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writer := newTrackingWriter()
			ctx, _ := gin.CreateTestContext(writer)
			ctx.Request = httptest.NewRequest(http.MethodGet, test.path, nil)

			respondTaskErrorForRequest(ctx, &dto.TaskError{
				Code:       "fixture_error",
				Message:    "fixture message",
				StatusCode: http.StatusBadRequest,
			})

			if test.wantNested {
				response := decodeOpenAIVideoError(t, writer.Body.Bytes())
				assert.Equal(t, "fixture_error", response.Error.Code)
				return
			}
			var response struct {
				Code string `json:"code"`
			}
			require.NoError(t, common.Unmarshal(writer.Body.Bytes(), &response))
			assert.Equal(t, "fixture_error", response.Code)
		})
	}
}

func TestLegacyVideoDataURLHeadersRemainUnchanged(t *testing.T) {
	writer := newTrackingWriter()
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/task_legacy/content", nil)

	err := writeVideoDataURL(ctx, "data:video/webm;base64,"+"bGVnYWN5")

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, writer.Code)
	assert.Equal(t, "video/webm", writer.Header().Get("Content-Type"))
	assert.Equal(t, "public, max-age=86400", writer.Header().Get("Cache-Control"))
	assert.Equal(t, "legacy", writer.Body.String())
}

func TestVideoProxySuccessDoesNotEmitJSONAfterCopyFailure(t *testing.T) {
	db := setupVideoProxyDB(t)
	createVideoProxyChannel(t, db, 501, constant.ChannelTypeSeedDance, "STORED_KEY", "https://seedance.fixture", "")
	createVideoProxyTask(
		t, db, "task_copy_failure", 41, 501, constant.TaskPlatform("59"),
		model.TaskStatusSuccess, "STORED_KEY", "UPSTREAM_PRIVATE", "",
	)
	copyFailure := errors.New("copy failed")
	fetcher := &videoProxyFetcherFixture{
		fetch: func(context.Context, string, string, string, string) (*channel.VideoContent, error) {
			return &channel.VideoContent{
				ContentLength: 1,
				Body: &videoProxyReadCloser{Reader: &errorAfterReader{
					data: []byte("x"),
					err:  copyFailure,
				}},
			}, nil
		},
	}
	writer := newTrackingWriter()
	ctx := newVideoProxyContext(writer, "/v1/videos/task_copy_failure/content", "task_copy_failure", 41)
	ctx.Set(videoProxyTaskAdaptorLookupContextKey, videoProxyTaskAdaptorLookupFunc(
		func(constant.TaskPlatform) channel.TaskAdaptor { return fetcher },
	))

	VideoProxy(ctx)

	require.Equal(t, http.StatusOK, writer.Code)
	assert.NotContains(t, writer.Body.String(), `"error"`)
}

type errorAfterReader struct {
	data []byte
	err  error
	done bool
}

func (r *errorAfterReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, r.data), r.err
}

func TestVideoProxyStoredKeyNeverFallsBackToCurrentKey(t *testing.T) {
	db := setupVideoProxyDB(t)
	createVideoProxyChannel(t, db, 501, constant.ChannelTypeSeedDance, "CURRENT_KEY", "https://seedance.fixture", "")
	createVideoProxyTask(
		t, db, "task_key_no_fallback", 41, 501, constant.TaskPlatform("59"),
		model.TaskStatusSuccess, "REMOVED_STORED_KEY", "UPSTREAM_PRIVATE", "",
	)
	fetchCalled := false
	fetcher := &videoProxyFetcherFixture{
		fetch: func(context.Context, string, string, string, string) (*channel.VideoContent, error) {
			fetchCalled = true
			return nil, nil
		},
	}
	writer := newTrackingWriter()
	ctx := newVideoProxyContext(writer, "/v1/videos/task_key_no_fallback/content", "task_key_no_fallback", 41)
	ctx.Set(videoProxyTaskAdaptorLookupContextKey, videoProxyTaskAdaptorLookupFunc(
		func(constant.TaskPlatform) channel.TaskAdaptor { return fetcher },
	))

	VideoProxy(ctx)

	require.Equal(t, http.StatusBadGateway, writer.Code)
	assert.False(t, fetchCalled)
	response := decodeOpenAIVideoError(t, writer.Body.Bytes())
	assert.Equal(t, "stored_credential_unavailable", response.Error.Code)
}

func TestVideoProxyErrorPayloadHasNoProviderData(t *testing.T) {
	db := setupVideoProxyDB(t)
	createVideoProxyChannel(t, db, 501, constant.ChannelTypeSeedDance, "STORED_KEY", "https://seedance.fixture", "")
	createVideoProxyTask(
		t, db, "task_provider_data", 41, 501, constant.TaskPlatform("59"),
		model.TaskStatusSuccess, "STORED_KEY", "UPSTREAM_PRIVATE", "",
	)
	fetcher := &videoProxyFetcherFixture{
		fetch: func(context.Context, string, string, string, string) (*channel.VideoContent, error) {
			return nil, fmt.Errorf("provider payload: %s", bytes.Repeat([]byte("X"), 16))
		},
	}
	writer := newTrackingWriter()
	ctx := newVideoProxyContext(writer, "/v1/videos/task_provider_data/content", "task_provider_data", 41)
	ctx.Set(videoProxyTaskAdaptorLookupContextKey, videoProxyTaskAdaptorLookupFunc(
		func(constant.TaskPlatform) channel.TaskAdaptor { return fetcher },
	))

	VideoProxy(ctx)

	require.Equal(t, http.StatusBadGateway, writer.Code)
	assert.Equal(t, "failed to fetch video content", decodeOpenAIVideoError(t, writer.Body.Bytes()).Error.Message)
	assert.NotContains(t, writer.Body.String(), strings.Repeat("X", 16))
}
