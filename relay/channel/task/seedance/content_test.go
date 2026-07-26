package seedance

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var _ channel.VideoContentFetcher = (*TaskAdaptor)(nil)

var errContentTrackingReader = errors.New("content tracking reader stopped")

type contentTrackingReadSeeker struct {
	reader    *bytes.Reader
	failAfter int64
	read      int64
	err       error
}

func (r *contentTrackingReadSeeker) Read(p []byte) (int, error) {
	if r.read >= r.failAfter {
		return 0, r.err
	}
	remaining := r.failAfter - r.read
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.reader.Read(p)
	r.read += int64(n)
	if n > 0 {
		return n, nil
	}
	return n, err
}

func (r *contentTrackingReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return r.reader.Seek(offset, whence)
}

func useContentTempDir(t *testing.T) string {
	t.Helper()
	previous := contentTempDir
	directory := t.TempDir()
	contentTempDir = directory
	t.Cleanup(func() {
		contentTempDir = previous
	})
	return directory
}

func requireContentTempDirEmpty(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Empty(t, entries, "temporary content directory must be empty")
}

func testMP4Bytes() []byte {
	return []byte{
		0x00, 0x00, 0x00, 0x08,
		'f', 't', 'y', 'p',
		0x00, 0x00, 0x00, 0x00,
	}
}

func videoSuccessJSON(encoded string) string {
	return `{"requestId":"REQUEST_ID","success":true,"video_base64":"` + encoded + `"}`
}

func assertVideoContentError(
	t *testing.T,
	err error,
	wantStatus int,
	wantType string,
	wantCode string,
) *channel.VideoContentError {
	t.Helper()
	require.Error(t, err)
	var contentErr *channel.VideoContentError
	require.ErrorAs(t, err, &contentErr)
	assert.Equal(t, wantStatus, contentErr.StatusCode)
	assert.Equal(t, wantType, contentErr.Type)
	assert.Equal(t, wantCode, contentErr.Code)
	assert.NotEmpty(t, contentErr.Message)
	assert.NotContains(t, contentErr.Error(), "TEST_KEY")
	assert.NotContains(t, contentErr.Error(), "PROVIDER_PRIVATE_DETAIL")
	return contentErr
}

func TestExtractVideoBase64JSON(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		encoded string
		wantErr bool
	}{
		{"plain", `{"requestId":"R","video_base64":"QUJDRA=="}`, "QUJDRA==", false},
		{"escaped slash", `{"video_base64":"QUJD\/RA=="}`, "QUJD/RA==", false},
		{"unicode escape rejected in base64", `{"video_base64":"QUJD\u003d"}`, "", true},
		{"duplicate", `{"video_base64":"QQ==","video_base64":"Qg=="}`, "", true},
		{"non string", `{"video_base64":null}`, "", true},
		{"truncated", `{"video_base64":"QQ==`, "", true},
		{"trailing junk", `{"video_base64":"QQ=="}x`, "", true},
		{"missing", `{"success":true}`, "", true},
		{
			"nested video field is not extracted",
			`{"label":"\uD83D\uDE00","nested":{"video_base64":"NESTED"},"video_base64":"QQ=="}`,
			"QQ==",
			false,
		},
		{
			"isolated surrogate in unrelated string",
			`{"note":"\uD800","video_base64":"QQ=="}`,
			"",
			true,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var redacted bytes.Buffer
			var encoded bytes.Buffer
			err := extractVideoBase64JSON(
				strings.NewReader(test.input),
				&redacted,
				&encoded,
			)
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.encoded, encoded.String())
			var redactedObject map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(redacted.Bytes(), &redactedObject))
			assert.JSONEq(t, `"[redacted]"`, string(redactedObject["video_base64"]))
			assert.NotContains(t, redacted.String(), test.encoded)
		})
	}
}

func TestExtractVideoBase64JSONRejectsTrailingJunkBeforeMissingField(t *testing.T) {
	var redacted bytes.Buffer
	var encoded bytes.Buffer
	err := extractVideoBase64JSON(
		strings.NewReader(`{"success":true}x`),
		&redacted,
		&encoded,
	)
	require.ErrorIs(t, err, errInvalidVideoJSON)
	assert.NotErrorIs(t, err, errVideoBase64Missing)
}

func TestDecodeVideoBase64(t *testing.T) {
	validVideo := testMP4Bytes()
	validEncoded := base64.StdEncoding.EncodeToString(validVideo)
	cases := []struct {
		name      string
		input     string
		want      []byte
		wantError bool
	}{
		{"pure Base64 MP4", validEncoded, validVideo, false},
		{
			"data video mp4 prefix",
			videoDataPrefix + validEncoded,
			validVideo,
			false,
		},
		{
			"data video webm rejected",
			"data:video/webm;base64," + validEncoded,
			nil,
			true,
		},
		{"invalid alphabet in middle", "QUJD$A==", nil, true},
		{"invalid alphabet at tail", "QUJDRA=$", nil, true},
		{"whitespace inside Base64", "QUJD\nRA==", nil, true},
		{"non canonical padding", "AB==", nil, true},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var decoded bytes.Buffer
			written, err := decodeVideoBase64(strings.NewReader(test.input), &decoded)
			if test.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, int64(len(test.want)), written)
			assert.Equal(t, test.want, decoded.Bytes())
		})
	}

	t.Run("tracking reader failure is surfaced", func(t *testing.T) {
		reader := &contentTrackingReadSeeker{
			reader:    bytes.NewReader([]byte(validEncoded)),
			failAfter: 4,
			err:       errContentTrackingReader,
		}
		_, err := decodeVideoBase64(reader, io.Discard)
		require.ErrorIs(t, err, errContentTrackingReader)
	})
}

func TestValidateMP4(t *testing.T) {
	cases := []struct {
		name      string
		input     []byte
		wantError bool
	}{
		{"shorter than MP4 box", []byte{0, 0, 0, 8, 'f', 't', 'y'}, true},
		{"without ftyp at offset four", []byte{0, 0, 0, 8, 'm', 'd', 'a', 't', 0, 0, 0, 0}, true},
		{"box size smaller than eight", []byte{0, 0, 0, 7, 'f', 't', 'y', 'p', 0, 0, 0, 0}, true},
		{"valid ftyp", testMP4Bytes(), false},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			reader := bytes.NewReader(test.input)
			err := validateMP4(reader)
			if test.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			first, err := reader.ReadByte()
			require.NoError(t, err)
			assert.Equal(t, test.input[0], first)
		})
	}
}

func TestFetchVideoContent(t *testing.T) {
	validVideo := testMP4Bytes()
	validEncoded := base64.StdEncoding.EncodeToString(validVideo)

	t.Run("HTTP 200 success business shape", func(t *testing.T) {
		directory := useContentTempDir(t)
		requests := make(chan *http.Request, 1)
		upstream := httptest.NewServer(http.HandlerFunc(func(
			writer http.ResponseWriter,
			req *http.Request,
		) {
			requests <- req.Clone(req.Context())
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, videoSuccessJSON(validEncoded))
		}))
		defer upstream.Close()

		content, err := (&TaskAdaptor{}).FetchVideoContent(
			context.Background(),
			upstream.URL+"/",
			"TEST_KEY",
			"UPSTREAM_TASK_ID",
			"",
		)
		require.NoError(t, err)
		require.NotNil(t, content)
		require.Equal(t, "video/mp4", content.ContentType)
		assert.Equal(t, int64(len(validVideo)), content.ContentLength)

		request := <-requests
		assert.Equal(t, http.MethodGet, request.Method)
		assert.Equal(t, "/video/UPSTREAM_TASK_ID", request.URL.Path)
		assert.Equal(t, "Bearer TEST_KEY", request.Header.Get("Authorization"))

		entries, err := os.ReadDir(directory)
		require.NoError(t, err)
		require.Len(t, entries, 1, "only returned MP4 may remain before Body.Close")
		info, err := entries[0].Info()
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

		body, err := io.ReadAll(content.Body)
		require.NoError(t, err)
		assert.Equal(t, validVideo, body)
		require.NoError(t, content.Body.Close())
		require.NoError(t, content.Body.Close())
		requireContentTempDirEmpty(t, directory)
	})

	t.Run("HTTP 200 success false business error", func(t *testing.T) {
		directory := useContentTempDir(t)
		upstream := httptest.NewServer(http.HandlerFunc(func(
			writer http.ResponseWriter,
			_ *http.Request,
		) {
			_, _ = io.WriteString(
				writer,
				`{"success":false,"errMessage":"PROVIDER_PRIVATE_DETAIL","video_base64":"`+validEncoded+`"}`,
			)
		}))
		defer upstream.Close()

		content, err := (&TaskAdaptor{}).FetchVideoContent(
			context.Background(), upstream.URL, "TEST_KEY", "UPSTREAM_TASK_ID", "",
		)
		assert.Nil(t, content)
		contentErr := assertVideoContentError(
			t,
			err,
			http.StatusBadGateway,
			"invalid_upstream_response",
			"invalid_upstream_response",
		)
		require.Error(t, contentErr.Cause)
		requireContentTempDirEmpty(t, directory)
	})

	for _, statusCode := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(statusCode)+" maps to authentication error", func(t *testing.T) {
			directory := useContentTempDir(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(
				writer http.ResponseWriter,
				_ *http.Request,
			) {
				writer.WriteHeader(statusCode)
			}))
			defer upstream.Close()

			content, err := (&TaskAdaptor{}).FetchVideoContent(
				context.Background(), upstream.URL, "TEST_KEY", "UPSTREAM_TASK_ID", "",
			)
			assert.Nil(t, content)
			assertVideoContentError(
				t,
				err,
				http.StatusBadGateway,
				"upstream_error",
				"upstream_authentication_error",
			)
			requireContentTempDirEmpty(t, directory)
		})
	}

	t.Run("429 maps to rate limit error", func(t *testing.T) {
		directory := useContentTempDir(t)
		upstream := httptest.NewServer(http.HandlerFunc(func(
			writer http.ResponseWriter,
			_ *http.Request,
		) {
			writer.WriteHeader(http.StatusTooManyRequests)
		}))
		defer upstream.Close()

		content, err := (&TaskAdaptor{}).FetchVideoContent(
			context.Background(), upstream.URL, "TEST_KEY", "UPSTREAM_TASK_ID", "",
		)
		assert.Nil(t, content)
		assertVideoContentError(
			t,
			err,
			http.StatusTooManyRequests,
			"upstream_rate_limit_error",
			"upstream_rate_limit_error",
		)
		requireContentTempDirEmpty(t, directory)
	})

	t.Run("large declared Content-Length is not pre-rejected", func(t *testing.T) {
		directory := useContentTempDir(t)
		upstream := httptest.NewServer(http.HandlerFunc(func(
			writer http.ResponseWriter,
			_ *http.Request,
		) {
			writer.Header().Set("Content-Length", "1099511627776")
			writer.Header().Set("Transfer-Encoding", "chunked")
			_, _ = io.WriteString(writer, videoSuccessJSON(validEncoded))
		}))
		defer upstream.Close()

		content, err := (&TaskAdaptor{}).FetchVideoContent(
			context.Background(), upstream.URL, "TEST_KEY", "UPSTREAM_TASK_ID", "",
		)
		require.NoError(t, err)
		require.NotNil(t, content)
		assert.Equal(t, int64(len(validVideo)), content.ContentLength)
		require.NoError(t, content.Body.Close())
		requireContentTempDirEmpty(t, directory)
	})

	t.Run("JSON error leaves no temp files", func(t *testing.T) {
		directory := useContentTempDir(t)
		upstream := httptest.NewServer(http.HandlerFunc(func(
			writer http.ResponseWriter,
			_ *http.Request,
		) {
			_, _ = io.WriteString(writer, `{"success":true,"video_base64":`)
		}))
		defer upstream.Close()

		content, err := (&TaskAdaptor{}).FetchVideoContent(
			context.Background(), upstream.URL, "TEST_KEY", "UPSTREAM_TASK_ID", "",
		)
		assert.Nil(t, content)
		assertVideoContentError(
			t,
			err,
			http.StatusBadGateway,
			"invalid_upstream_response",
			"invalid_upstream_response",
		)
		requireContentTempDirEmpty(t, directory)
	})

	t.Run("Base64 error leaves no temp files", func(t *testing.T) {
		directory := useContentTempDir(t)
		upstream := httptest.NewServer(http.HandlerFunc(func(
			writer http.ResponseWriter,
			_ *http.Request,
		) {
			_, _ = io.WriteString(writer, videoSuccessJSON("QUJD$A=="))
		}))
		defer upstream.Close()

		content, err := (&TaskAdaptor{}).FetchVideoContent(
			context.Background(), upstream.URL, "TEST_KEY", "UPSTREAM_TASK_ID", "",
		)
		assert.Nil(t, content)
		assertVideoContentError(
			t,
			err,
			http.StatusBadGateway,
			"invalid_upstream_response",
			"invalid_upstream_response",
		)
		requireContentTempDirEmpty(t, directory)
	})

	t.Run("MP4 error leaves no temp files", func(t *testing.T) {
		directory := useContentTempDir(t)
		invalidMP4 := base64.StdEncoding.EncodeToString([]byte("not-an-mp4"))
		upstream := httptest.NewServer(http.HandlerFunc(func(
			writer http.ResponseWriter,
			_ *http.Request,
		) {
			_, _ = io.WriteString(writer, videoSuccessJSON(invalidMP4))
		}))
		defer upstream.Close()

		content, err := (&TaskAdaptor{}).FetchVideoContent(
			context.Background(), upstream.URL, "TEST_KEY", "UPSTREAM_TASK_ID", "",
		)
		assert.Nil(t, content)
		assertVideoContentError(
			t,
			err,
			http.StatusBadGateway,
			"invalid_upstream_response",
			"invalid_upstream_response",
		)
		requireContentTempDirEmpty(t, directory)
	})

	t.Run("download timeout maps to 504 and leaves no temp files", func(t *testing.T) {
		directory := useContentTempDir(t)
		started := make(chan struct{})
		disconnected := make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(
			writer http.ResponseWriter,
			req *http.Request,
		) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusOK)
			writer.(http.Flusher).Flush()
			close(started)
			<-req.Context().Done()
			close(disconnected)
		}))
		defer upstream.Close()

		parent, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := (&TaskAdaptor{}).FetchVideoContent(
				parent, upstream.URL, "TEST_KEY", "UPSTREAM_TASK_ID", "",
			)
			result <- err
		}()
		<-started
		err := <-result
		assertVideoContentError(
			t,
			err,
			http.StatusGatewayTimeout,
			"upstream_timeout_error",
			"upstream_timeout_error",
		)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		select {
		case <-disconnected:
		case <-time.After(time.Second):
			t.Fatal("timeout did not close upstream video response")
		}
		requireContentTempDirEmpty(t, directory)
	})

	t.Run("parent cancellation stops upstream read and leaves no temp files", func(t *testing.T) {
		directory := useContentTempDir(t)
		started := make(chan struct{})
		disconnected := make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(
			writer http.ResponseWriter,
			req *http.Request,
		) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusOK)
			writer.(http.Flusher).Flush()
			close(started)
			<-req.Context().Done()
			close(disconnected)
		}))
		defer upstream.Close()

		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := (&TaskAdaptor{}).FetchVideoContent(
				parent, upstream.URL, "TEST_KEY", "UPSTREAM_TASK_ID", "",
			)
			result <- err
		}()
		<-started
		cancel()
		err := <-result
		assertVideoContentError(
			t,
			err,
			http.StatusBadGateway,
			"upstream_error",
			"upstream_connection_error",
		)
		require.ErrorIs(t, err, context.Canceled)
		select {
		case <-disconnected:
		case <-time.After(time.Second):
			t.Fatal("parent cancellation did not close upstream video response")
		}
		requireContentTempDirEmpty(t, directory)
	})
}
