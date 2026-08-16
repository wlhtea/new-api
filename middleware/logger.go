package middleware

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

const RouteTagKey = "route_tag"

func RouteTag(tag string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(RouteTagKey, tag)
		c.Next()
	}
}

func SetUpLogger(server *gin.Engine) {
	server.Use(gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		var requestID string
		if param.Keys != nil {
			requestID, _ = param.Keys[common.RequestIdKey].(string)
		}
		tag, _ := param.Keys[RouteTagKey].(string)
		if tag == "" {
			tag = "web"
		}
		return fmt.Sprintf("[GIN] %s | %s | %s | %3d | %13v | %15s | %7s %s\n",
			param.TimeStamp.Format("2006/01/02 - 15:04:05"),
			tag,
			requestID,
			param.StatusCode,
			param.Latency,
			param.ClientIP,
			param.Method,
			sanitizeAccessLogPath(param.Path),
		)
	}))
}

func sanitizeAccessLogPath(path string) string {
	path = common.SanitizeRelayRequestPathForLog(path)
	if !strings.Contains(path, "/opencode-go/") {
		return path
	}
	segments := strings.Split(path, "/")
	for index := 0; index+2 < len(segments); index++ {
		if segments[index] != "opencode-go" || (segments[index+1] != "workspaces" && segments[index+1] != "identities") {
			continue
		}
		identifier := segments[index+2]
		if identifier == "" || isOpenCodeGoCollectionAction(identifier) {
			continue
		}
		segments[index+2] = common.OpenCodeGoDiagnosticRef("access-"+segments[index+1], identifier)
	}
	return strings.Join(segments, "/")
}

func isOpenCodeGoCollectionAction(segment string) bool {
	switch segment {
	case "import", "non-members", "batch-china-models", "batch-cancel-renewal":
		return true
	default:
		return false
	}
}
