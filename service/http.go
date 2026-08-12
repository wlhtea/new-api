package service

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"

	"github.com/gin-gonic/gin"
)

const responseBodyWriteErrorKey = "oneapi_response_body_write_error"

// ResetResponseBodyWriteError clears response-copy state before a response
// handler starts writing to a potentially reused Gin context.
func ResetResponseBodyWriteError(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(responseBodyWriteErrorKey, nil)
}

// ResponseBodyWriteError returns the first response-copy error recorded for
// the current request.
func ResponseBodyWriteError(c *gin.Context) error {
	if c == nil {
		return nil
	}
	value, exists := c.Get(responseBodyWriteErrorKey)
	if !exists {
		return nil
	}
	err, _ := value.(error)
	return err
}

func recordResponseBodyWriteError(c *gin.Context, err error) {
	if c == nil || err == nil || ResponseBodyWriteError(c) != nil {
		return
	}
	c.Set(responseBodyWriteErrorKey, err)
}

func CloseResponseBodyGracefully(httpResponse *http.Response) {
	if httpResponse == nil || httpResponse.Body == nil {
		return
	}
	err := httpResponse.Body.Close()
	if err != nil {
		common.SysError("failed to close response body: " + err.Error())
	}
}

// ShouldCopyUpstreamHeader checks whether a given upstream response header
// should be copied to the client response. X-Oneapi-Request-Id is captured for
// internal logging but never exposed. OpenCode Go response headers are entirely
// upstream-controlled, so its public response headers are synthesized locally.
func ShouldCopyUpstreamHeader(c *gin.Context, k string, v []string) bool {
	if strings.EqualFold(k, common.RequestIdKey) {
		if c != nil && len(v) > 0 {
			c.Set(common.UpstreamRequestIdKey, v[0])
		}
		return false
	}
	if c != nil && common.GetContextKeyInt(c, constant.ContextKeyChannelType) == constant.ChannelTypeOpenCodeGo {
		return false
	}
	if strings.EqualFold(k, "Content-Length") {
		return false
	}
	return true
}

func IOCopyBytesGracefully(c *gin.Context, src *http.Response, data []byte) {
	if c.Writer == nil {
		return
	}

	body := io.NopCloser(bytes.NewBuffer(data))

	// We shouldn't set the header before we parse the response body, because the parse part may fail.
	// And then we will have to send an error response, but in this case, the header has already been set.
	// So the httpClient will be confused by the response.
	// For example, Postman will report error, and we cannot check the response at all.
	if src != nil {
		for k, v := range src.Header {
			if !ShouldCopyUpstreamHeader(c, k, v) {
				continue
			}
			c.Writer.Header().Set(k, v[0])
		}
	}
	if common.GetContextKeyInt(c, constant.ContextKeyChannelType) == constant.ChannelTypeOpenCodeGo {
		c.Writer.Header().Set("Content-Type", gin.MIMEJSON)
	}

	// set Content-Length header manually BEFORE calling WriteHeader
	c.Writer.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))

	// Write header with status code (this sends the headers)
	if src != nil {
		c.Writer.WriteHeader(src.StatusCode)
	} else {
		c.Writer.WriteHeader(http.StatusOK)
	}

	_, err := io.Copy(c.Writer, body)
	if err != nil {
		recordResponseBodyWriteError(c, err)
		logger.LogError(c, fmt.Sprintf("failed to copy response body: %s", err.Error()))
	}
	c.Writer.Flush()
}
