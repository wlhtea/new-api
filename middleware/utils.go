package middleware

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func abortWithOpenAiMessage(c *gin.Context, statusCode int, message string, code ...types.ErrorCode) {
	codeStr := ""
	if len(code) > 0 {
		codeStr = string(code[0])
	}
	userId := c.GetInt("id")
	logger.LogError(c.Request.Context(), fmt.Sprintf("user %d | %s", userId, message))
	internalErr := types.WithOpenAIError(types.OpenAIError{
		Message: message,
		Type:    "new_api_error",
		Code:    codeStr,
	}, statusCode)
	publicErr := service.PublicOpenCodeGoRelayError(
		common.GetContextKeyInt(c, constant.ContextKeyChannelType),
		internalErr,
	)
	if publicErr != internalErr {
		publicOpenAIError := publicErr.ToOpenAIError()
		publicOpenAIError.Message = common.MessageWithRequestId(publicOpenAIError.Message, c.GetString(common.RequestIdKey))
		c.JSON(publicErr.StatusCode, gin.H{"error": publicOpenAIError})
	} else {
		c.JSON(statusCode, gin.H{
			"error": gin.H{
				"message": common.MessageWithRequestId(message, c.GetString(common.RequestIdKey)),
				"type":    "new_api_error",
				"code":    codeStr,
			},
		})
	}
	c.Abort()
}

func abortWithMidjourneyMessage(c *gin.Context, statusCode int, code int, description string) {
	c.JSON(statusCode, gin.H{
		"description": description,
		"type":        "new_api_error",
		"code":        code,
	})
	c.Abort()
	logger.LogError(c.Request.Context(), description)
}
