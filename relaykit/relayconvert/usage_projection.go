package relayconvert

import (
	"reflect"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// projectNativeUsageForTarget applies the opt-out usage policy to the public
// response value only. The canonical usage returned by a converter is never
// changed; callers continue to use that value for settlement and logging.
func projectNativeUsageForTarget(info convmeta.Meta, target types.RelayFormat, value any, canonical *dto.Usage) any {
	if info == nil || info.ConvOptions() == nil || info.ConvOptions().IsUsageConversionEnabled() {
		return value
	}

	if projected, ok := projectNativeUsageSlice(info, target, value, canonical); ok {
		return projected
	}
	native := nativeUsageFromCanonical(canonical)
	if native.empty() {
		native = nativeUsageFromValue(value)
	}
	if native.empty() {
		return value
	}

	switch target {
	case types.RelayFormatOpenAI:
		value = projectOpenAIValueUsage(value, native)
	case types.RelayFormatOpenAIResponses:
		value = projectResponsesValueUsage(value, native)
	case types.RelayFormatClaude:
		value = projectClaudeValueUsage(value, native)
	}
	return value
}

func projectNativeUsageValuesForTarget(info convmeta.Meta, target types.RelayFormat, values []any, canonical *dto.Usage) []any {
	for index, value := range values {
		values[index] = projectNativeUsageForTarget(info, target, value, canonical)
	}
	return values
}

func projectNativeUsageSlice(info convmeta.Meta, target types.RelayFormat, value any, canonical *dto.Usage) (any, bool) {
	if value == nil {
		return value, false
	}
	rv := reflect.ValueOf(value)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return value, false
	}
	if rv.Type().Elem().Kind() == reflect.Uint8 {
		return value, false
	}
	if rv.Kind() == reflect.Slice && rv.IsNil() {
		return value, true
	}
	copyValue := reflect.New(rv.Type()).Elem()
	if rv.Kind() == reflect.Slice {
		copyValue = reflect.MakeSlice(rv.Type(), rv.Len(), rv.Len())
	}
	for index := 0; index < rv.Len(); index++ {
		item := rv.Index(index)
		if item.Kind() == reflect.Pointer && item.IsNil() {
			copyValue.Index(index).Set(item)
			continue
		}
		projected := projectNativeUsageForTarget(info, target, item.Interface(), canonical)
		projectedValue := reflect.ValueOf(projected)
		if projectedValue.IsValid() && projectedValue.Type().AssignableTo(copyValue.Index(index).Type()) {
			copyValue.Index(index).Set(projectedValue)
		} else {
			copyValue.Index(index).Set(item)
		}
	}
	return copyValue.Interface(), true
}

type nativeUsageSnapshot struct {
	source string
	openAI *dto.Usage
	claude *dto.ClaudeUsage
}

func (u nativeUsageSnapshot) empty() bool {
	return u.openAI == nil && u.claude == nil
}

func nativeUsageFromCanonical(usage *dto.Usage) nativeUsageSnapshot {
	if usage == nil {
		return nativeUsageSnapshot{}
	}
	if billing := dto.CloneBillingUsage(usage.BillingUsage); billing != nil {
		snapshot := nativeUsageSnapshot{source: billing.Source}
		if billing.OpenAIUsage != nil {
			clone := *billing.OpenAIUsage
			clone.BillingUsage = billing
			snapshot.openAI = &clone
		}
		if billing.ClaudeUsage != nil {
			clone := *billing.ClaudeUsage
			clone.BillingUsage = billing
			snapshot.claude = &clone
		}
		if !snapshot.empty() {
			return snapshot
		}
	}
	return nativeUsageSnapshot{}
}

func nativeUsageFromValue(value any) nativeUsageSnapshot {
	switch typed := value.(type) {
	case *dto.OpenAITextResponse:
		if typed != nil {
			return nativeUsageFromCanonical(&typed.Usage)
		}
	case dto.OpenAITextResponse:
		return nativeUsageFromCanonical(&typed.Usage)
	case *dto.ChatCompletionsStreamResponse:
		if typed != nil && typed.Usage != nil {
			return nativeUsageFromCanonical(typed.Usage)
		}
	case dto.ChatCompletionsStreamResponse:
		if typed.Usage != nil {
			return nativeUsageFromCanonical(typed.Usage)
		}
	case *dto.OpenAIResponsesResponse:
		if typed != nil {
			return nativeUsageFromCanonical(typed.Usage)
		}
	case dto.OpenAIResponsesResponse:
		return nativeUsageFromCanonical(typed.Usage)
	case *dto.ResponsesStreamResponse:
		if typed != nil && typed.Response != nil {
			return nativeUsageFromCanonical(typed.Response.Usage)
		}
	case dto.ResponsesStreamResponse:
		if typed.Response != nil {
			return nativeUsageFromCanonical(typed.Response.Usage)
		}
	case *ChatToResponsesStreamEvent:
		if typed != nil && typed.Payload.Response != nil {
			return nativeUsageFromCanonical(typed.Payload.Response.Usage)
		}
	case ChatToResponsesStreamEvent:
		if typed.Payload.Response != nil {
			return nativeUsageFromCanonical(typed.Payload.Response.Usage)
		}
	case *dto.ClaudeResponse:
		if typed != nil {
			if native := nativeUsageFromClaudeResponse(typed); !native.empty() {
				return native
			}
		}
	case dto.ClaudeResponse:
		return nativeUsageFromClaudeResponse(&typed)
	}
	return nativeUsageSnapshot{}
}

func nativeUsageFromClaudeResponse(response *dto.ClaudeResponse) nativeUsageSnapshot {
	if response == nil {
		return nativeUsageSnapshot{}
	}
	if response.Usage != nil {
		if native := nativeUsageFromClaudeUsage(response.Usage); !native.empty() {
			return native
		}
	}
	if response.Message != nil && response.Message.Usage != nil {
		return nativeUsageFromClaudeUsage(response.Message.Usage)
	}
	return nativeUsageSnapshot{}
}

func nativeUsageFromClaudeUsage(usage *dto.ClaudeUsage) nativeUsageSnapshot {
	if usage == nil || usage.BillingUsage == nil {
		return nativeUsageSnapshot{}
	}
	billing := dto.CloneBillingUsage(usage.BillingUsage)
	if billing == nil {
		return nativeUsageSnapshot{}
	}
	if billing.ClaudeUsage != nil {
		clone := *billing.ClaudeUsage
		clone.BillingUsage = billing
		return nativeUsageSnapshot{source: billing.Source, claude: &clone}
	}
	if billing.OpenAIUsage != nil {
		clone := *billing.OpenAIUsage
		clone.BillingUsage = billing
		return nativeUsageSnapshot{source: billing.Source, openAI: &clone}
	}
	return nativeUsageSnapshot{}
}

func projectOpenAIValueUsage(value any, native nativeUsageSnapshot) any {
	usage := chatUsageFromNative(native)
	if usage == nil {
		return value
	}
	switch typed := value.(type) {
	case *dto.OpenAITextResponse:
		if typed != nil {
			projected := *typed
			projected.Usage = *usage
			return &projected
		}
	case dto.OpenAITextResponse:
		typed.Usage = *usage
		return typed
	case *dto.ChatCompletionsStreamResponse:
		if typed != nil && typed.Usage != nil {
			projected := *typed
			projected.Usage = usage
			return &projected
		}
	case dto.ChatCompletionsStreamResponse:
		if typed.Usage != nil {
			typed.Usage = usage
			return typed
		}
	}
	return value
}

func projectResponsesValueUsage(value any, native nativeUsageSnapshot) any {
	usage := responsesUsageFromNative(native)
	if usage == nil {
		return value
	}
	projectResponse := func(response *dto.OpenAIResponsesResponse) (*dto.OpenAIResponsesResponse, bool) {
		if response == nil || response.Usage == nil {
			return response, false
		}
		projected := *response
		projected.Usage = usage
		return &projected, true
	}
	switch typed := value.(type) {
	case *dto.OpenAIResponsesResponse:
		if projected, ok := projectResponse(typed); ok {
			return projected
		}
	case dto.OpenAIResponsesResponse:
		if projected, ok := projectResponse(&typed); ok {
			return *projected
		}
	case *dto.ResponsesStreamResponse:
		if typed != nil {
			if response, ok := projectResponse(typed.Response); ok {
				projected := *typed
				projected.Response = response
				return &projected
			}
		}
	case dto.ResponsesStreamResponse:
		if response, ok := projectResponse(typed.Response); ok {
			typed.Response = response
			return typed
		}
	case *ChatToResponsesStreamEvent:
		if typed != nil {
			if response, ok := projectResponse(typed.Payload.Response); ok {
				projected := *typed
				projected.Payload.Response = response
				return &projected
			}
		}
	case ChatToResponsesStreamEvent:
		if response, ok := projectResponse(typed.Payload.Response); ok {
			typed.Payload.Response = response
			return typed
		}
	}
	return value
}

func projectClaudeValueUsage(value any, native nativeUsageSnapshot) any {
	usage := claudeUsageFromNative(native)
	if usage == nil {
		return value
	}
	projectResponse := func(response *dto.ClaudeResponse) (*dto.ClaudeResponse, bool) {
		if response == nil {
			return response, false
		}
		projected := *response
		changed := false
		if response.Usage != nil {
			projected.Usage = usage
			changed = true
		}
		if response.Message != nil && response.Message.Usage != nil {
			message := *response.Message
			message.Usage = usage
			projected.Message = &message
			changed = true
		}
		return &projected, changed
	}
	switch typed := value.(type) {
	case *dto.ClaudeResponse:
		if projected, ok := projectResponse(typed); ok {
			return projected
		}
	case dto.ClaudeResponse:
		if projected, ok := projectResponse(&typed); ok {
			return *projected
		}
	}
	return value
}

func chatUsageFromNative(native nativeUsageSnapshot) *dto.Usage {
	usage := openAIUsageFromNative(native)
	if usage == nil {
		return nil
	}
	if native.openAI != nil && native.source == dto.BillingUsageSourceOAIResponses {
		usage = UsageFromResponsesUsage(usage)
	}
	usage.Cost = nil
	return usage
}

func responsesUsageFromNative(native nativeUsageSnapshot) *dto.Usage {
	usage := openAIUsageFromNative(native)
	if usage == nil {
		return nil
	}
	if native.openAI == nil || native.source == dto.BillingUsageSourceOAIChat {
		usage = UsageFromChatUsage(usage)
	}
	usage.Cost = nil
	return usage
}

func openAIUsageFromNative(native nativeUsageSnapshot) *dto.Usage {
	if native.openAI != nil {
		clone := *native.openAI
		clone.BillingUsage = dto.CloneBillingUsage(native.openAI.BillingUsage)
		if native.openAI.InputTokensDetails != nil {
			details := *native.openAI.InputTokensDetails
			clone.InputTokensDetails = &details
		}
		clone.Cost = nil
		return &clone
	}
	if native.claude == nil {
		return nil
	}
	claude := native.claude
	input := nonNegativeUsageTokens(claude.InputTokens)
	output := nonNegativeUsageTokens(claude.OutputTokens)
	cacheRead := nonNegativeUsageTokens(claude.CacheReadInputTokens)
	cacheWrite := nonNegativeUsageTokens(claude.CacheCreationInputTokens)
	cacheWrite5m := nonNegativeUsageTokens(claude.ClaudeCacheCreation5mTokens)
	cacheWrite1h := nonNegativeUsageTokens(claude.ClaudeCacheCreation1hTokens)
	if claude.CacheCreation != nil {
		cacheWrite5m = maxUsageTokens(cacheWrite5m, nonNegativeUsageTokens(claude.CacheCreation.Ephemeral5mInputTokens))
		cacheWrite1h = maxUsageTokens(cacheWrite1h, nonNegativeUsageTokens(claude.CacheCreation.Ephemeral1hInputTokens))
	}
	cacheWrite = maxUsageTokens(cacheWrite, cacheWrite5m+cacheWrite1h)
	usage := &dto.Usage{
		PromptTokens:         input,
		CompletionTokens:     output,
		TotalTokens:          input + output,
		InputTokens:          input,
		OutputTokens:         output,
		CachedTokens:         cacheRead,
		PromptCacheHitTokens: cacheRead,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens:         cacheRead,
			CachedCreationTokens: cacheWrite,
			CacheWriteTokens:     cacheWrite,
		},
		ClaudeCacheCreation5mTokens: cacheWrite5m,
		ClaudeCacheCreation1hTokens: cacheWrite1h,
	}
	if native.openAI == nil && native.claude != nil {
		usage.BillingUsage = dto.CloneBillingUsage(claude.BillingUsage)
	}
	return usage
}

func claudeUsageFromNative(native nativeUsageSnapshot) *dto.ClaudeUsage {
	if native.claude != nil {
		clone := *native.claude
		clone.BillingUsage = dto.CloneBillingUsage(native.claude.BillingUsage)
		return &clone
	}
	if native.openAI == nil {
		return nil
	}
	source := native.openAI
	input := source.InputTokens
	if input == 0 {
		input = source.PromptTokens
	}
	output := source.OutputTokens
	if output == 0 {
		output = source.CompletionTokens
	}
	details := source.InputTokensDetails
	if details == nil {
		detailsValue := source.PromptTokensDetails
		details = &detailsValue
	}
	cacheRead := nonNegativeUsageTokens(details.CachedTokens)
	cacheWrite := nonNegativeUsageTokens(details.CacheCreationTokensTotal())
	cacheWrite5m := nonNegativeUsageTokens(source.ClaudeCacheCreation5mTokens)
	cacheWrite1h := nonNegativeUsageTokens(source.ClaudeCacheCreation1hTokens)
	if cacheWrite5m+cacheWrite1h > cacheWrite {
		cacheWrite = cacheWrite5m + cacheWrite1h
	}
	uncachedInput := input - cacheRead - cacheWrite
	if uncachedInput < 0 {
		uncachedInput = 0
	}
	usage := &dto.ClaudeUsage{
		InputTokens:                 uncachedInput,
		CacheReadInputTokens:        cacheRead,
		CacheCreationInputTokens:    cacheWrite,
		OutputTokens:                nonNegativeUsageTokens(output),
		ClaudeCacheCreation5mTokens: cacheWrite5m,
		ClaudeCacheCreation1hTokens: cacheWrite1h,
	}
	if cacheWrite5m != 0 || cacheWrite1h != 0 {
		usage.CacheCreation = &dto.ClaudeCacheCreationUsage{
			Ephemeral5mInputTokens: cacheWrite5m,
			Ephemeral1hInputTokens: cacheWrite1h,
		}
	}
	usage.BillingUsage = dto.CloneBillingUsage(source.BillingUsage)
	return usage
}

func nonNegativeUsageTokens(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

func maxUsageTokens(first int, second int) int {
	if first > second {
		return first
	}
	return second
}
