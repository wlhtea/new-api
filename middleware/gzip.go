package middleware

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

type readCloser struct {
	io.Reader
	closeFn  func() error
	closeErr error
	once     sync.Once
}

func (rc *readCloser) Close() error {
	rc.once.Do(func() {
		if rc.closeFn != nil {
			rc.closeErr = rc.closeFn()
		}
	})
	return rc.closeErr
}

var errInvalidRequestContentEncoding = errors.New("invalid request content encoding")

type contentEncodingReader struct {
	io.Reader
	encoding string
}

type singleGzipReader struct {
	reader  *gzip.Reader
	source  *bufio.Reader
	checked bool
}

func (reader *singleGzipReader) Read(p []byte) (int, error) {
	n, err := reader.reader.Read(p)
	if !errors.Is(err, io.EOF) || reader.checked {
		return n, err
	}
	reader.checked = true
	_, trailingErr := reader.source.Peek(1)
	if errors.Is(trailingErr, io.EOF) {
		return n, io.EOF
	}
	if trailingErr != nil {
		return n, trailingErr
	}
	return n, errors.New("gzip request body contains multiple members or trailing data")
}

func (reader *contentEncodingReader) Read(p []byte) (int, error) {
	n, err := reader.Reader.Read(p)
	if err == nil || errors.Is(err, io.EOF) || common.IsRequestBodyTooLargeError(err) {
		return n, err
	}
	return n, fmt.Errorf("%w: %s", errInvalidRequestContentEncoding, reader.encoding)
}

func requestBodyLimitBytes() int64 {
	maxMB := constant.MaxRequestBodyMB
	if maxMB <= 0 {
		maxMB = 32
	}
	return int64(maxMB) << 20
}

// EncodedRequestAdmissionMiddleware caps bytes received from the client before
// authentication. It performs no decompression or format parsing.
func EncodedRequestAdmissionMiddleware() gin.HandlerFunc {
	return encodedRequestAdmissionMiddleware(requestBodyLimitBytes())
}

func encodedRequestAdmissionMiddleware(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil && c.Request.Method != http.MethodGet {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		}
		c.Next()
	}
}

func normalizedRequestContentEncoding(header http.Header) (string, error) {
	var encodings []string
	for _, value := range header.Values("Content-Encoding") {
		for _, item := range strings.Split(value, ",") {
			item = strings.ToLower(strings.TrimSpace(item))
			if item != "" {
				encodings = append(encodings, item)
			}
		}
	}
	if len(encodings) == 0 {
		return "", nil
	}
	if len(encodings) != 1 {
		return "", errors.New("multiple or stacked Content-Encoding values are unsupported")
	}
	switch encodings[0] {
	case "identity", "gzip", "br", "zstd":
		return encodings[0], nil
	default:
		return "", errors.New("unsupported Content-Encoding")
	}
}

func rejectRequestContentEncoding(c *gin.Context, statusCode int, message string) {
	if c == nil {
		return
	}
	if c.Request != nil {
		if c.Request.Body != nil {
			_ = c.Request.Body.Close()
		}
		if c.Request.URL != nil {
			if format, matched := preValidatedRelayFormat(c.Request.Method, c.Request.URL.Path); matched {
				renderRelayTransportValidationError(c, format, statusCode, message)
				return
			}
		}
	}
	c.AbortWithStatusJSON(statusCode, gin.H{"error": gin.H{
		"message": message,
		"type":    "invalid_request_error",
		"code":    "invalid_request_error",
	}})
}

func DecompressRequestMiddleware() gin.HandlerFunc {
	return decompressRequestMiddleware(requestBodyLimitBytes())
}

func decompressRequestMiddleware(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body == nil || c.Request.Method == http.MethodGet {
			c.Next()
			return
		}
		origBody := c.Request.Body
		encodedBody := http.MaxBytesReader(c.Writer, origBody, maxBytes)
		wrapDecodedMaxBytes := func(body io.ReadCloser) io.ReadCloser {
			return http.MaxBytesReader(c.Writer, body, maxBytes)
		}
		encoding, err := normalizedRequestContentEncoding(c.Request.Header)
		if err != nil {
			rejectRequestContentEncoding(c, http.StatusUnsupportedMediaType, "request Content-Encoding is unsupported")
			return
		}

		switch encoding {
		case "", "identity":
			c.Request.Body = encodedBody
		case "gzip":
			bufferedBody := bufio.NewReader(encodedBody)
			gzipReader, err := gzip.NewReader(bufferedBody)
			if err != nil {
				if common.IsRequestBodyTooLargeError(err) {
					rejectRequestContentEncoding(c, http.StatusRequestEntityTooLarge, "request body is too large")
					return
				}
				rejectRequestContentEncoding(c, http.StatusBadRequest, "request body is not valid gzip data")
				return
			}
			gzipReader.Multistream(false)
			c.Request.Body = wrapDecodedMaxBytes(&readCloser{
				Reader: &contentEncodingReader{
					Reader:   &singleGzipReader{reader: gzipReader, source: bufferedBody},
					encoding: encoding,
				},
				closeFn: func() error {
					_ = gzipReader.Close()
					return encodedBody.Close()
				},
			})
		case "br":
			reader := brotli.NewReader(encodedBody)
			c.Request.Body = wrapDecodedMaxBytes(&readCloser{
				Reader: &contentEncodingReader{Reader: reader, encoding: encoding},
				closeFn: func() error {
					return encodedBody.Close()
				},
			})
		case "zstd":
			reader, err := zstd.NewReader(
				encodedBody,
				zstd.WithDecoderConcurrency(1),
				zstd.WithDecoderMaxMemory(uint64(maxBytes)),
			)
			if err != nil {
				rejectRequestContentEncoding(c, http.StatusBadRequest, "request body is not valid zstd data")
				return
			}
			c.Request.Body = wrapDecodedMaxBytes(&readCloser{
				Reader: &contentEncodingReader{Reader: reader, encoding: encoding},
				closeFn: func() error {
					reader.Close()
					return encodedBody.Close()
				},
			})
		}

		c.Request.Header.Del("Content-Encoding")
		c.Request.Header.Del("Content-Length")
		c.Request.ContentLength = -1

		c.Next()
	}
}
