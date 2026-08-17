package controller

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestOpenCodeUnsupportedOptionalFieldPolicyCreateAndUpdate(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		channelType := channelType
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			db := setupOpenCodePolicyControllerDB(t)
			channel := newOpenCodePolicyTestChannel(t, channelType, dto.OpenCodeGoUnsupportedOptionalFieldDropKnown)

			response := addChannelForTest(t, AddChannelRequest{Mode: "single", Channel: channel})
			require.True(t, response.Success, response.Message)

			stored := loadOnlyOpenCodePolicyChannel(t, db, channelType)
			assertOpenCodePolicy(t, stored, dto.OpenCodeGoUnsupportedOptionalFieldDropKnown)

			invalidSettings := openCodePolicySettingsJSON(t, "ignore_all_optional_fields")
			updateResponse := updateOpenCodePolicyChannelForTest(t, stored, invalidSettings)
			require.False(t, updateResponse.Success)
			assert.Contains(t, updateResponse.Message, "unsupported_optional_field_policy")
			unchanged, err := model.GetChannelById(stored.Id, true)
			require.NoError(t, err)
			assertOpenCodePolicy(t, unchanged, dto.OpenCodeGoUnsupportedOptionalFieldDropKnown)

			strictSettings := openCodePolicySettingsJSON(t, dto.OpenCodeGoUnsupportedOptionalFieldStrict)
			updateResponse = updateOpenCodePolicyChannelForTest(t, stored, strictSettings)
			require.True(t, updateResponse.Success, updateResponse.Message)
			updated, err := model.GetChannelById(stored.Id, true)
			require.NoError(t, err)
			assertOpenCodePolicy(t, updated, dto.OpenCodeGoUnsupportedOptionalFieldStrict)
		})
	}
}

func TestOpenCodeUnsupportedOptionalFieldPolicyCreateRejectsInvalidShapes(t *testing.T) {
	invalidValues := []struct {
		name  string
		value any
	}{
		{name: "null", value: nil},
		{name: "empty", value: ""},
		{name: "boolean", value: true},
		{name: "number", value: 1},
		{name: "object", value: map[string]any{}},
		{name: "array", value: []any{}},
		{name: "unknown", value: "ignore_all_optional_fields"},
	}
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, invalid := range invalidValues {
			t.Run(fmt.Sprintf("type-%d/%s", channelType, invalid.name), func(t *testing.T) {
				db := setupOpenCodePolicyControllerDB(t)
				channel := newOpenCodePolicyTestChannel(t, channelType, invalid.value)

				response := addChannelForTest(t, AddChannelRequest{Mode: "single", Channel: channel})

				require.False(t, response.Success)
				assert.Contains(t, response.Message, "unsupported_optional_field_policy")
				var count int64
				require.NoError(t, db.Model(&model.Channel{}).Count(&count).Error)
				assert.Zero(t, count)
			})
		}
	}
}

func TestOpenCodeUnsupportedOptionalFieldPolicyCopyValidation(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		channelType := channelType
		t.Run(fmt.Sprintf("valid/type-%d", channelType), func(t *testing.T) {
			db := setupOpenCodePolicyControllerDB(t)
			origin := newOpenCodePolicyTestChannel(t, channelType, dto.OpenCodeGoUnsupportedOptionalFieldDropKnown)
			require.NoError(t, db.Create(origin).Error)

			response := copyOpenCodePolicyChannelForTest(t, origin.Id)

			require.True(t, response.Success, response.Message)
			clone, err := model.GetChannelById(response.Data.ID, true)
			require.NoError(t, err)
			assertOpenCodePolicy(t, clone, dto.OpenCodeGoUnsupportedOptionalFieldDropKnown)
		})

		t.Run(fmt.Sprintf("invalid/type-%d", channelType), func(t *testing.T) {
			db := setupOpenCodePolicyControllerDB(t)
			origin := newOpenCodePolicyTestChannel(t, channelType, "ignore_all_optional_fields")
			require.NoError(t, db.Create(origin).Error)

			response := copyOpenCodePolicyChannelForTest(t, origin.Id)

			require.False(t, response.Success)
			assert.Equal(t, "Failed to copy channel: invalid channel settings", response.Message)
			assert.NotContains(t, response.Message, "ignore_all_optional_fields")
			var count int64
			require.NoError(t, db.Model(&model.Channel{}).Count(&count).Error)
			assert.Equal(t, int64(1), count)
		})
	}
}

func TestOpenCodeAPIKeyBatchClonesUnsupportedOptionalFieldPolicy(t *testing.T) {
	t.Run("valid policy reaches every row", func(t *testing.T) {
		db := setupOpenCodePolicyControllerDB(t)
		template := newOpenCodePolicyTestChannel(
			t,
			constant.ChannelTypeOpenCodeAPIKey,
			dto.OpenCodeGoUnsupportedOptionalFieldDropKnown,
		)
		template.Key = "batch-key-one\nbatch-key-two"

		response := addChannelForTest(t, AddChannelRequest{
			Mode:    openCodeAPIKeyBatchMode,
			Channel: template,
		})

		require.True(t, response.Success, response.Message)
		var channels []model.Channel
		require.NoError(t, db.Where("type = ?", constant.ChannelTypeOpenCodeAPIKey).Order("id ASC").Find(&channels).Error)
		require.Len(t, channels, 2)
		for index := range channels {
			assertOpenCodePolicy(t, &channels[index], dto.OpenCodeGoUnsupportedOptionalFieldDropKnown)
		}
	})

	t.Run("invalid policy rejects the batch atomically", func(t *testing.T) {
		db := setupOpenCodePolicyControllerDB(t)
		template := newOpenCodePolicyTestChannel(
			t,
			constant.ChannelTypeOpenCodeAPIKey,
			"ignore_all_optional_fields",
		)
		template.Key = "batch-key-one\nbatch-key-two"

		response := addChannelForTest(t, AddChannelRequest{
			Mode:    openCodeAPIKeyBatchMode,
			Channel: template,
		})

		require.False(t, response.Success)
		assert.Contains(t, response.Message, "unsupported_optional_field_policy")
		var count int64
		require.NoError(t, db.Model(&model.Channel{}).Count(&count).Error)
		assert.Zero(t, count)
	})
}

func setupOpenCodePolicyControllerDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(
		&model.Log{},
		&model.OpenCodeGoIdentity{},
		&model.OpenCodeGoWorkspace{},
		&model.OpenCodeGoQuotaWindow{},
		&model.OpenCodeGoWorkspaceModel{},
		&model.OpenCodeGoOperation{},
	))
	return db
}

func newOpenCodePolicyTestChannel(t *testing.T, channelType int, policy any) *model.Channel {
	t.Helper()
	key := ""
	if channelType == constant.ChannelTypeOpenCodeAPIKey {
		key = "single-api-key"
	}
	return &model.Channel{
		Name:          fmt.Sprintf("OpenCode policy type %d", channelType),
		Type:          channelType,
		Key:           key,
		Models:        "glm-5.2",
		Group:         "default",
		Status:        common.ChannelStatusEnabled,
		OtherSettings: openCodePolicySettingsJSON(t, policy),
	}
}

func openCodePolicySettingsJSON(t *testing.T, policy any) string {
	t.Helper()
	encoded, err := common.Marshal(map[string]any{
		"opencode_go": map[string]any{
			"unsupported_optional_field_policy": policy,
		},
	})
	require.NoError(t, err)
	return string(encoded)
}

func loadOnlyOpenCodePolicyChannel(t *testing.T, db *gorm.DB, channelType int) *model.Channel {
	t.Helper()
	var channel model.Channel
	require.NoError(t, db.Where("type = ?", channelType).First(&channel).Error)
	return &channel
}

func assertOpenCodePolicy(t *testing.T, channel *model.Channel, want string) {
	t.Helper()
	settings, err := channel.DecodeOtherSettings()
	require.NoError(t, err)
	require.NotNil(t, settings.OpenCodeGo)
	assert.Equal(t, want, settings.OpenCodeGo.EffectiveUnsupportedOptionalFieldPolicy())
}

func updateOpenCodePolicyChannelForTest(
	t *testing.T,
	channel *model.Channel,
	settings string,
) addChannelTestResponse {
	t.Helper()
	body, err := common.Marshal(map[string]any{
		"id":       channel.Id,
		"type":     channel.Type,
		"name":     channel.Name,
		"models":   channel.Models,
		"group":    channel.Group,
		"settings": settings,
	})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("role", common.RoleRootUser)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/channel", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	UpdateChannel(ctx)

	response := addChannelTestResponse{}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response), recorder.Body.String())
	return response
}

type copyOpenCodePolicyResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    struct {
		ID int `json:"id"`
	} `json:"data"`
}

func copyOpenCodePolicyChannelForTest(t *testing.T, channelID int) copyOpenCodePolicyResponse {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channelID)}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/copy", nil)
	CopyChannel(ctx)

	response := copyOpenCodePolicyResponse{}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response), recorder.Body.String())
	return response
}
