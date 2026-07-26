package seedance

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/service"
)

const videoDataPrefix = "data:video/mp4;base64,"

const (
	contentAuthenticationMessage = "upstream authentication failed"
	contentRateLimitMessage      = "upstream rate limit exceeded"
	contentTimeoutMessage        = "upstream request timed out"
	contentConnectionMessage     = "upstream connection failed"
	contentInvalidMessage        = "upstream returned invalid video content"
)

var (
	errUnsupportedVideoDataURI = errors.New("unsupported video data URI")
	errInvalidVideoBase64      = errors.New("invalid video base64")
	errInvalidMP4              = errors.New("invalid MP4")

	contentTempDir      = os.TempDir()
	contentTempSequence uint64
)

type strictBase64Reader struct {
	src io.Reader

	length      int64
	padding     int
	seenPadding bool
	finished    bool
	pendingErr  error
}

func (r *strictBase64Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.finished {
		return 0, io.EOF
	}
	if r.pendingErr != nil {
		err := r.pendingErr
		r.pendingErr = nil
		if err == io.EOF {
			return r.finish()
		}
		return 0, err
	}

	n, err := r.src.Read(p)
	if n > 0 {
		if validationErr := r.validate(p[:n]); validationErr != nil {
			return 0, validationErr
		}
		if err != nil {
			r.pendingErr = err
		}
		return n, nil
	}
	if err == nil {
		return 0, nil
	}
	if err == io.EOF {
		return r.finish()
	}
	return 0, err
}

func (r *strictBase64Reader) validate(value []byte) error {
	for _, character := range value {
		r.length++
		switch {
		case isBase64Alphabet(character):
			if r.seenPadding {
				return errInvalidVideoBase64
			}
		case character == '=':
			r.seenPadding = true
			r.padding++
			if r.padding > 2 {
				return errInvalidVideoBase64
			}
		default:
			return errInvalidVideoBase64
		}
	}
	return nil
}

func (r *strictBase64Reader) finish() (int, error) {
	r.finished = true
	if r.length%4 != 0 {
		return 0, errInvalidVideoBase64
	}
	switch r.padding {
	case 0, 1, 2:
		return 0, io.EOF
	default:
		return 0, errInvalidVideoBase64
	}
}

func isBase64Alphabet(value byte) bool {
	return value >= 'A' && value <= 'Z' ||
		value >= 'a' && value <= 'z' ||
		value >= '0' && value <= '9' ||
		value == '+' || value == '/'
}

func decodeVideoBase64(src io.ReadSeeker, dst io.Writer) (int64, error) {
	if src == nil || dst == nil {
		return 0, errInvalidVideoBase64
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}

	prefix := make([]byte, len(videoDataPrefix))
	read, prefixErr := io.ReadFull(src, prefix)
	startsDataURI := read >= len("data:") &&
		bytes.Equal(prefix[:len("data:")], []byte("data:"))
	if startsDataURI {
		if read != len(videoDataPrefix) ||
			!bytes.Equal(prefix, []byte(videoDataPrefix)) {
			return 0, errUnsupportedVideoDataURI
		}
	} else {
		if prefixErr != nil && prefixErr != io.EOF &&
			prefixErr != io.ErrUnexpectedEOF {
			return 0, prefixErr
		}
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
	}

	validated := &strictBase64Reader{src: src}
	decoder := base64.NewDecoder(base64.StdEncoding.Strict(), validated)
	written, err := io.Copy(dst, decoder)
	if err != nil {
		return written, normalizeBase64Error(err)
	}

	var probe [1]byte
	read, err = decoder.Read(probe[:])
	if read != 0 || err != io.EOF {
		if err == nil {
			return written, errInvalidVideoBase64
		}
		return written, normalizeBase64Error(err)
	}
	return written, nil
}

func normalizeBase64Error(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errInvalidVideoBase64) {
		return errInvalidVideoBase64
	}
	var corrupt base64.CorruptInputError
	if errors.As(err, &corrupt) {
		return errInvalidVideoBase64
	}
	return err
}

func validateMP4(src io.ReadSeeker) (result error) {
	if src == nil {
		return errInvalidMP4
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return errInvalidMP4
	}
	defer func() {
		if _, err := src.Seek(0, io.SeekStart); err != nil && result == nil {
			result = errInvalidMP4
		}
	}()

	header := make([]byte, 12)
	if _, err := io.ReadFull(src, header); err != nil {
		return errInvalidMP4
	}
	boxSize := binary.BigEndian.Uint32(header[:4])
	if string(header[4:8]) != "ftyp" || boxSize < 8 {
		return errInvalidMP4
	}
	return nil
}

type removingReadCloser struct {
	file  *os.File
	paths []string
	once  sync.Once
}

func (r *removingReadCloser) Read(p []byte) (int, error) {
	return r.file.Read(p)
}

func (r *removingReadCloser) Close() error {
	var closeErr error
	r.once.Do(func() {
		closeErr = r.file.Close()
		for _, path := range r.paths {
			_ = os.Remove(path)
		}
	})
	return closeErr
}

type contentTempFile struct {
	file *os.File
	path string
}

func newContentTempFile(label string) (*contentTempFile, error) {
	directory := contentTempDir
	if directory == "" {
		directory = os.TempDir()
	}
	for attempt := 0; attempt < 128; attempt++ {
		sequence := atomic.AddUint64(&contentTempSequence, 1)
		path := filepath.Join(
			directory,
			fmt.Sprintf(".seedance-content-%s-%d-%d", label, os.Getpid(), sequence),
		)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &contentTempFile{file: file, path: path}, nil
	}
	return nil, errors.New("unable to allocate Seed Dance content temporary file")
}

func (f *contentTempFile) closeAndRemove() {
	if f == nil {
		return
	}
	if f.file != nil {
		_ = f.file.Close()
		f.file = nil
	}
	if f.path != "" {
		_ = os.Remove(f.path)
		f.path = ""
	}
}

func contentError(
	status int,
	errorType string,
	code string,
	message string,
	cause error,
) error {
	return &channel.VideoContentError{
		StatusCode: status,
		Type:       errorType,
		Code:       code,
		Message:    message,
		Cause:      cause,
	}
}

func invalidContentResponse(cause error) error {
	return contentError(
		http.StatusBadGateway,
		"invalid_upstream_response",
		"invalid_upstream_response",
		contentInvalidMessage,
		cause,
	)
}

func contentRequestError(ctx context.Context, cause error) error {
	if errors.Is(cause, context.DeadlineExceeded) ||
		errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return contentError(
			http.StatusGatewayTimeout,
			"upstream_timeout_error",
			"upstream_timeout_error",
			contentTimeoutMessage,
			cause,
		)
	}
	return contentError(
		http.StatusBadGateway,
		"upstream_error",
		"upstream_connection_error",
		contentConnectionMessage,
		cause,
	)
}

func (a *TaskAdaptor) FetchVideoContent(
	parent context.Context,
	baseURL string,
	key string,
	upstreamTaskID string,
	proxy string,
) (*channel.VideoContent, error) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, contentTimeout)

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		strings.TrimRight(baseURL, "/")+"/video/"+url.PathEscape(upstreamTaskID),
		nil,
	)
	if err != nil {
		cancel()
		return nil, contentRequestError(ctx, err)
	}
	req.Header.Set("Authorization", "Bearer "+key)

	baseClient, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		cancel()
		return nil, contentRequestError(ctx, err)
	}
	client, err := newStageClient(baseClient, proxy, connectTimeout)
	if err != nil {
		cancel()
		return nil, contentRequestError(ctx, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, contentRequestError(ctx, err)
	}
	if resp == nil {
		cancel()
		return nil, contentRequestError(ctx, errors.New("empty upstream video response"))
	}
	resp = bindCancelToBody(resp, cancel)

	switch resp.StatusCode {
	case http.StatusOK:
		// Continue through the streaming extraction flow below.
	case http.StatusUnauthorized, http.StatusForbidden:
		_ = resp.Body.Close()
		return nil, contentError(
			http.StatusBadGateway,
			"upstream_error",
			"upstream_authentication_error",
			contentAuthenticationMessage,
			fmt.Errorf("upstream returned HTTP %d", resp.StatusCode),
		)
	case http.StatusTooManyRequests:
		_ = resp.Body.Close()
		return nil, contentError(
			http.StatusTooManyRequests,
			"upstream_rate_limit_error",
			"upstream_rate_limit_error",
			contentRateLimitMessage,
			fmt.Errorf("upstream returned HTTP %d", resp.StatusCode),
		)
	default:
		_ = resp.Body.Close()
		return nil, invalidContentResponse(
			fmt.Errorf("upstream returned HTTP %d", resp.StatusCode),
		)
	}

	var rawResponse, redactedJSON, encodedBase64, mp4 *contentTempFile
	completed := false
	defer func() {
		if !completed {
			rawResponse.closeAndRemove()
			redactedJSON.closeAndRemove()
			encodedBase64.closeAndRemove()
			mp4.closeAndRemove()
		}
	}()

	rawResponse, err = newContentTempFile("raw")
	if err != nil {
		_ = resp.Body.Close()
		return nil, invalidContentResponse(err)
	}
	if _, copyErr := io.Copy(rawResponse.file, resp.Body); copyErr != nil {
		_ = resp.Body.Close()
		return nil, contentRequestError(ctx, copyErr)
	}
	if err := resp.Body.Close(); err != nil {
		return nil, contentRequestError(ctx, err)
	}
	if _, err := rawResponse.file.Seek(0, io.SeekStart); err != nil {
		return nil, invalidContentResponse(err)
	}

	redactedJSON, err = newContentTempFile("redacted")
	if err != nil {
		return nil, invalidContentResponse(err)
	}
	encodedBase64, err = newContentTempFile("base64")
	if err != nil {
		return nil, invalidContentResponse(err)
	}
	if err := extractVideoBase64JSON(
		rawResponse.file,
		redactedJSON.file,
		encodedBase64.file,
	); err != nil {
		return nil, invalidContentResponse(err)
	}

	if _, err := redactedJSON.file.Seek(0, io.SeekStart); err != nil {
		return nil, invalidContentResponse(err)
	}
	var provider providerEnvelope
	if err := common.DecodeJson(redactedJSON.file, &provider); err != nil {
		return nil, invalidContentResponse(err)
	}
	if provider.Success == nil {
		return nil, invalidContentResponse(
			errors.New("upstream content response has no success state"),
		)
	}
	if explicitBusinessFailure(&provider) {
		return nil, invalidContentResponse(errors.New(providerErrorMessage(&provider)))
	}

	if _, err := encodedBase64.file.Seek(0, io.SeekStart); err != nil {
		return nil, invalidContentResponse(err)
	}
	mp4, err = newContentTempFile("mp4")
	if err != nil {
		return nil, invalidContentResponse(err)
	}
	decodedLength, err := decodeVideoBase64(encodedBase64.file, mp4.file)
	if err != nil {
		return nil, invalidContentResponse(err)
	}
	if err := validateMP4(mp4.file); err != nil {
		return nil, invalidContentResponse(err)
	}
	if _, err := mp4.file.Seek(0, io.SeekStart); err != nil {
		return nil, invalidContentResponse(err)
	}

	rawResponse.closeAndRemove()
	redactedJSON.closeAndRemove()
	encodedBase64.closeAndRemove()
	removingBody := &removingReadCloser{
		file:  mp4.file,
		paths: []string{mp4.path},
	}
	mp4.file = nil
	mp4.path = ""
	completed = true

	return &channel.VideoContent{
		ContentType:   "video/mp4",
		ContentLength: decodedLength,
		Body:          removingBody,
	}, nil
}
