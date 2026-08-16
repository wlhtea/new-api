package opencodego

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestRequestCleanupReleasesOpenCodeGoInFlightExactlyOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	const channelID = 982341
	const workspaceUID = "cleanup-workspace"

	adaptor := &Adaptor{}
	adaptor.Init(nil)
	baseline := service.OpenCodeGoWorkspaceInFlight(channelID, workspaceUID)
	adaptor.acquireInFlight(c, channelID, workspaceUID)
	assert.Equal(t, baseline+1, service.OpenCodeGoWorkspaceInFlight(channelID, workspaceUID))

	common.RunRequestCleanups(c)
	adaptor.releaseInFlight()
	common.RunRequestCleanups(c)

	assert.Equal(t, baseline, service.OpenCodeGoWorkspaceInFlight(channelID, workspaceUID))
}
