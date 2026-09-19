package worker

// Typed tool executor assembly (ADR-0004): supervisor-side tool EXECUTION
// is plugin-ownable, not a core switch. The dispatch table is ASSEMBLED —
// never init-registered — from the same registry assembly every other
// consumer derives from: platform tools register their executors here, and
// every plugin-contributed supervisor_typed tool enters the table through
// its plugin's registered ExecutionBundle (ToolExecutor binding), verified
// contract-exact against the assembled implementation table. Retired
// implementations (受控退役) keep serving frozen historical attempts
// through the same assembled table; they can never enter a new catalog, so
// the binding is history-serving only. Assembly is boot-only and
// duplicate-rejecting and ends frozen, so an unknown or unregistered tool
// fails the tool call explicitly instead of falling through core code.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
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

// registerTypedExecutor binds one tool name to its executing implementation.
// Duplicate names and post-freeze registration are deterministic wiring
// failures (the tool declaration table rejects them equally).
func registerTypedExecutor(toolName string, executor TypedExecutor) error {
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
// connection settings and the grant-backed secret resolver of one typed
// tool execution. It is supplied once at wiring by the host that actually
// owns credential resolution (this supervisor process); the worker child
// never sees credentials. Production uses the Runner itself; tests may pin
// a stub through SetPluginCallHost.
type PluginCallHost interface {
	CallFor(execution *TypedToolContext) (*plugins.Call, error)
}

var pluginCallHost PluginCallHost

// SetPluginCallHost overrides the Call resolver for tests. It must run
// before any attempt executes, never per call.
func SetPluginCallHost(host PluginCallHost) { pluginCallHost = host }

// pluginCallError carries the frozen model-visible failure code of a Call
// resolution failure (e.g. grant_missing): the tool's sealed failure code is
// part of the wire contract and must survive the seam unchanged.
type pluginCallError struct {
	code, detail string
}

func (e *pluginCallError) Error() string { return e.detail }

// pluginCall resolves the frozen plugins.Call of one execution through the
// wired host seam, stamping the executing bundle's plugin identity. Secret
// resolution stays in THIS supervisor process; worker children never
// receive credentials.
func pluginCall(execution *TypedToolContext, bundle plugins.ExecutionBundle) (*plugins.Call, error) {
	host := pluginCallHost
	if host == nil {
		if execution.Runner == nil {
			return nil, fmt.Errorf("supervisor plugin call host is not wired")
		}
		host = execution.Runner
	}
	call, err := host.CallFor(execution)
	if err != nil {
		return nil, err
	}
	if call == nil || len(call.Settings) == 0 {
		return nil, fmt.Errorf("resolved plugin call carries no frozen connection settings")
	}
	call.PluginID = bundle.PluginID
	return call, nil
}

// toolWorkspace is the bounded host seam one spill-capable tool execution
// may use: the attempt's one-shot workspace directory plus the tool_result
// artifact upload path.
func (runner *Runner) toolWorkspace(attemptID, toolCallID int64) *plugins.ToolWorkspace {
	return &plugins.ToolWorkspace{
		Dir:        filepath.Join(runner.Config.WorkspaceRoot, fmt.Sprintf("attempt-%d", attemptID)),
		AttemptID:  attemptID,
		ToolCallID: toolCallID,
		UploadFile: func(ctx context.Context, path, mediaType string) (int64, error) {
			return runner.uploadWorkspaceFileAs(ctx, attemptID, toolCallID, path, mediaType)
		},
	}
}

// AssembleTypedExecutors derives the supervisor typed-tool dispatch table
// from the assembled plugin registry and implementation table — the SAME
// sources every catalog and lookup consumer uses. Platform tools register
// their own executors; every plugin tool resolves its owner descriptor and
// bound ExecutionBundle in the registry; a retired implementation with no
// bound executor keeps its host adapter strictly for frozen historical
// attempts. Assembly freezes the table; calling twice is an error.
func AssembleTypedExecutors(registry *plugins.Registry, table *attempt.ImplementationTable) error {
	typedExecutorRegistry.mu.Lock()
	if typedExecutorRegistry.frozen {
		typedExecutorRegistry.mu.Unlock()
		return fmt.Errorf("typed executor registry already assembled")
	}
	typedExecutorRegistry.mu.Unlock()
	platform := platformTypedExecutors()
	for toolName, executor := range platform {
		if err := registerTypedExecutor(toolName, executor); err != nil {
			return err
		}
	}
	for _, def := range table.Definitions() {
		if def.ExecutionMode != "supervisor_typed" {
			continue
		}
		if _, isPlatform := platform[def.Name]; isPlatform {
			continue
		}
		owner, ok := registry.ToolOwner(def.Name)
		if !ok {
			return fmt.Errorf("tool %s has no registered plugin declaration", def.Name)
		}
		descriptor, _ := registry.Descriptor(owner)
		bundle, bound := registry.Bundle(owner)
		if !bound || bundle.ToolExecutor == nil {
			return fmt.Errorf("plugin %s tool %s has no bound executor in this process", owner, def.Name)
		}
		if err := RegisterPluginExecutor(descriptor, bundle, table); err != nil {
			return err
		}
	}
	freezeTypedExecutors()
	return nil
}

// platformTypedExecutors are the supervisor-executed PLATFORM tools
// (artifact tools): platform-owned implementations registered by this host,
// never by a plugin.
func platformTypedExecutors() map[string]TypedExecutor {
	return map[string]TypedExecutor{
		"artifact_read": executeArtifactRead,
		"artifact_grep": executeArtifactGrep,
	}
}

// RegisterPluginExecutor registers every tool of one plugin binding through
// the plugins.ExecutionBundle.ToolExecutor seam — the single extension path
// for plugin-owned typed tool execution. Registration verifies, per
// declared tool, the full contract against the assembled implementation
// table:
//
//   - the bundle belongs to the descriptor and carries execute_tool with a
//     non-nil ToolExecutor, located in THIS process (plinth_supervisor);
//   - the compiled ToolDef exists and agrees on version, failure mode and
//     execution location (descriptor location == bundle location);
//   - the frozen provider parameter schema matches the implementation's
//     canonical schema byte-for-byte.
//
// A registration that fails any check is a deterministic wiring failure.
func RegisterPluginExecutor(descriptor plugins.Descriptor, bundle plugins.ExecutionBundle, table *attempt.ImplementationTable) error {
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
		def, ok := table.Lookup(declared.Name)
		if !ok {
			return fmt.Errorf("plugin %s tool %s has no compiled implementation", descriptor.ID, declared.Name)
		}
		if declared.ExecutionLocation != bundle.Location {
			return fmt.Errorf("plugin %s tool %s declares location %q, bundle executes at %q", descriptor.ID, declared.Name, declared.ExecutionLocation, bundle.Location)
		}
		if def.Version != declared.Version || def.FailureMode != declared.FailureMode {
			return fmt.Errorf("plugin %s tool %s drifts from the compiled implementation (version/failure mode)", descriptor.ID, declared.Name)
		}
		installed, err := json.Marshal(def.ProviderParameters())
		if err != nil {
			return err
		}
		frozen, err := json.Marshal(declared.Parameters)
		if err != nil {
			return err
		}
		if string(installed) != string(frozen) {
			return fmt.Errorf("plugin %s tool %s parameter schema drifts from the compiled implementation", descriptor.ID, declared.Name)
		}
		tool := declared
		if err := registerTypedExecutor(tool.Name, func(execution *TypedToolContext) error {
			call, err := pluginCall(execution, bundle)
			if err != nil {
				var codeErr *pluginCallError
				if errors.As(err, &codeErr) {
					return execution.Fail(codeErr.code, codeErr.detail)
				}
				return execution.Fail("plugin_call_unavailable", err.Error())
			}
			arguments, err := json.Marshal(execution.Args)
			if err != nil {
				return execution.Fail("invalid_arguments", err.Error())
			}
			request := plugins.ToolRequest{Name: tool.Name, ArgumentsJSON: arguments}
			if execution.Runner != nil {
				request.Workspace = execution.Runner.toolWorkspace(execution.AttemptID, execution.ToolCallID)
			}
			result, err := bundle.ToolExecutor.ExecuteTool(execution.BaseCtx, call, request)
			if err != nil {
				return execution.Fail("plugin_execution_failed", err.Error())
			}
			if result.Success || len(result.Payload) > 0 {
				// The executor's payload IS the tool's sealed result shape
				// (a structured failure payload carries success=false); the
				// commit derives the terminal outcome from it.
				var payload map[string]any
				if err := json.Unmarshal(result.Payload, &payload); err != nil {
					return execution.Fail("invalid_result", err.Error())
				}
				return execution.Succeed(execution.DefaultResultSchemaKind(), payload, result.ArtifactID)
			}
			return execution.Fail(result.ErrorCode, result.ErrorDetail)
		}); err != nil {
			return err
		}
	}
	return nil
}
