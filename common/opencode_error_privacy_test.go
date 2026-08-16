package common

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/require"
)

func TestOpenCodeGoErrorPrivacyDetectsAdditionalUpstreamIdentifiers(t *testing.T) {
	tests := []string{
		"invalid tool schema (request_id: req_private)",
		"traceId=trace_private",
		"correlation_id: correlation_private",
		"workspace=workspace_private",
		"workspaceId: workspace_private_camel",
		"endpoint=provider-private.example/v1",
		"upstream_request_id=req_upstream_private",
		"proxy_host=proxy.internal",
		"upstream key sk-private-key-value",
		"Open/Code upstream rejected the request",
		"Open.Code upstream rejected the request",
		"Console.Go upstream rejected the request",
		"work.space is unavailable",
		`{"requestId":"req_private"}`,
		`{"trace_id":"trace_private"}`,
		`{"proxyHost":"proxy.internal"}`,
	}

	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			require.True(t, OpenCodeGoErrorHasPrivateDetail(input))
			require.Equal(t, constant.OpenCodeGoPublicInvalidRequestMessage,
				OpenCodeGoPublicClientRequestMessage(input))
		})
	}
}

func TestRedactOpenCodeGoPrivateErrorJSONFieldsRedactsAdditionalIdentifiers(t *testing.T) {
	redacted := RedactOpenCodeGoPrivateErrorJSONFields(
		`{"request_id":"req_private","upstreamRequestId":"req_upstream_private","traceId":"trace_private","workspace_id":"workspace_private","endpointUrl":"https://provider-private.example/v1","proxy_host":"proxy.internal","message":"safe detail"}`,
	)

	for _, secret := range []string{
		"req_private", "req_upstream_private", "trace_private", "workspace_private",
		"provider-private.example", "proxy.internal",
	} {
		require.NotContains(t, redacted, secret)
	}
	require.Contains(t, redacted, "[redacted]")
	require.True(t, strings.Contains(redacted, "safe detail"))
}
