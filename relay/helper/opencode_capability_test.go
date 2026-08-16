package helper

import (
	"context"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateOpenCodeModelCapabilityUsesExactModelProtocolPathAndValue(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		model      string
		protocol   string
		wantReject bool
	}{
		{
			name:       "exact model chat disabled",
			body:       `{"model":"glm-5.3","thinking":{"type":"disabled"}}`,
			model:      "glm-5.3",
			protocol:   "chat",
			wantReject: true,
		},
		{
			name:       "decoded value escape",
			body:       `{"model":"glm-5.3","thinking":{"type":"\u0064isabled"}}`,
			model:      "glm-5.3",
			protocol:   "chat",
			wantReject: true,
		},
		{
			name:       "provider prefixed canonical model",
			body:       `{"model":"alias","thinking":{"type":"disabled"}}`,
			model:      "Provider/GLM-5.3",
			protocol:   "CHAT",
			wantReject: true,
		},
		{
			name:     "glm 5.2 remains unknown",
			body:     `{"model":"glm-5.2","thinking":{"type":"disabled"}}`,
			model:    "glm-5.2",
			protocol: "chat",
		},
		{
			name:     "near miss model",
			body:     `{"model":"glm-5.30","thinking":{"type":"disabled"}}`,
			model:    "glm-5.30",
			protocol: "chat",
		},
		{
			name:     "different final protocol",
			body:     `{"model":"glm-5.3","thinking":{"type":"disabled"}}`,
			model:    "glm-5.3",
			protocol: "responses",
		},
		{
			name:     "value predicate is exact",
			body:     `{"model":"glm-5.3","thinking":{"type":"Disabled"}}`,
			model:    "glm-5.3",
			protocol: "chat",
		},
		{
			name:     "non-string value does not claim unsupported",
			body:     `{"model":"glm-5.3","thinking":{"type":false}}`,
			model:    "glm-5.3",
			protocol: "chat",
		},
		{
			name:     "nested unrelated path",
			body:     `{"model":"glm-5.3","metadata":{"thinking":{"type":"disabled"}}}`,
			model:    "glm-5.3",
			protocol: "chat",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			storage, err := common.CreateBodyStorage([]byte(test.body))
			require.NoError(t, err)
			defer storage.Close()
			envelope, err := parseValidatedRequestEnvelope(
				context.Background(),
				storage,
				http.MethodPost,
				"/v1/chat/completions",
				types.RelayFormatOpenAI,
				defaultStrictJSONLimits,
			)
			require.NoError(t, err)

			err = ValidateOpenCodeModelCapability(envelope, test.model, test.protocol)
			if !test.wantReject {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			validationErr, ok := AsClientRequestValidationError(err)
			require.True(t, ok)
			assert.Equal(t, http.StatusBadRequest, validationErr.StatusCode)
			assert.Equal(t, OpenCodeGLM53ThinkingDisabledRule, validationErr.RuleID)
			assert.Equal(t, OpenCodeCapabilityStage, validationErr.StageID)
			assert.Equal(t, OpenCodeGLM53ThinkingDisabledPublicMessage, validationErr.Message)
		})
	}
}

func TestCanonicalOpenCodeModelNameDoesNotUseFamilyPrefixMatching(t *testing.T) {
	assert.Equal(t, "glm-5.3", CanonicalOpenCodeModelName(" provider/GLM-5.3 "))
	assert.Equal(t, "glm-5.30", CanonicalOpenCodeModelName("glm-5.30"))
	assert.NotEqual(t, CanonicalOpenCodeModelName("glm-5.3"), CanonicalOpenCodeModelName("glm-5.30"))
}
