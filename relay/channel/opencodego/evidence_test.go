package opencodego

import (
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validEvidenceResult() RelayEvidenceResult {
	stream := false
	return RelayEvidenceResult{
		CaseID:            "type-63:messages-to-chat:stream-false",
		ControlCaseID:     "type-63:messages-to-chat:control",
		Environment:       "isolated",
		CommitSHA:         strings.Repeat("a", 40),
		ObservedAt:        time.Unix(1, 0).UTC(),
		Evidence:          map[EvidenceGrade]bool{EvidenceE0: true, EvidenceE1: true, EvidenceE2: true, EvidenceE3: true, EvidenceE4: true},
		Claim:             EvidenceClaimSupported,
		ChannelType:       constant.ChannelTypeOpenCodeAPIKey,
		ChannelID:         63,
		Attempt:           1,
		ClientFormat:      "messages",
		FinalProtocol:     ProtocolChat,
		ProtocolSource:    "exact-built-in",
		OriginModel:       "glm-5.2",
		MappedModel:       "glm-5.2",
		ConfigFingerprint: strings.Repeat("b", 64),
		StreamPresence:    "false",
		StreamValue:       &stream,
		Routed:            true,
		Sent:              true,
		Accepted:          true,
		BehaviorObserved:  true,
		PhysicalCallCount: 1,
		CaptureSHA256:     strings.Repeat("c", 64),
		HTTPStatus:        200,
		TerminalObserved:  true,
		ControlPassed:     true,
		ObservableID:      "thinking-disabled-observable-v1",
		Repetitions:       3,
	}
}

func TestRelayEvidenceRejectsFalseSupportSignals(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RelayEvidenceResult)
	}{
		{name: "zero calls", mutate: func(r *RelayEvidenceResult) { r.PhysicalCallCount = 0 }},
		{name: "skipped credential", mutate: func(r *RelayEvidenceResult) { r.Skipped = true }},
		{name: "failed control", mutate: func(r *RelayEvidenceResult) { r.ControlPassed = false }},
		{name: "missing capture", mutate: func(r *RelayEvidenceResult) { r.CaptureSHA256 = "" }},
		{name: "field loss plus 2xx", mutate: func(r *RelayEvidenceResult) { r.Sent = false }},
		{name: "http 200 error envelope", mutate: func(r *RelayEvidenceResult) { r.HTTP200ErrorObserved = true }},
		{name: "stream error", mutate: func(r *RelayEvidenceResult) { r.StreamErrorObserved = true }},
		{name: "missing terminal", mutate: func(r *RelayEvidenceResult) { r.TerminalObserved = false }},
		{name: "mixed retry channels", mutate: func(r *RelayEvidenceResult) { r.MixedChannelAttempts = true }},
		{name: "one-off behavior", mutate: func(r *RelayEvidenceResult) { r.Repetitions = 1 }},
		{name: "missing authority", mutate: func(r *RelayEvidenceResult) { r.Evidence[EvidenceE0] = false }},
		{name: "mock claims real evidence", mutate: func(r *RelayEvidenceResult) { r.Environment = "mock" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := validEvidenceResult()
			test.mutate(&result)
			assert.Error(t, result.Validate())
		})
	}
}

func TestRelayEvidenceRequiresPairedControlForPolicyRejection(t *testing.T) {
	result := validEvidenceResult()
	result.Claim = EvidenceClaimPolicyRejected
	result.PolicyRejected = true
	result.RejectionRuleID = "model.glm-5.3.chat.thinking-disabled"
	result.RejectionStageID = "preflight.capability"
	result.Routed = false
	result.Sent = false
	result.Accepted = false
	result.BehaviorObserved = false
	result.PhysicalCallCount = 0
	result.CaptureSHA256 = ""
	result.HTTPStatus = 400
	result.TerminalObserved = false
	result.ObservableID = ""
	result.Repetitions = 0

	require.NoError(t, result.Validate())
	result.ControlPassed = false
	assert.Error(t, result.Validate())
}

func TestEvidenceManifestsHaveIndependentCompleteCardinality(t *testing.T) {
	base := BaseRouterEvidenceManifest()
	protocol := ProtocolEvidenceManifest()
	models := ModelInventoryEvidenceManifest(ModelList)

	require.Len(t, base, 18)
	require.Len(t, protocol, 36)
	require.Len(t, models, 19)
	assertUniqueEvidenceIDs(t, base, func(value BaseRouterEvidenceCase) string { return value.ID })
	assertUniqueEvidenceIDs(t, protocol, func(value ProtocolEvidenceCase) string { return value.ID })
	assertUniqueEvidenceIDs(t, models, func(value ModelInventoryEvidenceCase) string { return value.ID })
}

func assertUniqueEvidenceIDs[T any](t *testing.T, values []T, id func(T) string) {
	t.Helper()
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		caseID := id(value)
		require.NotEmpty(t, caseID)
		_, duplicate := seen[caseID]
		require.False(t, duplicate, "duplicate evidence case %q", caseID)
		seen[caseID] = struct{}{}
	}
}
