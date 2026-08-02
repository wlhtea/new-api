package channel

import (
	"context"
	"io"
	"net/http"

	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
)

type Adaptor interface {
	// Init IsStream bool
	Init(info *relaycommon.RelayInfo)
	GetRequestURL(info *relaycommon.RelayInfo) (string, error)
	SetupRequestHeader(c *gin.Context, req *http.Header, info *relaycommon.RelayInfo) error
	ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error)
	ConvertRerankRequest(c *gin.Context, relayMode int, request dto.RerankRequest) (any, error)
	ConvertEmbeddingRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.EmbeddingRequest) (any, error)
	ConvertAudioRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.AudioRequest) (io.Reader, error)
	ConvertImageRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (any, error)
	ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest) (any, error)
	DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error)
	DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError)
	GetModelList() []string
	GetChannelName() string
	ConvertClaudeRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.ClaudeRequest) (any, error)
	ConvertGeminiRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeminiChatRequest) (any, error)
}

const ContextKeyNon2xxResponseObservation = "non_2xx_response_observation"

// Non2xxResponseObservation is a secret-free provider error summary. Provider
// adaptors may attach one to the request context for health reducers without
// retaining the upstream response body.
type Non2xxResponseObservation struct {
	Provider   string `json:"provider"`
	StatusCode int    `json:"status_code"`
	ErrorType  string `json:"error_type,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
	Message    string `json:"message,omitempty"`
	RetryAfter string `json:"retry_after,omitempty"`
	LimitName  string `json:"limit_name,omitempty"`
}

// Non2xxResponseHandler lets a provider preserve structured failure metadata
// before the generic relay error handler consumes the response body.
type Non2xxResponseHandler interface {
	HandleNon2xxResponse(
		c *gin.Context,
		resp *http.Response,
		info *relaycommon.RelayInfo,
	) (*types.NewAPIError, *Non2xxResponseObservation)
}

func TryHandleNon2xxResponse(
	c *gin.Context,
	adaptor Adaptor,
	resp *http.Response,
	info *relaycommon.RelayInfo,
) (*types.NewAPIError, bool) {
	handler, ok := adaptor.(Non2xxResponseHandler)
	if !ok {
		return nil, false
	}
	err, observation := handler.HandleNon2xxResponse(c, resp, info)
	if observation != nil && c != nil {
		c.Set(ContextKeyNon2xxResponseObservation, observation)
	}
	return err, err != nil
}

type TaskAdaptor interface {
	Init(info *relaycommon.RelayInfo)

	ValidateRequestAndSetAction(c *gin.Context, info *relaycommon.RelayInfo) *taskdto.TaskError

	// ── Billing ──────────────────────────────────────────────────────

	// EstimateBilling returns OtherRatios for pre-charge based on user request.
	// Called after ValidateRequestAndSetAction, before price calculation.
	// Adaptors should extract duration, resolution, etc. from the parsed request
	// and return them as ratio multipliers (e.g. {"seconds": 5, "size": 1.666}).
	// Return nil to use the base model price without extra ratios.
	EstimateBilling(c *gin.Context, info *relaycommon.RelayInfo) map[string]float64

	// AdjustBillingOnSubmit returns adjusted OtherRatios from the upstream
	// submit response. Called after a successful DoResponse.
	// If the upstream returned actual parameters that differ from the estimate
	// (e.g. actual seconds), return updated ratios so the caller can recalculate
	// the quota and settle the delta with the pre-charge.
	// Return nil if no adjustment is needed.
	AdjustBillingOnSubmit(info *relaycommon.RelayInfo, taskData []byte) map[string]float64

	// AdjustBillingOnComplete returns the actual quota when a task reaches a
	// terminal state (success/failure) during polling.
	// Called by the polling loop after ParseTaskResult.
	// Return a positive value to trigger delta settlement (supplement / refund).
	// Return 0 to keep the pre-charged amount unchanged.
	AdjustBillingOnComplete(task *model.Task, taskResult *relaycommon.TaskInfo) int

	// ── Request / Response ───────────────────────────────────────────

	BuildRequestURL(info *relaycommon.RelayInfo) (string, error)
	BuildRequestHeader(c *gin.Context, req *http.Request, info *relaycommon.RelayInfo) error
	BuildRequestBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, error)

	DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error)
	DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (taskID string, taskData []byte, err *taskdto.TaskError)

	GetModelList() []string
	GetChannelName() string

	// ── Polling ──────────────────────────────────────────────────────

	FetchTask(baseUrl, key string, body map[string]any, proxy string) (*http.Response, error)
	ParseTaskResult(respBody []byte) (*relaycommon.TaskInfo, error)
}

type TaskSubmitHTTPResponse struct {
	StatusCode      int
	Body            any
	InitialStatus   model.TaskStatus
	InitialProgress string
}

type DeferredTaskSubmitResponder interface {
	BuildTaskSubmitResponse(
		info *relaycommon.RelayInfo,
		taskData []byte,
	) (*TaskSubmitHTTPResponse, error)
}

type FullPrepaidTaskSubmitter interface {
	RequiresFullPrepaidBilling() bool
}

type DurableTaskSubmitter interface {
	RequiresDurableTaskBeforeSubmit() bool
}

type TaskSubmitFailureClassification struct {
	TaskError      *taskdto.TaskError
	UpstreamTaskID string
	TaskData       []byte
}

type TaskSubmitFailureClassifier interface {
	ClassifyTaskSubmitFailure(
		resp *http.Response,
		requestErr error,
	) *TaskSubmitFailureClassification
}

type VideoContent struct {
	ContentType   string
	ContentLength int64
	Body          io.ReadCloser
}

type VideoContentFetcher interface {
	FetchVideoContent(
		ctx context.Context,
		baseURL string,
		key string,
		upstreamTaskID string,
		proxy string,
	) (*VideoContent, error)
}

type VideoContentError struct {
	StatusCode int
	Type       string
	Code       string
	Message    string
	Cause      error
}

func (e *VideoContentError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Code
}

func (e *VideoContentError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type OpenAIVideoConverter interface {
	ConvertToOpenAIVideo(originTask *model.Task) ([]byte, error)
}
