package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
)

const (
	openCodeGoConsoleOrigin       = "https://opencode.ai"
	openCodeGoInferenceOrigin     = "https://opencode.ai/zen/go/v1"
	openCodeGoConsoleRequestLimit = 20 * time.Second
	openCodeGoConsoleBodyLimit    = int64(openCodeGoMaxSSRDocumentSize)
	openCodeGoModelsBodyLimit     = int64(2 * 1024 * 1024)
	openCodeGoConsoleUserAgent    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

var ErrOpenCodeGoAuthenticationInvalid = errors.New("OpenCode Go console authentication is invalid")

type OpenCodeGoWorkspacePageResult struct {
	Workspace OpenCodeGoDiscoveredWorkspace
	Page      *OpenCodeGoConsolePage
	Error     error
}

type OpenCodeGoConsoleClient struct {
	consoleBase   *url.URL
	inferenceBase *url.URL
	followClient  *http.Client
	manualClient  *http.Client
	now           func() time.Time
}

func newOpenCodeGoChannelConsoleClient(channelID int) (*OpenCodeGoConsoleClient, error) {
	baseClient, err := getOpenCodeGoChannelHTTPClient(channelID)
	if err != nil {
		return nil, err
	}
	return newOpenCodeGoConsoleClient(openCodeGoConsoleOrigin, openCodeGoInferenceOrigin, baseClient)
}

func newOpenCodeGoIdentityConsoleClient(channelID int, identityUID string) (*OpenCodeGoConsoleClient, error) {
	baseClient, err := GetOpenCodeGoIdentityHTTPClient(channelID, identityUID)
	if err != nil {
		return nil, err
	}
	return newOpenCodeGoConsoleClient(openCodeGoConsoleOrigin, openCodeGoInferenceOrigin, baseClient)
}

func newOpenCodeGoConsoleClient(consoleBase string, inferenceBase string, baseClient *http.Client) (*OpenCodeGoConsoleClient, error) {
	consoleURL, err := url.Parse(consoleBase)
	if err != nil || consoleURL.Scheme == "" || consoleURL.Host == "" {
		return nil, errors.New("OpenCode Go console base URL is invalid")
	}
	inferenceURL, err := url.Parse(inferenceBase)
	if err != nil || inferenceURL.Scheme == "" || inferenceURL.Host == "" {
		return nil, errors.New("OpenCode Go inference base URL is invalid")
	}

	transport := http.DefaultTransport
	if baseClient != nil && baseClient.Transport != nil {
		transport = baseClient.Transport
	}
	followClient := &http.Client{
		Transport: transport,
		Timeout:   openCodeGoConsoleRequestLimit,
	}
	followClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if !sameOpenCodeGoOrigin(request.URL, consoleURL) {
			request.Header.Del("Cookie")
			return errors.New("OpenCode Go console redirect left the allowed origin")
		}
		if len(via) >= 5 {
			return errors.New("OpenCode Go console redirect limit exceeded")
		}
		return nil
	}
	manualClient := &http.Client{
		Transport: transport,
		Timeout:   openCodeGoConsoleRequestLimit,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return &OpenCodeGoConsoleClient{
		consoleBase:   consoleURL,
		inferenceBase: inferenceURL,
		followClient:  followClient,
		manualClient:  manualClient,
		now:           time.Now,
	}, nil
}

func (client *OpenCodeGoConsoleClient) DiscoverWorkspacePages(
	ctx context.Context,
	authCookie string,
	cachedWorkspaceID string,
) ([]OpenCodeGoWorkspacePageResult, error) {
	workspaceID := strings.TrimSpace(cachedWorkspaceID)
	var err error
	if workspaceID == "" {
		workspaceID, err = client.ResolveWorkspaceID(ctx, authCookie)
		if err != nil {
			return nil, err
		}
	}

	primary, err := client.FetchWorkspacePage(ctx, authCookie, workspaceID)
	if err != nil {
		return nil, err
	}
	primaryWorkspace := OpenCodeGoDiscoveredWorkspace{ID: primary.WorkspaceID, Name: primary.WorkspaceName}
	results := []OpenCodeGoWorkspacePageResult{{Workspace: primaryWorkspace, Page: primary}}
	seen := map[string]struct{}{strings.ToLower(primary.WorkspaceID): {}}
	for _, workspace := range primary.Workspaces {
		key := strings.ToLower(workspace.ID)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		page, pageErr := client.FetchWorkspacePage(ctx, authCookie, workspace.ID)
		results = append(results, OpenCodeGoWorkspacePageResult{
			Workspace: workspace,
			Page:      page,
			Error:     pageErr,
		})
	}
	return results, nil
}

func (client *OpenCodeGoConsoleClient) ResolveWorkspaceID(ctx context.Context, authCookie string) (string, error) {
	cookieHeader, err := BuildOpenCodeGoCookieHeader(authCookie)
	if err != nil {
		return "", err
	}
	request, err := client.newConsoleRequest(ctx, http.MethodGet, "/auth", cookieHeader)
	if err != nil {
		return "", err
	}
	response, err := client.manualClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("OpenCode Go auth request failed: %w", err)
	}
	defer response.Body.Close()

	if !isOpenCodeGoRedirectStatus(response.StatusCode) {
		return "", fmt.Errorf("%w (status %d)", ErrOpenCodeGoAuthenticationInvalid, response.StatusCode)
	}
	location, err := response.Location()
	if err != nil {
		return "", fmt.Errorf("%w: workspace redirect is missing", ErrOpenCodeGoAuthenticationInvalid)
	}
	if !sameOpenCodeGoOrigin(location, client.consoleBase) {
		return "", errors.New("OpenCode Go auth redirect is invalid")
	}
	workspaceID, ok := openCodeGoWorkspaceIDFromPath(location.Path, false)
	if !ok {
		return "", fmt.Errorf("%w: redirect did not identify a workspace", ErrOpenCodeGoAuthenticationInvalid)
	}
	return workspaceID, nil
}

func (client *OpenCodeGoConsoleClient) FetchWorkspacePage(
	ctx context.Context,
	authCookie string,
	workspaceID string,
) (*OpenCodeGoConsolePage, error) {
	if !openCodeGoWorkspaceIDPattern.MatchString(workspaceID) {
		return nil, errors.New("OpenCode Go workspace ID is invalid")
	}
	cookieHeader, err := BuildOpenCodeGoCookieHeader(authCookie)
	if err != nil {
		return nil, err
	}
	path := "/workspace/" + workspaceID + "/go"
	request, err := client.newConsoleRequest(ctx, http.MethodGet, path, cookieHeader)
	if err != nil {
		return nil, err
	}
	response, err := client.followClient.Do(request)
	if err != nil {
		var requestErr *url.Error
		if errors.As(err, &requestErr) {
			failedURL, parseErr := url.Parse(requestErr.URL)
			if parseErr == nil && strings.HasSuffix(strings.TrimRight(failedURL.Path, "/"), "/login") {
				return nil, fmt.Errorf("%w: workspace request redirected to login", ErrOpenCodeGoAuthenticationInvalid)
			}
		}
		return nil, fmt.Errorf("OpenCode Go workspace request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w (status %d)", ErrOpenCodeGoAuthenticationInvalid, response.StatusCode)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("OpenCode Go workspace request returned status %d", response.StatusCode)
	}
	finalURL := response.Request.URL
	finalWorkspaceID, ok := openCodeGoWorkspaceIDFromPath(finalURL.Path, true)
	if !sameOpenCodeGoOrigin(finalURL, client.consoleBase) || !ok {
		return nil, fmt.Errorf("%w: workspace request ended outside the expected page", ErrOpenCodeGoAuthenticationInvalid)
	}
	document, err := readOpenCodeGoResponseBody(response.Body, openCodeGoConsoleBodyLimit)
	if err != nil {
		return nil, err
	}
	return ParseOpenCodeGoConsolePage(string(document), finalWorkspaceID, client.now())
}

func (client *OpenCodeGoConsoleClient) FetchAPIKey(
	ctx context.Context,
	authCookie string,
	workspaceID string,
) (string, error) {
	if !openCodeGoWorkspaceIDPattern.MatchString(workspaceID) {
		return "", errors.New("OpenCode Go workspace ID is invalid")
	}
	cookieHeader, err := BuildOpenCodeGoCookieHeader(authCookie)
	if err != nil {
		return "", err
	}
	path := "/workspace/" + workspaceID + "/keys"
	request, err := client.newConsoleRequest(ctx, http.MethodGet, path, cookieHeader)
	if err != nil {
		return "", err
	}
	response, err := client.followClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("OpenCode Go API-key request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("OpenCode Go API-key request returned status %d", response.StatusCode)
	}
	if !sameOpenCodeGoOrigin(response.Request.URL, client.consoleBase) || response.Request.URL.Path != path {
		return "", errors.New("OpenCode Go API-key request ended outside the expected page")
	}
	document, err := readOpenCodeGoResponseBody(response.Body, openCodeGoConsoleBodyLimit)
	if err != nil {
		return "", err
	}
	return ParseOpenCodeGoAPIKeyPage(string(document))
}

func (client *OpenCodeGoConsoleClient) FetchModels(ctx context.Context, apiKey string) ([]string, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("OpenCode Go API key is empty")
	}
	requestURL := *client.inferenceBase
	requestURL.Path = strings.TrimRight(requestURL.Path, "/") + "/models"
	requestURL.RawQuery = ""
	requestURL.Fragment = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, errors.New("OpenCode Go models request could not be created")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("User-Agent", openCodeGoConsoleUserAgent)
	response, err := client.followClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("OpenCode Go models request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("OpenCode Go models request returned status %d", response.StatusCode)
	}
	body, err := readOpenCodeGoResponseBody(response.Body, openCodeGoModelsBodyLimit)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := common.Unmarshal(body, &payload); err != nil {
		return nil, errors.New("OpenCode Go models response is invalid")
	}
	seen := make(map[string]struct{})
	models := make([]string, 0, len(payload.Data))
	for _, entry := range payload.Data {
		modelID := strings.TrimSpace(entry.ID)
		if modelID == "" || len(modelID) > 191 || strings.ContainsAny(modelID, "\r\n\t ") {
			continue
		}
		if _, exists := seen[modelID]; exists {
			continue
		}
		seen[modelID] = struct{}{}
		models = append(models, modelID)
	}
	sort.Strings(models)
	return models, nil
}

func (client *OpenCodeGoConsoleClient) newConsoleRequest(
	ctx context.Context,
	method string,
	path string,
	cookieHeader string,
) (*http.Request, error) {
	requestURL := *client.consoleBase
	requestURL.Path = path
	requestURL.RawQuery = ""
	requestURL.Fragment = ""
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), nil)
	if err != nil {
		return nil, errors.New("OpenCode Go console request could not be created")
	}
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	request.Header.Set("Accept-Language", "zh")
	request.Header.Set("Cookie", cookieHeader)
	request.Header.Set("Referer", client.consoleBase.ResolveReference(&url.URL{Path: "/zh/go"}).String())
	request.Header.Set("User-Agent", openCodeGoConsoleUserAgent)
	return request, nil
}

func sameOpenCodeGoOrigin(candidate *url.URL, expected *url.URL) bool {
	if candidate == nil || expected == nil {
		return false
	}
	return strings.EqualFold(candidate.Scheme, expected.Scheme) && strings.EqualFold(candidate.Host, expected.Host)
}

func openCodeGoWorkspaceIDFromPath(path string, requireGoSuffix bool) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if requireGoSuffix {
		if len(parts) != 3 || parts[0] != "workspace" || parts[2] != "go" {
			return "", false
		}
	} else if len(parts) != 2 || parts[0] != "workspace" {
		return "", false
	}
	if !openCodeGoWorkspaceIDPattern.MatchString(parts[1]) {
		return "", false
	}
	return parts[1], true
}

func isOpenCodeGoRedirectStatus(status int) bool {
	switch status {
	case http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func readOpenCodeGoResponseBody(body io.Reader, limit int64) ([]byte, error) {
	limited := io.LimitReader(body, limit+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return nil, errors.New("OpenCode Go response body could not be read")
	}
	if int64(len(payload)) > limit {
		return nil, errors.New("OpenCode Go response body is too large")
	}
	return payload, nil
}
