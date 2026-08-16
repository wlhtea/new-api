package model

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestFormatUserLogsStripsQuotaSaturation verifies the admin-only quota
// saturation marker (nested under other.admin_info) is removed for non-admin
// log views, since formatUserLogs strips the whole admin_info object.
func TestFormatUserLogsStripsQuotaSaturation(t *testing.T) {
	other := common.MapToJsonStr(map[string]interface{}{
		"model_price": 0.004,
		"admin_info": map[string]interface{}{
			"quota_saturation": map[string]interface{}{
				"op":      "QuotaFromDecimal",
				"kind":    "overflow",
				"clamped": common.MaxQuota,
			},
		},
	})
	logs := []*Log{{Other: other}}

	formatUserLogs(logs, 0)

	parsed, err := common.StrToMap(logs[0].Other)
	require.NoError(t, err)
	_, hasAdminInfo := parsed["admin_info"]
	require.False(t, hasAdminInfo, "admin_info (and nested quota_saturation) must be stripped for non-admin views")
	// Non-admin billing fields remain visible.
	require.Contains(t, parsed, "model_price")
}

func TestFormatUserLogsStripsOpenCodeGoAffinityFields(t *testing.T) {
	logs := []*Log{{Other: common.MapToJsonStr(map[string]interface{}{
		"model_price":                 0.004,
		"opencode_go_affinity_source": "top-level-source",
		"opencode_go_workspace_uid":   "top-level-workspace",
		"opencode_go_affinity_key":    "top-level-private-key",
		"admin_info": map[string]interface{}{
			"opencode_go_affinity_source": "claude-code-session",
			"opencode_go_workspace_uid":   "workspace_0123456789abcdef",
			"opencode_go_affinity_key":    "nested-private-key",
			"caller_ip":                   "192.0.2.1",
		},
	})}}

	formatUserLogs(logs, 0)

	parsed, err := common.StrToMap(logs[0].Other)
	require.NoError(t, err)
	require.NotContains(t, parsed, "opencode_go_affinity_source")
	require.NotContains(t, parsed, "opencode_go_workspace_uid")
	require.NotContains(t, parsed, "opencode_go_affinity_key")
	require.NotContains(t, parsed, "admin_info")
	require.Equal(t, 0.004, parsed["model_price"])
}

func TestFormatUserLogsToleratesMissingOrMalformedAffinityData(t *testing.T) {
	logs := []*Log{
		{Other: ""},
		{Other: "not-json"},
		{Other: `{"admin_info":"not-an-object"}`},
		{Other: `{}`},
	}

	require.NotPanics(t, func() {
		formatUserLogs(logs, 0)
	})
	for i, log := range logs {
		require.Equal(t, i+1, log.Id)
	}
}

func TestFormatUserLogsHidesOpenCodeGoErrorDetails(t *testing.T) {
	logs := []*Log{{
		Type:              LogTypeError,
		Content:           "status_code=503, Error from provider (Console Go): workspace wrk_private is unavailable",
		ChannelId:         72,
		ChannelName:       "OpenCode Go pool",
		UpstreamRequestId: "wrk_upstream_private",
		Other: common.MapToJsonStr(map[string]interface{}{
			"error_type":        "upstream_error",
			"error_code":        "workspace_unavailable",
			"status_code":       503,
			"channel_id":        72,
			"channel_name":      "OpenCode Go pool",
			"channel_type":      constant.ChannelTypeOpenCodeGo,
			"request_path":      "/v1/responses",
			"private_extension": "workspace_secret",
			"admin_info": map[string]interface{}{
				"opencode_go_workspace_uid": "wrk_private",
			},
		}),
	}}

	formatUserLogs(logs, 0)

	require.Equal(t, constant.OpenCodeGoPublicOverloadMessage, logs[0].Content)
	require.Zero(t, logs[0].ChannelId)
	require.Empty(t, logs[0].ChannelName)
	require.Empty(t, logs[0].UpstreamRequestId)
	parsed, err := common.StrToMap(logs[0].Other)
	require.NoError(t, err)
	require.Equal(t, constant.OpenCodeGoPublicRateLimitErrorCode, parsed["error_type"])
	require.Equal(t, constant.OpenCodeGoPublicRateLimitErrorCode, parsed["error_code"])
	require.Equal(t, float64(429), parsed["status_code"])
	require.Len(t, parsed, 3, "the public error metadata must use an allowlist")
	publicView := strings.ToLower(logs[0].Content + logs[0].ChannelName + logs[0].Other)
	for _, marker := range []string{"opencode", "console go", "workspace", "wrk_"} {
		require.NotContains(t, publicView, marker)
	}
}

func TestFormatUserLogsPreservesOtherChannelErrors(t *testing.T) {
	logs := []*Log{{
		Type:      LogTypeError,
		Content:   "workspace is unavailable",
		ChannelId: 7,
		Other: common.MapToJsonStr(map[string]interface{}{
			"channel_type": constant.ChannelTypeOpenAI,
			"channel_id":   7,
		}),
	}}

	formatUserLogs(logs, 0)

	require.Equal(t, "workspace is unavailable", logs[0].Content)
	require.Equal(t, 7, logs[0].ChannelId)
	parsed, err := common.StrToMap(logs[0].Other)
	require.NoError(t, err)
	require.Equal(t, float64(constant.ChannelTypeOpenAI), parsed["channel_type"])
}

func TestFormatUserLogsPreservesUnclassifiedGenericChannelError(t *testing.T) {
	log := &Log{
		Type:      LogTypeError,
		Content:   "upstream request failed",
		ChannelId: 7,
		Other:     `{"channel_id":7}`,
	}

	formatUserLogs([]*Log{log}, 0)

	require.Equal(t, "upstream request failed", log.Content)
	require.Equal(t, 7, log.ChannelId)
	require.JSONEq(t, `{"channel_id":7}`, log.Other)
}

func TestFormatUserLogsHidesOpenCodeGoUpstreamRequestIdOnConsumeLogs(t *testing.T) {
	raw := Log{
		Type:              LogTypeConsume,
		Content:           "request completed",
		ChannelId:         72,
		UpstreamRequestId: "internal-endpoint-request-id",
		Other: common.MapToJsonStr(map[string]interface{}{
			"model_price": 0.004,
			"admin_info": map[string]interface{}{
				"channel_type": constant.ChannelTypeOpenCodeGo,
			},
		}),
	}
	public := raw

	formatUserLogs([]*Log{&public}, 0)

	require.Empty(t, public.UpstreamRequestId)
	require.Equal(t, "internal-endpoint-request-id", raw.UpstreamRequestId)
	require.Equal(t, "request completed", public.Content)
	require.Equal(t, 72, public.ChannelId)
	parsed, err := common.StrToMap(public.Other)
	require.NoError(t, err)
	require.Equal(t, 0.004, parsed["model_price"])
	require.NotContains(t, parsed, "channel_type")
	require.NotContains(t, parsed, "admin_info")
}

func TestFormatUserLogsResolvesHistoricalOpenCodeGoConsumeChannel(t *testing.T) {
	previousDB := DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	DB = db
	t.Cleanup(func() { DB = previousDB })
	require.NoError(t, db.AutoMigrate(&Channel{}))
	require.NoError(t, db.Create(&Channel{Id: 72, Type: constant.ChannelTypeOpenCodeGo, Key: ""}).Error)

	log := &Log{
		Type:              LogTypeConsume,
		ChannelId:         72,
		UpstreamRequestId: "historical-private-request-id",
		Other:             `{"model_price":0.004}`,
	}

	formatUserLogs([]*Log{log}, 0)

	require.Empty(t, log.UpstreamRequestId)
}

func TestFormatUserLogsFailsClosedForUnclassifiedHistoricalRequestId(t *testing.T) {
	previousDB := DB
	DB = nil
	t.Cleanup(func() { DB = previousDB })
	log := &Log{
		Type:              LogTypeConsume,
		ChannelId:         72,
		UpstreamRequestId: "historical-private-request-id",
		Other:             `{"model_price":0.004}`,
	}

	formatUserLogs([]*Log{log}, 0)

	require.Empty(t, log.UpstreamRequestId)
}

func TestFormatUserLogsPreservesExplicitOtherChannelRequestId(t *testing.T) {
	log := &Log{
		Type:              LogTypeConsume,
		ChannelId:         7,
		UpstreamRequestId: "other-provider-request-id",
		Other: common.MapToJsonStr(map[string]interface{}{
			"channel_type": constant.ChannelTypeOpenAI,
		}),
	}

	formatUserLogs([]*Log{log}, 0)

	require.Equal(t, "other-provider-request-id", log.UpstreamRequestId)
}

func TestUserLogChannelTypeUsesTopLevelBeforeAdminInfo(t *testing.T) {
	channelType, ok := userLogChannelType(map[string]interface{}{
		"channel_type": constant.ChannelTypeOpenAI,
		"admin_info": map[string]interface{}{
			"channel_type": constant.ChannelTypeOpenCodeGo,
		},
	})

	require.True(t, ok)
	require.Equal(t, constant.ChannelTypeOpenAI, channelType)
}

func TestUserLogChannelTypeFallsBackToAdminInfo(t *testing.T) {
	channelType, ok := userLogChannelType(map[string]interface{}{
		"admin_info": map[string]interface{}{
			"channel_type": constant.ChannelTypeOpenCodeGo,
		},
	})

	require.True(t, ok)
	require.Equal(t, constant.ChannelTypeOpenCodeGo, channelType)
}

func TestUserLogChannelTypeFallsBackFromMalformedTopLevel(t *testing.T) {
	channelType, ok := userLogChannelType(map[string]interface{}{
		"channel_type": "private-workspace",
		"admin_info": map[string]interface{}{
			"channel_type": constant.ChannelTypeOpenCodeGo,
		},
	})

	require.True(t, ok)
	require.Equal(t, constant.ChannelTypeOpenCodeGo, channelType)
}

func TestFormatUserLogsHidesPrivateErrorWhenChannelMetadataIsMalformed(t *testing.T) {
	tests := []struct {
		name  string
		other string
	}{
		{name: "missing", other: ""},
		{name: "malformed", other: "not-json"},
		{name: "null", other: "null"},
		{name: "invalid channel type", other: `{"channel_type":"private-workspace"}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			log := &Log{
				Type:              LogTypeError,
				Content:           "setup requestheader failed: OpenCode Go workspace wrk_private is unavailable",
				ChannelId:         72,
				ChannelName:       "private pool",
				UpstreamRequestId: "wrk_upstream_private",
				Other:             test.other,
			}

			formatUserLogs([]*Log{log}, 0)

			require.Equal(t, constant.OpenCodeGoPublicOverloadMessage, log.Content)
			require.Zero(t, log.ChannelId)
			require.Empty(t, log.ChannelName)
			require.Empty(t, log.UpstreamRequestId)
			parsed, err := common.StrToMap(log.Other)
			require.NoError(t, err)
			require.Equal(t, map[string]interface{}{
				"error_type":  constant.OpenCodeGoPublicRateLimitErrorCode,
				"error_code":  constant.OpenCodeGoPublicRateLimitErrorCode,
				"status_code": float64(429),
			}, parsed)
		})
	}
}

func TestFormatUserLogsPreservesRawOpenCodeGoErrorRecord(t *testing.T) {
	raw := Log{
		Type:              LogTypeError,
		Content:           "Console Go workspace wrk_private is unavailable",
		ChannelId:         72,
		ChannelName:       "OpenCode Go pool",
		UpstreamRequestId: "wrk_upstream_private",
		Other: common.MapToJsonStr(map[string]interface{}{
			"channel_type": constant.ChannelTypeOpenCodeGo,
			"private_key":  "workspace_secret",
		}),
	}
	public := raw

	formatUserLogs([]*Log{&public}, 0)

	require.Equal(t, "Console Go workspace wrk_private is unavailable", raw.Content)
	require.Equal(t, 72, raw.ChannelId)
	require.Equal(t, "OpenCode Go pool", raw.ChannelName)
	require.Equal(t, "wrk_upstream_private", raw.UpstreamRequestId)
	require.Contains(t, raw.Other, "workspace_secret")
	require.NotEqual(t, raw, public)
}

func TestFormatUserLogsProjectsOpenCodeAPIKeyWithoutMutatingStoredRecord(t *testing.T) {
	raw := Log{
		Type:              LogTypeError,
		Content:           "Console Go endpoint failed for workspace wrk_private",
		ChannelId:         81,
		ChannelName:       "OpenCode API Key account",
		UpstreamRequestId: "upstream-private-request",
		Other: common.MapToJsonStr(map[string]interface{}{
			"channel_type": constant.ChannelTypeOpenCodeAPIKey,
			"error_type":   "upstream_error",
			"error_code":   "server_error",
			"status_code":  http.StatusBadGateway,
			"admin_info": map[string]interface{}{
				"proxy": "socks5://private.example:1080",
			},
		}),
	}
	public := raw

	formatUserLogs([]*Log{&public}, 0)

	require.Equal(t, constant.OpenCodeGoPublicOverloadMessage, public.Content)
	require.Zero(t, public.ChannelId)
	require.Empty(t, public.ChannelName)
	require.Empty(t, public.UpstreamRequestId)
	publicOther, err := common.StrToMap(public.Other)
	require.NoError(t, err)
	require.Equal(t, map[string]interface{}{
		"error_type":  constant.OpenCodeGoPublicRateLimitErrorCode,
		"error_code":  constant.OpenCodeGoPublicRateLimitErrorCode,
		"status_code": float64(http.StatusTooManyRequests),
	}, publicOther)

	require.Contains(t, raw.Content, "Console Go")
	require.Equal(t, 81, raw.ChannelId)
	require.Equal(t, "upstream-private-request", raw.UpstreamRequestId)
	require.Contains(t, raw.Other, "private.example")
}

func TestFormatUserLogsFailClosedWithoutRawUpstreamProvenance(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		errorCode   string
		wantStatus  int
		wantCode    string
		wantContent string
	}{
		{
			name:        "safe explicit client error",
			content:     "status_code=400, Error from provider (Console Go): Upstream request failed: [invalid_request_error] messages[0].role is required",
			errorCode:   "invalid_request_error",
			wantStatus:  http.StatusTooManyRequests,
			wantCode:    constant.OpenCodeGoPublicRateLimitErrorCode,
			wantContent: constant.OpenCodeGoPublicOverloadMessage,
		},
		{
			name:        "private explicit client error",
			content:     "status_code=400, invalid request for workspace wrk_private",
			errorCode:   "invalid_request_error",
			wantStatus:  http.StatusTooManyRequests,
			wantCode:    constant.OpenCodeGoPublicRateLimitErrorCode,
			wantContent: constant.OpenCodeGoPublicOverloadMessage,
		},
		{
			name:        "credential explicit client error",
			content:     "status_code=400, invalid request: Authorization: Bearer private-upstream-token; proxy socks5://proxy-user:proxy-password@10.0.0.8:1080",
			errorCode:   "invalid_request_error",
			wantStatus:  http.StatusTooManyRequests,
			wantCode:    constant.OpenCodeGoPublicRateLimitErrorCode,
			wantContent: constant.OpenCodeGoPublicOverloadMessage,
		},
		{
			name:        "ambiguous upstream 400",
			content:     "status_code=400, operator-managed credential was rejected",
			errorCode:   "upstream_error",
			wantStatus:  http.StatusTooManyRequests,
			wantCode:    constant.OpenCodeGoPublicRateLimitErrorCode,
			wantContent: constant.OpenCodeGoPublicOverloadMessage,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			log := &Log{
				Type:      LogTypeError,
				Content:   test.content,
				ChannelId: 81,
				Other: common.MapToJsonStr(map[string]interface{}{
					"channel_type": constant.ChannelTypeOpenCodeAPIKey,
					"error_type":   "openai_error",
					"error_code":   test.errorCode,
					"status_code":  http.StatusBadRequest,
				}),
			}

			formatUserLogs([]*Log{log}, 0)

			require.Equal(t, test.wantContent, log.Content)
			publicOther, err := common.StrToMap(log.Other)
			require.NoError(t, err)
			require.Equal(t, float64(test.wantStatus), publicOther["status_code"])
			require.Equal(t, test.wantCode, publicOther["error_type"])
			require.Equal(t, test.wantCode, publicOther["error_code"])
		})
	}
}

func TestFormatUserLogsUsesRawUpstreamStatusForOpenCodeClientProjection(t *testing.T) {
	tests := []struct {
		name               string
		upstreamStatusCode int
		wantStatus         int
		wantCode           string
	}{
		{name: "raw 400 uses fixed client detail", upstreamStatusCode: http.StatusBadRequest, wantStatus: http.StatusBadRequest, wantCode: constant.OpenCodeGoPublicInvalidRequestCode},
		{name: "raw 422 uses fixed client detail", upstreamStatusCode: http.StatusUnprocessableEntity, wantStatus: http.StatusBadRequest, wantCode: constant.OpenCodeGoPublicInvalidRequestCode},
		{name: "raw 401 fails closed", upstreamStatusCode: http.StatusUnauthorized, wantStatus: http.StatusTooManyRequests, wantCode: constant.OpenCodeGoPublicRateLimitErrorCode},
		{name: "raw 429 fails closed", upstreamStatusCode: http.StatusTooManyRequests, wantStatus: http.StatusTooManyRequests, wantCode: constant.OpenCodeGoPublicRateLimitErrorCode},
		{name: "raw 200 envelope fails closed", upstreamStatusCode: http.StatusOK, wantStatus: http.StatusTooManyRequests, wantCode: constant.OpenCodeGoPublicRateLimitErrorCode},
	}

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("type-%d/%s", channelType, test.name), func(t *testing.T) {
				log := &Log{
					Type:      LogTypeError,
					Content:   "messages[0].role is required",
					ChannelId: 81,
					Other: common.MapToJsonStr(map[string]interface{}{
						"channel_type": channelType,
						"error_type":   "invalid_request_error",
						"error_code":   "invalid_request_error",
						"status_code":  http.StatusBadRequest,
						"admin_info": map[string]interface{}{
							"upstream_status_code": test.upstreamStatusCode,
							"error_origin":         relaytypes.ErrorOriginUpstreamHTTP,
							"error_subtype":        "non_2xx",
						},
					}),
				}

				formatUserLogs([]*Log{log}, 0)

				other, err := common.StrToMap(log.Other)
				require.NoError(t, err)
				require.Equal(t, float64(test.wantStatus), other["status_code"])
				require.Equal(t, test.wantCode, other["error_type"])
				require.Equal(t, test.wantCode, other["error_code"])
				if test.wantStatus == http.StatusBadRequest {
					require.Equal(t, constant.OpenCodeGoPublicInvalidRequestMessage, log.Content)
				} else {
					require.Equal(t, constant.OpenCodeGoPublicOverloadMessage, log.Content)
				}
			})
		}
	}
}

func TestFormatUserLogsRequiresTypedOriginForFixedUpstream400(t *testing.T) {
	for _, test := range []struct {
		name        string
		adminInfo   map[string]interface{}
		wantStatus  int
		wantContent string
	}{
		{
			name: "raw status without origin fails closed",
			adminInfo: map[string]interface{}{
				"upstream_status_code": http.StatusBadRequest,
			},
			wantStatus:  http.StatusTooManyRequests,
			wantContent: constant.OpenCodeGoPublicOverloadMessage,
		},
		{
			name: "transport origin cannot forge raw status",
			adminInfo: map[string]interface{}{
				"error_origin":         relaytypes.ErrorOriginUpstreamTransport,
				"error_subtype":        "request_transport",
				"upstream_status_code": http.StatusBadRequest,
			},
			wantStatus:  http.StatusTooManyRequests,
			wantContent: constant.OpenCodeGoPublicOverloadMessage,
		},
		{
			name: "typed upstream http 400 uses fixed content",
			adminInfo: map[string]interface{}{
				"error_origin":         relaytypes.ErrorOriginUpstreamHTTP,
				"error_subtype":        "non_2xx",
				"upstream_status_code": http.StatusBadRequest,
			},
			wantStatus:  http.StatusBadRequest,
			wantContent: constant.OpenCodeGoPublicInvalidRequestMessage,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := Log{
				Type:      LogTypeError,
				Content:   "upstream secret workspace=hidden",
				ChannelId: 81,
				Other: common.MapToJsonStr(map[string]interface{}{
					"channel_type": constant.ChannelTypeOpenCodeAPIKey,
					"error_type":   "invalid_request_error",
					"error_code":   "invalid_request_error",
					"status_code":  http.StatusBadRequest,
					"admin_info":   test.adminInfo,
				}),
			}
			public := raw

			formatUserLogs([]*Log{&public}, 0)

			publicOther, err := common.StrToMap(public.Other)
			require.NoError(t, err)
			assert.Equal(t, float64(test.wantStatus), publicOther["status_code"])
			assert.Equal(t, test.wantContent, public.Content)
			assert.NotContains(t, public.Content, "workspace")
			assert.Equal(t, "upstream secret workspace=hidden", raw.Content)
			assert.Contains(t, raw.Other, "admin_info")
		})
	}
}

func TestFormatUserLogsPreservesOnlyMaskerSafeTypedLocalValidation(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantContent string
	}{
		{
			name:        "safe fixed message",
			content:     "status_code=400, The selected model does not support disabling reasoning",
			wantContent: "The selected model does not support disabling reasoning",
		},
		{
			name:        "domain-like path falls back",
			content:     "status_code=400, thinking.type=disabled is not supported for the selected model",
			wantContent: constant.OpenCodeGoPublicInvalidRequestMessage,
		},
	}
	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("type-%d/%s", channelType, test.name), func(t *testing.T) {
				raw := Log{
					Type:      LogTypeError,
					Content:   test.content,
					ChannelId: 81,
					Other: common.MapToJsonStr(map[string]interface{}{
						"channel_type": channelType,
						"error_type":   "invalid_request_error",
						"error_code":   "invalid_request_error",
						"status_code":  http.StatusBadRequest,
						"admin_info": map[string]interface{}{
							"error_origin":  relaytypes.ErrorOriginLocalValidation,
							"error_subtype": "model.glm-5.3.chat.thinking-disabled",
						},
					}),
				}
				public := raw

				formatUserLogs([]*Log{&public}, 0)

				publicOther, err := common.StrToMap(public.Other)
				require.NoError(t, err)
				assert.Equal(t, float64(http.StatusBadRequest), publicOther["status_code"])
				assert.Equal(t, constant.OpenCodeGoPublicInvalidRequestCode, publicOther["error_type"])
				assert.Equal(t, constant.OpenCodeGoPublicInvalidRequestCode, publicOther["error_code"])
				assert.Equal(t, test.wantContent, public.Content)
				assert.NotContains(t, public.Content, "***")
				assert.Equal(t, test.content, raw.Content)
				assert.Contains(t, raw.Other, string(relaytypes.ErrorOriginLocalValidation))
			})
		}
	}
}
