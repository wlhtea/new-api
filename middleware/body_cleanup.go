package middleware

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// BodyStorageCleanup 请求体存储清理中间件
// 在请求处理完成后自动清理磁盘/内存缓存
func BodyStorageCleanup() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			// Request-owned leases and response bodies may depend on body-backed
			// state, so release them before the request body and file caches.
			common.RunRequestCleanups(c)
			common.CleanupBodyStorage(c)
			service.CleanupFileSources(c)
		}()
		c.Next()
	}
}
