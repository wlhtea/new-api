package helper

import (
	"bufio"
	"errors"
	"strings"

	rootcommon "github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
)

var (
	ErrModelMappingMalformed   = errors.New("model mapping is malformed")
	ErrModelMappingInvalid     = errors.New("model mapping is invalid")
	ErrModelMappingCycle       = errors.New("model mapping contains a cycle")
	ErrModelMappingEmptyOrigin = errors.New("model mapping origin is empty")
)

type ModelMappingResolution struct {
	OriginModel string
	FinalModel  string
	Mapped      bool
	Configured  bool
	ChainLength int
}

type normalizedModelMappingEntry struct {
	target string
}

// ResolveModelMapping is the side-effect-free source of truth used by both
// request preflight and the relay handlers. Lookup is case-insensitive and
// ignores surrounding whitespace, while the configured target's casing is
// preserved on the wire. Normalized duplicate sources are rejected so map
// iteration order can never decide routing.
func ResolveModelMapping(originModel string, rawMapping string) (ModelMappingResolution, error) {
	originModel = strings.TrimSpace(originModel)
	resolution := ModelMappingResolution{
		OriginModel: originModel,
		FinalModel:  originModel,
		ChainLength: 1,
	}
	if originModel == "" {
		return ModelMappingResolution{}, ErrModelMappingEmptyOrigin
	}

	rawMapping = strings.TrimSpace(rawMapping)
	if rawMapping == "" || rawMapping == "{}" {
		return resolution, nil
	}
	resolution.Configured = true

	parsed, err := parseModelMapping(rawMapping)
	if err != nil {
		return ModelMappingResolution{}, err
	}

	normalized := make(map[string]normalizedModelMappingEntry, len(parsed))
	for source, target := range parsed {
		sourceKey := normalizeModelMappingKey(source)
		target = strings.TrimSpace(target)
		if sourceKey == "" || target == "" {
			return ModelMappingResolution{}, ErrModelMappingInvalid
		}
		if _, duplicate := normalized[sourceKey]; duplicate {
			return ModelMappingResolution{}, ErrModelMappingInvalid
		}
		normalized[sourceKey] = normalizedModelMappingEntry{target: target}
	}

	current := originModel
	visited := map[string]struct{}{normalizeModelMappingKey(current): {}}
	for {
		entry, exists := normalized[normalizeModelMappingKey(current)]
		if !exists {
			break
		}

		targetKey := normalizeModelMappingKey(entry.target)
		currentKey := normalizeModelMappingKey(current)
		if targetKey == currentKey {
			if entry.target != current {
				current = entry.target
				resolution.Mapped = true
				resolution.ChainLength++
			}
			break
		}
		if _, exists := visited[targetKey]; exists {
			return ModelMappingResolution{}, ErrModelMappingCycle
		}
		visited[targetKey] = struct{}{}
		current = entry.target
		resolution.Mapped = true
		resolution.ChainLength++
	}

	resolution.FinalModel = current
	return resolution, nil
}

func parseModelMapping(rawMapping string) (map[string]string, error) {
	parser := strictJSONParser{
		reader: bufio.NewReader(strings.NewReader(rawMapping)),
		limits: defaultStrictJSONLimits,
	}
	rootIndex, err := parser.parseDocument()
	if err != nil || parser.readErr != nil {
		var ruleErr *strictJSONRuleError
		if errors.As(err, &ruleErr) && ruleErr.ruleID == "json.duplicate_key" {
			return nil, ErrModelMappingInvalid
		}
		return nil, ErrModelMappingMalformed
	}
	if rootIndex < 0 || rootIndex >= len(parser.values) || parser.values[rootIndex].kind != JSONValueObject {
		return nil, ErrModelMappingMalformed
	}

	parsed := make(map[string]string)
	if err := rootcommon.UnmarshalJsonStr(rawMapping, &parsed); err != nil || parsed == nil {
		return nil, ErrModelMappingMalformed
	}
	return parsed, nil
}

func normalizeModelMappingKey(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

func ModelMappedHelper(c *gin.Context, info *relaycommon.RelayInfo, request dto.Request) error {
	if info.ChannelMeta == nil {
		info.ChannelMeta = &relaycommon.ChannelMeta{}
	}

	resolution, err := ResolveModelMapping(info.OriginModelName, c.GetString("model_mapping"))
	if err != nil {
		return err
	}
	info.IsModelMapped = resolution.Mapped
	info.UpstreamModelName = resolution.FinalModel

	if request != nil {
		request.SetModelName(info.UpstreamModelName)
	}
	return nil
}
