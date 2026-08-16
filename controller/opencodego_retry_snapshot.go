package controller

import (
	"errors"
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/opencodego"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

const (
	openCodeAPIKeyRetrySnapshotContextKey = "opencode_api_key_retry_snapshot_v1"
	openCodeAPIKeyRetrySnapshotVersion    = "opencode-api-key-retry-snapshot-v1"
)

// frozenOpenCodeAPIKeySelection contains the effective selected-row context,
// not a live model.Channel pointer. Applying it on a retry therefore cannot
// observe a cache or database update that occurred after request preflight.
type frozenOpenCodeAPIKeySelection struct {
	selectionGroup       string
	channelID            int
	priority             int64
	weight               uint
	channelName          string
	channelCreateTime    int64
	channelAutoBan       bool
	channelBaseURL       string
	channelKey           string
	organization         string
	modelMapping         string
	statusCodeMapping    string
	channelSetting       dto.ChannelSettings
	channelOther         dto.ChannelOtherSettings
	paramOverride        map[string]interface{}
	headerOverride       map[string]interface{}
	originalModel        string
	systemPromptOverride bool
}

type openCodeAPIKeyRetrySnapshot struct {
	version    string
	topology   []frozenOpenCodeAPIKeySelection
	selections []frozenOpenCodeAPIKeySelection
}

func newOpenCodeRetrySnapshotAPIError(c *gin.Context, err error) *types.NewAPIError {
	return newOpenCodeRequestPreflightAPIError(c, opencodego.NewRequestPreflightPlanStorageError(err))
}

func freezeOpenCodeAPIKeyRetrySnapshot(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	if c == nil || info == nil ||
		common.GetContextKeyInt(c, constant.ContextKeyChannelType) != constant.ChannelTypeOpenCodeAPIKey {
		return nil
	}

	initialKey := opencodego.RequestPreflightPlanKey{
		SelectionGroup: relaycommon.ResolveSelectionGroup(c, info),
		ChannelID:      common.GetContextKeyInt(c, constant.ContextKeyChannelId),
	}
	if strings.TrimSpace(initialKey.SelectionGroup) == "" || initialKey.ChannelID <= 0 {
		return newOpenCodeRequestPreflightAPIError(c, opencodego.NewRequestPreflightPlanStorageError(
			errors.New("initial OpenCode API-key selection is invalid"),
		))
	}

	var initialSelection frozenOpenCodeAPIKeySelection
	var initialPlan opencodego.RequestPreflightPlan
	topologySelections := make([]frozenOpenCodeAPIKeySelection, 0, 1)
	plans := make([]opencodego.RequestPreflightPlan, 0, 1)
	_, specific := c.Get(string(constant.ContextKeyTokenSpecificChannelId))
	if specific {
		var err error
		initialPlan, err = opencodego.BuildRequestPreflightPlan(c, info)
		if err != nil {
			return newOpenCodeRequestPreflightAPIError(c, err)
		}
		initialSelection, err = captureFrozenOpenCodeAPIKeySelection(c, initialPlan.SelectionGroup)
		if err != nil {
			return newOpenCodeRequestPreflightAPIError(c, opencodego.NewRequestPreflightPlanStorageError(err))
		}
		topologySelections = append(topologySelections, initialSelection)
		plans = append(plans, initialPlan)
	} else {
		candidates, topologyErr := service.SnapshotOpenCodeAPIKeyCandidateTopology(
			c,
			info.TokenGroup,
			info.UserGroup,
			info.OriginModelName,
			c.Request.URL.Path,
		)
		if topologyErr != nil {
			if errors.Is(topologyErr, service.ErrOpenCodeAPIKeyMixedTopology) ||
				errors.Is(topologyErr, service.ErrOpenCodeAPIKeyInconsistentTopology) {
				return newOpenCodeRequestPreflightAPIError(c, opencodego.NewRequestPreflightCandidateConfigError(topologyErr))
			}
			return newOpenCodeRequestPreflightAPIError(c, opencodego.NewRequestPreflightPlanStorageError(topologyErr))
		}

		topologySelections = make([]frozenOpenCodeAPIKeySelection, 0, len(candidates))
		candidateGroupOrder := make([]string, 0)
		seenCandidateGroups := make(map[string]struct{})
		plansByKey := make(map[opencodego.RequestPreflightPlanKey]opencodego.RequestPreflightPlan, len(candidates))
		initialSeen := false
		initialFound := false
		var initialValidationErr error
		for _, candidate := range candidates {
			if candidate.Channel == nil {
				return newOpenCodeRequestPreflightAPIError(c, opencodego.NewRequestPreflightPlanStorageError(
					errors.New("OpenCode API-key topology contains an empty channel"),
				))
			}
			selectionGroup := strings.TrimSpace(candidate.SelectionGroup)
			key := opencodego.RequestPreflightPlanKey{
				SelectionGroup: selectionGroup,
				ChannelID:      candidate.Channel.Id,
			}
			if _, found := seenCandidateGroups[selectionGroup]; !found {
				seenCandidateGroups[selectionGroup] = struct{}{}
				candidateGroupOrder = append(candidateGroupOrder, selectionGroup)
			}
			if key == initialKey {
				initialSeen = true
			}
			candidateContext := c.Copy()
			common.SetContextKey(candidateContext, constant.ContextKeyAutoGroup, selectionGroup)
			if setupErr := middleware.SetupContextForSelectedChannel(candidateContext, candidate.Channel, info.OriginModelName); setupErr != nil {
				return newOpenCodeRequestPreflightAPIError(c, opencodego.NewRequestPreflightCandidateConfigError(setupErr))
			}
			selection, captureErr := captureFrozenOpenCodeAPIKeySelection(candidateContext, selectionGroup)
			if captureErr != nil {
				return newOpenCodeRequestPreflightAPIError(c, opencodego.NewRequestPreflightPlanStorageError(captureErr))
			}
			selection.priority = candidate.Priority
			selection.weight = candidate.Weight
			plan, planErr := opencodego.BuildRequestPreflightPlan(candidateContext, info)
			if planErr != nil {
				if preflightErr, ok := opencodego.AsRequestPreflightError(planErr); ok &&
					preflightErr.Origin == types.ErrorOriginLocalValidation {
					if key == initialKey {
						initialValidationErr = planErr
					}
					continue
				}
				return newOpenCodeRequestPreflightAPIError(c, planErr)
			}
			if plan.Key() != key {
				return newOpenCodeRequestPreflightAPIError(c, opencodego.NewRequestPreflightPlanStorageError(
					errors.New("frozen OpenCode API-key selection does not match its preflight plan"),
				))
			}
			if key == initialKey {
				initialSelection = selection
				initialPlan = plan
				initialFound = true
			}
			topologySelections = append(topologySelections, selection)
			plans = append(plans, plan)
			plansByKey[key] = plan
		}
		if !initialSeen {
			return newOpenCodeRequestPreflightAPIError(c, opencodego.NewRequestPreflightPlanStorageError(
				errors.New("selected OpenCode API-key channel is absent from its eligible topology"),
			))
		}
		if !initialFound {
			crossGroupRetry := common.GetContextKeyBool(c, constant.ContextKeyTokenCrossGroupRetry)
			replacement, replacementErr := selectCompatibleOpenCodeAPIKeyInitialSelection(
				topologySelections,
				candidateGroupOrder,
				initialKey.SelectionGroup,
				info.TokenGroup,
				crossGroupRetry,
				common.MemoryCacheEnabled,
				common.GetRandomInt,
			)
			if replacementErr != nil {
				if initialValidationErr != nil {
					return newOpenCodeRequestPreflightAPIError(c, initialValidationErr)
				}
				return newOpenCodeRequestPreflightAPIError(c, opencodego.NewRequestPreflightPlanStorageError(replacementErr))
			}
			replacementKey := opencodego.RequestPreflightPlanKey{
				SelectionGroup: replacement.selectionGroup,
				ChannelID:      replacement.channelID,
			}
			replacementPlan, found := plansByKey[replacementKey]
			if !found {
				return newOpenCodeRequestPreflightAPIError(c, opencodego.NewRequestPreflightPlanStorageError(
					errors.New("compatible OpenCode API-key selection has no preflight plan"),
				))
			}
			initialKey = replacementKey
			initialSelection = replacement
			initialPlan = replacementPlan
		}
		if applyErr := initialSelection.apply(c); applyErr != nil {
			return newOpenCodeRequestPreflightAPIError(c, opencodego.NewRequestPreflightPlanStorageError(applyErr))
		}
	}

	if initialPlan.Key() != initialKey {
		return newOpenCodeRequestPreflightAPIError(c, opencodego.NewRequestPreflightPlanStorageError(
			errors.New("initial OpenCode API-key plan does not match its selection"),
		))
	}
	if err := opencodego.StoreRequestPreflightPlans(c, plans); err != nil {
		return newOpenCodeRequestPreflightAPIError(c, opencodego.NewRequestPreflightPlanStorageError(err))
	}
	selections := []frozenOpenCodeAPIKeySelection{initialSelection}
	if !specific {
		var err error
		selections, err = deriveOpenCodeAPIKeyPhysicalSchedule(
			topologySelections,
			initialKey,
			info.TokenGroup,
			common.GetContextKeyBool(c, constant.ContextKeyTokenCrossGroupRetry),
			common.RetryTimes,
		)
		if err != nil {
			return newOpenCodeRequestPreflightAPIError(c, opencodego.NewRequestPreflightPlanStorageError(err))
		}
	}
	c.Set(openCodeAPIKeyRetrySnapshotContextKey, &openCodeAPIKeyRetrySnapshot{
		version:    openCodeAPIKeyRetrySnapshotVersion,
		topology:   topologySelections,
		selections: selections,
	})
	return nil
}

func selectCompatibleOpenCodeAPIKeyInitialSelection(
	topology []frozenOpenCodeAPIKeySelection,
	groupOrder []string,
	initialGroup string,
	tokenGroup string,
	crossGroupRetry bool,
	memoryCacheEnabled bool,
	pick func(int) int,
) (frozenOpenCodeAPIKeySelection, error) {
	initialGroup = strings.TrimSpace(initialGroup)
	if len(topology) == 0 || initialGroup == "" || len(groupOrder) == 0 {
		return frozenOpenCodeAPIKeySelection{}, errors.New("no compatible OpenCode API-key candidate is available")
	}
	groupSelections := make(map[string][]frozenOpenCodeAPIKeySelection, len(groupOrder))
	for _, selection := range topology {
		groupSelections[selection.selectionGroup] = append(groupSelections[selection.selectionGroup], selection)
	}
	initialGroupIndex := -1
	for index, group := range groupOrder {
		if group == initialGroup {
			initialGroupIndex = index
			break
		}
	}
	if initialGroupIndex < 0 {
		return frozenOpenCodeAPIKeySelection{}, errors.New("initial OpenCode API-key selection group is unavailable")
	}

	lastGroupIndex := initialGroupIndex + 1
	if strings.TrimSpace(tokenGroup) == "auto" && crossGroupRetry {
		lastGroupIndex = len(groupOrder)
	}
	for _, group := range groupOrder[initialGroupIndex:lastGroupIndex] {
		selections := groupSelections[group]
		if len(selections) == 0 {
			continue
		}
		return selectFrozenOpenCodeAPIKeyPriorityTier(selections, 0, memoryCacheEnabled, pick)
	}
	return frozenOpenCodeAPIKeySelection{}, errors.New("no compatible OpenCode API-key candidate is available in the permitted groups")
}

func getOpenCodeAPIKeyRetrySnapshot(c *gin.Context) (*openCodeAPIKeyRetrySnapshot, bool, error) {
	if c == nil {
		return nil, false, errors.New("OpenCode API-key retry context is nil")
	}
	value, found := c.Get(openCodeAPIKeyRetrySnapshotContextKey)
	if !found {
		return nil, false, nil
	}
	snapshot, ok := value.(*openCodeAPIKeyRetrySnapshot)
	if !ok || snapshot == nil || snapshot.version != openCodeAPIKeyRetrySnapshotVersion ||
		len(snapshot.topology) == 0 || len(snapshot.selections) == 0 {
		return nil, true, errors.New("OpenCode API-key retry snapshot is corrupt")
	}
	return snapshot, true, nil
}

func deriveOpenCodeAPIKeyPhysicalSchedule(
	topology []frozenOpenCodeAPIKeySelection,
	initialKey opencodego.RequestPreflightPlanKey,
	tokenGroup string,
	crossGroupRetry bool,
	retryBudget int,
) ([]frozenOpenCodeAPIKeySelection, error) {
	return deriveOpenCodeAPIKeyPhysicalScheduleWithPicker(
		topology,
		initialKey,
		tokenGroup,
		crossGroupRetry,
		retryBudget,
		common.MemoryCacheEnabled,
		common.GetRandomInt,
	)
}

func deriveOpenCodeAPIKeyPhysicalScheduleWithPicker(
	topology []frozenOpenCodeAPIKeySelection,
	initialKey opencodego.RequestPreflightPlanKey,
	tokenGroup string,
	crossGroupRetry bool,
	retryBudget int,
	memoryCacheEnabled bool,
	pick func(int) int,
) ([]frozenOpenCodeAPIKeySelection, error) {
	if len(topology) == 0 {
		return nil, errors.New("OpenCode API-key topology is empty")
	}
	type selectionKey struct {
		group     string
		channelID int
	}
	seen := make(map[selectionKey]struct{}, len(topology))
	initialIndex := -1
	for index, selection := range topology {
		key := selectionKey{group: selection.selectionGroup, channelID: selection.channelID}
		if key.group == "" || key.channelID <= 0 {
			return nil, errors.New("OpenCode API-key topology selection is invalid")
		}
		if _, exists := seen[key]; exists {
			return nil, errors.New("OpenCode API-key topology contains a duplicate selection")
		}
		seen[key] = struct{}{}
		if key.group == strings.TrimSpace(initialKey.SelectionGroup) && key.channelID == initialKey.ChannelID {
			initialIndex = index
		}
	}
	if initialIndex < 0 {
		return nil, errors.New("OpenCode API-key topology does not contain the initial selection")
	}

	initial := topology[initialIndex]
	schedule := []frozenOpenCodeAPIKeySelection{initial}
	if retryBudget <= 0 {
		return schedule, nil
	}

	groupOrder := make([]string, 0)
	groupSelections := make(map[string][]frozenOpenCodeAPIKeySelection)
	for _, selection := range topology {
		if _, exists := groupSelections[selection.selectionGroup]; !exists {
			groupOrder = append(groupOrder, selection.selectionGroup)
		}
		groupSelections[selection.selectionGroup] = append(groupSelections[selection.selectionGroup], selection)
	}
	initialGroupIndex := -1
	for index, group := range groupOrder {
		if group == initial.selectionGroup {
			initialGroupIndex = index
			break
		}
	}
	if initialGroupIndex < 0 {
		return nil, errors.New("OpenCode API-key initial selection group is unavailable")
	}

	groupsToSchedule := groupOrder[initialGroupIndex : initialGroupIndex+1]
	if strings.TrimSpace(tokenGroup) == "auto" && crossGroupRetry {
		groupsToSchedule = groupOrder[initialGroupIndex:]
	}
	for _, group := range groupsToSchedule {
		firstRetryIndex := 0
		if group == initial.selectionGroup {
			firstRetryIndex = 1
		}
		for retryIndex := firstRetryIndex; retryIndex <= retryBudget; retryIndex++ {
			selection, selectErr := selectFrozenOpenCodeAPIKeyPriorityTier(
				groupSelections[group],
				retryIndex,
				memoryCacheEnabled,
				pick,
			)
			if selectErr != nil {
				return nil, selectErr
			}
			schedule = append(schedule, selection)
		}
	}
	return schedule, nil
}

func selectFrozenOpenCodeAPIKeyPriorityTier(
	selections []frozenOpenCodeAPIKeySelection,
	retryIndex int,
	memoryCacheEnabled bool,
	pick func(int) int,
) (frozenOpenCodeAPIKeySelection, error) {
	if len(selections) == 0 || retryIndex < 0 || pick == nil {
		return frozenOpenCodeAPIKeySelection{}, errors.New("OpenCode API-key retry tier input is invalid")
	}
	priorities := make([]int64, 0, len(selections))
	seenPriorities := make(map[int64]struct{}, len(selections))
	for _, selection := range selections {
		if _, found := seenPriorities[selection.priority]; found {
			continue
		}
		seenPriorities[selection.priority] = struct{}{}
		priorities = append(priorities, selection.priority)
	}
	sort.Slice(priorities, func(i, j int) bool { return priorities[i] > priorities[j] })
	if len(priorities) == 0 {
		return frozenOpenCodeAPIKeySelection{}, errors.New("OpenCode API-key retry tier is empty")
	}
	if retryIndex >= len(priorities) {
		retryIndex = len(priorities) - 1
	}
	targetPriority := priorities[retryIndex]
	tier := make([]frozenOpenCodeAPIKeySelection, 0, len(selections))
	allZero := true
	for _, selection := range selections {
		if selection.priority != targetPriority {
			continue
		}
		tier = append(tier, selection)
		if selection.weight != 0 {
			allZero = false
		}
	}
	if len(tier) == 0 {
		return frozenOpenCodeAPIKeySelection{}, errors.New("OpenCode API-key retry tier has no candidates")
	}

	weights := make([]uint64, len(tier))
	var rawTotal uint64
	maxSamplerWeight := uint64(^uint(0) >> 1)
	for index, selection := range tier {
		weight := uint64(selection.weight)
		if weight > maxSamplerWeight || rawTotal > maxSamplerWeight-weight {
			return frozenOpenCodeAPIKeySelection{}, errors.New("OpenCode API-key retry weight exceeds sampler range")
		}
		weights[index] = weight
		rawTotal += weight
	}

	var total uint64
	for index, weight := range weights {
		if memoryCacheEnabled {
			switch {
			case allZero:
				weight = 100
			case rawTotal/uint64(len(tier)) < 10:
				if weight > maxSamplerWeight/100 {
					return frozenOpenCodeAPIKeySelection{}, errors.New("OpenCode API-key retry weight exceeds sampler range")
				}
				weight *= 100
			}
		} else {
			if weight > maxSamplerWeight-10 {
				return frozenOpenCodeAPIKeySelection{}, errors.New("OpenCode API-key retry weight overflows")
			}
			weight += 10
		}
		if total > maxSamplerWeight-weight {
			return frozenOpenCodeAPIKeySelection{}, errors.New("OpenCode API-key retry weight exceeds sampler range")
		}
		weights[index] = weight
		total += weight
	}
	if total == 0 {
		return frozenOpenCodeAPIKeySelection{}, errors.New("OpenCode API-key retry tier has no selectable weight")
	}
	draw := pick(int(total))
	if draw < 0 || uint64(draw) >= total {
		return frozenOpenCodeAPIKeySelection{}, errors.New("OpenCode API-key retry sampler returned an invalid value")
	}
	remaining := uint64(draw)
	for index, weight := range weights {
		// The database selector historically uses `remaining -= weight; <= 0`,
		// while the memory-cache selector uses the conventional `< weight`
		// boundary. Preserve both so freezing a retry topology cannot change the
		// selected-row distribution merely because the cache is disabled.
		if (memoryCacheEnabled && remaining < weight) || (!memoryCacheEnabled && remaining <= weight) {
			return tier[index], nil
		}
		remaining -= weight
	}
	return frozenOpenCodeAPIKeySelection{}, errors.New("OpenCode API-key retry sampler did not select a candidate")
}

func (snapshot *openCodeAPIKeyRetrySnapshot) selectAttempt(c *gin.Context, cursor int) (*model.Channel, error) {
	if snapshot == nil || cursor < 0 || cursor >= len(snapshot.selections) {
		return nil, errors.New("OpenCode API-key physical attempt cursor is out of range")
	}
	selection := snapshot.selections[cursor]
	if err := selection.apply(c); err != nil {
		return nil, err
	}
	autoBan := 0
	if selection.channelAutoBan {
		autoBan = 1
	}
	return &model.Channel{
		Id:      selection.channelID,
		Type:    constant.ChannelTypeOpenCodeAPIKey,
		Name:    selection.channelName,
		AutoBan: &autoBan,
	}, nil
}

func captureFrozenOpenCodeAPIKeySelection(c *gin.Context, selectionGroup string) (frozenOpenCodeAPIKeySelection, error) {
	if c == nil || common.GetContextKeyInt(c, constant.ContextKeyChannelType) != constant.ChannelTypeOpenCodeAPIKey {
		return frozenOpenCodeAPIKeySelection{}, errors.New("selected channel is not an OpenCode API-key row")
	}
	channelID := common.GetContextKeyInt(c, constant.ContextKeyChannelId)
	if channelID <= 0 {
		return frozenOpenCodeAPIKeySelection{}, errors.New("selected OpenCode API-key channel ID is invalid")
	}
	channelSetting, _ := common.GetContextKeyType[dto.ChannelSettings](c, constant.ContextKeyChannelSetting)
	channelOther, _ := common.GetContextKeyType[dto.ChannelOtherSettings](c, constant.ContextKeyChannelOtherSetting)
	clonedOther, err := cloneFrozenChannelOtherSettings(channelOther)
	if err != nil {
		return frozenOpenCodeAPIKeySelection{}, err
	}
	paramOverride, err := cloneFrozenChannelMap(common.GetContextKeyStringMap(c, constant.ContextKeyChannelParamOverride))
	if err != nil {
		return frozenOpenCodeAPIKeySelection{}, err
	}
	headerOverride, err := cloneFrozenChannelMap(common.GetContextKeyStringMap(c, constant.ContextKeyChannelHeaderOverride))
	if err != nil {
		return frozenOpenCodeAPIKeySelection{}, err
	}
	return frozenOpenCodeAPIKeySelection{
		selectionGroup:       strings.TrimSpace(selectionGroup),
		channelID:            channelID,
		channelName:          common.GetContextKeyString(c, constant.ContextKeyChannelName),
		channelCreateTime:    c.GetInt64(string(constant.ContextKeyChannelCreateTime)),
		channelAutoBan:       common.GetContextKeyBool(c, constant.ContextKeyChannelAutoBan),
		channelBaseURL:       common.GetContextKeyString(c, constant.ContextKeyChannelBaseUrl),
		channelKey:           common.GetContextKeyString(c, constant.ContextKeyChannelKey),
		organization:         common.GetContextKeyString(c, constant.ContextKeyChannelOrganization),
		modelMapping:         common.GetContextKeyString(c, constant.ContextKeyChannelModelMapping),
		statusCodeMapping:    common.GetContextKeyString(c, constant.ContextKeyChannelStatusCodeMapping),
		channelSetting:       channelSetting,
		channelOther:         clonedOther,
		paramOverride:        paramOverride,
		headerOverride:       headerOverride,
		originalModel:        common.GetContextKeyString(c, constant.ContextKeyOriginalModel),
		systemPromptOverride: common.GetContextKeyBool(c, constant.ContextKeySystemPromptOverride),
	}, nil
}

func (selection frozenOpenCodeAPIKeySelection) apply(c *gin.Context) error {
	if c == nil || selection.channelID <= 0 {
		return errors.New("frozen OpenCode API-key selection is invalid")
	}
	channelOther, err := cloneFrozenChannelOtherSettings(selection.channelOther)
	if err != nil {
		return err
	}
	paramOverride, err := cloneFrozenChannelMap(selection.paramOverride)
	if err != nil {
		return err
	}
	headerOverride, err := cloneFrozenChannelMap(selection.headerOverride)
	if err != nil {
		return err
	}

	for _, key := range []string{"api_version", "region", "plugin", "bot_id"} {
		c.Set(key, nil)
	}
	common.SetContextKey(c, constant.ContextKeyAutoGroup, selection.selectionGroup)
	common.SetContextKey(c, constant.ContextKeyOriginalModel, selection.originalModel)
	common.SetContextKey(c, constant.ContextKeyChannelId, selection.channelID)
	common.SetContextKey(c, constant.ContextKeyChannelName, selection.channelName)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenCodeAPIKey)
	common.SetContextKey(c, constant.ContextKeyChannelCreateTime, selection.channelCreateTime)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, selection.channelBaseURL)
	common.SetContextKey(c, constant.ContextKeyChannelKey, selection.channelKey)
	common.SetContextKey(c, constant.ContextKeyChannelOrganization, selection.organization)
	common.SetContextKey(c, constant.ContextKeyChannelAutoBan, selection.channelAutoBan)
	common.SetContextKey(c, constant.ContextKeyChannelModelMapping, selection.modelMapping)
	common.SetContextKey(c, constant.ContextKeyChannelStatusCodeMapping, selection.statusCodeMapping)
	common.SetContextKey(c, constant.ContextKeyChannelSetting, selection.channelSetting)
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, channelOther)
	common.SetContextKey(c, constant.ContextKeyChannelParamOverride, paramOverride)
	common.SetContextKey(c, constant.ContextKeyChannelHeaderOverride, headerOverride)
	common.SetContextKey(c, constant.ContextKeyChannelIsMultiKey, false)
	common.SetContextKey(c, constant.ContextKeyChannelMultiKeyIndex, 0)
	common.SetContextKey(c, constant.ContextKeySystemPromptOverride, selection.systemPromptOverride)
	common.SetContextKey(c, constant.ContextKeyOpenCodeGoWorkspaceUID, nil)
	return nil
}

func cloneFrozenChannelOtherSettings(source dto.ChannelOtherSettings) (dto.ChannelOtherSettings, error) {
	data, err := common.Marshal(source)
	if err != nil {
		return dto.ChannelOtherSettings{}, err
	}
	var clone dto.ChannelOtherSettings
	if err := common.Unmarshal(data, &clone); err != nil {
		return dto.ChannelOtherSettings{}, err
	}
	return clone, nil
}

func cloneFrozenChannelMap(source map[string]interface{}) (map[string]interface{}, error) {
	if source == nil {
		return nil, nil
	}
	clone, err := common.DeepCopy(&source)
	if err != nil {
		return nil, err
	}
	return *clone, nil
}
