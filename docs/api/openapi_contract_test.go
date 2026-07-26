package api_test

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const nestedErrorSchemaRef = "#/components/schemas/OpenAIError"

func TestSeedDanceStandaloneOpenAPIContract(t *testing.T) {
	doc := loadYAMLDocument(t, "seed-dance-openapi.yaml")

	require.Equal(t, "3.0.3", stringAt(t, doc, "openapi"))
	servers := sliceAt(t, doc, "servers")
	require.NotEmpty(t, servers)
	server := objectValue(t, servers[0])
	assert.Equal(t, "{{base_url}}", stringAt(t, server, "url"))
	assert.NotContains(t, server, "variables")
	bearerAuth := objectAt(t, doc, "components", "securitySchemes", "BearerAuth")
	assert.Equal(t, "http", stringAt(t, bearerAuth, "type"))
	assert.Equal(t, "bearer", stringAt(t, bearerAuth, "scheme"))
	assert.Equal(t, "API key", stringAt(t, bearerAuth, "bearerFormat"))
	assertVideoOperations(t, doc)
	assertSeedDanceResponseExamples(t, doc)
	assertSeedDanceMultipartMetadataExample(t, doc)
	assertSeedDanceErrorExamples(t, doc)
	assertSeedDanceSubmitErrorExamples(t, doc)
	assertLocalRefsResolve(t, doc)
	assertExamplesMatchBasicSchema(t, doc)
}

func assertSeedDanceResponseExamples(t *testing.T, document map[string]any) {
	t.Helper()

	createExample := objectAt(
		t,
		document,
		"paths",
		"/v1/videos",
		"post",
		"responses",
		"200",
		"content",
		"application/json",
		"example",
	)
	assert.Equal(t, "queued", stringAt(t, createExample, "status"))
	assert.Equal(t, float64(10), numberAt(t, createExample, "progress"))
	assert.NotContains(t, createExample, "completed_at")

	statusExample := objectAt(
		t,
		document,
		"paths",
		"/v1/videos/{task_id}",
		"get",
		"responses",
		"200",
		"content",
		"application/json",
		"example",
	)
	assert.Equal(t, "in_progress", stringAt(t, statusExample, "status"))
	assert.Equal(t, float64(30), numberAt(t, statusExample, "progress"))
	assert.NotContains(t, statusExample, "completed_at")
}

func assertSeedDanceMultipartMetadataExample(t *testing.T, document map[string]any) {
	t.Helper()

	multipart := objectAt(
		t,
		document,
		"paths",
		"/v1/videos",
		"post",
		"requestBody",
		"content",
		"multipart/form-data",
	)
	metadataText := stringAt(t, objectAt(t, multipart, "example"), "metadata")
	assertMultipartMetadataTextBooleans(t, metadataText)
	assertMultipartMetadataTextBooleans(
		t,
		stringAt(
			t,
			document,
			"components",
			"schemas",
			"VideoCreateMultipart",
			"properties",
			"metadata",
			"example",
		),
	)
}

func assertMultipartMetadataTextBooleans(t *testing.T, metadataText string) {
	t.Helper()

	var metadata map[string]any
	require.NoError(t, common.Unmarshal([]byte(metadataText), &metadata))
	for _, field := range []string{
		"prompt_optimization",
		"multi_shot",
		"strict_duration",
	} {
		value, ok := metadata[field].(string)
		require.True(t, ok, "multipart metadata %s must be a JSON string", field)
		assert.Contains(t, []string{"true", "false"}, value)
	}
}

func assertSeedDanceErrorExamples(t *testing.T, document map[string]any) {
	t.Helper()

	unauthorized := resolvedOperationResponse(
		t,
		document,
		"/v1/videos",
		"post",
		"401",
	)
	unauthorizedExamples := objectAt(
		t,
		unauthorized,
		"content",
		"application/json",
		"examples",
	)
	unauthorizedError := objectAt(
		t,
		objectAt(t, unauthorizedExamples, "clientAuthentication"),
		"value",
		"error",
	)
	assert.Equal(t, "new_api_error", stringAt(t, unauthorizedError, "type"))
	assert.Equal(t, "", stringAt(t, unauthorizedError, "code"))

	upstreamAuthenticationError := objectAt(
		t,
		objectAt(t, unauthorizedExamples, "upstreamAuthentication"),
		"value",
		"error",
	)
	assert.Equal(
		t,
		"invalid_request_error",
		stringAt(t, upstreamAuthenticationError, "type"),
	)
	assert.Equal(
		t,
		"upstream_authentication_error",
		stringAt(t, upstreamAuthenticationError, "code"),
	)

	rateLimited := resolvedOperationResponse(t, document, "/v1/videos", "post", "429")
	rateError := objectAt(
		t,
		rateLimited,
		"content",
		"application/json",
		"example",
		"error",
	)
	assert.Equal(t, "rate_limit_error", stringAt(t, rateError, "type"))
	assert.Equal(t, "upstream_rate_limit_error", stringAt(t, rateError, "code"))

	contentRateLimited := resolvedOperationResponse(
		t,
		document,
		"/v1/videos/{task_id}/content",
		"get",
		"429",
	)
	contentRateError := objectAt(
		t,
		contentRateLimited,
		"content",
		"application/json",
		"example",
		"error",
	)
	assert.Equal(
		t,
		"upstream_rate_limit_error",
		stringAt(t, contentRateError, "type"),
	)
	assert.Equal(
		t,
		"upstream_rate_limit_error",
		stringAt(t, contentRateError, "code"),
	)
}

func assertSeedDanceSubmitErrorExamples(t *testing.T, document map[string]any) {
	t.Helper()

	create := objectAt(t, document, "paths", "/v1/videos", "post")
	assert.NotContains(t, objectAt(t, create, "responses"), "504")

	badRequest := resolvedOperationResponse(t, document, "/v1/videos", "post", "400")
	badRequestExamples := objectAt(
		t,
		badRequest,
		"content",
		"application/json",
		"examples",
	)
	malformedError := objectAt(
		t,
		objectAt(t, badRequestExamples, "malformedRequest"),
		"value",
		"error",
	)
	assert.Equal(t, "new_api_error", stringAt(t, malformedError, "type"))
	assert.Equal(t, "", stringAt(t, malformedError, "code"))
	parameterError := objectAt(
		t,
		objectAt(t, badRequestExamples, "invalidParameter"),
		"value",
		"error",
	)
	assert.Equal(t, "invalid_request_error", stringAt(t, parameterError, "type"))
	assert.Equal(t, "invalid_duration", stringAt(t, parameterError, "code"))

	forbidden := resolvedOperationResponse(t, document, "/v1/videos", "post", "403")
	forbiddenError := objectAt(
		t,
		forbidden,
		"content",
		"application/json",
		"example",
		"error",
	)
	assert.Equal(t, "invalid_request_error", stringAt(t, forbiddenError, "type"))
	assert.Equal(t, "upstream_authentication_error", stringAt(t, forbiddenError, "code"))

	unavailable := resolvedOperationResponse(t, document, "/v1/videos", "post", "503")
	unavailableError := objectAt(
		t,
		unavailable,
		"content",
		"application/json",
		"example",
		"error",
	)
	assert.Equal(t, "new_api_error", stringAt(t, unavailableError, "type"))
	assert.Equal(t, "model_not_found", stringAt(t, unavailableError, "code"))

	upstream := resolvedOperationResponse(t, document, "/v1/videos", "post", "502")
	upstreamExamples := objectAt(
		t,
		upstream,
		"content",
		"application/json",
		"examples",
	)
	upstreamError := objectAt(
		t,
		objectAt(t, upstreamExamples, "outcomeUnknown"),
		"value",
		"error",
	)
	assert.Equal(t, "server_error", stringAt(t, upstreamError, "type"))
	assert.Equal(t, "seedance_submit_outcome_unknown", stringAt(t, upstreamError, "code"))

	businessError := objectAt(
		t,
		objectAt(t, upstreamExamples, "businessFailure"),
		"value",
		"error",
	)
	assert.Equal(t, "server_error", stringAt(t, businessError, "type"))
	assert.Equal(t, "upstream_error", stringAt(t, businessError, "code"))
}

func resolvedOperationResponse(
	t *testing.T,
	document map[string]any,
	path string,
	method string,
	status string,
) map[string]any {
	t.Helper()

	response := responseAt(
		t,
		objectAt(t, document, "paths", path, method),
		status,
	)
	if ref, ok := response["$ref"].(string); ok {
		return resolveRef(t, document, ref)
	}
	return response
}

func TestSeedDanceFixturesAreSanitizedContracts(t *testing.T) {
	fixtureDir := filepath.Join("fixtures", "seed-dance")
	expectedStatuses := []struct {
		name   string
		status string
	}{
		{name: "status-accepted.json", status: "accepted"},
		{name: "status-processing.json", status: "processing"},
		{name: "status-completed.json", status: "completed"},
	}

	for _, expected := range expectedStatuses {
		t.Run(expected.name, func(t *testing.T) {
			document := loadJSONDocument(t, filepath.Join(fixtureDir, expected.name))
			assert.Equal(t, "REQUEST_ID", stringAt(t, document, "requestId"))
			assert.Equal(t, "UPSTREAM_TASK_ID", stringAt(t, document, "task_id"))
			assert.Equal(t, expected.status, stringAt(t, document, "status"))
		})
	}

	t.Run("generate-response.json", func(t *testing.T) {
		document := loadJSONDocument(t, filepath.Join(fixtureDir, "generate-response.json"))
		assert.Equal(t, "REQUEST_ID", stringAt(t, document, "requestId"))
		assert.Equal(t, "UPSTREAM_TASK_ID", stringAt(t, document, "task_id"))
		assert.Equal(t, "accepted", stringAt(t, document, "status"))
	})

	t.Run("status-business-error.json", func(t *testing.T) {
		document := loadJSONDocument(t, filepath.Join(fixtureDir, "status-business-error.json"))
		assert.Equal(t, false, valueAt(t, document, "success"))
		assert.Equal(t, "400", stringAt(t, document, "errCode"))
		assert.Equal(t, `{"detail":"Task not found"}`, stringAt(t, document, "errMessage"))
		assert.Nil(t, valueAt(t, document, "data"))
	})

	t.Run("ffprobe-output.json", func(t *testing.T) {
		document := loadJSONDocument(t, filepath.Join(fixtureDir, "ffprobe-output.json"))
		formatName := stringAt(t, document, "format", "format_name")
		assert.NotEmpty(t, formatName)
		assert.NotContains(t, formatName, "/")

		streams := sliceAt(t, document, "streams")
		require.Len(t, streams, 2)
		assert.Equal(t, "h264", stringAt(t, objectValue(t, streams[0]), "codec_name"))
		assert.Equal(t, "video", stringAt(t, objectValue(t, streams[0]), "codec_type"))
		assert.Equal(t, "aac", stringAt(t, objectValue(t, streams[1]), "codec_name"))
		assert.Equal(t, "audio", stringAt(t, objectValue(t, streams[1]), "codec_type"))
	})

	t.Run("video-response-minimal.json", func(t *testing.T) {
		document := loadJSONDocument(t, filepath.Join(fixtureDir, "video-response-minimal.json"))
		assert.ElementsMatch(t, []string{"requestId", "video_base64"}, mapKeys(document))
		assert.Equal(t, "REQUEST_ID", stringAt(t, document, "requestId"))

		decoded, decodeErr := base64.StdEncoding.Strict().DecodeString(
			stringAt(t, document, "video_base64"),
		)
		require.NoError(t, decodeErr)
		require.GreaterOrEqual(t, len(decoded), 8)
		assert.Equal(t, "ftyp", string(decoded[4:8]))
	})

	matches, err := filepath.Glob(filepath.Join(fixtureDir, "*.json"))
	require.NoError(t, err)
	require.Len(t, matches, 7)

	for _, name := range matches {
		data, readErr := os.ReadFile(name)
		require.NoError(t, readErr)
		text := string(data)
		assert.NotContains(t, text, "Authorization")
		assert.NotContains(t, text, "image_base64")
		if filepath.Base(name) != "video-response-minimal.json" {
			assert.NotContains(t, text, "video_base64")
		}
		assert.NotRegexp(t, regexp.MustCompile(`(?i)\b(?:sk|key|token)-[a-z0-9_-]+\b`), text)
	}
}

func loadYAMLDocument(t *testing.T, name string) map[string]any {
	t.Helper()

	data, err := os.ReadFile(name)
	require.NoError(t, err)

	var document map[string]any
	require.NoError(t, yaml.Unmarshal(data, &document))
	return document
}

func loadJSONDocument(t *testing.T, name string) map[string]any {
	t.Helper()

	data, err := os.ReadFile(name)
	require.NoError(t, err)

	var document map[string]any
	require.NoError(t, common.Unmarshal(data, &document))
	return document
}

func assertVideoOperations(t *testing.T, document map[string]any) {
	t.Helper()

	operations := []struct {
		path          string
		method        string
		errorStatuses []string
	}{
		{
			path:          "/v1/videos",
			method:        "post",
			errorStatuses: []string{"400", "401", "403", "429", "502", "503"},
		},
		{
			path:          "/v1/videos/{task_id}",
			method:        "get",
			errorStatuses: []string{"400", "401", "404", "500"},
		},
		{
			path:          "/v1/videos/{task_id}/content",
			method:        "get",
			errorStatuses: []string{"400", "401", "404", "429", "502", "504"},
		},
	}

	for _, expected := range operations {
		operation := objectAt(t, document, "paths", expected.path, expected.method)
		assertBearerSecurity(t, operation, expected.method+" "+expected.path)
		assert.ElementsMatch(
			t,
			append([]string{"200"}, expected.errorStatuses...),
			mapKeys(objectAt(t, operation, "responses")),
			"%s %s response status contract",
			expected.method,
			expected.path,
		)
		assertNestedErrorResponses(
			t,
			document,
			operation,
			expected.method+" "+expected.path,
			expected.errorStatuses,
		)
	}

	createContent := objectAt(t, document, "paths", "/v1/videos", "post", "requestBody", "content")
	assert.Contains(t, createContent, "application/json")
	assert.Contains(t, createContent, "multipart/form-data")
	assertSeedDanceRequestSchemas(t, document)

	contentSchema := objectAt(
		t,
		document,
		"paths",
		"/v1/videos/{task_id}/content",
		"get",
		"responses",
		"200",
		"content",
		"video/mp4",
		"schema",
	)
	assert.Equal(t, "string", stringAt(t, contentSchema, "type"))
	assert.Equal(t, "binary", stringAt(t, contentSchema, "format"))

	errorSchema := objectAt(t, document, "components", "schemas", "OpenAIError")
	assert.Equal(t, []any{"error"}, sliceAt(t, errorSchema, "required"))
	errorObject := objectAt(t, errorSchema, "properties", "error")
	assert.ElementsMatch(t, []any{"message", "type", "code"}, sliceAt(t, errorObject, "required"))
	for _, field := range []string{"message", "type", "code"} {
		assert.Equal(t, "string", stringAt(t, errorObject, "properties", field, "type"))
	}
}

func assertSeedDanceRequestSchemas(t *testing.T, document map[string]any) {
	t.Helper()

	jsonSchema := objectAt(t, document, "components", "schemas", "VideoCreateJSON")
	assert.ElementsMatch(t, []string{"model", "prompt"}, stringSlice(t, jsonSchema, "required"))
	assert.NotEqual(t, false, jsonSchema["additionalProperties"])
	for _, field := range []string{"model", "prompt", "image", "input_reference"} {
		assert.Equal(t, "string", stringAt(t, jsonSchema, "properties", field, "type"))
	}
	assert.Equal(
		t,
		[]any{"seedance-uncensored"},
		sliceAt(t, jsonSchema, "properties", "model", "enum"),
	)
	assert.Equal(t, float64(1), numberAt(t, jsonSchema, "properties", "prompt", "minLength"))
	assert.Equal(t, `\S`, stringAt(t, jsonSchema, "properties", "prompt", "pattern"))

	duration := objectAt(t, jsonSchema, "properties", "duration")
	assertDurationAlternatives(t, duration)
	assert.Equal(t, float64(15), numberAt(t, duration, "default"))

	assertDurationAlternatives(t, objectAt(t, jsonSchema, "properties", "seconds"))

	expectedSizes := []any{
		"854x480",
		"480x854",
		"480P",
		"1280x720",
		"720x1280",
		"720P",
		"1920x1080",
		"1080x1920",
		"1080P",
	}
	assert.Equal(t, expectedSizes, sliceAt(t, jsonSchema, "properties", "size", "enum"))
	assert.Equal(t, "array", stringAt(t, jsonSchema, "properties", "images", "type"))
	assert.Equal(t, float64(1), numberAt(t, jsonSchema, "properties", "images", "maxItems"))
	assert.Equal(t, "string", stringAt(t, jsonSchema, "properties", "images", "items", "type"))

	image := objectAt(t, jsonSchema, "properties", "image")
	assert.NotContains(t, image, "maxLength")
	metadata := objectAt(t, document, "components", "schemas", "VideoMetadata")
	assert.NotEqual(t, false, metadata["additionalProperties"])
	assert.Contains(t, objectAt(t, metadata, "properties"), "duration")
	assertDurationAlternatives(t, objectAt(t, metadata, "properties", "duration"))
	assert.Equal(t, expectedSizes, sliceAt(t, metadata, "properties", "resolution", "enum"))
	imageBase64 := objectAt(t, metadata, "properties", "image_base64")
	assert.Equal(t, "string", stringAt(t, imageBase64, "type"))
	assert.NotContains(t, imageBase64, "maxLength")
	for _, field := range []string{"prompt_optimization", "multi_shot", "strict_duration"} {
		assert.Equal(t, "boolean", stringAt(t, metadata, "properties", field, "type"))
	}
	assert.Equal(t, "string", stringAt(t, metadata, "properties", "negative_prompt", "type"))

	multipart := objectAt(t, document, "components", "schemas", "VideoCreateMultipart")
	assert.ElementsMatch(t, []string{"model", "prompt"}, stringSlice(t, multipart, "required"))
	assert.NotEqual(t, false, multipart["additionalProperties"])
	for _, field := range []string{"model", "prompt", "duration", "seconds", "size", "metadata"} {
		assert.Equal(t, "string", stringAt(t, multipart, "properties", field, "type"))
	}
	assert.Equal(
		t,
		[]any{"seedance-uncensored"},
		sliceAt(t, multipart, "properties", "model", "enum"),
	)
	assert.Equal(t, float64(1), numberAt(t, multipart, "properties", "prompt", "minLength"))
	assert.Equal(t, `\S`, stringAt(t, multipart, "properties", "prompt", "pattern"))
	assert.Equal(t, expectedSizes, sliceAt(t, multipart, "properties", "size", "enum"))
	for _, field := range []string{"image", "images"} {
		assert.Equal(t, "string", stringAt(t, multipart, "properties", field, "type"))
	}
	inputReferenceAlternatives := sliceAt(
		t,
		multipart,
		"properties",
		"input_reference",
		"anyOf",
	)
	require.Len(t, inputReferenceAlternatives, 2)
	assert.Equal(t, "string", stringAt(t, objectValue(t, inputReferenceAlternatives[0]), "type"))
	assert.Equal(t, "string", stringAt(t, objectValue(t, inputReferenceAlternatives[1]), "type"))
	assert.Equal(t, "binary", stringAt(t, objectValue(t, inputReferenceAlternatives[1]), "format"))

	video := objectAt(t, document, "components", "schemas", "OpenAIVideo")
	for _, field := range []string{
		"id",
		"task_id",
		"object",
		"model",
		"status",
		"progress",
		"created_at",
		"completed_at",
		"expires_at",
		"seconds",
		"size",
		"error",
	} {
		assert.Contains(t, objectAt(t, video, "properties"), field)
	}
	completedAt := objectAt(t, video, "properties", "completed_at")
	assert.NotContains(t, completedAt, "nullable")
	assert.Contains(t, stringAt(t, completedAt, "description"), "completed")
	assert.Contains(t, stringAt(t, completedAt, "description"), "failed")
}

func assertDurationAlternatives(t *testing.T, schema map[string]any) {
	t.Helper()

	alternatives := sliceAt(t, schema, "oneOf")
	require.Len(t, alternatives, 2)
	integerSchema := objectValue(t, alternatives[0])
	assert.Equal(t, "integer", stringAt(t, integerSchema, "type"))
	assert.Equal(t, float64(1), numberAt(t, integerSchema, "minimum"))
	assert.Equal(t, float64(15), numberAt(t, integerSchema, "maximum"))
	stringSchema := objectValue(t, alternatives[1])
	assert.Equal(t, "string", stringAt(t, stringSchema, "type"))
	assert.Equal(t, "^[0-9]+$", stringAt(t, stringSchema, "pattern"))
}

func assertBearerSecurity(t *testing.T, operation map[string]any, operationName string) {
	t.Helper()

	security := sliceAt(t, operation, "security")
	require.Len(t, security, 1, operationName)
	require.Empty(t, sliceAt(t, objectValue(t, security[0]), "BearerAuth"), operationName)
}

func assertNestedErrorResponses(
	t *testing.T,
	document map[string]any,
	operation map[string]any,
	operationName string,
	statuses []string,
) {
	t.Helper()

	for _, status := range statuses {
		response := responseAt(t, operation, status)
		if ref, ok := response["$ref"].(string); ok {
			response = resolveRef(t, document, ref)
		}
		ref := stringAt(t, response, "content", "application/json", "schema", "$ref")
		assert.Equal(t, nestedErrorSchemaRef, ref, "%s response %s", operationName, status)
	}
}

func assertLocalRefsResolve(t *testing.T, document map[string]any) {
	t.Helper()

	walkDocument(document, func(value any) {
		object, ok := value.(map[string]any)
		if !ok {
			return
		}
		ref, ok := object["$ref"].(string)
		if !ok {
			return
		}
		require.True(t, strings.HasPrefix(ref, "#/"), "only local refs are allowed in this contract: %s", ref)
		_ = resolveRef(t, document, ref)
	})
}

func assertExamplesMatchBasicSchema(t *testing.T, document map[string]any) {
	t.Helper()

	createContent := objectAt(t, document, "paths", "/v1/videos", "post", "requestBody", "content")
	for _, mediaType := range []string{"application/json", "multipart/form-data"} {
		media := objectValue(t, createContent[mediaType])
		example := valueAt(t, media, "example")
		schema := objectAt(t, media, "schema")
		validateBasicSchema(t, document, schema, example, "POST /v1/videos "+mediaType+" example")
	}

	for _, operationPath := range [][]string{
		{"paths", "/v1/videos", "post"},
		{"paths", "/v1/videos/{task_id}", "get"},
	} {
		operation := objectAt(t, document, operationPath...)
		media := objectAt(t, operation, "responses", "200", "content", "application/json")
		validateBasicSchema(
			t,
			document,
			objectAt(t, media, "schema"),
			valueAt(t, media, "example"),
			strings.Join(operationPath, "/")+" response example",
		)
	}

	for _, operationPath := range [][]string{
		{"paths", "/v1/videos", "post", "400", "401", "403", "429", "502", "503"},
		{"paths", "/v1/videos/{task_id}", "get", "400", "401", "404", "500"},
		{"paths", "/v1/videos/{task_id}/content", "get", "400", "401", "404", "429", "502", "504"},
	} {
		operation := objectAt(t, document, operationPath[:3]...)
		for _, status := range operationPath[3:] {
			response := responseAt(t, operation, status)
			if ref, ok := response["$ref"].(string); ok {
				response = resolveRef(t, document, ref)
			}
			media := objectAt(t, response, "content", "application/json")
			validateMediaExamples(
				t,
				document,
				media,
				strings.Join(operationPath[:3], "/")+" "+status,
			)
		}
	}
}

func validateMediaExamples(
	t *testing.T,
	document map[string]any,
	media map[string]any,
	location string,
) {
	t.Helper()

	schema := objectAt(t, media, "schema")
	if example, ok := media["example"]; ok {
		validateBasicSchema(t, document, schema, example, location+" example")
		return
	}

	examples := objectAt(t, media, "examples")
	require.NotEmpty(t, examples, "%s must publish at least one example", location)
	for name, rawExample := range examples {
		example := objectValue(t, rawExample)
		validateBasicSchema(
			t,
			document,
			schema,
			valueAt(t, example, "value"),
			location+" "+name+" example",
		)
	}
}

func responseAt(t *testing.T, operation map[string]any, status string) map[string]any {
	t.Helper()

	return objectAt(t, operation, "responses", status)
}

func validateBasicSchema(
	t *testing.T,
	document map[string]any,
	schema map[string]any,
	value any,
	location string,
) {
	t.Helper()

	if ref, ok := schema["$ref"].(string); ok {
		validateBasicSchema(t, document, resolveRef(t, document, ref), value, location)
		return
	}

	if alternatives, ok := schema["oneOf"].([]any); ok {
		for _, alternative := range alternatives {
			if basicSchemaMatches(document, objectValue(t, alternative), value) {
				return
			}
		}
		assert.Fail(t, "example does not match any oneOf schema", location)
		return
	}

	if alternatives, ok := schema["anyOf"].([]any); ok {
		for _, alternative := range alternatives {
			if basicSchemaMatches(document, objectValue(t, alternative), value) {
				return
			}
		}
		assert.Fail(t, "example does not match any anyOf schema", location)
		return
	}

	schemaType, _ := schema["type"].(string)
	switch schemaType {
	case "object":
		object, ok := value.(map[string]any)
		require.True(t, ok, "%s must be an object", location)
		for _, requiredField := range stringSlice(t, schema, "required") {
			assert.Contains(t, object, requiredField, "%s required property", location)
		}
		properties, _ := schema["properties"].(map[string]any)
		for name, propertyValue := range object {
			propertySchema, exists := properties[name]
			if !exists {
				continue
			}
			validateBasicSchema(t, document, objectValue(t, propertySchema), propertyValue, location+"."+name)
		}
	case "array":
		values, ok := value.([]any)
		require.True(t, ok, "%s must be an array", location)
		if maxItems, ok := numberValue(schema["maxItems"]); ok {
			assert.LessOrEqual(t, float64(len(values)), maxItems, "%s maxItems", location)
		}
		if itemSchema, ok := schema["items"].(map[string]any); ok {
			for index, item := range values {
				validateBasicSchema(t, document, itemSchema, item, fmt.Sprintf("%s[%d]", location, index))
			}
		}
	case "string":
		_, ok := value.(string)
		require.True(t, ok, "%s must be a string", location)
	case "integer":
		number, ok := numberValue(value)
		require.True(t, ok, "%s must be an integer", location)
		assert.Equal(t, number, float64(int64(number)), "%s must be an integer", location)
		if minimum, ok := numberValue(schema["minimum"]); ok {
			assert.GreaterOrEqual(t, number, minimum, "%s minimum", location)
		}
		if maximum, ok := numberValue(schema["maximum"]); ok {
			assert.LessOrEqual(t, number, maximum, "%s maximum", location)
		}
	case "number":
		_, ok := numberValue(value)
		require.True(t, ok, "%s must be numeric", location)
	case "boolean":
		_, ok := value.(bool)
		require.True(t, ok, "%s must be boolean", location)
	}

	if enum, ok := schema["enum"].([]any); ok {
		assert.Contains(t, enum, value, "%s enum", location)
	}
	if pattern, ok := schema["pattern"].(string); ok {
		text, ok := value.(string)
		require.True(t, ok, "%s pattern applies to a string", location)
		assert.Regexp(t, regexp.MustCompile(pattern), text, "%s pattern", location)
	}
}

func basicSchemaMatches(document map[string]any, schema map[string]any, value any) bool {
	if ref, ok := schema["$ref"].(string); ok {
		schema = resolveRefWithoutTest(document, ref)
		if schema == nil {
			return false
		}
	}

	schemaType, _ := schema["type"].(string)
	switch schemaType {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		text, ok := value.(string)
		if !ok {
			return false
		}
		if pattern, ok := schema["pattern"].(string); ok {
			return regexp.MustCompile(pattern).MatchString(text)
		}
		return true
	case "integer":
		number, ok := numberValue(value)
		return ok && number == float64(int64(number))
	case "number":
		_, ok := numberValue(value)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	default:
		return false
	}
}

func walkDocument(value any, visit func(any)) {
	visit(value)
	switch typed := value.(type) {
	case map[string]any:
		for _, child := range typed {
			walkDocument(child, visit)
		}
	case []any:
		for _, child := range typed {
			walkDocument(child, visit)
		}
	}
}

func resolveRef(t *testing.T, document map[string]any, ref string) map[string]any {
	t.Helper()

	resolved := resolveRefWithoutTest(document, ref)
	require.NotNil(t, resolved, "unresolved local ref %s", ref)
	return resolved
}

func resolveRefWithoutTest(document map[string]any, ref string) map[string]any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}

	var current any = document
	for _, rawSegment := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		segment := strings.ReplaceAll(strings.ReplaceAll(rawSegment, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current, ok = object[segment]
		if !ok {
			return nil
		}
	}

	resolved, _ := current.(map[string]any)
	return resolved
}

func objectAt(t *testing.T, object map[string]any, path ...string) map[string]any {
	t.Helper()

	return objectValue(t, valueAt(t, object, path...))
}

func valueAt(t *testing.T, object map[string]any, path ...string) any {
	t.Helper()

	var current any = object
	for _, segment := range path {
		currentObject, ok := current.(map[string]any)
		require.True(t, ok, "path %s is not an object at %s", strings.Join(path, "."), segment)
		var exists bool
		current, exists = currentObject[segment]
		require.True(t, exists, "path %s is missing %s", strings.Join(path, "."), segment)
	}
	return current
}

func objectValue(t *testing.T, value any) map[string]any {
	t.Helper()

	object, ok := value.(map[string]any)
	require.True(t, ok, "expected object, got %T", value)
	return object
}

func sliceAt(t *testing.T, object map[string]any, path ...string) []any {
	t.Helper()

	value := valueAt(t, object, path...)
	items, ok := value.([]any)
	require.True(t, ok, "path %s must be an array, got %T", strings.Join(path, "."), value)
	return items
}

func stringAt(t *testing.T, object map[string]any, path ...string) string {
	t.Helper()

	value := valueAt(t, object, path...)
	text, ok := value.(string)
	require.True(t, ok, "path %s must be a string, got %T", strings.Join(path, "."), value)
	return text
}

func numberAt(t *testing.T, object map[string]any, path ...string) float64 {
	t.Helper()

	value := valueAt(t, object, path...)
	number, ok := numberValue(value)
	require.True(t, ok, "path %s must be numeric, got %T", strings.Join(path, "."), value)
	return number
}

func stringSlice(t *testing.T, object map[string]any, path ...string) []string {
	t.Helper()

	var current any = object
	for _, segment := range path {
		currentObject, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		var exists bool
		current, exists = currentObject[segment]
		if !exists {
			return nil
		}
	}
	rawValues, ok := current.([]any)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(rawValues))
	for _, rawValue := range rawValues {
		value, ok := rawValue.(string)
		require.True(t, ok)
		values = append(values, value)
	}
	return values
}

func numberValue(value any) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case uint64:
		return float64(number), true
	case float64:
		return number, true
	default:
		return 0, false
	}
}

func mapKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	return keys
}
