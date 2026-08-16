package helper

import (
	"errors"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveModelMappingDeterministicCases(t *testing.T) {
	tests := []struct {
		name    string
		origin  string
		mapping string
		want    ModelMappingResolution
		wantErr error
	}{
		{
			name:    "no configuration",
			origin:  " glm-5.2 ",
			mapping: "",
			want: ModelMappingResolution{
				OriginModel: "glm-5.2",
				FinalModel:  "glm-5.2",
				ChainLength: 1,
			},
		},
		{
			name:    "empty object",
			origin:  "glm-5.2",
			mapping: " {} ",
			want: ModelMappingResolution{
				OriginModel: "glm-5.2",
				FinalModel:  "glm-5.2",
				ChainLength: 1,
			},
		},
		{
			name:    "direct mapping",
			origin:  "alias",
			mapping: `{"alias":"glm-5.3"}`,
			want: ModelMappingResolution{
				OriginModel: "alias",
				FinalModel:  "glm-5.3",
				Mapped:      true,
				Configured:  true,
				ChainLength: 2,
			},
		},
		{
			name:    "case and whitespace normalized chain",
			origin:  " Alias ",
			mapping: `{" alias ":" Middle ","middle":" GLM-5.3 "}`,
			want: ModelMappingResolution{
				OriginModel: "Alias",
				FinalModel:  "GLM-5.3",
				Mapped:      true,
				Configured:  true,
				ChainLength: 3,
			},
		},
		{
			name:    "origin self mapping is a no-op",
			origin:  "glm-5.2",
			mapping: `{"glm-5.2":"glm-5.2"}`,
			want: ModelMappingResolution{
				OriginModel: "glm-5.2",
				FinalModel:  "glm-5.2",
				Configured:  true,
				ChainLength: 1,
			},
		},
		{
			name:    "chain ending in self mapping",
			origin:  "alias",
			mapping: `{"alias":"glm-5.3","glm-5.3":"glm-5.3"}`,
			want: ModelMappingResolution{
				OriginModel: "alias",
				FinalModel:  "glm-5.3",
				Mapped:      true,
				Configured:  true,
				ChainLength: 2,
			},
		},
		{
			name:    "provider prefix is not implicitly stripped",
			origin:  "provider/alias",
			mapping: `{"alias":"glm-5.3"}`,
			want: ModelMappingResolution{
				OriginModel: "provider/alias",
				FinalModel:  "provider/alias",
				Configured:  true,
				ChainLength: 1,
			},
		},
		{
			name:    "provider prefix can be explicitly mapped",
			origin:  "provider/alias",
			mapping: `{"provider/alias":"provider/glm-5.3"}`,
			want: ModelMappingResolution{
				OriginModel: "provider/alias",
				FinalModel:  "provider/glm-5.3",
				Mapped:      true,
				Configured:  true,
				ChainLength: 2,
			},
		},
		{name: "empty origin", origin: "  ", mapping: `{}`, wantErr: ErrModelMappingEmptyOrigin},
		{name: "malformed JSON", origin: "alias", mapping: `{"alias":`, wantErr: ErrModelMappingMalformed},
		{name: "non-object JSON", origin: "alias", mapping: `[]`, wantErr: ErrModelMappingMalformed},
		{name: "null JSON", origin: "alias", mapping: `null`, wantErr: ErrModelMappingMalformed},
		{name: "trailing JSON", origin: "alias", mapping: `{"alias":"glm-5.3"}{}`, wantErr: ErrModelMappingMalformed},
		{name: "empty source", origin: "alias", mapping: `{" ":"glm-5.3"}`, wantErr: ErrModelMappingInvalid},
		{name: "empty target", origin: "alias", mapping: `{"alias":" "}`, wantErr: ErrModelMappingInvalid},
		{name: "exact duplicate source", origin: "alias", mapping: `{"alias":"glm-5.2","alias":"glm-5.3"}`, wantErr: ErrModelMappingInvalid},
		{name: "decoded equivalent duplicate source", origin: "model", mapping: `{"model":"glm-5.2","\u006dodel":"glm-5.3"}`, wantErr: ErrModelMappingInvalid},
		{name: "duplicate unrelated source", origin: "alias", mapping: `{"other":"glm-5.2","other":"glm-5.3"}`, wantErr: ErrModelMappingInvalid},
		{name: "normalized duplicate source", origin: "alias", mapping: `{"Alias":"glm-5.2"," alias ":"glm-5.3"}`, wantErr: ErrModelMappingInvalid},
		{name: "direct cycle", origin: "a", mapping: `{"a":"b","b":"a"}`, wantErr: ErrModelMappingCycle},
		{name: "normalized cycle", origin: " A ", mapping: `{"a":" B ","b":"a"}`, wantErr: ErrModelMappingCycle},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveModelMapping(test.origin, test.mapping)
			if test.wantErr != nil {
				require.Error(t, err)
				assert.True(t, errors.Is(err, test.wantErr), "error %v should match %v", err, test.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestModelMappedHelperUsesPureResolution(t *testing.T) {
	c, _ := gin.CreateTestContext(nil)
	c.Set("model_mapping", `{"alias":"middle","middle":"glm-5.3"}`)
	request := &dto.GeneralOpenAIRequest{Model: "alias"}
	info := &relaycommon.RelayInfo{
		OriginModelName: "alias",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "stale-attempt-model",
		},
	}

	require.NoError(t, ModelMappedHelper(c, info, request))
	assert.True(t, info.IsModelMapped)
	assert.Equal(t, "glm-5.3", info.UpstreamModelName)
	assert.Equal(t, "glm-5.3", request.Model)
}
