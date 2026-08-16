package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFinalizedCandidateCompletionReservationCoversKnownAmplifiers(t *testing.T) {
	estimate, err := finalizedCandidateCompletionReservation([]byte(`{
		"max_tokens":100,
		"max_new_tokens":200,
		"n":2,
		"best_of":3,
		"num_return_sequences":4,
		"thinking":{"type":"enabled","budget_tokens":50}
	}`))

	require.NoError(t, err)
	assert.Equal(t, (200+50)*2*3*4, estimate)
}

func TestFinalizedCandidateCompletionReservationUsesConservativeFallback(t *testing.T) {
	estimate, err := finalizedCandidateCompletionReservation([]byte(`{"model":"test"}`))
	require.NoError(t, err)
	assert.Equal(t, defaultOpenCodeCompletionReservation, estimate)
}

func TestFinalizedCandidateCompletionReservationRejectsUnboundedNumericShape(t *testing.T) {
	_, err := finalizedCandidateCompletionReservation([]byte(`{"max_new_tokens":1e9}`))
	require.Error(t, err)
}

func TestFinalizedCandidatePromptReservationIncludesFinalizerAddedStrings(t *testing.T) {
	short, err := finalizedCandidatePromptReservation(
		context.Background(),
		[]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}`),
		"glm-5.2",
	)
	require.NoError(t, err)

	long, err := finalizedCandidatePromptReservation(
		context.Background(),
		[]byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"`+
			strings.Repeat("candidate system prompt ", 200)+
			`"},{"role":"user","content":"hello"}],"operator_extension":"`+
			strings.Repeat("override text ", 200)+`"}`),
		"glm-5.2",
	)
	require.NoError(t, err)
	assert.Greater(t, long, short)
}

func TestOpenCodeFinalizedCandidateBillingViewsApplyOriginalPromptFloor(t *testing.T) {
	plans := &openCodeFinalizedCandidatePlans{plans: []openCodeFinalizedCandidatePlan{
		{estimatedPromptTokens: 80, estimatedCompletionTokens: 10},
		{estimatedPromptTokens: 240, estimatedCompletionTokens: 20},
	}}

	views := plans.billingViews(100)
	require.Len(t, views, 2)
	assert.Equal(t, 100, views[0].EstimatedPromptTokens)
	assert.Equal(t, 240, views[1].EstimatedPromptTokens)
	assert.Equal(t, 100, plans.basePromptTokens)
}
