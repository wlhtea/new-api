package service

import (
	"context"
	"errors"
	"fmt"
	stdhtml "html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	xhtml "golang.org/x/net/html"
)

const (
	openCodeGoLifecycleBodyLimit = int64(2 * 1024 * 1024)
	openCodeGoStripeVersion      = "2026-06-24.dahlia"
)

var (
	openCodeGoReferralActionPattern  = regexp.MustCompile(`(?is)createServerReference\("([a-f0-9]{64})"\).{0,300}"go\.referral\.reward\.apply"`)
	openCodeGoBillingActionPattern   = regexp.MustCompile(`(?i)createSessionUrl_action\s*=\s*createServerReference\("([a-f0-9]{64})"\)`)
	openCodeGoStripeVersionPattern   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}\.[a-z]+`)
	openCodeGoSubscriptionIDPattern  = regexp.MustCompile(`^sub_[A-Za-z0-9]+$`)
	openCodeGoPortalSessionIDPattern = regexp.MustCompile(`^bps_[A-Za-z0-9]+$`)
)

type OpenCodeGoSubscriptionCancellation struct {
	AlreadyCancelled bool  `json:"already_cancelled"`
	CurrentPeriodEnd int64 `json:"current_period_end"`
}

type openCodeGoLifecycleMutator interface {
	EnableChinaModels(ctx context.Context, authCookie string, page *OpenCodeGoConsolePage) error
	DisableChinaModels(ctx context.Context, authCookie string, page *OpenCodeGoConsolePage) error
	ApplyReferralReward(ctx context.Context, authCookie string, page *OpenCodeGoConsolePage, rewardID string) error
	CancelSubscriptionRenewal(ctx context.Context, authCookie string, page *OpenCodeGoConsolePage) (OpenCodeGoSubscriptionCancellation, error)
}

type OpenCodeGoLifecycleClient struct {
	console       *OpenCodeGoConsoleClient
	billingBase   *url.URL
	billingClient *http.Client
}

func NewOpenCodeGoLifecycleClient() *OpenCodeGoLifecycleClient {
	console := NewOpenCodeGoConsoleClient()
	client, err := newOpenCodeGoLifecycleClient(console, "https://billing.stripe.com", GetHttpClient())
	if err != nil {
		panic(err)
	}
	return client
}

func newOpenCodeGoLifecycleClient(
	console *OpenCodeGoConsoleClient,
	billingOrigin string,
	baseClient *http.Client,
) (*OpenCodeGoLifecycleClient, error) {
	if console == nil {
		return nil, errors.New("OpenCode Go console client is required")
	}
	billingBase, err := url.Parse(billingOrigin)
	if err != nil || billingBase.Scheme == "" || billingBase.Host == "" {
		return nil, errors.New("OpenCode Go billing origin is invalid")
	}
	transport := http.DefaultTransport
	if baseClient != nil && baseClient.Transport != nil {
		transport = baseClient.Transport
	}
	billingClient := &http.Client{
		Transport: transport,
		Timeout:   openCodeGoConsoleRequestLimit,
	}
	billingClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if !sameOpenCodeGoOrigin(request.URL, billingBase) {
			request.Header.Del("Authorization")
			return errors.New("OpenCode Go billing redirect left the allowed origin")
		}
		if len(via) >= 5 {
			return errors.New("OpenCode Go billing redirect limit exceeded")
		}
		return nil
	}
	return &OpenCodeGoLifecycleClient{
		console:       console,
		billingBase:   billingBase,
		billingClient: billingClient,
	}, nil
}

func (client *OpenCodeGoLifecycleClient) EnableChinaModels(
	ctx context.Context,
	authCookie string,
	page *OpenCodeGoConsolePage,
) error {
	if page == nil || !openCodeGoWorkspaceIDPattern.MatchString(page.WorkspaceID) {
		return errors.New("OpenCode Go China-model action target is invalid")
	}
	if page.ChinaModelsEnabled == nil {
		return errors.New("OpenCode Go China-model state is not authoritative")
	}
	if *page.ChinaModelsEnabled {
		return nil
	}
	if !openCodeGoServerActionIDPattern.MatchString(page.ChinaModelsServerID) {
		return errors.New("OpenCode Go China-model action could not be resolved")
	}
	cookieHeader, err := BuildOpenCodeGoCookieHeader(authCookie)
	if err != nil {
		return err
	}
	actionURL := client.console.consoleBase.ResolveReference(&url.URL{Path: "/_server"})
	query := actionURL.Query()
	query.Set("id", page.ChinaModelsServerID)
	actionURL.RawQuery = query.Encode()
	body := url.Values{
		"workspaceID":       []string{page.WorkspaceID},
		"useChinaProviders": []string{"false"},
	}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, actionURL.String(), strings.NewReader(body))
	if err != nil {
		return errors.New("OpenCode Go China-model request could not be created")
	}
	client.setConsoleMutationHeaders(request, cookieHeader, page.WorkspaceID)
	request.Header.Set("Accept", "text/html,application/xhtml+xml")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", openCodeGoURLOrigin(client.console.consoleBase))
	response, err := client.console.manualClient.Do(request)
	if err != nil {
		return fmt.Errorf("OpenCode Go China-model action failed: %w", err)
	}
	defer response.Body.Close()
	if (!responseSuccessOrRedirect(response.StatusCode)) || response.Header.Get("X-Error") != "" {
		return fmt.Errorf("OpenCode Go China-model action returned status %d", response.StatusCode)
	}
	return nil
}

func (client *OpenCodeGoLifecycleClient) DisableChinaModels(
	ctx context.Context,
	authCookie string,
	page *OpenCodeGoConsolePage,
) error {
	if page == nil || !openCodeGoWorkspaceIDPattern.MatchString(page.WorkspaceID) {
		return errors.New("OpenCode Go China-model action target is invalid")
	}
	if page.ChinaModelsEnabled == nil {
		return errors.New("OpenCode Go China-model state is not authoritative")
	}
	if !*page.ChinaModelsEnabled {
		return nil
	}
	if !openCodeGoServerActionIDPattern.MatchString(page.ChinaModelsServerID) {
		return errors.New("OpenCode Go China-model action could not be resolved")
	}
	cookieHeader, err := BuildOpenCodeGoCookieHeader(authCookie)
	if err != nil {
		return err
	}
	actionURL := client.console.consoleBase.ResolveReference(&url.URL{Path: "/_server"})
	query := actionURL.Query()
	query.Set("id", page.ChinaModelsServerID)
	actionURL.RawQuery = query.Encode()
	body := url.Values{
		"workspaceID":       []string{page.WorkspaceID},
		"useChinaProviders": []string{"true"},
	}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, actionURL.String(), strings.NewReader(body))
	if err != nil {
		return errors.New("OpenCode Go China-model request could not be created")
	}
	client.setConsoleMutationHeaders(request, cookieHeader, page.WorkspaceID)
	request.Header.Set("Accept", "text/html,application/xhtml+xml")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", openCodeGoURLOrigin(client.console.consoleBase))
	response, err := client.console.manualClient.Do(request)
	if err != nil {
		return fmt.Errorf("OpenCode Go China-model action failed: %w", err)
	}
	defer response.Body.Close()
	if (!responseSuccessOrRedirect(response.StatusCode)) || response.Header.Get("X-Error") != "" {
		return fmt.Errorf("OpenCode Go China-model action returned status %d", response.StatusCode)
	}
	return nil
}

func (client *OpenCodeGoLifecycleClient) ApplyReferralReward(
	ctx context.Context,
	authCookie string,
	page *OpenCodeGoConsolePage,
	rewardID string,
) error {
	if page == nil || !openCodeGoWorkspaceIDPattern.MatchString(page.WorkspaceID) ||
		!openCodeGoReferralRewardIDPattern.MatchString(rewardID) || len(rewardID) > 96 {
		return errors.New("OpenCode Go referral action target is invalid")
	}
	serverID, err := client.resolveServerAction(ctx, authCookie, page, openCodeGoReferralActionPattern)
	if err != nil {
		return err
	}
	return client.postConsoleServerAction(ctx, authCookie, page.WorkspaceID, serverID, []string{page.WorkspaceID, rewardID})
}

func (client *OpenCodeGoLifecycleClient) CancelSubscriptionRenewal(
	ctx context.Context,
	authCookie string,
	page *OpenCodeGoConsolePage,
) (OpenCodeGoSubscriptionCancellation, error) {
	if page == nil || !openCodeGoWorkspaceIDPattern.MatchString(page.WorkspaceID) ||
		!openCodeGoSubscriptionIDPattern.MatchString(page.SubscriptionReference) || len(page.SubscriptionReference) > 128 {
		return OpenCodeGoSubscriptionCancellation{}, errors.New("OpenCode Go subscription cancellation target is invalid")
	}
	serverID, err := client.resolveServerAction(ctx, authCookie, page, openCodeGoBillingActionPattern)
	if err != nil {
		return OpenCodeGoSubscriptionCancellation{}, err
	}
	returnURL := client.console.consoleBase.ResolveReference(&url.URL{Path: "/workspace/" + page.WorkspaceID + "/go"}).String()
	portalBody, err := client.postConsoleServerActionBody(
		ctx,
		authCookie,
		page.WorkspaceID,
		serverID,
		[]string{page.WorkspaceID, returnURL},
	)
	if err != nil {
		return OpenCodeGoSubscriptionCancellation{}, err
	}
	portalURL, err := client.billingPortalURL(string(portalBody))
	if err != nil {
		return OpenCodeGoSubscriptionCancellation{}, err
	}
	portalPage, err := client.fetchBillingPortalPage(ctx, portalURL)
	if err != nil {
		return OpenCodeGoSubscriptionCancellation{}, err
	}
	sessionKey, portalSessionID, err := parseOpenCodeGoBillingPortalSession(portalPage)
	if err != nil {
		return OpenCodeGoSubscriptionCancellation{}, err
	}

	subscriptionURL := client.billingBase.ResolveReference(&url.URL{
		Path: "/v1/billing_portal/sessions/" + portalSessionID + "/subscriptions/" + page.SubscriptionReference,
	})
	stripeVersion := openCodeGoStripeVersion
	readSubscription := func() (openCodeGoStripeSubscription, error) {
		body, status, resolvedVersion, requestErr := client.doStripeRequest(
			ctx,
			subscriptionURL,
			portalURL,
			sessionKey,
			stripeVersion,
			http.MethodGet,
		)
		stripeVersion = resolvedVersion
		if requestErr != nil {
			return openCodeGoStripeSubscription{}, requestErr
		}
		var subscription openCodeGoStripeSubscription
		if status < http.StatusOK || status >= http.StatusMultipleChoices || common.Unmarshal(body, &subscription) != nil || subscription.ID != page.SubscriptionReference {
			return openCodeGoStripeSubscription{}, fmt.Errorf("OpenCode Go subscription read returned status %d", status)
		}
		return subscription, nil
	}

	before, err := readSubscription()
	if err != nil {
		return OpenCodeGoSubscriptionCancellation{}, err
	}
	if before.CancelAtPeriodEnd {
		return OpenCodeGoSubscriptionCancellation{AlreadyCancelled: true, CurrentPeriodEnd: before.CurrentPeriodEnd}, nil
	}
	cancelURL := *subscriptionURL
	cancelURL.Path += "/cancel"
	_, status, resolvedVersion, err := client.doStripeRequest(
		ctx,
		&cancelURL,
		portalURL,
		sessionKey,
		stripeVersion,
		http.MethodPost,
	)
	stripeVersion = resolvedVersion
	if err != nil {
		return OpenCodeGoSubscriptionCancellation{}, err
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return OpenCodeGoSubscriptionCancellation{}, fmt.Errorf("OpenCode Go subscription cancellation returned status %d", status)
	}
	after, err := readSubscription()
	if err != nil {
		return OpenCodeGoSubscriptionCancellation{}, err
	}
	if !after.CancelAtPeriodEnd {
		return OpenCodeGoSubscriptionCancellation{}, errors.New("OpenCode Go subscription renewal is still enabled after cancellation")
	}
	return OpenCodeGoSubscriptionCancellation{CurrentPeriodEnd: after.CurrentPeriodEnd}, nil
}

type openCodeGoServerArgument struct {
	Type  int    `json:"t"`
	Value string `json:"s"`
}

type openCodeGoServerPayload struct {
	Tuple struct {
		Type   int                        `json:"t"`
		Index  int                        `json:"i"`
		Length int                        `json:"l"`
		Args   []openCodeGoServerArgument `json:"a"`
		Offset int                        `json:"o"`
	} `json:"t"`
	Flags int   `json:"f"`
	Meta  []any `json:"m"`
}

func marshalOpenCodeGoServerArgs(args []string) ([]byte, error) {
	payload := openCodeGoServerPayload{Flags: 31, Meta: []any{}}
	payload.Tuple.Type = 9
	payload.Tuple.Length = len(args)
	payload.Tuple.Args = make([]openCodeGoServerArgument, 0, len(args))
	for _, value := range args {
		payload.Tuple.Args = append(payload.Tuple.Args, openCodeGoServerArgument{Type: 1, Value: value})
	}
	return common.Marshal(payload)
}

func (client *OpenCodeGoLifecycleClient) postConsoleServerAction(
	ctx context.Context,
	authCookie string,
	workspaceID string,
	serverID string,
	args []string,
) error {
	_, err := client.postConsoleServerActionBody(ctx, authCookie, workspaceID, serverID, args)
	return err
}

func (client *OpenCodeGoLifecycleClient) postConsoleServerActionBody(
	ctx context.Context,
	authCookie string,
	workspaceID string,
	serverID string,
	args []string,
) ([]byte, error) {
	if !openCodeGoServerActionIDPattern.MatchString(serverID) {
		return nil, errors.New("OpenCode Go server action could not be resolved")
	}
	cookieHeader, err := BuildOpenCodeGoCookieHeader(authCookie)
	if err != nil {
		return nil, err
	}
	body, err := marshalOpenCodeGoServerArgs(args)
	if err != nil {
		return nil, errors.New("OpenCode Go server action payload could not be encoded")
	}
	actionURL := client.console.consoleBase.ResolveReference(&url.URL{Path: "/_server"})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, actionURL.String(), strings.NewReader(string(body)))
	if err != nil {
		return nil, errors.New("OpenCode Go server action request could not be created")
	}
	client.setConsoleMutationHeaders(request, cookieHeader, workspaceID)
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Server-Id", serverID)
	request.Header.Set("X-Server-Instance", "server-fn:0")
	response, err := client.console.followClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("OpenCode Go server action failed: %w", err)
	}
	defer response.Body.Close()
	responseBody, readErr := readOpenCodeGoResponseBody(response.Body, openCodeGoLifecycleBodyLimit)
	if readErr != nil {
		return nil, readErr
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || response.Header.Get("X-Error") != "" {
		return nil, fmt.Errorf("OpenCode Go server action returned status %d", response.StatusCode)
	}
	return responseBody, nil
}

func (client *OpenCodeGoLifecycleClient) resolveServerAction(
	ctx context.Context,
	authCookie string,
	page *OpenCodeGoConsolePage,
	pattern *regexp.Regexp,
) (string, error) {
	cookieHeader, err := BuildOpenCodeGoCookieHeader(authCookie)
	if err != nil {
		return "", err
	}
	for index := len(page.RouteModuleAssets) - 1; index >= 0; index-- {
		assetURL, parseErr := url.Parse(page.RouteModuleAssets[index])
		if parseErr != nil {
			continue
		}
		assetURL = client.console.consoleBase.ResolveReference(assetURL)
		if !sameOpenCodeGoOrigin(assetURL, client.console.consoleBase) || !strings.HasSuffix(strings.ToLower(assetURL.Path), ".js") {
			continue
		}
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, assetURL.String(), nil)
		if requestErr != nil {
			continue
		}
		request.Header.Set("Accept", "*/*")
		request.Header.Set("Cookie", cookieHeader)
		request.Header.Set("User-Agent", openCodeGoConsoleUserAgent)
		response, requestErr := client.console.followClient.Do(request)
		if requestErr != nil {
			continue
		}
		body, readErr := readOpenCodeGoResponseBody(response.Body, openCodeGoLifecycleBodyLimit)
		response.Body.Close()
		if readErr != nil || response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			continue
		}
		match := pattern.FindSubmatch(body)
		if len(match) == 2 && openCodeGoServerActionIDPattern.Match(match[1]) {
			return string(match[1]), nil
		}
	}
	return "", errors.New("OpenCode Go server action could not be resolved")
}

func (client *OpenCodeGoLifecycleClient) setConsoleMutationHeaders(request *http.Request, cookieHeader string, workspaceID string) {
	request.Header.Set("Cookie", cookieHeader)
	request.Header.Set("Referer", client.console.consoleBase.ResolveReference(&url.URL{Path: "/workspace/" + workspaceID + "/go"}).String())
	request.Header.Set("User-Agent", openCodeGoConsoleUserAgent)
}

func openCodeGoURLOrigin(value *url.URL) string {
	if value == nil {
		return ""
	}
	return value.Scheme + "://" + value.Host
}

func responseSuccessOrRedirect(status int) bool {
	return (status >= http.StatusOK && status < http.StatusMultipleChoices) || status == http.StatusFound || status == http.StatusSeeOther
}

func (client *OpenCodeGoLifecycleClient) billingPortalURL(body string) (*url.URL, error) {
	prefix := strings.TrimRight(client.billingBase.String(), "/") + "/p/session/"
	start := strings.Index(body, prefix)
	if start < 0 {
		return nil, errors.New("OpenCode Go billing portal session URL was not returned")
	}
	end := start
	for end < len(body) && !strings.ContainsRune("\"'\\; \t\r\n", rune(body[end])) {
		end++
	}
	portalURL, err := url.Parse(body[start:end])
	if err != nil || !sameOpenCodeGoOrigin(portalURL, client.billingBase) || !strings.HasPrefix(portalURL.Path, "/p/session/") {
		return nil, errors.New("OpenCode Go billing portal session URL is invalid")
	}
	return portalURL, nil
}

func (client *OpenCodeGoLifecycleClient) fetchBillingPortalPage(ctx context.Context, portalURL *url.URL) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, portalURL.String(), nil)
	if err != nil {
		return nil, errors.New("OpenCode Go billing portal request could not be created")
	}
	request.Header.Set("Accept", "text/html")
	request.Header.Set("User-Agent", openCodeGoConsoleUserAgent)
	response, err := client.billingClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("OpenCode Go billing portal request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("OpenCode Go billing portal returned status %d", response.StatusCode)
	}
	return readOpenCodeGoResponseBody(response.Body, openCodeGoLifecycleBodyLimit)
}

func parseOpenCodeGoBillingPortalSession(document []byte) (string, string, error) {
	doc, err := xhtml.Parse(strings.NewReader(string(document)))
	if err != nil {
		return "", "", errors.New("OpenCode Go billing portal HTML could not be parsed")
	}
	preload := ""
	var walk func(*xhtml.Node)
	walk = func(node *xhtml.Node) {
		if preload != "" {
			return
		}
		if node.Type == xhtml.ElementNode && strings.EqualFold(node.Data, "script") && openCodeGoHTMLAttribute(node, "id") == "preloaded_json" && node.FirstChild != nil {
			preload = node.FirstChild.Data
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	if strings.TrimSpace(preload) == "" {
		return "", "", errors.New("OpenCode Go billing portal session data is missing")
	}
	var payload struct {
		SessionAPIKey   string `json:"session_api_key"`
		PortalSessionID string `json:"portal_session_id"`
	}
	if err := common.Unmarshal([]byte(stdhtml.UnescapeString(preload)), &payload); err != nil || payload.SessionAPIKey == "" || !openCodeGoPortalSessionIDPattern.MatchString(payload.PortalSessionID) {
		return "", "", errors.New("OpenCode Go billing portal session data is invalid")
	}
	return payload.SessionAPIKey, payload.PortalSessionID, nil
}

type openCodeGoStripeSubscription struct {
	ID                string `json:"id"`
	CancelAtPeriodEnd bool   `json:"cancel_at_period_end"`
	CurrentPeriodEnd  int64  `json:"current_period_end"`
}

func (client *OpenCodeGoLifecycleClient) doStripeRequest(
	ctx context.Context,
	requestURL *url.URL,
	portalURL *url.URL,
	sessionKey string,
	stripeVersion string,
	method string,
) ([]byte, int, string, error) {
	if !sameOpenCodeGoOrigin(requestURL, client.billingBase) || !sameOpenCodeGoOrigin(portalURL, client.billingBase) {
		return nil, 0, stripeVersion, errors.New("OpenCode Go billing request target is invalid")
	}
	doRequest := func(version string) ([]byte, int, error) {
		request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), nil)
		if err != nil {
			return nil, 0, errors.New("OpenCode Go billing request could not be created")
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("Authorization", "Bearer "+sessionKey)
		request.Header.Set("Stripe-Version", version)
		request.Header.Set("User-Agent", openCodeGoConsoleUserAgent)
		request.Header.Set("Referer", portalURL.String())
		if method == http.MethodPost {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		response, err := client.billingClient.Do(request)
		if err != nil {
			return nil, 0, fmt.Errorf("OpenCode Go billing request failed: %w", err)
		}
		defer response.Body.Close()
		body, err := readOpenCodeGoResponseBody(response.Body, openCodeGoLifecycleBodyLimit)
		return body, response.StatusCode, err
	}
	body, status, err := doRequest(stripeVersion)
	if err != nil || status != http.StatusBadRequest || method != http.MethodGet {
		return body, status, stripeVersion, err
	}
	var failure struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if common.Unmarshal(body, &failure) != nil {
		return body, status, stripeVersion, nil
	}
	suggested := openCodeGoStripeVersionPattern.FindString(failure.Error.Message)
	if suggested == "" || suggested == stripeVersion {
		return body, status, stripeVersion, nil
	}
	body, status, err = doRequest(suggested)
	return body, status, suggested, err
}

func openCodeGoLifecycleAutomationEnabled() bool {
	// A DB-backed option (settable from the admin UI) takes precedence over the
	// deployment env default. The env var remains the bootstrap/fallback value
	// so existing deployments keep working without a DB row.
	common.OptionMapRWMutex.RLock()
	raw, ok := common.OptionMap["OpenCodeGoLifecycleAutomationEnabled"]
	common.OptionMapRWMutex.RUnlock()
	if ok {
		if enabled, err := strconv.ParseBool(raw); err == nil {
			return enabled
		}
	}
	return common.GetEnvOrDefaultBool("OPENCODE_GO_LIFECYCLE_AUTOMATION_ENABLED", false)
}

func openCodeGoLifecycleNow() time.Time {
	return time.Now()
}
