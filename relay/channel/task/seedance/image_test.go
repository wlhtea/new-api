package seedance

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testImage(t *testing.T, width, height int, format string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8((x + y) % 256),
				G: uint8((2*x + y) % 256),
				B: uint8((x + 2*y) % 256),
				A: 255,
			})
		}
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
		t.Fatalf("unsupported test image format %q", format)
	}
	require.NoError(t, err)
	return encoded.Bytes()
}

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	return testImage(t, width, height, "png")
}

func testNoisyPNG(t *testing.T, dimension int, seed int64) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, dimension, dimension))
	random := rand.New(rand.NewSource(seed))
	_, err := random.Read(img.Pix)
	require.NoError(t, err)
	for index := 3; index < len(img.Pix); index += 4 {
		img.Pix[index] = 255
	}

	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, img))
	return data.Bytes()
}

func TestNormalizeImagesDeduplicatesDecodedBytes(t *testing.T) {
	pngBytes := testPNG(t, 240, 240)
	encoded := base64.StdEncoding.EncodeToString(pngBytes)
	body := fmt.Sprintf(`{
      "prompt":"p",
      "image":%q,
      "input_reference":%q,
      "metadata":{"image_base64":%q}
    }`, encoded, "data:image/png;base64,"+encoded, encoded)

	got, taskErr := normalizeRequest(jsonContext(t, body))
	require.Nil(t, taskErr)
	assert.Equal(t, encoded, got.ImageBase64)
}

func TestNormalizeImagesRejectsDifferentDecodedBytes(t *testing.T) {
	first := base64.StdEncoding.EncodeToString(testPNG(t, 240, 240))
	second := base64.StdEncoding.EncodeToString(testPNG(t, 241, 240))
	body := fmt.Sprintf(`{"prompt":"p","image":%q,"images":[%q]}`, first, second)

	_, taskErr := normalizeRequest(jsonContext(t, body))
	require.NotNil(t, taskErr)
	assert.Equal(t, "invalid_image", taskErr.Code)
}

func TestNormalizeImagesRejectsT2V480P(t *testing.T) {
	_, taskErr := normalizeRequest(jsonContext(t,
		`{"prompt":"p","size":"854x480"}`,
	))
	require.NotNil(t, taskErr)
	assert.Equal(t, "invalid_resolution", taskErr.Code)
}

func TestNormalizeImagesValidation(t *testing.T) {
	validJPEG := base64.StdEncoding.EncodeToString(testImage(t, 240, 240, "jpeg"))
	validPNG := base64.StdEncoding.EncodeToString(testPNG(t, 240, 240))
	gifData := base64.StdEncoding.EncodeToString(testImage(t, 240, 240, "gif"))
	webpData := base64.StdEncoding.EncodeToString([]byte(
		"RIFF\x16\x00\x00\x00WEBPVP8 \x0a\x00\x00\x00\x00\x00\x00\x00\x00\x00",
	))

	tests := []struct {
		name   string
		image  string
		size   string
		wantOK bool
	}{
		{"jpeg succeeds", validJPEG, "", true},
		{"png succeeds", validPNG, "", true},
		{"gif fails", gifData, "", false},
		{"webp fails", webpData, "", false},
		{"too narrow dimension", base64.StdEncoding.EncodeToString(testPNG(t, 239, 240)), "", false},
		{"too wide dimension", base64.StdEncoding.EncodeToString(testPNG(t, 8001, 240)), "", false},
		{"ratio wider than eight", base64.StdEncoding.EncodeToString(testPNG(t, 1921, 240)), "", false},
		{"ratio taller than eight", base64.StdEncoding.EncodeToString(testPNG(t, 240, 1921)), "", false},
		{"invalid base64", "%%%", "", false},
		{"unsupported data URI MIME", "data:image/gif;base64," + gifData, "", false},
		{"i2v 480p succeeds", validPNG, "854x480", true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sizeField := ""
			if test.size != "" {
				sizeField = fmt.Sprintf(`,"size":%q`, test.size)
			}
			body := fmt.Sprintf(`{"prompt":"p","image":%q%s}`, test.image, sizeField)
			got, taskErr := normalizeRequest(jsonContext(t, body))
			if !test.wantOK {
				require.NotNil(t, taskErr)
				assert.Equal(t, "invalid_image", taskErr.Code)
				return
			}
			require.Nil(t, taskErr)
			assert.NotEmpty(t, got.ImageBase64)
		})
	}
}

func TestNormalizeImagesRejectsArrayWithMoreThanOneItem(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(testPNG(t, 240, 240))
	body := fmt.Sprintf(`{"prompt":"p","images":[%q,%q]}`, encoded, encoded)

	_, taskErr := normalizeRequest(jsonContext(t, body))
	require.NotNil(t, taskErr)
	assert.Equal(t, "invalid_image", taskErr.Code)
}

func TestNormalizeRemoteImageDownloadsOnceAndCachesResult(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(testPNG(t, 240, 240))
	c := jsonContext(t, `{"prompt":"p","image":"https://HOST/image.png"}`)
	calls := 0
	loader := func(url string) (string, string, error) {
		calls++
		assert.Equal(t, "https://HOST/image.png", url)
		return "image/png", encoded, nil
	}

	first, taskErr := normalizeRequestWithLoader(c, loader)
	require.Nil(t, taskErr)
	second, taskErr := normalizeRequestWithLoader(c, loader)
	require.Nil(t, taskErr)
	assert.Same(t, first, second)
	assert.Equal(t, 1, calls)
	assert.Equal(t, encoded, first.ImageBase64)
}

func TestNormalizeRemoteImageRejectsMIMEContentMismatch(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(testPNG(t, 240, 240))
	loader := func(string) (string, string, error) {
		return "image/jpeg", encoded, nil
	}

	_, taskErr := normalizeRequestWithLoader(
		jsonContext(t, `{"prompt":"p","image":"https://HOST/image"}`),
		loader,
	)
	require.NotNil(t, taskErr)
	assert.Equal(t, "invalid_image", taskErr.Code)
}

func TestImageOverSupplierRecommendation(t *testing.T) {
	data := testNoisyPNG(t, 3000, 1)
	require.Greater(t, len(data), 5*1024*1024)
	encoded := base64.StdEncoding.EncodeToString(data)
	body := fmt.Sprintf(`{"prompt":"p","image":%q}`, encoded)

	got, taskErr := normalizeRequest(jsonContext(t, body))
	require.Nil(t, taskErr)
	assert.True(t, strings.HasPrefix(got.ImageBase64, "iVBOR"))
	assert.Equal(t, encoded, got.ImageBase64)
}
