package types

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewOpenAIErrorPreservesWrappedCause(t *testing.T) {
	sentinel := errors.New("sentinel")
	wrapped := fmt.Errorf("request failed: %w", sentinel)

	apiErr := NewOpenAIError(wrapped, ErrorCodeDoRequestFailed, http.StatusInternalServerError)

	assert.ErrorIs(t, apiErr, sentinel)
	assert.Equal(t, wrapped.Error(), apiErr.Error())
	assert.Equal(t, OpenAIError{
		Message: wrapped.Error(),
		Type:    string(ErrorCodeDoRequestFailed),
		Code:    ErrorCodeDoRequestFailed,
	}, apiErr.ToOpenAIError())
}

func TestOpenAIErrorHasDetailsSupportsCodeOnlyResponses(t *testing.T) {
	assert.True(t, (&OpenAIError{Code: "invalid_prompt"}).HasDetails())
	assert.True(t, (&OpenAIError{Message: "provider failed"}).HasDetails())
	assert.True(t, (&OpenAIError{Param: "input"}).HasDetails())
	assert.False(t, (&OpenAIError{}).HasDetails())
	var nilError *OpenAIError
	assert.False(t, nilError.HasDetails())
}
