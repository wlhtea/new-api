package dto

import (
	"testing"

	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/stretchr/testify/require"
)

func TestClaudeRedactedThinkingDataRoundTrip(t *testing.T) {
	const body = `{"model":"test-model","messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"encrypted-state"}]}]}`

	var request ClaudeRequest
	require.NoError(t, kitutil.Unmarshal([]byte(body), &request))
	parts, err := request.Messages[0].ParseContent()
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.Equal(t, "encrypted-state", parts[0].Data)

	roundTrip, err := kitutil.Marshal(request)
	require.NoError(t, err)
	require.JSONEq(t, body, string(roundTrip))
}
