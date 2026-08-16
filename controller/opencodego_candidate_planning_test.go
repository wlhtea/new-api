package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/opencodego"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const openCodeCandidatePlannerCapabilityModel = "deepseek-v4-flash"

func newOpenCodeCandidatePlannerClaudeFixture(
	t *testing.T,
	channelType int,
	effort string,
) (*gin.Context, *relaycommon.RelayInfo) {
	t.Helper()
	body, err := common.Marshal(map[string]any{
		"model":      openCodeCandidatePlannerCapabilityModel,
		"max_tokens": 32,
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
		"metadata": map[string]any{"user_id": "planner-session"},
		"output_config": map[string]any{
			"effort": effort,
		},
		"context_management": map[string]any{
			"edits": []any{
				map[string]any{"type": "clear_thinking_20251015", "keep": "all"},
			},
		},
		"stream": false,
	})
	require.NoError(t, err)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	storage, err := common.CreateBodyStorage(body)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })
	c.Set(common.KeyBodyStorage, storage)
	common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
	common.SetContextKey(c, constant.ContextKeyChannelId, 8000+channelType)
	common.SetContextKey(c, constant.ContextKeyChannelName, fmt.Sprintf("planner-type-%d", channelType))
	common.SetContextKey(c, constant.ContextKeyChannelKey, "planner-test-key")
	common.SetContextKey(c, constant.ContextKeyChannelModelMapping, "")
	common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{})
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{
		OpenCodeGo: &dto.OpenCodeGoConfig{
			ModelProtocols: map[string]string{
				openCodeCandidatePlannerCapabilityModel: dto.OpenCodeGoProtocolChat,
			},
		},
	})
	common.SetContextKey(c, constant.ContextKeyOriginalModel, openCodeCandidatePlannerCapabilityModel)
	common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyAutoGroup, "default")
	if channelType == constant.ChannelTypeOpenCodeAPIKey {
		c.Set(string(constant.ContextKeyTokenSpecificChannelId), 8000+channelType)
	}

	request, err := helper.GetAndValidateRequest(c, types.RelayFormatClaude)
	require.NoError(t, err)
	info, err := relaycommon.GenRelayInfo(c, types.RelayFormatClaude, request, nil)
	require.NoError(t, err)
	return c, info
}

func requireFreshOpenCodeCandidatePlannerCapability(t *testing.T) string {
	t.Helper()
	const payload = `{"schema_version":1,"provider":"opencode-go","models":[{"id":"deepseek-v4-flash","options_known":true,"efforts":["high","low","max"]}]}`
	digest := sha256.Sum256([]byte(payload))
	revision := hex.EncodeToString(digest[:])
	now := time.Now().Unix()
	require.NoError(t, model.DB.AutoMigrate(&model.OpenCodeGoCapabilitySnapshot{}))
	require.NoError(t, model.DB.Create(&model.OpenCodeGoCapabilitySnapshot{
		Provider:          model.OpenCodeGoCapabilityProvider,
		Generation:        time.Now().UnixNano(),
		SchemaVersion:     1,
		SemanticRevision:  revision,
		SourceETag:        "",
		CheckedAt:         now,
		NormalizedPayload: payload,
		UpdatedAt:         now,
	}).Error)
	service.StartOpenCodeGoCapabilityAuthority(24 * 60 * 60)
	view := service.CurrentOpenCodeGoCapabilityView()
	require.Equal(t, service.OpenCodeGoCapabilitySupported,
		view.CheckEffort(openCodeCandidatePlannerCapabilityModel, "high"))
	require.Equal(t, service.OpenCodeGoCapabilityUnsupported,
		view.CheckEffort(openCodeCandidatePlannerCapabilityModel, "xhigh"))
	require.Equal(t, revision, view.SemanticRevision())
	return revision
}

func TestPrepareOpenCodeCandidatePlansRetainsSupportedClaudeEffortForTypes62And63(t *testing.T) {
	setupModelListControllerTestDB(t)
	revision := requireFreshOpenCodeCandidatePlannerCapability(t)

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			c, info := newOpenCodeCandidatePlannerClaudeFixture(t, channelType, "high")
			plans, relayErr := prepareAndFreezeOpenCodeCandidatePlans(c, info)

			require.Nil(t, relayErr)
			require.NotNil(t, plans)
			require.Len(t, plans.plans, 1)
			finalized := plans.plans[0]
			assert.Equal(t, revision, finalized.capabilityRevision)
			assert.True(t, finalized.effort.Present)
			assert.False(t, finalized.effort.Null)
			assert.Equal(t, "high", finalized.effort.Value)

			var wire map[string]any
			require.NoError(t, common.Unmarshal(finalized.body, &wire))
			assert.Equal(t, openCodeCandidatePlannerCapabilityModel, wire["model"])
			assert.Equal(t, "high", wire["reasoning_effort"])
			assert.Equal(t, map[string]any{"user_id": "planner-session"}, wire["metadata"])
			assert.NotContains(t, wire, "output_config")
			assert.NotContains(t, wire, "context_management")

			preflight, found, err := opencodego.GetRequestPreflightPlan(c)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, revision, preflight.CapabilityRevision)
			assert.Equal(t, opencodego.ProtocolChat, preflight.FinalProtocol)

			stored, storedFound := c.Get(openCodeFinalizedCandidatePlansContextKey)
			require.True(t, storedFound)
			assert.Same(t, plans, stored)
			_, snapshotFound, snapshotErr := getOpenCodeAPIKeyRetrySnapshot(c)
			require.NoError(t, snapshotErr)
			assert.Equal(t, channelType == constant.ChannelTypeOpenCodeAPIKey, snapshotFound)
		})
	}
}

func TestPrepareOpenCodeCandidatePlansRejectsUnsupportedXHighWithoutCommittedState(t *testing.T) {
	setupModelListControllerTestDB(t)
	requireFreshOpenCodeCandidatePlannerCapability(t)

	for _, channelType := range []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey} {
		t.Run(fmt.Sprintf("type-%d", channelType), func(t *testing.T) {
			c, info := newOpenCodeCandidatePlannerClaudeFixture(t, channelType, "xhigh")
			initialChannelID := common.GetContextKeyInt(c, constant.ContextKeyChannelId)

			plans, relayErr := prepareAndFreezeOpenCodeCandidatePlans(c, info)

			assert.Nil(t, plans)
			require.NotNil(t, relayErr)
			assert.Equal(t, http.StatusBadRequest, relayErr.StatusCode)
			assert.Equal(t, types.ErrorOriginLocalValidation, relayErr.Provenance().Origin)
			assert.Equal(t, openCodeCapabilityUnsupportedRule, relayErr.Provenance().Subtype)
			assert.NotContains(t, relayErr.Error(), "xhigh")
			assert.Equal(t, initialChannelID, common.GetContextKeyInt(c, constant.ContextKeyChannelId))
			_, workspaceSelected := c.Get(string(constant.ContextKeyOpenCodeGoWorkspaceUID))
			assert.False(t, workspaceSelected)
			assert.Nil(t, info.ChannelMeta)
			assert.Nil(t, info.Billing)

			_, found, err := opencodego.GetRequestPreflightPlan(c)
			require.NoError(t, err)
			assert.False(t, found)
			_, finalizedFound := c.Get(openCodeFinalizedCandidatePlansContextKey)
			assert.False(t, finalizedFound)
			_, snapshotFound, snapshotErr := getOpenCodeAPIKeyRetrySnapshot(c)
			require.NoError(t, snapshotErr)
			assert.False(t, snapshotFound)
			ruleID, stageID, rejected := openCodeRequestPreflightRejection(c)
			require.True(t, rejected)
			assert.Equal(t, openCodeCapabilityUnsupportedRule, ruleID)
			assert.Equal(t, openCodeCapabilityStage, stageID)
		})
	}
}

func TestPrepareOpenCodeCandidatePlansRetainedCandidateWinsOverCapabilityUnknown(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	revision := requireFreshOpenCodeCandidatePlannerCapability(t)
	const (
		requestModel = "candidate-capability-fallback-model"
		unknownModel = "candidate-capability-unlisted-model"
	)

	unknown := newOpenCodeAPIKeySnapshotTestChannel(
		"candidate-capability-unknown",
		requestModel,
		20,
		dto.OpenCodeGoProtocolChat,
	)
	unknownMapping := `{"candidate-capability-fallback-model":"candidate-capability-unlisted-model"}`
	unknown.ModelMapping = &unknownMapping
	unknown.SetOtherSettings(dto.ChannelOtherSettings{OpenCodeGo: &dto.OpenCodeGoConfig{
		ModelProtocols: map[string]string{unknownModel: dto.OpenCodeGoProtocolChat},
	}})
	persistOpenCodeAPIKeySnapshotTestChannel(t, db, &unknown)

	supported := newOpenCodeAPIKeySnapshotTestChannel(
		"candidate-capability-supported",
		requestModel,
		10,
		dto.OpenCodeGoProtocolChat,
	)
	supportedMapping := `{"candidate-capability-fallback-model":"deepseek-v4-flash"}`
	supported.ModelMapping = &supportedMapping
	supported.SetOtherSettings(dto.ChannelOtherSettings{OpenCodeGo: &dto.OpenCodeGoConfig{
		ModelProtocols: map[string]string{
			openCodeCandidatePlannerCapabilityModel: dto.OpenCodeGoProtocolChat,
		},
	}})
	persistOpenCodeAPIKeySnapshotTestChannel(t, db, &supported)

	c, info := newOpenCodeAPIKeySnapshotTestFixtureWithExtra(
		t,
		&unknown,
		requestModel,
		`,"reasoning_effort":"high"`,
	)
	plans, relayErr := prepareAndFreezeOpenCodeCandidatePlans(c, info)

	require.Nil(t, relayErr)
	require.NotNil(t, plans)
	require.Len(t, plans.plans, 1)
	assert.Equal(t, supported.Id, plans.plans[0].key.ChannelID)
	assert.Equal(t, revision, plans.plans[0].capabilityRevision)
	assert.Equal(t, supported.Id, common.GetContextKeyInt(c, constant.ContextKeyChannelId))

	_, unknownFound, err := opencodego.GetRequestPreflightPlanForSelection(c, "default", unknown.Id)
	require.NoError(t, err)
	assert.False(t, unknownFound)
	supportedPlan, supportedFound, err := opencodego.GetRequestPreflightPlanForSelection(c, "default", supported.Id)
	require.NoError(t, err)
	require.True(t, supportedFound)
	assert.Equal(t, openCodeCandidatePlannerCapabilityModel, supportedPlan.FinalModel)
	assert.Equal(t, revision, supportedPlan.CapabilityRevision)

	snapshot, found, err := getOpenCodeAPIKeyRetrySnapshot(c)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, snapshot.topology, 1)
	require.Len(t, snapshot.selections, 1)
	assert.Equal(t, supported.Id, snapshot.topology[0].channelID)
	assert.Equal(t, supported.Id, snapshot.selections[0].channelID)
}

func TestPrepareOpenCodeCandidatePlansKeepsRetainedTopologyScheduleRegistryAndBillingConsistent(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	revision := requireFreshOpenCodeCandidatePlannerCapability(t)

	first := newOpenCodeAPIKeySnapshotTestChannel(
		"candidate-consistency-first",
		openCodeCandidatePlannerCapabilityModel,
		20,
		dto.OpenCodeGoProtocolChat,
	)
	persistOpenCodeAPIKeySnapshotTestChannel(t, db, &first)
	second := newOpenCodeAPIKeySnapshotTestChannel(
		"candidate-consistency-second",
		openCodeCandidatePlannerCapabilityModel,
		10,
		dto.OpenCodeGoProtocolChat,
	)
	persistOpenCodeAPIKeySnapshotTestChannel(t, db, &second)

	originalRetryTimes := common.RetryTimes
	common.RetryTimes = 1
	t.Cleanup(func() { common.RetryTimes = originalRetryTimes })

	c, info := newOpenCodeAPIKeySnapshotTestFixtureWithExtra(
		t,
		&first,
		openCodeCandidatePlannerCapabilityModel,
		`,"reasoning_effort":"high"`,
	)
	plans, relayErr := prepareAndFreezeOpenCodeCandidatePlans(c, info)

	require.Nil(t, relayErr)
	require.NotNil(t, plans)
	require.Len(t, plans.plans, 2)
	snapshot, found, err := getOpenCodeAPIKeyRetrySnapshot(c)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, snapshot.topology, 2)
	require.Len(t, snapshot.selections, 2)
	assert.Equal(t, []int{first.Id, second.Id}, []int{
		snapshot.selections[0].channelID,
		snapshot.selections[1].channelID,
	})

	topologyKeys := make(map[opencodego.RequestPreflightPlanKey]struct{}, len(snapshot.topology))
	for _, selection := range snapshot.topology {
		topologyKeys[opencodego.RequestPreflightPlanKey{
			SelectionGroup: selection.selectionGroup,
			ChannelID:      selection.channelID,
		}] = struct{}{}
	}
	billingViews := plans.billingViews(0)
	require.Len(t, billingViews, len(plans.plans))
	for index, finalized := range plans.plans {
		assert.Equal(t, revision, finalized.capabilityRevision)
		assert.Equal(t, "high", finalized.effort.Value)
		_, inTopology := topologyKeys[finalized.key]
		assert.True(t, inTopology)
		preflight, inRegistry, registryErr := opencodego.GetRequestPreflightPlanForSelection(
			c,
			finalized.key.SelectionGroup,
			finalized.key.ChannelID,
		)
		require.NoError(t, registryErr)
		require.True(t, inRegistry)
		assert.Equal(t, revision, preflight.CapabilityRevision)
		assert.Equal(t, finalized.key.SelectionGroup, billingViews[index].SelectionGroup)
		assert.Equal(t, finalized.body, billingViews[index].Body)
	}
}

func TestReduceOpenCodeCandidateFailuresIsOrderIndependent(t *testing.T) {
	type failureKind string
	const (
		failureClient     failureKind = "client"
		failureCapability failureKind = "capability"
		failureConfig     failureKind = "config"
		failureFatal      failureKind = "fatal"
	)
	newFailure := func(kind failureKind) openCodeCandidateFailure {
		switch kind {
		case failureClient:
			return *clientUnsupportedOpenCodeCandidateFailure()
		case failureCapability:
			return openCodeCandidateFailure{
				class: openCodeCandidateFailureCapability,
				err:   errors.New("capability unavailable"),
			}
		case failureConfig:
			return *configOpenCodeCandidateFailure(errors.New("candidate configuration invalid"))
		case failureFatal:
			return *fatalOpenCodeCandidateFailure(errors.New("candidate invariant failed"))
		default:
			t.Fatalf("unknown failure kind %q", kind)
			return openCodeCandidateFailure{}
		}
	}

	tests := []struct {
		name           string
		left           failureKind
		right          failureKind
		expectedStatus int
		expectedOrigin types.ErrorOrigin
		expectedRule   string
	}{
		{name: "fatal over config", left: failureFatal, right: failureConfig, expectedStatus: http.StatusInternalServerError, expectedOrigin: types.ErrorOriginGatewayInvariant, expectedRule: opencodego.PreflightPlanMismatchRule},
		{name: "fatal over capability", left: failureFatal, right: failureCapability, expectedStatus: http.StatusInternalServerError, expectedOrigin: types.ErrorOriginGatewayInvariant, expectedRule: opencodego.PreflightPlanMismatchRule},
		{name: "fatal over client", left: failureFatal, right: failureClient, expectedStatus: http.StatusInternalServerError, expectedOrigin: types.ErrorOriginGatewayInvariant, expectedRule: opencodego.PreflightPlanMismatchRule},
		{name: "config over capability", left: failureConfig, right: failureCapability, expectedStatus: http.StatusServiceUnavailable, expectedOrigin: types.ErrorOriginGatewayConfig, expectedRule: opencodego.PreflightCandidateConfigInvalidRule},
		{name: "config over client", left: failureConfig, right: failureClient, expectedStatus: http.StatusServiceUnavailable, expectedOrigin: types.ErrorOriginGatewayConfig, expectedRule: opencodego.PreflightCandidateConfigInvalidRule},
		{name: "capability over client", left: failureCapability, right: failureClient, expectedStatus: http.StatusServiceUnavailable, expectedOrigin: types.ErrorOriginGatewayDependency, expectedRule: openCodeCapabilityUnknownRule},
	}

	for _, test := range tests {
		for _, order := range []struct {
			name     string
			failures []openCodeCandidateFailure
		}{
			{name: "forward", failures: []openCodeCandidateFailure{newFailure(test.left), newFailure(test.right)}},
			{name: "reverse", failures: []openCodeCandidateFailure{newFailure(test.right), newFailure(test.left)}},
		} {
			t.Run(test.name+"/"+order.name, func(t *testing.T) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				relayErr := reduceOpenCodeCandidateFailures(c, order.failures)
				require.NotNil(t, relayErr)
				assert.Equal(t, test.expectedStatus, relayErr.StatusCode)
				assert.Equal(t, test.expectedOrigin, relayErr.Provenance().Origin)
				assert.Equal(t, test.expectedRule, relayErr.Provenance().Subtype)
			})
		}
	}
}
