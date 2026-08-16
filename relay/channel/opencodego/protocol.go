package opencodego

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
)

type Protocol string

const (
	ProtocolChat      Protocol = dto.OpenCodeGoProtocolChat
	ProtocolMessages  Protocol = dto.OpenCodeGoProtocolMessages
	ProtocolResponses Protocol = dto.OpenCodeGoProtocolResponses
)

type ProtocolResolutionSource string

const (
	ProtocolSourceExactOverride     ProtocolResolutionSource = "exact-override"
	ProtocolSourceWildcardOverride  ProtocolResolutionSource = "wildcard-override"
	ProtocolSourceExactBuiltIn      ProtocolResolutionSource = "exact-built-in"
	ProtocolSourceFamilyFallback    ProtocolResolutionSource = "family-fallback"
	ProtocolSourceConfiguredDefault ProtocolResolutionSource = "configured-default"
)

type ProtocolResolution struct {
	Protocol       Protocol
	Source         ProtocolResolutionSource
	MatchedPattern string
}

func ResolveProtocol(model string, config *dto.OpenCodeGoConfig) (Protocol, error) {
	resolution, err := ResolveProtocolWithSource(model, config)
	return resolution.Protocol, err
}

func ResolveProtocolWithSource(model string, config *dto.OpenCodeGoConfig) (ProtocolResolution, error) {
	normalizedModel := strings.ToLower(strings.TrimSpace(model))
	if normalizedModel == "" {
		return ProtocolResolution{}, fmt.Errorf("OpenCode Go upstream model is empty")
	}

	if config != nil {
		if err := config.ValidateProtocolRouting(); err != nil {
			return ProtocolResolution{}, fmt.Errorf("invalid OpenCode Go protocol configuration: %w", err)
		}
		type protocolOverride struct {
			pattern  string
			protocol string
		}
		overrides := make([]protocolOverride, 0, len(config.ModelProtocols))
		for pattern, value := range config.ModelProtocols {
			overrides = append(overrides, protocolOverride{
				pattern:  strings.ToLower(strings.TrimSpace(pattern)),
				protocol: value,
			})
		}
		sort.Slice(overrides, func(i, j int) bool {
			if overrides[i].pattern == overrides[j].pattern {
				return overrides[i].protocol < overrides[j].protocol
			}
			return overrides[i].pattern < overrides[j].pattern
		})
		for i := 1; i < len(overrides); i++ {
			if overrides[i-1].pattern == overrides[i].pattern {
				return ProtocolResolution{}, fmt.Errorf("duplicate OpenCode Go model protocol pattern %q", overrides[i].pattern)
			}
		}

		for _, override := range overrides {
			if strings.ContainsAny(override.pattern, "*?[") || override.pattern != normalizedModel {
				continue
			}
			protocol, err := parseProtocol(override.protocol)
			if err != nil {
				return ProtocolResolution{}, err
			}
			return ProtocolResolution{
				Protocol:       protocol,
				Source:         ProtocolSourceExactOverride,
				MatchedPattern: override.pattern,
			}, nil
		}

		patterns := make([]protocolOverride, 0, len(overrides))
		for _, override := range overrides {
			if strings.ContainsAny(override.pattern, "*?[") {
				patterns = append(patterns, override)
			}
		}
		sort.Slice(patterns, func(i, j int) bool {
			if len(patterns[i].pattern) == len(patterns[j].pattern) {
				return patterns[i].pattern < patterns[j].pattern
			}
			return len(patterns[i].pattern) > len(patterns[j].pattern)
		})
		for _, override := range patterns {
			matched, err := path.Match(override.pattern, normalizedModel)
			if err != nil {
				return ProtocolResolution{}, fmt.Errorf("invalid OpenCode Go model protocol pattern %q: %w", override.pattern, err)
			}
			if !matched {
				continue
			}
			protocol, err := parseProtocol(override.protocol)
			if err != nil {
				return ProtocolResolution{}, err
			}
			return ProtocolResolution{
				Protocol:       protocol,
				Source:         ProtocolSourceWildcardOverride,
				MatchedPattern: override.pattern,
			}, nil
		}
	}

	if protocol, ok := builtInModelProtocols[normalizedModel]; ok {
		return ProtocolResolution{Protocol: protocol, Source: ProtocolSourceExactBuiltIn}, nil
	}
	if protocol, ok := resolveModelFamily(normalizedModel); ok {
		return ProtocolResolution{Protocol: protocol, Source: ProtocolSourceFamilyFallback}, nil
	}
	if config != nil && strings.TrimSpace(config.DefaultProtocol) != "" {
		protocol, err := parseProtocol(config.DefaultProtocol)
		if err != nil {
			return ProtocolResolution{}, err
		}
		return ProtocolResolution{Protocol: protocol, Source: ProtocolSourceConfiguredDefault}, nil
	}
	return ProtocolResolution{}, fmt.Errorf("OpenCode Go protocol is not configured for upstream model %q", model)
}

func resolveModelFamily(model string) (Protocol, bool) {
	if slash := strings.LastIndex(model, "/"); slash >= 0 {
		model = model[slash+1:]
	}
	switch {
	case strings.HasPrefix(model, "gpt-5.6-luna"):
		return ProtocolResponses, true
	case strings.HasPrefix(model, "minimax-"), strings.HasPrefix(model, "qwen"):
		return ProtocolMessages, true
	case strings.HasPrefix(model, "grok-"),
		strings.HasPrefix(model, "glm-"),
		strings.HasPrefix(model, "kimi-"),
		strings.HasPrefix(model, "deepseek-"),
		strings.HasPrefix(model, "mimo-"),
		model == "hy3",
		strings.HasPrefix(model, "hy3-"):
		return ProtocolChat, true
	default:
		return "", false
	}
}

func parseProtocol(value string) (Protocol, error) {
	switch Protocol(strings.ToLower(strings.TrimSpace(value))) {
	case ProtocolChat:
		return ProtocolChat, nil
	case ProtocolMessages:
		return ProtocolMessages, nil
	case ProtocolResponses:
		return ProtocolResponses, nil
	default:
		return "", fmt.Errorf("expected chat, messages, or responses, got %q", value)
	}
}

func (p Protocol) RelayFormat() types.RelayFormat {
	switch p {
	case ProtocolMessages:
		return types.RelayFormatClaude
	case ProtocolResponses:
		return types.RelayFormatOpenAIResponses
	default:
		return types.RelayFormatOpenAI
	}
}
