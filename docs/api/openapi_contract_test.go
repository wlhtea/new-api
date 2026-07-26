package api_test

import (
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

	matches, err := filepath.Glob(filepath.Join(fixtureDir, "*.json"))
	require.NoError(t, err)
	require.Len(t, matches, 6)

	for _, name := range matches {
		data, readErr := os.ReadFile(name)
		require.NoError(t, readErr)
		text := string(data)
		assert.NotContains(t, text, "Authorization")
		assert.NotContains(t, text, "image_base64")
		assert.NotContains(t, text, "video_base64")
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
		path   string
		method string
	}{
		{path: "/v1/videos", method: "post"},
		{path: "/v1/videos/{task_id}", method: "get"},
		{path: "/v1/videos/{task_id}/content", method: "get"},
	}

	for _, expected := range operations {
		operation := objectAt(t, document, "paths", expected.path, expected.method)
		assertBearerSecurity(t, operation, expected.method+" "+expected.path)
		assertNestedErrorResponses(t, document, operation, expected.method+" "+expected.path)
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
	for _, field := range []string{"model", "prompt", "image", "input_reference"} {
		assert.Equal(t, "string", stringAt(t, jsonSchema, "properties", field, "type"))
	}

	duration := objectAt(t, jsonSchema, "properties", "duration")
	assert.Equal(t, "integer", stringAt(t, duration, "type"))
	assert.Equal(t, float64(1), numberAt(t, duration, "minimum"))
	assert.Equal(t, float64(15), numberAt(t, duration, "maximum"))
	assert.Equal(t, float64(15), numberAt(t, duration, "default"))

	secondsAlternatives := sliceAt(t, jsonSchema, "properties", "seconds", "oneOf")
	require.Len(t, secondsAlternatives, 2)
	assert.Equal(t, "integer", stringAt(t, objectValue(t, secondsAlternatives[0]), "type"))
	assert.Equal(t, "string", stringAt(t, objectValue(t, secondsAlternatives[1]), "type"))
	assert.Equal(t, "^[0-9]+$", stringAt(t, objectValue(t, secondsAlternatives[1]), "pattern"))

	expectedSizes := []any{
		"854x480",
		"480x854",
		"1280x720",
		"720x1280",
		"1920x1080",
		"1080x1920",
	}
	assert.Equal(t, expectedSizes, sliceAt(t, jsonSchema, "properties", "size", "enum"))
	assert.Equal(t, "array", stringAt(t, jsonSchema, "properties", "images", "type"))
	assert.Equal(t, float64(1), numberAt(t, jsonSchema, "properties", "images", "maxItems"))
	assert.Equal(t, "string", stringAt(t, jsonSchema, "properties", "images", "items", "type"))

	image := objectAt(t, jsonSchema, "properties", "image")
	assert.NotContains(t, image, "maxLength")
	metadata := objectAt(t, document, "components", "schemas", "VideoMetadata")
	assert.Contains(t, objectAt(t, metadata, "properties"), "duration")
	assert.Equal(
		t,
		[]any{"480P", "720P", "1080P"},
		sliceAt(t, metadata, "properties", "resolution", "enum"),
	)
	imageBase64 := objectAt(t, metadata, "properties", "image_base64")
	assert.Equal(t, "string", stringAt(t, imageBase64, "type"))
	assert.NotContains(t, imageBase64, "maxLength")
	for _, field := range []string{"prompt_optimization", "multi_shot", "strict_duration"} {
		assert.Equal(t, "boolean", stringAt(t, metadata, "properties", field, "type"))
	}
	assert.Equal(t, "string", stringAt(t, metadata, "properties", "negative_prompt", "type"))

	multipart := objectAt(t, document, "components", "schemas", "VideoCreateMultipart")
	assert.ElementsMatch(t, []string{"model", "prompt"}, stringSlice(t, multipart, "required"))
	for _, field := range []string{"model", "prompt", "duration", "seconds", "size", "metadata"} {
		assert.Equal(t, "string", stringAt(t, multipart, "properties", field, "type"))
	}
	assert.Equal(t, "string", stringAt(t, multipart, "properties", "input_reference", "type"))
	assert.Equal(t, "binary", stringAt(t, multipart, "properties", "input_reference", "format"))

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
) {
	t.Helper()

	for _, status := range []string{"400", "401", "404", "429", "502", "504"} {
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
		{"paths", "/v1/videos", "post"},
		{"paths", "/v1/videos/{task_id}", "get"},
		{"paths", "/v1/videos/{task_id}/content", "get"},
	} {
		operation := objectAt(t, document, operationPath...)
		for _, status := range []string{"400", "401", "404", "429", "502", "504"} {
			response := responseAt(t, operation, status)
			if ref, ok := response["$ref"].(string); ok {
				response = resolveRef(t, document, ref)
			}
			media := objectAt(t, response, "content", "application/json")
			validateBasicSchema(
				t,
				document,
				objectAt(t, media, "schema"),
				valueAt(t, media, "example"),
				strings.Join(operationPath, "/")+" "+status+" example",
			)
		}
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
