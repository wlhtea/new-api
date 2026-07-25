package dto

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskErrorRetryableIsInternalOnly(t *testing.T) {
	retryable := false
	body, err := common.Marshal(TaskError{
		Code:       "seedance_submit_outcome_unknown",
		Message:    "submit result is unknown",
		StatusCode: 502,
		Retryable:  &retryable,
	})
	require.NoError(t, err)
	assert.JSONEq(t,
		`{"code":"seedance_submit_outcome_unknown","message":"submit result is unknown","data":null}`,
		string(body),
	)
}
