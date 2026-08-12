package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCodeGoIdentityAuditRecordsDoNotExposeIdentityUID(t *testing.T) {
	const identityUID = "123e4567-e89b-42d3-a456-426614174000"
	for _, action := range []string{
		"channel.opencode_go_identity_update",
		"channel.opencode_go_identity_status",
		"channel.opencode_go_cookie_replace",
		"channel.opencode_go_identity_refresh",
		"channel.opencode_go_identity_delete",
	} {
		params := sanitizeAuditParams(action, map[string]interface{}{
			"id":           7,
			"identity_uid": identityUID,
			"enabled":      true,
		})
		content := auditContentEN(action, params)
		encodedParams, err := common.Marshal(params)
		require.NoError(t, err)
		assert.NotContains(t, content, identityUID)
		assert.NotContains(t, string(encodedParams), "identity_uid")
		assert.NotContains(t, string(encodedParams), identityUID)
	}
}
