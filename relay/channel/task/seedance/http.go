package seedance

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

type cancelOnCloseReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (r *cancelOnCloseReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.cancel)
	return err
}

func bindCancelToBody(
	resp *http.Response,
	cancel context.CancelFunc,
) *http.Response {
	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	resp.Body = &cancelOnCloseReadCloser{
		ReadCloser: resp.Body,
		cancel:     cancel,
	}
	return resp
}

func newStageClient(
	base *http.Client,
	_ string,
	connectTimeout time.Duration,
) (*http.Client, error) {
	if base == nil {
		return nil, fmt.Errorf("base HTTP client is nil")
	}

	client := *base
	var transport *http.Transport
	switch current := base.Transport.(type) {
	case nil:
		defaultTransport, ok := http.DefaultTransport.(*http.Transport)
		if !ok || defaultTransport == nil {
			return nil, fmt.Errorf("default HTTP transport is not cloneable")
		}
		transport = defaultTransport.Clone()
	case *http.Transport:
		if current == nil {
			return nil, fmt.Errorf("base HTTP transport is nil")
		}
		transport = current.Clone()
	default:
		return nil, fmt.Errorf("base HTTP transport is not cloneable")
	}

	originalDial := transport.DialContext
	if originalDial == nil {
		originalDial = (&net.Dialer{}).DialContext
	}
	transport.DialContext = func(
		ctx context.Context,
		network string,
		address string,
	) (net.Conn, error) {
		dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
		defer cancel()
		return originalDial(dialCtx, network, address)
	}
	client.Transport = transport
	return &client, nil
}
