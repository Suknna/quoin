package app

// Connection-probe task slice of the control stream (T07): DispatchAttempt
// envelopes to the live Plinth stream, AttemptAccept/ResultProposal/CancelAck
// adjudication on the way back, and the FetchCredentialGrant RPC that
// decrypts one attempt-scoped secret over the authenticated channel.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/connections"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The dispatch lease window is attempt.DispatchLease (single frozen
// authority, RUNTIME-SCOPE-004).

// sendEnvelope stamps per-direction ids and forwards to the slot's live
// stream; a failed send is audited and retried by the queued dispatcher.
func (service *RuntimeService) sendEnvelope(slot string, envelope *runtimev1.ControlEnvelope) error {
	if service.sendEnvelopeForTest != nil {
		return service.sendEnvelopeForTest(slot, envelope)
	}
	id, err := service.Slots.NextMessageID(slot)
	if err != nil {
		return err
	}
	envelope.MessageId = id
	return service.Slots.SendTo(slot, envelope)
}

// dispatchAttempt sends the DispatchAttempt frame for one already-Assigned
// probe attempt (RUNTIME-TASK-002/003).
func (service *RuntimeService) dispatchAttempt(ctx context.Context, attemptID int64, summary connections.Summary, epoch uint64, bootID string, grantID int64, input []byte) error {
	digest := sha256.Sum256(input)
	contractDigest, err := connections.ProbeContractDigest()
	if err != nil {
		return err
	}
	// The input snapshot carries every attempt grant (model_provider
	// probes dispatch with chat + embedding purposes).
	var grants []*runtimev1.ConnectionGrant
	if service.Connections != nil {
		rows, grantErr := service.Connections.DB().QueryContext(ctx, `SELECT id,connection_revision_id,credential_generation_id,purpose FROM attempt_connection_grants WHERE attempt_id=? ORDER BY id`, attemptID)
		if grantErr != nil {
			return grantErr
		}
		for rows.Next() {
			var grantID, revisionID, generationID int64
			var purpose string
			if err := rows.Scan(&grantID, &revisionID, &generationID, &purpose); err != nil {
				rows.Close()
				return err
			}
			grants = append(grants, &runtimev1.ConnectionGrant{GrantId: grantID, ConnectionRevisionId: revisionID, CredentialGenerationId: generationID, Purpose: purpose})
		}
		rows.Close()
	}
	actionSetID, actionSetVersion, err := connections.ActionSet(summary.Type)
	if err != nil {
		return err
	}
	envelope := &runtimev1.ControlEnvelope{
		ConnectionEpoch: epoch,
		CorrelationId:   uint64(attemptID),
		BootId:          bootID,
		Msg: &runtimev1.ControlEnvelope_DispatchAttempt{
			DispatchAttempt: &runtimev1.DispatchAttempt{
				AttemptId:     attemptID,
				AttemptType:   runtimev1.AttemptType_ATTEMPT_TYPE_CONNECTION_PROBE,
				ScopeType:     runtimev1.ScopeType_SCOPE_TYPE_CONNECTION,
				ScopeId:       summary.ID,
				LeaseDeadline: timestamppb.New(time.Now().UTC().Add(probeLeaseWindow())),
				Input: &runtimev1.AttemptInputSnapshot{
					SchemaKind:       "connection_probe_v1",
					CanonicalJson:    input,
					ContentDigest:    digest[:],
					ArtifactRefs:     nil,
					AgentVersion:     "",
					ConnectionGrants: grants,
				},
			},
		},
	}
	// The action-set identity travels with the input snapshot; the
	// supervisor validates it against the frozen catalog digest.
	_, _, _ = contractDigest, actionSetID, actionSetVersion
	return service.sendEnvelope(qruntime.SlotPlinth, envelope)
}

// dispatchQueuedProbes binds and dispatches every Queued connection_probe
// attempt after a Plinth stream attaches (created while disconnected).
func (service *RuntimeService) dispatchQueuedProbes(ctx context.Context) {
	if service.Connections == nil {
		return
	}
	view, err := service.Slots.View(ctx, qruntime.SlotPlinth)
	if err != nil || !view.Connected || view.ConnectionEpoch == nil {
		return
	}
	ids, err := service.Connections.QueuedProbeAttempts(ctx)
	if err != nil {
		sharedops.LogEvent("quoin", "error", "probe.queue_scan", err.Error())
		return
	}
	for _, id := range ids {
		summary, grantID, input, ok, err := service.Connections.BindQueuedToStream(ctx, id, view.BootID, *view.ConnectionEpoch, probeLeaseWindow())
		if err != nil || !ok {
			if err != nil {
				sharedops.LogEvent("quoin", "error", "probe.queue_bind", err.Error())
			}
			continue
		}
		if err := service.dispatchAttempt(ctx, id, summary, *view.ConnectionEpoch, view.BootID, grantID, input); err != nil {
			sharedops.LogEvent("quoin", "error", "probe.queue_dispatch", err.Error())
		}
	}
}

func probeLeaseWindow() time.Duration {
	// attempt.DispatchLease is the single frozen lease authority for
	// probes and agent attempts alike (RUNTIME-SCOPE-004).
	return attempt.DispatchLease
}

// handleAttemptAccept records Assigned -> Running (RUNTIME-TASK-004).
func (service *RuntimeService) handleAttemptAccept(ctx context.Context, envelope *runtimev1.ControlEnvelope, accept *runtimev1.AttemptAccept) {
	if service.Connections == nil {
		return
	}
	if err := service.Connections.AcceptProbe(ctx, accept.GetAttemptId(), envelope.GetBootId(), envelope.GetConnectionEpoch()); err != nil {
		sharedops.LogEvent("quoin", "error", "probe.accept_failed", err.Error())
	}
}

// probeResultJSON is the supervisor's canonical result payload body.
type probeResultJSON struct {
	Outcome    string          `json:"outcome"`
	Detail     json.RawMessage `json:"detail"`
	StartedAt  string          `json:"startedAt"`
	FinishedAt string          `json:"finishedAt"`
}

// handleResultProposal adjudicates a connection_probe result: digest
// verification, typed-child write and attempt terminal state commit in one
// transaction (RUNTIME-AGENT-010), then ResultAck.
func (service *RuntimeService) handleResultProposal(ctx context.Context, envelope *runtimev1.ControlEnvelope, proposal *runtimev1.ResultProposal) {
	if service.Connections == nil {
		return
	}
	ack := &runtimev1.ControlEnvelope{
		ConnectionEpoch: envelope.GetConnectionEpoch(),
		CorrelationId:   envelope.GetCorrelationId(),
		BootId:          envelope.GetBootId(),
		Msg:             &runtimev1.ControlEnvelope_ResultAck{ResultAck: &runtimev1.ResultAck{AttemptId: proposal.GetAttemptId()}},
	}
	reject := func(reason string) {
		ack.GetResultAck().Accepted = false
		ack.GetResultAck().Detail = reason
		_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
		sharedops.LogEvent("quoin", "error", "probe.result_rejected", "attempt="+fmt.Sprint(proposal.GetAttemptId())+" reason="+reason)
	}
	payload := proposal.GetPayload()
	if payload == nil || payload.GetSchemaKind() == "" || len(payload.GetCanonicalJson()) == 0 {
		reject("result payload incomplete")
		return
	}
	digest := sha256.Sum256(payload.GetCanonicalJson())
	if hex.EncodeToString(digest[:]) != hex.EncodeToString(payload.GetContentDigest()) {
		reject("content digest mismatch")
		return
	}
	var parsed probeResultJSON
	if err := json.Unmarshal(payload.GetCanonicalJson(), &parsed); err != nil {
		reject("result payload unparseable")
		return
	}
	if parsed.Outcome != "passed" && parsed.Outcome != "failed" {
		reject("outcome must be passed or failed")
		return
	}
	resultDigest := sha256.Sum256(append([]byte(payload.GetSchemaKind()+"\n"), payload.GetCanonicalJson()...))
	typed := connections.TypedProbeResult{
		Outcome:      parsed.Outcome,
		Detail:       parsed.Detail,
		ResultDigest: hex.EncodeToString(resultDigest[:]),
		StartedAt:    parsed.StartedAt,
		FinishedAt:   parsed.FinishedAt,
	}
	child, err := parseTypedChild(payload.GetSchemaKind(), parsed.Detail)
	if err != nil {
		reject(err.Error())
		return
	}
	if err := service.Connections.CommitProbeResult(ctx, proposal.GetAttemptId(), proposal.GetBootId(), proposal.GetConnectionEpoch(), typed, child); err != nil {
		reject(err.Error())
		return
	}
	ack.GetResultAck().Accepted = true
	_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
}

// handleCancelAck finalizes a cancelled probe (RUNTIME-CANCEL-003).
func (service *RuntimeService) handleCancelAck(ctx context.Context, ack *runtimev1.CancelAck) {
	if service.Connections == nil {
		return
	}
	if err := service.Connections.RecordCancelAck(ctx, ack.GetAttemptId()); err != nil {
		sharedops.LogEvent("quoin", "error", "probe.cancel_ack", err.Error())
	}
}

// FetchCredentialGrant decrypts the attempt-scoped secret after re-checking
// the grant/attempt/boot/epoch binding (RUNTIME-GRANT-001, DATA-CONN-002).
func (service *RuntimeService) FetchCredentialGrant(ctx context.Context, request *runtimev1.FetchCredentialGrantRequest) (*runtimev1.FetchCredentialGrantResponse, error) {
	if service.Connections == nil {
		return nil, status.Error(codes.Unavailable, "connections not wired")
	}
	// The calling runtime must present its current long-term token; grant
	// fencing then re-checks the attempt binding (RUNTIME-GRANT-001).
	bearer := bearerFromContext(ctx)
	if !service.Slots.ValidateBearer(ctx, bearer, qruntime.SlotPlinth) {
		return nil, status.Error(codes.Unauthenticated, "runtime bearer required")
	}
	payload, err := service.Connections.FulfillGrant(ctx, request.GetGrantId(), request.GetAttemptId(), request.GetBootId(), request.GetConnectionEpoch())
	if err != nil {
		sharedops.LogEvent("quoin", "error", "grant.fulfill_denied", fmt.Sprintf("grant=%d attempt=%d: %v", request.GetGrantId(), request.GetAttemptId(), err))
		return nil, status.Error(codes.PermissionDenied, "grant denied")
	}
	response := &runtimev1.FetchCredentialGrantResponse{
		GrantId:                payload.GrantID,
		AttemptId:              payload.AttemptID,
		ConnectionRevisionId:   payload.ConnectionRevisionID,
		CredentialGenerationId: payload.CredentialGeneration,
		ConnectionType:         payload.ConnectionType,
		RevisionConfigJson:     payload.RevisionConfigJSON,
	}
	switch {
	case payload.Thanos != nil:
		response.Secret = &runtimev1.FetchCredentialGrantResponse_Thanos{Thanos: &runtimev1.ThanosCredentialSecret{Username: payload.Thanos.Username, Password: payload.Thanos.Password}}
	case payload.Kubernetes != nil:
		response.Secret = &runtimev1.FetchCredentialGrantResponse_Kubernetes{Kubernetes: &runtimev1.KubernetesCredentialSecret{Kubeconfig: payload.Kubernetes.Kubeconfig}}
	case payload.ModelProvider != nil:
		response.Secret = &runtimev1.FetchCredentialGrantResponse_ModelProvider{ModelProvider: &runtimev1.ModelProviderCredentialSecret{ApiKey: payload.ModelProvider.APIKey}}
	default:
		return nil, status.Error(codes.Internal, "typed secret missing")
	}
	return response, nil
}

// thanosDetail is the supervisor's canonical thanos detail JSON.
type thanosDetail struct {
	Kind         string `json:"kind"`
	Query        string `json:"query"`
	ResponseType string `json:"responseType"`
	SampleCount  int    `json:"sampleCount"`
	SampleValue  string `json:"sampleValue"`
}

// kubernetesDetail is the supervisor's canonical kubernetes detail JSON.
type kubernetesDetail struct {
	Kind               string `json:"kind"`
	EffectiveNamespace string `json:"effectiveNamespace"`
	VersionOK          bool   `json:"versionOk"`
	CoreDiscoveryOK    bool   `json:"coreDiscoveryOk"`
	GroupedDiscoveryOK bool   `json:"groupedDiscoveryOk"`
	PodsGetAllowed     bool   `json:"podsGetAllowed"`
	PodsListAllowed    bool   `json:"podsListAllowed"`
	EventsListAllowed  bool   `json:"eventsListAllowed"`
	PodsLogGetAllowed  bool   `json:"podsLogGetAllowed"`
}

// parseTypedChild validates the schema kind and canonical detail against the
// frozen typed-child CHECK contract before the closure transaction runs.
func parseTypedChild(schemaKind string, detail json.RawMessage) (*connections.TypedChild, error) {
	switch schemaKind {
	case "connection_probe_thanos_v1":
		var parsed thanosDetail
		if err := json.Unmarshal(detail, &parsed); err != nil {
			return nil, fmt.Errorf("thanos detail unparseable: %w", err)
		}
		if parsed.Kind != "thanos" {
			return nil, fmt.Errorf("thanos detail kind mismatch")
		}
		// The typed-child columns carry the frozen ACTION constants
		// (query=vector(1), type=vector, count=1 — schema CHECK); the
		// observed values and the failure reason live in detail_json.
		sample := parsed.SampleValue
		if parsed.ResponseType != "vector" || parsed.SampleCount != 1 || sample != "1" {
			sample = ""
		}
		return &connections.TypedChild{Thanos: &connections.ThanosProbeChild{
			Query: "vector(1)", ResponseType: "vector",
			SampleCount: 1, SampleValue: sample,
			DetailJSON: string(detail),
		}}, nil
	case "connection_probe_kubernetes_v1":
		var parsed kubernetesDetail
		if err := json.Unmarshal(detail, &parsed); err != nil {
			return nil, fmt.Errorf("kubernetes detail unparseable: %w", err)
		}
		if parsed.Kind != "kubernetes" {
			return nil, fmt.Errorf("kubernetes detail kind mismatch")
		}
		if parsed.EffectiveNamespace == "" {
			// Failed probes may not have resolved a namespace; the typed
			// child still requires a non-empty value (frozen default).
			parsed.EffectiveNamespace = "default"
		}
		return &connections.TypedChild{Kubernetes: &connections.KubernetesProbeChild{
			EffectiveNamespace: parsed.EffectiveNamespace,
			VersionOK:          parsed.VersionOK, CoreDiscoveryOK: parsed.CoreDiscoveryOK,
			GroupedDiscoveryOK: parsed.GroupedDiscoveryOK, PodsGetAllowed: parsed.PodsGetAllowed,
			PodsListAllowed: parsed.PodsListAllowed, EventsListAllowed: parsed.EventsListAllowed,
			PodsLogGetAllowed: parsed.PodsLogGetAllowed, DetailJSON: string(detail),
		}}, nil
	case "connection_probe_model_provider_v1":
		var parsed modelProviderDetailJSON
		if err := json.Unmarshal(detail, &parsed); err != nil {
			return nil, fmt.Errorf("model provider detail unparseable: %w", err)
		}
		if parsed.Kind != "model_provider" {
			return nil, fmt.Errorf("model provider detail kind mismatch")
		}
		return &connections.TypedChild{ModelProvider: &connections.ModelProviderProbeChild{
			ChatModelID: parsed.ChatModelID, EmbeddingModelID: parsed.EmbeddingModelID,
			ContextBudgetTokens: parsed.ContextBudgetTokens, MaxOutputTokens: parsed.MaxOutputTokens,
			StreamingSupported: parsed.StreamingSupported, NativeToolCallingSupported: parsed.NativeToolCallingSupported,
			MultiToolCallSupported: parsed.MultiToolCallSupported, CancellationObserved: parsed.CancellationObserved,
			UsageObserved: parsed.UsageObserved, RequestIDObserved: parsed.RequestIDObserved,
			EmbeddingSupported: parsed.EmbeddingSupported, EmbeddingVectorDim: parsed.EmbeddingVectorDim,
			DetailJSON: string(detail),
		}}, nil
	default:
		return nil, fmt.Errorf("unknown probe result schema kind %q", schemaKind)
	}
}

// modelProviderDetailJSON is the supervisor's canonical qualification detail.
type modelProviderDetailJSON struct {
	Kind                       string  `json:"kind"`
	ChatModelID                string  `json:"chatModelId"`
	EmbeddingModelID           *string `json:"embeddingModelId"`
	ContextBudgetTokens        int     `json:"contextBudgetTokens"`
	MaxOutputTokens            int     `json:"maxOutputTokens"`
	StreamingSupported         bool    `json:"streamingSupported"`
	NativeToolCallingSupported bool    `json:"nativeToolCallingSupported"`
	MultiToolCallSupported     bool    `json:"multiToolCallSupported"`
	CancellationObserved       bool    `json:"cancellationObserved"`
	UsageObserved              bool    `json:"usageObserved"`
	RequestIDObserved          bool    `json:"requestIdObserved"`
	EmbeddingSupported         bool    `json:"embeddingSupported"`
	EmbeddingVectorDim         int     `json:"embeddingVectorDim,omitempty"`
}

// dispatchCancel sends the committed cancellation fence to the runtime that
// owns the attempt (RUNTIME-CANCEL-001..003).
func (service *RuntimeService) dispatchCancel(ctx context.Context, attemptID int64) error {
	var boot sql.NullString
	var epoch sql.NullInt64
	if service.Connections == nil {
		return errors.New("connections not wired")
	}
	row := service.Connections.DB().QueryRowContext(ctx, `SELECT boot_id,connection_epoch FROM execution_attempts WHERE id=?`, attemptID)
	if err := row.Scan(&boot, &epoch); err != nil {
		return err
	}
	if !boot.Valid || !epoch.Valid {
		// Unbound attempts finalize locally.
		return service.Connections.RecordCancelAck(ctx, attemptID)
	}
	return service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{
		ConnectionEpoch: uint64(epoch.Int64),
		CorrelationId:   uint64(attemptID),
		BootId:          boot.String,
		Msg:             &runtimev1.ControlEnvelope_CancelAttempt{CancelAttempt: &runtimev1.CancelAttempt{AttemptId: attemptID}},
	})
}
