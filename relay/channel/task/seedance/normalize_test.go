package seedance

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func TestNormalizeJSONRejectsStringBoolean(t *testing.T) {
	input, taskErr := parseJSONRequest(jsonContext(t,
		`{"prompt":"p","metadata":{"strict_duration":"false"}}`,
	))
	require.Nil(t, taskErr)
	_, taskErr = normalizeScalars(input)
	require.NotNil(t, taskErr)
	assert.Equal(t, "invalid_request", taskErr.Code)
}
