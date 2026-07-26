package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSeedDanceProbeChannel(t *testing.T, baseURL string, key string) *model.Channel {
	t.Helper()
	url := baseURL
	channel := &model.Channel{
		Type:    constant.ChannelTypeSeedDance,
		Name:    "seed-dance-test",
		Key:     key,
		Status:  common.ChannelStatusEnabled,
		BaseURL: &url,
		Models:  "seedance-uncensored",
		Group:   "default",
	}
	return channel
}

func TestSeedDanceChannelTestUsesMissingTaskProbe(t *testing.T) {
	var gotPath string
	var gotAuth string
	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		atomic.AddInt32(&requestCount, 1)
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w,
			`{"success":false,"errCode":"400","errMessage":"Task not found"}`,
		)
	}))
	defer server.Close()

	channel := newSeedDanceProbeChannel(t, server.URL, "TEST_KEY")

	_, _, err := testSeedDanceChannel(context.Background(), channel)
	require.NoError(t, err)
	assert.Equal(t, "Bearer TEST_KEY", gotAuth)
	assert.Regexp(t, `^/status/new-api-channel-test-[A-Za-z0-9]+$`, gotPath)
	assert.Equal(t, int32(1), atomic.LoadInt32(&requestCount), "probe must issue exactly one request")
}

func TestSeedDanceChannelTestRejectsAuthenticationFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"unauthorized", http.StatusUnauthorized},
		{"forbidden", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(
				w http.ResponseWriter,
				r *http.Request,
			) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"success":false,"errCode":"401"}`)
			}))
			defer server.Close()

			channel := newSeedDanceProbeChannel(t, server.URL, "TEST_KEY")
			key, statusCode, err := testSeedDanceChannel(
				context.Background(),
				channel,
			)
			require.Error(t, err)
			assert.Equal(t, "TEST_KEY", key)
			assert.Equal(t, tc.status, statusCode)
			assert.Contains(t, err.Error(), "authentication failed")
		})
	}
}

func TestSeedDanceChannelTestRejectsBusinessAuthenticationFailure(t *testing.T) {
	cases := []struct {
		name           string
		response       string
		status         int
		messageSnippet string
	}{
		{
			name:           "api key not found",
			response:       `{"success":false,"errCode":"401","errMessage":"API key not found"}`,
			status:         http.StatusUnauthorized,
			messageSnippet: "authentication failed",
		},
		{
			name:           "credential does not exist",
			response:       `{"success":false,"errCode":403,"errMessage":"authentication credential does not exist"}`,
			status:         http.StatusForbidden,
			messageSnippet: "authentication failed",
		},
		{
			name:           "named authentication error",
			response:       `{"success":false,"errCode":"AUTHENTICATION_FAILED","errMessage":"API key not found"}`,
			status:         http.StatusBadGateway,
			messageSnippet: "unrelated business error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(
				w http.ResponseWriter,
				r *http.Request,
			) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.response)
			}))
			defer server.Close()

			channel := newSeedDanceProbeChannel(t, server.URL, "TEST_KEY")
			key, statusCode, err := testSeedDanceChannel(
				context.Background(),
				channel,
			)
			require.Error(t, err)
			assert.Equal(t, "TEST_KEY", key)
			assert.Equal(t, tc.status, statusCode)
			assert.Contains(t, err.Error(), tc.messageSnippet)
		})
	}
}

func TestSeedDanceChannelTestRejectsUnrelatedBusinessError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w,
			`{"success":false,"errCode":"500","errMessage":"internal upstream error"}`,
		)
	}))
	defer server.Close()

	channel := newSeedDanceProbeChannel(t, server.URL, "TEST_KEY")
	_, _, err := testSeedDanceChannel(context.Background(), channel)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unrelated business error")
}

func TestSeedDanceChannelTestRejectsUnexpectedSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"status":"completed"}`)
	}))
	defer server.Close()

	channel := newSeedDanceProbeChannel(t, server.URL, "TEST_KEY")
	_, _, err := testSeedDanceChannel(context.Background(), channel)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected success")
}

func TestSeedDanceChannelTestRejectsMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{not-valid-json`)
	}))
	defer server.Close()

	channel := newSeedDanceProbeChannel(t, server.URL, "TEST_KEY")
	_, _, err := testSeedDanceChannel(context.Background(), channel)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed response")
}

func TestSeedDanceChannelTestRejectsNetworkError(t *testing.T) {
	channel := newSeedDanceProbeChannel(t, "http://127.0.0.1:1", "TEST_KEY")
	_, _, err := testSeedDanceChannel(context.Background(), channel)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transport error")
}

func TestSeedDanceChannelTestRejectsDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		// Sleep beyond the probe deadline so the parent context expires first.
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer server.Close()

	channel := newSeedDanceProbeChannel(t, server.URL, "TEST_KEY")
	// Use an aggressively short parent context to force the deadline path.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, _, err := testSeedDanceChannel(ctx, channel)
	require.Error(t, err)
	// The probe wraps either a deadline or transport error; both are acceptable
	// failures here, but the message must clearly indicate a probe failure.
	assert.True(t,
		strings.Contains(err.Error(), "timed out") ||
			strings.Contains(err.Error(), "transport error") ||
			errors.Is(err, context.DeadlineExceeded),
		"expected deadline/transport failure, got: %v", err,
	)
}

func TestSeedDanceChannelTestAcceptsChineseNotFoundMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w,
			`{"success":false,"errCode":"404","errMessage":"任务不存在"}`,
		)
	}))
	defer server.Close()

	channel := newSeedDanceProbeChannel(t, server.URL, "TEST_KEY")
	_, _, err := testSeedDanceChannel(context.Background(), channel)
	require.NoError(t, err)
}

func TestSeedDanceChannelTestRequiresEnabledKey(t *testing.T) {
	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		atomic.AddInt32(&requestCount, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w,
			`{"success":false,"errCode":"404","errMessage":"Task not found"}`,
		)
	}))
	defer server.Close()

	for _, key := range []string{"", "   "} {
		channel := newSeedDanceProbeChannel(t, server.URL, key)
		_, _, err := testSeedDanceChannel(context.Background(), channel)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "key")
	}
	assert.Zero(
		t,
		atomic.LoadInt32(&requestCount),
		"an empty channel key must fail before any upstream request",
	)
}

func TestTestChannelRoutesSeedDanceToProbe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w,
			`{"success":false,"errCode":"404","errMessage":"Task not found"}`,
		)
	}))
	defer server.Close()

	channel := newSeedDanceProbeChannel(t, server.URL, "TEST_KEY")

	result := testChannel(context.Background(), channel, 0, "", "", false)
	require.NoError(t, result.localErr)
	require.Nil(t, result.newAPIError)
	require.NotNil(t, result.context)
	assert.Equal(
		t,
		"TEST_KEY",
		common.GetContextKeyString(
			result.context,
			constant.ContextKeyChannelKey,
		),
	)
}

func TestTestChannelRoutesSeedDanceFailureToResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"success":false,"errCode":"401"}`)
	}))
	defer server.Close()

	channel := newSeedDanceProbeChannel(t, server.URL, "TEST_KEY")

	result := testChannel(context.Background(), channel, 0, "", "", false)
	require.Error(t, result.localErr)
	require.NotNil(t, result.newAPIError)
	assert.Equal(t, http.StatusUnauthorized, result.newAPIError.StatusCode)
	require.NotNil(t, result.context)
	require.NotNil(
		t,
		result.context.Request,
		"channel error processing requires a non-nil request on the Gin context",
	)
	assert.Equal(
		t,
		"TEST_KEY",
		common.GetContextKeyString(
			result.context,
			constant.ContextKeyChannelKey,
		),
	)
}
