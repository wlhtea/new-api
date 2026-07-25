package seedance

import (
	"bytes"
	"encoding/base64"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func jsonContext(t *testing.T, body string) *gin.Context {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}

func multipartRequest(
	t *testing.T,
	fields map[string]string,
	fileField string,
	fileName string,
	fileBytes []byte,
) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for name, value := range fields {
		require.NoError(t, writer.WriteField(name, value))
	}
	if fileField != "" {
		part, err := writer.CreateFormFile(fileField, fileName)
		require.NoError(t, err)
		_, err = part.Write(fileBytes)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return body, writer.FormDataContentType()
}

func multipartContext(t *testing.T, body *bytes.Buffer, contentType string) *gin.Context {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", bytes.NewReader(body.Bytes()))
	c.Request.Header.Set("Content-Type", contentType)
	return c
}

func TestNormalizeJSONDurationContract(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		duration int
		code     string
	}{
		{"missing defaults to fifteen", `{"prompt":"p"}`, 15, ""},
		{"null defaults to fifteen", `{"prompt":"p","duration":null}`, 15, ""},
		{"integer", `{"prompt":"p","duration":10}`, 10, ""},
		{"decimal string integer", `{"prompt":"p","seconds":"10"}`, 10, ""},
		{"same sources", `{"prompt":"p","duration":10,"seconds":"10","metadata":{"duration":10}}`, 10, ""},
		{"zero", `{"prompt":"p","duration":0}`, 0, "invalid_duration"},
		{"empty", `{"prompt":"p","duration":""}`, 0, "invalid_duration"},
		{"negative", `{"prompt":"p","duration":-1}`, 0, "invalid_duration"},
		{"fraction", `{"prompt":"p","duration":1.5}`, 0, "invalid_duration"},
		{"exponent", `{"prompt":"p","duration":1e1}`, 0, "invalid_duration"},
		{"nonnumeric", `{"prompt":"p","duration":"abc"}`, 0, "invalid_duration"},
		{"over provider maximum", `{"prompt":"p","duration":16}`, 0, "invalid_duration"},
		{"conflicting sources", `{"prompt":"p","duration":9,"seconds":10}`, 0, "invalid_duration"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input, taskErr := parseJSONRequest(jsonContext(t, test.body))
			require.Nil(t, taskErr)
			got, taskErr := normalizeScalars(input)
			if test.code != "" {
				require.NotNil(t, taskErr)
				assert.Equal(t, test.code, taskErr.Code)
				return
			}
			require.Nil(t, taskErr)
			assert.Equal(t, test.duration, got.Duration)
		})
	}
}

func TestNormalizeJSONResolutionAndMetadata(t *testing.T) {
	input, taskErr := parseJSONRequest(jsonContext(t, `{
      "prompt":"  flower  ",
      "size":"1920x1080",
      "metadata":{
        "resolution":"1080p",
        "prompt_optimization":false,
        "multi_shot":true,
        "strict_duration":false,
        "negative_prompt":"blur"
      }
    }`))
	require.Nil(t, taskErr)

	got, taskErr := normalizeScalars(input)
	require.Nil(t, taskErr)
	assert.Equal(t, "flower", got.Prompt)
	assert.Equal(t, "1080P", got.Resolution)
	require.NotNil(t, got.PromptOptimization)
	assert.False(t, *got.PromptOptimization)
	require.NotNil(t, got.MultiShot)
	assert.True(t, *got.MultiShot)
	require.NotNil(t, got.StrictDuration)
	assert.False(t, *got.StrictDuration)
	assert.Equal(t, "blur", got.NegativePrompt)
}

func TestNormalizeJSONRejectsResolutionConflict(t *testing.T) {
	input, taskErr := parseJSONRequest(jsonContext(t,
		`{"prompt":"p","size":"1280x720","metadata":{"resolution":"1080P"}}`,
	))
	require.Nil(t, taskErr)
	_, taskErr = normalizeScalars(input)
	require.NotNil(t, taskErr)
	assert.Equal(t, "invalid_resolution", taskErr.Code)
}

func TestNormalizeJSONRejectsNonStringSize(t *testing.T) {
	input, taskErr := parseJSONRequest(jsonContext(t, `{"prompt":"p","size":720}`))
	require.Nil(t, taskErr)

	_, taskErr = normalizeScalars(input)
	require.NotNil(t, taskErr)
	assert.Equal(t, "invalid_request", taskErr.Code)
}

func TestNormalizeJSONRejectsNonStringMetadataResolution(t *testing.T) {
	input, taskErr := parseJSONRequest(jsonContext(t,
		`{"prompt":"p","metadata":{"resolution":true}}`,
	))
	require.Nil(t, taskErr)

	_, taskErr = normalizeScalars(input)
	require.NotNil(t, taskErr)
	assert.Equal(t, "invalid_request", taskErr.Code)
}

func TestNormalizeJSONRejectsStringBoolean(t *testing.T) {
	input, taskErr := parseJSONRequest(jsonContext(t,
		`{"prompt":"p","metadata":{"strict_duration":"false"}}`,
	))
	require.Nil(t, taskErr)
	_, taskErr = normalizeScalars(input)
	require.NotNil(t, taskErr)
	assert.Equal(t, "invalid_request", taskErr.Code)
}

func TestNormalizeMultipartRemovesTemporaryFilesAndCachesResult(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	originalLimit := constant.MaxFileDownloadMB
	constant.MaxFileDownloadMB = 1
	t.Cleanup(func() {
		constant.MaxFileDownloadMB = originalLimit
	})

	uploaded := testNoisyPNG(t, 700, 2)
	require.Greater(t, len(uploaded), 1*1024*1024)
	encoded := base64.StdEncoding.EncodeToString(uploaded)

	body, contentType := multipartRequest(t, map[string]string{
		"prompt":   "flower",
		"duration": "10",
		"metadata": `{"strict_duration":"false","multi_shot":"true"}`,
		"image":    "https://HOST/input.png",
	}, "input_reference", "input.png", uploaded)
	c := multipartContext(t, body, contentType)

	observedTemporaryFile := false
	loaderCalls := 0
	loader := func(string) (string, string, error) {
		loaderCalls++
		entries, err := os.ReadDir(tempDir)
		require.NoError(t, err)
		observedTemporaryFile = len(entries) > 0
		return "image/png", encoded, nil
	}

	first, taskErr := normalizeRequestWithLoader(c, loader)
	require.Nil(t, taskErr)
	second, taskErr := normalizeRequestWithLoader(c, loader)
	require.Nil(t, taskErr)
	assert.Same(t, first, second)
	assert.True(t, observedTemporaryFile, "multipart parsing must spill the oversized file to TMPDIR")
	assert.Equal(t, 1, loaderCalls)
	assert.Equal(t, 10, first.Duration)
	require.NotNil(t, first.StrictDuration)
	assert.False(t, *first.StrictDuration)
	require.NotNil(t, first.MultiShot)
	assert.True(t, *first.MultiShot)

	entries, err := os.ReadDir(tempDir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestNormalizeMultipartRemovesTemporaryFilesAfterImageFailure(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	originalLimit := constant.MaxFileDownloadMB
	constant.MaxFileDownloadMB = 1
	t.Cleanup(func() {
		constant.MaxFileDownloadMB = originalLimit
	})

	uploaded := testNoisyPNG(t, 700, 3)
	require.Greater(t, len(uploaded), 1*1024*1024)
	body, contentType := multipartRequest(t, map[string]string{
		"prompt": "flower",
		"image":  "https://HOST/input.png",
	}, "input_reference", "input.png", uploaded)

	observedTemporaryFile := false
	loader := func(string) (string, string, error) {
		entries, err := os.ReadDir(tempDir)
		require.NoError(t, err)
		observedTemporaryFile = len(entries) > 0
		return "", "", errors.New("download failed")
	}
	_, taskErr := normalizeRequestWithLoader(
		multipartContext(t, body, contentType),
		loader,
	)
	require.NotNil(t, taskErr)
	assert.Equal(t, "invalid_image", taskErr.Code)
	assert.True(t, observedTemporaryFile, "multipart parsing must spill the oversized file to TMPDIR")

	entries, err := os.ReadDir(tempDir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestNormalizeMultipartRejectsEmptyInputReferenceFile(t *testing.T) {
	body, contentType := multipartRequest(t, map[string]string{
		"prompt": "flower",
	}, "input_reference", "empty.png", nil)

	_, taskErr := normalizeRequest(multipartContext(t, body, contentType))
	require.NotNil(t, taskErr)
	assert.Equal(t, "invalid_image", taskErr.Code)
}

func TestNormalizeMultipartRejectsMultipleInputReferenceFiles(t *testing.T) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	require.NoError(t, writer.WriteField("prompt", "flower"))
	for _, name := range []string{"first.png", "second.png"} {
		part, err := writer.CreateFormFile("input_reference", name)
		require.NoError(t, err)
		_, err = part.Write(testPNG(t, 240, 240))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	_, taskErr := normalizeRequest(multipartContext(t, body, writer.FormDataContentType()))
	require.NotNil(t, taskErr)
	assert.Equal(t, "invalid_image", taskErr.Code)
}

func TestNormalizeMultipartRejectsInvalidBooleanText(t *testing.T) {
	body, contentType := multipartRequest(t, map[string]string{
		"prompt":   "flower",
		"metadata": `{"strict_duration":"yes"}`,
	}, "", "", nil)

	_, taskErr := normalizeRequest(multipartContext(t, body, contentType))
	require.NotNil(t, taskErr)
	assert.Equal(t, "invalid_request", taskErr.Code)
}

func TestNormalizeMultipartMatchesJSONScalarContract(t *testing.T) {
	body, contentType := multipartRequest(t, map[string]string{
		"prompt":   " flower ",
		"duration": "10",
		"seconds":  "10",
		"size":     "1920x1080",
		"metadata": `{"duration":"10","resolution":"1080p","prompt_optimization":"false","negative_prompt":"blur"}`,
	}, "", "", nil)

	got, taskErr := normalizeRequest(multipartContext(t, body, contentType))
	require.Nil(t, taskErr)
	assert.Equal(t, "flower", got.Prompt)
	assert.Equal(t, 10, got.Duration)
	assert.Equal(t, "1080P", got.Resolution)
	require.NotNil(t, got.PromptOptimization)
	assert.False(t, *got.PromptOptimization)
	assert.Equal(t, "blur", got.NegativePrompt)
}

func TestNormalizeRequestRejectsInvalidCachedValues(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{"wrong type", "not a normalized request"},
		{"nil pointer", (*NormalizedRequest)(nil)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := jsonContext(t, `{"prompt":"p"}`)
			c.Set(normalizedRequestContextKey, test.value)
			_, taskErr := normalizeRequest(c)
			require.NotNil(t, taskErr)
			assert.Equal(t, "invalid_request", taskErr.Code)
		})
	}
}

func TestGetNormalizedRequestRequiresValidCachedValue(t *testing.T) {
	c := jsonContext(t, `{"prompt":"p"}`)
	_, err := getNormalizedRequest(c)
	require.EqualError(t, err, "Seed Dance request was not normalized")

	c.Set(normalizedRequestContextKey, "wrong")
	_, err = getNormalizedRequest(c)
	require.EqualError(t, err, "invalid Seed Dance normalized request")

	want := &NormalizedRequest{Prompt: "p"}
	c.Set(normalizedRequestContextKey, want)
	got, err := getNormalizedRequest(c)
	require.NoError(t, err)
	assert.Same(t, want, got)
}
