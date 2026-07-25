package seedance

import (
	"bytes"
	"context"
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
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	taskcommon "github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

type TaskAdaptor struct {
	taskcommon.BaseBilling
	apiKey  string
	baseURL string
}

type cleanedStatusData struct {
	RequestID  string `json:"requestId,omitempty"`
	Success    *bool  `json:"success,omitempty"`
	ErrCode    string `json:"errCode,omitempty"`
	ErrMessage string `json:"errMessage,omitempty"`
	Status     string `json:"status,omitempty"`
	Message    string `json:"message,omitempty"`
}

func (a *TaskAdaptor) Init(info *relaycommon.RelayInfo) {
	a.apiKey = info.ApiKey
	a.baseURL = strings.TrimRight(info.ChannelBaseUrl, "/")
}

func (*TaskAdaptor) RequiresFullPrepaidBilling() bool {
	return true
}

func (*TaskAdaptor) RequiresDurableTaskBeforeSubmit() bool {
	return true
}

func (*TaskAdaptor) GetModelList() []string {
	return []string{ModelName}
}

func (*TaskAdaptor) GetChannelName() string {
	return ChannelName
}

func (a *TaskAdaptor) ValidateRequestAndSetAction(
	c *gin.Context,
	info *relaycommon.RelayInfo,
) *dto.TaskError {
	normalized, taskErr := normalizeRequest(c)
	if taskErr != nil {
		return taskErr
	}
	if info.OriginModelName != ModelName {
		return service.TaskErrorWrapperLocal(
			fmt.Errorf("unsupported model %q", info.OriginModelName),
			"model_not_supported",
			http.StatusBadRequest,
		)
	}
	info.Action = constant.TaskActionGenerate
	c.Set(normalizedRequestContextKey, normalized)
	return nil
}

func (a *TaskAdaptor) BuildRequestURL(
	_ *relaycommon.RelayInfo,
) (string, error) {
	return a.baseURL + "/generate", nil
}

func (a *TaskAdaptor) BuildRequestHeader(
	_ *gin.Context,
	req *http.Request,
	_ *relaycommon.RelayInfo,
) error {
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return nil
}

func (a *TaskAdaptor) BuildRequestBody(
	c *gin.Context,
	info *relaycommon.RelayInfo,
) (io.Reader, error) {
	normalized, err := getNormalizedRequest(c)
	if err != nil {
		return nil, err
	}
	payload := generateRequest{
		Prompt:             normalized.Prompt,
		ImageBase64:        normalized.ImageBase64,
		Duration:           normalized.Duration,
		Resolution:         normalized.Resolution,
		PromptOptimization: normalized.PromptOptimization,
		MultiShot:          normalized.MultiShot,
		StrictDuration:     normalized.StrictDuration,
		NegativePrompt:     normalized.NegativePrompt,
	}
	body, err := common.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal Seed Dance generate request: %w", err)
	}
	info.UpstreamRequestBodySize = int64(len(body))
	return bytes.NewReader(body), nil
}

func (a *TaskAdaptor) DoRequest(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	requestBody io.Reader,
) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), submitTimeout)
	requestURL, err := a.BuildRequestURL(info)
	if err != nil {
		cancel()
		return nil, err
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		requestURL,
		requestBody,
	)
	if err != nil {
		cancel()
		return nil, err
	}
	if info.UpstreamRequestBodySize > 0 {
		req.ContentLength = info.UpstreamRequestBodySize
	}
	if err := a.BuildRequestHeader(c, req, info); err != nil {
		cancel()
		return nil, err
	}

	proxy := info.ChannelSetting.Proxy
	baseClient, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		cancel()
		return nil, err
	}
	client, err := newStageClient(baseClient, proxy, connectTimeout)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	return bindCancelToBody(resp, cancel), nil
}

func submitOutcomeUnknown(cause error) *dto.TaskError {
	retryable := false
	return &dto.TaskError{
		Code:       "seedance_submit_outcome_unknown",
		Message:    "upstream submission result is unknown",
		StatusCode: http.StatusBadGateway,
		LocalError: false,
		Error:      cause,
		Retryable:  &retryable,
	}
}

func nonRetryableTaskError(
	code string,
	message string,
	statusCode int,
	cause error,
) *dto.TaskError {
	retryable := false
	return &dto.TaskError{
		Code:       code,
		Message:    message,
		StatusCode: statusCode,
		LocalError: false,
		Error:      cause,
		Retryable:  &retryable,
	}
}

func rawErrorCode(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var value string
	if err := common.Unmarshal(trimmed, &value); err == nil {
		return value
	}
	var number jsonNumber
	if err := common.Unmarshal(trimmed, &number); err == nil {
		return string(number)
	}
	return ""
}

type jsonNumber string

func (n *jsonNumber) UnmarshalJSON(data []byte) error {
	text := string(bytes.TrimSpace(data))
	if text == "" {
		return errors.New("empty JSON number")
	}
	for index, char := range text {
		if (char < '0' || char > '9') && !(index == 0 && char == '-') {
			return errors.New("not a JSON integer")
		}
	}
	*n = jsonNumber(text)
	return nil
}

func cleanedProviderData(provider *providerEnvelope) cleanedTaskData {
	return cleanedTaskData{
		RequestID:  provider.RequestID,
		Success:    provider.Success,
		ErrCode:    rawErrorCode(provider.ErrCode),
		ErrMessage: provider.ErrMessage,
		Status:     provider.Status,
		Message:    provider.Message,
	}
}

func cleanedSubmitData(
	c *gin.Context,
	provider *providerEnvelope,
) ([]byte, error) {
	cleaned := cleanedProviderData(provider)
	cleaned.Model = ModelName
	if normalized, err := getNormalizedRequest(c); err == nil {
		cleaned.Seconds = strconv.Itoa(normalized.Duration)
		cleaned.Size = normalized.Resolution
	}
	return common.Marshal(cleaned)
}

func explicitBusinessFailure(provider *providerEnvelope) bool {
	return provider.Success != nil && !*provider.Success ||
		strings.EqualFold(provider.Status, "failed")
}

func providerErrorMessage(provider *providerEnvelope) string {
	if provider.ErrMessage != "" {
		return provider.ErrMessage
	}
	if provider.Message != "" {
		return provider.Message
	}
	if code := rawErrorCode(provider.ErrCode); code != "" {
		return "upstream error " + code
	}
	return "upstream business error"
}

func (a *TaskAdaptor) DoResponse(
	c *gin.Context,
	resp *http.Response,
	_ *relaycommon.RelayInfo,
) (taskID string, taskData []byte, taskErr *dto.TaskError) {
	if resp == nil || resp.Body == nil {
		return "", nil, submitOutcomeUnknown(errors.New("empty upstream response"))
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, submitOutcomeUnknown(err)
	}
	var provider providerEnvelope
	if err := common.Unmarshal(responseBody, &provider); err != nil {
		return "", nil, submitOutcomeUnknown(err)
	}

	taskData, err = cleanedSubmitData(c, &provider)
	if err != nil {
		return "", nil, submitOutcomeUnknown(err)
	}
	if explicitBusinessFailure(&provider) {
		message := providerErrorMessage(&provider)
		return provider.TaskID, taskData, nonRetryableTaskError(
			"upstream_error",
			message,
			http.StatusBadGateway,
			errors.New(message),
		)
	}
	if provider.TaskID == "" {
		return "", nil, submitOutcomeUnknown(errors.New("upstream response has no task_id"))
	}
	return provider.TaskID, taskData, nil
}

func (a *TaskAdaptor) BuildTaskSubmitResponse(
	info *relaycommon.RelayInfo,
	taskData []byte,
) (*channel.TaskSubmitHTTPResponse, error) {
	body := dto.NewOpenAIVideo()
	body.ID = info.PublicTaskID
	body.TaskID = info.PublicTaskID
	body.Model = ModelName
	body.Status = dto.VideoStatusQueued
	body.Progress = 10
	body.CreatedAt = time.Now().Unix()
	var cleaned cleanedTaskData
	if err := common.Unmarshal(taskData, &cleaned); err != nil {
		return nil, fmt.Errorf("decode cleaned task data: %w", err)
	}
	body.Seconds = cleaned.Seconds
	body.Size = cleaned.Size
	return &channel.TaskSubmitHTTPResponse{
		StatusCode:      http.StatusOK,
		Body:            body,
		InitialStatus:   model.TaskStatusSubmitted,
		InitialProgress: taskcommon.ProgressSubmitted,
	}, nil
}

func dataIsEmpty(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return true
	}
	var value any
	if err := common.Unmarshal(trimmed, &value); err != nil {
		return false
	}
	switch data := value.(type) {
	case string:
		return data == ""
	case []any:
		return len(data) == 0
	case map[string]any:
		return len(data) == 0
	default:
		return false
	}
}

func isRateLimitCode(raw []byte) bool {
	code := strings.ToLower(strings.TrimSpace(rawErrorCode(raw)))
	return code == "429" ||
		code == "rate_limit" ||
		code == "rate_limited" ||
		code == "rate_limit_error" ||
		code == "too_many_requests"
}

func marshalCleanedProvider(provider *providerEnvelope) []byte {
	if provider == nil {
		return nil
	}
	taskData, err := common.Marshal(cleanedProviderData(provider))
	if err != nil {
		return nil
	}
	return taskData
}

func (a *TaskAdaptor) ClassifyTaskSubmitFailure(
	resp *http.Response,
	requestErr error,
) *channel.TaskSubmitFailureClassification {
	if requestErr != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return &channel.TaskSubmitFailureClassification{
			TaskError: submitOutcomeUnknown(requestErr),
		}
	}
	if resp == nil || resp.Body == nil {
		return &channel.TaskSubmitFailureClassification{
			TaskError: submitOutcomeUnknown(errors.New("empty upstream response")),
		}
	}
	defer resp.Body.Close()

	responseBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return &channel.TaskSubmitFailureClassification{
			TaskError: submitOutcomeUnknown(readErr),
		}
	}

	var provider providerEnvelope
	decodeErr := common.Unmarshal(responseBody, &provider)
	classification := &channel.TaskSubmitFailureClassification{}
	if decodeErr == nil {
		classification.UpstreamTaskID = provider.TaskID
		classification.TaskData = marshalCleanedProvider(&provider)
	}

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		classification.TaskError = nonRetryableTaskError(
			"upstream_authentication_error",
			"upstream authentication failed",
			resp.StatusCode,
			errors.New("upstream authentication failed"),
		)
		return classification
	case http.StatusTooManyRequests:
		if decodeErr == nil &&
			provider.Success != nil &&
			!*provider.Success &&
			isRateLimitCode(provider.ErrCode) &&
			provider.TaskID == "" &&
			dataIsEmpty(provider.Data) {
			retryable := true
			classification.TaskError = &dto.TaskError{
				Code:       "upstream_rate_limit_error",
				Message:    providerErrorMessage(&provider),
				StatusCode: http.StatusTooManyRequests,
				LocalError: false,
				Error:      errors.New(providerErrorMessage(&provider)),
				Retryable:  &retryable,
			}
			return classification
		}
		classification.TaskError = submitOutcomeUnknown(
			fmt.Errorf("ambiguous upstream 429 response"),
		)
		return classification
	case http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		classification.TaskError = submitOutcomeUnknown(
			fmt.Errorf("upstream returned HTTP %d", resp.StatusCode),
		)
		return classification
	}

	if decodeErr != nil {
		classification.TaskError = submitOutcomeUnknown(decodeErr)
		return classification
	}
	if !explicitBusinessFailure(&provider) {
		classification.TaskError = submitOutcomeUnknown(
			fmt.Errorf("ambiguous upstream HTTP %d response", resp.StatusCode),
		)
		return classification
	}
	message := providerErrorMessage(&provider)
	classification.TaskError = nonRetryableTaskError(
		"upstream_error",
		message,
		resp.StatusCode,
		errors.New(message),
	)
	return classification
}

func (a *TaskAdaptor) FetchTaskWithContext(
	parent context.Context,
	baseURL string,
	key string,
	body map[string]any,
	proxy string,
) (*http.Response, error) {
	taskID, _ := body["task_id"].(string)
	if taskID == "" {
		return nil, errors.New("task_id is required")
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, statusTimeout)
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		strings.TrimRight(baseURL, "/")+"/status/"+url.PathEscape(taskID),
		nil,
	)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	baseClient, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		cancel()
		return nil, err
	}
	client, err := newStageClient(baseClient, proxy, connectTimeout)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp == nil || resp.Body == nil {
		cancel()
		return nil, errors.New("empty Seed Dance status response")
	}
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(
			"Seed Dance status request returned HTTP %d",
			resp.StatusCode,
		)
	}
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read Seed Dance status response: %w", err)
	}
	var provider providerEnvelope
	if err := common.Unmarshal(responseBody, &provider); err != nil {
		return nil, fmt.Errorf("decode Seed Dance status response: %w", err)
	}
	if provider.Success != nil &&
		!*provider.Success &&
		!strings.EqualFold(provider.Status, "failed") {
		return nil, errors.New("Seed Dance status business request failed")
	}
	cleaned := cleanedStatusData{
		RequestID:  provider.RequestID,
		Success:    provider.Success,
		ErrCode:    rawErrorCode(provider.ErrCode),
		ErrMessage: provider.ErrMessage,
		Status:     provider.Status,
		Message:    provider.Message,
	}
	cleanedBody, err := common.Marshal(cleaned)
	if err != nil {
		return nil, fmt.Errorf("encode cleaned Seed Dance status: %w", err)
	}
	if _, err := a.ParseTaskResult(cleanedBody); err != nil {
		return nil, err
	}
	result := *resp
	result.Body = io.NopCloser(bytes.NewReader(cleanedBody))
	result.ContentLength = int64(len(cleanedBody))
	result.Header = resp.Header.Clone()
	result.Header.Set("Content-Length", strconv.Itoa(len(cleanedBody)))
	return &result, nil
}

func (a *TaskAdaptor) FetchTask(
	baseURL string,
	key string,
	body map[string]any,
	proxy string,
) (*http.Response, error) {
	return a.FetchTaskWithContext(
		context.Background(),
		baseURL,
		key,
		body,
		proxy,
	)
}

func (a *TaskAdaptor) ParseTaskResult(
	respBody []byte,
) (*relaycommon.TaskInfo, error) {
	var provider providerEnvelope
	if err := common.Unmarshal(respBody, &provider); err != nil {
		return nil, fmt.Errorf("decode Seed Dance task status: %w", err)
	}
	info := &relaycommon.TaskInfo{}
	switch strings.ToLower(provider.Status) {
	case "accepted":
		info.Status, info.Progress = model.TaskStatusSubmitted, "10%"
	case "queued":
		info.Status, info.Progress = model.TaskStatusQueued, "20%"
	case "running", "processing":
		info.Status, info.Progress = model.TaskStatusInProgress, "30%"
	case "completed":
		info.Status, info.Progress = model.TaskStatusSuccess, "100%"
	case "failed":
		info.Status, info.Progress = model.TaskStatusFailure, "100%"
		if provider.ErrMessage != "" {
			info.Reason = provider.ErrMessage
		} else {
			info.Reason = provider.Message
		}
	default:
		return nil, fmt.Errorf("unknown Seed Dance status %q", provider.Status)
	}
	return info, nil
}

func resolutionLabel(ratio float64) string {
	for label, candidate := range resolutionRatios {
		if ratio == candidate {
			return label
		}
	}
	return ""
}

func (a *TaskAdaptor) ConvertToOpenAIVideo(
	originTask *model.Task,
) ([]byte, error) {
	video := dto.NewOpenAIVideo()
	video.ID = originTask.TaskID
	video.TaskID = originTask.TaskID
	video.Model = ModelName
	video.Status = originTask.Status.ToVideoStatus()
	video.SetProgressStr(originTask.Progress)
	video.CreatedAt = originTask.CreatedAt
	if originTask.FinishTime > 0 {
		video.CompletedAt = originTask.FinishTime
	} else if originTask.UpdatedAt > 0 {
		video.CompletedAt = originTask.UpdatedAt
	}

	if billing := originTask.PrivateData.BillingContext; billing != nil {
		if billing.OriginModelName != "" {
			video.Model = billing.OriginModelName
		}
		if seconds, ok := billing.OtherRatios["seconds"]; ok {
			video.Seconds = strconv.FormatFloat(seconds, 'f', -1, 64)
		}
		if resolution, ok := billing.OtherRatios["resolution"]; ok {
			video.Size = resolutionLabel(resolution)
		}
	}
	return common.Marshal(video)
}
