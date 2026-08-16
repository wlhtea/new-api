package service

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

var (
	ErrOpenCodeAPIKeyMixedTopology        = errors.New("OpenCode API-key retry group contains another channel type")
	ErrOpenCodeAPIKeyInconsistentTopology = errors.New("OpenCode API-key channel and ability topology are inconsistent")
)

// OpenCodeAPIKeyTopologyCandidate is one independently eligible Type-63 row in
// one concrete selection group. Channel is a caller-owned deep copy.
type OpenCodeAPIKeyTopologyCandidate struct {
	SelectionGroup string
	Priority       int64
	Weight         uint
	Channel        *model.Channel
}

type openCodeAPIKeyGroupAbilities struct {
	group      string
	exact      []model.Ability
	normalized []model.Ability
}

type openCodeAPIKeyTopologyKey struct {
	selectionGroup string
	channelID      int
}

// SnapshotOpenCodeAPIKeyCandidateTopology returns every enabled Type-63
// (selection group, channel ID) candidate for a request exactly once. It reads
// configuration only: it does not sample weights, consult affinity, mutate the
// request selection cursor, update channel/cache state, or write the database.
func SnapshotOpenCodeAPIKeyCandidateTopology(
	c *gin.Context,
	tokenGroup string,
	userGroup string,
	modelName string,
	requestPath string,
) ([]OpenCodeAPIKeyTopologyCandidate, error) {
	if c == nil {
		return nil, errors.New("OpenCode API-key topology context is nil")
	}
	tokenGroup = strings.TrimSpace(tokenGroup)
	modelName = strings.TrimSpace(modelName)
	if tokenGroup == "" || modelName == "" {
		return nil, errors.New("OpenCode API-key topology request is incomplete")
	}
	if !model.IsOpenCodeSupportedRequestPath(requestPath) {
		return []OpenCodeAPIKeyTopologyCandidate{}, nil
	}

	groups, err := resolveOpenCodeAPIKeyTopologyGroups(c, tokenGroup, userGroup)
	if err != nil {
		return nil, err
	}
	normalizedModel := ratio_setting.FormatMatchingModelName(modelName)
	var topology []OpenCodeAPIKeyTopologyCandidate
	err = model.DB.Transaction(func(tx *gorm.DB) error {
		var snapshotErr error
		topology, snapshotErr = snapshotOpenCodeAPIKeyCandidateTopology(
			tx,
			groups,
			modelName,
			normalizedModel,
			requestPath,
		)
		return snapshotErr
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("snapshot OpenCode API-key topology: %w", err)
	}
	return topology, nil
}

func snapshotOpenCodeAPIKeyCandidateTopology(
	tx *gorm.DB,
	groups []string,
	modelName string,
	normalizedModel string,
	requestPath string,
) ([]OpenCodeAPIKeyTopologyCandidate, error) {
	if tx == nil {
		return nil, errors.New("OpenCode API-key topology database is nil")
	}
	groupAbilities := make([]openCodeAPIKeyGroupAbilities, 0, len(groups))
	channelIDs := make(map[int]struct{})
	for _, group := range groups {
		exact, queryErr := loadEnabledOpenCodeAPIKeyAbilities(tx, group, modelName)
		if queryErr != nil {
			return nil, queryErr
		}
		var normalized []model.Ability
		if normalizedModel != "" && normalizedModel != modelName {
			normalized, queryErr = loadEnabledOpenCodeAPIKeyAbilities(tx, group, normalizedModel)
			if queryErr != nil {
				return nil, queryErr
			}
		}
		groupAbilities = append(groupAbilities, openCodeAPIKeyGroupAbilities{
			group:      group,
			exact:      exact,
			normalized: normalized,
		})
		for _, ability := range append(append([]model.Ability(nil), exact...), normalized...) {
			channelIDs[ability.ChannelId] = struct{}{}
		}
	}
	if len(channelIDs) == 0 {
		return []OpenCodeAPIKeyTopologyCandidate{}, nil
	}

	ids := make([]int, 0, len(channelIDs))
	for channelID := range channelIDs {
		ids = append(ids, channelID)
	}
	sort.Ints(ids)
	var channels []model.Channel
	if err := tx.Where("id IN ?", ids).Find(&channels).Error; err != nil {
		return nil, fmt.Errorf("load OpenCode API-key topology channels: %w", err)
	}
	channelsByID := make(map[int]*model.Channel, len(channels))
	for index := range channels {
		channelsByID[channels[index].Id] = &channels[index]
	}
	if len(channelsByID) != len(channelIDs) {
		return nil, errors.New("OpenCode API-key topology references a missing channel")
	}

	topology := make([]OpenCodeAPIKeyTopologyCandidate, 0, len(channelIDs))
	seen := make(map[openCodeAPIKeyTopologyKey]struct{}, len(channelIDs))
	for _, abilities := range groupAbilities {
		routeAbilities := abilities.exact
		if len(routeAbilities) == 0 {
			routeAbilities = abilities.normalized
		}
		if err := validateOpenCodeAPIKeyTopologyAbilities(
			routeAbilities,
			channelsByID,
		); err != nil {
			return nil, err
		}
		routeAbilities = filterEligibleOpenCodeTopologyAbilities(routeAbilities, channelsByID, requestPath, modelName)
		selected := filterOpenCodeAPIKeyAbilities(routeAbilities, channelsByID)
		for _, ability := range selected {
			key := openCodeAPIKeyTopologyKey{
				selectionGroup: abilities.group,
				channelID:      ability.ChannelId,
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			channelCopy, copyErr := cloneOpenCodeAPIKeyTopologyChannel(channelsByID[ability.ChannelId])
			if copyErr != nil {
				return nil, fmt.Errorf("copy OpenCode API-key topology channel: %w", copyErr)
			}
			priority := int64(0)
			if ability.Priority != nil {
				priority = *ability.Priority
			}
			topology = append(topology, OpenCodeAPIKeyTopologyCandidate{
				SelectionGroup: abilities.group,
				Priority:       priority,
				Weight:         ability.Weight,
				Channel:        channelCopy,
			})
		}
	}

	groupOrder := make(map[string]int, len(groups))
	for index, group := range groups {
		groupOrder[group] = index
	}
	sort.Slice(topology, func(i, j int) bool {
		left := topology[i]
		right := topology[j]
		if groupOrder[left.SelectionGroup] != groupOrder[right.SelectionGroup] {
			return groupOrder[left.SelectionGroup] < groupOrder[right.SelectionGroup]
		}
		if left.Priority != right.Priority {
			return left.Priority > right.Priority
		}
		if left.Weight != right.Weight {
			return left.Weight > right.Weight
		}
		return left.Channel.Id < right.Channel.Id
	})
	return topology, nil
}

func validateOpenCodeAPIKeyTopologyAbilities(
	abilities []model.Ability,
	channelsByID map[int]*model.Channel,
) error {
	for _, ability := range abilities {
		channel := channelsByID[ability.ChannelId]
		if channel == nil {
			continue
		}
		if channel.Type != constant.ChannelTypeOpenCodeAPIKey {
			return fmt.Errorf("%w: group=%s channel_id=%d", ErrOpenCodeAPIKeyMixedTopology, ability.Group, ability.ChannelId)
		}
		if channel.Status != common.ChannelStatusEnabled ||
			!openCodeAPIKeyChannelDeclaresAbility(channel, ability.Group, ability.Model) ||
			channel.GetPriority() != openCodeAPIKeyAbilityPriority(ability) ||
			uint(channel.GetWeight()) != ability.Weight {
			return fmt.Errorf(
				"%w: group=%s channel_id=%d",
				ErrOpenCodeAPIKeyInconsistentTopology,
				ability.Group,
				ability.ChannelId,
			)
		}
	}
	return nil
}

func openCodeAPIKeyAbilityPriority(ability model.Ability) int64 {
	if ability.Priority == nil {
		return 0
	}
	return *ability.Priority
}

func openCodeAPIKeyChannelDeclaresAbility(channel *model.Channel, group string, modelName string) bool {
	if channel == nil {
		return false
	}
	groupDeclared := false
	for _, candidate := range strings.Split(channel.Group, ",") {
		if candidate == group {
			groupDeclared = true
			break
		}
	}
	if !groupDeclared {
		return false
	}
	for _, candidate := range strings.Split(channel.Models, ",") {
		if candidate == modelName {
			return true
		}
	}
	return false
}

func filterEligibleOpenCodeTopologyAbilities(
	abilities []model.Ability,
	channelsByID map[int]*model.Channel,
	requestPath string,
	modelName string,
) []model.Ability {
	eligible := make([]model.Ability, 0, len(abilities))
	for _, ability := range abilities {
		channel := channelsByID[ability.ChannelId]
		if channel == nil || channel.Status != common.ChannelStatusEnabled ||
			!model.ChannelSupportsRequestPath(channel, requestPath, modelName) {
			continue
		}
		eligible = append(eligible, ability)
	}
	return eligible
}

func filterOpenCodeAPIKeyAbilities(
	abilities []model.Ability,
	channelsByID map[int]*model.Channel,
) []model.Ability {
	eligible := make([]model.Ability, 0, len(abilities))
	for _, ability := range abilities {
		channel := channelsByID[ability.ChannelId]
		if channel != nil && channel.Type == constant.ChannelTypeOpenCodeAPIKey {
			eligible = append(eligible, ability)
		}
	}
	return eligible
}

func cloneOpenCodeAPIKeyTopologyChannel(source *model.Channel) (*model.Channel, error) {
	if source == nil {
		return nil, errors.New("OpenCode API-key topology channel is nil")
	}
	data, err := common.Marshal(source)
	if err != nil {
		return nil, err
	}
	var clone model.Channel
	if err := common.Unmarshal(data, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

func resolveOpenCodeAPIKeyTopologyGroups(c *gin.Context, tokenGroup string, userGroup string) ([]string, error) {
	if tokenGroup != "auto" {
		return []string{tokenGroup}, nil
	}
	groups := GetRequestAutoGroups(c, userGroup)
	if len(groups) == 0 {
		return nil, errors.New("auto groups is not enabled")
	}
	return append([]string(nil), groups...), nil
}

func loadEnabledOpenCodeAPIKeyAbilities(tx *gorm.DB, group string, modelName string) ([]model.Ability, error) {
	var abilities []model.Ability
	query := tx.Where(&model.Ability{
		Group:   group,
		Model:   modelName,
		Enabled: true,
	})
	if err := query.Find(&abilities).Error; err != nil {
		return nil, fmt.Errorf("load OpenCode API-key topology abilities: %w", err)
	}
	return abilities, nil
}

func filterEligibleOpenCodeAPIKeyAbilities(
	abilities []model.Ability,
	channelsByID map[int]*model.Channel,
	requestPath string,
	modelName string,
) []model.Ability {
	return filterOpenCodeAPIKeyAbilities(
		filterEligibleOpenCodeTopologyAbilities(abilities, channelsByID, requestPath, modelName),
		channelsByID,
	)
}
