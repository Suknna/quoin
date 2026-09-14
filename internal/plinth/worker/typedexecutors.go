package worker

// Typed tool executor registry (ADR-0004): supervisor-side tool EXECUTION is
// plugin-ownable, not a core switch. Platform tools (artifact/thanos) register
// their executors here at init; a plugin package registers its own executor
// for its declared tool name during process wiring. Registration is boot-only
// and duplicate-rejecting; Run freezes the registry, so an unknown or
// unregistered tool fails the tool call explicitly instead of falling through
// core code.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/attempt"
)

// TypedToolContext carries one typed tool execution and the sealed-commit
// surface. Tool-level failures go through Fail (the return_to_model shape);
// only transport-level breakdowns return a non-nil error without sealing.
type TypedToolContext struct {
	BaseCtx               context.Context
	Runner                *Runner
	Writer                *FrameWriter
	AttemptID, ToolCallID int64
	ToolName              string
	Args                  map[string]any
}

// Fail seals a failed typed tool with the return_to_model failure shape.
func (c *TypedToolContext) Fail(errorCode, errorDetail string) error {
	return c.Runner.failTypedTool(c.BaseCtx, c.Writer, c.AttemptID, c.ToolCallID, c.ToolName, errorCode, errorDetail)
}

// Succeed seals the committed payload under the tool's frozen result schema
// kind and relays it with the optional long-body artifact.
func (c *TypedToolContext) Succeed(schemaKind string, payload map[string]any, artifactID int64) error {
	return c.Runner.commitTypedTool(c.BaseCtx, c.Writer, c.AttemptID, c.ToolCallID, schemaKind, payload, artifactID)
}

// DefaultResultSchemaKind is the frozen result schema kind derived from the
// tool name (matching the ToolDef table).
func (c *TypedToolContext) DefaultResultSchemaKind() string {
	return typedResultSchemaKind(c.ToolName)
}

// TypedExecutor executes one authorized typed tool call.
type TypedExecutor func(execution *TypedToolContext) error

var typedExecutorRegistry struct {
	mu        sync.Mutex
	executors map[string]TypedExecutor
	frozen    bool
}

// RegisterTypedExecutor binds one tool name to its executing implementation.
// Duplicate names and post-freeze registration are deterministic wiring
// failures (the tool declaration table rejects them equally).
func RegisterTypedExecutor(toolName string, executor TypedExecutor) error {
	typedExecutorRegistry.mu.Lock()
	defer typedExecutorRegistry.mu.Unlock()
	if typedExecutorRegistry.frozen {
		return fmt.Errorf("typed executor registry frozen: tool %s rejected", toolName)
	}
	if toolName == "" || executor == nil {
		return fmt.Errorf("typed executor registration invalid for %q", toolName)
	}
	if _, exists := typedExecutorRegistry.executors[toolName]; exists {
		return fmt.Errorf("typed executor %s is already registered", toolName)
	}
	if typedExecutorRegistry.executors == nil {
		typedExecutorRegistry.executors = map[string]TypedExecutor{}
	}
	typedExecutorRegistry.executors[toolName] = executor
	return nil
}

// freezeTypedExecutors locks the registry against further registration.
func freezeTypedExecutors() {
	typedExecutorRegistry.mu.Lock()
	defer typedExecutorRegistry.mu.Unlock()
	typedExecutorRegistry.frozen = true
}

func lookupTypedExecutor(toolName string) (TypedExecutor, bool) {
	typedExecutorRegistry.mu.Lock()
	defer typedExecutorRegistry.mu.Unlock()
	executor, ok := typedExecutorRegistry.executors[toolName]
	return executor, ok
}

// PluginCallHost is the supervisor-side seam that resolves the frozen
// plugins.Call of one typed tool execution: frozen instance settings plus a
// grant-backed secret resolver. It is supplied once at wiring by the host
// that actually owns credential resolution (this supervisor process); the
// worker child never sees credentials.
type PluginCallHost interface {
	CallFor(execution *TypedToolContext) (*plugins.Call, error)
}

var pluginCallHost PluginCallHost

// SetPluginCallHost wires the supervisor Call resolver before any attempt
// runs. It must be called during process wiring, not per call.
func SetPluginCallHost(host PluginCallHost) { pluginCallHost = host }

// RegisterPluginExecutor is the SINGLE extension path for plugin-owned typed
// tool execution: the source of truth is the plugin's
// plugins.ExecutionBundle.ToolExecutor binding — not a parallel mechanism.
// Registration verifies, per declared tool, the full contract against the
// compiled implementation table:
//
//   - the bundle belongs to the descriptor and carries execute_tool with a
//     non-nil ToolExecutor, located in THIS process (plinth_supervisor);
//   - the compiled ToolDef exists and agrees on version, failure mode and
//     execution location (descriptor location == bundle location);
//   - the frozen provider parameter schema matches the implementation's
//     canonical schema byte-for-byte.
//
// A registration that fails any check is a deterministic wiring failure.
type PluginExecutorAdapter struct {
	Descriptor plugins.Descriptor
	Bundle     plugins.ExecutionBundle
}

// RegisterPluginExecutor validates and registers every tool of one plugin
// binding.
func RegisterPluginExecutor(descriptor plugins.Descriptor, bundle plugins.ExecutionBundle) error {
	if bundle.PluginID != descriptor.ID {
		return fmt.Errorf("plugin executor bundle %s does not belong to descriptor %s", bundle.PluginID, descriptor.ID)
	}
	if bundle.Location != plugins.LocationPlinthSupervisor {
		return fmt.Errorf("plugin %s bundle executes at %q; this registry only hosts %q", descriptor.ID, bundle.Location, plugins.LocationPlinthSupervisor)
	}
	var executeTool bool
	for _, capability := range bundle.Capabilities {
		if capability == plugins.CapabilityExecuteTool {
			executeTool = true
		}
	}
	if !executeTool || bundle.ToolExecutor == nil {
		return fmt.Errorf("plugin %s bundle lacks an execute_tool ToolExecutor", descriptor.ID)
	}
	for _, declared := range descriptor.Tools {
		def, ok := attempt.CompiledToolDefinition(declared.Name)
		if !ok {
			return fmt.Errorf("plugin %s tool %s has no compiled implementation", descriptor.ID, declared.Name)
		}
		if declared.ExecutionLocation != bundle.Location {
			return fmt.Errorf("plugin %s tool %s declares location %q, bundle executes at %q", descriptor.ID, declared.Name, declared.ExecutionLocation, bundle.Location)
		}
		if def.Version != declared.Version || def.FailureMode != declared.FailureMode {
			return fmt.Errorf("plugin %s tool %s drifts from the compiled implementation (version/failure mode)", descriptor.ID, declared.Name)
		}
		installed, err := json.Marshal(attempt.CanonicalToolParameters(def))
		if err != nil {
			return err
		}
		frozen, err := json.Marshal(declared.Parameters)
		if err != nil {
			return err
		}
		if !bytes.Equal(installed, frozen) {
			return fmt.Errorf("plugin %s tool %s parameter schema drifts from the compiled implementation", descriptor.ID, declared.Name)
		}
		tool := declared
		if err := RegisterTypedExecutor(tool.Name, func(execution *TypedToolContext) error {
			call, err := pluginCall(execution, bundle)
			if err != nil {
				return execution.Fail("plugin_call_unavailable", err.Error())
			}
			arguments, err := json.Marshal(execution.Args)
			if err != nil {
				return execution.Fail("invalid_arguments", err.Error())
			}
			result, err := bundle.ToolExecutor.ExecuteTool(execution.BaseCtx, call, plugins.ToolRequest{
				Name: tool.Name, ArgumentsJSON: arguments,
			})
			if err != nil {
				return execution.Fail("plugin_execution_failed", err.Error())
			}
			if result.Success {
				return execution.Succeed(execution.DefaultResultSchemaKind(), map[string]any{
					"success": true, "payload": json.RawMessage(result.Payload),
				}, 0)
			}
			return execution.Fail(result.ErrorCode, result.ErrorDetail)
		}); err != nil {
			return err
		}
	}
	return nil
}

// pluginCall resolves the frozen plugins.Call through the wired host seam.
// Secret resolution stays in THIS supervisor process; worker children never
// receive credentials.
func pluginCall(execution *TypedToolContext, bundle plugins.ExecutionBundle) (*plugins.Call, error) {
	if pluginCallHost == nil {
		return nil, fmt.Errorf("supervisor plugin call host is not wired")
	}
	call, err := pluginCallHost.CallFor(execution)
	if err != nil {
		return nil, err
	}
	if call.PluginID != bundle.PluginID {
		return nil, fmt.Errorf("resolved call targets plugin %q, bundle executes %q", call.PluginID, bundle.PluginID)
	}
	return call, nil
}

func init() {
	mustRegisterTypedExecutor("artifact_read", executeArtifactRead)
	mustRegisterTypedExecutor("artifact_grep", executeArtifactGrep)
	mustRegisterTypedExecutor("thanos_query", executeThanosQueryTyped)
}

// mustRegisterTypedExecutor panics on a platform built-in registration
// failure: built-ins are compile-time facts pinned by tests, and a collision
// is a programming error that must fail the process, not degrade silently.
func mustRegisterTypedExecutor(toolName string, executor TypedExecutor) {
	if err := RegisterTypedExecutor(toolName, executor); err != nil {
		panic("platform typed executor registration failed: " + err.Error())
	}
}
