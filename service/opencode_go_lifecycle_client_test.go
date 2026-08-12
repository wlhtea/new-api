package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCodeGoLifecycleClientUsesVerifiedUpstreamProtocols(t *testing.T) {
	chinaActionID := strings.Repeat("c", 64)
	referralActionID := strings.Repeat("a", 64)
	billingActionID := strings.Repeat("b", 64)
	var chinaCalls atomic.Int32
	var referralCalls atomic.Int32
	var billingActionCalls atomic.Int32
	var subscriptionReads atomic.Int32
	var cancellationCalls atomic.Int32

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/assets/go-route.js":
			_, _ = writer.Write([]byte(
				`createServerReference("` + referralActionID + `");"go.referral.reward.apply";` +
					`const createSessionUrl_action = createServerReference("` + billingActionID + `");`,
			))
		case "/_server":
			require.Equal(t, "auth=synthetic-cookie; oc_locale=zh", request.Header.Get("Cookie"))
			switch request.URL.Query().Get("id") {
			case chinaActionID:
				chinaCalls.Add(1)
				require.NoError(t, request.ParseForm())
				assert.Equal(t, "wrk_TEST", request.Form.Get("workspaceID"))
				assert.Equal(t, "false", request.Form.Get("useChinaProviders"))
				writer.Header().Set("Location", "/workspace/wrk_TEST/go")
				writer.WriteHeader(http.StatusFound)
			case "":
				var payload openCodeGoServerPayload
				require.NoError(t, common.DecodeJson(request.Body, &payload))
				assert.Equal(t, 9, payload.Tuple.Type)
				assert.Zero(t, payload.Tuple.Index)
				assert.Equal(t, 2, payload.Tuple.Length)
				assert.Zero(t, payload.Tuple.Offset)
				require.Len(t, payload.Tuple.Args, 2)
				assert.Equal(t, 31, payload.Flags)
				require.NotNil(t, payload.Meta)
				assert.Empty(t, payload.Meta)
				switch request.Header.Get("X-Server-Id") {
				case referralActionID:
					referralCalls.Add(1)
					assert.Equal(t, "server-fn:1", request.Header.Get("X-Server-Instance"))
					assert.Equal(t, "true", request.Header.Get("X-Single-Flight"))
					assert.Equal(t, server.URL, request.Header.Get("Origin"))
					assert.Equal(t, "wrk_TEST", payload.Tuple.Args[0].Value)
					assert.Equal(t, "ref_TEST", payload.Tuple.Args[1].Value)
					_, _ = writer.Write([]byte(`;0x00000055;((self.$R=self.$R||{})["server-fn:1"]=[],($R=>$R[0]={amount:500})($R["server-fn:1"]))`))
				case billingActionID:
					billingActionCalls.Add(1)
					assert.Equal(t, "server-fn:0", request.Header.Get("X-Server-Instance"))
					assert.Empty(t, request.Header.Get("X-Single-Flight"))
					assert.Equal(t, "wrk_TEST", payload.Tuple.Args[0].Value)
					assert.Equal(t, server.URL+"/workspace/wrk_TEST/go", payload.Tuple.Args[1].Value)
					_, _ = writer.Write([]byte(`;data:"` + server.URL + `/p/session/testtoken"`))
				default:
					http.Error(writer, "unknown action", http.StatusBadRequest)
				}
			default:
				http.Error(writer, "unknown form action", http.StatusBadRequest)
			}
		case "/p/session/testtoken":
			_, _ = writer.Write([]byte(`<script type="application/json" id="preloaded_json">{&quot;session_api_key&quot;:&quot;ek_test&quot;,&quot;portal_session_id&quot;:&quot;bps_test&quot;}</script>`))
		case "/v1/billing_portal/sessions/bps_test/subscriptions/sub_TEST":
			require.Equal(t, "Bearer ek_test", request.Header.Get("Authorization"))
			reads := subscriptionReads.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"id":"sub_TEST","cancel_at_period_end":` + fmt.Sprint(reads > 1) + `,"current_period_end":1900100000}`))
		case "/v1/billing_portal/sessions/bps_test/subscriptions/sub_TEST/cancel":
			cancellationCalls.Add(1)
			require.Equal(t, http.MethodPost, request.Method)
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"id":"sub_TEST"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	console, err := newOpenCodeGoConsoleClient(server.URL, server.URL+"/zen/go/v1", server.Client())
	require.NoError(t, err)
	client, err := newOpenCodeGoLifecycleClient(console, server.URL, server.Client())
	require.NoError(t, err)
	chinaEnabled := false
	page := &OpenCodeGoConsolePage{
		WorkspaceID:           "wrk_TEST",
		SubscriptionReference: "sub_TEST",
		ChinaModelsEnabled:    &chinaEnabled,
		ChinaModelsServerID:   chinaActionID,
		RouteModuleAssets:     []string{"/assets/go-route.js"},
	}

	require.NoError(t, client.EnableChinaModels(context.Background(), "synthetic-cookie", page))
	amount, err := client.ApplyReferralReward(context.Background(), "synthetic-cookie", page, "ref_TEST")
	require.NoError(t, err)
	assert.Equal(t, 500, amount)
	cancellation, err := client.CancelSubscriptionRenewal(context.Background(), "synthetic-cookie", page)
	require.NoError(t, err)
	assert.False(t, cancellation.AlreadyCancelled)
	assert.Equal(t, int64(1_900_100_000), cancellation.CurrentPeriodEnd)
	assert.Equal(t, int32(1), chinaCalls.Load())
	assert.Equal(t, int32(1), referralCalls.Load())
	assert.Equal(t, int32(1), billingActionCalls.Load())
	assert.Equal(t, int32(2), subscriptionReads.Load())
	assert.Equal(t, int32(1), cancellationCalls.Load())
}

func TestOpenCodeGoLifecycleClientDoesNotRetryFailedMutation(t *testing.T) {
	actionID := strings.Repeat("a", 64)
	var mutationCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/assets/go-route.js" {
			_, _ = writer.Write([]byte(`createServerReference("` + actionID + `");"go.referral.reward.apply"`))
			return
		}
		if request.URL.Path == "/_server" {
			mutationCalls.Add(1)
			writer.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(writer, "temporary")
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()
	console, err := newOpenCodeGoConsoleClient(server.URL, server.URL+"/zen/go/v1", server.Client())
	require.NoError(t, err)
	client, err := newOpenCodeGoLifecycleClient(console, server.URL, server.Client())
	require.NoError(t, err)

	_, err = client.ApplyReferralReward(context.Background(), "synthetic-cookie", &OpenCodeGoConsolePage{
		WorkspaceID:       "wrk_TEST",
		RouteModuleAssets: []string{"/assets/go-route.js"},
	}, "ref_TEST")
	require.Error(t, err)
	assert.Equal(t, int32(1), mutationCalls.Load())
}

func TestOpenCodeGoLifecycleClientDoesNotFollowReferralMutationRedirects(t *testing.T) {
	actionID := strings.Repeat("a", 64)
	var mutationCalls atomic.Int32
	var redirectedCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/assets/go-route.js":
			_, _ = writer.Write([]byte(`createServerReference("` + actionID + `");"go.referral.reward.apply"`))
		case "/_server":
			mutationCalls.Add(1)
			writer.Header().Set("Location", "/redirected")
			writer.WriteHeader(http.StatusTemporaryRedirect)
		case "/redirected":
			redirectedCalls.Add(1)
			_, _ = writer.Write([]byte(`;0x1;((self.$R=self.$R||{})["server-fn:1"]=[],($R=>$R[0]={amount:500})($R["server-fn:1"]))`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	console, err := newOpenCodeGoConsoleClient(server.URL, server.URL+"/zen/go/v1", server.Client())
	require.NoError(t, err)
	client, err := newOpenCodeGoLifecycleClient(console, server.URL, server.Client())
	require.NoError(t, err)

	_, err = client.ApplyReferralReward(context.Background(), "synthetic-cookie", &OpenCodeGoConsolePage{
		WorkspaceID:       "wrk_TEST",
		RouteModuleAssets: []string{"/assets/go-route.js"},
	}, "ref_TEST")
	require.ErrorContains(t, err, "status 307")
	assert.Equal(t, int32(1), mutationCalls.Load())
	assert.Zero(t, redirectedCalls.Load())
}

func TestParseOpenCodeGoReferralApplyAmountRequiresPositiveExpectedResult(t *testing.T) {
	valid := `;0x00000055;((self.$R=self.$R||{})["server-fn:1"]=[],($R=>$R[0]={amount:500})($R["server-fn:1"]))`
	amount, err := parseOpenCodeGoReferralApplyAmount([]byte(valid))
	require.NoError(t, err)
	assert.Equal(t, 500, amount)

	for _, test := range []struct {
		name string
		body string
	}{
		{name: "zero", body: strings.Replace(valid, "amount:500", "amount:0", 1)},
		{name: "negative", body: strings.Replace(valid, "amount:500", "amount:-1", 1)},
		{name: "fraction", body: strings.Replace(valid, "amount:500", "amount:1.5", 1)},
		{name: "string", body: strings.Replace(valid, "amount:500", `amount:"500"`, 1)},
		{name: "wrong slot", body: strings.ReplaceAll(valid, "server-fn:1", "server-fn:0")},
		{name: "decoy", body: "ok amount:500"},
		{name: "duplicate", body: strings.Replace(valid, "amount:500", "amount:500,amount:900", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, parseErr := parseOpenCodeGoReferralApplyAmount([]byte(test.body))
			require.Error(t, parseErr)
		})
	}
}

func TestOpenCodeGoLifecycleClientDoesNotRetryStripeCancellationForVersionNegotiation(t *testing.T) {
	billingActionID := strings.Repeat("b", 64)
	var cancellationCalls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/assets/go-route.js":
			_, _ = writer.Write([]byte(`const createSessionUrl_action = createServerReference("` + billingActionID + `");`))
		case "/_server":
			_, _ = writer.Write([]byte(`;data:"` + server.URL + `/p/session/testtoken"`))
		case "/p/session/testtoken":
			_, _ = writer.Write([]byte(`<script type="application/json" id="preloaded_json">{&quot;session_api_key&quot;:&quot;ek_test&quot;,&quot;portal_session_id&quot;:&quot;bps_test&quot;}</script>`))
		case "/v1/billing_portal/sessions/bps_test/subscriptions/sub_TEST":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"id":"sub_TEST","cancel_at_period_end":false,"current_period_end":1900100000}`))
		case "/v1/billing_portal/sessions/bps_test/subscriptions/sub_TEST/cancel":
			cancellationCalls.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"error":{"message":"Please use Stripe-Version 2027-01-01.future"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	console, err := newOpenCodeGoConsoleClient(server.URL, server.URL+"/zen/go/v1", server.Client())
	require.NoError(t, err)
	client, err := newOpenCodeGoLifecycleClient(console, server.URL, server.Client())
	require.NoError(t, err)
	_, err = client.CancelSubscriptionRenewal(context.Background(), "synthetic-cookie", &OpenCodeGoConsolePage{
		WorkspaceID:           "wrk_TEST",
		SubscriptionReference: "sub_TEST",
		RouteModuleAssets:     []string{"/assets/go-route.js"},
	})
	require.ErrorContains(t, err, "status 400")
	assert.Equal(t, int32(1), cancellationCalls.Load())
}
