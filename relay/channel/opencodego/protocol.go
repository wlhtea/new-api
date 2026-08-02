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

var builtInModelProtocols = map[string]Protocol{
	"grok-4.5":          ProtocolChat,
	"glm-5.2":           ProtocolChat,
	"glm-5.1":           ProtocolChat,
	"kimi-k3":           ProtocolChat,
	"kimi-k2.7-code":    ProtocolChat,
	"kimi-k2.6":         ProtocolChat,
	"deepseek-v4-pro":   ProtocolChat,
	"deepseek-v4-flash": ProtocolChat,
	"mimo-v2.5":         ProtocolChat,
	"mimo-v2.5-pro":     ProtocolChat,
	"hy3":               ProtocolChat,
	"minimax-m3":        ProtocolMessages,
	"minimax-m2.7":      ProtocolMessages,
	"minimax-m2.5":      ProtocolMessages,
	"qwen3.7-max":       ProtocolMessages,
	"qwen3.7-plus":      ProtocolMessages,
	"qwen3.6-plus":      ProtocolMessages,
	"gpt-5.6-luna":      ProtocolResponses,
}

func ResolveProtocol(model string, config *dto.OpenCodeGoConfig) (Protocol, error) {
	normalizedModel := strings.ToLower(strings.TrimSpace(model))
	if normalizedModel == "" {
		return "", fmt.Errorf("OpenCode Go upstream model is empty")
	}

	if config != nil {
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
				return "", fmt.Errorf("duplicate OpenCode Go model protocol pattern %q", overrides[i].pattern)
			}
		}

		for _, override := range overrides {
			if strings.ContainsAny(override.pattern, "*?[") || override.pattern != normalizedModel {
				continue
			}
			return parseProtocol(override.protocol)
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
				return "", fmt.Errorf("invalid OpenCode Go model protocol pattern %q: %w", override.pattern, err)
			}
			if !matched {
				continue
			}
			return parseProtocol(override.protocol)
		}
	}

	if protocol, ok := builtInModelProtocols[normalizedModel]; ok {
		return protocol, nil
	}
	if protocol, ok := resolveModelFamily(normalizedModel); ok {
		return protocol, nil
	}
	if config != nil && strings.TrimSpace(config.DefaultProtocol) != "" {
		return parseProtocol(config.DefaultProtocol)
	}
	return "", fmt.Errorf("OpenCode Go protocol is not configured for upstream model %q", model)
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
