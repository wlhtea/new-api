package seedance

import (
	"net/http/httptest"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestEstimateBillingUsesNormalizedDurationAndResolution(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(normalizedRequestContextKey, &NormalizedRequest{
		Duration:   10,
		Resolution: "1080P",
	})
	a := &TaskAdaptor{}

	ratios := a.EstimateBilling(c, &relaycommon.RelayInfo{})

	assert.Equal(t, map[string]float64{
		"seconds":    10,
		"resolution": 2.25,
	}, ratios)
}

func TestEstimateBillingRequiresCachedNormalizedRequest(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	assert.Nil(t, (&TaskAdaptor{}).EstimateBilling(c, &relaycommon.RelayInfo{}))
}
