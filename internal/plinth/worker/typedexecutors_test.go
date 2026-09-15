package worker

// The test-binary dispatch assembly: the same assembly the production serve
// wiring runs (shared builtin registry + metrics execute_tool bundles +
// implementation table), executed exactly once for the process.

import (
	"sync"
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/plugins/builtin"
	"github.com/Suknna/quoin/internal/quoin/attempt"
)

var (
	assembleTestOnce sync.Once
	assembleTestErr  error
)

func assembleTestTypedExecutors(t *testing.T) {
	t.Helper()
	assembleTestOnce.Do(func() {
		registry := builtin.Registry()
		executors := ExecutionBundles()
		for _, pluginID := range []string{"prometheus", "thanos"} {
			bundle := plugins.ExecutionBundle{
				PluginID: pluginID, Location: plugins.LocationPlinthSupervisor,
				Capabilities: []plugins.Capability{plugins.CapabilityExecuteTool},
				ToolExecutor: executors[pluginID],
			}
			if err := registry.RegisterBundle(bundle); err != nil {
				assembleTestErr = err
				return
			}
		}
		table, err := attempt.NewImplementationTable(attempt.Implementations())
		if err != nil {
			assembleTestErr = err
			return
		}
		assembleTestErr = AssembleTypedExecutors(registry, table)
	})
	if assembleTestErr != nil {
		t.Fatalf("assemble typed executors: %v", assembleTestErr)
	}
}
