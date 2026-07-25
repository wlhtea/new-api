package seedance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
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

var errResolutionMustBeString = errors.New("resolution must be a JSON string")

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
	if err != nil {
		return "", present, errResolutionMustBeString
	}
	if !present {
		return "", present, err
	}

	resolution, ok := resolutionAliases[strings.ToUpper(strings.TrimSpace(value))]
	if !ok {
		return "", true, errors.New("unsupported resolution")
	}
	return resolution, true, nil
}

func resolutionTaskError(err error) *dto.TaskError {
	code := "invalid_resolution"
	if errors.Is(err, errResolutionMustBeString) {
		code = "invalid_request"
	}
	return service.TaskErrorWrapperLocal(err, code, http.StatusBadRequest)
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
		return nil, resolutionTaskError(err)
	}
	metadataResolution, metadataPresent, err := parseResolutionValue(input.Metadata.Resolution)
	if err != nil {
		return nil, resolutionTaskError(err)
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

func quotedRawMessage(value string) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func singleMultipartValue(form *multipart.Form, name string) (string, bool, error) {
	values := form.Value[name]
	if len(values) == 0 {
		return "", false, nil
	}
	if len(values) != 1 {
		return "", true, fmt.Errorf("multipart field %s must occur once", name)
	}
	return values[0], true, nil
}

func normalizeMultipartBoolean(raw *json.RawMessage, name string) error {
	trimmed := bytes.TrimSpace(*raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var text string
	if err := common.Unmarshal(trimmed, &text); err != nil {
		return fmt.Errorf("multipart metadata field %s must be text true or false", name)
	}
	switch text {
	case "true":
		*raw = json.RawMessage("true")
	case "false":
		*raw = json.RawMessage("false")
	default:
		return fmt.Errorf("multipart metadata field %s must be text true or false", name)
	}
	return nil
}

func parseMultipartRequest(
	form *multipart.Form,
) (*requestInput, *imageCandidate, *dto.TaskError) {
	input := &requestInput{}
	stringFields := []struct {
		name string
		raw  *json.RawMessage
	}{
		{"model", &input.Raw.Model},
		{"prompt", &input.Raw.Prompt},
		{"duration", &input.Raw.Duration},
		{"seconds", &input.Raw.Seconds},
		{"size", &input.Raw.Size},
		{"image", &input.Raw.Image},
		{"input_reference", &input.Raw.InputReference},
	}
	for _, field := range stringFields {
		value, present, err := singleMultipartValue(form, field.name)
		if err != nil {
			return nil, nil, service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
		}
		if present {
			*field.raw = quotedRawMessage(value)
		}
	}

	if values := form.Value["images"]; len(values) != 0 {
		if len(values) == 1 && strings.HasPrefix(strings.TrimSpace(values[0]), "[") {
			input.Raw.Images = json.RawMessage(values[0])
		} else {
			encoded, err := json.Marshal(values)
			if err != nil {
				return nil, nil, service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
			}
			input.Raw.Images = encoded
		}
	}

	metadata, present, err := singleMultipartValue(form, "metadata")
	if err != nil {
		return nil, nil, service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
	}
	if present && strings.TrimSpace(metadata) != "" {
		if err := common.Unmarshal([]byte(metadata), &input.Metadata); err != nil {
			return nil, nil, service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
		}
		for _, boolean := range []struct {
			name string
			raw  *json.RawMessage
		}{
			{"prompt_optimization", &input.Metadata.PromptOptimization},
			{"multi_shot", &input.Metadata.MultiShot},
			{"strict_duration", &input.Metadata.StrictDuration},
		} {
			if err := normalizeMultipartBoolean(boolean.raw, boolean.name); err != nil {
				return nil, nil, service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
			}
		}
	}

	for name, files := range form.File {
		if name != "input_reference" && len(files) != 0 {
			return nil, nil, service.TaskErrorWrapperLocal(
				fmt.Errorf("unsupported multipart file field %s", name),
				"invalid_image",
				http.StatusBadRequest,
			)
		}
	}
	files := form.File["input_reference"]
	if len(files) > 1 {
		return nil, nil, service.TaskErrorWrapperLocal(
			errors.New("multipart input_reference must contain one file"),
			"invalid_image",
			http.StatusBadRequest,
		)
	}
	if len(files) == 0 {
		return input, nil, nil
	}

	file, err := files[0].Open()
	if err != nil {
		return nil, nil, service.TaskErrorWrapperLocal(err, "invalid_image", http.StatusBadRequest)
	}
	uploaded, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, nil, service.TaskErrorWrapperLocal(readErr, "invalid_image", http.StatusBadRequest)
	}
	if closeErr != nil {
		return nil, nil, service.TaskErrorWrapperLocal(closeErr, "invalid_image", http.StatusBadRequest)
	}
	return input, &imageCandidate{
		source: "multipart input_reference",
		bytes:  uploaded,
	}, nil
}

func cachedNormalizedRequest(c *gin.Context) (*NormalizedRequest, bool, *dto.TaskError) {
	cached, ok := c.Get(normalizedRequestContextKey)
	if !ok {
		return nil, false, nil
	}
	normalized, valid := cached.(*NormalizedRequest)
	if !valid || normalized == nil {
		return nil, true, service.TaskErrorWrapperLocal(
			errors.New("invalid normalized request cache"),
			"invalid_request",
			http.StatusBadRequest,
		)
	}
	return normalized, true, nil
}

func normalizeRequest(c *gin.Context) (*NormalizedRequest, *dto.TaskError) {
	if cached, found, taskErr := cachedNormalizedRequest(c); found {
		return cached, taskErr
	}
	return normalizeRequestWithLoader(c, service.GetImageFromUrl)
}

func normalizeRequestWithLoader(
	c *gin.Context,
	loadRemote remoteImageLoader,
) (*NormalizedRequest, *dto.TaskError) {
	if cached, found, taskErr := cachedNormalizedRequest(c); found {
		return cached, taskErr
	}

	var (
		input    *requestInput
		uploaded *imageCandidate
		taskErr  *dto.TaskError
	)
	contentType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil {
		return nil, service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
	}
	if contentType == "multipart/form-data" {
		form, err := common.ParseMultipartFormReusable(c)
		if err != nil {
			return nil, service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
		}
		defer form.RemoveAll()
		input, uploaded, taskErr = parseMultipartRequest(form)
	} else {
		input, taskErr = parseJSONRequest(c)
	}
	if taskErr != nil {
		return nil, taskErr
	}

	normalized, taskErr := normalizeScalars(input)
	if taskErr != nil {
		return nil, taskErr
	}
	normalized.ImageBase64, taskErr = normalizeImages(input, uploaded, loadRemote)
	if taskErr != nil {
		return nil, taskErr
	}
	if normalized.ImageBase64 == "" && normalized.Resolution == "480P" {
		return nil, service.TaskErrorWrapperLocal(
			errors.New("480P resolution requires an input image"),
			"invalid_resolution",
			http.StatusBadRequest,
		)
	}

	c.Set(normalizedRequestContextKey, normalized)
	return normalized, nil
}

func getNormalizedRequest(c *gin.Context) (*NormalizedRequest, error) {
	cached, ok := c.Get(normalizedRequestContextKey)
	if !ok {
		return nil, errors.New("Seed Dance request was not normalized")
	}
	normalized, ok := cached.(*NormalizedRequest)
	if !ok || normalized == nil {
		return nil, errors.New("invalid Seed Dance normalized request")
	}
	return normalized, nil
}
