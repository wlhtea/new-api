package controller

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestValidateChannelProxy(t *testing.T) {
	tests := []struct {
		name    string
		proxy   string
		wantErr bool
	}{
		{name: "empty"},
		{name: "http", proxy: "http://proxy.example:8080"},
		{name: "https", proxy: "https://proxy.example:8443"},
		{name: "socks5", proxy: "socks5://proxy.example"},
		{name: "socks5h", proxy: "socks5h://proxy.example:1080/"},
		{name: "unsupported", proxy: "ftp://proxy.example", wantErr: true},
		{name: "path", proxy: "socks5://proxy.example:1080/path", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setting, err := common.Marshal(dto.ChannelSettings{Proxy: test.proxy})
			require.NoError(t, err)
			channel := &model.Channel{
				Type:    constant.ChannelTypeOpenAI,
				Setting: common.GetPointer(string(setting)),
			}

			err = validateChannel(channel, false)

			if test.wantErr {
				require.ErrorContains(t, err, "invalid channel proxy")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestValidateChannelRequiresNewAPIBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL *string
		wantErr bool
	}{
		{name: "missing", wantErr: true},
		{name: "blank", baseURL: common.GetPointer("  "), wantErr: true},
		{name: "configured", baseURL: common.GetPointer("https://new-api.example")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			channel := &model.Channel{
				Type:    constant.ChannelTypeNewAPI,
				BaseURL: test.baseURL,
			}

			err := validateChannel(channel, false)

			if test.wantErr {
				require.ErrorContains(t, err, "New API channel base URL cannot be empty")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestNewAPIChannelRegistration(t *testing.T) {
	apiType, ok := common.ChannelType2APIType(constant.ChannelTypeNewAPI)

	require.True(t, ok)
	assert.Equal(t, constant.APITypeNewAPI, apiType)
	assert.Equal(t, "New API", constant.GetChannelTypeName(constant.ChannelTypeNewAPI))
	require.Greater(t, len(constant.ChannelBaseURLs), constant.ChannelTypeNewAPI)
	assert.Empty(t, constant.ChannelBaseURLs[constant.ChannelTypeNewAPI])
}

func TestOpenCodeGoChannelRegistration(t *testing.T) {
	apiType, ok := common.ChannelType2APIType(constant.ChannelTypeOpenCodeGo)

	require.True(t, ok)
	assert.Equal(t, constant.APITypeOpenCodeGo, apiType)
	assert.Equal(t, "OpenCode Go", constant.GetChannelTypeName(constant.ChannelTypeOpenCodeGo))
	require.Greater(t, len(constant.ChannelBaseURLs), constant.ChannelTypeOpenCodeGo)
	assert.Equal(t, "https://opencode.ai/zen/go/v1", constant.ChannelBaseURLs[constant.ChannelTypeOpenCodeGo])
	assert.Equal(t, []constant.EndpointType{
		constant.EndpointTypeOpenAI,
		constant.EndpointTypeOpenAIResponse,
		constant.EndpointTypeAnthropic,
	}, common.GetEndpointTypesByChannelType(constant.ChannelTypeOpenCodeGo, "glm-5.2"))
}

func TestValidateOpenCodeGoChannelUsesFixedBaseURLAndPoolCredentials(t *testing.T) {
	tests := []struct {
		name        string
		baseURL     *string
		key         string
		wantErrText string
	}{
		{name: "empty legacy key and implicit fixed URL"},
		{name: "explicit fixed URL", baseURL: common.GetPointer("https://opencode.ai/zen/go/v1/")},
		{name: "custom URL rejected", baseURL: common.GetPointer("https://proxy.example/v1"), wantErrText: "base URL is fixed"},
		{name: "legacy key rejected", key: "must-not-be-stored", wantErrText: "credentials must be managed by the account pool"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			channel := &model.Channel{
				Type:    constant.ChannelTypeOpenCodeGo,
				BaseURL: test.baseURL,
				Key:     test.key,
			}

			err := validateChannel(channel, true)
			if test.wantErrText != "" {
				require.ErrorContains(t, err, test.wantErrText)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestAddOpenCodeGoChannelWithoutLegacyKey(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	body := []byte(`{
		"mode":"single",
		"channel":{
			"name":"OpenCode Go pool",
			"type":62,
			"key":"",
			"models":"model-a,model-b",
			"group":"default",
			"status":1
		}
	}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	AddChannel(ctx)

	var response struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success, response.Message)

	var channels []model.Channel
	require.NoError(t, db.Where("type = ?", constant.ChannelTypeOpenCodeGo).Find(&channels).Error)
	require.Len(t, channels, 1)
	assert.Empty(t, channels[0].Key)
	assert.Equal(t, "model-a,model-b", channels[0].Models)
	assert.Equal(t, common.ChannelStatusAutoDisabled, channels[0].Status)
	assert.Equal(t, "opencode_go:no_eligible_workspace", channels[0].GetOtherInfo()["status_reason"])

	var abilities []model.Ability
	require.NoError(t, db.Where("channel_id = ?", channels[0].Id).Order("model ASC").Find(&abilities).Error)
	require.Len(t, abilities, 2)
	assert.Equal(t, []string{"model-a", "model-b"}, []string{abilities[0].Model, abilities[1].Model})
	assert.False(t, abilities[0].Enabled)
	assert.False(t, abilities[1].Enabled)
}

func TestCopyOpenCodeGoChannelPreservesAdminModelsInDisabledEmptyPool(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(
		&model.OpenCodeGoIdentity{},
		&model.OpenCodeGoWorkspace{},
		&model.OpenCodeGoQuotaWindow{},
		&model.OpenCodeGoWorkspaceModel{},
		&model.OpenCodeGoOperation{},
	))
	origin := &model.Channel{
		Type:   constant.ChannelTypeOpenCodeGo,
		Name:   "OpenCode Go source",
		Key:    "legacy-key-placeholder",
		Models: "model-a,model-b",
		Group:  "vip",
		Status: common.ChannelStatusEnabled,
	}
	require.NoError(t, db.Create(origin).Error)
	identity := &model.OpenCodeGoIdentity{
		UID:                   "identity-source",
		ChannelID:             origin.Id,
		AuthCookieCiphertext:  "synthetic-ciphertext",
		AuthCookieFingerprint: fmt.Sprintf("%064s", "source"),
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, db.Create(identity).Error)
	require.NoError(t, db.Create(&model.OpenCodeGoWorkspace{
		UID:                 "workspace-source",
		ChannelID:           origin.Id,
		IdentityID:          identity.ID,
		UpstreamWorkspaceID: "workspace-source-upstream",
	}).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", origin.Id)}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/copy", nil)

	CopyChannel(ctx)

	var response struct {
		Success bool `json:"success"`
		Data    struct {
			ID int `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success, recorder.Body.String())
	require.NotZero(t, response.Data.ID)

	clone, err := model.GetChannelById(response.Data.ID, true)
	require.NoError(t, err)
	assert.Equal(t, constant.ChannelTypeOpenCodeGo, clone.Type)
	assert.Empty(t, clone.Key)
	assert.Equal(t, "model-a,model-b", clone.Models)
	assert.Equal(t, "vip", clone.Group)
	assert.Equal(t, common.ChannelStatusAutoDisabled, clone.Status)
	assert.Equal(t, "opencode_go:no_eligible_workspace", clone.GetOtherInfo()["status_reason"])

	var identityCount int64
	require.NoError(t, db.Model(&model.OpenCodeGoIdentity{}).Where("channel_id = ?", clone.Id).Count(&identityCount).Error)
	assert.Zero(t, identityCount)
	var workspaceCount int64
	require.NoError(t, db.Model(&model.OpenCodeGoWorkspace{}).Where("channel_id = ?", clone.Id).Count(&workspaceCount).Error)
	assert.Zero(t, workspaceCount)
	var abilities []model.Ability
	require.NoError(t, db.Where("channel_id = ?", clone.Id).Order("model ASC").Find(&abilities).Error)
	require.Len(t, abilities, 2)
	assert.Equal(t, []string{"model-a", "model-b"}, []string{abilities[0].Model, abilities[1].Model})
	assert.False(t, abilities[0].Enabled)
	assert.False(t, abilities[1].Enabled)
}

func TestUpdateOpenCodeGoChannelPersistsAdminModelsWithEmptyPool(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(
		&model.OpenCodeGoIdentity{},
		&model.OpenCodeGoWorkspace{},
		&model.OpenCodeGoQuotaWindow{},
		&model.OpenCodeGoWorkspaceModel{},
		&model.OpenCodeGoOperation{},
	))
	channel := &model.Channel{
		Type:   constant.ChannelTypeOpenCodeGo,
		Name:   "OpenCode Go edit",
		Status: common.ChannelStatusAutoDisabled,
		Models: "model-a,model-b",
		Group:  "default",
	}
	service.PrepareOpenCodeGoPoolContainer(channel)
	require.NoError(t, channel.Insert())

	body := []byte(fmt.Sprintf(`{
		"id":%d,
		"type":62,
		"name":"OpenCode Go edited",
		"key":"",
		"models":"model-a",
		"group":"default"
	}`, channel.Id))
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/channel", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	UpdateChannel(ctx)

	var response struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			Models string `json:"models"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success, response.Message)
	assert.Equal(t, "model-a", response.Data.Models)
	reloaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "OpenCode Go edited", reloaded.Name)
	assert.Equal(t, "model-a", reloaded.Models)
	assert.Equal(t, common.ChannelStatusAutoDisabled, reloaded.Status)
	var abilities []model.Ability
	require.NoError(t, db.Where("channel_id = ?", channel.Id).Find(&abilities).Error)
	require.Len(t, abilities, 1)
	assert.Equal(t, "model-a", abilities[0].Model)
	assert.False(t, abilities[0].Enabled)
}

func TestUpdateOpenCodeGoChannelPreservesLifecyclePolicy(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(
		&model.OpenCodeGoIdentity{},
		&model.OpenCodeGoWorkspace{},
		&model.OpenCodeGoQuotaWindow{},
		&model.OpenCodeGoWorkspaceModel{},
		&model.OpenCodeGoOperation{},
	))
	autoEnableChinaModels := false
	autoApplyReferralRewards := false
	rewardLimit := 0
	channel := &model.Channel{
		Type:   constant.ChannelTypeOpenCodeGo,
		Name:   "OpenCode Go protected policy",
		Status: common.ChannelStatusAutoDisabled,
		Models: "model-old",
		Group:  "default",
	}
	channel.SetOtherSettings(dto.ChannelOtherSettings{
		OpenCodeGo: &dto.OpenCodeGoConfig{
			ModelProtocols:                map[string]string{"old-*": dto.OpenCodeGoProtocolChat},
			DefaultProtocol:               dto.OpenCodeGoProtocolChat,
			AutoEnableChinaModels:         &autoEnableChinaModels,
			AutoApplyReferralRewards:      &autoApplyReferralRewards,
			ReferralRewardsMaxPerRun:      &rewardLimit,
			AutoCancelSubscriptionRenewal: true,
		},
	})
	service.PrepareOpenCodeGoPoolContainer(channel)
	require.NoError(t, channel.Insert())

	proposedSettings := dto.ChannelOtherSettings{
		OpenCodeGo: &dto.OpenCodeGoConfig{
			ModelProtocols:                map[string]string{"new-*": dto.OpenCodeGoProtocolMessages},
			DefaultProtocol:               dto.OpenCodeGoProtocolResponses,
			AutoEnableChinaModels:         common.GetPointer(true),
			AutoApplyReferralRewards:      common.GetPointer(true),
			ReferralRewardsMaxPerRun:      common.GetPointer(20),
			AutoCancelSubscriptionRenewal: false,
		},
	}
	encodedSettings, err := common.Marshal(proposedSettings)
	require.NoError(t, err)
	body, err := common.Marshal(map[string]any{
		"id":       channel.Id,
		"type":     constant.ChannelTypeOpenCodeGo,
		"name":     "OpenCode Go protocol edited",
		"key":      "",
		"models":   "model-new",
		"group":    "default",
		"settings": string(encodedSettings),
	})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("role", common.RoleRootUser)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/channel", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	UpdateChannel(ctx)

	var response struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success, response.Message)
	reloaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "model-new", reloaded.Models)
	config := reloaded.GetOtherSettings().OpenCodeGo
	require.NotNil(t, config)
	assert.Equal(t, dto.OpenCodeGoProtocolResponses, config.DefaultProtocol)
	assert.Equal(t, map[string]string{"new-*": dto.OpenCodeGoProtocolMessages}, config.ModelProtocols)
	require.NotNil(t, config.AutoEnableChinaModels)
	assert.False(t, *config.AutoEnableChinaModels)
	require.NotNil(t, config.AutoApplyReferralRewards)
	assert.False(t, *config.AutoApplyReferralRewards)
	require.NotNil(t, config.ReferralRewardsMaxPerRun)
	assert.Zero(t, *config.ReferralRewardsMaxPerRun)
	assert.True(t, config.AutoCancelSubscriptionRenewal)
	var abilities []model.Ability
	require.NoError(t, db.Where("channel_id = ?", channel.Id).Find(&abilities).Error)
	require.Len(t, abilities, 1)
	assert.Equal(t, "model-new", abilities[0].Model)
	assert.False(t, abilities[0].Enabled)
}

func TestUpdateOpenCodeGoChannelRefreshesMemoryRoutingAfterModelRemoval(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(
		&model.OpenCodeGoIdentity{},
		&model.OpenCodeGoWorkspace{},
		&model.OpenCodeGoQuotaWindow{},
		&model.OpenCodeGoWorkspaceModel{},
		&model.OpenCodeGoOperation{},
	))
	enableOpenCodeGoControllerMemoryCache(t)

	channel := &model.Channel{
		Type:   constant.ChannelTypeOpenCodeGo,
		Name:   "OpenCode Go cache edit",
		Status: common.ChannelStatusEnabled,
		Models: "model-a,model-b",
		Group:  "default",
	}
	require.NoError(t, channel.Insert())
	t.Cleanup(func() { service.RemoveOpenCodeGoPoolChannel(channel.Id) })
	createEligibleOpenCodeGoControllerWorkspace(t, db, channel.Id, []string{"model-a", "model-b"})
	require.NoError(t, service.ReconcileOpenCodeGoPoolChannel(channel.Id))
	model.InitChannelCache()

	for _, modelID := range []string{"model-a", "model-b"} {
		selected, err := model.GetRandomSatisfiedChannel("default", modelID, 0, "/v1/chat/completions")
		require.NoError(t, err)
		require.NotNil(t, selected)
		assert.Equal(t, channel.Id, selected.Id)
	}

	body := []byte(fmt.Sprintf(`{
		"id":%d,
		"type":62,
		"name":"OpenCode Go cache edited",
		"key":"",
		"models":"model-a",
		"group":"default"
	}`, channel.Id))
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/channel", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	UpdateChannel(ctx)

	var response struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success, response.Message)
	retained, err := model.GetRandomSatisfiedChannel("default", "model-a", 0, "/v1/chat/completions")
	require.NoError(t, err)
	require.NotNil(t, retained)
	assert.Equal(t, channel.Id, retained.Id)
	removed, err := model.GetRandomSatisfiedChannel("default", "model-b", 0, "/v1/chat/completions")
	require.NoError(t, err)
	assert.Nil(t, removed)
}

func TestUpdateOpenCodeGoChannelRefreshesMemoryRoutingWhenReconcileFails(t *testing.T) {
	setupModelListControllerTestDB(t)
	enableOpenCodeGoControllerMemoryCache(t)

	channel := &model.Channel{
		Type:   constant.ChannelTypeOpenCodeGo,
		Name:   "OpenCode Go failed reconcile edit",
		Status: common.ChannelStatusEnabled,
		Models: "model-a,model-b",
		Group:  "default",
	}
	require.NoError(t, channel.Insert())
	t.Cleanup(func() { service.RemoveOpenCodeGoPoolChannel(channel.Id) })
	model.InitChannelCache()
	selected, err := model.GetRandomSatisfiedChannel("default", "model-b", 0, "/v1/chat/completions")
	require.NoError(t, err)
	require.NotNil(t, selected)

	body := []byte(fmt.Sprintf(`{
		"id":%d,
		"type":62,
		"name":"OpenCode Go persisted before reconcile error",
		"key":"",
		"models":"model-a",
		"group":"default"
	}`, channel.Id))
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/channel", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	UpdateChannel(ctx)

	var response struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.False(t, response.Success)
	assert.NotEmpty(t, response.Message)
	reloaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "model-a", reloaded.Models)
	retained, err := model.GetRandomSatisfiedChannel("default", "model-a", 0, "/v1/chat/completions")
	require.NoError(t, err)
	require.NotNil(t, retained)
	removed, err := model.GetRandomSatisfiedChannel("default", "model-b", 0, "/v1/chat/completions")
	require.NoError(t, err)
	assert.Nil(t, removed)
}

func enableOpenCodeGoControllerMemoryCache(t *testing.T) {
	t.Helper()
	previousMemoryCache := common.MemoryCacheEnabled
	previousSecret := common.CryptoSecret
	previousSecretConfigured := common.CryptoSecretExplicitlyConfigured
	common.MemoryCacheEnabled = true
	common.CryptoSecret = "test-only-controller-cache-secret"
	common.CryptoSecretExplicitlyConfigured = true
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCache
		common.CryptoSecret = previousSecret
		common.CryptoSecretExplicitlyConfigured = previousSecretConfigured
	})
}

func createEligibleOpenCodeGoControllerWorkspace(
	t *testing.T,
	db *gorm.DB,
	channelID int,
	models []string,
) {
	t.Helper()
	codec, err := service.NewOpenCodeGoCredentialCodec(common.CryptoSecret)
	require.NoError(t, err)
	identityUID := "identity-controller-cache"
	cookie := "cookie-controller-cache"
	cookieCiphertext, err := codec.Encrypt(service.OpenCodeGoCredentialAuthCookie, channelID, identityUID, cookie)
	require.NoError(t, err)
	cookieFingerprint, err := codec.Fingerprint(service.OpenCodeGoCredentialAuthCookie, cookie)
	require.NoError(t, err)
	identity := &model.OpenCodeGoIdentity{
		UID:                   identityUID,
		ChannelID:             channelID,
		AuthCookieCiphertext:  cookieCiphertext,
		AuthCookieFingerprint: cookieFingerprint,
		Status:                model.OpenCodeGoIdentityStatusActive,
	}
	require.NoError(t, db.Create(identity).Error)

	workspaceUID := "workspace-controller-cache"
	apiKey := "sk-controller-cache"
	apiKeyCiphertext, err := codec.Encrypt(service.OpenCodeGoCredentialAPIKey, channelID, workspaceUID, apiKey)
	require.NoError(t, err)
	apiKeyFingerprint, err := codec.Fingerprint(service.OpenCodeGoCredentialAPIKey, apiKey)
	require.NoError(t, err)
	fetchedAt := time.Now().Unix()
	workspace := &model.OpenCodeGoWorkspace{
		UID:                 workspaceUID,
		ChannelID:           channelID,
		IdentityID:          identity.ID,
		UpstreamWorkspaceID: "workspace-controller-cache-upstream",
		APIKeyCiphertext:    apiKeyCiphertext,
		APIKeyFingerprint:   apiKeyFingerprint,
		CredentialStatus:    model.OpenCodeGoCredentialValid,
		MembershipStatus:    model.OpenCodeGoMembershipActive,
		ManualEnabled:       true,
		EffectiveState:      model.OpenCodeGoStateEligible,
		QuotaSnapshotStatus: model.OpenCodeGoQuotaSnapshotComplete,
		QuotaFetchedAt:      fetchedAt,
		QuotaNextRefreshAt:  fetchedAt + 3600,
		QuotaParserVersion:  service.OpenCodeGoSSRParserVersion,
	}
	require.NoError(t, db.Create(workspace).Error)
	for index, kind := range model.OpenCodeGoQuotaKinds {
		require.NoError(t, db.Create(&model.OpenCodeGoQuotaWindow{
			WorkspaceID:  workspace.ID,
			Kind:         kind,
			UsedPercent:  float64(10 + index),
			ResetSeconds: int64((index + 1) * 3600),
			ResetAt:      fetchedAt + int64((index+1)*3600),
			FetchedAt:    fetchedAt,
		}).Error)
	}
	for _, modelID := range models {
		require.NoError(t, db.Create(&model.OpenCodeGoWorkspaceModel{
			WorkspaceID: workspace.ID,
			Model:       modelID,
			Discovered:  true,
			State:       model.OpenCodeGoModelAvailable,
		}).Error)
	}
}

func TestOpenCodeGoChannelTypeCannotBeChangedAfterCreation(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	channel := &model.Channel{
		Type:   constant.ChannelTypeOpenCodeGo,
		Name:   "OpenCode Go immutable type",
		Status: common.ChannelStatusAutoDisabled,
		Group:  "default",
	}
	require.NoError(t, db.Create(channel).Error)
	body := []byte(fmt.Sprintf(`{
		"id":%d,
		"type":1,
		"name":"converted",
		"key":"replacement-key",
		"models":"gpt-test",
		"group":"default"
	}`, channel.Id))
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/channel", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	UpdateChannel(ctx)

	assert.Contains(t, recorder.Body.String(), "OpenCode Go channel type cannot be changed")
	reloaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, constant.ChannelTypeOpenCodeGo, reloaded.Type)
	assert.Empty(t, reloaded.Key)
}

func TestEnablingEmptyOpenCodeGoChannelImmediatelyRestoresPoolDisablement(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(
		&model.OpenCodeGoIdentity{},
		&model.OpenCodeGoWorkspace{},
		&model.OpenCodeGoQuotaWindow{},
		&model.OpenCodeGoWorkspaceModel{},
		&model.OpenCodeGoOperation{},
	))
	channel := &model.Channel{
		Type:   constant.ChannelTypeOpenCodeGo,
		Name:   "OpenCode Go empty status",
		Status: common.ChannelStatusAutoDisabled,
		Group:  "default",
	}
	service.PrepareOpenCodeGoPoolContainer(channel)
	require.NoError(t, db.Create(channel).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.Id)}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/status", bytes.NewBufferString(`{"status":1}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	UpdateChannelStatus(ctx)

	reloaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusAutoDisabled, reloaded.Status)
	assert.Equal(t, "opencode_go:no_eligible_workspace", reloaded.GetOtherInfo()["status_reason"])
}

func TestResponsesCompactAPITypeSupport(t *testing.T) {
	tests := []struct {
		name    string
		apiType int
		want    bool
	}{
		{name: "OpenAI", apiType: constant.APITypeOpenAI, want: true},
		{name: "Codex", apiType: constant.APITypeCodex, want: true},
		{name: "Advanced Custom", apiType: constant.APITypeAdvancedCustom, want: true},
		{name: "Sub2API", apiType: constant.APITypeSub2API, want: true},
		{name: "New API", apiType: constant.APITypeNewAPI, want: true},
		{name: "OpenCode Go", apiType: constant.APITypeOpenCodeGo, want: false},
		{name: "Anthropic", apiType: constant.APITypeAnthropic, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, common.IsResponsesCompactAPIType(test.apiType))
		})
	}
}

func TestMultiprotocolGatewayEndpointTypes(t *testing.T) {
	want := []constant.EndpointType{
		constant.EndpointTypeOpenAI,
		constant.EndpointTypeOpenAIResponse,
		constant.EndpointTypeOpenAIResponseCompact,
		constant.EndpointTypeAnthropic,
		constant.EndpointTypeGemini,
		constant.EndpointTypeOpenAIAlphaSearch,
	}

	assert.Equal(t, want, common.GetEndpointTypesByChannelType(constant.ChannelTypeNewAPI, "gpt-5"))
	assert.Equal(t, want, common.GetEndpointTypesByChannelType(constant.ChannelTypeSub2API, "gpt-5"))
}

func TestCopyChannelRejectsInvalidLegacyProxySettings(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	settingBytes, err := common.Marshal(dto.ChannelSettings{
		Proxy: "socks5://proxy.example/legacy-path",
	})
	require.NoError(t, err)
	setting := string(settingBytes)
	origin := &model.Channel{
		Type:    constant.ChannelTypeOpenAI,
		Name:    "legacy proxy channel",
		Key:     "test-key",
		Models:  "gpt-test",
		Group:   "default",
		Setting: &setting,
	}
	require.NoError(t, db.Create(origin).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", origin.Id)}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/copy", nil)

	CopyChannel(ctx)

	assert.Contains(t, recorder.Body.String(), "invalid channel settings")
	var channelCount int64
	require.NoError(t, db.Model(&model.Channel{}).Count(&channelCount).Error)
	assert.Equal(t, int64(1), channelCount)
}

func TestDeleteChannelResetsProxyCacheWhenPreReadFails(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	service.ResetProxyClientCache()
	t.Cleanup(service.ResetProxyClientCache)

	proxyURL := "http://proxy.example:8080"
	beforeDelete, err := service.GetHttpClientWithProxy(proxyURL)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "999999"}}
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/channel/999999", nil)

	DeleteChannel(ctx)

	assert.Contains(t, recorder.Body.String(), `"success":true`)
	afterDelete, err := service.GetHttpClientWithProxy(proxyURL)
	require.NoError(t, err)
	assert.NotSame(t, beforeDelete, afterDelete)
}

func TestDeleteChannelBatchReportsAndAuditsActualDeletedCount(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	channel := &model.Channel{Name: "existing", Key: "test-key"}
	require.NoError(t, db.Create(channel).Error)

	requestBody, err := common.Marshal(ChannelBatch{Ids: []int{channel.Id, 999999}})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/channel/batch", bytes.NewReader(requestBody))
	ctx.Request.Header.Set("Content-Type", "application/json")

	DeleteChannelBatch(ctx)

	var response struct {
		Success bool  `json:"success"`
		Data    int64 `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.Equal(t, int64(1), response.Data)

	var auditLog model.Log
	require.NoError(t, db.Order("id desc").First(&auditLog).Error)
	var auditData struct {
		Operation struct {
			Params map[string]any `json:"params"`
		} `json:"op"`
	}
	require.NoError(t, common.UnmarshalJsonStr(auditLog.Other, &auditData))
	assert.Equal(t, float64(1), auditData.Operation.Params["count"])
}

func TestSettleTestQuotaUsesTieredBilling(t *testing.T) {
	info := &relaycommon.RelayInfo{
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:   "tiered_expr",
			ExprString:    `param("stream") == true ? tier("stream", p * 3) : tier("base", p * 2)`,
			ExprHash:      billingexpr.ExprHashString(`param("stream") == true ? tier("stream", p * 3) : tier("base", p * 2)`),
			GroupRatio:    1,
			EstimatedTier: "stream",
			QuotaPerUnit:  common.QuotaPerUnit,
			ExprVersion:   1,
		},
		BillingRequestInput: &billingexpr.RequestInput{
			Body: []byte(`{"stream":true}`),
		},
	}

	quota, result := settleTestQuota(info, types.PriceData{
		ModelRatio:      1,
		CompletionRatio: 2,
	}, &dto.Usage{
		PromptTokens: 1000,
	})

	require.Equal(t, 1500, quota)
	require.NotNil(t, result)
	require.Equal(t, "stream", result.MatchedTier)
}

func TestBuildTestLogOtherInjectsTieredInfo(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())

	info := &relaycommon.RelayInfo{
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode: "tiered_expr",
			ExprString:  `tier("base", p * 2)`,
		},
		ChannelMeta: &relaycommon.ChannelMeta{},
	}
	priceData := types.PriceData{
		GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1},
	}
	usage := &dto.Usage{
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 12,
		},
	}

	other := buildTestLogOther(ctx, info, priceData, usage, &billingexpr.TieredResult{
		MatchedTier: "base",
	})

	require.Equal(t, "tiered_expr", other["billing_mode"])
	require.Equal(t, "base", other["matched_tier"])
	require.NotEmpty(t, other["expr_b64"])
}

func TestResolveChannelTestUserIDUsesRequestUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("id", 2)

	userID, err := resolveChannelTestUserID(ctx)

	require.NoError(t, err)
	require.Equal(t, 2, userID)
}

func TestSelectChannelsForAutomaticTestPassiveRecoveryOnlyUsesAutoDisabled(t *testing.T) {
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusAutoDisabled},
		{Id: 3, Status: common.ChannelStatusManuallyDisabled},
	}

	selected := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModePassiveRecovery)

	require.Len(t, selected, 1)
	require.Equal(t, 2, selected[0].Id)
}

func TestSelectChannelsForAutomaticTestScheduledSkipsManualDisabled(t *testing.T) {
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusAutoDisabled},
		{Id: 3, Status: common.ChannelStatusManuallyDisabled},
	}

	selected := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModeScheduledAll)

	require.Len(t, selected, 2)
	require.Equal(t, 1, selected[0].Id)
	require.Equal(t, 2, selected[1].Id)
}

func TestTestAllChannelsRejectsExistingActiveTask(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}))

	existing, err := model.CreateSystemTask(model.SystemTaskTypeChannelTest, nil, nil)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/test", nil)

	TestAllChannels(ctx)

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Contains(t, recorder.Body.String(), existing.TaskID)
	require.Contains(t, recorder.Body.String(), "已有通道测试任务正在运行或等待中")
}
