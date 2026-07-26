package seedance

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type parameterMatrixSolidImage struct {
	bounds image.Rectangle
	pixel  color.RGBA
}

func (i parameterMatrixSolidImage) ColorModel() color.Model {
	return color.RGBAModel
}

func (i parameterMatrixSolidImage) Bounds() image.Rectangle {
	return i.bounds
}

func (i parameterMatrixSolidImage) At(_, _ int) color.Color {
	return i.pixel
}

func parameterMatrixImage(
	t *testing.T,
	width int,
	height int,
	format string,
) []byte {
	t.Helper()
	img := parameterMatrixSolidImage{
		bounds: image.Rect(0, 0, width, height),
		pixel:  color.RGBA{R: 19, G: 83, B: 157, A: 255},
	}
	var encoded bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&encoded, img)
	case "jpeg":
		err = jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 90})
	case "gif":
		err = gif.Encode(&encoded, img, nil)
	default:
		t.Fatalf("unsupported parameter-matrix image format %q", format)
	}
	require.NoError(t, err)
	return encoded.Bytes()
}

func parameterMatrixJSONContext(t *testing.T, body string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/videos",
		strings.NewReader(body),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}

func parameterMatrixNormalizeJSON(
	t *testing.T,
	body string,
) (*NormalizedRequest, string) {
	t.Helper()
	normalized, taskErr := normalizeRequestWithLoader(
		parameterMatrixJSONContext(t, body),
		func(source string) (string, string, error) {
			t.Fatalf("unexpected remote image load for %q", source)
			return "", "", nil
		},
	)
	if taskErr != nil {
		require.Equal(t, http.StatusBadRequest, taskErr.StatusCode)
		require.True(t, taskErr.LocalError)
		return nil, taskErr.Code
	}
	return normalized, ""
}

func parameterMatrixRequireJSONError(
	t *testing.T,
	body string,
	wantCode string,
) {
	t.Helper()
	normalized, gotCode := parameterMatrixNormalizeJSON(t, body)
	require.Nil(t, normalized)
	require.Equal(t, wantCode, gotCode)
}

func parameterMatrixDurationBody(source string, rawValue string) string {
	switch source {
	case "duration":
		return fmt.Sprintf(`{"prompt":"p","duration":%s}`, rawValue)
	case "seconds":
		return fmt.Sprintf(`{"prompt":"p","seconds":%s}`, rawValue)
	case "metadata.duration":
		return fmt.Sprintf(
			`{"prompt":"p","metadata":{"duration":%s}}`,
			rawValue,
		)
	default:
		panic("unknown duration source " + source)
	}
}

func parameterMatrixResolutionBody(
	source string,
	value string,
	imageBase64 string,
) string {
	switch source {
	case "size":
		return fmt.Sprintf(
			`{"prompt":"p","image":%q,"size":%q}`,
			imageBase64,
			value,
		)
	case "metadata.resolution":
		return fmt.Sprintf(
			`{"prompt":"p","image":%q,"metadata":{"resolution":%q}}`,
			imageBase64,
			value,
		)
	default:
		panic("unknown resolution source " + source)
	}
}

type parameterMatrixMultipartField struct {
	name  string
	value string
}

type parameterMatrixMultipartFile struct {
	fieldName   string
	fileName    string
	contentType string
	data        []byte
}

func parameterMatrixMultipartContext(
	t *testing.T,
	fields []parameterMatrixMultipartField,
	files []parameterMatrixMultipartFile,
) *gin.Context {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for _, field := range fields {
		require.NoError(t, writer.WriteField(field.name, field.value))
	}
	for _, file := range files {
		header := make(textproto.MIMEHeader)
		header.Set(
			"Content-Disposition",
			fmt.Sprintf(
				`form-data; name=%q; filename=%q`,
				file.fieldName,
				file.fileName,
			),
		)
		if file.contentType != "" {
			header.Set("Content-Type", file.contentType)
		}
		part, err := writer.CreatePart(header)
		require.NoError(t, err)
		_, err = part.Write(file.data)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/videos",
		bytes.NewReader(body.Bytes()),
	)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	return c
}

func parameterMatrixNormalizeMultipart(
	t *testing.T,
	fields []parameterMatrixMultipartField,
	files []parameterMatrixMultipartFile,
) (*NormalizedRequest, string) {
	t.Helper()
	normalized, taskErr := normalizeRequestWithLoader(
		parameterMatrixMultipartContext(t, fields, files),
		func(source string) (string, string, error) {
			t.Fatalf("unexpected remote image load for %q", source)
			return "", "", nil
		},
	)
	if taskErr != nil {
		require.Equal(t, http.StatusBadRequest, taskErr.StatusCode)
		require.True(t, taskErr.LocalError)
		return nil, taskErr.Code
	}
	return normalized, ""
}

func parameterMatrixRequireMultipartError(
	t *testing.T,
	fields []parameterMatrixMultipartField,
	files []parameterMatrixMultipartFile,
	wantCode string,
) {
	t.Helper()
	normalized, gotCode := parameterMatrixNormalizeMultipart(t, fields, files)
	require.Nil(t, normalized)
	require.Equal(t, wantCode, gotCode)
}

func TestSeedDanceParameterMatrixPrompt(t *testing.T) {
	invalid := []struct {
		name string
		body string
	}{
		{"missing", `{}`},
		{"null", `{"prompt":null}`},
		{"empty", `{"prompt":""}`},
		{"blank spaces", `{"prompt":" \t\r\n "}`},
		{"number", `{"prompt":1}`},
		{"boolean", `{"prompt":true}`},
		{"array", `{"prompt":[]}`},
		{"object", `{"prompt":{}}`},
	}
	for _, test := range invalid {
		t.Run("rejects "+test.name, func(t *testing.T) {
			parameterMatrixRequireJSONError(t, test.body, "invalid_request")
		})
	}

	t.Run("trims a valid prompt", func(t *testing.T) {
		got, code := parameterMatrixNormalizeJSON(
			t,
			`{"prompt":" \t cute cat \r\n"}`,
		)
		require.Empty(t, code)
		require.NotNil(t, got)
		assert.Equal(t, "cute cat", got.Prompt)
	})
}

func TestSeedDanceParameterMatrixDurationAcceptedForms(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"missing defaults", `{"prompt":"p"}`, 15},
		{"null duration defaults", `{"prompt":"p","duration":null}`, 15},
		{"null seconds defaults", `{"prompt":"p","seconds":null}`, 15},
		{
			"null metadata duration defaults",
			`{"prompt":"p","metadata":{"duration":null}}`,
			15,
		},
		{"duration integer lower", `{"prompt":"p","duration":1}`, 1},
		{"duration integer upper", `{"prompt":"p","duration":15}`, 15},
		{"seconds integer lower", `{"prompt":"p","seconds":1}`, 1},
		{"seconds integer upper", `{"prompt":"p","seconds":15}`, 15},
		{
			"metadata integer lower",
			`{"prompt":"p","metadata":{"duration":1}}`,
			1,
		},
		{
			"metadata integer upper",
			`{"prompt":"p","metadata":{"duration":15}}`,
			15,
		},
		{"duration string lower", `{"prompt":"p","duration":"1"}`, 1},
		{"duration string upper", `{"prompt":"p","duration":"15"}`, 15},
		{"seconds string lower", `{"prompt":"p","seconds":"1"}`, 1},
		{"seconds string upper", `{"prompt":"p","seconds":"15"}`, 15},
		{
			"metadata string lower",
			`{"prompt":"p","metadata":{"duration":"1"}}`,
			1,
		},
		{
			"metadata string upper",
			`{"prompt":"p","metadata":{"duration":"15"}}`,
			15,
		},
		{"duration leading zero", `{"prompt":"p","duration":"01"}`, 1},
		{"seconds leading zero", `{"prompt":"p","seconds":"015"}`, 15},
		{
			"metadata leading zero",
			`{"prompt":"p","metadata":{"duration":"001"}}`,
			1,
		},
		{
			"all sources equal with mixed forms",
			`{
				"prompt":"p",
				"duration":1,
				"seconds":"01",
				"metadata":{"duration":"1"}
			}`,
			1,
		},
		{
			"null source does not conflict with explicit source",
			`{"prompt":"p","duration":null,"seconds":"15"}`,
			15,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, code := parameterMatrixNormalizeJSON(t, test.body)
			require.Empty(t, code)
			require.NotNil(t, got)
			assert.Equal(t, test.want, got.Duration)
		})
	}
}

func TestSeedDanceParameterMatrixDurationRejectsEveryInvalidJSONKind(t *testing.T) {
	invalidValues := []struct {
		name string
		raw  string
	}{
		{"zero", `0`},
		{"over maximum", `16`},
		{"negative", `-1`},
		{"fraction", `1.5`},
		{"exponent", `1e0`},
		{"empty string", `""`},
		{"blank string", `" "`},
		{"decimal string", `"1.0"`},
		{"signed string", `"+1"`},
		{"negative string", `"-1"`},
		{"alphabetic string", `"one"`},
		{"boolean true", `true`},
		{"boolean false", `false`},
		{"array", `[]`},
		{"object", `{}`},
		{"array containing integer", `[1]`},
		{"object containing integer", `{"value":1}`},
		{"integer overflow", `9223372036854775808`},
	}
	for _, source := range []string{
		"duration",
		"seconds",
		"metadata.duration",
	} {
		for _, value := range invalidValues {
			t.Run(source+"/"+value.name, func(t *testing.T) {
				parameterMatrixRequireJSONError(
					t,
					parameterMatrixDurationBody(source, value.raw),
					"invalid_duration",
				)
			})
		}
	}
}

func TestSeedDanceParameterMatrixDurationConflicts(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{
			"duration and seconds",
			`{"prompt":"p","duration":1,"seconds":2}`,
		},
		{
			"duration and metadata",
			`{"prompt":"p","duration":1,"metadata":{"duration":2}}`,
		},
		{
			"seconds and metadata",
			`{"prompt":"p","seconds":1,"metadata":{"duration":2}}`,
		},
		{
			"all three",
			`{
				"prompt":"p",
				"duration":1,
				"seconds":1,
				"metadata":{"duration":2}
			}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			parameterMatrixRequireJSONError(
				t,
				test.body,
				"invalid_duration",
			)
		})
	}
}

func TestSeedDanceParameterMatrixAllResolutionAliasesAtBothEntrypoints(
	t *testing.T,
) {
	imageBase64 := base64.StdEncoding.EncodeToString(
		parameterMatrixImage(t, 240, 240, "png"),
	)
	aliases := []struct {
		input string
		want  string
	}{
		{"854x480", "480P"},
		{"480x854", "480P"},
		{"480P", "480P"},
		{"1280x720", "720P"},
		{"720x1280", "720P"},
		{"720P", "720P"},
		{"1920x1080", "1080P"},
		{"1080x1920", "1080P"},
		{"1080P", "1080P"},
	}
	for _, source := range []string{"size", "metadata.resolution"} {
		for _, alias := range aliases {
			t.Run(source+"/"+alias.input, func(t *testing.T) {
				got, code := parameterMatrixNormalizeJSON(
					t,
					parameterMatrixResolutionBody(
						source,
						alias.input,
						imageBase64,
					),
				)
				require.Empty(t, code)
				require.NotNil(t, got)
				assert.Equal(t, alias.want, got.Resolution)
			})
			t.Run(source+"/trim-and-case/"+alias.input, func(t *testing.T) {
				got, code := parameterMatrixNormalizeJSON(
					t,
					parameterMatrixResolutionBody(
						source,
						" \t"+strings.ToLower(alias.input)+"\r\n ",
						imageBase64,
					),
				)
				require.Empty(t, code)
				require.NotNil(t, got)
				assert.Equal(t, alias.want, got.Resolution)
			})
		}
	}
}

func TestSeedDanceParameterMatrixResolutionDefaultsConflictsAndTypes(
	t *testing.T,
) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{"missing", `{"prompt":"p"}`, "720P"},
		{"size null", `{"prompt":"p","size":null}`, "720P"},
		{
			"metadata resolution null",
			`{"prompt":"p","metadata":{"resolution":null}}`,
			"720P",
		},
		{
			"both null",
			`{"prompt":"p","size":null,"metadata":{"resolution":null}}`,
			"720P",
		},
		{
			"equivalent aliases agree",
			`{
				"prompt":"p",
				"size":"1280x720",
				"metadata":{"resolution":"720p"}
			}`,
			"720P",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, code := parameterMatrixNormalizeJSON(t, test.body)
			require.Empty(t, code)
			require.NotNil(t, got)
			assert.Equal(t, test.want, got.Resolution)
		})
	}

	for _, body := range []string{
		`{"prompt":"p","size":"1280x720","metadata":{"resolution":"1080P"}}`,
		`{"prompt":"p","size":"480P","metadata":{"resolution":"720P"}}`,
	} {
		parameterMatrixRequireJSONError(t, body, "invalid_resolution")
	}

	for _, source := range []string{"size", "metadata.resolution"} {
		for _, test := range []struct {
			name string
			raw  string
			code string
		}{
			{"unsupported", `"640x360"`, "invalid_resolution"},
			{"empty", `""`, "invalid_resolution"},
			{"blank", `"   "`, "invalid_resolution"},
			{"number", `720`, "invalid_request"},
			{"boolean", `true`, "invalid_request"},
			{"array", `[]`, "invalid_request"},
			{"object", `{}`, "invalid_request"},
		} {
			t.Run(source+"/"+test.name, func(t *testing.T) {
				var body string
				if source == "size" {
					body = fmt.Sprintf(
						`{"prompt":"p","size":%s}`,
						test.raw,
					)
				} else {
					body = fmt.Sprintf(
						`{"prompt":"p","metadata":{"resolution":%s}}`,
						test.raw,
					)
				}
				parameterMatrixRequireJSONError(t, body, test.code)
			})
		}
	}

	t.Run("480P requires an image", func(t *testing.T) {
		parameterMatrixRequireJSONError(
			t,
			`{"prompt":"p","size":"480P"}`,
			"invalid_resolution",
		)
	})
}

func TestSeedDanceParameterMatrixMetadataBooleans(t *testing.T) {
	type boolPointer func(*NormalizedRequest) *bool
	fields := []struct {
		name string
		get  boolPointer
	}{
		{"prompt_optimization", func(got *NormalizedRequest) *bool {
			return got.PromptOptimization
		}},
		{"multi_shot", func(got *NormalizedRequest) *bool {
			return got.MultiShot
		}},
		{"strict_duration", func(got *NormalizedRequest) *bool {
			return got.StrictDuration
		}},
	}

	t.Run("omitted fields remain nil", func(t *testing.T) {
		got, code := parameterMatrixNormalizeJSON(t, `{"prompt":"p"}`)
		require.Empty(t, code)
		require.NotNil(t, got)
		for _, field := range fields {
			assert.Nil(t, field.get(got), field.name)
		}
	})

	for _, field := range fields {
		for _, value := range []struct {
			name    string
			raw     string
			wantNil bool
			want    bool
		}{
			{"null", `null`, true, false},
			{"true", `true`, false, true},
			{"false", `false`, false, false},
		} {
			t.Run(field.name+"/"+value.name, func(t *testing.T) {
				body := fmt.Sprintf(
					`{"prompt":"p","metadata":{%q:%s}}`,
					field.name,
					value.raw,
				)
				got, code := parameterMatrixNormalizeJSON(t, body)
				require.Empty(t, code)
				require.NotNil(t, got)
				if value.wantNil {
					assert.Nil(t, field.get(got))
					return
				}
				require.NotNil(t, field.get(got))
				assert.Equal(t, value.want, *field.get(got))
			})
		}
		for _, wrong := range []struct {
			name string
			raw  string
		}{
			{"string true", `"true"`},
			{"string false", `"false"`},
			{"number zero", `0`},
			{"number one", `1`},
			{"empty string", `""`},
			{"array", `[]`},
			{"object", `{}`},
		} {
			t.Run(field.name+"/rejects "+wrong.name, func(t *testing.T) {
				parameterMatrixRequireJSONError(
					t,
					fmt.Sprintf(
						`{"prompt":"p","metadata":{%q:%s}}`,
						field.name,
						wrong.raw,
					),
					"invalid_request",
				)
			})
		}
	}
}

func TestSeedDanceParameterMatrixNegativePrompt(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want string
	}{
		{"omitted", "", ""},
		{"null", `null`, ""},
		{"empty", `""`, ""},
		{"normal", `"blur, watermark"`, "blur, watermark"},
		{"whitespace is preserved", `"  blur  "`, "  blur  "},
		{"unicode", `"模糊、水印"`, "模糊、水印"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `{"prompt":"p"}`
			if test.raw != "" {
				body = fmt.Sprintf(
					`{"prompt":"p","metadata":{"negative_prompt":%s}}`,
					test.raw,
				)
			}
			got, code := parameterMatrixNormalizeJSON(t, body)
			require.Empty(t, code)
			require.NotNil(t, got)
			assert.Equal(t, test.want, got.NegativePrompt)
		})
	}

	for _, wrong := range []struct {
		name string
		raw  string
	}{
		{"number", `1`},
		{"boolean", `false`},
		{"array", `[]`},
		{"object", `{}`},
	} {
		t.Run("rejects "+wrong.name, func(t *testing.T) {
			parameterMatrixRequireJSONError(
				t,
				fmt.Sprintf(
					`{"prompt":"p","metadata":{"negative_prompt":%s}}`,
					wrong.raw,
				),
				"invalid_request",
			)
		})
	}
}

func TestSeedDanceParameterMatrixUnknownJSONFieldsAreNotForwarded(
	t *testing.T,
) {
	body := `{
		"model":"seedance-uncensored",
		"prompt":"p",
		"duration":1,
		"size":"720p",
		"unknown_top_level":{"secret":"DO_NOT_FORWARD"},
		"metadata":{
			"prompt_optimization":false,
			"unknown_metadata":["DO_NOT_FORWARD"]
		}
	}`
	c := parameterMatrixJSONContext(t, body)
	info := &relaycommon.RelayInfo{
		OriginModelName: ModelName,
		TaskRelayInfo:   &relaycommon.TaskRelayInfo{},
	}
	adaptor := &TaskAdaptor{}
	require.Nil(t, adaptor.ValidateRequestAndSetAction(c, info))

	reader, err := adaptor.BuildRequestBody(c, info)
	require.NoError(t, err)
	upstreamBody, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"prompt":"p",
		"duration":1,
		"resolution":"720P",
		"prompt_optimization":false
	}`, string(upstreamBody))
	assert.NotContains(t, string(upstreamBody), "unknown")
	assert.NotContains(t, string(upstreamBody), "DO_NOT_FORWARD")
}

func TestSeedDanceParameterMatrixFiveImageEntrypoints(t *testing.T) {
	pngBytes := parameterMatrixImage(t, 240, 240, "png")
	encoded := base64.StdEncoding.EncodeToString(pngBytes)
	jsonBodies := []struct {
		name string
		body string
	}{
		{"image", fmt.Sprintf(`{"prompt":"p","image":%q}`, encoded)},
		{
			"images",
			fmt.Sprintf(`{"prompt":"p","images":[%q]}`, encoded),
		},
		{
			"input_reference text",
			fmt.Sprintf(`{"prompt":"p","input_reference":%q}`, encoded),
		},
		{
			"metadata.image_base64",
			fmt.Sprintf(
				`{"prompt":"p","metadata":{"image_base64":%q}}`,
				encoded,
			),
		},
	}
	for _, test := range jsonBodies {
		t.Run(test.name, func(t *testing.T) {
			got, code := parameterMatrixNormalizeJSON(t, test.body)
			require.Empty(t, code)
			require.NotNil(t, got)
			assert.Equal(t, encoded, got.ImageBase64)
		})
	}

	t.Run("multipart input_reference file", func(t *testing.T) {
		got, code := parameterMatrixNormalizeMultipart(
			t,
			[]parameterMatrixMultipartField{{name: "prompt", value: "p"}},
			[]parameterMatrixMultipartFile{{
				fieldName:   "input_reference",
				fileName:    "reference.png",
				contentType: "image/png",
				data:        pngBytes,
			}},
		)
		require.Empty(t, code)
		require.NotNil(t, got)
		assert.Equal(t, encoded, got.ImageBase64)
	})
}

func TestSeedDanceParameterMatrixImageNullEmptyArrayAndTypes(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{"image null", `{"prompt":"p","image":null}`},
		{"input_reference null", `{"prompt":"p","input_reference":null}`},
		{
			"metadata image_base64 null",
			`{"prompt":"p","metadata":{"image_base64":null}}`,
		},
		{"images null", `{"prompt":"p","images":null}`},
		{"images empty array", `{"prompt":"p","images":[]}`},
		{"images one null", `{"prompt":"p","images":[null]}`},
		{"images one empty string", `{"prompt":"p","images":[""]}`},
	} {
		t.Run(test.name+" means no image", func(t *testing.T) {
			got, code := parameterMatrixNormalizeJSON(t, test.body)
			require.Empty(t, code)
			require.NotNil(t, got)
			assert.Empty(t, got.ImageBase64)
		})
	}

	for _, field := range []string{
		"image",
		"input_reference",
		"metadata.image_base64",
	} {
		for _, wrong := range []struct {
			name string
			raw  string
		}{
			{"number", `1`},
			{"boolean", `true`},
			{"array", `[]`},
			{"object", `{}`},
		} {
			t.Run(field+"/rejects "+wrong.name, func(t *testing.T) {
				var body string
				if field == "metadata.image_base64" {
					body = fmt.Sprintf(
						`{"prompt":"p","metadata":{"image_base64":%s}}`,
						wrong.raw,
					)
				} else {
					body = fmt.Sprintf(
						`{"prompt":"p",%q:%s}`,
						field,
						wrong.raw,
					)
				}
				parameterMatrixRequireJSONError(t, body, "invalid_image")
			})
		}
	}

	for _, wrong := range []struct {
		name string
		raw  string
	}{
		{"string", `"PAYLOAD"`},
		{"number", `1`},
		{"boolean", `false`},
		{"object", `{}`},
		{"array item number", `[1]`},
		{"array item object", `[{}]`},
	} {
		t.Run("images rejects "+wrong.name, func(t *testing.T) {
			parameterMatrixRequireJSONError(
				t,
				fmt.Sprintf(`{"prompt":"p","images":%s}`, wrong.raw),
				"invalid_image",
			)
		})
	}

	encoded := base64.StdEncoding.EncodeToString(
		parameterMatrixImage(t, 240, 240, "png"),
	)
	t.Run("images rejects two items even when identical", func(t *testing.T) {
		parameterMatrixRequireJSONError(
			t,
			fmt.Sprintf(
				`{"prompt":"p","images":[%q,%q]}`,
				encoded,
				encoded,
			),
			"invalid_image",
		)
	})
	t.Run("same decoded bytes across aliases are accepted", func(t *testing.T) {
		got, code := parameterMatrixNormalizeJSON(
			t,
			fmt.Sprintf(`{
				"prompt":"p",
				"image":%q,
				"images":[%q],
				"input_reference":"data:image/png;base64,%s",
				"metadata":{"image_base64":%q}
			}`, encoded, encoded, encoded, encoded),
		)
		require.Empty(t, code)
		require.NotNil(t, got)
		assert.Equal(t, encoded, got.ImageBase64)
	})
	t.Run("different decoded bytes across aliases are rejected", func(t *testing.T) {
		other := base64.StdEncoding.EncodeToString(
			parameterMatrixImage(t, 241, 240, "png"),
		)
		parameterMatrixRequireJSONError(
			t,
			fmt.Sprintf(
				`{"prompt":"p","image":%q,"input_reference":%q}`,
				encoded,
				other,
			),
			"invalid_image",
		)
	})
}

func TestSeedDanceParameterMatrixImageDimensionAndRatioBoundaries(
	t *testing.T,
) {
	valid := []struct {
		name   string
		width  int
		height int
	}{
		{"minimum dimensions", 240, 240},
		{"maximum width at exact 8 to 1", 8000, 1000},
		{"maximum height at exact 1 to 8", 1000, 8000},
		{"exact 8 to 1 with minimum height", 1920, 240},
		{"exact 1 to 8 with minimum width", 240, 1920},
	}
	for _, test := range valid {
		t.Run("accepts "+test.name, func(t *testing.T) {
			data := parameterMatrixImage(
				t,
				test.width,
				test.height,
				"png",
			)
			encoded := base64.StdEncoding.EncodeToString(data)
			got, code := parameterMatrixNormalizeJSON(
				t,
				fmt.Sprintf(`{"prompt":"p","image":%q}`, encoded),
			)
			require.Empty(t, code)
			require.NotNil(t, got)
			assert.Equal(t, encoded, got.ImageBase64)
		})
	}

	invalid := []struct {
		name   string
		width  int
		height int
	}{
		{"width below 240", 239, 240},
		{"height below 240", 240, 239},
		{"width above 8000", 8001, 1001},
		{"height above 8000", 1001, 8001},
		{"wider than 8 to 1", 1921, 240},
		{"taller than 1 to 8", 240, 1921},
	}
	for _, test := range invalid {
		t.Run("rejects "+test.name, func(t *testing.T) {
			encoded := base64.StdEncoding.EncodeToString(
				parameterMatrixImage(
					t,
					test.width,
					test.height,
					"png",
				),
			)
			parameterMatrixRequireJSONError(
				t,
				fmt.Sprintf(`{"prompt":"p","image":%q}`, encoded),
				"invalid_image",
			)
		})
	}
}

func TestSeedDanceParameterMatrixStrictImageBase64AndDataURI(t *testing.T) {
	pngBytes := parameterMatrixImage(t, 240, 240, "png")
	for len(pngBytes)%3 == 0 {
		pngBytes = append(pngBytes, 0)
	}
	pngBase64 := base64.StdEncoding.EncodeToString(pngBytes)
	jpegBytes := parameterMatrixImage(t, 240, 240, "jpeg")
	jpegBase64 := base64.StdEncoding.EncodeToString(jpegBytes)

	for _, test := range []struct {
		name string
		src  string
		want string
	}{
		{"canonical plain Base64", pngBase64, pngBase64},
		{
			"canonical PNG data URI",
			"data:image/png;base64," + pngBase64,
			pngBase64,
		},
		{
			"canonical JPEG data URI",
			"data:image/jpeg;base64," + jpegBase64,
			jpegBase64,
		},
		{
			"outer whitespace is trimmed",
			" \t\r\n" + pngBase64 + "\r\n ",
			pngBase64,
		},
	} {
		t.Run("accepts "+test.name, func(t *testing.T) {
			got, code := parameterMatrixNormalizeJSON(
				t,
				fmt.Sprintf(`{"prompt":"p","image":%q}`, test.src),
			)
			require.Empty(t, code)
			require.NotNil(t, got)
			assert.Equal(t, test.want, got.ImageBase64)
		})
	}

	paddingIndex := strings.IndexByte(pngBase64, '=')
	require.Positive(t, paddingIndex)
	invalidPadBits := []byte(pngBase64)
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	lastData := paddingIndex - 1
	value := strings.IndexByte(alphabet, invalidPadBits[lastData])
	require.NotEqual(t, -1, value)
	invalidPadBits[lastData] = alphabet[value|1]

	urlAlphabet := strings.NewReplacer("+", "-", "/", "_").Replace(pngBase64)
	if urlAlphabet == pngBase64 {
		urlAlphabet = "_" + pngBase64[1:]
	}
	insertWhitespace := func(whitespace string) string {
		return pngBase64[:len(pngBase64)/2] +
			whitespace +
			pngBase64[len(pngBase64)/2:]
	}

	for _, test := range []struct {
		name string
		src  string
	}{
		{"invalid alphabet", "%%%"},
		{"URL-safe alphabet", urlAlphabet},
		{"missing required padding", strings.TrimRight(pngBase64, "=")},
		{"extra padding", pngBase64 + "="},
		{"non-zero trailing padding bits", string(invalidPadBits)},
		{"embedded LF", insertWhitespace("\n")},
		{"embedded CR", insertWhitespace("\r")},
		{"embedded CRLF", insertWhitespace("\r\n")},
		{"embedded space", insertWhitespace(" ")},
		{"embedded tab", insertWhitespace("\t")},
		{
			"embedded LF in data URI",
			"data:image/png;base64," + insertWhitespace("\n"),
		},
		{
			"embedded CR in data URI",
			"data:image/png;base64," + insertWhitespace("\r"),
		},
		{
			"embedded CRLF in data URI",
			"data:image/png;base64," + insertWhitespace("\r\n"),
		},
		{
			"embedded space in data URI",
			"data:image/png;base64," + insertWhitespace(" "),
		},
		{
			"embedded tab in data URI",
			"data:image/png;base64," + insertWhitespace("\t"),
		},
		{
			"unsupported image jpg data URI",
			"data:image/jpg;base64," + jpegBase64,
		},
		{
			"uppercase data URI",
			"data:image/PNG;base64," + pngBase64,
		},
		{
			"data URI parameters",
			"data:image/png;charset=utf-8;base64," + pngBase64,
		},
		{
			"non-base64 data URI",
			"data:image/png," + pngBase64,
		},
		{
			"declared JPEG with PNG content",
			"data:image/jpeg;base64," + pngBase64,
		},
	} {
		t.Run("rejects "+test.name, func(t *testing.T) {
			parameterMatrixRequireJSONError(
				t,
				fmt.Sprintf(`{"prompt":"p","image":%q}`, test.src),
				"invalid_image",
			)
		})
	}
}

func TestSeedDanceParameterMatrixRemoteImageUsesValidatedContent(
	t *testing.T,
) {
	pngBytes := parameterMatrixImage(t, 240, 240, "png")
	encoded := base64.StdEncoding.EncodeToString(pngBytes)
	c := parameterMatrixJSONContext(
		t,
		`{"prompt":"p","image":"https://HOST/reference.png"}`,
	)
	calls := 0
	got, taskErr := normalizeRequestWithLoader(
		c,
		func(source string) (string, string, error) {
			calls++
			require.Equal(t, "https://HOST/reference.png", source)
			return "image/png", encoded, nil
		},
	)
	require.Nil(t, taskErr)
	require.NotNil(t, got)
	assert.Equal(t, encoded, got.ImageBase64)
	assert.Equal(t, 1, calls)
}

func TestSeedDanceParameterMatrixMultipartDuplicateAndUnknownFields(
	t *testing.T,
) {
	base := []parameterMatrixMultipartField{{name: "prompt", value: "p"}}
	for _, field := range []string{
		"model",
		"prompt",
		"duration",
		"seconds",
		"size",
		"image",
		"input_reference",
		"metadata",
	} {
		t.Run("rejects duplicate "+field, func(t *testing.T) {
			fields := append(
				append([]parameterMatrixMultipartField{}, base...),
				parameterMatrixMultipartField{name: field, value: "a"},
				parameterMatrixMultipartField{name: field, value: "b"},
			)
			if field == "prompt" {
				fields = []parameterMatrixMultipartField{
					{name: "prompt", value: "a"},
					{name: "prompt", value: "b"},
				}
			}
			parameterMatrixRequireMultipartError(
				t,
				fields,
				nil,
				"invalid_request",
			)
		})
	}

	t.Run("duplicate images values are an invalid image array", func(t *testing.T) {
		parameterMatrixRequireMultipartError(
			t,
			[]parameterMatrixMultipartField{
				{name: "prompt", value: "p"},
				{name: "images", value: "a"},
				{name: "images", value: "b"},
			},
			nil,
			"invalid_image",
		)
	})

	t.Run("unknown text fields are ignored", func(t *testing.T) {
		got, code := parameterMatrixNormalizeMultipart(
			t,
			[]parameterMatrixMultipartField{
				{name: "prompt", value: " p "},
				{name: "unknown", value: "DO_NOT_FORWARD"},
				{name: "unknown", value: "STILL_DO_NOT_FORWARD"},
			},
			nil,
		)
		require.Empty(t, code)
		require.NotNil(t, got)
		assert.Equal(t, "p", got.Prompt)
	})

	t.Run("unknown file field is rejected", func(t *testing.T) {
		parameterMatrixRequireMultipartError(
			t,
			base,
			[]parameterMatrixMultipartFile{{
				fieldName:   "unknown",
				fileName:    "reference.png",
				contentType: "image/png",
				data:        parameterMatrixImage(t, 240, 240, "png"),
			}},
			"invalid_image",
		)
	})

	t.Run("duplicate input_reference files are rejected", func(t *testing.T) {
		data := parameterMatrixImage(t, 240, 240, "png")
		parameterMatrixRequireMultipartError(
			t,
			base,
			[]parameterMatrixMultipartFile{
				{
					fieldName:   "input_reference",
					fileName:    "a.png",
					contentType: "image/png",
					data:        data,
				},
				{
					fieldName:   "input_reference",
					fileName:    "b.png",
					contentType: "image/png",
					data:        data,
				},
			},
			"invalid_image",
		)
	})
}

func TestSeedDanceParameterMatrixMultipartBooleanTextContract(
	t *testing.T,
) {
	type boolPointer func(*NormalizedRequest) *bool
	fields := []struct {
		name string
		get  boolPointer
	}{
		{"prompt_optimization", func(got *NormalizedRequest) *bool {
			return got.PromptOptimization
		}},
		{"multi_shot", func(got *NormalizedRequest) *bool {
			return got.MultiShot
		}},
		{"strict_duration", func(got *NormalizedRequest) *bool {
			return got.StrictDuration
		}},
	}
	for _, field := range fields {
		for _, value := range []struct {
			name    string
			jsonRaw string
			wantNil bool
			want    bool
		}{
			{"null", `null`, true, false},
			{"text true", `"true"`, false, true},
			{"text false", `"false"`, false, false},
		} {
			t.Run(field.name+"/"+value.name, func(t *testing.T) {
				metadata, err := json.Marshal(map[string]json.RawMessage{
					field.name: json.RawMessage(value.jsonRaw),
				})
				require.NoError(t, err)
				got, code := parameterMatrixNormalizeMultipart(
					t,
					[]parameterMatrixMultipartField{
						{name: "prompt", value: "p"},
						{name: "metadata", value: string(metadata)},
					},
					nil,
				)
				require.Empty(t, code)
				require.NotNil(t, got)
				if value.wantNil {
					assert.Nil(t, field.get(got))
					return
				}
				require.NotNil(t, field.get(got))
				assert.Equal(t, value.want, *field.get(got))
			})
		}

		for _, wrong := range []struct {
			name string
			raw  string
		}{
			{"JSON true", `true`},
			{"JSON false", `false`},
			{"number", `1`},
			{"invalid text", `"yes"`},
			{"array", `[]`},
			{"object", `{}`},
		} {
			t.Run(field.name+"/rejects "+wrong.name, func(t *testing.T) {
				parameterMatrixRequireMultipartError(
					t,
					[]parameterMatrixMultipartField{
						{name: "prompt", value: "p"},
						{
							name: "metadata",
							value: fmt.Sprintf(
								`{%q:%s}`,
								field.name,
								wrong.raw,
							),
						},
					},
					nil,
					"invalid_request",
				)
			})
		}
	}
}

func TestSeedDanceParameterMatrixMultipartUsesActualFileContent(
	t *testing.T,
) {
	pngBytes := parameterMatrixImage(t, 240, 240, "png")
	jpegBytes := parameterMatrixImage(t, 240, 240, "jpeg")
	gifBytes := parameterMatrixImage(t, 240, 240, "gif")
	for _, test := range []struct {
		name         string
		fileName     string
		declaredMIME string
		data         []byte
		want         []byte
	}{
		{
			"PNG declared as PNG",
			"reference.png",
			"image/png",
			pngBytes,
			pngBytes,
		},
		{
			"PNG declared as octet stream",
			"reference.bin",
			"application/octet-stream",
			pngBytes,
			pngBytes,
		},
		{
			"JPEG declared as PNG",
			"reference.png",
			"image/png",
			jpegBytes,
			jpegBytes,
		},
	} {
		t.Run("accepts actual "+test.name, func(t *testing.T) {
			got, code := parameterMatrixNormalizeMultipart(
				t,
				[]parameterMatrixMultipartField{{
					name:  "prompt",
					value: "p",
				}},
				[]parameterMatrixMultipartFile{{
					fieldName:   "input_reference",
					fileName:    test.fileName,
					contentType: test.declaredMIME,
					data:        test.data,
				}},
			)
			require.Empty(t, code)
			require.NotNil(t, got)
			assert.Equal(
				t,
				base64.StdEncoding.EncodeToString(test.want),
				got.ImageBase64,
			)
		})
	}

	for _, test := range []struct {
		name         string
		declaredMIME string
		data         []byte
	}{
		{"GIF declared as PNG", "image/png", gifBytes},
		{"text declared as PNG", "image/png", []byte("not an image")},
		{"empty PNG", "image/png", nil},
	} {
		t.Run("rejects actual "+test.name, func(t *testing.T) {
			parameterMatrixRequireMultipartError(
				t,
				[]parameterMatrixMultipartField{{
					name:  "prompt",
					value: "p",
				}},
				[]parameterMatrixMultipartFile{{
					fieldName:   "input_reference",
					fileName:    "reference.png",
					contentType: test.declaredMIME,
					data:        test.data,
				}},
				"invalid_image",
			)
		})
	}
}
