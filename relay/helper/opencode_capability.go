package helper

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
)

const (
	OpenCodeCapabilityStage                    = "preflight.capability"
	OpenCodeGLM53ThinkingDisabledRule          = "model.glm-5.3.chat.thinking-disabled"
	OpenCodeGLM53ThinkingDisabledPublicMessage = "所选模型不支持关闭思考"
)

// CanonicalOpenCodeModelName is used only for exact policy identity. Provider
// prefixes are routing namespaces, so the final path segment is the canonical
// model ID; family-prefix matching is deliberately not used for policy rules.
func CanonicalOpenCodeModelName(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if separator := strings.LastIndexByte(model, '/'); separator >= 0 {
		model = model[separator+1:]
	}
	return model
}

func ValidateOpenCodeModelCapability(
	envelope *ValidatedRequestEnvelope,
	finalModel string,
	finalProtocol string,
) error {
	if envelope == nil {
		return errors.New("validated request envelope is nil")
	}
	if strings.ToLower(strings.TrimSpace(finalProtocol)) != "chat" ||
		CanonicalOpenCodeModelName(finalModel) != "glm-5.3" {
		return nil
	}

	rawType, kind, present, err := envelope.RawObjectPath("thinking", "type")
	if err != nil || !present || kind != JSONValueString {
		return err
	}
	var thinkingType string
	if err := decodeJSONSpan(rawType, &thinkingType); err != nil {
		return err
	}
	if thinkingType != "disabled" {
		return nil
	}
	return &ClientRequestValidationError{
		StatusCode: http.StatusBadRequest,
		Message:    OpenCodeGLM53ThinkingDisabledPublicMessage,
		RuleID:     OpenCodeGLM53ThinkingDisabledRule,
		StageID:    OpenCodeCapabilityStage,
	}
}

func decodeJSONSpan(raw []byte, destination any) error {
	return common.Unmarshal(raw, destination)
}
