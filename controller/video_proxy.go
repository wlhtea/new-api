package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"

	"github.com/gin-gonic/gin"
)

const videoProxyTaskAdaptorLookupContextKey = "video_proxy_task_adaptor_lookup"

// videoProxyTaskAdaptorLookup is an intentionally request-local test seam.
// Its zero-value production path always resolves the registered adaptor.
type videoProxyTaskAdaptorLookup interface {
	TaskAdaptor(constant.TaskPlatform) channel.TaskAdaptor
}

func getVideoProxyTaskAdaptor(c *gin.Context, platform constant.TaskPlatform) channel.TaskAdaptor {
	if c != nil {
		if value, ok := c.Get(videoProxyTaskAdaptorLookupContextKey); ok {
			if lookup, ok := value.(videoProxyTaskAdaptorLookup); ok && lookup != nil {
				return lookup.TaskAdaptor(platform)
			}
		}
	}
	return relay.GetTaskAdaptor(platform)
}

// videoProxyError returns a standardized OpenAI-style error response.
func videoProxyError(c *gin.Context, status int, errType, code, message string) {
	if isOpenAIVideoPath(c) {
		writeOpenAIVideoError(c, status, errType, code, message)
		return
	}
	c.JSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    errType,
		},
	})
}

func validVideoContentError(err *channel.VideoContentError) bool {
	return err != nil &&
		err.StatusCode >= http.StatusBadRequest &&
		err.StatusCode <= 599 &&
		strings.TrimSpace(err.Type) != "" &&
		strings.TrimSpace(err.Code) != "" &&
		strings.TrimSpace(err.Message) != ""
}

func videoContentFetchFailure(c *gin.Context, logMessage string) {
	logger.LogError(c, logMessage)
	videoProxyError(
		c,
		http.StatusBadGateway,
		"upstream_error",
		"upstream_connection_error",
		"failed to fetch video content",
	)
}

func VideoProxy(c *gin.Context) {
	taskID := c.Param("task_id")
	if taskID == "" {
		videoProxyError(c, http.StatusBadRequest, "invalid_request_error", "task_id_required", "task_id is required")
		return
	}

	userID := c.GetInt("id")
	task, exists, err := model.GetByTaskId(userID, taskID)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to query task %s: %s", taskID, err.Error()))
		videoProxyError(c, http.StatusInternalServerError, "server_error", "task_lookup_failed", "failed to retrieve video task")
		return
	}
	if !exists || task == nil {
		videoProxyError(c, http.StatusNotFound, "invalid_request_error", "task_not_found", "video task was not found")
		return
	}

	if task.Status != model.TaskStatusSuccess {
		if task.Status == model.TaskStatusFailure {
			videoProxyError(c, http.StatusBadRequest, "invalid_request_error", "task_failed", "video task failed")
			return
		}
		videoProxyError(c, http.StatusBadRequest, "invalid_request_error", "task_not_completed", "video task is not completed")
		return
	}

	if originChannel, channelErr := model.GetChannelById(task.ChannelId, true); channelErr == nil &&
		!model.ChannelTypeSupportsRequestPath(originChannel.Type, c.Request.URL.Path) {
		videoProxyError(
			c,
			http.StatusBadRequest,
			"invalid_request_error",
			"task_channel_unsupported_endpoint",
			"video task channel does not support content retrieval",
		)
		return
	}

	adaptor := getVideoProxyTaskAdaptor(c, task.Platform)
	if fetcher, ok := adaptor.(channel.VideoContentFetcher); ok {
		channelModel, channelErr := model.GetChannelById(task.ChannelId, true)
		if channelErr != nil {
			logger.LogError(c, "video channel is unavailable")
			videoProxyError(c, http.StatusBadGateway, "upstream_error", "channel_unavailable", "video channel is unavailable")
			return
		}
		key, keyErr := service.ResolveStoredTaskKey(channelModel, task.PrivateData.Key)
		if keyErr != nil {
			logger.LogError(c, "video channel credential is unavailable")
			videoProxyError(c, http.StatusBadGateway, "upstream_error", "stored_credential_unavailable", "video channel credential is unavailable")
			return
		}
		content, fetchErr := fetcher.FetchVideoContent(
			c.Request.Context(),
			channelModel.GetBaseURL(),
			key,
			task.PrivateData.UpstreamTaskID,
			channelModel.GetSetting().Proxy,
		)
		if fetchErr != nil {
			if content != nil && content.Body != nil {
				_ = content.Body.Close()
			}
			var structured *channel.VideoContentError
			if errors.As(fetchErr, &structured) && structured != nil {
				if !validVideoContentError(structured) {
					videoContentFetchFailure(c, "video content fetch returned a malformed structured error")
					return
				}
				logger.LogError(c, "video content fetch failed with a structured upstream error")
				videoProxyError(c, structured.StatusCode, structured.Type, structured.Code, structured.Message)
				return
			}
			videoContentFetchFailure(c, "video content fetch failed")
			return
		}
		if content == nil || content.Body == nil {
			videoContentFetchFailure(c, "video content fetch returned an empty body")
			return
		}
		if content.ContentLength < 0 {
			_ = content.Body.Close()
			videoContentFetchFailure(c, "video content fetch returned an invalid content length")
			return
		}
		defer content.Body.Close()
		c.Header("Content-Type", "video/mp4")
		c.Header("Content-Disposition", fmt.Sprintf(`inline; filename="%s.mp4"`, task.TaskID))
		c.Header("Cache-Control", "private, max-age=3600")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Content-Length", strconv.FormatInt(content.ContentLength, 10))
		c.Status(http.StatusOK)
		if _, copyErr := io.Copy(c.Writer, content.Body); copyErr != nil {
			logger.LogError(c, fmt.Sprintf("Failed to stream video content: %s", copyErr.Error()))
		}
		return
	}

	channel, err := model.CacheGetChannel(task.ChannelId)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to get channel for task %s: %s", taskID, err.Error()))
		videoProxyError(c, http.StatusInternalServerError, "server_error", "channel_unavailable", "Failed to retrieve channel information")
		return
	}
	baseURL := channel.GetBaseURL()
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}

	var videoURL string
	proxy := channel.GetSetting().Proxy
	client := service.GetSSRFProtectedHTTPClient()
	if proxy != "" {
		// 渠道代理路径的连接由代理侧建立，无法做拨号时逐 IP 校验，
		// 因此后面对 videoURL 保留请求前的一次性 SSRF 校验。
		client, err = service.GetHttpClientWithProxy(proxy)
		if err != nil {
			logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to create proxy client for task %s: %s", taskID, err.Error()))
			videoProxyError(c, http.StatusInternalServerError, "server_error", "proxy_client_unavailable", "Failed to create proxy client")
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "", nil)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to create request: %s", err.Error()))
		videoProxyError(c, http.StatusInternalServerError, "server_error", "proxy_request_failed", "Failed to create proxy request")
		return
	}

	switch channel.Type {
	case constant.ChannelTypeGemini:
		apiKey := task.PrivateData.Key
		if apiKey == "" {
			logger.LogError(c.Request.Context(), fmt.Sprintf("Missing stored API key for Gemini task %s", taskID))
			videoProxyError(c, http.StatusInternalServerError, "server_error", "stored_credential_unavailable", "API key not stored for task")
			return
		}
		videoURL, err = getGeminiVideoURL(channel, task, apiKey)
		if err != nil {
			logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to resolve Gemini video URL for task %s: %s", taskID, err.Error()))
			videoProxyError(c, http.StatusBadGateway, "server_error", "video_url_unavailable", "Failed to resolve Gemini video URL")
			return
		}
		req.Header.Set("x-goog-api-key", apiKey)
	case constant.ChannelTypeVertexAi:
		videoURL, err = getVertexVideoURL(channel, task)
		if err != nil {
			logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to resolve Vertex video URL for task %s: %s", taskID, err.Error()))
			videoProxyError(c, http.StatusBadGateway, "server_error", "video_url_unavailable", "Failed to resolve Vertex video URL")
			return
		}
	case constant.ChannelTypeOpenAI, constant.ChannelTypeSora:
		videoURL = fmt.Sprintf("%s/v1/videos/%s/content", baseURL, task.GetUpstreamTaskID())
		req.Header.Set("Authorization", "Bearer "+channel.Key)
	default:
		// Video URL is stored in PrivateData.ResultURL (fallback to FailReason for old data)
		videoURL = task.GetResultURL()
	}

	videoURL = strings.TrimSpace(videoURL)
	if videoURL == "" {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Video URL is empty for task %s", taskID))
		videoProxyError(c, http.StatusBadGateway, "server_error", "video_content_unavailable", "Failed to fetch video content")
		return
	}

	if strings.HasPrefix(videoURL, "data:") {
		if err := writeVideoDataURL(c, videoURL); err != nil {
			logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to decode video data URL for task %s: %s", taskID, err.Error()))
			videoProxyError(c, http.StatusBadGateway, "server_error", "video_content_unavailable", "Failed to fetch video content")
		}
		return
	}

	var validateErr error
	if proxy == "" {
		validateErr = service.ValidateSSRFProtectedFetchURL(videoURL)
	} else {
		fetchSetting := system_setting.GetFetchSetting()
		validateErr = common.ValidateURLWithFetchSetting(videoURL, fetchSetting.EnableSSRFProtection, fetchSetting.AllowPrivateIp, fetchSetting.DomainFilterMode, fetchSetting.IpFilterMode, fetchSetting.DomainList, fetchSetting.IpList, fetchSetting.AllowedPorts, fetchSetting.ApplyIPFilterForDomain)
	}
	if validateErr != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Video URL blocked for task %s: %v", taskID, validateErr))
		videoProxyError(c, http.StatusForbidden, "server_error", "video_url_blocked", fmt.Sprintf("request blocked: %v", validateErr))
		return
	}

	req.URL, err = url.Parse(videoURL)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to parse URL %s: %s", videoURL, err.Error()))
		videoProxyError(c, http.StatusInternalServerError, "server_error", "proxy_request_failed", "Failed to create proxy request")
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to fetch video from %s: %s", videoURL, err.Error()))
		videoProxyError(c, http.StatusBadGateway, "server_error", "video_content_unavailable", "Failed to fetch video content")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Upstream returned status %d for %s", resp.StatusCode, videoURL))
		videoProxyError(c, http.StatusBadGateway, "server_error", "upstream_response_error",
			fmt.Sprintf("Upstream service returned status %d", resp.StatusCode))
		return
	}

	for key, values := range resp.Header {
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}

	c.Writer.Header().Set("Cache-Control", "public, max-age=86400")
	c.Writer.WriteHeader(resp.StatusCode)
	if _, err = io.Copy(c.Writer, resp.Body); err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Failed to stream video content: %s", err.Error()))
	}
}

func writeVideoDataURL(c *gin.Context, dataURL string) error {
	parts := strings.SplitN(dataURL, ",", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid data url")
	}

	header := parts[0]
	payload := parts[1]
	if !strings.HasPrefix(header, "data:") || !strings.Contains(header, ";base64") {
		return fmt.Errorf("unsupported data url")
	}

	mimeType := strings.TrimPrefix(header, "data:")
	mimeType = strings.TrimSuffix(mimeType, ";base64")
	if mimeType == "" {
		mimeType = "video/mp4"
	}

	videoBytes, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		videoBytes, err = base64.RawStdEncoding.DecodeString(payload)
		if err != nil {
			return err
		}
	}

	c.Writer.Header().Set("Content-Type", mimeType)
	c.Writer.Header().Set("Cache-Control", "public, max-age=86400")
	c.Writer.WriteHeader(http.StatusOK)
	_, err = c.Writer.Write(videoBytes)
	return err
}
