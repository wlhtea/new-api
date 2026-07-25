package seedance

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type countingReadCloser struct {
	io.Reader
	closes atomic.Int32
}

func (r *countingReadCloser) Close() error {
	r.closes.Add(1)
	return nil
}

func TestBindCancelToBodyCancelsExactlyOnce(t *testing.T) {
	var cancels atomic.Int32
	body := &countingReadCloser{Reader: http.NoBody}
	resp := bindCancelToBody(&http.Response{Body: body}, func() {
		cancels.Add(1)
	})

	require.NoError(t, resp.Body.Close())
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, int32(1), cancels.Load())
	assert.Equal(t, int32(2), body.closes.Load())
}

func TestNewStageClientPreservesTransportAndWrapsConnectDeadline(t *testing.T) {
	var proxiedRequests atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		req *http.Request,
	) {
		proxiedRequests.Add(1)
		assert.Equal(t, "http://TARGET/generate", req.URL.String())
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer proxyServer.Close()
	proxyURL, err := url.Parse(proxyServer.URL)
	require.NoError(t, err)

	var dialCalls atomic.Int32
	originalDial := (&net.Dialer{}).DialContext
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(proxyURL)
	transport.TLSHandshakeTimeout = 17 * time.Second
	transport.MaxIdleConns = 23
	transport.DialContext = func(
		ctx context.Context,
		network string,
		address string,
	) (net.Conn, error) {
		dialCalls.Add(1)
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		remaining := time.Until(deadline)
		assert.Greater(t, remaining, 0*time.Second)
		assert.LessOrEqual(t, remaining, 10*time.Second)
		assert.Equal(t, proxyServer.Listener.Addr().String(), address)
		return originalDial(ctx, network, address)
	}
	base := &http.Client{
		Transport: transport,
		Timeout:   45 * time.Second,
	}

	staged, err := newStageClient(base, proxyURL.String(), 10*time.Second)
	require.NoError(t, err)
	require.NotSame(t, base, staged)
	assert.Equal(t, base.Timeout, staged.Timeout)

	stagedTransport, ok := staged.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotSame(t, transport, stagedTransport)
	assert.Equal(t, transport.TLSHandshakeTimeout, stagedTransport.TLSHandshakeTimeout)
	assert.Equal(t, transport.MaxIdleConns, stagedTransport.MaxIdleConns)

	selectedProxy, err := stagedTransport.Proxy(
		&http.Request{URL: &url.URL{Scheme: "http", Host: "TARGET"}},
	)
	require.NoError(t, err)
	assert.Equal(t, proxyURL.String(), selectedProxy.String())
	originalProxy, err := base.Transport.(*http.Transport).Proxy(
		&http.Request{URL: &url.URL{Scheme: "http", Host: "TARGET"}},
	)
	require.NoError(t, err)
	assert.Equal(t, proxyURL.String(), originalProxy.String())

	resp, err := staged.Get("http://TARGET/generate")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, int32(1), dialCalls.Load())
	assert.Equal(t, int32(1), proxiedRequests.Load())
}

func TestNewStageClientPreservesSOCKSDialSelection(t *testing.T) {
	var socksDialCalls atomic.Int32
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		socksDialCalls.Add(1)
		_, hasDeadline := ctx.Deadline()
		assert.True(t, hasDeadline)
		return nil, errors.New("SOCKS_DIAL_STOP")
	}

	staged, err := newStageClient(
		&http.Client{Transport: transport},
		"socks5://PROXY:1080",
		10*time.Second,
	)
	require.NoError(t, err)
	stagedTransport := staged.Transport.(*http.Transport)
	assert.Nil(t, stagedTransport.Proxy)
	_, err = stagedTransport.DialContext(context.Background(), "tcp", "TARGET:443")
	require.ErrorContains(t, err, "SOCKS_DIAL_STOP")
	assert.Equal(t, int32(1), socksDialCalls.Load())
}
