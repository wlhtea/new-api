package seedance

import (
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
)

func (a *TaskAdaptor) EstimateBilling(
	c *gin.Context,
	_ *relaycommon.RelayInfo,
) map[string]float64 {
	normalized, err := getNormalizedRequest(c)
	if err != nil {
		return nil
	}
	resolution, ok := resolutionRatios[normalized.Resolution]
	if !ok {
		return nil
	}
	return map[string]float64{
		"seconds":    float64(normalized.Duration),
		"resolution": resolution,
	}
}
