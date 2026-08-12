package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeOpenCodeGoReferralRewardLifecycle struct {
	summary      service.OpenCodeGoReferralApplySummary
	err          error
	calls        int
	channelID    int
	workspaceUID string
	source       string
}

func (fake *fakeOpenCodeGoReferralRewardLifecycle) ApplyReferralReward(
	_ context.Context,
	channelID int,
	workspaceUID string,
	source string,
) (service.OpenCodeGoReferralApplySummary, error) {
	fake.calls++
	fake.channelID = channelID
	fake.workspaceUID = workspaceUID
	fake.source = source
	return fake.summary, fake.err
}

func TestCancelOpenCodeGoSubscriptionRenewalRequiresExplicitConfirmation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST(
		"/channel/:id/opencode-go/workspaces/:workspace_uid/subscription/cancel-renewal",
		CancelOpenCodeGoSubscriptionRenewal,
	)
	request := httptest.NewRequest(
		http.MethodPost,
		"/channel/1/opencode-go/workspaces/workspace-test/subscription/cancel-renewal",
		strings.NewReader(`{"confirmation":"cancel"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	engine.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, response.Body.String(), `"success":false`)
	assert.Contains(t, response.Body.String(), openCodeGoCancelRenewalConfirmation)
}

func TestApplyOpenCodeGoReferralRewardOnlyAuditsVerifiedSingleApply(t *testing.T) {
	originalPoolView := getOpenCodeGoReferralPoolView
	t.Cleanup(func() { getOpenCodeGoReferralPoolView = originalPoolView })
	tests := []struct {
		name        string
		summary     service.OpenCodeGoReferralApplySummary
		err         error
		wantSuccess bool
		wantAudits  int64
	}{
		{name: "service failure", err: errors.New("synthetic verification failure")},
		{name: "zero execution", summary: service.OpenCodeGoReferralApplySummary{}},
		{name: "unverified apply", summary: service.OpenCodeGoReferralApplySummary{Attempted: 1}},
		{name: "unexpected multi apply", summary: service.OpenCodeGoReferralApplySummary{Attempted: 2, Applied: 2}},
		{
			name:        "verified single apply",
			summary:     service.OpenCodeGoReferralApplySummary{Attempted: 1, Applied: 1},
			wantSuccess: true,
			wantAudits:  1,
		},
		{
			name: "service failure redacts upstream identifiers",
			err:  errors.New("auth=synthetic-cookie sk-synthetic-key wrk_PRIVATE1 ref_PRIVATE1"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := setupModelListControllerTestDB(t)
			require.NoError(t, db.AutoMigrate(
				&model.Log{},
				&model.OpenCodeGoIdentity{},
				&model.OpenCodeGoWorkspace{},
				&model.OpenCodeGoQuotaWindow{},
				&model.OpenCodeGoWorkspaceModel{},
				&model.OpenCodeGoOperation{},
			))
			channel := &model.Channel{
				Type:   constant.ChannelTypeOpenCodeGo,
				Name:   "OpenCode Go referral controller test",
				Status: common.ChannelStatusEnabled,
				Group:  "default",
			}
			require.NoError(t, db.Create(channel).Error)

			fake := &fakeOpenCodeGoReferralRewardLifecycle{summary: test.summary, err: test.err}
			gin.SetMode(gin.TestMode)
			engine := gin.New()
			engine.POST(
				"/channel/:id/opencode-go/workspaces/:workspace_uid/referral-rewards/apply",
				func(c *gin.Context) {
					c.Set("id", 42)
					applyOpenCodeGoReferralReward(c, fake)
				},
			)
			request := httptest.NewRequest(
				http.MethodPost,
				fmt.Sprintf("/channel/%d/opencode-go/workspaces/workspace-test/referral-rewards/apply", channel.Id),
				nil,
			)
			response := httptest.NewRecorder()

			engine.ServeHTTP(response, request)

			require.Equal(t, http.StatusOK, response.Code)
			assert.Contains(t, response.Body.String(), fmt.Sprintf(`"success":%t`, test.wantSuccess))
			if test.name == "service failure redacts upstream identifiers" {
				for _, forbidden := range []string{"synthetic-cookie", "sk-synthetic-key", "wrk_PRIVATE1", "ref_PRIVATE1"} {
					assert.NotContains(t, response.Body.String(), forbidden)
				}
			}
			assert.Equal(t, 1, fake.calls)
			assert.Equal(t, channel.Id, fake.channelID)
			assert.Equal(t, "workspace-test", fake.workspaceUID)
			assert.Equal(t, "manual", fake.source)
			var auditCount int64
			require.NoError(t, db.Model(&model.Log{}).
				Where("type = ? AND other LIKE ?", model.LogTypeManage, `%channel.opencode_go_referral_apply%`).
				Count(&auditCount).Error)
			assert.Equal(t, test.wantAudits, auditCount)
		})
	}
}

func TestApplyOpenCodeGoReferralRewardKeepsVerifiedSuccessWhenPoolRefreshFails(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	originalPoolView := getOpenCodeGoReferralPoolView
	getOpenCodeGoReferralPoolView = func(int) (*service.OpenCodeGoPoolView, error) {
		return nil, errors.New("synthetic pool read failure")
	}
	t.Cleanup(func() { getOpenCodeGoReferralPoolView = originalPoolView })

	fake := &fakeOpenCodeGoReferralRewardLifecycle{
		summary: service.OpenCodeGoReferralApplySummary{Attempted: 1, Applied: 1},
	}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/channel/:id/opencode-go/workspaces/:workspace_uid/referral-rewards/apply", func(c *gin.Context) {
		c.Set("id", 42)
		applyOpenCodeGoReferralReward(c, fake)
	})
	request := httptest.NewRequest(http.MethodPost, "/channel/62/opencode-go/workspaces/workspace-test/referral-rewards/apply", nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, response.Body.String(), `"success":true`)
	assert.Contains(t, response.Body.String(), `"pool_refresh_required":true`)
	assert.NotContains(t, response.Body.String(), `"pool":`)
	var auditCount int64
	require.NoError(t, db.Model(&model.Log{}).
		Where("type = ? AND other LIKE ?", model.LogTypeManage, `%channel.opencode_go_referral_apply%`).
		Count(&auditCount).Error)
	assert.Equal(t, int64(1), auditCount)
}

func TestApplyOpenCodeGoReferralRewardOmitsStalePoolWhenServiceRequiresRefresh(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	originalPoolView := getOpenCodeGoReferralPoolView
	poolViewCalls := 0
	getOpenCodeGoReferralPoolView = func(int) (*service.OpenCodeGoPoolView, error) {
		poolViewCalls++
		return &service.OpenCodeGoPoolView{}, nil
	}
	t.Cleanup(func() { getOpenCodeGoReferralPoolView = originalPoolView })

	fake := &fakeOpenCodeGoReferralRewardLifecycle{
		summary: service.OpenCodeGoReferralApplySummary{
			Attempted:           1,
			Applied:             1,
			PoolRefreshRequired: true,
		},
	}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/channel/:id/opencode-go/workspaces/:workspace_uid/referral-rewards/apply", func(c *gin.Context) {
		c.Set("id", 42)
		applyOpenCodeGoReferralReward(c, fake)
	})
	request := httptest.NewRequest(http.MethodPost, "/channel/62/opencode-go/workspaces/workspace-test/referral-rewards/apply", nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, response.Body.String(), `"success":true`)
	assert.Contains(t, response.Body.String(), `"pool_refresh_required":true`)
	assert.Equal(t, 1, strings.Count(response.Body.String(), `"pool_refresh_required"`), "the internal summary signal must not be serialized")
	assert.NotContains(t, response.Body.String(), `"pool":`)
	assert.Zero(t, poolViewCalls, "a locally stale pool must not be read into the success response")
	var auditCount int64
	require.NoError(t, db.Model(&model.Log{}).
		Where("type = ? AND other LIKE ?", model.LogTypeManage, `%channel.opencode_go_referral_apply%`).
		Count(&auditCount).Error)
	assert.Equal(t, int64(1), auditCount)
}
