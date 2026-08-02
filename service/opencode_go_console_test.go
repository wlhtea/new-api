package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenCodeGoConsoleClientDiscoversWorkspacesKeysAndModels(t *testing.T) {
	fixedNow := time.Unix(1_900_000_000, 0)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/zen/go/v1/models" {
			require.Equal(t, "auth=synthetic-cookie; oc_locale=zh", request.Header.Get("Cookie"))
		}
		switch request.URL.Path {
		case "/auth":
			http.Redirect(writer, request, "/workspace/wrk_ALPHA1", http.StatusFound)
		case "/workspace/wrk_ALPHA1/go":
			_, _ = writer.Write([]byte(openCodeGoCompleteSSRFixture("wrk_ALPHA1", "sub_SAMPLE1")))
		case "/workspace/wrk_BETA2/go":
			_, _ = writer.Write([]byte(openCodeGoCompleteSSRFixture("wrk_BETA2", "sub_SAMPLE2")))
		case "/workspace/wrk_ALPHA1/keys":
			_, _ = writer.Write([]byte(`<html><script>{ key: "sk-synthetic-value" }</script></html>`))
		case "/zen/go/v1/models":
			require.Equal(t, "Bearer sk-synthetic-value", request.Header.Get("Authorization"))
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"data":[{"id":"model-z"},{"id":"model-a"},{"id":"model-a"},{"id":"invalid model"}]}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client, err := newOpenCodeGoConsoleClient(server.URL, server.URL+"/zen/go/v1", server.Client())
	require.NoError(t, err)
	client.now = func() time.Time { return fixedNow }

	results, err := client.DiscoverWorkspacePages(context.Background(), "synthetic-cookie", "")
	require.NoError(t, err)
	require.Len(t, results, 2)
	require.Equal(t, "wrk_ALPHA1", results[0].Workspace.ID)
	require.Equal(t, "wrk_BETA2", results[1].Workspace.ID)
	require.NoError(t, results[1].Error)
	require.Equal(t, fixedNow.Unix(), results[1].Page.Quota.FetchedAt)

	key, err := client.FetchAPIKey(context.Background(), "synthetic-cookie", "wrk_ALPHA1")
	require.NoError(t, err)
	require.Equal(t, "sk-synthetic-value", key)
	models, err := client.FetchModels(context.Background(), key)
	require.NoError(t, err)
	require.Equal(t, []string{"model-a", "model-z"}, models)
}

func TestOpenCodeGoConsoleClientDoesNotFallBackFromFailedCachedWorkspace(t *testing.T) {
	var authCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/auth" {
			authCalls.Add(1)
		}
		http.Redirect(writer, request, "/login", http.StatusFound)
	}))
	defer server.Close()
	client, err := newOpenCodeGoConsoleClient(server.URL, server.URL+"/zen/go/v1", server.Client())
	require.NoError(t, err)

	_, err = client.DiscoverWorkspacePages(context.Background(), "synthetic-cookie", "wrk_STALE1")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrOpenCodeGoAuthenticationInvalid)
	require.Zero(t, authCalls.Load())
}

func TestOpenCodeGoConsoleClientBlocksRedirectOutsideOrigin(t *testing.T) {
	var escapedCalls atomic.Int32
	escaped := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		escapedCalls.Add(1)
		_, _ = writer.Write([]byte("unexpected"))
	}))
	defer escaped.Close()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, escaped.URL+"/workspace/wrk_ESCAPE1/go", http.StatusFound)
	}))
	defer server.Close()
	client, err := newOpenCodeGoConsoleClient(server.URL, server.URL+"/zen/go/v1", server.Client())
	require.NoError(t, err)

	_, err = client.FetchWorkspacePage(context.Background(), "synthetic-cookie", "wrk_ALPHA1")
	require.Error(t, err)
	require.Zero(t, escapedCalls.Load())
}

func TestOpenCodeGoConsoleClientRetainsSecondaryWorkspaceFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/auth":
			http.Redirect(writer, request, "/workspace/wrk_ALPHA1", http.StatusFound)
		case "/workspace/wrk_ALPHA1/go":
			_, _ = writer.Write([]byte(openCodeGoCompleteSSRFixture("wrk_ALPHA1", "sub_SAMPLE1")))
		case "/workspace/wrk_BETA2/go":
			http.Error(writer, "temporary", http.StatusGatewayTimeout)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := newOpenCodeGoConsoleClient(server.URL, server.URL+"/zen/go/v1", server.Client())
	require.NoError(t, err)

	results, err := client.DiscoverWorkspacePages(context.Background(), "synthetic-cookie", "")
	require.NoError(t, err)
	require.Len(t, results, 2)
	require.NotNil(t, results[0].Page)
	require.Error(t, results[1].Error)
	require.Nil(t, results[1].Page)
	require.NotContains(t, fmt.Sprint(results[1].Error), "temporary")
}
