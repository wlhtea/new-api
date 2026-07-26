package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAIVideoErrorWriterUsesNestedSchemaOnlyForExactNewPaths(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantNested bool
	}{
		{name: "create", path: "/v1/videos", wantNested: true},
		{name: "status", path: "/v1/videos/task_public", wantNested: true},
		{name: "content", path: "/v1/videos/task_public/content", wantNested: true},
		{name: "remix remains legacy", path: "/v1/videos/task_public/remix", wantNested: false},
		{name: "old generation route remains legacy", path: "/v1/video/generations/task_public", wantNested: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writer := newTrackingWriter()
			ctx, _ := gin.CreateTestContext(writer)
			ctx.Request = httptest.NewRequest(http.MethodGet, test.path, nil)

			respondTaskErrorForRequest(ctx, &dto.TaskError{
				Code:       "fixture_error",
				Message:    "fixture message",
				StatusCode: http.StatusBadRequest,
			})

			if test.wantNested {
				response := decodeOpenAIVideoError(t, writer.Body.Bytes())
				assert.Equal(t, "fixture message", response.Error.Message)
				assert.Equal(t, "fixture_error", response.Error.Code)
				assert.NotEmpty(t, response.Error.Type)
				return
			}

			var response struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			require.NoError(t, common.Unmarshal(writer.Body.Bytes(), &response))
			assert.Equal(t, "fixture_error", response.Code)
			assert.Equal(t, "fixture message", response.Message)
		})
	}
}
