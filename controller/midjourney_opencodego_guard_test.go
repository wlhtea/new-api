package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMidjourneyPollingSkipsOpenCodeGoOriginBeforeExternalRequest(t *testing.T) {
	db := setupVideoProxyDB(t)
	require.NoError(t, db.AutoMigrate(&model.Midjourney{}))

	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	baseURL := server.URL
	channel := &model.Channel{
		Type:    constant.ChannelTypeOpenCodeGo,
		Status:  common.ChannelStatusEnabled,
		Name:    "Midjourney polling endpoint guard",
		BaseURL: &baseURL,
	}
	require.NoError(t, db.Create(channel).Error)
	require.NoError(t, db.Create(&model.Midjourney{
		UserId:    4204,
		MjId:      "mj-poll-origin-endpoint-guard",
		ChannelId: channel.Id,
		Progress:  "0%",
		Status:    "IN_PROGRESS",
	}).Error)

	summary := runMidjourneyTaskUpdateOnce(context.Background(), nil)

	assert.Equal(t, 1, summary.UnfinishedTasks)
	assert.Equal(t, 1, summary.ChannelsScanned)
	assert.Zero(t, requestCount.Load())

	stored := model.GetByOnlyMJId("mj-poll-origin-endpoint-guard")
	require.NotNil(t, stored)
	assert.Equal(t, "0%", stored.Progress)
	assert.Equal(t, "IN_PROGRESS", stored.Status)
}
