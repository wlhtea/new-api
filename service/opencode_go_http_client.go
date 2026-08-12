package service

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

type openCodeGoChannelRoundTripper struct {
	base       http.RoundTripper
	redactions []string
}

type openCodeGoChannelRequestError struct {
	message string
	cause   error
}

func (transport *openCodeGoChannelRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err == nil {
		return response, nil
	}
	message := sanitizeOpenCodeGoError(err, transport.redactions...)
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
	redactions := []string{
		strings.TrimSpace(settings.Proxy),
		proxyURL.String(),
		proxyURL.Host,
		proxyURL.Hostname(),
	}
	baseTransport := client.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	scopedClient := *client
	scopedClient.Transport = &openCodeGoChannelRoundTripper{
		base:       baseTransport,
		redactions: redactions,
	}
	return &scopedClient, nil
}
