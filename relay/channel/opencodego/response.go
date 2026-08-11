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
	model                     string
	protocol                  Protocol
	namespaceTools            map[string]openCodeGoNamespaceTool
	estimatedInputTokens      int
	sawStandardUsage          bool
	sawPositiveStandardUsage  bool
	sawStandardInputField     bool
	sawFallbackInputField     bool
	emittedSyntheticChatUsage bool
	standardUsageCategories   responseUsageCategoryPresence
	fallbackUsage             *normalizedCostUsage
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
		capturedFallback := s.captureFallbackUsage(data)
		if stream && s.protocol == ProtocolChat && !s.sawStandardUsage && !s.emittedSyntheticChatUsage &&
			capturedFallback && s.sawFallbackInputField {
			s.emittedSyntheticChatUsage = true
			return s.standardChatUsageFrame()
		}
		if stream {
			return nil
		}
		data = sanitizeNonstreamCostExtension(data)
		if data == nil {
			return nil
		}
	}
	data = stripPrivateCostFields(data)
	if hasStandardUsage(data) {
		s.sawStandardUsage = true
		if hasPositiveStandardUsage(data) {
			s.sawPositiveStandardUsage = true
		}
		s.capturePositiveStandardUsageCategories(data)
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
	if strings.EqualFold(gjson.GetBytes(data, "type").String(), "ping") &&
		gjson.GetBytes(data, "cost").Exists() {
		return true
	}

	// OpenCode's oa-compatible endpoint appends a bare Chat trailer after the
	// provider usage chunk: {"choices":[],"cost":"..."}.
	choices := gjson.GetBytes(data, "choices")
	return gjson.GetBytes(data, "cost").Exists() &&
		choices.IsArray() && len(choices.Array()) == 0 &&
		!hasStandardUsage(data) &&
		!gjson.GetBytes(data, "id").Exists() &&
		!gjson.GetBytes(data, "object").Exists() &&
		!gjson.GetBytes(data, "model").Exists()
}

func hasStandardUsage(data []byte) bool {
	paths := []string{"usage", "message.usage", "response.usage"}
	for _, path := range paths {
		if usage := gjson.GetBytes(data, path); usage.Exists() && usage.IsObject() {
			return true
		}
	}
	return false
}

func hasPositiveStandardUsage(data []byte) bool {
	usagePaths := []string{"usage", "message.usage", "response.usage"}
	tokenPaths := []string{
		"prompt_tokens", "input_tokens", "completion_tokens", "output_tokens", "cached_tokens", "prompt_cache_hit_tokens",
		"cache_read_input_tokens", "cache_creation_input_tokens",
		"prompt_tokens_details.cached_tokens", "prompt_tokens_details.cached_creation_tokens", "prompt_tokens_details.cache_creation_input_tokens", "prompt_tokens_details.cache_write_tokens",
		"input_tokens_details.cached_tokens", "input_tokens_details.cached_creation_tokens", "input_tokens_details.cache_creation_input_tokens", "input_tokens_details.cache_write_tokens",
		"completion_tokens_details.reasoning_tokens", "output_tokens_details.reasoning_tokens",
		"prompt_tokens_details.text_tokens", "prompt_tokens_details.audio_tokens", "prompt_tokens_details.image_tokens",
		"input_tokens_details.text_tokens", "input_tokens_details.audio_tokens", "input_tokens_details.image_tokens",
		"completion_tokens_details.text_tokens", "completion_tokens_details.audio_tokens", "completion_tokens_details.image_tokens",
		"output_tokens_details.text_tokens", "output_tokens_details.audio_tokens", "output_tokens_details.image_tokens",
		"cache_creation.ephemeral_5m_input_tokens", "cache_creation.ephemeral_1h_input_tokens",
		"claude_cache_creation_5_m_tokens", "claude_cache_creation_1_h_tokens",
	}
	for _, usagePath := range usagePaths {
		for _, tokenPath := range tokenPaths {
			value := gjson.GetBytes(data, usagePath+"."+tokenPath)
			if value.Type == gjson.Number && value.Float() > 0 {
				return true
			}
		}
	}
	return false
}

func sanitizeNonstreamCostExtension(data []byte) []byte {
	hasPublicResponse := gjson.GetBytes(data, "choices").Exists() ||
		gjson.GetBytes(data, "content").Exists() ||
		gjson.GetBytes(data, "output").Exists() ||
		strings.EqualFold(gjson.GetBytes(data, "object").String(), "response")
	if !hasPublicResponse {
		return nil
	}
	return stripPrivateCostFields(data)
}

func stripPrivateCostFields(data []byte) []byte {
	for _, path := range []string{"cost", "normalizedUsage", "x-opencode-type"} {
		updated, err := sjson.DeleteBytes(data, path)
		if err == nil {
			data = updated
		}
	}
	return data
}

type responseUsageCategoryPresence struct {
	input        bool
	output       bool
	cacheRead    bool
	cacheWrite   bool
	cacheWrite5m bool
	cacheWrite1h bool
	reasoning    bool
	inputText    bool
	inputAudio   bool
	inputImage   bool
	outputText   bool
	outputAudio  bool
	outputImage  bool
}

func (s *responseTransformState) capturePositiveStandardUsageCategories(data []byte) {
	if s == nil {
		return
	}
	usagePaths := []string{"usage"}
	switch s.protocol {
	case ProtocolMessages:
		usagePaths = append(usagePaths, "message.usage")
	case ProtocolResponses:
		usagePaths = append(usagePaths, "response.usage")
	}
	for _, usagePath := range usagePaths {
		s.sawStandardInputField = s.sawStandardInputField || hasNonNegativeTokenField(data, usagePath,
			"prompt_tokens", "input_tokens")
		s.standardUsageCategories.input = s.standardUsageCategories.input || hasPositiveTokenField(data, usagePath,
			"prompt_tokens", "input_tokens")
		s.standardUsageCategories.output = s.standardUsageCategories.output || hasPositiveTokenField(data, usagePath,
			"completion_tokens", "output_tokens")
		s.standardUsageCategories.cacheRead = s.standardUsageCategories.cacheRead || hasPositiveTokenField(data, usagePath,
			"cached_tokens", "prompt_cache_hit_tokens", "cache_read_input_tokens", "prompt_tokens_details.cached_tokens", "input_tokens_details.cached_tokens")
		s.standardUsageCategories.cacheWrite = s.standardUsageCategories.cacheWrite || hasPositiveTokenField(data, usagePath,
			"cache_creation_input_tokens", "prompt_tokens_details.cached_creation_tokens", "prompt_tokens_details.cache_creation_input_tokens", "prompt_tokens_details.cache_write_tokens",
			"input_tokens_details.cached_creation_tokens", "input_tokens_details.cache_creation_input_tokens", "input_tokens_details.cache_write_tokens")
		s.standardUsageCategories.cacheWrite5m = s.standardUsageCategories.cacheWrite5m || hasPositiveTokenField(data, usagePath,
			"cache_creation.ephemeral_5m_input_tokens", "claude_cache_creation_5_m_tokens")
		s.standardUsageCategories.cacheWrite1h = s.standardUsageCategories.cacheWrite1h || hasPositiveTokenField(data, usagePath,
			"cache_creation.ephemeral_1h_input_tokens", "claude_cache_creation_1_h_tokens")
		s.standardUsageCategories.reasoning = s.standardUsageCategories.reasoning || hasPositiveTokenField(data, usagePath,
			"completion_tokens_details.reasoning_tokens", "output_tokens_details.reasoning_tokens")
		s.standardUsageCategories.inputText = s.standardUsageCategories.inputText || hasPositiveTokenField(data, usagePath,
			"prompt_tokens_details.text_tokens", "input_tokens_details.text_tokens")
		s.standardUsageCategories.inputAudio = s.standardUsageCategories.inputAudio || hasPositiveTokenField(data, usagePath,
			"prompt_tokens_details.audio_tokens", "input_tokens_details.audio_tokens")
		s.standardUsageCategories.inputImage = s.standardUsageCategories.inputImage || hasPositiveTokenField(data, usagePath,
			"prompt_tokens_details.image_tokens", "input_tokens_details.image_tokens")
		s.standardUsageCategories.outputText = s.standardUsageCategories.outputText || hasPositiveTokenField(data, usagePath,
			"completion_tokens_details.text_tokens", "output_tokens_details.text_tokens")
		s.standardUsageCategories.outputAudio = s.standardUsageCategories.outputAudio || hasPositiveTokenField(data, usagePath,
			"completion_tokens_details.audio_tokens", "output_tokens_details.audio_tokens")
		s.standardUsageCategories.outputImage = s.standardUsageCategories.outputImage || hasPositiveTokenField(data, usagePath,
			"completion_tokens_details.image_tokens", "output_tokens_details.image_tokens")
	}
}

func hasNonNegativeTokenField(data []byte, usagePath string, fields ...string) bool {
	for _, field := range fields {
		value := gjson.GetBytes(data, usagePath+"."+field)
		if value.Type == gjson.Number && value.Float() >= 0 {
			return true
		}
	}
	return false
}

func hasPositiveTokenField(data []byte, usagePath string, fields ...string) bool {
	for _, field := range fields {
		value := gjson.GetBytes(data, usagePath+"."+field)
		if value.Type == gjson.Number && value.Int() > 0 {
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

func (s *responseTransformState) captureFallbackUsage(data []byte) bool {
	raw := gjson.GetBytes(data, "normalizedUsage")
	if !raw.Exists() || !raw.IsObject() {
		return false
	}
	inputField := raw.Get("inputTokens")
	hasInputField := inputField.Type == gjson.Number && inputField.Float() >= 0
	var usage normalizedCostUsage
	if err := common.Unmarshal([]byte(raw.Raw), &usage); err != nil {
		return false
	}
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.ReasoningTokens < 0 || usage.CacheReadTokens < 0 ||
		usage.CacheWrite5mTokens < 0 || usage.CacheWrite1hTokens < 0 {
		return false
	}
	// An explicit zero input is authoritative and must not be replaced by a
	// request-side estimate, even when every normalized category is zero.
	if !hasInputField && usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.ReasoningTokens == 0 && usage.CacheReadTokens == 0 &&
		usage.CacheWrite5mTokens == 0 && usage.CacheWrite1hTokens == 0 {
		return false
	}
	s.fallbackUsage = &usage
	s.sawFallbackInputField = hasInputField
	return true
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
	return finalizeUsageSnapshot(protocol, responseUsageSnapshotFromCost(protocol, u), nil)
}

func finalizeResponseUsage(usage any, state *responseTransformState) any {
	parsed, _ := usage.(*dto.Usage)
	protocol := ProtocolChat
	if state != nil {
		protocol = state.protocol
	}

	var fallback *responseUsageSnapshot
	if state != nil && state.fallbackUsage != nil {
		candidate := responseUsageSnapshotFromCost(protocol, state.fallbackUsage)
		if !state.sawFallbackInputField {
			// For Chat and Responses, the cost snapshot normally adds cache
			// categories to inputTokens. Without that field the sum is only a
			// partial input and must not suppress a request-side estimate.
			candidate.input = 0
		}
		fallback = &candidate
	}
	var parsedSnapshot *responseUsageSnapshot
	if parsed != nil {
		allowOuterInput := state == nil || state.standardUsageCategories.input
		parsedSnapshot = responseUsageSnapshotFromStandard(protocol, parsed, allowOuterInput)
	}
	var standard *responseUsageSnapshot
	if parsedSnapshot != nil && (state == nil || state.sawStandardUsage) {
		candidate := *parsedSnapshot
		if state != nil {
			retainPositiveStandardUsageCategories(&candidate, state.standardUsageCategories)
		}
		standard = &candidate
	}

	reconciled := mergeResponseUsageSnapshots(standard, fallback)
	estimated := state == nil && parsed != nil && parsed.BillingUsage != nil && parsed.BillingUsage.Estimated
	localFallback := localResponseUsageFallback(protocol, state, parsedSnapshot, reconciled)
	if localFallback != nil && responseUsageSnapshotNeedsLocalFallback(reconciled, *localFallback) {
		fillResponseUsageDetailGaps(&reconciled, localFallback, true)
		estimated = true
	}
	result := finalizeUsageSnapshot(protocol, reconciled, parsed)
	if result.BillingUsage != nil {
		if state != nil && !state.sawStandardInputField && !state.sawFallbackInputField {
			estimated = true
		}
		result.BillingUsage.Estimated = estimated
	}
	return result
}

func localResponseUsageFallback(
	protocol Protocol,
	state *responseTransformState,
	parsed *responseUsageSnapshot,
	provider responseUsageSnapshot,
) *responseUsageSnapshot {
	if parsed == nil && state == nil {
		return nil
	}
	local := responseUsageSnapshot{}
	if parsed != nil {
		local = *parsed
	}
	if state == nil {
		return &local
	}
	if state.sawStandardInputField || state.sawFallbackInputField {
		local.input = 0
		return &local
	}
	estimatedInput := nonNegativeTokens(state.estimatedInputTokens)
	if estimatedInput > 0 || local.input == 0 {
		local.input = estimatedInput
	}
	if protocol == ProtocolMessages {
		// Request-side estimates represent the full prompt. Messages billing
		// stores uncached input separately, so remove provider-reported cache
		// categories before using the estimate as an input fallback.
		local.input = nonNegativeTokens(local.input - provider.cacheRead - provider.cacheWrite)
	}
	return &local
}

// responseUsageSnapshotNeedsLocalFallback distinguishes handler-generated token
// estimates from provider usage. Scalar cache/reasoning fields own their detail
// aliases, so only independent modality details participate in this check.
func responseUsageSnapshotNeedsLocalFallback(target responseUsageSnapshot, local responseUsageSnapshot) bool {
	return target.input == 0 && local.input > 0 ||
		target.output == 0 && local.output > 0 ||
		target.cacheRead == 0 && local.cacheRead > 0 ||
		target.cacheWrite == 0 && local.cacheWrite > 0 ||
		target.cacheWrite5m == 0 && local.cacheWrite5m > 0 ||
		target.cacheWrite1h == 0 && local.cacheWrite1h > 0 ||
		target.reasoning == 0 && local.reasoning > 0 ||
		target.inputDetails.TextTokens == 0 && local.inputDetails.TextTokens > 0 ||
		target.inputDetails.AudioTokens == 0 && local.inputDetails.AudioTokens > 0 ||
		target.inputDetails.ImageTokens == 0 && local.inputDetails.ImageTokens > 0 ||
		target.outputDetails.TextTokens == 0 && local.outputDetails.TextTokens > 0 ||
		target.outputDetails.AudioTokens == 0 && local.outputDetails.AudioTokens > 0 ||
		target.outputDetails.ImageTokens == 0 && local.outputDetails.ImageTokens > 0
}

func retainPositiveStandardUsageCategories(snapshot *responseUsageSnapshot, presence responseUsageCategoryPresence) {
	if snapshot == nil {
		return
	}
	if !presence.input {
		snapshot.input = 0
	}
	if !presence.output {
		snapshot.output = 0
	}
	if !presence.cacheRead {
		snapshot.cacheRead = 0
		snapshot.inputDetails.CachedTokens = 0
	}
	if !presence.cacheWrite {
		snapshot.cacheWrite = 0
		snapshot.inputDetails.CachedCreationTokens = 0
		snapshot.inputDetails.CacheWriteTokens = 0
	}
	if !presence.cacheWrite5m {
		snapshot.cacheWrite5m = 0
	}
	if !presence.cacheWrite1h {
		snapshot.cacheWrite1h = 0
	}
	if !presence.reasoning {
		snapshot.reasoning = 0
		snapshot.outputDetails.ReasoningTokens = 0
	}
	if !presence.inputText {
		snapshot.inputDetails.TextTokens = 0
	}
	if !presence.inputAudio {
		snapshot.inputDetails.AudioTokens = 0
	}
	if !presence.inputImage {
		snapshot.inputDetails.ImageTokens = 0
	}
	if !presence.outputText {
		snapshot.outputDetails.TextTokens = 0
	}
	if !presence.outputAudio {
		snapshot.outputDetails.AudioTokens = 0
	}
	if !presence.outputImage {
		snapshot.outputDetails.ImageTokens = 0
	}
}

type responseUsageSnapshot struct {
	input         int
	output        int
	cacheRead     int
	cacheWrite    int
	cacheWrite5m  int
	cacheWrite1h  int
	reasoning     int
	inputDetails  dto.InputTokenDetails
	outputDetails dto.OutputTokenDetails
	usageSource   string
}

func responseUsageSnapshotFromCost(protocol Protocol, usage *normalizedCostUsage) responseUsageSnapshot {
	if usage == nil {
		return responseUsageSnapshot{}
	}
	cacheWrite5m := nonNegativeTokens(usage.CacheWrite5mTokens)
	cacheWrite1h := nonNegativeTokens(usage.CacheWrite1hTokens)
	cacheWrite := cacheWrite5m + cacheWrite1h
	input := nonNegativeTokens(usage.InputTokens)
	if protocol != ProtocolMessages {
		input += nonNegativeTokens(usage.CacheReadTokens) + cacheWrite
	}
	return responseUsageSnapshot{
		input:        input,
		output:       nonNegativeTokens(usage.OutputTokens),
		cacheRead:    nonNegativeTokens(usage.CacheReadTokens),
		cacheWrite:   cacheWrite,
		cacheWrite5m: cacheWrite5m,
		cacheWrite1h: cacheWrite1h,
		reasoning:    nonNegativeTokens(usage.ReasoningTokens),
		usageSource:  "opencode_go_inference_cost",
	}
}

func responseUsageSnapshotFromStandard(protocol Protocol, parsed *dto.Usage, allowOuterInput bool) *responseUsageSnapshot {
	if parsed == nil {
		return nil
	}
	if snapshot := responseUsageSnapshotFromNativeBilling(protocol, parsed); snapshot != nil {
		outer := responseUsageSnapshotFromOpenAIUsage(protocol, parsed)
		if allowOuterInput && snapshot.input == 0 && protocol == ProtocolMessages &&
			strings.EqualFold(parsed.UsageSemantic, dto.BillingUsageSemanticOpenAI) {
			cacheRead := maxTokens(snapshot.cacheRead, outer.cacheRead)
			cacheWrite := maxTokens(snapshot.cacheWrite, outer.cacheWrite)
			outer.input = nonNegativeTokens(outer.input - cacheRead - cacheWrite)
		}
		fillResponseUsageDetailGaps(snapshot, &outer, allowOuterInput)
		return snapshot
	}
	snapshot := responseUsageSnapshotFromOpenAIUsage(protocol, parsed)
	return &snapshot
}

func responseUsageSnapshotFromNativeBilling(protocol Protocol, parsed *dto.Usage) *responseUsageSnapshot {
	billing := parsed.BillingUsage
	if billing == nil {
		return nil
	}
	switch protocol {
	case ProtocolMessages:
		if billing.Source != dto.BillingUsageSourceClaudeMessages || billing.ClaudeUsage == nil {
			return nil
		}
		usage := billing.ClaudeUsage
		cacheWrite5m := nonNegativeTokens(usage.ClaudeCacheCreation5mTokens)
		cacheWrite1h := nonNegativeTokens(usage.ClaudeCacheCreation1hTokens)
		if usage.CacheCreation != nil {
			if value := nonNegativeTokens(usage.CacheCreation.Ephemeral5mInputTokens); value > 0 {
				cacheWrite5m = value
			}
			if value := nonNegativeTokens(usage.CacheCreation.Ephemeral1hInputTokens); value > 0 {
				cacheWrite1h = value
			}
		}
		cacheWrite := maxTokens(nonNegativeTokens(usage.CacheCreationInputTokens), cacheWrite5m+cacheWrite1h)
		return &responseUsageSnapshot{
			input:        nonNegativeTokens(usage.InputTokens),
			output:       nonNegativeTokens(usage.OutputTokens),
			cacheRead:    nonNegativeTokens(usage.CacheReadInputTokens),
			cacheWrite:   cacheWrite,
			cacheWrite5m: cacheWrite5m,
			cacheWrite1h: cacheWrite1h,
			usageSource:  parsed.UsageSource,
		}
	case ProtocolResponses:
		if billing.Source != dto.BillingUsageSourceOAIResponses || billing.OpenAIUsage == nil {
			return nil
		}
		snapshot := responseUsageSnapshotFromOpenAIUsage(protocol, billing.OpenAIUsage)
		return &snapshot
	default:
		if billing.Source != dto.BillingUsageSourceOAIChat || billing.OpenAIUsage == nil {
			return nil
		}
		snapshot := responseUsageSnapshotFromOpenAIUsage(protocol, billing.OpenAIUsage)
		return &snapshot
	}
}

func responseUsageSnapshotFromOpenAIUsage(protocol Protocol, usage *dto.Usage) responseUsageSnapshot {
	if usage == nil {
		return responseUsageSnapshot{}
	}
	inputDetails := canonicalInputTokenDetails(protocol, usage)
	outputDetails := usage.CompletionTokenDetails
	input := usage.PromptTokens
	if protocol == ProtocolResponses {
		input = usage.InputTokens
		if input == 0 {
			input = usage.PromptTokens
		}
	} else if input == 0 {
		input = usage.InputTokens
	}
	output := usage.CompletionTokens
	if protocol == ProtocolResponses {
		output = usage.OutputTokens
		if output == 0 {
			output = usage.CompletionTokens
		}
	} else if output == 0 {
		output = usage.OutputTokens
	}
	cacheWrite5m := nonNegativeTokens(usage.ClaudeCacheCreation5mTokens)
	cacheWrite1h := nonNegativeTokens(usage.ClaudeCacheCreation1hTokens)
	cacheWrite := maxTokens(inputDetails.CacheCreationTokensTotal(), cacheWrite5m+cacheWrite1h)
	return responseUsageSnapshot{
		input:         nonNegativeTokens(input),
		output:        nonNegativeTokens(output),
		cacheRead:     nonNegativeTokens(inputDetails.CachedTokens),
		cacheWrite:    cacheWrite,
		cacheWrite5m:  cacheWrite5m,
		cacheWrite1h:  cacheWrite1h,
		reasoning:     nonNegativeTokens(outputDetails.ReasoningTokens),
		inputDetails:  normalizeInputTokenDetails(inputDetails),
		outputDetails: normalizeOutputTokenDetails(outputDetails),
		usageSource:   usage.UsageSource,
	}
}

func canonicalInputTokenDetails(protocol Protocol, usage *dto.Usage) dto.InputTokenDetails {
	details := normalizeInputTokenDetails(usage.PromptTokensDetails)
	if details.CachedTokens == 0 {
		details.CachedTokens = nonNegativeTokens(usage.CachedTokens)
	}
	if details.CachedTokens == 0 {
		details.CachedTokens = nonNegativeTokens(usage.PromptCacheHitTokens)
	}
	if usage.InputTokensDetails == nil {
		return details
	}
	inputDetails := normalizeInputTokenDetails(*usage.InputTokensDetails)
	if protocol == ProtocolResponses {
		fillInputTokenDetailGaps(&inputDetails, details)
		return inputDetails
	}
	fillInputTokenDetailGaps(&details, inputDetails)
	return details
}

func fillResponseUsageDetailGaps(target *responseUsageSnapshot, fallback *responseUsageSnapshot, allowInput bool) {
	if target == nil || fallback == nil {
		return
	}
	if allowInput && target.input == 0 {
		target.input = fallback.input
	}
	if target.output == 0 {
		target.output = fallback.output
	}
	if target.cacheRead == 0 {
		target.cacheRead = fallback.cacheRead
	}
	if target.cacheWrite == 0 {
		target.cacheWrite = fallback.cacheWrite
	}
	if target.cacheWrite5m == 0 {
		target.cacheWrite5m = fallback.cacheWrite5m
	}
	if target.cacheWrite1h == 0 {
		target.cacheWrite1h = fallback.cacheWrite1h
	}
	fillInputTokenDetailGaps(&target.inputDetails, fallback.inputDetails)
	fillOutputTokenDetailGaps(&target.outputDetails, fallback.outputDetails)
	if target.reasoning == 0 {
		target.reasoning = fallback.reasoning
	}
	if target.usageSource == "" {
		target.usageSource = fallback.usageSource
	}
}

func fillInputTokenDetailGaps(target *dto.InputTokenDetails, fallback dto.InputTokenDetails) {
	if target.CachedTokens == 0 {
		target.CachedTokens = fallback.CachedTokens
	}
	if target.CachedCreationTokens == 0 {
		target.CachedCreationTokens = fallback.CachedCreationTokens
	}
	if target.CacheWriteTokens == 0 {
		target.CacheWriteTokens = fallback.CacheWriteTokens
	}
	if target.TextTokens == 0 {
		target.TextTokens = fallback.TextTokens
	}
	if target.AudioTokens == 0 {
		target.AudioTokens = fallback.AudioTokens
	}
	if target.ImageTokens == 0 {
		target.ImageTokens = fallback.ImageTokens
	}
}

func fillOutputTokenDetailGaps(target *dto.OutputTokenDetails, fallback dto.OutputTokenDetails) {
	if target.ReasoningTokens == 0 {
		target.ReasoningTokens = fallback.ReasoningTokens
	}
	if target.TextTokens == 0 {
		target.TextTokens = fallback.TextTokens
	}
	if target.AudioTokens == 0 {
		target.AudioTokens = fallback.AudioTokens
	}
	if target.ImageTokens == 0 {
		target.ImageTokens = fallback.ImageTokens
	}
}

func mergeResponseUsageSnapshots(standard *responseUsageSnapshot, fallback *responseUsageSnapshot) responseUsageSnapshot {
	if standard == nil {
		if fallback == nil {
			return responseUsageSnapshot{}
		}
		return *fallback
	}
	merged := *standard
	if fallback == nil {
		return merged
	}
	if merged.input == 0 {
		merged.input = fallback.input
	}
	if merged.output == 0 {
		merged.output = fallback.output
	}
	if merged.cacheRead == 0 {
		merged.cacheRead = fallback.cacheRead
	}
	if merged.cacheWrite == 0 {
		merged.cacheWrite = fallback.cacheWrite
	}
	if merged.cacheWrite5m == 0 {
		merged.cacheWrite5m = fallback.cacheWrite5m
	}
	if merged.cacheWrite1h == 0 {
		merged.cacheWrite1h = fallback.cacheWrite1h
	}
	if merged.reasoning == 0 {
		merged.reasoning = fallback.reasoning
	}
	if merged.usageSource == "" {
		merged.usageSource = fallback.usageSource
	}
	fillInputTokenDetailGaps(&merged.inputDetails, fallback.inputDetails)
	fillOutputTokenDetailGaps(&merged.outputDetails, fallback.outputDetails)
	return merged
}

func finalizeUsageSnapshot(protocol Protocol, snapshot responseUsageSnapshot, base *dto.Usage) *dto.Usage {
	usage := &dto.Usage{}
	if base != nil {
		*usage = *base
	}
	usage.BillingUsage = nil
	snapshot.input = nonNegativeTokens(snapshot.input)
	snapshot.output = nonNegativeTokens(snapshot.output)
	snapshot.cacheRead = nonNegativeTokens(snapshot.cacheRead)
	snapshot.cacheWrite5m = nonNegativeTokens(snapshot.cacheWrite5m)
	snapshot.cacheWrite1h = nonNegativeTokens(snapshot.cacheWrite1h)
	snapshot.cacheWrite = maxTokens(nonNegativeTokens(snapshot.cacheWrite), snapshot.cacheWrite5m+snapshot.cacheWrite1h)
	snapshot.reasoning = nonNegativeTokens(snapshot.reasoning)
	if snapshot.reasoning > snapshot.output {
		snapshot.reasoning = snapshot.output
	}

	usage.PromptTokens = snapshot.input
	usage.CompletionTokens = snapshot.output
	usage.TotalTokens = snapshot.input + snapshot.output
	usage.InputTokens = snapshot.input
	usage.OutputTokens = snapshot.output
	usage.PromptCacheHitTokens = snapshot.cacheRead
	usage.UsageSource = snapshot.usageSource
	usage.PromptTokensDetails = normalizeInputTokenDetails(snapshot.inputDetails)
	usage.PromptTokensDetails.CachedTokens = snapshot.cacheRead
	usage.PromptTokensDetails.CachedCreationTokens = snapshot.cacheWrite
	usage.PromptTokensDetails.CacheWriteTokens = snapshot.cacheWrite
	usage.CompletionTokenDetails = normalizeOutputTokenDetails(snapshot.outputDetails)
	usage.CompletionTokenDetails.ReasoningTokens = snapshot.reasoning
	usage.ClaudeCacheCreation5mTokens = snapshot.cacheWrite5m
	usage.ClaudeCacheCreation1hTokens = snapshot.cacheWrite1h
	usage.InputTokensDetails = nil

	switch protocol {
	case ProtocolMessages:
		usage.InputTokens = snapshot.input + snapshot.cacheRead + snapshot.cacheWrite
		usage.UsageSemantic = dto.BillingUsageSemanticAnthropic
		claudeUsage := &dto.ClaudeUsage{
			InputTokens:                 snapshot.input,
			CacheReadInputTokens:        snapshot.cacheRead,
			CacheCreationInputTokens:    snapshot.cacheWrite,
			OutputTokens:                snapshot.output,
			ClaudeCacheCreation5mTokens: snapshot.cacheWrite5m,
			ClaudeCacheCreation1hTokens: snapshot.cacheWrite1h,
		}
		if snapshot.cacheWrite5m > 0 || snapshot.cacheWrite1h > 0 {
			claudeUsage.CacheCreation = &dto.ClaudeCacheCreationUsage{
				Ephemeral5mInputTokens: snapshot.cacheWrite5m,
				Ephemeral1hInputTokens: snapshot.cacheWrite1h,
			}
		}
		usage.BillingUsage = dto.NewClaudeMessagesBillingUsage(claudeUsage)
	case ProtocolResponses:
		usage.UsageSemantic = dto.BillingUsageSemanticOpenAI
		inputDetails := usage.PromptTokensDetails
		usage.InputTokensDetails = &inputDetails
		usage.BillingUsage = dto.NewOpenAIResponsesBillingUsage(usage)
	default:
		usage.UsageSemantic = dto.BillingUsageSemanticOpenAI
		usage.BillingUsage = dto.NewOpenAIChatBillingUsage(usage)
	}
	return usage
}

func normalizeInputTokenDetails(details dto.InputTokenDetails) dto.InputTokenDetails {
	details.CachedTokens = nonNegativeTokens(details.CachedTokens)
	details.CachedCreationTokens = nonNegativeTokens(details.CachedCreationTokens)
	details.CacheWriteTokens = nonNegativeTokens(details.CacheWriteTokens)
	details.TextTokens = nonNegativeTokens(details.TextTokens)
	details.AudioTokens = nonNegativeTokens(details.AudioTokens)
	details.ImageTokens = nonNegativeTokens(details.ImageTokens)
	return details
}

func normalizeOutputTokenDetails(details dto.OutputTokenDetails) dto.OutputTokenDetails {
	details.TextTokens = nonNegativeTokens(details.TextTokens)
	details.AudioTokens = nonNegativeTokens(details.AudioTokens)
	details.ImageTokens = nonNegativeTokens(details.ImageTokens)
	details.ReasoningTokens = nonNegativeTokens(details.ReasoningTokens)
	return details
}

func nonNegativeTokens(tokens int) int {
	if tokens < 0 {
		return 0
	}
	return tokens
}

func maxTokens(first int, second int) int {
	if second > first {
		return second
	}
	return first
}
