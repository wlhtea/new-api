package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

const (
	OpenCodeGoDefaultRiskRecheckConcurrency = 4
	OpenCodeGoMaxRiskRecheckConcurrency     = 16

	openCodeGoDefaultRiskProbeModel = "glm-5.2"
	openCodeGoRiskProbeTimeout      = 60 * time.Second
	openCodeGoRiskProbeBodyLimit    = int64(64 * 1024)
)

type OpenCodeGoRiskProbeResponse struct {
	StatusCode int
	Failure    *OpenCodeGoProviderFailure
}

type openCodeGoRiskProbeClient interface {
	Probe(ctx context.Context, apiKey string, modelID string) (OpenCodeGoRiskProbeResponse, error)
}

type openCodeGoRiskProbeFactory func(channelID int, identityUID string) (openCodeGoRiskProbeClient, error)

type openCodeGoHTTPRiskProbeClient struct {
	endpoint *url.URL
	client   *http.Client
}

type OpenCodeGoRiskRecheckService struct {
	probe        openCodeGoRiskProbeClient
	probeFactory openCodeGoRiskProbeFactory
	codec        *OpenCodeGoCredentialCodec
	now          func() time.Time
	rebuild      func(int) error
}

type OpenCodeGoRiskRecheckResult struct {
	ChannelID      int    `json:"channel_id"`
	WorkspaceUID   string `json:"workspace_uid"`
	Model          string `json:"model,omitempty"`
	Status         string `json:"status"`
	Blocked        bool   `json:"blocked"`
	UpstreamStatus int    `json:"upstream_status,omitempty"`
	ErrorType      string `json:"error_type,omitempty"`
	Error          string `json:"error,omitempty"`
}

type OpenCodeGoRiskRecheckSummary struct {
	Total     int                           `json:"total"`
	Processed int                           `json:"processed"`
	Recovered int                           `json:"recovered"`
	Blocked   int                           `json:"blocked"`
	Failed    int                           `json:"failed"`
	Results   []OpenCodeGoRiskRecheckResult `json:"results"`
}

type openCodeGoRiskProbeRequest struct {
	Model     string                       `json:"model"`
	Messages  []openCodeGoRiskProbeMessage `json:"messages"`
	MaxTokens int                          `json:"max_tokens"`
	Stream    bool                         `json:"stream"`
}

type openCodeGoRiskProbeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openCodeGoIndexedRiskTarget struct {
	index  int
	target model.OpenCodeGoRiskRecheckTarget
}

type openCodeGoIndexedRiskResult struct {
	index  int
	result OpenCodeGoRiskRecheckResult
}

func NewConfiguredOpenCodeGoRiskRecheckService() (*OpenCodeGoRiskRecheckService, error) {
	codec, err := NewConfiguredOpenCodeGoCredentialCodec()
	if err != nil {
		return nil, err
	}
	return &OpenCodeGoRiskRecheckService{
		probeFactory: func(channelID int, identityUID string) (openCodeGoRiskProbeClient, error) {
			baseClient, err := GetOpenCodeGoIdentityHTTPClient(channelID, identityUID)
			if err != nil {
				return nil, err
			}
			return newOpenCodeGoHTTPRiskProbeClient(openCodeGoInferenceOrigin, baseClient)
		},
		codec:   codec,
		now:     time.Now,
		rebuild: ReconcileOpenCodeGoPoolChannel,
	}, nil
}

func newOpenCodeGoRiskRecheckService(
	probe openCodeGoRiskProbeClient,
	codec *OpenCodeGoCredentialCodec,
) *OpenCodeGoRiskRecheckService {
	return &OpenCodeGoRiskRecheckService{
		probe:   probe,
		codec:   codec,
		now:     time.Now,
		rebuild: ReconcileOpenCodeGoPoolChannel,
	}
}

func (service *OpenCodeGoRiskRecheckService) scopedForIdentity(channelID int, identityUID string) (*OpenCodeGoRiskRecheckService, error) {
	if service == nil || service.codec == nil {
		return nil, errors.New("OpenCode Go risk recheck service is not configured")
	}
	if service.probe != nil {
		return service, nil
	}
	if service.probeFactory == nil {
		return nil, errors.New("OpenCode Go risk recheck service is not configured")
	}
	probe, err := service.probeFactory(channelID, identityUID)
	if err != nil {
		return nil, err
	}
	scoped := *service
	scoped.probe = probe
	scoped.probeFactory = nil
	return &scoped, nil
}

func newOpenCodeGoHTTPRiskProbeClient(baseURL string, baseClient *http.Client) (*openCodeGoHTTPRiskProbeClient, error) {
	endpoint, err := url.Parse(strings.TrimRight(baseURL, "/") + "/chat/completions")
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, errors.New("OpenCode Go risk probe endpoint is invalid")
	}
	transport := http.DefaultTransport
	if baseClient != nil && baseClient.Transport != nil {
		transport = baseClient.Transport
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   openCodeGoRiskProbeTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &openCodeGoHTTPRiskProbeClient{endpoint: endpoint, client: client}, nil
}

func (client *openCodeGoHTTPRiskProbeClient) Probe(
	ctx context.Context,
	apiKey string,
	modelID string,
) (OpenCodeGoRiskProbeResponse, error) {
	if client == nil || client.endpoint == nil || client.client == nil {
		return OpenCodeGoRiskProbeResponse{}, errors.New("OpenCode Go risk probe client is not configured")
	}
	payload := openCodeGoRiskProbeRequest{
		Model:     modelID,
		Messages:  []openCodeGoRiskProbeMessage{{Role: "user", Content: "Reply OK"}},
		MaxTokens: 1,
		Stream:    false,
	}
	body, err := common.Marshal(payload)
	if err != nil {
		return OpenCodeGoRiskProbeResponse{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return OpenCodeGoRiskProbeResponse{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-opencode-client", "new-api-risk-probe")

	response, err := client.client.Do(request)
	if err != nil {
		return OpenCodeGoRiskProbeResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, openCodeGoRiskProbeBodyLimit))
		return OpenCodeGoRiskProbeResponse{StatusCode: response.StatusCode}, nil
	}
	errorBody, readErr := io.ReadAll(io.LimitReader(response.Body, openCodeGoRiskProbeBodyLimit+1))
	if readErr != nil {
		return OpenCodeGoRiskProbeResponse{}, errors.New("failed to read OpenCode Go risk probe response")
	}
	if int64(len(errorBody)) > openCodeGoRiskProbeBodyLimit {
		errorBody = errorBody[:openCodeGoRiskProbeBodyLimit]
	}
	failure := ParseOpenCodeGoProviderFailure(response.StatusCode, response.Header, errorBody)
	return OpenCodeGoRiskProbeResponse{StatusCode: response.StatusCode, Failure: &failure}, nil
}

func (service *OpenCodeGoRiskRecheckService) RecheckWorkspace(
	ctx context.Context,
	channelID int,
	workspaceUID string,
	source string,
) (result OpenCodeGoRiskRecheckResult, resultErr error) {
	result = OpenCodeGoRiskRecheckResult{
		ChannelID:    channelID,
		WorkspaceUID: strings.TrimSpace(workspaceUID),
		Status:       "failed",
	}
	if service == nil || service.codec == nil || service.now == nil {
		return result, errors.New("OpenCode Go risk recheck service is not configured")
	}
	if err := validateOpenCodeGoPoolChannel(channelID); err != nil {
		return result, err
	}
	workspace, err := model.GetOpenCodeGoWorkspace(channelID, result.WorkspaceUID)
	if err != nil {
		return result, err
	}
	if workspace == nil {
		return result, gorm.ErrRecordNotFound
	}
	identity, err := model.GetOpenCodeGoIdentityByID(channelID, workspace.IdentityID)
	if err != nil {
		return result, err
	}
	if identity == nil {
		return result, gorm.ErrRecordNotFound
	}
	scoped, err := service.scopedForIdentity(channelID, identity.UID)
	if err != nil {
		return result, err
	}
	service = scoped
	if service.probe == nil {
		return result, errors.New("OpenCode Go risk recheck service is not configured")
	}
	unlock := lockOpenCodeGoIdentityOperation(fmt.Sprintf("%d:identity:%s", channelID, identity.UID))
	defer unlock()

	workspace, err = model.GetOpenCodeGoWorkspace(channelID, workspace.UID)
	if err != nil {
		return result, err
	}
	if workspace == nil {
		return result, gorm.ErrRecordNotFound
	}
	if workspace.EffectiveState != model.OpenCodeGoStateRiskBlocked {
		return result, errors.New("OpenCode Go workspace is not risk blocked")
	}
	modelID, err := selectOpenCodeGoRiskProbeModel(*workspace, service.now())
	if err != nil {
		return result, err
	}
	result.Model = modelID
	apiKey, err := service.codec.Decrypt(
		OpenCodeGoCredentialAPIKey,
		channelID,
		workspace.UID,
		workspace.APIKeyCiphertext,
	)
	if err != nil || strings.TrimSpace(apiKey) == "" {
		return result, ErrOpenCodeGoSelectedCredentialUnavailable
	}

	operation, err := startOpenCodeGoOperation(
		channelID,
		*workspace,
		OpenCodeGoOperationRiskRecheck,
		source,
		service.now(),
	)
	if err != nil {
		return result, err
	}
	defer func() {
		status := OpenCodeGoOperationStatusSucceeded
		if resultErr != nil {
			status = OpenCodeGoOperationStatusFailed
		}
		operationResult := result.Status
		if result.UpstreamStatus > 0 {
			operationResult = fmt.Sprintf("%s:status_%d", result.Status, result.UpstreamStatus)
		}
		if finishErr := finishOpenCodeGoOperation(operation, status, operationResult, resultErr, service.now()); finishErr != nil {
			resultErr = errors.Join(resultErr, finishErr)
		}
	}()

	probeResponse, err := service.probe.Probe(ctx, apiKey, modelID)
	observedAt := service.now()
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		result.Error = sanitizeOpenCodeGoError(err, apiKey)
		if applyErr := service.applyRiskProbeTransportFailure(channelID, *workspace, modelID, result.Error, observedAt); applyErr != nil {
			return result, applyErr
		}
		return result, errors.New(result.Error)
	}
	result.UpstreamStatus = probeResponse.StatusCode
	if probeResponse.StatusCode >= http.StatusOK && probeResponse.StatusCode < http.StatusMultipleChoices {
		applied, applyErr := applyOpenCodeGoClassifiedFailure(
			channelID,
			workspace.UID,
			modelID,
			OpenCodeGoClassifiedFailure{
				Scope: OpenCodeGoHealthScopeWorkspace,
				Observation: OpenCodeGoHealthObservation{
					Kind:            OpenCodeGoObservationRiskProbeSucceeded,
					ObservedAt:      observedAt,
					HasUsableModels: hasUsableOpenCodeGoModels(nil, false, workspace),
				},
			},
			service.rebuild,
		)
		if applyErr != nil {
			return result, applyErr
		}
		if !applied {
			return result, errors.New("OpenCode Go risk recovery evidence was not applied")
		}
		result.Status = "recovered"
		return result, nil
	}

	if probeResponse.Failure == nil {
		failure := ParseOpenCodeGoProviderFailure(probeResponse.StatusCode, nil, nil)
		probeResponse.Failure = &failure
	}
	result.ErrorType = probeResponse.Failure.ErrorType
	result.Error = probeResponse.Failure.Message
	classified, classifiedOK := ClassifyOpenCodeGoProviderFailure(*probeResponse.Failure, observedAt)
	restrictive := classifiedOK && isRestrictiveOpenCodeGoHealthObservation(classified.Observation.Kind)
	releaseMutation := func() {}
	if restrictive {
		releaseMutation = BeginOpenCodeGoPoolMutation(channelID)
	}
	defer releaseMutation()
	appliedRestrictive := false
	if classifiedOK {
		result.Blocked = classified.Observation.Kind == OpenCodeGoObservationRiskBlocked
		applied, applyErr := applyOpenCodeGoClassifiedFailureWithMutation(
			channelID,
			workspace.UID,
			modelID,
			classified,
			nil,
			restrictive,
		)
		if applyErr != nil {
			return result, applyErr
		}
		appliedRestrictive = restrictive && applied
	}
	appliedProbeFailure, applyErr := applyOpenCodeGoClassifiedFailureWithMutation(
		channelID,
		workspace.UID,
		modelID,
		OpenCodeGoClassifiedFailure{
			Scope: OpenCodeGoHealthScopeWorkspace,
			Observation: OpenCodeGoHealthObservation{
				Kind:       OpenCodeGoObservationRiskProbeFailed,
				ObservedAt: observedAt,
				Reason:     result.Error,
			},
		},
		nil,
		restrictive,
	)
	if applyErr != nil {
		return result, applyErr
	}
	if appliedRestrictive {
		InvalidateOpenCodeGoIdentityProxyChannel(channelID)
	}
	if (appliedRestrictive || appliedProbeFailure) && service.rebuild != nil {
		if rebuildErr := service.rebuild(channelID); rebuildErr != nil {
			return result, rebuildErr
		}
	}
	if result.Blocked {
		result.Status = "blocked"
	} else {
		result.Status = "not_recovered"
	}
	return result, nil
}

func (service *OpenCodeGoRiskRecheckService) applyRiskProbeTransportFailure(
	channelID int,
	workspace model.OpenCodeGoWorkspace,
	modelID string,
	reason string,
	observedAt time.Time,
) error {
	classified := ClassifyOpenCodeGoTransportFailure(reason, observedAt)
	releaseMutation := BeginOpenCodeGoPoolMutation(channelID)
	defer releaseMutation()
	modelApplied, modelErr := applyOpenCodeGoClassifiedFailureWithMutation(
		channelID,
		workspace.UID,
		modelID,
		classified,
		nil,
		true,
	)
	workspaceApplied, workspaceErr := applyOpenCodeGoClassifiedFailureWithMutation(
		channelID,
		workspace.UID,
		modelID,
		OpenCodeGoClassifiedFailure{
			Scope: OpenCodeGoHealthScopeWorkspace,
			Observation: OpenCodeGoHealthObservation{
				Kind:       OpenCodeGoObservationRiskProbeFailed,
				ObservedAt: observedAt,
				Reason:     reason,
			},
		},
		nil,
		true,
	)
	if err := errors.Join(modelErr, workspaceErr); err != nil {
		return err
	}
	if !modelApplied && !workspaceApplied {
		return nil
	}
	InvalidateOpenCodeGoIdentityProxyChannel(channelID)
	if service.rebuild == nil {
		return nil
	}
	return service.rebuild(channelID)
}

func (service *OpenCodeGoRiskRecheckService) RecheckRiskWorkspaces(
	ctx context.Context,
	channelID int,
	concurrency int,
	limit int,
	source string,
	reportProgress func(processed, total int),
) (OpenCodeGoRiskRecheckSummary, error) {
	targets, err := model.ListOpenCodeGoRiskRecheckTargets(channelID, limit)
	if err != nil {
		return OpenCodeGoRiskRecheckSummary{}, err
	}
	summary := OpenCodeGoRiskRecheckSummary{
		Total:   len(targets),
		Results: make([]OpenCodeGoRiskRecheckResult, 0, len(targets)),
	}
	if reportProgress != nil {
		reportProgress(0, summary.Total)
	}
	if len(targets) == 0 {
		return summary, nil
	}
	concurrency = normalizeOpenCodeGoRiskRecheckConcurrency(concurrency, len(targets))

	jobs := make(chan openCodeGoIndexedRiskTarget)
	outcomes := make(chan openCodeGoIndexedRiskResult)
	var workers sync.WaitGroup
	workers.Add(concurrency)
	for workerIndex := 0; workerIndex < concurrency; workerIndex++ {
		go func() {
			defer workers.Done()
			for job := range jobs {
				result, recheckErr := service.RecheckWorkspace(
					ctx,
					job.target.ChannelID,
					job.target.WorkspaceUID,
					source,
				)
				if recheckErr != nil {
					result.Status = "failed"
					result.Error = sanitizeOpenCodeGoError(recheckErr)
				}
				outcomes <- openCodeGoIndexedRiskResult{index: job.index, result: result}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for index, target := range targets {
			select {
			case <-ctx.Done():
				return
			case jobs <- openCodeGoIndexedRiskTarget{index: index, target: target}:
			}
		}
	}()
	go func() {
		workers.Wait()
		close(outcomes)
	}()

	indexedResults := make([]*OpenCodeGoRiskRecheckResult, len(targets))
	for outcome := range outcomes {
		result := outcome.result
		indexedResults[outcome.index] = &result
		summary.Processed++
		switch result.Status {
		case "recovered":
			summary.Recovered++
		case "blocked":
			summary.Blocked++
		default:
			summary.Failed++
		}
		if reportProgress != nil {
			reportProgress(summary.Processed, summary.Total)
		}
	}
	for _, result := range indexedResults {
		if result != nil {
			summary.Results = append(summary.Results, *result)
		}
	}
	if err := ctx.Err(); err != nil {
		return summary, fmt.Errorf("OpenCode Go risk recheck cancelled: %w", err)
	}
	return summary, nil
}

func selectOpenCodeGoRiskProbeModel(workspace model.OpenCodeGoWorkspace, now time.Time) (string, error) {
	preferred := strings.TrimSpace(common.GetEnvOrDefaultString(
		"OPENCODE_GO_RISK_PROBE_MODEL",
		openCodeGoDefaultRiskProbeModel,
	))
	if preferred == "" {
		preferred = openCodeGoDefaultRiskProbeModel
	}
	for _, entry := range workspace.Models {
		if entry.Model != preferred || !entry.Discovered {
			continue
		}
		if entry.State == model.OpenCodeGoModelAvailable && entry.DisabledUntil == 0 {
			return preferred, nil
		}
		if entry.DisabledUntil > 0 && entry.DisabledUntil <= now.Unix() {
			return preferred, nil
		}
	}
	return "", fmt.Errorf("OpenCode Go risk probe model %q is not available in the workspace", preferred)
}

func normalizeOpenCodeGoRiskRecheckConcurrency(concurrency int, targetCount int) int {
	if concurrency <= 0 {
		concurrency = OpenCodeGoDefaultRiskRecheckConcurrency
	}
	if concurrency > OpenCodeGoMaxRiskRecheckConcurrency {
		concurrency = OpenCodeGoMaxRiskRecheckConcurrency
	}
	if concurrency > targetCount {
		concurrency = targetCount
	}
	if concurrency < 1 {
		concurrency = 1
	}
	return concurrency
}
