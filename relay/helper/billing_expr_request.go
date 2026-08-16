package helper

import (
	"errors"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func ResolveIncomingBillingExprRequestInput(c *gin.Context, info *relaycommon.RelayInfo) (billingexpr.RequestInput, error) {
	if info != nil && info.BillingRequestInput != nil {
		input := cloneRequestInput(*info.BillingRequestInput)
		merged := cloneStringMap(info.RequestHeaders)
		for k, v := range input.Headers {
			merged[k] = v
		}
		input.Headers = merged
		return input, nil
	}

	input := billingexpr.RequestInput{}
	if info != nil {
		input.Headers = cloneStringMap(info.RequestHeaders)
	}
	if c != nil && info != nil {
		envelope, found, err := GetValidatedRequestEnvelope(c, info.RelayFormat)
		if err != nil {
			return billingexpr.RequestInput{}, err
		}
		if found && envelope != nil {
			input.ResolveParam = envelope.billingExprParamResolver()
			return input, nil
		}
	}

	bodyBytes, err := readIncomingBillingExprBody(c)
	if err != nil {
		return billingexpr.RequestInput{}, err
	}
	input.Body = bodyBytes
	return input, nil
}

func BuildBillingExprRequestInputFromRequest(request dto.Request, headers map[string]string) (billingexpr.RequestInput, error) {
	input := billingexpr.RequestInput{
		Headers: cloneStringMap(headers),
	}
	if request == nil {
		return input, nil
	}

	bodyBytes, err := common.Marshal(request)
	if err != nil {
		return billingexpr.RequestInput{}, err
	}
	input.Body = bodyBytes
	return input, nil
}

func readIncomingBillingExprBody(c *gin.Context) ([]byte, error) {
	if c == nil || c.Request == nil || !isJSONContentType(c.Request.Header.Get("Content-Type")) {
		return nil, nil
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, err
	}
	return storage.Bytes()
}

func cloneRequestInput(src billingexpr.RequestInput) billingexpr.RequestInput {
	input := billingexpr.RequestInput{
		Headers:      cloneStringMap(src.Headers),
		ResolveParam: src.ResolveParam,
	}
	if len(src.Body) > 0 {
		input.Body = append([]byte(nil), src.Body...)
	}
	return input
}

func (e *ValidatedRequestEnvelope) billingExprParamResolver() billingexpr.ParamResolver {
	return func(path string) (interface{}, bool, error) {
		topLevel, remainder, err := splitBillingExprGJSONPath(path)
		if err != nil {
			return nil, false, err
		}
		raw, found, err := e.RawTopLevelField(topLevel)
		if err != nil || !found {
			return nil, found, err
		}
		result := gjson.ParseBytes(raw)
		if remainder != "" {
			result = gjson.GetBytes(raw, remainder)
		}
		if !result.Exists() {
			return nil, false, nil
		}
		return result.Value(), true, nil
	}
}

func splitBillingExprGJSONPath(path string) (string, string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", "", errors.New("billing parameter path is empty")
	}
	var topLevel strings.Builder
	escaped := false
	for index := 0; index < len(path); index++ {
		current := path[index]
		if escaped {
			topLevel.WriteByte(current)
			escaped = false
			continue
		}
		if current == '\\' {
			escaped = true
			continue
		}
		if current == '.' {
			if topLevel.Len() == 0 {
				return "", "", errors.New("billing parameter path has no top-level field")
			}
			return topLevel.String(), path[index+1:], nil
		}
		switch current {
		case '#', '|', '!', '@', '*', '?':
			return "", "", errors.New("billing parameter path must begin with an exact top-level field")
		}
		topLevel.WriteByte(current)
	}
	if escaped {
		return "", "", errors.New("billing parameter path has an incomplete escape")
	}
	if topLevel.Len() == 0 {
		return "", "", errors.New("billing parameter path has no top-level field")
	}
	return topLevel.String(), "", nil
}

func isJSONContentType(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	return strings.HasPrefix(contentType, "application/json")
}

func cloneStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return map[string]string{}
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		if strings.TrimSpace(key) == "" {
			continue
		}
		dst[key] = value
	}
	return dst
}
