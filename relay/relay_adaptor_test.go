package relay

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	taskseedance "github.com/QuantumNous/new-api/relay/channel/task/seedance"
	"github.com/stretchr/testify/require"
)

func TestSeedDanceTaskAdaptorRegistration(t *testing.T) {
	adaptor := GetTaskAdaptor(constant.TaskPlatform("59"))
	require.IsType(t, &taskseedance.TaskAdaptor{}, adaptor)
}
