package service

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
)

const (
	OpenCodeGoPublicOverloadMessage    = constant.OpenCodeGoPublicOverloadMessage
	OpenCodeGoPublicRateLimitErrorCode = constant.OpenCodeGoPublicRateLimitErrorCode
)

var errOpenCodeGoUpstreamOrigin = errors.New("opencode go upstream-origin error")

type openCodeGoUpstreamOriginError struct {
	cause              error
	upstreamStatusCode int
}

func (e *openCodeGoUpstreamOriginError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *openCodeGoUpstreamOriginError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *openCodeGoUpstreamOriginError) Is(target error) bool {
	return target == errOpenCodeGoUpstreamOrigin
}

// MarkOpenCodeGoUpstreamRelayError records that a relay error came from the
// type-62 upstream. The wrapper is not serialized and preserves Error() so
// internal logs keep the original diagnostics.
func MarkOpenCodeGoUpstreamRelayError(relayErr *types.NewAPIError) *types.NewAPIError {
	if relayErr == nil || errors.Is(relayErr, errOpenCodeGoUpstreamOrigin) {
		return relayErr
	}
	return markOpenCodeGoUpstreamRelayErrorWithStatus(relayErr, relayErr.StatusCode)
}

func markOpenCodeGoUpstreamRelayErrorWithStatus(relayErr *types.NewAPIError, upstreamStatusCode int) *types.NewAPIError {
	if relayErr == nil || errors.Is(relayErr, errOpenCodeGoUpstreamOrigin) {
		return relayErr
	}
	cause := relayErr.Err
	if cause == nil {
		cause = errors.New(relayErr.Error())
	}
	relayErr.Err = &openCodeGoUpstreamOriginError{
		cause:              cause,
		upstreamStatusCode: upstreamStatusCode,
	}
	return relayErr
}

// IsOpenCodeGoUpstreamRelayError reports whether the error was explicitly
// marked at a type-62 upstream boundary.
func IsOpenCodeGoUpstreamRelayError(err error) bool {
	return errors.Is(err, errOpenCodeGoUpstreamOrigin)
}

func openCodeGoUpstreamRelayStatusCode(err error) (int, bool) {
	var originErr *openCodeGoUpstreamOriginError
	if !errors.As(err, &originErr) || originErr == nil || originErr.upstreamStatusCode <= 0 {
		return 0, false
	}
	return originErr.upstreamStatusCode, true
}

// PublicOpenCodeGoRelayError replaces type-62 errors that name internal
// infrastructure with a provider-neutral error. It returns a fresh error so
// private relay metadata and the original unwrap chain cannot be serialized.
func PublicOpenCodeGoRelayError(channelType int, relayErr *types.NewAPIError) *types.NewAPIError {
	if relayErr == nil {
		return nil
	}
	poolExhausted := errors.Is(relayErr, ErrOpenCodeGoNoEligibleWorkspace)
	upstreamOrigin := channelType == constant.ChannelTypeOpenCodeGo && IsOpenCodeGoUpstreamRelayError(relayErr)
	privateDetail := channelType == constant.ChannelTypeOpenCodeGo && openCodeGoRelayErrorContainsPrivateDetail(relayErr)
	if !poolExhausted && !upstreamOrigin && !privateDetail {
		return relayErr
	}
	return types.WithOpenAIError(types.OpenAIError{
		Message: OpenCodeGoPublicOverloadMessage,
		Type:    OpenCodeGoPublicRateLimitErrorCode,
		Code:    OpenCodeGoPublicRateLimitErrorCode,
	}, http.StatusTooManyRequests, types.ErrOptionWithSkipRetry())
}

func openCodeGoRelayErrorContainsPrivateDetail(relayErr *types.NewAPIError) bool {
	if relayErr == nil {
		return false
	}
	values := []string{
		relayErr.Error(),
		string(relayErr.GetErrorType()),
		string(relayErr.GetErrorCode()),
		string(relayErr.Metadata),
	}
	switch typed := relayErr.RelayError.(type) {
	case types.OpenAIError:
		values = append(values, typed.Message, typed.Type, typed.Param, fmt.Sprint(typed.Code), string(typed.Metadata))
	case *types.OpenAIError:
		if typed != nil {
			values = append(values, typed.Message, typed.Type, typed.Param, fmt.Sprint(typed.Code), string(typed.Metadata))
		}
	case types.ClaudeError:
		values = append(values, typed.Message, typed.Type)
	case *types.ClaudeError:
		if typed != nil {
			values = append(values, typed.Message, typed.Type)
		}
	case nil:
	default:
		values = append(values, fmt.Sprint(typed))
	}
	return OpenCodeGoErrorHasPrivateDetail(values...)
}

// OpenCodeGoErrorHasPrivateDetail reports whether an error representation
// contains provider, channel, or workspace details that must stay internal.
func OpenCodeGoErrorHasPrivateDetail(values ...string) bool {
	for _, value := range values {
		if openCodeGoStringHasPrivateErrorMarker(value) {
			return true
		}
		var decoded any
		if common.UnmarshalJsonStr(value, &decoded) == nil && openCodeGoJSONHasPrivateErrorMarker(decoded) {
			return true
		}
	}
	return false
}

func openCodeGoJSONHasPrivateErrorMarker(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if openCodeGoStringHasPrivateErrorMarker(key) || openCodeGoJSONHasPrivateErrorMarker(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if openCodeGoJSONHasPrivateErrorMarker(child) {
				return true
			}
		}
	case string:
		return openCodeGoStringHasPrivateErrorMarker(typed)
	}
	return false
}

func openCodeGoStringHasPrivateErrorMarker(value string) bool {
	return constant.OpenCodeGoStringHasPrivateErrorMarker(value)
}
