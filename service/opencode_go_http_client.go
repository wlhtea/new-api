package service

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"gorm.io/gorm"
)

type openCodeGoChannelRoundTripper struct {
	base       http.RoundTripper
	redactions []string
}

type openCodeGoChannelRequestError struct {
	message string
	cause   error
}

var getOpenCodeGoIdentityForHTTPClient = model.GetOpenCodeGoIdentity

func (transport *openCodeGoChannelRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err == nil {
		return response, nil
	}
	redactions := append([]string(nil), transport.redactions...)
	if request != nil {
		for _, headerName := range []string{"Authorization", "Proxy-Authorization", "x-api-key", "Cookie"} {
			value := strings.TrimSpace(request.Header.Get(headerName))
			if value == "" {
				continue
			}
			redactions = append(redactions, value)
			if _, credential, ok := strings.Cut(value, " "); ok {
				redactions = append(redactions, strings.TrimSpace(credential))
			}
		}
	}
	message := sanitizeOpenCodeGoError(err, redactions...)
	if message == "" {
		message = "OpenCode Go channel request failed"
	}
	return response, &openCodeGoChannelRequestError{message: message, cause: err}
}

func (transport *openCodeGoChannelRoundTripper) CloseIdleConnections() {
	if closer, ok := transport.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (err *openCodeGoChannelRequestError) Error() string {
	return err.message
}

func (err *openCodeGoChannelRequestError) Unwrap() error {
	return err.cause
}

func getOpenCodeGoChannelHTTPClient(channelID int) (*http.Client, error) {
	channel, err := model.GetChannelById(channelID, true)
	if err != nil {
		return nil, err
	}
	if channel.Type != constant.ChannelTypeOpenCodeGo {
		return nil, errors.New("channel is not an OpenCode Go channel")
	}

	settings := dto.ChannelSettings{}
	if channel.Setting != nil && strings.TrimSpace(*channel.Setting) != "" {
		if err := common.Unmarshal([]byte(*channel.Setting), &settings); err != nil {
			return nil, errors.New("OpenCode Go channel HTTP settings are invalid")
		}
	}
	return getOpenCodeGoHTTPClientForSettings(settings)
}

// GetOpenCodeGoIdentityHTTPClient resolves the request-local client for one
// stable imported account. An empty/disabled policy preserves the channel's
// static proxy or direct-client behavior.
func GetOpenCodeGoIdentityHTTPClient(channelID int, identityUID string) (*http.Client, error) {
	return getOpenCodeGoIdentityHTTPClient(channelID, identityUID, true)
}

func getOpenCodeGoProvisionalIdentityHTTPClient(channelID int, identityUID string) (*http.Client, error) {
	return getOpenCodeGoIdentityHTTPClient(channelID, identityUID, false)
}

func getOpenCodeGoIdentityHTTPClient(channelID int, identityUID string, requirePersistedIdentity bool) (*http.Client, error) {
	generation := openCodeGoIdentityProxyClients.captureGeneration(channelID)
	channel, err := model.GetChannelById(channelID, true)
	if err != nil {
		return nil, err
	}
	if channel.Type != constant.ChannelTypeOpenCodeGo {
		return nil, errors.New("channel is not an OpenCode Go channel")
	}
	settings := dto.ChannelSettings{}
	if channel.Setting != nil && strings.TrimSpace(*channel.Setting) != "" {
		if err := common.Unmarshal([]byte(*channel.Setting), &settings); err != nil {
			return nil, errors.New("OpenCode Go channel HTTP settings are invalid")
		}
	}
	otherSettings := dto.ChannelOtherSettings{}
	if strings.TrimSpace(channel.OtherSettings) != "" {
		if err := common.UnmarshalJsonStr(channel.OtherSettings, &otherSettings); err != nil {
			return nil, errors.New("OpenCode Go channel identity proxy settings are invalid")
		}
	}
	if requirePersistedIdentity {
		identity, err := getOpenCodeGoIdentityForHTTPClient(channelID, identityUID)
		if err != nil {
			return nil, err
		}
		if identity == nil {
			return nil, ErrOpenCodeGoIdentityProxySelectionStale
		}
	}
	return resolveOpenCodeGoIdentityHTTPClientWithGeneration(
		channelID,
		identityUID,
		settings,
		otherSettings.OpenCodeGo,
		time.Now(),
		&generation,
	)
}

// AcquireOpenCodeGoRelayHTTPClient validates a pool selection against current
// persistent ownership and credentials immediately before relay I/O. The
// release function keeps local restrictive mutations behind the request until
// net/http has either failed or returned the upstream response headers.
func AcquireOpenCodeGoRelayHTTPClient(
	channelID int,
	identityUID string,
	workspaceUID string,
	selectedAPIKey string,
	upstreamModel string,
	generation OpenCodeGoIdentityProxyGeneration,
) (_ *http.Client, release func(), err error) {
	lease := openCodeGoPoolMutations.beginRelay(channelID)
	keepLease := false
	defer func() {
		if !keepLease {
			lease()
		}
	}()
	if !openCodeGoIdentityProxyClients.generationMatches(channelID, &generation) {
		return nil, nil, ErrOpenCodeGoIdentityProxySelectionStale
	}

	settings, otherSettings, err := validateOpenCodeGoRelaySelection(
		channelID,
		identityUID,
		workspaceUID,
		selectedAPIKey,
		upstreamModel,
		time.Now(),
	)
	if err != nil {
		return nil, nil, err
	}
	client, err := resolveOpenCodeGoIdentityHTTPClientWithGeneration(
		channelID,
		identityUID,
		settings,
		otherSettings.OpenCodeGo,
		time.Now(),
		&generation,
	)
	if err != nil {
		return nil, nil, err
	}
	keepLease = true
	return client, lease, nil
}

func validateOpenCodeGoRelaySelection(
	channelID int,
	identityUID string,
	workspaceUID string,
	selectedAPIKey string,
	upstreamModel string,
	now time.Time,
) (dto.ChannelSettings, dto.ChannelOtherSettings, error) {
	stale := func() (dto.ChannelSettings, dto.ChannelOtherSettings, error) {
		return dto.ChannelSettings{}, dto.ChannelOtherSettings{}, ErrOpenCodeGoIdentityProxySelectionStale
	}
	if channelID <= 0 || strings.TrimSpace(identityUID) == "" ||
		strings.TrimSpace(workspaceUID) == "" || strings.TrimSpace(selectedAPIKey) == "" ||
		strings.TrimSpace(upstreamModel) == "" {
		return stale()
	}

	channel, err := model.GetChannelById(channelID, true)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return stale()
		}
		return dto.ChannelSettings{}, dto.ChannelOtherSettings{}, errors.New("OpenCode Go relay ownership validation failed")
	}
	if channel.Type != constant.ChannelTypeOpenCodeGo || channel.Status != common.ChannelStatusEnabled {
		return stale()
	}

	settings := dto.ChannelSettings{}
	if channel.Setting != nil && strings.TrimSpace(*channel.Setting) != "" {
		if err := common.Unmarshal([]byte(*channel.Setting), &settings); err != nil {
			return dto.ChannelSettings{}, dto.ChannelOtherSettings{}, errors.New("OpenCode Go channel HTTP settings are invalid")
		}
	}
	otherSettings := dto.ChannelOtherSettings{}
	if strings.TrimSpace(channel.OtherSettings) != "" {
		if err := common.UnmarshalJsonStr(channel.OtherSettings, &otherSettings); err != nil {
			return dto.ChannelSettings{}, dto.ChannelOtherSettings{}, errors.New("OpenCode Go channel identity proxy settings are invalid")
		}
	}

	identity, err := model.GetOpenCodeGoIdentity(channelID, identityUID)
	if err != nil {
		return dto.ChannelSettings{}, dto.ChannelOtherSettings{}, errors.New("OpenCode Go relay ownership validation failed")
	}
	if identity == nil || identity.Status != model.OpenCodeGoIdentityStatusActive {
		return stale()
	}
	workspace, err := model.GetOpenCodeGoWorkspace(channelID, workspaceUID)
	if err != nil {
		return dto.ChannelSettings{}, dto.ChannelOtherSettings{}, errors.New("OpenCode Go relay ownership validation failed")
	}
	if workspace == nil || workspace.IdentityID != identity.ID || !isOpenCodeGoWorkspaceEligibleForSnapshot(*workspace, now.Unix()) {
		return stale()
	}

	modelEligible := false
	for _, entry := range workspace.Models {
		if strings.TrimSpace(entry.Model) == strings.TrimSpace(upstreamModel) &&
			entry.Discovered && entry.State == model.OpenCodeGoModelAvailable && entry.DisabledUntil <= now.Unix() {
			modelEligible = true
			break
		}
	}
	if !modelEligible {
		return stale()
	}

	codec, err := NewConfiguredOpenCodeGoCredentialCodec()
	if err != nil {
		return dto.ChannelSettings{}, dto.ChannelOtherSettings{}, err
	}
	currentAPIKey, err := codec.Decrypt(
		OpenCodeGoCredentialAPIKey,
		channelID,
		workspace.UID,
		workspace.APIKeyCiphertext,
	)
	if err != nil || subtle.ConstantTimeCompare([]byte(currentAPIKey), []byte(selectedAPIKey)) != 1 {
		return stale()
	}
	return settings, otherSettings, nil
}

func getOpenCodeGoHTTPClientForSettings(settings dto.ChannelSettings) (*http.Client, error) {
	if err := settings.ValidateHTTPTransport(); err != nil {
		return nil, errors.New("OpenCode Go channel HTTP settings are invalid")
	}
	proxyURL, _, err := common.ParseProxyURLRuntime(settings.Proxy)
	if err != nil {
		return nil, errors.New("OpenCode Go channel HTTP client could not be configured")
	}
	proxyValue := ""
	if proxyURL != nil {
		proxyValue = proxyURL.String()
	}
	client, err := GetHttpClientWithProxySettings(proxyValue, settings)
	if err != nil {
		return nil, errors.New("OpenCode Go channel HTTP client could not be configured")
	}
	if proxyURL == nil {
		return client, nil
	}
	return wrapOpenCodeGoProxyHTTPClient(client, settings.Proxy, proxyURL), nil
}

func wrapOpenCodeGoProxyHTTPClient(
	client *http.Client,
	rawProxyURL string,
	proxyURL *url.URL,
	additionalRedactions ...string,
) *http.Client {
	redactions := []string{
		strings.TrimSpace(rawProxyURL),
		proxyURL.String(),
		proxyURL.Host,
		proxyURL.Hostname(),
		proxyURL.User.Username(),
	}
	if password, ok := proxyURL.User.Password(); ok {
		redactions = append(redactions, password)
	}
	redactions = append(redactions, additionalRedactions...)
	baseTransport := client.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	scopedClient := *client
	scopedClient.Transport = &openCodeGoChannelRoundTripper{
		base:       baseTransport,
		redactions: redactions,
	}
	return &scopedClient
}
