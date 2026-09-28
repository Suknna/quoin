package stele

// Relay-side settings delivery (ADR-0014 story 2): the snapshot carries each
// source instance's non-secret settings, Credential pins exactly the matched
// instance's document, and a snapshot refresh swaps both the document and the
// version — while a queued event keeps the version it was accepted under.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/plugins"
	"google.golang.org/grpc"
)

type settingsSnapshotClient struct {
	runtimev1.SteleRelayClient
	snapshot *runtimev1.GetCredentialSnapshotResponse
}

// digestOf derives the snapshot digest of a wire bearer whose 32 raw bytes
// are the secret padded with zeros (SEC-REVEAL-001: digest = SHA-256(raw)).
func digestOf(secret string) []byte {
	var raw [32]byte
	copy(raw[:], secret)
	digest := sha256.Sum256(raw[:])
	return digest[:]
}

func encodeBearer(secret string) string {
	var raw [32]byte
	copy(raw[:], secret)
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

func (client *settingsSnapshotClient) GetCredentialSnapshot(context.Context, *runtimev1.GetCredentialSnapshotRequest, ...grpc.CallOption) (*runtimev1.GetCredentialSnapshotResponse, error) {
	return client.snapshot, nil
}

func settingsSnapshot(version uint64) *runtimev1.GetCredentialSnapshotResponse {
	return &runtimev1.GetCredentialSnapshotResponse{
		SnapshotVersion:     version,
		ContractFingerprint: contract.ProtoAuthorityFingerprint,
		Sources: []*runtimev1.AlertSourceSnapshot{
			{
				SourceId: 11, SourceKey: "src-eu", Protocol: "synthetic-hook", Enabled: true,
				SettingsJson: []byte(`{"ignoredAlertnames":["noise"]}`),
				Credentials:  []*runtimev1.CredentialDigestEntry{{CredentialId: 21, Digest: digestOf("eu-credential")}},
			},
			{
				SourceId: 12, SourceKey: "src-us", Protocol: "synthetic-hook", Enabled: true,
				SettingsJson: []byte(`{"ignoredAlertnames":["drill"]}`),
				Credentials:  []*runtimev1.CredentialDigestEntry{{CredentialId: 22, Digest: digestOf("us-credential")}},
			},
		},
	}
}

// TestRelayCredentialPinsMatchedInstanceSettings proves two instances of the
// same kind resolve their OWN settings and snapshot version, and an unknown
// bearer resolves nothing.
func TestRelayCredentialPinsMatchedInstanceSettings(t *testing.T) {
	relay := &Relay{client: &settingsSnapshotClient{snapshot: settingsSnapshot(7)}, manifest: plugins.Default().InboundManifestFingerprint()}
	relay.mu.Lock()
	relay.snapshot = settingsSnapshot(7)
	relay.ready = true
	relay.mu.Unlock()

	eu, ok := relay.Credential(encodeBearer("eu-credential"), "synthetic-hook")
	if !ok || eu.SourceID != 11 || eu.CredentialID != 21 || eu.SnapshotVersion != 7 {
		t.Fatalf("eu match = %+v ok=%v", eu, ok)
	}
	if string(eu.Settings) != `{"ignoredAlertnames":["noise"]}` {
		t.Fatalf("eu settings = %q", eu.Settings)
	}
	us, ok := relay.Credential(encodeBearer("us-credential"), "synthetic-hook")
	if !ok || us.SourceID != 12 || string(us.Settings) != `{"ignoredAlertnames":["drill"]}` || us.SnapshotVersion != 7 {
		t.Fatalf("us match = %+v ok=%v", us, ok)
	}
	if _, ok := relay.Credential(encodeBearer("nope"), "synthetic-hook"); ok {
		t.Fatal("unknown bearer resolved")
	}
}

// TestRelaySettingsRefreshSwapsDocumentAndVersion proves a settings update on
// Quoin invalidates the cached document only through the next snapshot pull:
// after refresh the same bearer resolves the NEW settings and version — the
// cache reload contract Stele's 5s loop performs.
func TestRelaySettingsRefreshSwapsDocumentAndVersion(t *testing.T) {
	client := &settingsSnapshotClient{snapshot: settingsSnapshot(7)}
	relay := &Relay{client: client, manifest: plugins.Default().InboundManifestFingerprint()}
	relay.mu.Lock()
	relay.snapshot = settingsSnapshot(7)
	relay.ready = true
	relay.mu.Unlock()

	// Quoin committed a settings update: the next pull carries document v2.
	client.snapshot = &runtimev1.GetCredentialSnapshotResponse{
		SnapshotVersion:     9,
		ContractFingerprint: contract.ProtoAuthorityFingerprint,
		Sources: []*runtimev1.AlertSourceSnapshot{{
			SourceId: 11, SourceKey: "src-eu", Protocol: "synthetic-hook", Enabled: true,
			SettingsJson: []byte(`{"ignoredAlertnames":["noise","drill"]}`),
			Credentials:  []*runtimev1.CredentialDigestEntry{{CredentialId: 21, Digest: digestOf("eu-credential")}},
		}},
	}
	relay.refresh(context.Background())
	if !relay.Ready() {
		t.Fatal("refresh lost readiness")
	}
	match, ok := relay.Credential(encodeBearer("eu-credential"), "synthetic-hook")
	if !ok || match.SnapshotVersion != 9 || string(match.Settings) != `{"ignoredAlertnames":["noise","drill"]}` {
		t.Fatalf("post-refresh match = %+v ok=%v", match, ok)
	}
}

// TestQueuedEventKeepsAcceptedSnapshotVersion proves the freeze-at-accept
// semantics: the webhook captures the snapshot version (settings revision
// provenance) at accept time into the queued event, and a later settings
// change does not rewrite already-accepted events.
func TestQueuedEventKeepsAcceptedSnapshotVersion(t *testing.T) {
	_, digest := testBearer()
	source := &stubEventSource{}
	queue := openTestQueue(t)
	lookup := &stubLookup{ready: true, digest: digest, settings: []byte(`{"v":1}`)}
	webhook := NewWebhook(queue, lookup, stubSourceRegistry{sources: map[string]plugins.EventSource{"alertmanager": source}}, NewMetrics())
	server := httptest.NewServer(webhook.Handler())
	defer server.Close()

	response, err := http.DefaultClient.Do(bearerRequest(t, http.MethodPost, server.URL+"/webhook/alertmanager", validPayload))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}

	// Settings change on Quoin; Stele refreshes to the new revision.
	lookup.mu.Lock()
	lookup.settings = []byte(`{"v":2}`)
	lookup.mu.Unlock()

	// The already-queued event still records the version it was accepted
	// under — provenance stays attributable to exactly one settings revision.
	batch, err := queue.FetchDueBatch(context.Background(), 10, time.Now())
	if err != nil || len(batch) != 1 {
		t.Fatalf("due batch = %v err=%v", batch, err)
	}
	if batch[0].CredentialSnapshotVersion != 3 || batch[0].SourceID != 7 {
		t.Fatalf("queued event provenance = (source %d, snapshot %d), want (7, 3)", batch[0].SourceID, batch[0].CredentialSnapshotVersion)
	}

	// A NEW request after the refresh parses under the new document.
	second := &stubEventSource{}
	secondServer := httptest.NewServer(NewWebhook(queue, lookup, stubSourceRegistry{sources: map[string]plugins.EventSource{"alertmanager": second}}, NewMetrics()).Handler())
	defer secondServer.Close()
	secondResponse, err := http.DefaultClient.Do(bearerRequest(t, http.MethodPost, secondServer.URL+"/webhook/alertmanager", validPayload))
	if err != nil {
		t.Fatal(err)
	}
	secondResponse.Body.Close()
	if string(second.settingsSeen) != `{"v":2}` {
		t.Fatalf("post-refresh settings seen by plugin = %q, want the new revision", second.settingsSeen)
	}
}
