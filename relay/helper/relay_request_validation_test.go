package helper

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newRelayValidationContext(t *testing.T, path string, body []byte) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	t.Cleanup(func() { common.CleanupBodyStorage(c) })
	return c
}

func TestGetAndValidateRequestRejectsKnownClientSchemaErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		path       string
		format     types.RelayFormat
		body       []byte
		wantDetail string
	}{
		{
			name:       "messages role missing",
			path:       "/v1/messages",
			format:     types.RelayFormatClaude,
			body:       []byte(`{"model":"test-model","messages":[{"content":"hello"}]}`),
			wantDetail: "messages[0].role",
		},
		{
			name:       "messages content missing",
			path:       "/v1/messages",
			format:     types.RelayFormatClaude,
			body:       []byte(`{"model":"test-model","messages":[{"role":"user"}]}`),
			wantDetail: "messages[0].content",
		},
		{
			name:       "responses unknown non-empty item type",
			path:       "/v1/responses",
			format:     types.RelayFormatOpenAIResponses,
			body:       []byte(`{"model":"test-model","input":[{"type":"future_private_item","role":"user","content":"hello"}]}`),
			wantDetail: "input[0].type",
		},
		{
			name:       "trailing json value",
			path:       "/v1/messages",
			format:     types.RelayFormatClaude,
			body:       []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]} {}`),
			wantDetail: "JSON object",
		},
		{
			name:       "null root",
			path:       "/v1/messages",
			format:     types.RelayFormatClaude,
			body:       []byte(`null`),
			wantDetail: "JSON object",
		},
		{
			name:       "array root",
			path:       "/v1/messages",
			format:     types.RelayFormatClaude,
			body:       []byte(`[]`),
			wantDetail: "JSON object",
		},
		{
			name:       "scalar root",
			path:       "/v1/messages",
			format:     types.RelayFormatClaude,
			body:       []byte(`"request"`),
			wantDetail: "JSON object",
		},
		{
			name:       "model missing",
			path:       "/v1/messages",
			format:     types.RelayFormatClaude,
			body:       []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
			wantDetail: "model is required",
		},
		{
			name:       "model null",
			path:       "/v1/messages",
			format:     types.RelayFormatClaude,
			body:       []byte(`{"model":null,"messages":[{"role":"user","content":"hello"}]}`),
			wantDetail: "model is required",
		},
		{
			name:       "model blank",
			path:       "/v1/messages",
			format:     types.RelayFormatClaude,
			body:       []byte(`{"model":"  ","messages":[{"role":"user","content":"hello"}]}`),
			wantDetail: "model must not be empty",
		},
		{
			name:       "model wrong type",
			path:       "/v1/messages",
			format:     types.RelayFormatClaude,
			body:       []byte(`{"model":62,"messages":[{"role":"user","content":"hello"}]}`),
			wantDetail: "model must be a string",
		},
	}

	invalidUTF8 := append([]byte(`{"model":"test-model","messages":[{"role":"user","content":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`"}]}`)...)
	tests = append(tests, struct {
		name       string
		path       string
		format     types.RelayFormat
		body       []byte
		wantDetail string
	}{
		name:       "invalid utf8",
		path:       "/v1/messages",
		format:     types.RelayFormatClaude,
		body:       invalidUTF8,
		wantDetail: "UTF-8",
	})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newRelayValidationContext(t, test.path, test.body)
			_, err := GetAndValidateRequest(c, test.format)
			require.Error(t, err)
			require.Contains(t, err.Error(), test.wantDetail)
		})
	}
}

func TestGetAndValidateRequestRejectsProtocolFieldErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		path       string
		format     types.RelayFormat
		body       string
		wantDetail string
	}{
		{name: "messages must be array", path: "/v1/messages", format: types.RelayFormatClaude, body: `{"model":"test-model","messages":{}}`, wantDetail: "messages must be an array"},
		{name: "messages must not be empty", path: "/v1/messages", format: types.RelayFormatClaude, body: `{"model":"test-model","messages":[]}`, wantDetail: "messages must not be empty"},
		{name: "messages item must be object", path: "/v1/messages", format: types.RelayFormatClaude, body: `{"model":"test-model","messages":["hello"]}`, wantDetail: "messages[0] must be an object"},
		{name: "messages role unsupported", path: "/v1/messages", format: types.RelayFormatClaude, body: `{"model":"test-model","messages":[{"role":"developer","content":"hello"}]}`, wantDetail: "messages[0].role"},
		{name: "messages content wrong type", path: "/v1/messages", format: types.RelayFormatClaude, body: `{"model":"test-model","messages":[{"role":"user","content":42}]}`, wantDetail: "messages[0].content"},
		{name: "messages content item must be object", path: "/v1/messages", format: types.RelayFormatClaude, body: `{"model":"test-model","messages":[{"role":"user","content":["hello"]}]}`, wantDetail: "messages[0].content[0]"},
		{name: "messages content type missing", path: "/v1/messages", format: types.RelayFormatClaude, body: `{"model":"test-model","messages":[{"role":"user","content":[{"text":"hello"}]}]}`, wantDetail: "messages[0].content[0].type"},
		{name: "messages text missing", path: "/v1/messages", format: types.RelayFormatClaude, body: `{"model":"test-model","messages":[{"role":"user","content":[{"type":"text"}]}]}`, wantDetail: "messages[0].content[0].text"},
		{name: "messages tool use missing id", path: "/v1/messages", format: types.RelayFormatClaude, body: `{"model":"test-model","messages":[{"role":"assistant","content":[{"type":"tool_use","name":"lookup","input":{}}]}]}`, wantDetail: "messages[0].content[0].id"},
		{name: "messages tool use input must be object", path: "/v1/messages", format: types.RelayFormatClaude, body: `{"model":"test-model","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":"{}"}]}]}`, wantDetail: "messages[0].content[0].input"},
		{name: "messages tool result missing id", path: "/v1/messages", format: types.RelayFormatClaude, body: `{"model":"test-model","messages":[{"role":"user","content":[{"type":"tool_result","content":"ok"}]}]}`, wantDetail: "messages[0].content[0].tool_use_id"},
		{name: "messages tool result content wrong type", path: "/v1/messages", format: types.RelayFormatClaude, body: `{"model":"test-model","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":42}]}]}`, wantDetail: "messages[0].content[0].content"},
		{name: "messages system media is unsupported", path: "/v1/messages", format: types.RelayFormatClaude, body: `{"model":"test-model","messages":[{"role":"system","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]},{"role":"user","content":"hello"}]}`, wantDetail: "messages[0].content[0].type"},
		{name: "messages system only", path: "/v1/messages", format: types.RelayFormatClaude, body: `{"model":"test-model","messages":[{"role":"system","content":"rules"}]}`, wantDetail: "at least one user or assistant"},
		{name: "messages system wrong type", path: "/v1/messages", format: types.RelayFormatClaude, body: `{"model":"test-model","system":42,"messages":[{"role":"user","content":"hello"}]}`, wantDetail: "system"},
		{name: "messages system block type unsupported", path: "/v1/messages", format: types.RelayFormatClaude, body: `{"model":"test-model","system":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}],"messages":[{"role":"user","content":"hello"}]}`, wantDetail: "system[0].type"},
		{name: "messages content type unsupported", path: "/v1/messages", format: types.RelayFormatClaude, body: `{"model":"test-model","messages":[{"role":"user","content":[{"type":"future_private_block"}]}]}`, wantDetail: "messages[0].content[0].type"},

		{name: "chat messages must be array", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":{}}`, wantDetail: "messages must be an array"},
		{name: "chat message must be object", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":["hello"]}`, wantDetail: "messages[0] must be an object"},
		{name: "chat role missing", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"content":"hello"}]}`, wantDetail: "messages[0].role"},
		{name: "chat role unsupported", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"future","content":"hello"}]}`, wantDetail: "messages[0].role"},
		{name: "chat user content wrong type", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"user","content":42}]}`, wantDetail: "messages[0].content"},
		{name: "chat system media is unsupported", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"system","content":[{"type":"image_url","image_url":"https://example.test/image.png"}]}]}`, wantDetail: "messages[0].content[0].type"},
		{name: "chat assistant input media is unsupported", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"assistant","content":[{"type":"input_audio","input_audio":{"data":"abc","format":"wav"}}]}]}`, wantDetail: "messages[0].content[0].type"},
		{name: "chat responses text part is unsupported", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`, wantDetail: "messages[0].content[0].type"},
		{name: "chat content part missing type", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"user","content":[{"text":"hello"}]}]}`, wantDetail: "messages[0].content[0].type"},
		{name: "chat text part missing text", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"user","content":[{"type":"text"}]}]}`, wantDetail: "messages[0].content[0].text"},
		{name: "chat prompt cache breakpoint unsupported", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"system","content":[{"type":"text","text":"rules","prompt_cache_breakpoint":{"mode":"explicit"}}]}]}`, wantDetail: "messages[0].content[0].prompt_cache_breakpoint"},
		{name: "chat assistant empty", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"assistant","content":null}]}`, wantDetail: "messages[0] requires"},
		{name: "chat assistant empty string only", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"assistant","content":""}]}`, wantDetail: "messages[0].content must not be empty"},
		{name: "chat assistant empty structured content", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"assistant","content":[]}]}`, wantDetail: "messages[0].content must not be empty"},
		{name: "chat assistant empty structured content with tool calls", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"assistant","content":[],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]}`, wantDetail: "messages[0].content must not be empty"},
		{name: "chat assistant name unsupported", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"assistant","name":"agent","content":"hello"}]}`, wantDetail: "messages[0].name"},
		{name: "chat assistant null name unsupported", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"assistant","name":null,"content":"hello"}]}`, wantDetail: "messages[0].name"},
		{name: "chat assistant refusal field unsupported", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"assistant","refusal":"cannot comply"}]}`, wantDetail: "messages[0].refusal"},
		{name: "chat assistant refusal part unsupported", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"assistant","content":[{"type":"refusal","refusal":"cannot comply"}]}]}`, wantDetail: "messages[0].content[0].type"},
		{name: "chat assistant legacy function call unsupported", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"assistant","function_call":{"name":"lookup","arguments":"{}"}}]}`, wantDetail: "messages[0].function_call"},
		{name: "chat assistant prior audio unsupported", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"assistant","audio":{"id":"audio_1"}}]}`, wantDetail: "messages[0].audio"},
		{name: "chat tool calls wrong type", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"assistant","tool_calls":{}}]}`, wantDetail: "messages[0].tool_calls"},
		{name: "chat tool call missing id", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"lookup","arguments":"{}"}}]}]}`, wantDetail: "messages[0].tool_calls[0].id"},
		{name: "chat tool call missing type", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","function":{"name":"lookup","arguments":"{}"}}]}]}`, wantDetail: "messages[0].tool_calls[0].type"},
		{name: "chat custom tool call unsupported", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"custom","custom":{"name":"lookup","input":"{}"}}]}]}`, wantDetail: "messages[0].tool_calls[0].type"},
		{name: "chat tool call missing arguments", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup"}}]}]}`, wantDetail: "messages[0].tool_calls[0].function.arguments"},
		{name: "chat tool missing call id", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"tool","content":"ok"}]}`, wantDetail: "messages[0].tool_call_id"},
		{name: "chat function missing name", path: "/v1/chat/completions", format: types.RelayFormatOpenAI, body: `{"model":"test-model","messages":[{"role":"function","content":"ok"}]}`, wantDetail: "messages[0].name"},

		{name: "responses input wrong type", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":42}`, wantDetail: "input must be a string or array"},
		{name: "responses item must be object", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":["hello"]}`, wantDetail: "input[0] must be an object"},
		{name: "responses message role unsupported", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"role":"future","content":"hello"}]}`, wantDetail: "input[0].role"},
		{name: "responses message content wrong type", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"role":"user","content":42}]}`, wantDetail: "input[0].content"},
		{name: "responses user output content is unsupported", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"role":"user","content":[{"type":"output_text","text":"answer"}]}]}`, wantDetail: "input[0].content[0].type"},
		{name: "responses incomplete output message missing id", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"answer"}]}]}`, wantDetail: "input[0].id"},
		{name: "responses output message requires explicit type", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`, wantDetail: "input[0].type"},
		{name: "responses output message status null", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"message","id":"msg_1","status":null,"role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`, wantDetail: "input[0].status"},
		{name: "responses output message status wrong type", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"message","id":"msg_1","status":42,"role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`, wantDetail: "input[0].status"},
		{name: "responses output message status empty", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"message","id":"msg_1","status":"","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`, wantDetail: "input[0].status"},
		{name: "responses output message status unsupported", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"message","id":"msg_1","status":"future","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`, wantDetail: "input[0].status"},
		{name: "responses output message only permits output content", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"input_text","text":"answer"}]}]}`, wantDetail: "input[0].content[0].type"},
		{name: "responses output message content must be array", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":"answer"}]}`, wantDetail: "input[0].content"},
		{name: "responses output annotations null", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":null,"logprobs":[]}]}]}`, wantDetail: "input[0].content[0].annotations"},
		{name: "responses output logprobs null", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[],"logprobs":null}]}]}`, wantDetail: "input[0].content[0].logprobs"},
		{name: "responses output annotations must be array", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":{},"logprobs":[]}]}]}`, wantDetail: "input[0].content[0].annotations"},
		{name: "responses output logprobs must be array", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[],"logprobs":{}}]}]}`, wantDetail: "input[0].content[0].logprobs"},
		{name: "responses output annotation must be object", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[42],"logprobs":[]}]}]}`, wantDetail: "input[0].content[0].annotations[0]"},
		{name: "responses output annotation type unsupported", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[{"type":"future_citation"}],"logprobs":[]}]}]}`, wantDetail: "input[0].content[0].annotations[0].type"},
		{name: "responses output logprob token wrong type", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[],"logprobs":[{"token":42,"bytes":[],"logprob":-1,"top_logprobs":[]}]}]}]}`, wantDetail: "input[0].content[0].logprobs[0].token"},
		{name: "responses output logprob bytes wrong type", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[],"logprobs":[{"token":"a","bytes":["97"],"logprob":-1,"top_logprobs":[]}]}]}]}`, wantDetail: "input[0].content[0].logprobs[0].bytes[0]"},
		{name: "responses message part missing type", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"role":"user","content":[{"text":"hello"}]}]}`, wantDetail: "input[0].content[0].type"},
		{name: "responses function call missing arguments", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"function_call","call_id":"call_1","name":"lookup"}]}`, wantDetail: "input[0].arguments"},
		{name: "responses custom call missing input", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"custom_tool_call","call_id":"call_1","name":"patch"}]}`, wantDetail: "input[0].input"},
		{name: "responses function output missing output", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"function_call_output","call_id":"call_1"}]}`, wantDetail: "input[0].output"},
		{name: "responses reasoning missing id", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"reasoning","summary":[]}]}`, wantDetail: "input[0].id"},
		{name: "responses reasoning missing summary", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"reasoning","id":"rs_1"}]}`, wantDetail: "input[0].summary"},
		{name: "responses reasoning summary wrong type", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"reasoning","id":"rs_1","summary":{}}]}`, wantDetail: "input[0].summary"},
		{name: "responses reasoning summary item type unsupported", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"reasoning","id":"rs_1","summary":[{"type":"output_text","text":"not a summary"}]}]}`, wantDetail: "input[0].summary[0].type"},
		{name: "responses reasoning content wrong type", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"reasoning","id":"rs_1","summary":[],"content":{}}]}`, wantDetail: "input[0].content"},
		{name: "responses reasoning content item type unsupported", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"summary_text","text":"not reasoning text"}]}]}`, wantDetail: "input[0].content[0].type"},
		{name: "responses reasoning encrypted content wrong type", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":{}}]}`, wantDetail: "input[0].encrypted_content"},
		{name: "responses reasoning status wrong type", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"reasoning","id":"rs_1","summary":[],"status":42}]}`, wantDetail: "input[0].status"},
		{name: "responses reasoning status null", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"reasoning","id":"rs_1","summary":[],"status":null}]}`, wantDetail: "input[0].status"},
		{name: "responses reasoning status empty", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"reasoning","id":"rs_1","summary":[],"status":""}]}`, wantDetail: "input[0].status"},
		{name: "responses reasoning status unsupported", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"reasoning","id":"rs_1","summary":[],"status":"future"}]}`, wantDetail: "input[0].status"},
		{name: "responses additional tool must be object", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"additional_tools","tools":["silently-dropped"]}]}`, wantDetail: "input[0].tools[0]"},
		{name: "responses additional tool requires type", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"additional_tools","tools":[{}]}]}`, wantDetail: "input[0].tools[0].type"},
		{name: "responses additional tool requires name", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"additional_tools","tools":[{"type":"custom"}]}]}`, wantDetail: "input[0].tools[0].name"},
		{name: "responses additional tool type must be supported", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"additional_tools","tools":[{"type":"future_tool","name":"hidden"}]}]}`, wantDetail: "input[0].tools[0].type"},
		{name: "responses namespace children must be supported", path: "/v1/responses", format: types.RelayFormatOpenAIResponses, body: `{"model":"test-model","input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"repo","tools":[{"type":"custom","name":"silently_dropped"}]}]}]}`, wantDetail: "input[0].tools[0].tools[0].type"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newRelayValidationContext(t, test.path, []byte(test.body))
			_, err := GetAndValidateRequest(c, test.format)
			require.Error(t, err)
			require.Contains(t, err.Error(), test.wantDetail)
			validationErr, ok := AsClientRequestValidationError(err)
			require.True(t, ok)
			require.Equal(t, http.StatusBadRequest, validationErr.StatusCode)
		})
	}
}

func TestClaudeMessageSystemIsNormalizedIntoTopLevelSystem(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantSystem    []string
		wantStringSys string
		wantMessages  []string
	}{
		{
			name:          "string message system",
			body:          `{"model":"test-model","messages":[{"role":"system","content":"rules"},{"role":"user","content":"hello"}]}`,
			wantStringSys: "rules",
			wantMessages:  []string{"user"},
		},
		{
			name:         "block message system",
			body:         `{"model":"test-model","messages":[{"role":"user","content":"hello"},{"role":"system","content":[{"type":"text","text":"middle"},{"type":"input_text","text":"last"}]},{"role":"assistant","content":"ok"}]}`,
			wantSystem:   []string{"middle", "last"},
			wantMessages: []string{"user", "assistant"},
		},
		{
			name:         "top-level and message systems preserve order",
			body:         `{"model":"test-model","system":[{"type":"text","text":"top"}],"messages":[{"role":"user","content":"hello"},{"role":"system","content":"middle"},{"role":"system","content":[{"type":"text","text":"last"}]},{"role":"assistant","content":"ok"}]}`,
			wantSystem:   []string{"top", "middle", "last"},
			wantMessages: []string{"user", "assistant"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := GetAndValidateRequest(newRelayValidationContext(t, "/v1/messages", []byte(test.body)), types.RelayFormatClaude)
			require.NoError(t, err)
			claudeRequest, ok := request.(*dto.ClaudeRequest)
			require.True(t, ok)
			require.Len(t, claudeRequest.Messages, len(test.wantMessages))
			for index, role := range test.wantMessages {
				require.Equal(t, role, claudeRequest.Messages[index].Role)
			}

			if test.wantStringSys != "" {
				require.True(t, claudeRequest.IsStringSystem())
				require.Equal(t, test.wantStringSys, claudeRequest.GetStringSystem())
				return
			}

			systemParts := claudeRequest.ParseSystem()
			require.Len(t, systemParts, len(test.wantSystem))
			for index, want := range test.wantSystem {
				require.Equal(t, want, systemParts[index].GetText())
			}
		})
	}
}

func TestGetAndValidateRequestPreservesCompatibilityAndCachesTypedRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name   string
		path   string
		format types.RelayFormat
		body   string
	}{
		{
			name:   "chat fim without messages",
			path:   "/v1/chat/completions",
			format: types.RelayFormatOpenAI,
			body:   `{"model":"test-model","prefix":"func main() {","suffix":"}"}`,
		},
		{
			name:   "chat assistant tool-only",
			path:   "/v1/chat/completions",
			format: types.RelayFormatOpenAI,
			body:   `{"model":"test-model","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]}`,
		},
		{
			name:   "chat assistant empty string with tool calls",
			path:   "/v1/chat/completions",
			format: types.RelayFormatOpenAI,
			body:   `{"model":"test-model","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]}`,
		},
		{
			name:   "chat function arguments remain opaque strings",
			path:   "/v1/chat/completions",
			format: types.RelayFormatOpenAI,
			body:   `{"model":"test-model","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{not-json"}}]}]}`,
		},
		{
			name:   "chat tool result",
			path:   "/v1/chat/completions",
			format: types.RelayFormatOpenAI,
			body:   `{"model":"test-model","messages":[{"role":"assistant","reasoning_content":"inspect"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"ok"}]}`,
		},
		{
			name:   "claude thinking tool history",
			path:   "/v1/messages",
			format: types.RelayFormatClaude,
			body:   `{"model":"test-model","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"inspect"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}]}`,
		},
		{
			name:   "claude system blocks and tool result blocks",
			path:   "/v1/messages",
			format: types.RelayFormatClaude,
			body:   `{"model":"test-model","system":[{"type":"text","text":"rules"}],"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"ok"}]}]}]}`,
		},
		{
			name:   "responses message may omit type",
			path:   "/v1/responses",
			format: types.RelayFormatOpenAIResponses,
			body:   `{"model":"test-model","input":[{"role":"user","content":"hello"}],"provider_extension":{"kept":true}}`,
		},
		{
			name:   "responses easy messages allow input content for every role",
			path:   "/v1/responses",
			format: types.RelayFormatOpenAIResponses,
			body:   `{"model":"test-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"question"}]},{"type":"message","role":"assistant","content":[{"type":"input_image","image_url":"https://example.test/answer.png"}]},{"type":"message","role":"system","content":[{"type":"input_file","file_id":"file_system"}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"instruction"}]}]}`,
		},
		{
			name:   "responses complete output message replay",
			path:   "/v1/responses",
			format: types.RelayFormatOpenAIResponses,
			body:   `{"model":"test-model","input":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[],"logprobs":[]},{"type":"refusal","refusal":"cannot continue"}]}]}`,
		},
		{
			name:   "responses output replay metadata",
			path:   "/v1/responses",
			format: types.RelayFormatOpenAIResponses,
			body:   `{"model":"test-model","input":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"","annotations":[{"type":"url_citation","url":"https://example.test","title":"source","start_index":0,"end_index":0}],"logprobs":[{"token":"","bytes":[],"logprob":-1,"top_logprobs":[{"token":"a","bytes":[97],"logprob":-2}]}]}]}]}`,
		},
		{
			name:   "responses empty input text and refusal strings",
			path:   "/v1/responses",
			format: types.RelayFormatOpenAIResponses,
			body:   `{"model":"test-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]},{"type":"message","id":"msg_refusal","status":"completed","role":"assistant","content":[{"type":"refusal","refusal":""}]}]}`,
		},
		{
			name:   "responses string input",
			path:   "/v1/responses",
			format: types.RelayFormatOpenAIResponses,
			body:   `{"model":"test-model","input":"hello"}`,
		},
		{
			name:   "responses empty stateful continuation",
			path:   "/v1/responses",
			format: types.RelayFormatOpenAIResponses,
			body:   `{"model":"test-model","input":[],"previous_response_id":"resp_previous"}`,
		},
		{
			name:   "responses codex additional tools and nullable custom input",
			path:   "/v1/responses",
			format: types.RelayFormatOpenAIResponses,
			body:   `{"model":"test-model","input":[{"type":"additional_tools","tools":[{"type":"custom","name":"apply_patch"}]},{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":""}]},{"type":"custom_tool_call","call_id":"call_1","name":"apply_patch","input":null}]}`,
		},
		{
			name:   "responses reasoning and function history",
			path:   "/v1/responses",
			format: types.RelayFormatOpenAIResponses,
			body:   `{"model":"test-model","input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]},{"type":"reasoning","id":"rs_1","status":"completed","summary":[{"type":"summary_text","text":"inspect"}],"content":[{"type":"reasoning_text","text":"full reasoning"}],"encrypted_content":null},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"ok"},{"type":"custom_tool_call","call_id":"call_2","name":"patch","input":"diff"},{"type":"custom_tool_call_output","call_id":"call_2","output":"done"}]}`,
		},
		{
			name:   "responses refusal history uses refusal field",
			path:   "/v1/responses",
			format: types.RelayFormatOpenAIResponses,
			body:   `{"model":"test-model","input":[{"type":"message","id":"msg_refusal","status":"completed","role":"assistant","content":[{"type":"refusal","refusal":"I cannot help with that."}]}]}`,
		},
		{
			name:   "responses namespace function tools",
			path:   "/v1/responses",
			format: types.RelayFormatOpenAIResponses,
			body:   `{"model":"test-model","input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"repo","tools":[{"type":"function","name":"read_file"}]}]}]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newRelayValidationContext(t, test.path, []byte(test.body))
			first, err := GetAndValidateRequest(c, test.format)
			require.NoError(t, err)
			require.NotNil(t, first)

			model, found, err := GetCachedValidatedModel(c)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, "test-model", model)

			second, err := GetAndValidateRequest(c, test.format)
			require.NoError(t, err)
			require.Same(t, first, second)
		})
	}
}

func TestGetAndValidateRequestNormalizesCodexOutputReplayDefaults(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := newRelayValidationContext(t, "/v1/responses", []byte(`{
		"model":"kimi-k3",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hello"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"do you love me?"}]}
		]
	}`))

	request, err := GetAndValidateRequest(c, types.RelayFormatOpenAIResponses)
	require.NoError(t, err)
	responsesRequest, ok := request.(*dto.OpenAIResponsesRequest)
	require.True(t, ok)

	var input []map[string]any
	require.NoError(t, common.Unmarshal(responsesRequest.Input, &input))
	require.Len(t, input, 3)
	require.Equal(t, "completed", input[1]["status"])
	outputText := input[1]["content"].([]any)[0].(map[string]any)
	require.Equal(t, []any{}, outputText["annotations"])
	require.Equal(t, []any{}, outputText["logprobs"])

	cached, err := GetAndValidateRequest(c, types.RelayFormatOpenAIResponses)
	require.NoError(t, err)
	require.Same(t, request, cached)
}

func TestProtocolValidationCompatibilityPairs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name           string
		path           string
		format         types.RelayFormat
		invalidBody    string
		wantDetail     string
		compatibleBody string
	}{
		{
			name:           "claude text blocks may be empty",
			path:           "/v1/messages",
			format:         types.RelayFormatClaude,
			invalidBody:    `{"model":"test-model","messages":[{"role":"user","content":[{"type":"text"}]}]}`,
			wantDetail:     "messages[0].content[0].text",
			compatibleBody: `{"model":"test-model","messages":[{"role":"user","content":[{"type":"text","text":""}]}]}`,
		},
		{
			name:           "claude media source must contain usable data",
			path:           "/v1/messages",
			format:         types.RelayFormatClaude,
			invalidBody:    `{"model":"test-model","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png"}}]}]}`,
			wantDetail:     "messages[0].content[0].source.data",
			compatibleBody: `{"model":"test-model","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}`,
		},
		{
			name:           "claude media source type must be supported",
			path:           "/v1/messages",
			format:         types.RelayFormatClaude,
			invalidBody:    `{"model":"test-model","messages":[{"role":"user","content":[{"type":"image","source":{"type":"future_source","url":"https://example.test/image.png"}}]}]}`,
			wantDetail:     "messages[0].content[0].source.type",
			compatibleBody: `{"model":"test-model","messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.test/image.png"}}]}]}`,
		},
		{
			name:           "claude redacted thinking preserves encrypted data",
			path:           "/v1/messages",
			format:         types.RelayFormatClaude,
			invalidBody:    `{"model":"test-model","messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":{}}]}]}`,
			wantDetail:     "messages[0].content[0].data",
			compatibleBody: `{"model":"test-model","messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"encrypted-state"},{"type":"text","text":""}]}]}`,
		},
		{
			name:           "chat image object must contain url",
			path:           "/v1/chat/completions",
			format:         types.RelayFormatOpenAI,
			invalidBody:    `{"model":"test-model","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"detail":"low"}}]}]}`,
			wantDetail:     "messages[0].content[0].image_url.url",
			compatibleBody: `{"model":"test-model","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/image.png","detail":"low"}}]}]}`,
		},
		{
			name:           "chat audio must contain data and format",
			path:           "/v1/chat/completions",
			format:         types.RelayFormatOpenAI,
			invalidBody:    `{"model":"test-model","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"abc"}}]}]}`,
			wantDetail:     "messages[0].content[0].input_audio.format",
			compatibleBody: `{"model":"test-model","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"abc","format":"wav"}}]}]}`,
		},
		{
			name:           "chat file must contain a converter-supported payload",
			path:           "/v1/chat/completions",
			format:         types.RelayFormatOpenAI,
			invalidBody:    `{"model":"test-model","messages":[{"role":"user","content":[{"type":"file","file":{"filename":"a.txt"}}]}]}`,
			wantDetail:     "messages[0].content[0].file",
			compatibleBody: `{"model":"test-model","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file_1"}}]}]}`,
		},
		{
			name:           "chat function tool history requires string arguments",
			path:           "/v1/chat/completions",
			format:         types.RelayFormatOpenAI,
			invalidBody:    `{"model":"test-model","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":{}}}]}]}`,
			wantDetail:     "messages[0].tool_calls[0].function.arguments",
			compatibleBody: `{"model":"test-model","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":""}}]}]}`,
		},
		{
			name:           "responses image accepts file id",
			path:           "/v1/responses",
			format:         types.RelayFormatOpenAIResponses,
			invalidBody:    `{"model":"test-model","input":[{"role":"user","content":[{"type":"input_image","file_id":7}]}]}`,
			wantDetail:     "input[0].content[0].file_id",
			compatibleBody: `{"model":"test-model","input":[{"role":"user","content":[{"type":"input_image","file_id":"file_1"}]}]}`,
		},
		{
			name:           "responses function arguments must be a present string",
			path:           "/v1/responses",
			format:         types.RelayFormatOpenAIResponses,
			invalidBody:    `{"model":"test-model","input":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":{}}]}`,
			wantDetail:     "input[0].arguments",
			compatibleBody: `{"model":"test-model","input":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":""}]}`,
		},
		{
			name:           "responses audio must contain converter fields",
			path:           "/v1/responses",
			format:         types.RelayFormatOpenAIResponses,
			invalidBody:    `{"model":"test-model","input":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"abc"}}]}]}`,
			wantDetail:     "input[0].content[0].input_audio.format",
			compatibleBody: `{"model":"test-model","input":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"abc","format":"wav"}}]}]}`,
		},
		{
			name:           "responses file must contain a converter-supported payload",
			path:           "/v1/responses",
			format:         types.RelayFormatOpenAIResponses,
			invalidBody:    `{"model":"test-model","input":[{"role":"user","content":[{"type":"input_file","file":{"filename":"a.txt"}}]}]}`,
			wantDetail:     "input[0].content[0].file",
			compatibleBody: `{"model":"test-model","input":[{"role":"user","content":[{"type":"input_file","file":{"file_id":"file_1"}}]}]}`,
		},
		{
			name:           "responses video must contain a usable url",
			path:           "/v1/responses",
			format:         types.RelayFormatOpenAIResponses,
			invalidBody:    `{"model":"test-model","input":[{"role":"user","content":[{"type":"input_video","video_url":{}}]}]}`,
			wantDetail:     "input[0].content[0].video_url.url",
			compatibleBody: `{"model":"test-model","input":[{"role":"user","content":[{"type":"input_video","video_url":{"url":"https://example.test/video.mp4"}}]}]}`,
		},
		{
			name:           "responses empty input requires stateful anchor",
			path:           "/v1/responses",
			format:         types.RelayFormatOpenAIResponses,
			invalidBody:    `{"model":"test-model","input":[]}`,
			wantDetail:     "stateful response anchor",
			compatibleBody: `{"model":"test-model","input":[],"previous_response_id":"resp_previous"}`,
		},
		{
			name:           "responses conversation also anchors empty input",
			path:           "/v1/responses",
			format:         types.RelayFormatOpenAIResponses,
			invalidBody:    `{"model":"test-model","input":[],"conversation":null}`,
			wantDetail:     "stateful response anchor",
			compatibleBody: `{"model":"test-model","input":[],"conversation":"conv_previous"}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invalidContext := newRelayValidationContext(t, test.path, []byte(test.invalidBody))
			_, err := GetAndValidateRequest(invalidContext, test.format)
			require.Error(t, err)
			require.Contains(t, err.Error(), test.wantDetail)

			compatibleContext := newRelayValidationContext(t, test.path, []byte(test.compatibleBody))
			request, err := GetAndValidateRequest(compatibleContext, test.format)
			require.NoError(t, err)
			require.NotNil(t, request)
		})
	}
}

func TestClaudeRedactedThinkingDataSurvivesCachedDTORoundTrip(t *testing.T) {
	body := `{"model":"test-model","messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"encrypted-state"},{"type":"text","text":""}]}]}`
	c := newRelayValidationContext(t, "/v1/messages", []byte(body))

	request, err := GetAndValidateRequest(c, types.RelayFormatClaude)
	require.NoError(t, err)
	require.IsType(t, &dto.ClaudeRequest{}, request)
	parts, err := request.(*dto.ClaudeRequest).Messages[0].ParseContent()
	require.NoError(t, err)
	require.Len(t, parts, 2)
	require.Equal(t, "encrypted-state", parts[0].Data)

	roundTrip, err := common.Marshal(request)
	require.NoError(t, err)
	require.JSONEq(t, body, string(roundTrip))
}

func TestStrictValidationUsesDiskBackedBodyStorage(t *testing.T) {
	originalConfig := common.GetDiskCacheConfig()
	common.SetDiskCacheConfig(common.DiskCacheConfig{
		Enabled:     true,
		ThresholdMB: 0,
		MaxSizeMB:   16,
		Path:        filepath.Clean(t.TempDir()),
	})
	t.Cleanup(func() { common.SetDiskCacheConfig(originalConfig) })

	c := newRelayValidationContext(t, "/v1/messages", []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`))
	first, err := GetAndValidateRequest(c, types.RelayFormatClaude)
	require.NoError(t, err)
	require.NotNil(t, first)

	storage, err := common.GetBodyStorage(c)
	require.NoError(t, err)
	require.True(t, storage.IsDisk())

	second, err := GetAndValidateRequest(c, types.RelayFormatClaude)
	require.NoError(t, err)
	require.Same(t, first, second)
}

func TestClaudeRequestMayOmitMaxTokensForGatewayDefault(t *testing.T) {
	c := newRelayValidationContext(t, "/v1/messages", []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`))

	request, err := GetAndValidateRequest(c, types.RelayFormatClaude)
	require.NoError(t, err)
	require.Nil(t, request.(*dto.ClaudeRequest).MaxTokens)
}

func TestGetAndValidateRequestFailsClosedOnCacheMismatch(t *testing.T) {
	c := newRelayValidationContext(t, "/v1/messages", []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`))
	request, err := GetAndValidateRequest(c, types.RelayFormatClaude)
	require.NoError(t, err)
	require.IsType(t, &dto.ClaudeRequest{}, request)

	_, err = GetAndValidateRequest(c, types.RelayFormatOpenAI)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cached validated request")
}

func TestGetAndValidateRequestFailsClosedOnCorruptCache(t *testing.T) {
	tests := []struct {
		name   string
		cached any
	}{
		{name: "wrong cache value type", cached: "not-a-request"},
		{name: "nil cache entry", cached: (*cachedValidatedRequest)(nil)},
		{
			name: "wrong typed request",
			cached: &cachedValidatedRequest{
				format:  types.RelayFormatClaude,
				path:    "/v1/messages",
				model:   "test-model",
				request: &dto.GeneralOpenAIRequest{Model: "test-model"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newRelayValidationContext(t, "/v1/messages", []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`))
			c.Set(cachedValidatedRequestKey, test.cached)
			_, err := GetAndValidateRequest(c, types.RelayFormatClaude)
			require.Error(t, err)
			require.Contains(t, err.Error(), "cached validated request")
		})
	}
}

func TestGetCachedValidatedModelFailsClosedOnMutationAndPathMismatch(t *testing.T) {
	c := newRelayValidationContext(t, "/v1/messages", []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`))
	request, err := GetAndValidateRequest(c, types.RelayFormatClaude)
	require.NoError(t, err)
	request.(*dto.ClaudeRequest).Model = "changed-model"

	_, found, err := GetCachedValidatedModel(c)
	require.True(t, found)
	require.Error(t, err)
	require.Contains(t, err.Error(), "model")

	request.(*dto.ClaudeRequest).Model = "test-model"
	c.Request.URL.Path = "/v1/chat/completions"
	_, found, err = GetCachedValidatedModel(c)
	require.True(t, found)
	require.Error(t, err)
	require.Contains(t, err.Error(), "path")
}

func TestGetCachedValidatedModelFailsClosedOnRouteFormatMismatch(t *testing.T) {
	c := newRelayValidationContext(t, "/v1/messages", []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`))
	c.Set(cachedValidatedRequestKey, &cachedValidatedRequest{
		format:  types.RelayFormatOpenAI,
		path:    "/v1/messages",
		model:   "test-model",
		request: &dto.GeneralOpenAIRequest{Model: "test-model"},
	})

	_, found, err := GetCachedValidatedModel(c)
	require.True(t, found)
	require.Error(t, err)
	require.Contains(t, err.Error(), "format")
}
