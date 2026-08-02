package relay

import (
	"net/http"

	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func handleNon2xxResponse(c *gin.Context, adaptor channel.Adaptor, resp *http.Response, info *relaycommon.RelayInfo) *types.NewAPIError {
	if providerErr, handled := channel.TryHandleNon2xxResponse(c, adaptor, resp, info); handled {
		return providerErr
	}
	return service.RelayErrorHandler(c.Request.Context(), resp, false)
}
