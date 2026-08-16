package middleware

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

const (
	panicPublicMessage = "服务器内部错误"
	panicPublicType    = "internal_server_error"
)

// ProviderNeutralRecovery is the outermost panic boundary. It intentionally
// does not log or render the panic value, stack, upstream data, or cause chain.
func ProviderNeutralRecovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recover() == nil {
				return
			}
			requestID := c.GetString(common.RequestIdKey)
			if requestID == "" {
				common.SysError("request panic recovered")
			} else {
				common.SysError("request panic recovered: request_id=" + requestID)
			}
			c.Abort()
			if c.Writer == nil || c.Writer.Written() {
				return
			}
			resetPanicResponseHeaders(c, requestID)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"message": panicPublicMessage,
					"type":    panicPublicType,
					"code":    panicPublicType,
				},
			})
		}()
		c.Next()
	}
}

func resetPanicResponseHeaders(c *gin.Context, requestID string) {
	header := c.Writer.Header()
	localCORS := make(http.Header)
	for _, name := range []string{
		"Access-Control-Allow-Credentials",
		"Access-Control-Allow-Origin",
		"Access-Control-Expose-Headers",
		"Vary",
	} {
		if values := header.Values(name); len(values) > 0 {
			localCORS[name] = append([]string(nil), values...)
		}
	}
	for name := range header {
		header.Del(name)
	}
	for name, values := range localCORS {
		for _, value := range values {
			header.Add(name, value)
		}
	}
	if requestID != "" {
		header.Set(common.RequestIdKey, requestID)
	}
}
