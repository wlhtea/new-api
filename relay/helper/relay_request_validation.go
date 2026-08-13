package helper

import (
	"bytes"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

const cachedValidatedRequestKey = "relay_helper_cached_validated_request"

type cachedValidatedRequest struct {
	format  types.RelayFormat
	path    string
	model   string
	request dto.Request
}

type ClientRequestValidationError struct {
	StatusCode int
	Message    string
}

func (e *ClientRequestValidationError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func newClientRequestValidationError(status int, format string, args ...any) error {
	return &ClientRequestValidationError{
		StatusCode: status,
		Message:    fmt.Sprintf(format, args...),
	}
}

func AsClientRequestValidationError(err error) (*ClientRequestValidationError, bool) {
	var validationErr *ClientRequestValidationError
	if !errors.As(err, &validationErr) {
		return nil, false
	}
	return validationErr, true
}

func GetAndValidateRequest(c *gin.Context, format types.RelayFormat) (dto.Request, error) {
	if cached, found, err := getCachedValidatedRequest(c); found || err != nil {
		if err != nil {
			return nil, err
		}
		if cached.format != format || cached.path != c.Request.URL.Path {
			return nil, errors.New("cached validated request does not match relay format or path")
		}
		return cached.request, nil
	}

	if isStrictRelayValidationTarget(c.Request.Method, c.Request.URL.Path, format) {
		return parseAndCacheStrictRelayRequest(c, format)
	}

	return getAndValidateRequestUncached(c, format)
}

func GetCachedValidatedModel(c *gin.Context) (string, bool, error) {
	cached, found, err := getCachedValidatedRequest(c)
	if err != nil || !found {
		return "", found, err
	}
	if cached.path != c.Request.URL.Path {
		return "", true, errors.New("cached validated request path does not match current request")
	}
	if !isStrictRelayValidationTarget(c.Request.Method, c.Request.URL.Path, cached.format) {
		return "", true, errors.New("cached validated request format does not match current request")
	}
	requestModel, err := cachedRequestModel(cached)
	if err != nil {
		return "", true, err
	}
	if cached.model == "" || requestModel != cached.model {
		return "", true, errors.New("cached validated request model is invalid")
	}
	return cached.model, true, nil
}

func getCachedValidatedRequest(c *gin.Context) (*cachedValidatedRequest, bool, error) {
	value, found := c.Get(cachedValidatedRequestKey)
	if !found {
		return nil, false, nil
	}
	cached, ok := value.(*cachedValidatedRequest)
	if !ok || cached == nil || cached.request == nil {
		return nil, true, errors.New("cached validated request is invalid")
	}
	requestModel, err := cachedRequestModel(cached)
	if err != nil {
		return nil, true, err
	}
	if cached.model == "" || requestModel != cached.model {
		return nil, true, errors.New("cached validated request model is invalid")
	}
	return cached, true, nil
}

func cachedRequestModel(cached *cachedValidatedRequest) (string, error) {
	if cached == nil {
		return "", errors.New("cached validated request is invalid")
	}
	switch cached.format {
	case types.RelayFormatClaude:
		request, ok := cached.request.(*dto.ClaudeRequest)
		if !ok || request == nil {
			return "", errors.New("cached validated request has an invalid Messages request type")
		}
		return request.Model, nil
	case types.RelayFormatOpenAI:
		request, ok := cached.request.(*dto.GeneralOpenAIRequest)
		if !ok || request == nil {
			return "", errors.New("cached validated request has an invalid Chat Completions request type")
		}
		return request.Model, nil
	case types.RelayFormatOpenAIResponses:
		request, ok := cached.request.(*dto.OpenAIResponsesRequest)
		if !ok || request == nil {
			return "", errors.New("cached validated request has an invalid Responses request type")
		}
		return request.Model, nil
	default:
		return "", errors.New("cached validated request has an unsupported relay format")
	}
}

func isStrictRelayValidationTarget(method, path string, format types.RelayFormat) bool {
	if method != http.MethodPost {
		return false
	}
	switch path {
	case "/v1/messages":
		return format == types.RelayFormatClaude
	case "/v1/chat/completions":
		return format == types.RelayFormatOpenAI
	case "/v1/responses":
		return format == types.RelayFormatOpenAIResponses
	default:
		return false
	}
}

func parseAndCacheStrictRelayRequest(c *gin.Context, format types.RelayFormat) (dto.Request, error) {
	contentType := strings.TrimSpace(c.GetHeader("Content-Type"))
	if contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		mediaType = strings.ToLower(mediaType)
		if err != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
			return nil, newClientRequestValidationError(http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		}
	}

	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, err
	}
	body, err := storage.Bytes()
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, newClientRequestValidationError(http.StatusBadRequest, "request body must be a JSON object")
	}
	if !utf8.Valid(body) {
		return nil, newClientRequestValidationError(http.StatusBadRequest, "request body must contain valid UTF-8")
	}

	var raw map[string]any
	if err := common.Unmarshal(body, &raw); err != nil {
		return nil, newClientRequestValidationError(http.StatusBadRequest, "request body must contain exactly one valid JSON object")
	}
	if raw == nil {
		return nil, newClientRequestValidationError(http.StatusBadRequest, "request body must be a JSON object")
	}
	model, err := requiredStringField(raw, "model", "model")
	if err != nil {
		return nil, err
	}

	var request dto.Request
	switch format {
	case types.RelayFormatClaude:
		if err := validateClaudeRawRequest(raw); err != nil {
			return nil, err
		}
		typed := &dto.ClaudeRequest{}
		if err := common.Unmarshal(body, typed); err != nil {
			return nil, newClientRequestValidationError(http.StatusBadRequest, "request body does not match the Messages schema")
		}
		request, err = validateClaudeRequest(typed)
	case types.RelayFormatOpenAI:
		if err := validateChatRawRequest(raw); err != nil {
			return nil, err
		}
		typed := &dto.GeneralOpenAIRequest{}
		if err := common.Unmarshal(body, typed); err != nil {
			return nil, newClientRequestValidationError(http.StatusBadRequest, "request body does not match the Chat Completions schema")
		}
		request, err = validateTextRequest(c, typed, relayconstant.RelayModeChatCompletions)
	case types.RelayFormatOpenAIResponses:
		if err := validateResponsesRawRequest(raw); err != nil {
			return nil, err
		}
		typed := &dto.OpenAIResponsesRequest{}
		if err := common.Unmarshal(body, typed); err != nil {
			return nil, newClientRequestValidationError(http.StatusBadRequest, "request body does not match the Responses schema")
		}
		request, err = validateResponsesRequest(typed)
	default:
		return nil, fmt.Errorf("unsupported strict relay format: %s", format)
	}
	if err != nil {
		return nil, newClientRequestValidationError(http.StatusBadRequest, "%s", err.Error())
	}
	if request == nil {
		return nil, errors.New("validated request is nil")
	}
	typedModel, err := cachedRequestModel(&cachedValidatedRequest{format: format, request: request})
	if err != nil || typedModel != model {
		return nil, errors.New("validated request model does not match parsed model")
	}

	c.Set(cachedValidatedRequestKey, &cachedValidatedRequest{
		format:  format,
		path:    c.Request.URL.Path,
		model:   model,
		request: request,
	})
	return request, nil
}

func requiredStringField(object map[string]any, field, path string) (string, error) {
	value, found := object[field]
	if !found || value == nil {
		return "", newClientRequestValidationError(http.StatusBadRequest, "%s is required", path)
	}
	stringValue, ok := value.(string)
	if !ok {
		return "", newClientRequestValidationError(http.StatusBadRequest, "%s must be a string", path)
	}
	if strings.TrimSpace(stringValue) == "" {
		return "", newClientRequestValidationError(http.StatusBadRequest, "%s must not be empty", path)
	}
	return stringValue, nil
}

func presentStringField(object map[string]any, field, path string) (string, error) {
	value, found := object[field]
	if !found || value == nil {
		return "", newClientRequestValidationError(http.StatusBadRequest, "%s is required", path)
	}
	stringValue, ok := value.(string)
	if !ok {
		return "", newClientRequestValidationError(http.StatusBadRequest, "%s must be a string", path)
	}
	return stringValue, nil
}

func requiredObjectArrayField(object map[string]any, field, path string) ([]any, error) {
	value, found := object[field]
	if !found || value == nil {
		return nil, newClientRequestValidationError(http.StatusBadRequest, "%s is required", path)
	}
	values, ok := value.([]any)
	if !ok {
		return nil, newClientRequestValidationError(http.StatusBadRequest, "%s must be an array", path)
	}
	if len(values) == 0 {
		return nil, newClientRequestValidationError(http.StatusBadRequest, "%s must not be empty", path)
	}
	return values, nil
}

func objectAt(values []any, index int, path string) (map[string]any, error) {
	object, ok := values[index].(map[string]any)
	if !ok || object == nil {
		return nil, newClientRequestValidationError(http.StatusBadRequest, "%s[%d] must be an object", path, index)
	}
	return object, nil
}

func requirePresent(object map[string]any, field, path string) (any, error) {
	value, found := object[field]
	if !found || value == nil {
		return nil, newClientRequestValidationError(http.StatusBadRequest, "%s is required", path)
	}
	return value, nil
}

func requireObject(object map[string]any, field, path string) (map[string]any, error) {
	value, err := requirePresent(object, field, path)
	if err != nil {
		return nil, err
	}
	result, ok := value.(map[string]any)
	if !ok || result == nil {
		return nil, newClientRequestValidationError(http.StatusBadRequest, "%s must be an object", path)
	}
	return result, nil
}

func requireArray(object map[string]any, field, path string) ([]any, error) {
	value, err := requirePresent(object, field, path)
	if err != nil {
		return nil, err
	}
	result, ok := value.([]any)
	if !ok {
		return nil, newClientRequestValidationError(http.StatusBadRequest, "%s must be an array", path)
	}
	return result, nil
}

func validateArrayOfObjects(value any, path string, validate func(map[string]any, string) error) error {
	items, ok := value.([]any)
	if !ok {
		return newClientRequestValidationError(http.StatusBadRequest, "%s must be an array", path)
	}
	for index, rawItem := range items {
		itemPath := fmt.Sprintf("%s[%d]", path, index)
		item, ok := rawItem.(map[string]any)
		if !ok || item == nil {
			return newClientRequestValidationError(http.StatusBadRequest, "%s must be an object", itemPath)
		}
		if err := validate(item, itemPath); err != nil {
			return err
		}
	}
	return nil
}

func validateNumberArray(value any, path string) error {
	items, ok := value.([]any)
	if !ok {
		return newClientRequestValidationError(http.StatusBadRequest, "%s must be an array", path)
	}
	for index, item := range items {
		if _, ok := item.(float64); !ok {
			return newClientRequestValidationError(http.StatusBadRequest, "%s[%d] must be a number", path, index)
		}
	}
	return nil
}

func validateStringOrObjectArray(value any, path string, validatePart func(map[string]any, string) error) error {
	return validateStringOrObjectArrayWithTypes(value, path, validatePart)
}

func validateStringOrObjectArrayWithTypes(value any, path string, validatePart func(map[string]any, string) error, allowedTypes ...string) error {
	if _, ok := value.(string); ok {
		return nil
	}
	parts, ok := value.([]any)
	if !ok {
		return newClientRequestValidationError(http.StatusBadRequest, "%s must be a string or array", path)
	}
	return validateObjectArrayWithTypes(parts, path, validatePart, allowedTypes...)
}

func validateObjectArrayWithTypes(parts []any, path string, validatePart func(map[string]any, string) error, allowedTypes ...string) error {
	for index, rawPart := range parts {
		partPath := fmt.Sprintf("%s[%d]", path, index)
		part, ok := rawPart.(map[string]any)
		if !ok || part == nil {
			return newClientRequestValidationError(http.StatusBadRequest, "%s must be an object", partPath)
		}
		if len(allowedTypes) > 0 {
			partType, err := requiredStringField(part, "type", partPath+".type")
			if err != nil {
				return err
			}
			allowed := false
			for _, allowedType := range allowedTypes {
				if partType == allowedType {
					allowed = true
					break
				}
			}
			if !allowed {
				return newClientRequestValidationError(http.StatusBadRequest, "%s.type is unsupported in this context", partPath)
			}
		}
		if err := validatePart(part, partPath); err != nil {
			return err
		}
	}
	return nil
}

func validateClaudeContentPart(part map[string]any, path string) error {
	partType, err := requiredStringField(part, "type", path+".type")
	if err != nil {
		return err
	}
	switch partType {
	case "text", "input_text":
		_, err = presentStringField(part, "text", path+".text")
	case "image", "document":
		err = validateClaudeMediaSource(part, path)
	case "tool_use", "server_tool_use":
		if _, err = requiredStringField(part, "id", path+".id"); err == nil {
			_, err = requiredStringField(part, "name", path+".name")
		}
		if err == nil {
			_, err = requireObject(part, "input", path+".input")
		}
	case "tool_result", "web_search_tool_result":
		if _, err = requiredStringField(part, "tool_use_id", path+".tool_use_id"); err != nil {
			return err
		}
		if content, found := part["content"]; found && content != nil {
			err = validateStringOrObjectArrayWithTypes(content, path+".content", validateClaudeContentPart, "text", "input_text", "image", "document")
		}
	case "thinking":
		_, err = requiredStringField(part, "thinking", path+".thinking")
	case "redacted_thinking":
		_, err = requiredStringField(part, "data", path+".data")
	default:
		return newClientRequestValidationError(http.StatusBadRequest, "%s.type is unsupported", path)
	}
	return err
}

func validateClaudeMediaSource(part map[string]any, path string) error {
	sourcePath := path + ".source"
	source, err := requireObject(part, "source", sourcePath)
	if err != nil {
		return err
	}
	sourceType, err := requiredStringField(source, "type", sourcePath+".type")
	if err != nil {
		return err
	}
	switch sourceType {
	case "base64", "text":
		if _, err := requiredStringField(source, "media_type", sourcePath+".media_type"); err != nil {
			return err
		}
		_, err = requiredStringField(source, "data", sourcePath+".data")
		return err
	case "url":
		_, err = requiredStringField(source, "url", sourcePath+".url")
		return err
	default:
		return newClientRequestValidationError(http.StatusBadRequest, "%s.type is unsupported", sourcePath)
	}
}

func validateURLReference(value any, path string, allowFileID bool) error {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return newClientRequestValidationError(http.StatusBadRequest, "%s must not be empty", path)
		}
		return nil
	case map[string]any:
		if typed["url"] != nil {
			_, err := requiredStringField(typed, "url", path+".url")
			return err
		}
		if allowFileID && typed["file_id"] != nil {
			_, err := requiredStringField(typed, "file_id", path+".file_id")
			return err
		}
		if allowFileID {
			return newClientRequestValidationError(http.StatusBadRequest, "%s requires url or file_id", path)
		}
		return newClientRequestValidationError(http.StatusBadRequest, "%s.url is required", path)
	default:
		return newClientRequestValidationError(http.StatusBadRequest, "%s must be a string or object", path)
	}
}

func validateChatFile(file map[string]any, path string) error {
	if file["file_id"] != nil {
		_, err := requiredStringField(file, "file_id", path+".file_id")
		return err
	}
	if file["file_data"] != nil {
		if _, err := requiredStringField(file, "filename", path+".filename"); err != nil {
			return err
		}
		_, err := requiredStringField(file, "file_data", path+".file_data")
		return err
	}
	return newClientRequestValidationError(http.StatusBadRequest, "%s requires file_id or filename and file_data", path)
}

func validateChatContentPart(part map[string]any, path string) error {
	if _, found := part["prompt_cache_breakpoint"]; found {
		return newClientRequestValidationError(http.StatusBadRequest, "%s.prompt_cache_breakpoint is not supported by this gateway", path)
	}
	partType, err := requiredStringField(part, "type", path+".type")
	if err != nil {
		return err
	}
	switch partType {
	case "text":
		_, err = requiredStringField(part, "text", path+".text")
	case "image_url":
		imageURL, fieldErr := requirePresent(part, "image_url", path+".image_url")
		if fieldErr != nil {
			return fieldErr
		}
		err = validateURLReference(imageURL, path+".image_url", false)
	case "input_audio":
		inputAudio, fieldErr := requireObject(part, "input_audio", path+".input_audio")
		if fieldErr != nil {
			return fieldErr
		}
		if _, fieldErr = requiredStringField(inputAudio, "data", path+".input_audio.data"); fieldErr != nil {
			return fieldErr
		}
		_, err = requiredStringField(inputAudio, "format", path+".input_audio.format")
	case "file":
		file, fieldErr := requireObject(part, "file", path+".file")
		if fieldErr != nil {
			return fieldErr
		}
		err = validateChatFile(file, path+".file")
	case "video_url":
		_, err = requiredStringField(part, "video_url", path+".video_url")
	default:
		return newClientRequestValidationError(http.StatusBadRequest, "%s.type is unsupported", path)
	}
	return err
}

func validateResponsesImagePart(part map[string]any, path string) error {
	if imageURL := part["image_url"]; imageURL != nil {
		return validateURLReference(imageURL, path+".image_url", true)
	}
	if part["file_id"] != nil {
		_, err := requiredStringField(part, "file_id", path+".file_id")
		return err
	}
	if part["url"] != nil {
		_, err := requiredStringField(part, "url", path+".url")
		return err
	}
	return newClientRequestValidationError(http.StatusBadRequest, "%s requires image_url, file_id, or url", path)
}

func validateResponsesFilePart(part map[string]any, path string) error {
	reference := part
	referencePath := path
	if part["file"] != nil {
		file, err := requireObject(part, "file", path+".file")
		if err != nil {
			return err
		}
		reference = file
		referencePath = path + ".file"
	}
	if reference["file_id"] != nil {
		_, err := requiredStringField(reference, "file_id", referencePath+".file_id")
		return err
	}
	if reference["file_url"] != nil {
		_, err := requiredStringField(reference, "file_url", referencePath+".file_url")
		return err
	}
	if reference["file_data"] != nil {
		if _, err := requiredStringField(reference, "filename", referencePath+".filename"); err != nil {
			return err
		}
		_, err := requiredStringField(reference, "file_data", referencePath+".file_data")
		return err
	}
	return newClientRequestValidationError(http.StatusBadRequest, "%s requires file_id, file_url, or filename and file_data", referencePath)
}

func validateResponsesAudioPart(part map[string]any, path string) error {
	reference := part
	referencePath := path
	if part["input_audio"] != nil {
		inputAudio, err := requireObject(part, "input_audio", path+".input_audio")
		if err != nil {
			return err
		}
		reference = inputAudio
		referencePath = path + ".input_audio"
	}
	if _, err := requiredStringField(reference, "data", referencePath+".data"); err != nil {
		return err
	}
	_, err := requiredStringField(reference, "format", referencePath+".format")
	return err
}

func validateResponsesVideoPart(part map[string]any, path string) error {
	if videoURL := part["video_url"]; videoURL != nil {
		return validateURLReference(videoURL, path+".video_url", false)
	}
	if part["url"] != nil {
		_, err := requiredStringField(part, "url", path+".url")
		return err
	}
	return newClientRequestValidationError(http.StatusBadRequest, "%s requires video_url or url", path)
}

func validateResponsesContentPart(part map[string]any, path string) error {
	partType, err := requiredStringField(part, "type", path+".type")
	if err != nil {
		return err
	}
	switch partType {
	case "input_text", "text":
		_, err = presentStringField(part, "text", path+".text")
	case "output_text":
		if _, err = presentStringField(part, "text", path+".text"); err != nil {
			return err
		}
		err = validateResponsesOutputTextMetadata(part, path)
	case "refusal":
		_, err = presentStringField(part, "refusal", path+".refusal")
	case "summary_text":
		_, err = presentStringField(part, "text", path+".text")
	case "reasoning_text":
		_, err = presentStringField(part, "text", path+".text")
	case "input_image":
		err = validateResponsesImagePart(part, path)
	case "input_file":
		err = validateResponsesFilePart(part, path)
	case "input_audio":
		err = validateResponsesAudioPart(part, path)
	case "input_video":
		err = validateResponsesVideoPart(part, path)
	default:
		return newClientRequestValidationError(http.StatusBadRequest, "%s.type is unsupported", path)
	}
	return err
}

func validateResponsesOutputTextMetadata(part map[string]any, path string) error {
	annotations, err := requireArray(part, "annotations", path+".annotations")
	if err != nil {
		return err
	}
	if err := validateArrayOfObjects(annotations, path+".annotations", validateResponsesAnnotation); err != nil {
		return err
	}
	logprobs, err := requireArray(part, "logprobs", path+".logprobs")
	if err != nil {
		return err
	}
	return validateArrayOfObjects(logprobs, path+".logprobs", validateResponsesLogprob)
}

func validateResponsesAnnotation(annotation map[string]any, path string) error {
	annotationType, err := requiredStringField(annotation, "type", path+".type")
	if err != nil {
		return err
	}
	switch annotationType {
	case "file_citation":
		if _, err := requiredStringField(annotation, "file_id", path+".file_id"); err != nil {
			return err
		}
		if _, err := requiredStringField(annotation, "filename", path+".filename"); err != nil {
			return err
		}
		return validateRequiredNumberField(annotation, "index", path+".index")
	case "url_citation":
		for _, field := range []string{"url", "title"} {
			if _, err := requiredStringField(annotation, field, path+"."+field); err != nil {
				return err
			}
		}
		for _, field := range []string{"start_index", "end_index"} {
			if err := validateRequiredNumberField(annotation, field, path+"."+field); err != nil {
				return err
			}
		}
		return nil
	case "container_file_citation":
		for _, field := range []string{"container_id", "file_id", "filename"} {
			if _, err := requiredStringField(annotation, field, path+"."+field); err != nil {
				return err
			}
		}
		for _, field := range []string{"start_index", "end_index"} {
			if err := validateRequiredNumberField(annotation, field, path+"."+field); err != nil {
				return err
			}
		}
		return nil
	case "file_path":
		if _, err := requiredStringField(annotation, "file_id", path+".file_id"); err != nil {
			return err
		}
		return validateRequiredNumberField(annotation, "index", path+".index")
	default:
		return newClientRequestValidationError(http.StatusBadRequest, "%s.type is unsupported", path)
	}
}

func validateRequiredNumberField(object map[string]any, field, path string) error {
	value, found := object[field]
	if !found || value == nil {
		return newClientRequestValidationError(http.StatusBadRequest, "%s is required", path)
	}
	if _, ok := value.(float64); !ok {
		return newClientRequestValidationError(http.StatusBadRequest, "%s must be a number", path)
	}
	return nil
}

func validateResponsesLogprob(logprob map[string]any, path string) error {
	if _, err := presentStringField(logprob, "token", path+".token"); err != nil {
		return err
	}
	if err := validateNumberArray(logprob["bytes"], path+".bytes"); err != nil {
		return err
	}
	if err := validateRequiredNumberField(logprob, "logprob", path+".logprob"); err != nil {
		return err
	}
	topLogprobs, err := requireArray(logprob, "top_logprobs", path+".top_logprobs")
	if err != nil {
		return err
	}
	return validateArrayOfObjects(topLogprobs, path+".top_logprobs", func(item map[string]any, itemPath string) error {
		if _, err := presentStringField(item, "token", itemPath+".token"); err != nil {
			return err
		}
		if err := validateNumberArray(item["bytes"], itemPath+".bytes"); err != nil {
			return err
		}
		return validateRequiredNumberField(item, "logprob", itemPath+".logprob")
	})
}

func validateClaudeRawRequest(raw map[string]any) error {
	if system, found := raw["system"]; found && system != nil {
		if err := validateStringOrObjectArrayWithTypes(system, "system", validateClaudeContentPart, "text", "input_text"); err != nil {
			return err
		}
	}
	messages, err := requiredObjectArrayField(raw, "messages", "messages")
	if err != nil {
		return err
	}
	for index := range messages {
		message, err := objectAt(messages, index, "messages")
		if err != nil {
			return err
		}
		role, err := requiredStringField(message, "role", fmt.Sprintf("messages[%d].role", index))
		if err != nil {
			return err
		}
		if role != "user" && role != "assistant" {
			return newClientRequestValidationError(http.StatusBadRequest, "messages[%d].role must be user or assistant", index)
		}
		contentPath := fmt.Sprintf("messages[%d].content", index)
		content, err := requirePresent(message, "content", contentPath)
		if err != nil {
			return err
		}
		if err := validateStringOrObjectArray(content, contentPath, validateClaudeContentPart); err != nil {
			return err
		}
	}
	return nil
}

func validateChatToolCalls(message map[string]any, index int) error {
	path := fmt.Sprintf("messages[%d].tool_calls", index)
	toolCalls, err := requireArray(message, "tool_calls", path)
	if err != nil {
		return err
	}
	if len(toolCalls) == 0 {
		return newClientRequestValidationError(http.StatusBadRequest, "%s must not be empty", path)
	}
	for toolIndex, rawTool := range toolCalls {
		toolPath := fmt.Sprintf("%s[%d]", path, toolIndex)
		tool, ok := rawTool.(map[string]any)
		if !ok || tool == nil {
			return newClientRequestValidationError(http.StatusBadRequest, "%s must be an object", toolPath)
		}
		if _, err := requiredStringField(tool, "id", toolPath+".id"); err != nil {
			return err
		}
		toolType, err := requiredStringField(tool, "type", toolPath+".type")
		if err != nil {
			return err
		}
		if toolType != "function" {
			return newClientRequestValidationError(http.StatusBadRequest, "%s.type is unsupported", toolPath)
		}
		function, err := requireObject(tool, "function", toolPath+".function")
		if err != nil {
			return err
		}
		if _, err := requiredStringField(function, "name", toolPath+".function.name"); err != nil {
			return err
		}
		if _, err := presentStringField(function, "arguments", toolPath+".function.arguments"); err != nil {
			return err
		}
	}
	return nil
}

func validateChatRawRequest(raw map[string]any) error {
	value, found := raw["messages"]
	if !found || value == nil {
		if raw["prefix"] != nil || raw["suffix"] != nil {
			return nil
		}
		return newClientRequestValidationError(http.StatusBadRequest, "messages is required")
	}
	messages, ok := value.([]any)
	if !ok {
		return newClientRequestValidationError(http.StatusBadRequest, "messages must be an array")
	}
	if len(messages) == 0 && raw["prefix"] == nil && raw["suffix"] == nil {
		return newClientRequestValidationError(http.StatusBadRequest, "messages must not be empty")
	}
	for index := range messages {
		message, err := objectAt(messages, index, "messages")
		if err != nil {
			return err
		}
		role, err := requiredStringField(message, "role", fmt.Sprintf("messages[%d].role", index))
		if err != nil {
			return err
		}
		switch role {
		case "system", "developer":
			contentPath := fmt.Sprintf("messages[%d].content", index)
			content, err := requirePresent(message, "content", contentPath)
			if err != nil {
				return err
			}
			if err := validateStringOrObjectArrayWithTypes(content, contentPath, validateChatContentPart, "text"); err != nil {
				return err
			}
		case "user":
			contentPath := fmt.Sprintf("messages[%d].content", index)
			content, err := requirePresent(message, "content", contentPath)
			if err != nil {
				return err
			}
			if err := validateStringOrObjectArrayWithTypes(content, contentPath, validateChatContentPart, "text", "image_url", "input_audio", "file", "video_url"); err != nil {
				return err
			}
		case "assistant":
			if _, found := message["name"]; found {
				return newClientRequestValidationError(http.StatusBadRequest, "messages[%d].name is not supported for assistant messages", index)
			}
			for _, unsupportedField := range []string{"refusal", "function_call", "audio"} {
				if value, found := message[unsupportedField]; found && value != nil {
					return newClientRequestValidationError(http.StatusBadRequest, "messages[%d].%s is not supported by this gateway", index, unsupportedField)
				}
			}
			if message["content"] == nil && message["tool_calls"] == nil && message["reasoning_content"] == nil && message["reasoning"] == nil {
				return newClientRequestValidationError(http.StatusBadRequest, "messages[%d] requires content, tool_calls, or reasoning", index)
			}
			if content := message["content"]; content != nil {
				switch typed := content.(type) {
				case string:
					if typed == "" && message["tool_calls"] == nil && message["reasoning_content"] == nil && message["reasoning"] == nil {
						return newClientRequestValidationError(http.StatusBadRequest, "messages[%d].content must not be empty", index)
					}
				case []any:
					if len(typed) == 0 {
						return newClientRequestValidationError(http.StatusBadRequest, "messages[%d].content must not be empty", index)
					}
				}
				if err := validateStringOrObjectArrayWithTypes(content, fmt.Sprintf("messages[%d].content", index), validateChatContentPart, "text"); err != nil {
					return err
				}
			}
			if message["tool_calls"] != nil {
				if err := validateChatToolCalls(message, index); err != nil {
					return err
				}
			}
		case "tool":
			if _, err := requiredStringField(message, "tool_call_id", fmt.Sprintf("messages[%d].tool_call_id", index)); err != nil {
				return err
			}
			contentPath := fmt.Sprintf("messages[%d].content", index)
			content, err := requirePresent(message, "content", contentPath)
			if err != nil {
				return err
			}
			if err := validateStringOrObjectArrayWithTypes(content, contentPath, validateChatContentPart, "text"); err != nil {
				return err
			}
		case "function":
			if _, err := requiredStringField(message, "name", fmt.Sprintf("messages[%d].name", index)); err != nil {
				return err
			}
			contentPath := fmt.Sprintf("messages[%d].content", index)
			content, err := requirePresent(message, "content", contentPath)
			if err != nil {
				return err
			}
			if err := validateStringOrObjectArrayWithTypes(content, contentPath, validateChatContentPart, "text"); err != nil {
				return err
			}
		default:
			return newClientRequestValidationError(http.StatusBadRequest, "messages[%d].role is unsupported", index)
		}
	}
	return nil
}

func validateResponsesRawRequest(raw map[string]any) error {
	input, found := raw["input"]
	if !found || input == nil {
		return newClientRequestValidationError(http.StatusBadRequest, "input is required")
	}
	if _, ok := input.(string); ok {
		return nil
	}
	items, ok := input.([]any)
	if !ok {
		return newClientRequestValidationError(http.StatusBadRequest, "input must be a string or array")
	}
	if len(items) == 0 && !responsesStatefulAnchorPresent(raw) {
		return newClientRequestValidationError(http.StatusBadRequest, "input must not be empty without a stateful response anchor")
	}
	for index := range items {
		item, err := objectAt(items, index, "input")
		if err != nil {
			return err
		}
		itemType := ""
		if rawType, found := item["type"]; found && rawType != nil {
			typed, ok := rawType.(string)
			if !ok {
				return newClientRequestValidationError(http.StatusBadRequest, "input[%d].type must be a string", index)
			}
			itemType = strings.TrimSpace(typed)
		}
		switch itemType {
		case "", "message":
			role, err := requiredStringField(item, "role", fmt.Sprintf("input[%d].role", index))
			if err != nil {
				return err
			}
			if role != "user" && role != "assistant" && role != "system" && role != "developer" {
				return newClientRequestValidationError(http.StatusBadRequest, "input[%d].role is unsupported", index)
			}
			contentPath := fmt.Sprintf("input[%d].content", index)
			content, err := requirePresent(item, "content", contentPath)
			if err != nil {
				return err
			}
			if role == "assistant" && responsesAssistantMessageIsOutputReplay(item, content) {
				if err := validateResponsesOutputMessage(item, itemType, index); err != nil {
					return err
				}
				continue
			}
			allowedTypes := []string{"input_text", "text", "input_image", "input_file", "input_audio", "input_video"}
			if err := validateStringOrObjectArrayWithTypes(content, contentPath, validateResponsesContentPart, allowedTypes...); err != nil {
				return err
			}
		case "reasoning":
			if _, err := requiredStringField(item, "id", fmt.Sprintf("input[%d].id", index)); err != nil {
				return err
			}
			summaryPath := fmt.Sprintf("input[%d].summary", index)
			summary, err := requireArray(item, "summary", summaryPath)
			if err != nil {
				return err
			}
			if err := validateObjectArrayWithTypes(summary, summaryPath, validateResponsesContentPart, "summary_text"); err != nil {
				return err
			}
			if _, found := item["content"]; found {
				contentPath := fmt.Sprintf("input[%d].content", index)
				content, err := requireArray(item, "content", contentPath)
				if err != nil {
					return err
				}
				if err := validateObjectArrayWithTypes(content, contentPath, validateResponsesContentPart, "reasoning_text"); err != nil {
					return err
				}
			}
			if encryptedContent, found := item["encrypted_content"]; found && encryptedContent != nil {
				if _, ok := encryptedContent.(string); !ok {
					return newClientRequestValidationError(http.StatusBadRequest, "input[%d].encrypted_content must be a string or null", index)
				}
			}
			if rawStatus, found := item["status"]; found {
				status, ok := rawStatus.(string)
				if !ok {
					return newClientRequestValidationError(http.StatusBadRequest, "input[%d].status must be a string", index)
				}
				if status != "in_progress" && status != "completed" && status != "incomplete" {
					return newClientRequestValidationError(http.StatusBadRequest, "input[%d].status is unsupported", index)
				}
			}
		case "function_call", "custom_tool_call":
			if _, err := requiredStringField(item, "name", fmt.Sprintf("input[%d].name", index)); err != nil {
				return err
			}
			if _, err := requiredStringField(item, "call_id", fmt.Sprintf("input[%d].call_id", index)); err != nil {
				if _, idErr := requiredStringField(item, "id", fmt.Sprintf("input[%d].id", index)); idErr != nil {
					return err
				}
			}
			inputField := "arguments"
			if itemType == "custom_tool_call" {
				inputField = "input"
			}
			if itemType == "function_call" {
				if _, err := presentStringField(item, inputField, fmt.Sprintf("input[%d].%s", index, inputField)); err != nil {
					return err
				}
			} else if _, found := item[inputField]; !found {
				return newClientRequestValidationError(http.StatusBadRequest, "input[%d].%s is required", index, inputField)
			}
		case "function_call_output", "custom_tool_call_output":
			if _, err := requiredStringField(item, "call_id", fmt.Sprintf("input[%d].call_id", index)); err != nil {
				return err
			}
			if _, err := requirePresent(item, "output", fmt.Sprintf("input[%d].output", index)); err != nil {
				return err
			}
		case "additional_tools":
			toolsPath := fmt.Sprintf("input[%d].tools", index)
			tools, err := requireArray(item, "tools", toolsPath)
			if err != nil {
				return err
			}
			for toolIndex, tool := range tools {
				toolPath := fmt.Sprintf("%s[%d]", toolsPath, toolIndex)
				object, ok := tool.(map[string]any)
				if !ok || object == nil {
					return newClientRequestValidationError(http.StatusBadRequest, "%s[%d] must be an object", toolsPath, toolIndex)
				}
				if err := validateAdditionalResponsesTool(object, toolPath, true); err != nil {
					return err
				}
			}
		default:
			return newClientRequestValidationError(http.StatusBadRequest, "input[%d].type is unsupported", index)
		}
	}
	return nil
}

func responsesAssistantMessageIsOutputReplay(item map[string]any, content any) bool {
	if _, found := item["id"]; found {
		return true
	}
	if _, found := item["status"]; found {
		return true
	}
	parts, ok := content.([]any)
	if !ok {
		return false
	}
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok || part == nil {
			continue
		}
		partType, _ := part["type"].(string)
		if partType == "output_text" || partType == "refusal" {
			return true
		}
	}
	return false
}

func validateResponsesOutputMessage(item map[string]any, itemType string, index int) error {
	path := fmt.Sprintf("input[%d]", index)
	if rawType, found := item["type"]; !found || rawType == nil {
		return newClientRequestValidationError(http.StatusBadRequest, "%s.type is required for an output message", path)
	}
	if itemType != "message" {
		return newClientRequestValidationError(http.StatusBadRequest, "%s.type must be message for an output message", path)
	}
	if _, err := requiredStringField(item, "id", path+".id"); err != nil {
		return err
	}
	status, err := requiredStringField(item, "status", path+".status")
	if err != nil {
		return err
	}
	if status != "in_progress" && status != "completed" && status != "incomplete" {
		return newClientRequestValidationError(http.StatusBadRequest, "%s.status is unsupported", path)
	}
	contentPath := path + ".content"
	content, err := requireArray(item, "content", contentPath)
	if err != nil {
		return err
	}
	return validateObjectArrayWithTypes(content, contentPath, validateResponsesContentPart, "output_text", "refusal")
}

func validateAdditionalResponsesTool(tool map[string]any, path string, allowCustom bool) error {
	toolType, err := requiredStringField(tool, "type", path+".type")
	if err != nil {
		return err
	}
	toolType = strings.ToLower(strings.TrimSpace(toolType))
	switch toolType {
	case "function":
		_, err = requiredStringField(tool, "name", path+".name")
		return err
	case "custom":
		if !allowCustom {
			return newClientRequestValidationError(http.StatusBadRequest, "%s.type is unsupported inside a namespace", path)
		}
		_, err = requiredStringField(tool, "name", path+".name")
		return err
	case "namespace":
		if _, err = requiredStringField(tool, "name", path+".name"); err != nil {
			return err
		}
		childrenField := "tools"
		if _, found := tool[childrenField]; !found {
			childrenField = "children"
		}
		childrenPath := path + "." + childrenField
		children, err := requireArray(tool, childrenField, childrenPath)
		if err != nil {
			return err
		}
		if len(children) == 0 {
			return newClientRequestValidationError(http.StatusBadRequest, "%s must not be empty", childrenPath)
		}
		for index, rawChild := range children {
			childPath := fmt.Sprintf("%s[%d]", childrenPath, index)
			child, ok := rawChild.(map[string]any)
			if !ok || child == nil {
				return newClientRequestValidationError(http.StatusBadRequest, "%s must be an object", childPath)
			}
			if err := validateAdditionalResponsesTool(child, childPath, false); err != nil {
				return err
			}
		}
		return nil
	default:
		return newClientRequestValidationError(http.StatusBadRequest, "%s.type is unsupported", path)
	}
}

func responsesStatefulAnchorPresent(raw map[string]any) bool {
	if previousResponseID, ok := raw["previous_response_id"].(string); ok && strings.TrimSpace(previousResponseID) != "" {
		return true
	}
	for _, field := range []string{"conversation", "context_management"} {
		if value, found := raw[field]; found && value != nil {
			return true
		}
	}
	return false
}
