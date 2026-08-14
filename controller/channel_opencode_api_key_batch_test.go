package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseOpenCodeAPIKeyBatch(t *testing.T) {
	entries, err := parseOpenCodeAPIKeyBatch(
		"  key-one  |  http://proxy.example:8080  \r\nkey-two|http://user:pa|ss@proxy-two.example:3128\r\n key-three ",
	)
	require.NoError(t, err)
	parsedProxy, err := common.ParseProxyURLStrict(entries[1].Proxy)
	require.NoError(t, err)
	password, ok := parsedProxy.User.Password()
	require.True(t, ok)
	assert.Equal(t, "pa|ss", password)
	require.Equal(t, []openCodeAPIKeyBatchEntry{
		{Key: "key-one", Proxy: "http://proxy.example:8080", Line: 1},
		{Key: "key-two", Proxy: "http://user:pa%7Css@proxy-two.example:3128", Line: 2},
		{Key: "key-three", Proxy: "", Line: 3},
	}, entries)
}

func TestParseOpenCodeAPIKeyBatchRejectsInvalidInputWithoutSecrets(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantMessage string
		secrets     []string
	}{
		{
			name:        "blank line",
			input:       "secret-one\n\nsecret-two",
			wantMessage: "line 2 is empty",
			secrets:     []string{"secret-one", "secret-two"},
		},
		{
			name:        "missing key",
			input:       " | http://proxy.example:8080",
			wantMessage: "line 1 is missing an API key",
			secrets:     []string{"proxy.example"},
		},
		{
			name:        "duplicate key",
			input:       "duplicate-secret\nother-secret\nduplicate-secret",
			wantMessage: "line 3 duplicates the API key from line 1",
			secrets:     []string{"duplicate-secret", "other-secret"},
		},
		{
			name:        "invalid proxy",
			input:       "api-secret | http://proxy-user:proxy-pass@private-proxy.example/path",
			wantMessage: "line 1 has an invalid proxy URL",
			secrets:     []string{"api-secret", "proxy-user", "proxy-pass", "private-proxy.example"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entries, err := parseOpenCodeAPIKeyBatch(test.input)
			require.Error(t, err)
			require.Nil(t, entries)
			assert.Contains(t, err.Error(), test.wantMessage)
			for _, secret := range test.secrets {
				assert.NotContains(t, err.Error(), secret)
			}
		})
	}
}

func TestParseOpenCodeAPIKeyBatchRejectsOversizedInputWithoutSecrets(t *testing.T) {
	secret := "never-return-this-oversized-secret"
	tests := []struct {
		name        string
		input       string
		wantMessage string
	}{
		{
			name:        "byte limit",
			input:       secret + strings.Repeat("x", openCodeAPIKeyBatchMaxBytes),
			wantMessage: "exceeds the 4194304 byte limit",
		},
		{
			name:        "line limit",
			input:       strings.Repeat("key\n", openCodeAPIKeyBatchMaxLines) + secret,
			wantMessage: "exceeds the 10000 line limit",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entries, err := parseOpenCodeAPIKeyBatch(test.input)
			require.Error(t, err)
			require.Nil(t, entries)
			assert.Contains(t, err.Error(), test.wantMessage)
			assert.NotContains(t, err.Error(), secret)
		})
	}
}

func TestBuildOpenCodeAPIKeyBatchChannelsDeepCopiesSettings(t *testing.T) {
	sharedProxy := "http://shared-proxy.example:9000"
	template := &model.Channel{
		Id:          99,
		Type:        constant.ChannelTypeOpenCodeAPIKey,
		Key:         "batch-input-placeholder",
		Name:        "OpenCode account",
		Models:      "model-a,model-b",
		Group:       "vip",
		Tag:         common.GetPointer("pool-tag"),
		Status:      common.ChannelStatusEnabled,
		Priority:    common.GetPointer(int64(12)),
		Weight:      common.GetPointer(uint(34)),
		ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2},
	}
	template.SetSetting(dto.ChannelSettings{
		Proxy:                 sharedProxy,
		HTTPProtocol:          dto.HTTPProtocolAuto,
		HTTP2ConnectionShards: 2,
	})

	channels, err := buildOpenCodeAPIKeyBatchChannels(template, []openCodeAPIKeyBatchEntry{
		{Key: "key-one", Proxy: "http://proxy-one.example:8080", Line: 1},
		{Key: "key-two", Proxy: "socks5h://proxy-two.example:1080", Line: 2},
		{Key: "key-three", Proxy: "", Line: 3},
	})
	require.NoError(t, err)
	require.Len(t, channels, 3)

	assert.Equal(t, []string{"OpenCode account 001", "OpenCode account 002", "OpenCode account 003"}, []string{
		channels[0].Name,
		channels[1].Name,
		channels[2].Name,
	})
	assert.Equal(t, []string{"key-one", "key-two", "key-three"}, []string{
		channels[0].Key,
		channels[1].Key,
		channels[2].Key,
	})
	assert.Equal(t, []string{"http://proxy-one.example:8080", "socks5h://proxy-two.example:1080", ""}, []string{
		channels[0].GetSetting().Proxy,
		channels[1].GetSetting().Proxy,
		channels[2].GetSetting().Proxy,
	})
	require.NotNil(t, channels[0].Setting)
	require.NotNil(t, channels[1].Setting)
	require.NotNil(t, channels[2].Setting)
	assert.NotSame(t, channels[0].Setting, channels[1].Setting)
	assert.NotSame(t, channels[1].Setting, channels[2].Setting)
	assert.NotSame(t, channels[0].Tag, channels[1].Tag)
	assert.NotSame(t, channels[0].Priority, channels[1].Priority)
	assert.NotSame(t, channels[0].Weight, channels[1].Weight)
	for _, channel := range channels {
		assert.Zero(t, channel.Id)
		assert.Equal(t, "vip", channel.Group)
		assert.Equal(t, "pool-tag", *channel.Tag)
		assert.Equal(t, "model-a,model-b", channel.Models)
		assert.Equal(t, dto.HTTPProtocolAuto, channel.GetSetting().HTTPProtocol)
		assert.Equal(t, 2, channel.GetSetting().HTTP2ConnectionShards)
		assert.False(t, channel.ChannelInfo.IsMultiKey)
	}

	firstSettings := channels[0].GetSetting()
	firstSettings.Proxy = "http://changed.example:8080"
	channels[0].SetSetting(firstSettings)
	assert.Equal(t, "socks5h://proxy-two.example:1080", channels[1].GetSetting().Proxy)
	assert.Equal(t, "", channels[2].GetSetting().Proxy)
	assert.Equal(t, sharedProxy, template.GetSetting().Proxy)
}

func TestAddOpenCodeAPIKeyBatchCreatesIndependentRows(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	sharedProxy := "http://must-not-be-inherited.example:8080"
	template := model.Channel{
		Name:     "OpenCode account",
		Type:     constant.ChannelTypeOpenCodeAPIKey,
		Key:      "key-one | http://proxy-one.example:8080\r\nkey-two\r\nkey-three | socks5://proxy-three.example:1080",
		Models:   "model-a,model-b",
		Group:    "vip",
		Tag:      common.GetPointer("pool-tag"),
		Status:   common.ChannelStatusEnabled,
		Priority: common.GetPointer(int64(12)),
		Weight:   common.GetPointer(uint(34)),
	}
	template.SetSetting(dto.ChannelSettings{
		Proxy:        sharedProxy,
		HTTPProtocol: dto.HTTPProtocolHTTP1,
	})

	response := addChannelForTest(t, AddChannelRequest{
		Mode:    openCodeAPIKeyBatchMode,
		Channel: &template,
	})
	require.True(t, response.Success, response.Message)

	var channels []model.Channel
	require.NoError(t, db.Where("type = ?", constant.ChannelTypeOpenCodeAPIKey).Order("id ASC").Find(&channels).Error)
	require.Len(t, channels, 3)
	assert.Equal(t, []string{"OpenCode account 001", "OpenCode account 002", "OpenCode account 003"}, []string{
		channels[0].Name,
		channels[1].Name,
		channels[2].Name,
	})
	assert.Equal(t, []string{"key-one", "key-two", "key-three"}, []string{
		channels[0].Key,
		channels[1].Key,
		channels[2].Key,
	})
	assert.Equal(t, []string{"http://proxy-one.example:8080", "", "socks5://proxy-three.example:1080"}, []string{
		channels[0].GetSetting().Proxy,
		channels[1].GetSetting().Proxy,
		channels[2].GetSetting().Proxy,
	})
	for _, channel := range channels {
		assert.Equal(t, "vip", channel.Group)
		assert.Equal(t, "pool-tag", *channel.Tag)
		assert.Equal(t, dto.HTTPProtocolHTTP1, channel.GetSetting().HTTPProtocol)
	}

	var abilities []model.Ability
	require.NoError(t, db.Order("channel_id ASC, model ASC").Find(&abilities).Error)
	require.Len(t, abilities, 6)

	var auditLog model.Log
	require.NoError(t, db.Where("type = ?", model.LogTypeManage).Last(&auditLog).Error)
	var auditOther struct {
		Op struct {
			Action string         `json:"action"`
			Params map[string]any `json:"params"`
		} `json:"op"`
	}
	require.NoError(t, common.Unmarshal([]byte(auditLog.Other), &auditOther))
	assert.Equal(t, "channel.create", auditOther.Op.Action)
	assert.Equal(t, map[string]any{
		"count": float64(3),
		"tag":   "pool-tag",
		"type":  float64(constant.ChannelTypeOpenCodeAPIKey),
	}, auditOther.Op.Params)
}

func TestAddOpenCodeAPIKeyBatchValidationIsAtomicAndSecretFree(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	secretKey := "never-return-this-api-key"
	secretProxy := "private-proxy.example"
	template := model.Channel{
		Name:   "OpenCode account",
		Type:   constant.ChannelTypeOpenCodeAPIKey,
		Key:    "valid-key | http://proxy.example:8080\n" + secretKey + " | http://proxy-user:proxy-pass@" + secretProxy + "/path",
		Models: "model-a",
		Group:  "default",
		Status: common.ChannelStatusEnabled,
	}

	response := addChannelForTest(t, AddChannelRequest{
		Mode:    openCodeAPIKeyBatchMode,
		Channel: &template,
	})
	require.False(t, response.Success)
	assert.Contains(t, response.Message, "line 2 has an invalid proxy URL")
	assert.NotContains(t, response.Message, secretKey)
	assert.NotContains(t, response.Message, secretProxy)
	assert.NotContains(t, response.Message, "proxy-user")
	assert.NotContains(t, response.Message, "proxy-pass")

	var channelCount int64
	require.NoError(t, db.Model(&model.Channel{}).Count(&channelCount).Error)
	assert.Zero(t, channelCount)
	var abilityCount int64
	require.NoError(t, db.Model(&model.Ability{}).Count(&abilityCount).Error)
	assert.Zero(t, abilityCount)
}

func TestAddOpenCodeAPIKeyRejectsUnsupportedModes(t *testing.T) {
	tests := []string{"multi_to_single", "batch"}
	for _, mode := range tests {
		t.Run(mode, func(t *testing.T) {
			db := setupModelListControllerTestDB(t)
			response := addChannelForTest(t, AddChannelRequest{
				Mode: mode,
				Channel: &model.Channel{
					Name:   "OpenCode account",
					Type:   constant.ChannelTypeOpenCodeAPIKey,
					Key:    "key-one\nkey-two",
					Models: "model-a",
					Group:  "default",
					Status: common.ChannelStatusEnabled,
				},
			})
			require.False(t, response.Success)
			assert.Contains(t, response.Message, "only support single or opencode_api_key_batch")

			var count int64
			require.NoError(t, db.Model(&model.Channel{}).Count(&count).Error)
			assert.Zero(t, count)
		})
	}
}

func TestAddOpenCodeAPIKeySingleRejectsMultipleKeys(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	response := addChannelForTest(t, AddChannelRequest{
		Mode: "single",
		Channel: &model.Channel{
			Name:   "OpenCode account",
			Type:   constant.ChannelTypeOpenCodeAPIKey,
			Key:    "key-one\nkey-two",
			Models: "model-a",
			Group:  "default",
			Status: common.ChannelStatusEnabled,
		},
	})
	require.False(t, response.Success)
	assert.Contains(t, response.Message, "exactly one API key per row")

	var count int64
	require.NoError(t, db.Model(&model.Channel{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestValidateOpenCodeAPIKeyEditAllowsOmittedKey(t *testing.T) {
	channel := &model.Channel{
		Type:   constant.ChannelTypeOpenCodeAPIKey,
		Key:    "",
		Models: "model-a",
		Group:  "default",
	}

	require.NoError(t, validateChannel(channel, false))
}

func TestUpdateOpenCodeAPIKeyNormalizesLegacyMultiKeyMetadata(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	legacy := &model.Channel{
		Type:   constant.ChannelTypeOpenCodeAPIKey,
		Name:   "legacy OpenCode API key",
		Key:    "legacy-key",
		Models: "model-a",
		Group:  "default",
		Status: common.ChannelStatusEnabled,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:         true,
			MultiKeySize:       1,
			MultiKeyMode:       constant.MultiKeyModePolling,
			MultiKeyStatusList: map[int]int{0: common.ChannelStatusEnabled},
		},
	}
	require.NoError(t, legacy.Insert())

	body, err := common.Marshal(map[string]any{
		"id":     legacy.Id,
		"type":   constant.ChannelTypeOpenCodeAPIKey,
		"name":   legacy.Name,
		"models": legacy.Models,
		"group":  legacy.Group,
	})
	require.NoError(t, err)
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

	reloaded, err := model.GetChannelById(legacy.Id, true)
	require.NoError(t, err)
	assert.False(t, reloaded.ChannelInfo.IsMultiKey)
	assert.Zero(t, reloaded.ChannelInfo.MultiKeySize)
	assert.Equal(t, "legacy-key", reloaded.Key)
}

func TestManageMultiKeysRejectsOpenCodeAPIKeyResidualMultiKeyMetadata(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	legacy := &model.Channel{
		Type:   constant.ChannelTypeOpenCodeAPIKey,
		Name:   "legacy OpenCode API key",
		Key:    "row-key-one\nrow-key-two",
		Models: "model-a",
		Group:  "default",
		Status: common.ChannelStatusEnabled,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:   true,
			MultiKeySize: 2,
		},
	}
	require.NoError(t, db.Create(legacy).Error)

	body, err := common.Marshal(MultiKeyManageRequest{
		ChannelId: legacy.Id,
		Action:    "get_key_status",
	})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/manage-multi-key", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	ManageMultiKeys(ctx)

	response := addChannelTestResponse{}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.False(t, response.Success)
	assert.Contains(t, response.Message, "不支持多密钥管理")
	assert.NotContains(t, recorder.Body.String(), "row-key-one")
	assert.NotContains(t, recorder.Body.String(), "row-key-two")
}

func TestValidateOpenCodeAPIKeyUsesFixedBaseURL(t *testing.T) {
	tests := []struct {
		name        string
		baseURL     *string
		wantErrText string
	}{
		{name: "implicit fixed URL"},
		{name: "explicit fixed URL", baseURL: common.GetPointer("https://opencode.ai/zen/go/v1/")},
		{name: "custom URL rejected", baseURL: common.GetPointer("https://private-upstream.example/v1"), wantErrText: "base URL is fixed"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			channel := &model.Channel{
				Type:    constant.ChannelTypeOpenCodeAPIKey,
				Key:     "single-key",
				BaseURL: test.baseURL,
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

func TestAddOpenCodeAPIKeySingleKeepsOrdinaryProxy(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	channel := &model.Channel{
		Name:   "OpenCode single",
		Type:   constant.ChannelTypeOpenCodeAPIKey,
		Key:    "single-key",
		Models: "model-a",
		Group:  "default",
		Status: common.ChannelStatusEnabled,
	}
	channel.SetSetting(dto.ChannelSettings{Proxy: "https://single-proxy.example:8443"})

	response := addChannelForTest(t, AddChannelRequest{Mode: "single", Channel: channel})
	require.True(t, response.Success, response.Message)

	var stored model.Channel
	require.NoError(t, db.Where("type = ?", constant.ChannelTypeOpenCodeAPIKey).First(&stored).Error)
	assert.Equal(t, "single-key", stored.Key)
	assert.Equal(t, "OpenCode single", stored.Name)
	assert.Equal(t, "https://single-proxy.example:8443", stored.GetSetting().Proxy)
}

func TestAddOpenCodeGoStillSupportsOnlySingleEmptyKeyCreation(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	response := addChannelForTest(t, AddChannelRequest{
		Mode: "batch",
		Channel: &model.Channel{
			Name:   "OpenCode Go pool",
			Type:   constant.ChannelTypeOpenCodeGo,
			Key:    "",
			Models: "model-a",
			Group:  "default",
			Status: common.ChannelStatusEnabled,
		},
	})
	require.False(t, response.Success)
	assert.Contains(t, response.Message, "only support single creation")

	var count int64
	require.NoError(t, db.Model(&model.Channel{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestAddChannelGenericBatchRemainsSupported(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	response := addChannelForTest(t, AddChannelRequest{
		Mode: "batch",
		Channel: &model.Channel{
			Name:   "Generic batch",
			Type:   constant.ChannelTypeOpenAI,
			Key:    "generic-key-one\ngeneric-key-two",
			Models: "model-a",
			Group:  "default",
			Status: common.ChannelStatusEnabled,
		},
	})
	require.True(t, response.Success, response.Message)

	var channels []model.Channel
	require.NoError(t, db.Where("type = ?", constant.ChannelTypeOpenAI).Order("id ASC").Find(&channels).Error)
	require.Len(t, channels, 2)
	assert.Equal(t, []string{"generic-key-one", "generic-key-two"}, []string{channels[0].Key, channels[1].Key})
	assert.Equal(t, []string{"Generic batch", "Generic batch"}, []string{channels[0].Name, channels[1].Name})
}

type addChannelTestResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

func addChannelForTest(t *testing.T, request AddChannelRequest) addChannelTestResponse {
	t.Helper()
	body, err := common.Marshal(request)
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	AddChannel(ctx)

	response := addChannelTestResponse{}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response), strings.TrimSpace(recorder.Body.String()))
	return response
}
