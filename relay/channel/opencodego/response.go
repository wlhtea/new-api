package opencodego

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type normalizedCostUsage struct {
	InputTokens        int `json:"inputTokens"`
	OutputTokens       int `json:"outputTokens"`
	ReasoningTokens    int `json:"reasoningTokens"`
	CacheReadTokens    int `json:"cacheReadTokens"`
	CacheWrite5mTokens int `json:"cacheWrite5mTokens"`
	CacheWrite1hTokens int `json:"cacheWrite1hTokens"`
}

type responseTransformState struct {
	model            string
	protocol         Protocol
	namespaceTools   map[string]openCodeGoNamespaceTool
	sawStandardUsage bool
	fallbackUsage    *normalizedCostUsage
}

func prepareResponseForRelay(resp *http.Response, state *responseTransformState, stream bool) error {
	if resp == nil || resp.Body == nil || state == nil {
		return nil
	}
	if stream || strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		resp.Body = newTransformingReadCloser(resp.Body, state)
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	body = state.transformJSON(body, false)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

type transformingReadCloser struct {
	source  io.ReadCloser
	reader  *bufio.Reader
	state   *responseTransformState
	pending []byte
}

func newTransformingReadCloser(source io.ReadCloser, state *responseTransformState) io.ReadCloser {
	return &transformingReadCloser{
		source: source,
		reader: bufio.NewReader(source),
		state:  state,
	}
}

func (r *transformingReadCloser) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		line, err := r.reader.ReadBytes('\n')
		if len(line) > 0 {
			r.pending = r.transformLine(line)
		}
		if len(r.pending) > 0 {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *transformingReadCloser) Close() error {
	return r.source.Close()
}

func (r *transformingReadCloser) transformLine(line []byte) []byte {
	ending := []byte{}
	content := line
	if bytes.HasSuffix(content, []byte("\n")) {
		ending = []byte("\n")
		content = content[:len(content)-1]
		if bytes.HasSuffix(content, []byte("\r")) {
			ending = []byte("\r\n")
			content = content[:len(content)-1]
		}
	}
	if !bytes.HasPrefix(content, []byte("data:")) {
		return line
	}
	payload := bytes.TrimSpace(content[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || !gjson.ValidBytes(payload) {
		return line
	}

	transformed := r.state.transformJSON(payload, true)
	if transformed == nil {
		return nil
	}
	result := make([]byte, 0, len(transformed)+len(ending)+6)
	result = append(result, "data: "...)
	result = append(result, transformed...)
	result = append(result, ending...)
	return result
}

func (s *responseTransformState) transformJSON(data []byte, stream bool) []byte {
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return data
	}
	if isCostExtension(data) {
		s.captureFallbackUsage(data)
		if stream && s.protocol == ProtocolChat && !s.sawStandardUsage && s.fallbackUsage != nil {
			return s.standardChatUsageFrame()
		}
		if stream {
			return nil
		}
	}
	if hasStandardUsage(data) {
		s.sawStandardUsage = true
	}
	data = s.restoreOpenCodeGoNamespaceCalls(data)
	return normalizeResponseModel(data, s.model)
}

// restoreOpenCodeGoNamespaceCalls reverses the request lowering for native
// Responses payloads. The provider sees a flat function name, while Codex
// expects the original child name and namespace on output items. Walking the
// whole event also covers response.output_item.added/done and completed JSON.
func (s *responseTransformState) restoreOpenCodeGoNamespaceCalls(data []byte) []byte {
	if s == nil || len(s.namespaceTools) == 0 || len(data) == 0 {
		return data
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return data
	}
	changed := restoreOpenCodeGoNamespaceValue(value, s.namespaceTools)
	if !changed {
		return data
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return data
	}
	return bytes.TrimSuffix(encoded.Bytes(), []byte("\n"))
}

func restoreOpenCodeGoNamespaceValue(value any, names map[string]openCodeGoNamespaceTool) bool {
	changed := false
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			changed = restoreOpenCodeGoNamespaceValue(item, names) || changed
		}
	case map[string]any:
		if strings.TrimSpace(stringValue(typed["type"])) == "function_call" {
			if entry, ok := names[strings.TrimSpace(stringValue(typed["name"]))]; ok {
				typed["name"] = entry.Name
				typed["namespace"] = entry.Namespace
				changed = true
			}
		}
		for _, child := range typed {
			changed = restoreOpenCodeGoNamespaceValue(child, names) || changed
		}
	}
	return changed
}

func isCostExtension(data []byte) bool {
	if strings.EqualFold(gjson.GetBytes(data, "x-opencode-type").String(), "inference-cost") {
		return true
	}
	return strings.EqualFold(gjson.GetBytes(data, "type").String(), "ping") &&
		gjson.GetBytes(data, "cost").Exists()
}

func hasStandardUsage(data []byte) bool {
	paths := []string{
		"usage.prompt_tokens",
		"usage.input_tokens",
		"usage.output_tokens",
		"message.usage.input_tokens",
		"message.usage.output_tokens",
		"response.usage.input_tokens",
		"response.usage.output_tokens",
	}
	for _, path := range paths {
		if gjson.GetBytes(data, path).Exists() {
			return true
		}
	}
	return false
}

func normalizeResponseModel(data []byte, model string) []byte {
	if model == "" {
		return data
	}
	for _, path := range []string{"model", "message.model", "response.model"} {
		if !gjson.GetBytes(data, path).Exists() {
			continue
		}
		updated, err := sjson.SetBytes(data, path, model)
		if err == nil {
			data = updated
		}
	}
	return data
}

func (s *responseTransformState) captureFallbackUsage(data []byte) {
	raw := gjson.GetBytes(data, "normalizedUsage")
	if !raw.Exists() || !raw.IsObject() {
		return
	}
	var usage normalizedCostUsage
	if err := common.Unmarshal([]byte(raw.Raw), &usage); err != nil {
		return
	}
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.CacheReadTokens == 0 &&
		usage.CacheWrite5mTokens == 0 && usage.CacheWrite1hTokens == 0 {
		return
	}
	s.fallbackUsage = &usage
}

func (s *responseTransformState) standardChatUsageFrame() []byte {
	usage := s.fallbackUsage.toUsage(ProtocolChat)
	payload := struct {
		Model   string     `json:"model,omitempty"`
		Choices []struct{} `json:"choices"`
		Usage   struct {
			PromptTokens     int                    `json:"prompt_tokens"`
			CompletionTokens int                    `json:"completion_tokens"`
			TotalTokens      int                    `json:"total_tokens"`
			PromptDetails    dto.InputTokenDetails  `json:"prompt_tokens_details"`
			OutputDetails    dto.OutputTokenDetails `json:"completion_tokens_details"`
		} `json:"usage"`
	}{Model: s.model, Choices: []struct{}{}}
	payload.Usage.PromptTokens = usage.PromptTokens
	payload.Usage.CompletionTokens = usage.CompletionTokens
	payload.Usage.TotalTokens = usage.TotalTokens
	payload.Usage.PromptDetails = usage.PromptTokensDetails
	payload.Usage.OutputDetails = usage.CompletionTokenDetails
	encoded, err := common.Marshal(payload)
	if err != nil {
		return nil
	}
	return encoded
}

func (u *normalizedCostUsage) toUsage(protocol Protocol) *dto.Usage {
	if u == nil {
		return nil
	}
	cacheWrite := u.CacheWrite5mTokens + u.CacheWrite1hTokens
	totalInput := u.InputTokens + u.CacheReadTokens + cacheWrite
	usage := &dto.Usage{
		PromptTokens:                totalInput,
		CompletionTokens:            u.OutputTokens,
		TotalTokens:                 totalInput + u.OutputTokens,
		InputTokens:                 totalInput,
		OutputTokens:                u.OutputTokens,
		UsageSource:                 "opencode_go_inference_cost",
		ClaudeCacheCreation5mTokens: u.CacheWrite5mTokens,
		ClaudeCacheCreation1hTokens: u.CacheWrite1hTokens,
	}
	usage.PromptTokensDetails.CachedTokens = u.CacheReadTokens
	usage.PromptTokensDetails.CachedCreationTokens = cacheWrite
	usage.PromptTokensDetails.CacheWriteTokens = cacheWrite
	usage.CompletionTokenDetails.ReasoningTokens = u.ReasoningTokens
	usage.PromptCacheHitTokens = u.CacheReadTokens

	switch protocol {
	case ProtocolMessages:
		usage.PromptTokens = u.InputTokens
		usage.TotalTokens = u.InputTokens + u.OutputTokens
		usage.UsageSemantic = dto.BillingUsageSemanticAnthropic
		usage.BillingUsage = dto.NewClaudeMessagesBillingUsage(&dto.ClaudeUsage{
			InputTokens:                 u.InputTokens,
			CacheReadInputTokens:        u.CacheReadTokens,
			CacheCreationInputTokens:    cacheWrite,
			OutputTokens:                u.OutputTokens,
			ClaudeCacheCreation5mTokens: u.CacheWrite5mTokens,
			ClaudeCacheCreation1hTokens: u.CacheWrite1hTokens,
		})
	case ProtocolResponses:
		usage.UsageSemantic = dto.BillingUsageSemanticOpenAI
		usage.BillingUsage = dto.NewOpenAIResponsesBillingUsage(usage)
	default:
		usage.UsageSemantic = dto.BillingUsageSemanticOpenAI
		usage.BillingUsage = dto.NewOpenAIChatBillingUsage(usage)
	}
	return usage
}

func finalizeResponseUsage(usage any, state *responseTransformState) any {
	parsed, _ := usage.(*dto.Usage)
	if state != nil && !state.sawStandardUsage && state.fallbackUsage != nil {
		parsed = state.fallbackUsage.toUsage(state.protocol)
	}
	if parsed == nil {
		parsed = &dto.Usage{}
	}
	if parsed.PromptTokensDetails.CachedTokens == 0 && parsed.InputTokensDetails != nil {
		parsed.PromptTokensDetails.CachedTokens = parsed.InputTokensDetails.CachedTokens
	}
	if parsed.InputTokens == 0 {
		parsed.InputTokens = parsed.PromptTokens + parsed.PromptTokensDetails.CachedTokens +
			parsed.PromptTokensDetails.CacheCreationTokensTotal()
	}
	if parsed.OutputTokens == 0 {
		parsed.OutputTokens = parsed.CompletionTokens
	}
	if parsed.TotalTokens == 0 {
		parsed.TotalTokens = parsed.PromptTokens + parsed.CompletionTokens
	}
	if parsed.PromptCacheHitTokens == 0 {
		parsed.PromptCacheHitTokens = parsed.PromptTokensDetails.CachedTokens
	}
	if parsed.BillingUsage == nil && state != nil {
		switch state.protocol {
		case ProtocolMessages:
			parsed.BillingUsage = dto.NewClaudeMessagesBillingUsage(&dto.ClaudeUsage{
				InputTokens:              parsed.PromptTokens,
				CacheReadInputTokens:     parsed.PromptTokensDetails.CachedTokens,
				CacheCreationInputTokens: parsed.PromptTokensDetails.CacheCreationTokensTotal(),
				OutputTokens:             parsed.CompletionTokens,
			})
		case ProtocolResponses:
			parsed.BillingUsage = dto.NewOpenAIResponsesBillingUsage(parsed)
		default:
			parsed.BillingUsage = dto.NewOpenAIChatBillingUsage(parsed)
		}
	}
	return parsed
}
