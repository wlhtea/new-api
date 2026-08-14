package relay

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel/opencodego"
	taskseedance "github.com/QuantumNous/new-api/relay/channel/task/seedance"
	"github.com/stretchr/testify/require"
)

func TestSeedDanceTaskAdaptorRegistration(t *testing.T) {
	adaptor := GetTaskAdaptor(constant.TaskPlatform("59"))
	require.IsType(t, &taskseedance.TaskAdaptor{}, adaptor)
}

func TestOpenCodeAPIKeyAdaptorRegistration(t *testing.T) {
	require.IsType(t, &opencodego.Adaptor{}, GetAdaptor(constant.APITypeOpenCodeAPIKey))
}
