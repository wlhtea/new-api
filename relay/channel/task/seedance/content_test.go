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
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var _ channel.VideoContentFetcher = (*TaskAdaptor)(nil)

var errContentTrackingReader = errors.New("content tracking reader stopped")
var errContentTestWriter = errors.New("content test writer stopped")

type failAfterWriter struct {
	remaining int
	err       error
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, w.err
	}
	if len(p) > w.remaining {
		written := w.remaining
		w.remaining = 0
		return written, w.err
	}
	w.remaining -= len(p)
	return len(p), nil
}

type dataAndErrorReader struct {
	data []byte
	err  error
	done bool
}

func (r *dataAndErrorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, r.data), r.err
}

type contentTestReadCloser struct {
	reader     io.Reader
	closeErr   error
	closeCalls int
}

func (r *contentTestReadCloser) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *contentTestReadCloser) Close() error {
	r.closeCalls++
	return r.closeErr
}

type controlledBase64ReadSeeker struct {
	reader           *bytes.Reader
	maxChunk         int
	failAfterDecode  int64
	decodeBytesRead  int64
	decodeMode       bool
	zeroSeeks        int
	pendingErr       error
	maxRequested     int
	totalDecodeReads int
}

func newControlledBase64ReadSeeker(
	input string,
	maxChunk int,
	failAfterDecode int64,
	pendingErr error,
) *controlledBase64ReadSeeker {
	return &controlledBase64ReadSeeker{
		reader:          bytes.NewReader([]byte(input)),
		maxChunk:        maxChunk,
		failAfterDecode: failAfterDecode,
		pendingErr:      pendingErr,
	}
}

func (r *controlledBase64ReadSeeker) Read(p []byte) (int, error) {
	if len(p) > r.maxRequested {
		r.maxRequested = len(p)
	}
	if r.decodeMode {
		r.totalDecodeReads++
	}
	if r.maxChunk > 0 && len(p) > r.maxChunk {
		p = p[:r.maxChunk]
	}
	if r.decodeMode && r.pendingErr != nil && r.failAfterDecode >= 0 {
		remaining := r.failAfterDecode - r.decodeBytesRead
		if remaining <= 0 {
			err := r.pendingErr
			r.pendingErr = nil
			return 0, err
		}
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
	}
	n, err := r.reader.Read(p)
	if r.decodeMode {
		r.decodeBytesRead += int64(n)
		if r.pendingErr != nil &&
			r.failAfterDecode >= 0 &&
			r.decodeBytesRead >= r.failAfterDecode {
			deferred := r.pendingErr
			r.pendingErr = nil
			return n, deferred
		}
	}
	return n, err
}

func (r *controlledBase64ReadSeeker) Seek(offset int64, whence int) (int64, error) {
	position, err := r.reader.Seek(offset, whence)
	if err == nil && offset == 0 && whence == io.SeekStart && position == 0 {
		r.zeroSeeks++
		if r.zeroSeeks >= 2 {
			r.decodeMode = true
			r.decodeBytesRead = 0
		}
	}
	return position, err
}

type lazyNestedJSONReader struct {
	prefixOffset int
	opensLeft    int64
	middleDone   bool
	closesLeft   int64
	suffixOffset int
	totalRead    int64
	maxRead      int
}

func newLazyNestedJSONReader(depth int64) *lazyNestedJSONReader {
	return &lazyNestedJSONReader{
		opensLeft:  depth,
		closesLeft: depth,
	}
}

func (r *lazyNestedJSONReader) Read(p []byte) (int, error) {
	const prefix = `{"nested":`
	const suffix = `,"video_base64":"QQ=="}`
	requested := len(p)
	written := 0
	for written < len(p) {
		switch {
		case r.prefixOffset < len(prefix):
			count := copy(p[written:], prefix[r.prefixOffset:])
			r.prefixOffset += count
			written += count
		case r.opensLeft > 0:
			p[written] = '['
			r.opensLeft--
			written++
		case !r.middleDone:
			p[written] = '0'
			r.middleDone = true
			written++
		case r.closesLeft > 0:
			p[written] = ']'
			r.closesLeft--
			written++
		case r.suffixOffset < len(suffix):
			count := copy(p[written:], suffix[r.suffixOffset:])
			r.suffixOffset += count
			written += count
		default:
			r.totalRead += int64(written)
			if requested > r.maxRead {
				r.maxRead = requested
			}
			if written == 0 {
				return 0, io.EOF
			}
			return written, nil
		}
	}
	r.totalRead += int64(written)
	if requested > r.maxRead {
		r.maxRead = requested
	}
	return written, nil
}

type lazyBusinessJSONReader struct {
	prefixOffset int
	dataLeft     int64
	suffixOffset int
	totalRead    int64
	maxRead      int
}

func newLazyBusinessJSONReader(dataBytes int64) *lazyBusinessJSONReader {
	return &lazyBusinessJSONReader{dataLeft: dataBytes}
}

func (r *lazyBusinessJSONReader) Read(p []byte) (int, error) {
	const prefix = `{"success":true,"data":"`
	const suffix = `","requestId":"IGNORED","message":"IGNORED","video_base64":"QQ=="}`
	requested := len(p)
	written := 0
	for written < len(p) {
		switch {
		case r.prefixOffset < len(prefix):
			count := copy(p[written:], prefix[r.prefixOffset:])
			r.prefixOffset += count
			written += count
		case r.dataLeft > 0:
			count := len(p) - written
			if int64(count) > r.dataLeft {
				count = int(r.dataLeft)
			}
			for index := 0; index < count; index++ {
				p[written+index] = 'A'
			}
			r.dataLeft -= int64(count)
			written += count
		case r.suffixOffset < len(suffix):
			count := copy(p[written:], suffix[r.suffixOffset:])
			r.suffixOffset += count
			written += count
		default:
			r.totalRead += int64(written)
			if requested > r.maxRead {
				r.maxRead = requested
			}
			if written == 0 {
				return 0, io.EOF
			}
			return written, nil
		}
	}
	r.totalRead += int64(written)
	if requested > r.maxRead {
		r.maxRead = requested
	}
	return written, nil
}

func useContentTempDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func contentTestDependencies(directory string) contentFetchDependencies {
	return contentFetchDependencies{
		newTempFile: func(label string) (*contentTempFile, error) {
			return newContentTempFileIn(directory, label)
		},
	}
}

type recordedContentTemp struct {
	label  string
	path   string
	owner  *contentTempFile
	handle *os.File
}

type contentTempRecorder struct {
	directory              string
	failLabel              string
	failErr                error
	closeBeforeReturnLabel string
	temps                  []*recordedContentTemp
}

func newContentTempRecorder(directory string) *contentTempRecorder {
	return &contentTempRecorder{directory: directory}
}

func (r *contentTempRecorder) allocator(
	t *testing.T,
) func(string) (*contentTempFile, error) {
	t.Helper()
	return func(label string) (*contentTempFile, error) {
		for _, existing := range r.temps {
			requireContentHandleOpen(t, existing.handle)
			_, err := os.Stat(existing.path)
			require.NoErrorf(t, err, "%s path must still exist", existing.label)
		}
		if label == r.failLabel {
			require.Error(t, r.failErr)
			return nil, r.failErr
		}

		temp, err := newContentTempFileIn(r.directory, label)
		require.NoError(t, err)
		info, err := temp.file.Stat()
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		recorded := &recordedContentTemp{
			label:  label,
			path:   temp.path,
			owner:  temp,
			handle: temp.file,
		}
		r.temps = append(r.temps, recorded)
		if label == r.closeBeforeReturnLabel {
			require.NoError(t, recorded.handle.Close())
		}
		return temp, nil
	}
}

func (r *contentTempRecorder) requireTemp(
	t *testing.T,
	label string,
) *recordedContentTemp {
	t.Helper()
	for _, temp := range r.temps {
		if temp.label == label {
			return temp
		}
	}
	t.Fatalf("temporary file %q was not recorded", label)
	return nil
}

func (r *contentTempRecorder) requireAllCleaned(t *testing.T) {
	t.Helper()
	for _, temp := range r.temps {
		requireRecordedContentTempCleaned(t, temp)
	}
}

func requireContentHandleOpen(t *testing.T, handle *os.File) {
	t.Helper()
	require.NotNil(t, handle)
	_, err := handle.Stat()
	require.NoError(t, err)
}

func requireContentHandleClosed(t *testing.T, handle *os.File) {
	t.Helper()
	require.NotNil(t, handle)
	_, err := handle.Stat()
	require.Error(t, err)
	require.ErrorIs(t, err, os.ErrClosed)
	var pathErr *os.PathError
	require.ErrorAs(t, err, &pathErr)
	assert.Equal(t, "stat", pathErr.Op)
}

func requireRecordedContentTempCleaned(
	t *testing.T,
	temp *recordedContentTemp,
) {
	t.Helper()
	requireContentHandleClosed(t, temp.handle)
	assert.Nil(t, temp.owner.file)
	assert.Empty(t, temp.owner.path)
	_, err := os.Stat(temp.path)
	require.Error(t, err)
	require.ErrorIs(t, err, os.ErrNotExist)
	var pathErr *os.PathError
	require.ErrorAs(t, err, &pathErr)
	assert.Equal(t, "stat", pathErr.Op)
}

type contentTestFetcher struct {
	adaptor *TaskAdaptor
	deps    contentFetchDependencies
}

func newContentTestFetcher(directory string) *contentTestFetcher {
	return &contentTestFetcher{
		adaptor: &TaskAdaptor{},
		deps:    contentTestDependencies(directory),
	}
}

func (f *contentTestFetcher) FetchVideoContent(
	parent context.Context,
	baseURL string,
	key string,
	upstreamTaskID string,
	proxy string,
) (*channel.VideoContent, error) {
	return f.adaptor.fetchVideoContentWithDependencies(
		parent,
		baseURL,
		key,
		upstreamTaskID,
		proxy,
		f.deps,
	)
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
	expectedMessages := map[string]string{
		"upstream_authentication_error": contentAuthenticationMessage,
		"upstream_rate_limit_error":     contentRateLimitMessage,
		"upstream_timeout_error":        contentTimeoutMessage,
		"upstream_connection_error":     contentConnectionMessage,
		"invalid_upstream_response":     contentInvalidMessage,
	}
	assert.Equal(t, expectedMessages[wantCode], contentErr.Message)
	require.Error(t, contentErr.Cause)
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
			require.NoError(t, common.DecodeJson(bytes.NewReader(redacted.Bytes()), &redactedObject))
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

func TestExtractVideoBase64JSONGrammarMatrix(t *testing.T) {
	validCases := []struct {
		name  string
		input []byte
	}{
		{
			name:  "nested empty objects and arrays",
			input: []byte(`{"nested":{"object":{},"array":[[],{}]},"video_base64":"QQ=="}`),
		},
		{
			name:  "complete literals",
			input: []byte(`{"t":true,"f":false,"n":null,"video_base64":"QQ=="}`),
		},
		{
			name:  "valid JSON numbers",
			input: []byte(`{"numbers":[0,-0,17,-42,1.25,-2.5,1e3,2E+4,-3.5e-2],"video_base64":"QQ=="}`),
		},
		{
			name:  "escaped target key",
			input: []byte(`{"video\u005fbase64":"QQ=="}`),
		},
		{
			name:  "valid surrogate pair",
			input: []byte(`{"note":"\uD83D\uDE00","video_base64":"QQ=="}`),
		},
	}

	for _, test := range validCases {
		t.Run(test.name, func(t *testing.T) {
			var redacted bytes.Buffer
			var encoded bytes.Buffer
			require.NoError(t, extractVideoBase64JSON(
				bytes.NewReader(test.input),
				&redacted,
				&encoded,
			))
			assert.Equal(t, "QQ==", encoded.String())

			var oracle map[string]json.RawMessage
			require.NoError(
				t,
				common.DecodeJson(bytes.NewReader(redacted.Bytes()), &oracle),
			)
			assert.JSONEq(t, `"[redacted]"`, string(oracle["video_base64"]))
		})
	}

	invalidCases := []struct {
		name    string
		input   []byte
		wantErr error
	}{
		{"root string", []byte(`"QQ=="`), errInvalidVideoJSON},
		{"root array", []byte(`["QQ=="]`), errInvalidVideoJSON},
		{"empty root object", []byte(`{}`), errVideoBase64Missing},
		{"mismatched brackets", []byte(`{"nested":[},"video_base64":"QQ=="}`), errInvalidVideoJSON},
		{"leading object comma", []byte(`{,"video_base64":"QQ=="}`), errInvalidVideoJSON},
		{"trailing object comma", []byte(`{"video_base64":"QQ==",}`), errInvalidVideoJSON},
		{"leading array comma", []byte(`{"nested":[,0],"video_base64":"QQ=="}`), errInvalidVideoJSON},
		{"trailing array comma", []byte(`{"nested":[0,],"video_base64":"QQ=="}`), errInvalidVideoJSON},
		{"incomplete true literal", []byte(`{"value":tru,"video_base64":"QQ=="}`), errInvalidVideoJSON},
		{"literal without delimiter", []byte(`{"value":truex,"video_base64":"QQ=="}`), errInvalidVideoJSON},
		{"leading zero", []byte(`{"value":01,"video_base64":"QQ=="}`), errInvalidVideoJSON},
		{"dangling decimal point", []byte(`{"value":1.,"video_base64":"QQ=="}`), errInvalidVideoJSON},
		{"dangling exponent", []byte(`{"value":1e,"video_base64":"QQ=="}`), errInvalidVideoJSON},
		{"dangling exponent sign", []byte(`{"value":1e+,"video_base64":"QQ=="}`), errInvalidVideoJSON},
		{"dangling number sign", []byte(`{"value":-,"video_base64":"QQ=="}`), errInvalidVideoJSON},
		{
			"escaped duplicate target key",
			[]byte(`{"video_base64":"QQ==","video\u005fbase64":"Qg=="}`),
			errDuplicateVideoBase64,
		},
		{
			"isolated low surrogate",
			[]byte(`{"note":"\uDC00","video_base64":"QQ=="}`),
			errInvalidJSONStringEscape,
		},
		{
			"high surrogate followed by non surrogate",
			[]byte(`{"note":"\uD800\u0041","video_base64":"QQ=="}`),
			errInvalidJSONStringEscape,
		},
		{
			"invalid raw UTF-8",
			append(
				append([]byte(`{"note":"`), byte(0xff)),
				[]byte(`","video_base64":"QQ=="}`)...,
			),
			errInvalidVideoJSON,
		},
	}

	for _, test := range invalidCases {
		t.Run(test.name, func(t *testing.T) {
			err := extractVideoBase64JSON(
				bytes.NewReader(test.input),
				io.Discard,
				io.Discard,
			)
			require.ErrorIs(t, err, test.wantErr)
		})
	}
}

func TestExtractVideoBase64JSONStreamingIOErrors(t *testing.T) {
	const validJSON = `{"success":true,"video_base64":"QUJDRA=="}`

	t.Run("source data and error are both observed", func(t *testing.T) {
		sourceErr := errors.New("JSON source stopped")
		err := extractVideoBase64JSON(
			&dataAndErrorReader{data: []byte(validJSON), err: sourceErr},
			io.Discard,
			io.Discard,
		)
		require.ErrorIs(t, err, sourceErr)
	})

	t.Run("redacted writer error is surfaced", func(t *testing.T) {
		writerErr := errors.New("redacted writer stopped")
		err := extractVideoBase64JSON(
			strings.NewReader(validJSON),
			&failAfterWriter{remaining: 8, err: writerErr},
			io.Discard,
		)
		require.ErrorIs(t, err, writerErr)
	})

	t.Run("encoded writer error is surfaced", func(t *testing.T) {
		writerErr := errors.New("encoded writer stopped")
		err := extractVideoBase64JSON(
			strings.NewReader(validJSON),
			io.Discard,
			&failAfterWriter{remaining: 3, err: writerErr},
		)
		require.ErrorIs(t, err, writerErr)
	})
}

func TestExtractVideoBase64JSONBoundsNestingDepth(t *testing.T) {
	const declaredDepth = int64(1_000_000_000)
	reader := newLazyNestedJSONReader(declaredDepth)

	err := extractVideoBase64JSON(reader, io.Discard, io.Discard)

	require.ErrorIs(t, err, errJSONNestingTooDeep)
	assert.Less(t, reader.totalRead, int64(128*1024))
	assert.LessOrEqual(t, reader.maxRead, 32*1024)
	assert.Greater(t, reader.opensLeft, declaredDepth/2)
}

func TestExtractVideoBase64JSONBusinessState(t *testing.T) {
	cases := []struct {
		name            string
		input           string
		wantExtractErr  error
		wantBusinessErr error
	}{
		{
			name:  "success true with absent optional fields",
			input: `{"success":true,"video_base64":"QQ=="}`,
		},
		{
			name:            "success false",
			input:           `{"success":false,"video_base64":"QQ=="}`,
			wantBusinessErr: errContentBusinessFailure,
		},
		{
			name:            "failed status",
			input:           `{"success":true,"status":"FaIlEd","video_base64":"QQ=="}`,
			wantBusinessErr: errContentBusinessFailure,
		},
		{
			name:            "nonzero string error code",
			input:           `{"success":true,"errCode":"RATE_LIMIT","video_base64":"QQ=="}`,
			wantBusinessErr: errContentBusinessFailure,
		},
		{
			name:            "nonzero numeric error code",
			input:           `{"success":true,"errCode":401,"video_base64":"QQ=="}`,
			wantBusinessErr: errContentBusinessFailure,
		},
		{
			name:  "numeric zero error code",
			input: `{"success":true,"errCode":0,"video_base64":"QQ=="}`,
		},
		{
			name:  "string zero error code",
			input: `{"success":true,"errCode":" -0.00e+12 ","video_base64":"QQ=="}`,
		},
		{
			name:  "empty string error code",
			input: `{"success":true,"errCode":"  ","video_base64":"QQ=="}`,
		},
		{
			name:            "missing success",
			input:           `{"status":"completed","video_base64":"QQ=="}`,
			wantBusinessErr: errInvalidContentBusinessEnvelope,
		},
		{
			name:           "success has wrong type",
			input:          `{"success":"true","video_base64":"QQ=="}`,
			wantExtractErr: errInvalidContentBusinessEnvelope,
		},
		{
			name:           "status has wrong type",
			input:          `{"success":true,"status":0,"video_base64":"QQ=="}`,
			wantExtractErr: errInvalidContentBusinessEnvelope,
		},
		{
			name:           "error code object is malformed",
			input:          `{"success":true,"errCode":{},"video_base64":"QQ=="}`,
			wantExtractErr: errInvalidContentBusinessEnvelope,
		},
		{
			name:           "error code null is malformed",
			input:          `{"success":true,"errCode":null,"video_base64":"QQ=="}`,
			wantExtractErr: errInvalidContentBusinessEnvelope,
		},
		{
			name:           "duplicate success is ambiguous",
			input:          `{"success":true,"success":false,"video_base64":"QQ=="}`,
			wantExtractErr: errInvalidContentBusinessEnvelope,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var redacted bytes.Buffer
			var encoded bytes.Buffer
			business, err := extractVideoBase64JSONWithBusiness(
				strings.NewReader(test.input),
				&redacted,
				&encoded,
			)
			if test.wantExtractErr != nil {
				require.ErrorIs(t, err, test.wantExtractErr)
				return
			}
			require.NoError(t, err)
			err = validateContentBusinessState(business)
			if test.wantBusinessErr != nil {
				require.ErrorIs(t, err, test.wantBusinessErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestExtractVideoBase64JSONBusinessStateDoesNotMaterializeIgnoredData(t *testing.T) {
	const ignoredDataBytes = int64(8 * 1024 * 1024)
	reader := newLazyBusinessJSONReader(ignoredDataBytes)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	business, err := extractVideoBase64JSONWithBusiness(reader, io.Discard, io.Discard)

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	require.NoError(t, validateContentBusinessState(business))
	assert.Zero(t, reader.dataLeft)
	assert.Greater(t, reader.totalRead, ignoredDataBytes)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(2*1024*1024))
	assert.LessOrEqual(t, reader.maxRead, 32*1024)
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
		{"non canonical single padding", "AAB=", nil, true},
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
}

func TestDecodeVideoBase64StrictStreamingBoundaries(t *testing.T) {
	payload := bytes.Repeat([]byte("stream-boundary-"), 8)
	encoded := base64.StdEncoding.EncodeToString(payload)
	require.Greater(t, len(encoded), len(videoDataPrefix))

	t.Run("one byte short reads cross padding boundary", func(t *testing.T) {
		reader := newControlledBase64ReadSeeker(encoded, 1, -1, nil)
		var decoded bytes.Buffer

		written, err := decodeVideoBase64(reader, &decoded)

		require.NoError(t, err)
		assert.Equal(t, int64(len(payload)), written)
		assert.Equal(t, payload, decoded.Bytes())
		assert.Greater(t, reader.totalDecodeReads, len(encoded)/2)
		assert.Equal(t, int64(len(encoded)), reader.decodeBytesRead)
	})

	t.Run("data and deferred error after multiple quanta", func(t *testing.T) {
		const failAfter = int64(20)
		reader := newControlledBase64ReadSeeker(
			encoded,
			7,
			failAfter,
			errContentTrackingReader,
		)

		_, err := decodeVideoBase64(reader, io.Discard)

		require.ErrorIs(t, err, errContentTrackingReader)
		assert.Equal(t, failAfter, reader.decodeBytesRead)
		assert.Greater(t, reader.totalDecodeReads, 1)
	})

	t.Run("padded final quantum and deferred error share one read", func(t *testing.T) {
		paddedPayload := bytes.Repeat([]byte{0x5a}, 64)
		padded := base64.StdEncoding.EncodeToString(paddedPayload)
		require.True(t, strings.HasSuffix(padded, "=="))
		reader := newControlledBase64ReadSeeker(
			padded,
			0,
			int64(len(padded)),
			errContentTrackingReader,
		)
		var decoded bytes.Buffer

		written, err := decodeVideoBase64(reader, &decoded)

		require.ErrorIs(t, err, errContentTrackingReader)
		assert.Equal(t, int64(len(paddedPayload)), written)
		assert.Equal(t, paddedPayload, decoded.Bytes())
		assert.Equal(t, int64(len(padded)), reader.decodeBytesRead)
	})

	validLead := strings.Repeat("QUJD", 6)
	invalidCases := []struct {
		name  string
		input string
	}{
		{"line feed rejected", validLead + "QQ==\n"},
		{"carriage return rejected", validLead + "QQ==\r"},
		{"space rejected", validLead + "QQ== "},
		{"tab rejected", validLead + "QQ==\t"},
		{"invalid alphabet in middle", validLead + "$UJD"},
		{"invalid alphabet at tail", validLead + "QUJ$"},
		{"padding in first position", validLead + "=AAA"},
		{"padding in second position", validLead + "A=AA"},
		{"alphabet after padding", validLead + "AA=A"},
		{"more than two padding bytes", validLead + "A==="},
		{"noncanonical double padding bits", validLead + "AB=="},
		{"noncanonical single padding bits", validLead + "AAB="},
		{"incomplete final quantum", validLead + "AAA"},
	}
	for _, test := range invalidCases {
		t.Run(test.name, func(t *testing.T) {
			reader := newControlledBase64ReadSeeker(test.input, 1, -1, nil)
			_, err := decodeVideoBase64(reader, io.Discard)
			require.ErrorIs(t, err, errInvalidVideoBase64)
			assert.Greater(t, reader.totalDecodeReads, len(validLead)/2)
		})
	}

	t.Run("destination writer error is surfaced", func(t *testing.T) {
		reader := newControlledBase64ReadSeeker(encoded, 3, -1, nil)
		writer := &failAfterWriter{remaining: 11, err: errContentTestWriter}

		written, err := decodeVideoBase64(reader, writer)

		require.ErrorIs(t, err, errContentTestWriter)
		assert.Equal(t, int64(11), written)
		assert.Greater(t, reader.decodeBytesRead, int64(0))
	})

	t.Run("source read requests stay bounded", func(t *testing.T) {
		const quanta = 32 * 1024
		largeEncoded := strings.Repeat("QUJD", quanta)
		reader := newControlledBase64ReadSeeker(largeEncoded, 0, -1, nil)

		written, err := decodeVideoBase64(reader, io.Discard)

		require.NoError(t, err)
		assert.Equal(t, int64(quanta*3), written)
		assert.Equal(t, int64(len(largeEncoded)), reader.decodeBytesRead)
		assert.LessOrEqual(t, reader.maxRequested, 1024)
		assert.Greater(t, reader.totalDecodeReads, 2)
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

		content, err := newContentTestFetcher(directory).FetchVideoContent(
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

		content, err := newContentTestFetcher(directory).FetchVideoContent(
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

			content, err := newContentTestFetcher(directory).FetchVideoContent(
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

		content, err := newContentTestFetcher(directory).FetchVideoContent(
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
		const declaredLength = int64(1 << 50)
		body := &contentTestReadCloser{
			reader: strings.NewReader(videoSuccessJSON(validEncoded)),
		}
		fetcher := newContentTestFetcher(directory)
		responseObserved := false
		fetcher.deps.doRequest = func(req *http.Request) (*http.Response, error) {
			require.NotNil(t, req)
			responseObserved = true
			return &http.Response{
				StatusCode:    http.StatusOK,
				ContentLength: declaredLength,
				Body:          body,
				Header:        make(http.Header),
			}, nil
		}

		content, err := fetcher.FetchVideoContent(
			context.Background(), "http://content.test", "TEST_KEY", "UPSTREAM_TASK_ID", "",
		)
		require.NoError(t, err)
		require.NotNil(t, content)
		assert.True(t, responseObserved)
		assert.Equal(t, 1, body.closeCalls)
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

		content, err := newContentTestFetcher(directory).FetchVideoContent(
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

		content, err := newContentTestFetcher(directory).FetchVideoContent(
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

		content, err := newContentTestFetcher(directory).FetchVideoContent(
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
			_, err := newContentTestFetcher(directory).FetchVideoContent(
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
			_, err := newContentTestFetcher(directory).FetchVideoContent(
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

func TestFetchVideoContentTempFileStages(t *testing.T) {
	validEncoded := base64.StdEncoding.EncodeToString(testMP4Bytes())

	t.Run("all four files coexist as mode 0600 before ownership transfer", func(t *testing.T) {
		directory := useContentTempDir(t)
		fetcher := newContentTestFetcher(directory)
		recorder := newContentTempRecorder(directory)
		fetcher.deps.newTempFile = recorder.allocator(t)
		body := &contentTestReadCloser{
			reader: strings.NewReader(videoSuccessJSON(validEncoded)),
		}
		fetcher.deps.doRequest = func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       body,
				Header:     make(http.Header),
			}, nil
		}

		content, err := fetcher.FetchVideoContent(
			context.Background(),
			"http://content.test",
			"TEST_KEY",
			"UPSTREAM_TASK_ID",
			"",
		)

		require.NoError(t, err)
		require.Len(t, recorder.temps, 4)
		assert.Equal(t, []string{"raw", "redacted", "base64", "mp4"}, []string{
			recorder.temps[0].label,
			recorder.temps[1].label,
			recorder.temps[2].label,
			recorder.temps[3].label,
		})
		for _, temp := range recorder.temps[:3] {
			requireRecordedContentTempCleaned(t, temp)
		}

		mp4 := recorder.requireTemp(t, "mp4")
		requireContentHandleOpen(t, mp4.handle)
		assert.Nil(t, mp4.owner.file)
		assert.Empty(t, mp4.owner.path)
		info, err := os.Stat(mp4.path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		var brand [4]byte
		read, err := mp4.handle.ReadAt(brand[:], 4)
		require.NoError(t, err)
		assert.Equal(t, len(brand), read)
		assert.Equal(t, []byte("ftyp"), brand[:])
		assert.Equal(t, 1, body.closeCalls)

		require.NoError(t, content.Body.Close())
		requireContentHandleClosed(t, mp4.handle)
		_, err = os.Stat(mp4.path)
		require.ErrorIs(t, err, os.ErrNotExist)
		require.NoError(t, content.Body.Close())
		requireContentHandleClosed(t, mp4.handle)
		requireContentTempDirEmpty(t, directory)
	})

	for _, failLabel := range []string{"raw", "redacted", "base64", "mp4"} {
		t.Run(failLabel+" creation failure cleans prior files", func(t *testing.T) {
			directory := useContentTempDir(t)
			createErr := errors.New(failLabel + " tempfile creation stopped")
			fetcher := newContentTestFetcher(directory)
			recorder := newContentTempRecorder(directory)
			recorder.failLabel = failLabel
			recorder.failErr = createErr
			fetcher.deps.newTempFile = recorder.allocator(t)
			body := &contentTestReadCloser{
				reader: strings.NewReader(videoSuccessJSON(validEncoded)),
			}
			fetcher.deps.doRequest = func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       body,
					Header:     make(http.Header),
				}, nil
			}

			content, err := fetcher.FetchVideoContent(
				context.Background(),
				"http://content.test",
				"TEST_KEY",
				"UPSTREAM_TASK_ID",
				"",
			)

			assert.Nil(t, content)
			contentErr := assertVideoContentError(
				t,
				err,
				http.StatusBadGateway,
				"invalid_upstream_response",
				"invalid_upstream_response",
			)
			require.ErrorIs(t, contentErr.Cause, createErr)
			assert.Equal(t, 1, body.closeCalls)
			recorder.requireAllCleaned(t)
			requireContentTempDirEmpty(t, directory)
		})
	}
}

func TestFetchVideoContentTransportAndCopyFailures(t *testing.T) {
	validEncoded := base64.StdEncoding.EncodeToString(testMP4Bytes())
	validJSON := videoSuccessJSON(validEncoded)

	t.Run("general network error", func(t *testing.T) {
		directory := useContentTempDir(t)
		networkErr := errors.New("network stopped")
		fetcher := newContentTestFetcher(directory)
		fetcher.deps.doRequest = func(*http.Request) (*http.Response, error) {
			return nil, networkErr
		}

		content, err := fetcher.FetchVideoContent(
			context.Background(),
			"http://content.test",
			"TEST_KEY",
			"UPSTREAM_TASK_ID",
			"",
		)

		assert.Nil(t, content)
		contentErr := assertVideoContentError(
			t,
			err,
			http.StatusBadGateway,
			"upstream_error",
			"upstream_connection_error",
		)
		require.ErrorIs(t, contentErr.Cause, networkErr)
		requireContentTempDirEmpty(t, directory)
	})

	t.Run("upstream copy reader error", func(t *testing.T) {
		directory := useContentTempDir(t)
		readErr := errors.New("upstream body stopped")
		body := &contentTestReadCloser{
			reader: &dataAndErrorReader{
				data: []byte(`{"success":true,`),
				err:  readErr,
			},
		}
		fetcher := newContentTestFetcher(directory)
		recorder := newContentTempRecorder(directory)
		fetcher.deps.newTempFile = recorder.allocator(t)
		fetcher.deps.doRequest = func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       body,
				Header:     make(http.Header),
			}, nil
		}

		content, err := fetcher.FetchVideoContent(
			context.Background(),
			"http://content.test",
			"TEST_KEY",
			"UPSTREAM_TASK_ID",
			"",
		)

		assert.Nil(t, content)
		contentErr := assertVideoContentError(
			t,
			err,
			http.StatusBadGateway,
			"upstream_error",
			"upstream_connection_error",
		)
		require.ErrorIs(t, contentErr.Cause, readErr)
		assert.Equal(t, 1, body.closeCalls)
		recorder.requireAllCleaned(t)
		requireContentTempDirEmpty(t, directory)
	})

	t.Run("raw destination writer error", func(t *testing.T) {
		directory := useContentTempDir(t)
		body := &contentTestReadCloser{reader: strings.NewReader(validJSON)}
		fetcher := newContentTestFetcher(directory)
		recorder := newContentTempRecorder(directory)
		recorder.closeBeforeReturnLabel = "raw"
		fetcher.deps.newTempFile = recorder.allocator(t)
		fetcher.deps.doRequest = func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       body,
				Header:     make(http.Header),
			}, nil
		}
		require.Nil(t, fetcher.deps.copyResponse, "test must exercise default io.Copy")

		content, err := fetcher.FetchVideoContent(
			context.Background(),
			"http://content.test",
			"TEST_KEY",
			"UPSTREAM_TASK_ID",
			"",
		)

		assert.Nil(t, content)
		contentErr := assertVideoContentError(
			t,
			err,
			http.StatusBadGateway,
			"upstream_error",
			"upstream_connection_error",
		)
		require.ErrorIs(t, contentErr.Cause, os.ErrClosed)
		var pathErr *os.PathError
		require.ErrorAs(t, contentErr.Cause, &pathErr)
		assert.Equal(t, "write", pathErr.Op)
		raw := recorder.requireTemp(t, "raw")
		assert.Equal(t, raw.path, pathErr.Path)
		assert.Equal(t, 1, body.closeCalls)
		recorder.requireAllCleaned(t)
		requireContentTempDirEmpty(t, directory)
	})

	t.Run("upstream body close error", func(t *testing.T) {
		directory := useContentTempDir(t)
		closeErr := errors.New("upstream close stopped")
		body := &contentTestReadCloser{
			reader:   strings.NewReader(validJSON),
			closeErr: closeErr,
		}
		fetcher := newContentTestFetcher(directory)
		recorder := newContentTempRecorder(directory)
		fetcher.deps.newTempFile = recorder.allocator(t)
		fetcher.deps.doRequest = func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       body,
				Header:     make(http.Header),
			}, nil
		}

		content, err := fetcher.FetchVideoContent(
			context.Background(),
			"http://content.test",
			"TEST_KEY",
			"UPSTREAM_TASK_ID",
			"",
		)

		assert.Nil(t, content)
		contentErr := assertVideoContentError(
			t,
			err,
			http.StatusBadGateway,
			"upstream_error",
			"upstream_connection_error",
		)
		require.ErrorIs(t, contentErr.Cause, closeErr)
		assert.Equal(t, 1, body.closeCalls)
		recorder.requireAllCleaned(t)
		requireContentTempDirEmpty(t, directory)
	})

	t.Run("other HTTP status has invalid response contract", func(t *testing.T) {
		directory := useContentTempDir(t)
		body := &contentTestReadCloser{reader: strings.NewReader("ignored")}
		fetcher := newContentTestFetcher(directory)
		fetcher.deps.doRequest = func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       body,
				Header:     make(http.Header),
			}, nil
		}

		content, err := fetcher.FetchVideoContent(
			context.Background(),
			"http://content.test",
			"TEST_KEY",
			"UPSTREAM_TASK_ID",
			"",
		)

		assert.Nil(t, content)
		contentErr := assertVideoContentError(
			t,
			err,
			http.StatusBadGateway,
			"invalid_upstream_response",
			"invalid_upstream_response",
		)
		assert.Contains(t, contentErr.Cause.Error(), "503")
		assert.Equal(t, 1, body.closeCalls)
		requireContentTempDirEmpty(t, directory)
	})
}

func TestFetchVideoContentBusinessCauseContract(t *testing.T) {
	validEncoded := base64.StdEncoding.EncodeToString(testMP4Bytes())
	cases := []struct {
		name      string
		response  string
		wantCause error
	}{
		{
			name:      "success false",
			response:  `{"success":false,"errMessage":"PROVIDER_PRIVATE_DETAIL","video_base64":"` + validEncoded + `"}`,
			wantCause: errContentBusinessFailure,
		},
		{
			name:      "failed status",
			response:  `{"success":true,"status":"FAILED","message":"PROVIDER_PRIVATE_DETAIL","video_base64":"` + validEncoded + `"}`,
			wantCause: errContentBusinessFailure,
		},
		{
			name:      "nonzero errCode",
			response:  `{"success":true,"errCode":"RATE_LIMIT","video_base64":"` + validEncoded + `"}`,
			wantCause: errContentBusinessFailure,
		},
		{
			name:      "missing success",
			response:  `{"status":"completed","video_base64":"` + validEncoded + `"}`,
			wantCause: errInvalidContentBusinessEnvelope,
		},
		{
			name:      "invalid success shape",
			response:  `{"success":"true","video_base64":"` + validEncoded + `"}`,
			wantCause: errInvalidContentBusinessEnvelope,
		},
		{
			name:      "invalid errCode shape",
			response:  `{"success":true,"errCode":{},"video_base64":"` + validEncoded + `"}`,
			wantCause: errInvalidContentBusinessEnvelope,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			directory := useContentTempDir(t)
			body := &contentTestReadCloser{
				reader: strings.NewReader(test.response),
			}
			fetcher := newContentTestFetcher(directory)
			fetcher.deps.doRequest = func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       body,
					Header:     make(http.Header),
				}, nil
			}

			content, err := fetcher.FetchVideoContent(
				context.Background(),
				"http://content.test",
				"TEST_KEY",
				"UPSTREAM_TASK_ID",
				"",
			)

			assert.Nil(t, content)
			contentErr := assertVideoContentError(
				t,
				err,
				http.StatusBadGateway,
				"invalid_upstream_response",
				"invalid_upstream_response",
			)
			require.ErrorIs(t, contentErr.Cause, test.wantCause)
			assert.Equal(t, contentInvalidMessage, contentErr.Message)
			assert.NotContains(t, contentErr.Cause.Error(), "PROVIDER_PRIVATE_DETAIL")
			assert.Equal(t, 1, body.closeCalls)
			requireContentTempDirEmpty(t, directory)
		})
	}
}

func TestFetchVideoContentConsumerReadFailureStillCleansUp(t *testing.T) {
	directory := useContentTempDir(t)
	validEncoded := base64.StdEncoding.EncodeToString(testMP4Bytes())
	body := &contentTestReadCloser{
		reader: strings.NewReader(videoSuccessJSON(validEncoded)),
	}
	fetcher := newContentTestFetcher(directory)
	fetcher.deps.doRequest = func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       body,
			Header:     make(http.Header),
		}, nil
	}

	content, err := fetcher.FetchVideoContent(
		context.Background(),
		"http://content.test",
		"TEST_KEY",
		"UPSTREAM_TASK_ID",
		"",
	)
	require.NoError(t, err)
	removingBody, ok := content.Body.(*removingReadCloser)
	require.True(t, ok)
	require.NoError(t, removingBody.file.Close())

	read, readErr := removingBody.Read(make([]byte, 1))
	assert.Zero(t, read)
	require.Error(t, readErr)
	require.Error(t, content.Body.Close())
	require.NoError(t, content.Body.Close())
	requireContentTempDirEmpty(t, directory)
}
