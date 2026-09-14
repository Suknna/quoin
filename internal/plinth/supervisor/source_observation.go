// Source observation execution slice (ADR-0004 接入即有界观测): the Plinth
// supervisor accepts a dispatched observation_run child, fetches its
// attempt-scoped credential grant, and executes bounded discovery through the
// plugin Discoverer execution binding — never by a scope-specific platform
// query hard-coded in the dispatch switch. The metrics adapter below is the
// single PromQL-driving implementation shared by discovery (and, as the
// inspection template binding lands, collection) for the prometheus/thanos
// plugins; the descriptor's DiscoverObject owns the query, identity labels
// and per-pass budget, and the frozen input is validated against it so a
// declaration can never silently diverge from execution.
package supervisor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	plinthconnections "github.com/Suknna/quoin/internal/plinth/connections"
	"github.com/Suknna/quoin/internal/plinth/runtime"
	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"google.golang.org/grpc/metadata"
)

// sourceObservationExecutionSchemaKind is the frozen input schema the Quoin
// control plane seals for every observation_run child (the snapshot closure
// trigger pins it to the scope).
const sourceObservationExecutionSchemaKind = "source_observation_execution_v1"

// sourceObservationResultSchemaKind is the sealed result payload identity the
// control plane adjudicates.
const sourceObservationResultSchemaKind = "source_observation_result_v1"

// sourceObservationInput is the frozen dispatch input.
type sourceObservationInput struct {
	SchemaKind       string   `json:"schemaKind"`
	AttemptID        int64    `json:"attemptId"`
	ObservationRunID int64    `json:"observationRunId"`
	PluginID         string   `json:"pluginId"`
	ObjectType       string   `json:"objectType"`
	Query            string   `json:"query"`
	IdentityLabels   []string `json:"identityLabels"`
	Limit            int      `json:"limit"`
	GrantID          int64    `json:"grantId"`
}

// observedTarget is one discovered source object of a successful pass.
type observedTarget struct {
	Identity    map[string]string `json:"identity"`
	Labels      map[string]string `json:"labels"`
	DisplayName string            `json:"displayName,omitempty"`
}

// observationRegistry is the process plugin catalog. Descriptors come from
// the attempt package's compiled authority; this host binds the real
// Discoverer implementations for the plugins it can execute. Lazy and
// process-lifetime: bindings are compile-time facts, not runtime switches.
var (
	observationRegistryOnce sync.Once
	observationRegistry     *plugins.Registry
	// descriptorSource is the authority the catalog builds from; a test may
	// pin an isolated catalog. Production leaves the default untouched.
	descriptorSource = attempt.BuiltinDescriptors
)

// pluginRegistry builds and freezes the supervisor-side registry once. A
// registration failure is a build/launch bug and panics: a host that cannot
// honestly describe and bind its capabilities must not serve attempts.
func (supervisor *Supervisor) pluginRegistry() *plugins.Registry {
	observationRegistryOnce.Do(func() {
		registry := plugins.NewRegistry()
		for _, descriptor := range descriptorSource() {
			if err := registry.RegisterDescriptor(descriptor); err != nil {
				panic(fmt.Sprintf("plugin descriptor %s failed registration: %v", descriptor.ID, err))
			}
		}
		// Bind exactly the capabilities each metrics plugin's descriptor
		// declares, so the bundle can never advertise an execution the
		// catalog does not own and stays correct as the catalog gains
		// capabilities (discovery today, deterministic collection alongside).
		for _, pluginID := range []string{plugins.PrometheusID, plugins.ThanosID} {
			descriptor, ok := registry.Descriptor(pluginID)
			if !ok {
				continue
			}
			bundle := plugins.ExecutionBundle{PluginID: pluginID, Location: plugins.LocationPlinthSupervisor}
			for _, capability := range descriptor.Capabilities {
				switch capability {
				case plugins.CapabilityDiscover:
					bundle.Capabilities = append(bundle.Capabilities, capability)
					bundle.Discoverer = &metricsDiscoverer{}
				case plugins.CapabilityCollect:
					bundle.Capabilities = append(bundle.Capabilities, capability)
					bundle.Collector = &metricsCollector{}
				}
			}
			if len(bundle.Capabilities) == 0 {
				continue
			}
			if err := registry.RegisterBundle(bundle); err != nil {
				panic(fmt.Sprintf("plugin bundle %s failed binding: %v", pluginID, err))
			}
		}
		observationRegistry = registry
	})
	return observationRegistry
}

// metricsDiscoverer is the one PromQL-driving discovery adapter for the
// Prometheus-compatible plugins. It executes the descriptor-declared query
// through the existing controlled metrics transport and derives each
// object's canonical identity from the descriptor-declared label set.
type metricsDiscoverer struct{}

// Discover executes one bounded pass. The Call carries the frozen connection
// settings; the request selects the descriptor-declared object type and the
// budget. Incompleteness is reported, never collapsed into success.
func (d *metricsDiscoverer) Discover(ctx context.Context, call *plugins.Call, request plugins.DiscoverRequest) (*plugins.DiscoverResult, error) {
	observationRegistryOnce.Do(func() {})
	if observationRegistry == nil {
		return nil, fmt.Errorf("plugin registry is not initialized")
	}
	descriptor, ok := observationRegistry.Descriptor(call.PluginID)
	if !ok {
		return nil, fmt.Errorf("plugin %q is not registered", call.PluginID)
	}
	var declared *plugins.DiscoverObject
	for i := range descriptor.DiscoverObjects {
		if descriptor.DiscoverObjects[i].ObjectType == request.ObjectType {
			declared = &descriptor.DiscoverObjects[i]
			break
		}
	}
	if declared == nil {
		return nil, fmt.Errorf("plugin %q does not declare discovery object %q", call.PluginID, request.ObjectType)
	}
	config, err := metricsSettings(call)
	if err != nil {
		return nil, err
	}
	secret, err := metricsSecret(ctx, call)
	if err != nil {
		// 凭据解析失败 fail closed：绝不以匿名/空凭据继续观测。
		return nil, err
	}
	limit := declared.Limit
	if request.Limit > 0 && request.Limit < limit {
		limit = request.Limit
	}
	raw, warnings, err := plinthconnections.RunPromQL(ctx, config, secret, "instant", declared.Query, nil, nil)
	if err != nil {
		return nil, err
	}
	var response struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("invalid Prometheus response: %w", err)
	}
	objects := make([]plugins.DiscoveredObject, 0, len(response.Data.Result))
	for i, item := range response.Data.Result {
		if len(objects) == limit {
			// The budget is a hard fact of the pass: truncation is reported as
			// incompleteness so the control plane never projects a whole scope
			// from a truncated response.
			warnings = append(warnings, fmt.Sprintf("discovery truncated at %d objects", limit))
			break
		}
		identity := make(map[string]string, len(declared.IdentityLabels))
		for _, label := range declared.IdentityLabels {
			value, present := item.Metric[label]
			if !present || value == "" {
				return nil, fmt.Errorf("series %d lacks identity label %q", i, label)
			}
			identity[label] = value
		}
		objects = append(objects, plugins.DiscoveredObject{
			ObjectType:        declared.ObjectType,
			CanonicalIdentity: canonicalTargetIdentity(identity),
			DisplayName:       item.Metric["job"] + "/" + item.Metric["instance"],
		})
	}
	result := &plugins.DiscoverResult{Objects: objects, Incomplete: false}
	// Upstream warnings (partial federation, storage hints) mark the pass
	// incomplete even when every series arrived: absence is only meaningful
	// from a complete observation.
	if len(warnings) != 0 {
		result.Incomplete = true
	}
	return result, nil
}

// canonicalTargetIdentity is the source identity string for one target. The
// control plane's identity_key encoding stays the equality authority; this
// canonical identity carries the same label facts.
func canonicalTargetIdentity(identity map[string]string) string {
	parts := make([]string, 0, len(identity))
	for _, name := range []string{"job", "instance"} {
		if value, ok := identity[name]; ok {
			parts = append(parts, name+"="+value)
		}
	}
	return strings.Join(parts, ",")
}

// registryDescriptor looks up one descriptor over the frozen registry,
// building it on first use so validation paths never race initialization.
func (supervisor *Supervisor) registryDescriptor(id string) (plugins.Descriptor, bool) {
	return supervisor.pluginRegistry().Descriptor(id)
}

// runSourceObservation executes one dispatched observation child: accept,
// validate the frozen input against the bound plugin descriptor, resolve the
// credential grant, discover through the plugin binding, and seal the typed
// result proposal.
func (supervisor *Supervisor) runSourceObservation(parent context.Context, sink *runtime.FrameSink, client runtimev1.RuntimeControlClient, dispatch *runtimev1.DispatchAttempt, binding runtime.DispatchBinding, stopTask func(int64) bool) {
	attemptID := dispatch.GetAttemptId()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	supervisor.Channel.RegisterTask(attemptID, cancel)
	defer stopTask(attemptID)
	if err := sink.Send(&runtimev1.ControlEnvelope{CorrelationId: uint64(attemptID), Msg: &runtimev1.ControlEnvelope_AttemptAccept{AttemptAccept: &runtimev1.AttemptAccept{AttemptId: attemptID}}}); err != nil {
		return
	}
	var input sourceObservationInput
	if dispatch.GetInput() == nil || json.Unmarshal(dispatch.GetInput().GetCanonicalJson(), &input) != nil {
		supervisor.proposeSourceObservation(sink, attemptID, binding, input, "error", nil, []string{"invalid source observation input"}, "query_failed")
		return
	}
	if err := supervisor.validateSourceObservationInput(dispatch, attemptID, input); err != nil {
		supervisor.proposeSourceObservation(sink, attemptID, binding, input, "error", nil, []string{err.Error()}, "query_failed")
		return
	}
	grant, ok := supervisor.primaryGrant(dispatch.GetInput(), "config_thanos_query")
	if !ok || grant.GetGrantId() != input.GrantID {
		supervisor.proposeSourceObservation(sink, attemptID, binding, input, "error", nil, []string{"missing source observation grant"}, "query_failed")
		return
	}
	bearer, err := supervisor.Channel.BearerToken()
	if err != nil {
		supervisor.proposeSourceObservation(sink, attemptID, binding, input, "error", nil, []string{"runtime credential unavailable"}, "query_failed")
		return
	}
	grantCtx, grantCancel := context.WithTimeout(ctx, 15*time.Second)
	payload, err := client.FetchCredentialGrant(metadata.NewOutgoingContext(grantCtx, metadata.Pairs("authorization", "Bearer "+bearer)), &runtimev1.FetchCredentialGrantRequest{GrantId: grant.GetGrantId(), AttemptId: attemptID, BootId: binding.BootID, ConnectionEpoch: binding.Epoch})
	grantCancel()
	if err != nil || payload.GetThanos() == nil {
		supervisor.proposeSourceObservation(sink, attemptID, binding, input, "error", nil, []string{"credential grant unavailable"}, "query_failed")
		return
	}
	bundle, bound := supervisor.pluginRegistry().Bundle(input.PluginID)
	if !bound || bundle.Discoverer == nil {
		supervisor.proposeSourceObservation(sink, attemptID, binding, input, "error", nil, []string{fmt.Sprintf("plugin %q has no bound discoverer in this process", input.PluginID)}, "plugin_unavailable")
		return
	}
	// The Call freezes the connection's non-secret revision config and hands
	// the grant-resolved material to the Discoverer through the contract's
	// secret boundary — the adapter never sees raw transport envelopes.
	call, callErr := newMetricsCall(supervisor.pluginRegistry(), input.PluginID, json.RawMessage(payload.GetRevisionConfigJson()),
		payload.GetThanos().GetUsername(), payload.GetThanos().GetPassword(), payload.GetThanos().GetBearerToken())
	if callErr != nil {
		supervisor.proposeSourceObservation(sink, attemptID, binding, input, "error", nil, []string{callErr.Error()}, "query_failed")
		return
	}
	discoverCtx, discoverCancel := context.WithTimeout(ctx, 60*time.Second)
	result, discoverErr := bundle.Discoverer.Discover(discoverCtx, call, plugins.DiscoverRequest{ObjectType: input.ObjectType, Limit: input.Limit})
	discoverCancel()
	if discoverErr != nil {
		supervisor.proposeSourceObservation(sink, attemptID, binding, input, "error", nil, []string{discoverErr.Error()}, "query_failed")
		return
	}
	targets := make([]observedTarget, 0, len(result.Objects))
	for _, object := range result.Objects {
		identity := map[string]string{}
		for _, part := range strings.Split(object.CanonicalIdentity, ",") {
			if name, value, found := strings.Cut(part, "="); found {
				identity[name] = value
			}
		}
		// The plugin contract v1 carries exactly the declared identity label
		// set; those observed facts are both the identity and the label
		// projection. Nothing beyond them is invented here.
		labels := make(map[string]string, len(identity))
		for name, value := range identity {
			labels[name] = value
		}
		targets = append(targets, observedTarget{Identity: identity, Labels: labels, DisplayName: object.DisplayName})
	}
	if result.Incomplete {
		warnings := []string{"discovery pass incomplete"}
		supervisor.proposeSourceObservation(sink, attemptID, binding, input, "gap", targets, warnings, "partial_response")
		return
	}
	supervisor.proposeSourceObservation(sink, attemptID, binding, input, "success", targets, nil, "")
}

// validateSourceObservationInput enforces the frozen envelope identity and
// the descriptor agreement: a declared object type, query, identity label set
// and budget that no longer match the bound plugin's catalog fail the attempt
// instead of executing re-interpreted inputs.
func (supervisor *Supervisor) validateSourceObservationInput(dispatch *runtimev1.DispatchAttempt, attemptID int64, input sourceObservationInput) error {
	if input.SchemaKind != sourceObservationExecutionSchemaKind || input.AttemptID != attemptID || input.ObservationRunID != dispatch.GetScopeId() {
		return fmt.Errorf("source observation input identity does not match the dispatch")
	}
	descriptor, ok := supervisor.registryDescriptor(input.PluginID)
	if !ok {
		return fmt.Errorf("plugin %q is not registered in this process", input.PluginID)
	}
	for _, object := range descriptor.DiscoverObjects {
		if object.ObjectType != input.ObjectType {
			continue
		}
		if input.Query != object.Query {
			return fmt.Errorf("frozen discovery query %q does not match plugin %q catalog", input.Query, input.PluginID)
		}
		if strings.Join(input.IdentityLabels, ",") != strings.Join(object.IdentityLabels, ",") {
			return fmt.Errorf("frozen identity labels %v do not match plugin %q catalog", input.IdentityLabels, input.PluginID)
		}
		if input.Limit != object.Limit {
			return fmt.Errorf("frozen discovery limit %d does not match plugin %q catalog", input.Limit, input.PluginID)
		}
		return nil
	}
	return fmt.Errorf("plugin %q does not declare discovery object %q", input.PluginID, input.ObjectType)
}

// proposeSourceObservation seals one canonical source_observation_result_v1
// proposal. Non-success outcomes carry empty objects by contract: the control
// plane's frozen completeness rule never projects from a partial or failed
// pass, so partial facts stay unprojected (warnings and the gap reason carry
// the honesty) and a shape-invalid proposal can never wedge a Run in
// Running.
func (supervisor *Supervisor) proposeSourceObservation(sink *runtime.FrameSink, attemptID int64, binding runtime.DispatchBinding, input sourceObservationInput, outcome string, targets []observedTarget, warnings []string, gapReason string) {
	canonical, err := marshalSourceObservationProposal(attemptID, input, outcome, targets, warnings, gapReason)
	if err != nil {
		return
	}
	digest := sha256.Sum256(canonical)
	_ = sink.Send(&runtimev1.ControlEnvelope{CorrelationId: uint64(attemptID), Msg: &runtimev1.ControlEnvelope_ResultProposal{ResultProposal: &runtimev1.ResultProposal{AttemptId: attemptID, BootId: binding.BootID, ConnectionEpoch: binding.Epoch, Outcome: outcomeFor(map[string]string{"success": "passed", "gap": "failed", "error": "failed"}[outcome]), Payload: &runtimev1.ResultPayload{SchemaKind: sourceObservationResultSchemaKind, CanonicalJson: canonical, ContentDigest: digest[:]}}}})
}

// marshalSourceObservationProposal is the single definition of the
// source_observation_result_v1 canonical shape. The control plane
// (observation.CommitProposal) validates exactly this envelope, so the
// supervisor and the adjudicator cannot drift.
func marshalSourceObservationProposal(attemptID int64, input sourceObservationInput, outcome string, targets []observedTarget, warnings []string, gapReason string) ([]byte, error) {
	if outcome != "success" {
		// Only a complete success may propose objects: the control plane's
		// frozen envelope rejects non-success objects, so partial facts are
		// dropped here (warnings and the gap reason carry the honesty) and a
		// wedge-a-Run-in-Running proposal can never be generated.
		targets = nil
	}
	if targets == nil {
		targets = []observedTarget{}
	}
	if warnings == nil {
		warnings = []string{}
	}
	var gap any
	if gapReason != "" {
		gap = gapReason
	}
	errs := []string{}
	if outcome == "error" {
		errs = warnings
		warnings = []string{}
	}
	return json.Marshal(map[string]any{
		"schemaKind":       sourceObservationResultSchemaKind,
		"attemptId":        attemptID,
		"observationRunId": input.ObservationRunID,
		"objectType":       input.ObjectType,
		"outcome":          outcome,
		"observedAt":       time.Now().UTC().Format(time.RFC3339Nano),
		"objects":          targets,
		"warnings":         warnings,
		"errors":           errs,
		"gapReason":        gap,
	})
}

// secretResolverFunc adapts a function to the plugin secret boundary.
type secretResolverFunc func(ref string) ([]byte, error)

func (f secretResolverFunc) Resolve(ctx context.Context, ref string) ([]byte, error) { return f(ref) }
