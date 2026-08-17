package helper

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/require"
)

func TestClaudeDocumentValidationAcceptsPinnedSources(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
	}{
		{name: "base64 PDF", source: `{"type":"base64","media_type":"application/pdf","data":"AA=="}`},
		{name: "plain text", source: `{"type":"text","media_type":"text/plain","data":"synthetic text"}`},
		{name: "HTTPS URL", source: `{"type":"url","url":"https://example.invalid/document.pdf"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := fmt.Sprintf(
				`{"model":"test-model","messages":[{"role":"user","content":[{"type":"document","source":%s,"citations":{"enabled":true},"context":"synthetic context","title":"synthetic title"}]}]}`,
				test.source,
			)
			_, err := GetAndValidateRequest(
				newRelayValidationContext(t, "/v1/messages", []byte(body)),
				types.RelayFormatClaude,
			)
			require.NoError(t, err)
		})
	}
}

func TestClaudeDocumentValidationRejectsUnregisteredShapes(t *testing.T) {
	for _, test := range []struct {
		name     string
		document string
	}{
		{name: "unknown outer member", document: `{"type":"document","source":{"type":"text","media_type":"text/plain","data":"text"},"private_marker":true}`},
		{name: "unknown source member", document: `{"type":"document","source":{"type":"text","media_type":"text/plain","data":"text","private_marker":true}}`},
		{name: "unknown source type", document: `{"type":"document","source":{"type":"content","content":[]}}`},
		{name: "invalid base64", document: `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"not-base64"}}`},
		{name: "base64 line break", document: `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AA\n=="}}`},
		{name: "non canonical base64 padding bits", document: `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AB=="}}`},
		{name: "base64 MIME mismatch", document: `{"type":"document","source":{"type":"base64","media_type":"text/plain","data":"AA=="}}`},
		{name: "text MIME mismatch", document: `{"type":"document","source":{"type":"text","media_type":"application/pdf","data":"text"}}`},
		{name: "empty text", document: `{"type":"document","source":{"type":"text","media_type":"text/plain","data":""}}`},
		{name: "non HTTP URL", document: `{"type":"document","source":{"type":"url","url":"file:///tmp/document.pdf"}}`},
		{name: "URL user information", document: `{"type":"document","source":{"type":"url","url":"https://user:password@example.invalid/document.pdf"}}`},
		{name: "URL whitespace", document: `{"type":"document","source":{"type":"url","url":"https://example.invalid/a b.pdf"}}`},
		{name: "citations wrong type", document: `{"type":"document","source":{"type":"text","media_type":"text/plain","data":"text"},"citations":true}`},
		{name: "citations unknown member", document: `{"type":"document","source":{"type":"text","media_type":"text/plain","data":"text"},"citations":{"enabled":true,"private_marker":true}}`},
		{name: "citations enabled wrong type", document: `{"type":"document","source":{"type":"text","media_type":"text/plain","data":"text"},"citations":{"enabled":"yes"}}`},
		{name: "context wrong type", document: `{"type":"document","source":{"type":"text","media_type":"text/plain","data":"text"},"context":[]}`},
		{name: "title wrong type", document: `{"type":"document","source":{"type":"text","media_type":"text/plain","data":"text"},"title":42}`},
		{name: "duplicate source member", document: `{"type":"document","source":{"type":"text","media_type":"text/plain","data":"first","data":"second"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := fmt.Sprintf(
				`{"model":"test-model","messages":[{"role":"user","content":[%s]}]}`,
				test.document,
			)
			_, err := GetAndValidateRequest(
				newRelayValidationContext(t, "/v1/messages", []byte(body)),
				types.RelayFormatClaude,
			)
			require.Error(t, err)
			validationErr, ok := AsClientRequestValidationError(err)
			require.True(t, ok)
			require.Equal(t, 400, validationErr.StatusCode)
		})
	}
}
