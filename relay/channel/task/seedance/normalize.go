package seedance

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

var resolutionAliases = map[string]string{
	"854X480":   "480P",
	"480X854":   "480P",
	"480P":      "480P",
	"1280X720":  "720P",
	"720X1280":  "720P",
	"720P":      "720P",
	"1920X1080": "1080P",
	"1080X1920": "1080P",
	"1080P":     "1080P",
}

func parseJSONRequest(c *gin.Context) (*requestInput, *dto.TaskError) {
	var raw rawJSONRequest
	if err := common.UnmarshalBodyReusable(c, &raw); err != nil {
		return nil, service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
	}

	input := &requestInput{Raw: raw}
	metadata := bytes.TrimSpace(raw.Metadata)
	if len(metadata) != 0 && !bytes.Equal(metadata, []byte("null")) {
		if err := common.Unmarshal(metadata, &input.Metadata); err != nil {
			return nil, service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
		}
	}
	return input, nil
}

func parseDurationValue(raw json.RawMessage) (int, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, false, nil
	}

	text := string(trimmed)
	if trimmed[0] == '"' {
		var value string
		if err := common.Unmarshal(trimmed, &value); err != nil {
			return 0, true, err
		}
		text = value
	}
	if text == "" {
		return 0, true, errors.New("duration must not be empty")
	}
	for _, char := range text {
		if char < '0' || char > '9' {
			return 0, true, errors.New("duration must be a decimal integer")
		}
	}
	value, err := strconv.ParseInt(text, 10, 32)
	if err != nil {
		return 0, true, err
	}
	if value < 1 || value > 15 || value > relaycommon.MaxTaskDurationSeconds {
		return 0, true, errors.New("duration must be between 1 and 15")
	}
	return int(value), true, nil
}

func normalizeDuration(input *requestInput) (int, *dto.TaskError) {
	sources := []json.RawMessage{
		input.Raw.Duration,
		input.Raw.Seconds,
		input.Metadata.Duration,
	}
	value := defaultDuration
	found := false
	for _, source := range sources {
		candidate, present, err := parseDurationValue(source)
		if err != nil {
			return 0, service.TaskErrorWrapperLocal(err, "invalid_duration", http.StatusBadRequest)
		}
		if !present {
			continue
		}
		if found && candidate != value {
			return 0, service.TaskErrorWrapperLocal(
				errors.New("duration fields conflict"), "invalid_duration", http.StatusBadRequest,
			)
		}
		value = candidate
		found = true
	}
	return value, nil
}

func parseStringValue(raw json.RawMessage) (string, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", false, nil
	}
	var value string
	if err := common.Unmarshal(trimmed, &value); err != nil {
		return "", true, err
	}
	return value, true, nil
}

func parseResolutionValue(raw json.RawMessage) (string, bool, error) {
	value, present, err := parseStringValue(raw)
	if err != nil || !present {
		return "", present, err
	}

	resolution, ok := resolutionAliases[strings.ToUpper(strings.TrimSpace(value))]
	if !ok {
		return "", true, errors.New("unsupported resolution")
	}
	return resolution, true, nil
}

func parseOptionalBoolean(raw json.RawMessage) (*bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var value bool
	if err := common.Unmarshal(trimmed, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func normalizeScalars(input *requestInput) (*NormalizedRequest, *dto.TaskError) {
	prompt, _, err := parseStringValue(input.Raw.Prompt)
	if err != nil {
		return nil, service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, service.TaskErrorWrapperLocal(
			errors.New("prompt is required"), "invalid_request", http.StatusBadRequest,
		)
	}

	duration, taskErr := normalizeDuration(input)
	if taskErr != nil {
		return nil, taskErr
	}

	size, sizePresent, err := parseResolutionValue(input.Raw.Size)
	if err != nil {
		return nil, service.TaskErrorWrapperLocal(err, "invalid_resolution", http.StatusBadRequest)
	}
	metadataResolution, metadataPresent, err := parseResolutionValue(input.Metadata.Resolution)
	if err != nil {
		return nil, service.TaskErrorWrapperLocal(err, "invalid_resolution", http.StatusBadRequest)
	}
	if sizePresent && metadataPresent && size != metadataResolution {
		return nil, service.TaskErrorWrapperLocal(
			errors.New("resolution fields conflict"), "invalid_resolution", http.StatusBadRequest,
		)
	}
	resolution := defaultResolution
	if sizePresent {
		resolution = size
	} else if metadataPresent {
		resolution = metadataResolution
	}

	promptOptimization, err := parseOptionalBoolean(input.Metadata.PromptOptimization)
	if err != nil {
		return nil, service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
	}
	multiShot, err := parseOptionalBoolean(input.Metadata.MultiShot)
	if err != nil {
		return nil, service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
	}
	strictDuration, err := parseOptionalBoolean(input.Metadata.StrictDuration)
	if err != nil {
		return nil, service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
	}
	negativePrompt, _, err := parseStringValue(input.Metadata.NegativePrompt)
	if err != nil {
		return nil, service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
	}

	return &NormalizedRequest{
		Prompt:             prompt,
		Resolution:         resolution,
		Duration:           duration,
		PromptOptimization: promptOptimization,
		MultiShot:          multiShot,
		StrictDuration:     strictDuration,
		NegativePrompt:     negativePrompt,
	}, nil
}
