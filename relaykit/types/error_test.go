package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOpenAIErrorHasDetailsSupportsCodeOnlyResponses(t *testing.T) {
	assert.True(t, (&OpenAIError{Code: "invalid_prompt"}).HasDetails())
	assert.True(t, (&OpenAIError{Message: "provider failed"}).HasDetails())
	assert.True(t, (&OpenAIError{Param: "input"}).HasDetails())
	assert.False(t, (&OpenAIError{}).HasDetails())
	var nilError *OpenAIError
	assert.False(t, nilError.HasDetails())
}
