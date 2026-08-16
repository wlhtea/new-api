package common

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeJsonUseNumberPreservesLargeIntegerLexeme(t *testing.T) {
	const integer = "900719925474099312345678901234567890"
	var decoded map[string]any

	err := DecodeJsonUseNumber(strings.NewReader(`{"value":`+integer+`}`), &decoded)

	require.NoError(t, err)
	number, ok := decoded["value"].(json.Number)
	require.True(t, ok)
	require.Equal(t, integer, number.String())
}

func TestNewJsonDecoderUseNumberPreservesTokenLexeme(t *testing.T) {
	const integer = "9007199254740993"
	decoder := NewJsonDecoderUseNumber(strings.NewReader(`[` + integer + `]`))

	opening, err := decoder.Token()
	require.NoError(t, err)
	require.Equal(t, json.Delim('['), opening)

	value, err := decoder.Token()
	require.NoError(t, err)
	number, ok := value.(json.Number)
	require.True(t, ok)
	require.Equal(t, integer, number.String())
}

func TestJsonRawMessageToString(t *testing.T) {
	tests := []struct {
		name string
		data json.RawMessage
		want string
	}{
		{
			name: "object",
			data: json.RawMessage(`{"city":"Paris","days":0,"strict":false}`),
			want: `{"city":"Paris","days":0,"strict":false}`,
		},
		{
			name: "string",
			data: json.RawMessage(`"{\"city\":\"Paris\",\"days\":0,\"strict\":false}"`),
			want: `{"city":"Paris","days":0,"strict":false}`,
		},
		{
			name: "null",
			data: json.RawMessage(`null`),
			want: "",
		},
		{
			name: "empty",
			data: nil,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, JsonRawMessageToString(tt.data))
		})
	}
}
