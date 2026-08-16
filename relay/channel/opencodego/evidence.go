package opencodego

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/constant"
)

type EvidenceGrade string

const (
	EvidenceE0 EvidenceGrade = "E0"
	EvidenceE1 EvidenceGrade = "E1"
	EvidenceE2 EvidenceGrade = "E2"
	EvidenceE3 EvidenceGrade = "E3"
	EvidenceE4 EvidenceGrade = "E4"
)

type EvidenceClaim string

const (
	EvidenceClaimPolicyRejected EvidenceClaim = "policy_rejected"
	EvidenceClaimRouted         EvidenceClaim = "routed"
	EvidenceClaimSent           EvidenceClaim = "sent"
	EvidenceClaimAccepted       EvidenceClaim = "accepted"
	EvidenceClaimRejected       EvidenceClaim = "rejected_observed"
	EvidenceClaimBehavior       EvidenceClaim = "behavior_observed"
	EvidenceClaimSupported      EvidenceClaim = "supported"
)

// RelayEvidenceResult deliberately keeps routing, wire capture, acceptance,
// and behavior as independent observations. It contains only safe identifiers
// and fingerprints; request bodies, credentials, and private endpoints do not
// belong in this schema.
type RelayEvidenceResult struct {
	CaseID               string                 `json:"case_id"`
	ControlCaseID        string                 `json:"control_case_id,omitempty"`
	Environment          string                 `json:"environment"`
	CommitSHA            string                 `json:"commit_sha"`
	ObservedAt           time.Time              `json:"observed_at"`
	Evidence             map[EvidenceGrade]bool `json:"evidence"`
	Claim                EvidenceClaim          `json:"claim"`
	ChannelType          int                    `json:"channel_type"`
	ChannelID            int                    `json:"channel_id"`
	Attempt              int                    `json:"attempt"`
	ClientFormat         string                 `json:"client_format"`
	FinalProtocol        Protocol               `json:"final_protocol"`
	ProtocolSource       string                 `json:"protocol_source"`
	OriginModel          string                 `json:"origin_model"`
	MappedModel          string                 `json:"mapped_model"`
	ConfigFingerprint    string                 `json:"config_fingerprint"`
	StreamPresence       string                 `json:"stream_presence"`
	StreamValue          *bool                  `json:"stream_value,omitempty"`
	PolicyRejected       bool                   `json:"policy_rejected"`
	RejectionRuleID      string                 `json:"rejection_rule_id,omitempty"`
	RejectionStageID     string                 `json:"rejection_stage_id,omitempty"`
	Routed               bool                   `json:"routed"`
	Sent                 bool                   `json:"sent"`
	Accepted             bool                   `json:"accepted"`
	RejectedObserved     bool                   `json:"rejected_observed"`
	BehaviorObserved     bool                   `json:"behavior_observed"`
	PhysicalCallCount    int                    `json:"physical_call_count"`
	CaptureSHA256        string                 `json:"capture_sha256,omitempty"`
	HTTPStatus           int                    `json:"http_status,omitempty"`
	TerminalObserved     bool                   `json:"terminal_observed"`
	HTTP200ErrorObserved bool                   `json:"http_200_error_observed"`
	StreamErrorObserved  bool                   `json:"stream_error_observed"`
	Skipped              bool                   `json:"skipped"`
	ControlPassed        bool                   `json:"control_passed"`
	MixedChannelAttempts bool                   `json:"mixed_channel_attempts"`
	ObservableID         string                 `json:"observable_id,omitempty"`
	Repetitions          int                    `json:"repetitions"`
}

func (r RelayEvidenceResult) Validate() error {
	for name, value := range map[string]string{
		"case_id":            r.CaseID,
		"environment":        r.Environment,
		"commit_sha":         r.CommitSHA,
		"client_format":      r.ClientFormat,
		"origin_model":       r.OriginModel,
		"mapped_model":       r.MappedModel,
		"protocol_source":    r.ProtocolSource,
		"config_fingerprint": r.ConfigFingerprint,
		"stream_presence":    r.StreamPresence,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("evidence %s is required", name)
		}
	}
	if r.ObservedAt.IsZero() {
		return fmt.Errorf("evidence observed_at is required")
	}
	if r.ChannelType != constant.ChannelTypeOpenCodeGo && r.ChannelType != constant.ChannelTypeOpenCodeAPIKey {
		return fmt.Errorf("unsupported evidence channel type %d", r.ChannelType)
	}
	if r.ChannelID <= 0 || r.Attempt <= 0 {
		return fmt.Errorf("evidence requires positive channel_id and attempt")
	}
	if r.FinalProtocol != ProtocolChat && r.FinalProtocol != ProtocolMessages && r.FinalProtocol != ProtocolResponses {
		return fmt.Errorf("unsupported final protocol %q", r.FinalProtocol)
	}
	if r.StreamPresence != "absent" && r.StreamPresence != "false" && r.StreamPresence != "true" {
		return fmt.Errorf("unsupported stream presence %q", r.StreamPresence)
	}
	if r.StreamPresence == "absent" && r.StreamValue != nil {
		return fmt.Errorf("absent stream cannot carry a value")
	}
	if r.StreamPresence != "absent" && r.StreamValue == nil {
		return fmt.Errorf("present stream requires a value")
	}
	if r.Skipped {
		return fmt.Errorf("skipped evidence cannot establish a claim")
	}
	if r.MixedChannelAttempts {
		return fmt.Errorf("mixed-channel attempts invalidate evidence")
	}
	if r.PolicyRejected {
		if r.RejectionRuleID == "" || r.RejectionStageID == "" || r.ControlCaseID == "" || !r.ControlPassed {
			return fmt.Errorf("policy rejection requires a passing paired control and exact rule/stage")
		}
		if r.PhysicalCallCount != 0 || r.Sent || r.Accepted || r.BehaviorObserved {
			return fmt.Errorf("policy rejection must have zero calls and no wire/behavior claims")
		}
	}
	if r.Routed && r.PhysicalCallCount == 0 && !r.PolicyRejected {
		return fmt.Errorf("routed evidence requires a physical call or a typed policy rejection")
	}
	if r.Sent {
		if !r.Routed || r.PhysicalCallCount <= 0 || r.CaptureSHA256 == "" || !r.Evidence[EvidenceE2] {
			return fmt.Errorf("sent requires routing, a physical call, E2, and capture SHA")
		}
	}
	if r.Accepted {
		if !r.Sent || r.HTTPStatus < 200 || r.HTTPStatus >= 300 || r.HTTP200ErrorObserved || r.StreamErrorObserved || !r.TerminalObserved {
			return fmt.Errorf("accepted requires captured 2xx success with a valid terminal and no error envelope/event")
		}
	}
	if r.BehaviorObserved {
		if !r.Accepted || !r.Evidence[EvidenceE4] || r.ObservableID == "" || r.Repetitions < 2 || r.ControlCaseID == "" || !r.ControlPassed {
			return fmt.Errorf("behavior requires accepted repeated E4 observations and a passing control")
		}
	}
	if r.Environment == "mock" && (r.Evidence[EvidenceE3] || r.Evidence[EvidenceE4]) {
		return fmt.Errorf("mock evidence cannot claim E3 or E4")
	}
	return r.validateClaim()
}

func (r RelayEvidenceResult) validateClaim() error {
	switch r.Claim {
	case EvidenceClaimPolicyRejected:
		if !r.PolicyRejected || !r.Evidence[EvidenceE1] {
			return fmt.Errorf("policy_rejected claim requires E1 policy evidence")
		}
	case EvidenceClaimRouted:
		if !r.Routed || !r.Evidence[EvidenceE1] {
			return fmt.Errorf("routed claim requires E1 routing evidence")
		}
	case EvidenceClaimSent:
		if !r.Sent {
			return fmt.Errorf("sent claim is not observed")
		}
	case EvidenceClaimAccepted:
		if !r.Accepted || !r.Evidence[EvidenceE3] {
			return fmt.Errorf("accepted claim requires E3")
		}
	case EvidenceClaimRejected:
		if !r.Sent || !r.RejectedObserved || !r.Evidence[EvidenceE3] {
			return fmt.Errorf("rejected_observed claim requires sent E3 rejection evidence")
		}
	case EvidenceClaimBehavior:
		if !r.BehaviorObserved {
			return fmt.Errorf("behavior_observed claim is not established")
		}
	case EvidenceClaimSupported:
		if !r.BehaviorObserved {
			return fmt.Errorf("supported claim requires behavior evidence")
		}
		for _, grade := range []EvidenceGrade{EvidenceE0, EvidenceE1, EvidenceE2, EvidenceE3, EvidenceE4} {
			if !r.Evidence[grade] {
				return fmt.Errorf("supported claim is missing %s", grade)
			}
		}
	default:
		return fmt.Errorf("unsupported evidence claim %q", r.Claim)
	}
	return nil
}

type BaseRouterEvidenceCase struct {
	ID             string `json:"id"`
	ChannelType    int    `json:"channel_type"`
	Endpoint       string `json:"endpoint"`
	StreamPresence string `json:"stream_presence"`
}

func BaseRouterEvidenceManifest() []BaseRouterEvidenceCase {
	channelTypes := []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey}
	endpoints := []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"}
	streamStates := []string{"absent", "false", "true"}
	cases := make([]BaseRouterEvidenceCase, 0, len(channelTypes)*len(endpoints)*len(streamStates))
	for _, channelType := range channelTypes {
		for _, endpoint := range endpoints {
			for _, streamState := range streamStates {
				cases = append(cases, BaseRouterEvidenceCase{
					ID:             fmt.Sprintf("type-%d:%s:stream-%s", channelType, strings.TrimPrefix(endpoint, "/v1/"), streamState),
					ChannelType:    channelType,
					Endpoint:       endpoint,
					StreamPresence: streamState,
				})
			}
		}
	}
	return cases
}

type ProtocolEvidenceCase struct {
	ID            string   `json:"id"`
	ChannelType   int      `json:"channel_type"`
	ClientFormat  string   `json:"client_format"`
	FinalProtocol Protocol `json:"final_protocol"`
	Stream        bool     `json:"stream"`
}

func ProtocolEvidenceManifest() []ProtocolEvidenceCase {
	channelTypes := []int{constant.ChannelTypeOpenCodeGo, constant.ChannelTypeOpenCodeAPIKey}
	clientFormats := []string{"messages", "chat", "responses"}
	protocols := []Protocol{ProtocolChat, ProtocolMessages, ProtocolResponses}
	streamModes := []bool{false, true}
	cases := make([]ProtocolEvidenceCase, 0, len(channelTypes)*len(clientFormats)*len(protocols)*len(streamModes))
	for _, channelType := range channelTypes {
		for _, clientFormat := range clientFormats {
			for _, protocol := range protocols {
				for _, stream := range streamModes {
					cases = append(cases, ProtocolEvidenceCase{
						ID:            fmt.Sprintf("type-%d:%s-to-%s:stream-%t", channelType, clientFormat, protocol, stream),
						ChannelType:   channelType,
						ClientFormat:  clientFormat,
						FinalProtocol: protocol,
						Stream:        stream,
					})
				}
			}
		}
	}
	return cases
}

type ModelInventoryEvidenceCase struct {
	ID    string `json:"id"`
	Model string `json:"model"`
}

func ModelInventoryEvidenceManifest(models []string) []ModelInventoryEvidenceCase {
	normalized := make([]string, 0, len(models))
	for _, model := range models {
		normalized = append(normalized, strings.ToLower(strings.TrimSpace(model)))
	}
	sort.Strings(normalized)
	cases := make([]ModelInventoryEvidenceCase, 0, len(normalized))
	for _, model := range normalized {
		cases = append(cases, ModelInventoryEvidenceCase{ID: "model:" + model, Model: model})
	}
	return cases
}
