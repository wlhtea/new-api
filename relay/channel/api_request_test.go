package channel

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rootcommon "github.com/QuantumNous/new-api/common"
	rootconstant "github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type clientResolverTestAdaptor struct {
	Adaptor
	requestURL      string
	headerSetup     atomic.Bool
	headerSetupCall atomic.Int32
	setupHeader     func(*http.Header)
}

type clientResolverTestTransport struct {
	calls    atomic.Int32
	response *http.Response
	err      error
}

type roundTripTestFunc func(*http.Request) (*http.Response, error)

func (fn roundTripTestFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func (transport *clientResolverTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.calls.Add(1)
	if transport.response != nil {
		transport.response.Request = request
	}
	return transport.response, transport.err
}

func (adaptor *clientResolverTestAdaptor) GetRequestURL(*relaycommon.RelayInfo) (string, error) {
	return adaptor.requestURL, nil
}

func (adaptor *clientResolverTestAdaptor) SetupRequestHeader(
	_ *gin.Context,
	header *http.Header,
	_ *relaycommon.RelayInfo,
) error {
	adaptor.headerSetup.Store(true)
	adaptor.headerSetupCall.Add(1)
	if adaptor.setupHeader != nil {
		adaptor.setupHeader(header)
	}
	return nil
}

func TestNewUpstreamRequestInheritsDownstreamContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestContext, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestContext)

	upstreamRequest, err := newUpstreamRequest(c, http.MethodPost, "https://example.com/v1/responses", nil)
	require.NoError(t, err)
	require.NoError(t, upstreamRequest.Context().Err())

	cancel()
	require.ErrorIs(t, upstreamRequest.Context().Err(), context.Canceled)
}

func TestDownstreamRequestMethodRejectsMissingContext(t *testing.T) {
	for name, ctx := range map[string]*gin.Context{
		"nil context": nil,
		"nil request": {},
	} {
		t.Run(name, func(t *testing.T) {
			method, err := downstreamRequestMethod(ctx)
			require.Error(t, err)
			require.Empty(t, method)
		})
	}
}

func TestDownstreamRequestMethodReturnsMethod(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	method, err := downstreamRequestMethod(c)
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, method)
}

func TestDoApiRequestWithClientLeaseReleasesAfterResponseHeaders(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	adaptor := &clientResolverTestAdaptor{requestURL: "http://upstream.invalid/v1/chat/completions"}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
	transport := &clientResolverTestTransport{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       http.NoBody,
	}}
	var released atomic.Int32

	response, err := DoApiRequestWithClientLease(adaptor, c, info, strings.NewReader("{}"), func() (*http.Client, func(), error) {
		return &http.Client{Transport: transport}, func() { released.Add(1) }, nil
	})
	require.NoError(t, err)
	require.NotNil(t, response)
	require.Equal(t, int32(1), transport.calls.Load())
	require.Equal(t, int32(1), released.Load())
}

func TestDoApiRequestWithClientLeaseReleasesLeaseReturnedWithResolverFailure(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	adaptor := &clientResolverTestAdaptor{requestURL: "http://upstream.invalid/v1/chat/completions"}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
	var released atomic.Int32

	response, err := DoApiRequestWithClientLease(adaptor, c, info, strings.NewReader("{}"), func() (*http.Client, func(), error) {
		require.True(t, adaptor.headerSetup.Load(), "client resolution must follow account/header selection")
		return nil, func() { released.Add(1) }, errors.New("identity client resolution failed")
	})
	require.ErrorContains(t, err, "identity client resolution failed")
	require.Nil(t, response)
	require.Equal(t, int32(1), released.Load())
}

func TestDoApiRequestWithClientLeaseReleasesAfterTransportFailure(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	adaptor := &clientResolverTestAdaptor{requestURL: "http://upstream.invalid/v1/chat/completions"}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
	transport := &clientResolverTestTransport{err: errors.New("upstream transport failed")}
	var released atomic.Int32

	response, err := DoApiRequestWithClientLease(adaptor, c, info, strings.NewReader("{}"), func() (*http.Client, func(), error) {
		return &http.Client{Transport: transport}, func() { released.Add(1) }, nil
	})
	require.Error(t, err)
	require.Nil(t, response)
	require.Equal(t, int32(1), transport.calls.Load())
	require.Equal(t, int32(1), released.Load())
}

func TestDoApiRequestRedactsOpenCodeTransportFailureBeforeServerLog(t *testing.T) {
	previousWriter := gin.DefaultErrorWriter
	var logBuffer bytes.Buffer
	gin.DefaultErrorWriter = &logBuffer
	t.Cleanup(func() { gin.DefaultErrorWriter = previousWriter })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	adaptor := &clientResolverTestAdaptor{requestURL: "http://upstream.invalid/v1/responses"}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
		ChannelType: rootconstant.ChannelTypeOpenCodeAPIKey,
	}}
	transport := &clientResolverTestTransport{err: errors.New(
		"proxy Authorization: Bearer private-bearer via socks5://proxy-user:proxy-password@10.0.0.8:1080",
	)}

	response, err := DoApiRequestWithClientLease(adaptor, c, info, strings.NewReader("{}"), func() (*http.Client, func(), error) {
		return &http.Client{Transport: transport}, nil, nil
	})

	require.Error(t, err)
	require.Nil(t, response)
	serverLog := logBuffer.String()
	for _, secret := range []string{"private-bearer", "proxy-user", "proxy-password", "10.0.0.8"} {
		require.NotContains(t, serverLog, secret)
	}
	require.Contains(t, serverLog, "[redacted]")
}

func TestProcessHeaderOverride_ChannelTestSkipsPassthroughRules(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"*": "",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Empty(t, headers)
}

func TestProcessHeaderOverride_ChannelTestSkipsClientHeaderPlaceholder(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Upstream-Trace": "{client_header:X-Trace-Id}",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	_, ok := headers["x-upstream-trace"]
	require.False(t, ok)
}

func TestProcessHeaderOverride_NonTestKeepsClientHeaderPlaceholder(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Upstream-Trace": "{client_header:X-Trace-Id}",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "trace-123", headers["x-upstream-trace"])
}

func TestProcessHeaderOverride_OpenCodeRejectsGatewayOwnedOperatorHeaders(t *testing.T) {
	t.Parallel()

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	for _, channelType := range []int{
		rootconstant.ChannelTypeOpenCodeGo,
		rootconstant.ChannelTypeOpenCodeAPIKey,
	} {
		for _, headerName := range []string{"Authorization", "X-Api-Key", "X-OpenCode-Session"} {
			name := rootconstant.GetChannelTypeName(channelType) + "/" + headerName
			t.Run(name, func(t *testing.T) {
				info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
					ChannelType: channelType,
					HeadersOverride: map[string]any{
						headerName: "operator-must-not-own-this-value",
					},
				}}

				headers, err := processHeaderOverride(info, ctx)
				require.Nil(t, headers)
				var apiErr *types.NewAPIError
				require.ErrorAs(t, err, &apiErr)
				assert.Equal(t, types.ErrorOriginGatewayConfig, apiErr.Provenance().Origin)
				assert.Equal(t, "header_override", apiErr.Provenance().Subtype)
				assert.True(t, types.IsSkipRetryError(apiErr))
			})
		}
	}
}

func TestProcessHeaderOverride_OpenCodeForwardsOnlyDeclaredClientSemanticHeaders(t *testing.T) {
	t.Parallel()

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ctx.Request.Header.Set("Anthropic-Beta", "tools-2024-04-04")
	ctx.Request.Header.Set("Anthropic-Version", "2023-06-01")
	ctx.Request.Header.Set("Authorization", "Bearer client-secret")
	ctx.Request.Header.Set("Accept-Encoding", "gzip")
	ctx.Request.Header.Set("X-Unknown-Client", "must-not-cross")
	ctx.Request.Header.Set("Connection", "X-Custom")
	ctx.Request.Header.Set("X-Custom", "client-nominated-value")

	for _, channelType := range []int{
		rootconstant.ChannelTypeOpenCodeGo,
		rootconstant.ChannelTypeOpenCodeAPIKey,
	} {
		t.Run(rootconstant.GetChannelTypeName(channelType), func(t *testing.T) {
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
				ChannelType: channelType,
				HeadersOverride: map[string]any{
					"*":                  "",
					"X-Custom":           "operator-owned-value",
					"X-Upstream-Feature": "enabled",
				},
			}}

			headers, err := processHeaderOverride(info, ctx)
			require.NoError(t, err)
			assert.Equal(t, "tools-2024-04-04", headers["anthropic-beta"])
			assert.Equal(t, "2023-06-01", headers["anthropic-version"])
			assert.Equal(t, "operator-owned-value", headers["x-custom"])
			assert.Equal(t, "enabled", headers["x-upstream-feature"])
			assert.NotContains(t, headers, "authorization")
			assert.NotContains(t, headers, "accept-encoding")
			assert.NotContains(t, headers, "x-unknown-client")
		})
	}
}

func TestDoApiRequest_OpenCodeRejectsHeaderConfigBeforeSetupOrIO(t *testing.T) {
	t.Parallel()

	for _, channelType := range []int{
		rootconstant.ChannelTypeOpenCodeGo,
		rootconstant.ChannelTypeOpenCodeAPIKey,
	} {
		t.Run(rootconstant.GetChannelTypeName(channelType), func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			adaptor := &clientResolverTestAdaptor{requestURL: "http://upstream.invalid/v1/messages"}
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
				ChannelType: channelType,
				HeadersOverride: map[string]any{
					"Authorization": "Bearer operator-value",
				},
			}}
			transport := &clientResolverTestTransport{response: &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       http.NoBody,
			}}
			var resolverCalls atomic.Int32

			response, err := DoApiRequestWithClientLease(
				adaptor,
				ctx,
				info,
				strings.NewReader("{}"),
				func() (*http.Client, func(), error) {
					resolverCalls.Add(1)
					return &http.Client{Transport: transport}, nil, nil
				},
			)

			require.Nil(t, response)
			var apiErr *types.NewAPIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, types.ErrorOriginGatewayConfig, apiErr.Provenance().Origin)
			assert.True(t, types.IsSkipRetryError(apiErr))
			assert.Zero(t, adaptor.headerSetupCall.Load())
			assert.Zero(t, resolverCalls.Load())
			assert.Zero(t, transport.calls.Load())
		})
	}
}

func TestDoApiRequest_OpenCodeFinalWireHeadersKeepGatewayOwnership(t *testing.T) {
	t.Parallel()

	for _, channelType := range []int{
		rootconstant.ChannelTypeOpenCodeGo,
		rootconstant.ChannelTypeOpenCodeAPIKey,
	} {
		t.Run(rootconstant.GetChannelTypeName(channelType), func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			ctx.Request.Header.Set("Anthropic-Beta", "tools-2024-04-04")
			ctx.Request.Header.Set("Anthropic-Version", "2023-06-01")
			ctx.Request.Header.Set("X-Unknown-Client", "must-not-cross")

			adaptor := &clientResolverTestAdaptor{
				requestURL: "http://upstream.invalid/v1/messages",
				setupHeader: func(header *http.Header) {
					header.Set("Authorization", "Bearer gateway-owned")
					header.Set("X-OpenCode-Session", "gateway-session")
					header.Set("Content-Type", "application/json")
					header.Set("Accept", "application/json")
				},
			}
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
				ChannelType: channelType,
				HeadersOverride: map[string]any{
					"*":                  "",
					"X-Upstream-Feature": "enabled",
				},
			}}
			transport := &clientResolverTestTransport{response: &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       http.NoBody,
			}}

			response, err := DoApiRequestWithClientLease(
				adaptor,
				ctx,
				info,
				strings.NewReader("{}"),
				func() (*http.Client, func(), error) {
					return &http.Client{Transport: transport}, nil, nil
				},
			)

			require.NoError(t, err)
			require.NotNil(t, response)
			require.NotNil(t, response.Request)
			assert.Equal(t, "Bearer gateway-owned", response.Request.Header.Get("Authorization"))
			assert.Equal(t, "gateway-session", response.Request.Header.Get("X-OpenCode-Session"))
			assert.Equal(t, "application/json", response.Request.Header.Get("Content-Type"))
			assert.Equal(t, "application/json", response.Request.Header.Get("Accept"))
			assert.Equal(t, "tools-2024-04-04", response.Request.Header.Get("Anthropic-Beta"))
			assert.Equal(t, "2023-06-01", response.Request.Header.Get("Anthropic-Version"))
			assert.Equal(t, "enabled", response.Request.Header.Get("X-Upstream-Feature"))
			assert.Empty(t, response.Request.Header.Get("X-Unknown-Client"))
			assert.Equal(t, int32(1), transport.calls.Load())
		})
	}
}

func TestDoRequestWithClient_OpenCodeDelayedInvalidRequestDoesNotCommitSSE(t *testing.T) {
	type requestResult struct {
		response *http.Response
		err      error
	}
	setting := operation_setting.GetGeneralSetting()
	oldPingEnabled := setting.PingIntervalEnabled
	oldPingSeconds := setting.PingIntervalSeconds
	setting.PingIntervalEnabled = true
	setting.PingIntervalSeconds = 1
	t.Cleanup(func() {
		setting.PingIntervalEnabled = oldPingEnabled
		setting.PingIntervalSeconds = oldPingSeconds
	})

	for _, channelType := range []int{
		rootconstant.ChannelTypeOpenCodeGo,
		rootconstant.ChannelTypeOpenCodeAPIKey,
	} {
		for _, statusCode := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
			name := rootconstant.GetChannelTypeName(channelType) + "/" + http.StatusText(statusCode)
			t.Run(name, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				upstreamRequest, requestErr := http.NewRequestWithContext(
					ctx.Request.Context(),
					http.MethodPost,
					"http://upstream.invalid/v1/messages",
					strings.NewReader("{}"),
				)
				require.NoError(t, requestErr)

				roundTripStarted := make(chan struct{})
				releaseStatus := make(chan struct{})
				client := &http.Client{Transport: roundTripTestFunc(func(request *http.Request) (*http.Response, error) {
					close(roundTripStarted)
					<-releaseStatus
					return &http.Response{
						StatusCode: statusCode,
						Header:     make(http.Header),
						Body:       http.NoBody,
						Request:    request,
					}, nil
				})}
				info := &relaycommon.RelayInfo{
					IsStream: true,
					ChannelMeta: &relaycommon.ChannelMeta{
						ChannelType: channelType,
					},
				}
				result := make(chan requestResult, 1)
				go func() {
					response, err := doRequestWithClient(ctx, upstreamRequest, info, client)
					result <- requestResult{response: response, err: err}
				}()

				select {
				case <-roundTripStarted:
				case <-time.After(time.Second):
					close(releaseStatus)
					t.Fatal("timed out waiting for the upstream round trip")
				}
				// Hold the response past the configured ping interval. Before the
				// OpenCode guard, the pre-status pinger committed an SSE response here.
				time.Sleep(1100 * time.Millisecond)

				assert.False(t, ctx.Writer.Written(), "the delayed upstream status must own the downstream status")
				assert.False(t, recorder.Flushed)
				assert.Empty(t, recorder.Body.String())
				assert.Empty(t, recorder.Header().Get("Content-Type"), "SSE headers must be deferred until upstream status classification")

				close(releaseStatus)
				var outcome requestResult
				select {
				case outcome = <-result:
				case <-time.After(time.Second):
					t.Fatal("timed out waiting for the classified upstream response")
				}
				require.NoError(t, outcome.err)
				require.NotNil(t, outcome.response)
				t.Cleanup(func() { _ = outcome.response.Body.Close() })
				assert.Equal(t, statusCode, outcome.response.StatusCode)
				assert.False(t, ctx.Writer.Written())
				assert.False(t, recorder.Flushed)
				assert.Empty(t, recorder.Body.String())
				assert.Empty(t, recorder.Header().Get("Content-Type"))
			})
		}
	}
}

func TestDoOpenCodeRequestWithResponseHeaderTimeoutCancelsBlockedRoundTrip(t *testing.T) {
	request, err := http.NewRequest(http.MethodPost, "http://upstream.invalid/v1/messages", strings.NewReader("{}"))
	require.NoError(t, err)
	client := &http.Client{Transport: roundTripTestFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}

	startedAt := time.Now()
	response, err := doOpenCodeRequestWithResponseHeaderTimeout(client, request, 20*time.Millisecond)

	require.Nil(t, response)
	require.ErrorIs(t, err, errOpenCodeResponseHeaderTimeout)
	assert.Less(t, time.Since(startedAt), time.Second)
}

func TestDoOpenCodeRequestHeaderTimerStopsUntilResponseBodyCloses(t *testing.T) {
	request, err := http.NewRequest(http.MethodPost, "http://upstream.invalid/v1/messages", strings.NewReader("{}"))
	require.NoError(t, err)
	client := &http.Client{Transport: roundTripTestFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    request,
		}, nil
	})}

	response, err := doOpenCodeRequestWithResponseHeaderTimeout(client, request, 20*time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, response)
	responseContext := response.Request.Context()
	time.Sleep(40 * time.Millisecond)
	assert.NoError(t, responseContext.Err(), "header timer must not become a full-stream timeout")
	require.NoError(t, response.Body.Close())
	select {
	case <-responseContext.Done():
	case <-time.After(time.Second):
		t.Fatal("response body close did not release the request context")
	}
}

func TestDoRequestWithClientOpenCodeDoesNotInheritWholeResponseClientTimeout(t *testing.T) {
	for _, channelType := range []int{
		rootconstant.ChannelTypeOpenCodeGo,
		rootconstant.ChannelTypeOpenCodeAPIKey,
	} {
		t.Run(rootconstant.GetChannelTypeName(channelType), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			upstreamRequest, err := http.NewRequestWithContext(
				ctx.Request.Context(),
				http.MethodPost,
				"http://upstream.invalid/v1/messages",
				strings.NewReader("{}"),
			)
			require.NoError(t, err)

			client := &http.Client{
				Timeout: 10 * time.Millisecond,
				Transport: roundTripTestFunc(func(request *http.Request) (*http.Response, error) {
					select {
					case <-time.After(30 * time.Millisecond):
						return &http.Response{
							StatusCode: http.StatusOK,
							Header:     make(http.Header),
							Body:       http.NoBody,
							Request:    request,
						}, nil
					case <-request.Context().Done():
						return nil, request.Context().Err()
					}
				}),
			}
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: channelType}}

			response, err := doRequestWithClient(ctx, upstreamRequest, info, client)
			require.NoError(t, err)
			require.NotNil(t, response)
			require.NoError(t, response.Body.Close())
		})
	}
}

func TestDoRequestWithClientClassifiesOpenCodeHeaderTimeout(t *testing.T) {
	oldDisableEnabled := rootcommon.AutomaticDisableChannelEnabled
	oldDisableRanges := append([]operation_setting.StatusCodeRange(nil), operation_setting.AutomaticDisableStatusCodeRanges...)
	rootcommon.AutomaticDisableChannelEnabled = true
	operation_setting.AutomaticDisableStatusCodeRanges = []operation_setting.StatusCodeRange{{
		Start: http.StatusGatewayTimeout,
		End:   http.StatusGatewayTimeout,
	}}
	t.Cleanup(func() {
		rootcommon.AutomaticDisableChannelEnabled = oldDisableEnabled
		operation_setting.AutomaticDisableStatusCodeRanges = oldDisableRanges
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	upstreamRequest, err := http.NewRequestWithContext(
		ctx.Request.Context(),
		http.MethodPost,
		"http://upstream.invalid/v1/messages",
		strings.NewReader("{}"),
	)
	require.NoError(t, err)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
		ChannelType: rootconstant.ChannelTypeOpenCodeAPIKey,
	}}
	client := &http.Client{Transport: roundTripTestFunc(func(*http.Request) (*http.Response, error) {
		return nil, errOpenCodeResponseHeaderTimeout
	})}

	response, err := doRequestWithClient(ctx, upstreamRequest, info, client)
	require.Nil(t, response)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, types.ErrorOriginLocalDeadline, apiErr.Provenance().Origin)
	assert.Equal(t, "response_header_timeout", apiErr.Provenance().Subtype)
	assert.Zero(t, apiErr.Provenance().RawStatusCode)
	assert.True(t, types.IsSkipRetryError(apiErr))
	assert.False(t, service.IsOpenCodeGoUpstreamRelayError(apiErr))
	_, hasRawStatus := service.OpenCodeGoUpstreamRelayStatusCode(apiErr)
	assert.False(t, hasRawStatus)
	assert.False(t, service.ShouldRetryOpenCodeGoRelayError(rootconstant.ChannelTypeOpenCodeGo, apiErr))
	assert.False(t, service.ShouldDisableChannel(apiErr))
}

func TestPreflightOpenCodeRequestTransportRejectsUnclassifiedMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mutate     func(*http.Request)
		wantStatus int
		wantRule   string
	}{
		{
			name: "query",
			mutate: func(request *http.Request) {
				request.URL.RawQuery = "unknown=value&unknown=second"
			},
			wantStatus: http.StatusBadRequest,
			wantRule:   "query_parameters",
		},
		{
			name: "declared trailer",
			mutate: func(request *http.Request) {
				request.Header.Set("Trailer", "X-Late-Metadata")
			},
			wantStatus: http.StatusBadRequest,
			wantRule:   "request_trailers",
		},
		{
			name: "populated trailer",
			mutate: func(request *http.Request) {
				request.Trailer = http.Header{"X-Late-Metadata": []string{"value"}}
			},
			wantStatus: http.StatusBadRequest,
			wantRule:   "request_trailers",
		},
		{
			name: "undecoded encoding",
			mutate: func(request *http.Request) {
				request.Header.Set("Content-Encoding", "GZIP")
			},
			wantStatus: http.StatusUnsupportedMediaType,
			wantRule:   "content_encoding",
		},
	}

	for _, channelType := range []int{
		rootconstant.ChannelTypeOpenCodeGo,
		rootconstant.ChannelTypeOpenCodeAPIKey,
	} {
		for _, test := range tests {
			t.Run(rootconstant.GetChannelTypeName(channelType)+"/"+test.name, func(t *testing.T) {
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
				test.mutate(ctx.Request)
				info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: channelType}}

				apiErr := PreflightOpenCodeRequestTransport(info, ctx)
				require.NotNil(t, apiErr)
				assert.Equal(t, test.wantStatus, apiErr.StatusCode)
				assert.Equal(t, types.ErrorOriginLocalValidation, apiErr.Provenance().Origin)
				assert.Equal(t, test.wantRule, apiErr.Provenance().Subtype)
				assert.True(t, types.IsSkipRetryError(apiErr))
			})
		}
	}
}

func TestPreflightOpenCodeRequestTransportAcceptsClaudeCodeBetaQuery(t *testing.T) {
	t.Parallel()

	for _, channelType := range []int{
		rootconstant.ChannelTypeOpenCodeGo,
		rootconstant.ChannelTypeOpenCodeAPIKey,
	} {
		t.Run(rootconstant.GetChannelTypeName(channelType), func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader("{}"))
			info := &relaycommon.RelayInfo{
				RelayFormat: types.RelayFormatClaude,
				ChannelMeta: &relaycommon.ChannelMeta{ChannelType: channelType},
			}

			assert.Nil(t, PreflightOpenCodeRequestTransport(info, ctx))
			assert.Equal(t, "beta=true", ctx.Request.URL.RawQuery, "preflight must consume the marker logically without mutating the request")
		})
	}
}

func TestPreflightOpenCodeRequestTransportRejectsUnsupportedBetaVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
	}{
		{name: "false", path: "/v1/messages?beta=false"},
		{name: "uppercase value", path: "/v1/messages?beta=TRUE"},
		{name: "empty value", path: "/v1/messages?beta="},
		{name: "duplicate value", path: "/v1/messages?beta=true&beta=true"},
		{name: "unknown parameter", path: "/v1/messages?client=private-value"},
		{name: "mixed parameter", path: "/v1/messages?beta=true&client=private-value"},
		{name: "malformed escape", path: "/v1/messages?beta=%ZZ"},
		{name: "trailing separator", path: "/v1/messages?beta=true&"},
		{name: "leading separator", path: "/v1/messages?&beta=true"},
		{name: "encoded value", path: "/v1/messages?beta=%74rue"},
		{name: "wrong endpoint", path: "/v1/chat/completions?beta=true"},
		{name: "responses endpoint", path: "/v1/responses?beta=true"},
	}

	for _, channelType := range []int{
		rootconstant.ChannelTypeOpenCodeGo,
		rootconstant.ChannelTypeOpenCodeAPIKey,
	} {
		for _, test := range tests {
			t.Run(rootconstant.GetChannelTypeName(channelType)+"/"+test.name, func(t *testing.T) {
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = httptest.NewRequest(http.MethodPost, test.path, strings.NewReader("{}"))
				info := &relaycommon.RelayInfo{
					RelayFormat: types.RelayFormatClaude,
					ChannelMeta: &relaycommon.ChannelMeta{ChannelType: channelType},
				}

				apiErr := PreflightOpenCodeRequestTransport(info, ctx)
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
				assert.Equal(t, types.ErrorOriginLocalValidation, apiErr.Provenance().Origin)
				assert.Equal(t, "query_parameters", apiErr.Provenance().Subtype)
				assert.True(t, types.IsSkipRetryError(apiErr))
				assert.NotContains(t, apiErr.Error(), "private-value")
			})
		}
	}
}

func TestPreflightOpenCodeRequestTransportHasReachableValidControl(t *testing.T) {
	t.Parallel()

	for _, channelType := range []int{
		rootconstant.ChannelTypeOpenCodeGo,
		rootconstant.ChannelTypeOpenCodeAPIKey,
	} {
		t.Run(rootconstant.GetChannelTypeName(channelType), func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
			ctx.Request.Header.Set("Content-Encoding", "identity")
			ctx.Request.Header.Set("Anthropic-Version", "2023-06-01")
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
				ChannelType: channelType,
				HeadersOverride: map[string]any{
					"*":                  "",
					"X-Upstream-Feature": "enabled",
				},
			}}

			assert.Nil(t, PreflightOpenCodeRequestTransport(info, ctx))
		})
	}
}

func TestPreflightOpenCodeRequestTransportKeepsConfigAndClientOriginsDistinct(t *testing.T) {
	t.Parallel()

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
		ChannelType: rootconstant.ChannelTypeOpenCodeAPIKey,
		HeadersOverride: map[string]any{
			"Authorization": "Bearer operator-value",
		},
	}}

	apiErr := PreflightOpenCodeRequestTransport(info, ctx)
	require.NotNil(t, apiErr)
	assert.Equal(t, types.ErrorOriginGatewayConfig, apiErr.Provenance().Origin)
	assert.Equal(t, "header_override", apiErr.Provenance().Subtype)
	assert.True(t, types.IsSkipRetryError(apiErr))
}

func TestPreflightOpenCodeRequestTransportDoesNotChangeOtherChannels(t *testing.T) {
	t.Parallel()

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions?provider=value", strings.NewReader("{}"))
	ctx.Request.Header.Set("Content-Encoding", "custom")
	ctx.Request.Trailer = http.Header{"X-Late": []string{"value"}}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: rootconstant.ChannelTypeOpenAI}}

	assert.Nil(t, PreflightOpenCodeRequestTransport(info, ctx))
}

func TestPreflightOpenCodeRequestTransportUsesSelectedChannelContextBeforeRelayInit(t *testing.T) {
	t.Parallel()

	for _, channelType := range []int{
		rootconstant.ChannelTypeOpenCodeGo,
		rootconstant.ChannelTypeOpenCodeAPIKey,
	} {
		t.Run(rootconstant.GetChannelTypeName(channelType), func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(
				http.MethodPost,
				"/v1/chat/completions?unclassified=value",
				strings.NewReader("{}"),
			)
			rootcommon.SetContextKey(ctx, rootconstant.ContextKeyChannelType, channelType)
			rootcommon.SetContextKey(ctx, rootconstant.ContextKeyChannelHeaderOverride, map[string]any{
				"Authorization": "Bearer operator-value",
			})

			// This is the production pre-billing shape: the distributor context is
			// populated, while attempt-local ChannelMeta is still nil.
			info := &relaycommon.RelayInfo{}
			apiErr := PreflightOpenCodeRequestTransport(info, ctx)
			require.NotNil(t, apiErr)
			assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
			assert.Equal(t, types.ErrorOriginLocalValidation, apiErr.Provenance().Origin)
			assert.Equal(t, "query_parameters", apiErr.Provenance().Subtype)
			assert.Nil(t, info.ChannelMeta, "preflight must not mutate request-global relay state")
		})
	}
}

func TestPreflightOpenCodeRequestTransportValidatesContextHeaderOverrideBeforeRelayInit(t *testing.T) {
	t.Parallel()

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
	rootcommon.SetContextKey(ctx, rootconstant.ContextKeyChannelType, rootconstant.ChannelTypeOpenCodeAPIKey)
	rootcommon.SetContextKey(ctx, rootconstant.ContextKeyChannelHeaderOverride, map[string]any{
		"Authorization": "Bearer operator-value",
	})

	apiErr := PreflightOpenCodeRequestTransport(&relaycommon.RelayInfo{}, ctx)
	require.NotNil(t, apiErr)
	assert.Equal(t, types.ErrorOriginGatewayConfig, apiErr.Provenance().Origin)
	assert.Equal(t, "header_override", apiErr.Provenance().Subtype)
}

func TestProcessHeaderOverride_RuntimeOverrideIsFinalHeaderMap(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{
		IsChannelTest:             false,
		UseRuntimeHeadersOverride: true,
		RuntimeHeadersOverride: map[string]any{
			"x-static":  "runtime-value",
			"x-runtime": "runtime-only",
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Static": "legacy-value",
				"X-Legacy": "legacy-only",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "runtime-value", headers["x-static"])
	require.Equal(t, "runtime-only", headers["x-runtime"])
	_, exists := headers["x-legacy"]
	require.False(t, exists)
}

func TestProcessHeaderOverride_PassthroughSkipsAcceptEncoding(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")
	ctx.Request.Header.Set("Accept-Encoding", "gzip")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"*": "",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "trace-123", headers["x-trace-id"])

	_, hasAcceptEncoding := headers["accept-encoding"]
	require.False(t, hasAcceptEncoding)
}

func TestProcessHeaderOverride_PassHeadersTemplateSetsRuntimeHeaders(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Request.Header.Set("Originator", "Codex CLI")
	ctx.Request.Header.Set("Session_id", "sess-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		RequestHeaders: map[string]string{
			"Originator": "Codex CLI",
			"Session_id": "sess-123",
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			ParamOverride: map[string]any{
				"operations": []any{
					map[string]any{
						"mode":  "pass_headers",
						"value": []any{"Originator", "Session_id", "X-Codex-Beta-Features"},
					},
				},
			},
			HeadersOverride: map[string]any{
				"X-Static": "legacy-value",
			},
		},
	}

	_, err := relaycommon.ApplyParamOverrideWithRelayInfo([]byte(`{"model":"gpt-4.1"}`), info)
	require.NoError(t, err)
	require.True(t, info.UseRuntimeHeadersOverride)
	require.Equal(t, "Codex CLI", info.RuntimeHeadersOverride["originator"])
	require.Equal(t, "sess-123", info.RuntimeHeadersOverride["session_id"])
	_, exists := info.RuntimeHeadersOverride["x-codex-beta-features"]
	require.False(t, exists)
	require.Equal(t, "legacy-value", info.RuntimeHeadersOverride["x-static"])

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "Codex CLI", headers["originator"])
	require.Equal(t, "sess-123", headers["session_id"])
	_, exists = headers["x-codex-beta-features"]
	require.False(t, exists)

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	applyHeaderOverrideToRequest(upstreamReq, headers)
	require.Equal(t, "Codex CLI", upstreamReq.Header.Get("Originator"))
	require.Equal(t, "sess-123", upstreamReq.Header.Get("Session_id"))
	require.Empty(t, upstreamReq.Header.Get("X-Codex-Beta-Features"))
}
