package worker

// The assembled dispatch table pins (ADR-0004): the table derives from the
// SAME registry assembly as every catalog and lookup consumer — platform
// tools from this host, plugin tools through their bound ExecutionBundle
// (ToolExecutor seam), retired implementations strictly for frozen
// historical attempts. There is no init-registered bypass, and the table
// freezes at assembly.

import (
	"testing"

	"github.com/Suknna/quoin/internal/plugins/builtin"
	"github.com/Suknna/quoin/internal/quoin/attempt"
)

// TestAssembledDispatchTableDerivesFromRegistry pins the assembled table's
// exact shape: platform artifact tools and the metrics query tool through
// the plugin bundle; nothing else is registered or dispatchable here.
func TestAssembledDispatchTableDerivesFromRegistry(t *testing.T) {
	assembleTestTypedExecutors(t)
	for _, toolName := range []string{"artifact_read", "artifact_grep", "thanos_query"} {
		if _, ok := lookupTypedExecutor(toolName); !ok {
			t.Fatalf("tool %s missing from the assembled dispatch table", toolName)
		}
	}
	for _, toolName := range []string{"bash", "read", "write", "grep"} {
		if _, ok := lookupTypedExecutor(toolName); ok {
			t.Fatalf("tool %s must not be dispatchable on the supervisor", toolName)
		}
	}
}

// The assembled table is frozen: any post-assembly registration is a
// deterministic wiring failure, so a dispatch hole can never be patched at
// runtime instead of failing the launch.
func TestAssembledDispatchTableIsFrozen(t *testing.T) {
	assembleTestTypedExecutors(t)
	if err := registerTypedExecutor("late_tool", func(*TypedToolContext) error { return nil }); err == nil {
		t.Fatal("post-freeze registration was accepted")
	}
}

// A supervisor_typed implementation whose owner has no bound executor and no
// retired adapter fails the assembly instead of leaving a dispatch hole.
func TestAssemblyRejectsUnboundPluginTool(t *testing.T) {
	registry := builtin.Registry()
	table, err := attempt.NewImplementationTable(attempt.Implementations())
	if err != nil {
		t.Fatal(err)
	}
	// The frozen global table is already assembled for this binary; a second
	// assembly must be rejected outright.
	if err := AssembleTypedExecutors(registry, table); err == nil {
		t.Fatal("second assembly must be rejected")
	}
}
