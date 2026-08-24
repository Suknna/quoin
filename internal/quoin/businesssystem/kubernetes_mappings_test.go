package businesssystem

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestKubernetesConnectionMappingLifecycle exercises the public domain seam:
// an Admin command appends an Active mapping, command replay is idempotent,
// retirement is one-way, and a later mapping is a distinct historical row.
func TestKubernetesConnectionMappingLifecycle(t *testing.T) {
	h := newHarness(t)
	h.mustUpload(t, validSystemYAML, 1, "cmd-kubernetes-system")
	connectionID := seedKubernetesConnection(t, h)

	created, err := h.systems.CreateKubernetesConnectionMapping(context.Background(), h.principal, "cmd-kubernetes-map", "payments", connectionID)
	if err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	if created.State != "Active" || created.ConnectionID != connectionID || created.RowVersion != 1 {
		t.Fatalf("created mapping = %#v", created)
	}

	replayed, err := h.systems.CreateKubernetesConnectionMapping(context.Background(), h.principal, "cmd-kubernetes-map", "payments", connectionID)
	if err != nil {
		t.Fatalf("replay mapping command: %v", err)
	}
	if replayed.ID != created.ID {
		t.Fatalf("replay created a second mapping: first=%#v replay=%#v", created, replayed)
	}

	retired, err := h.systems.RetireKubernetesConnectionMapping(context.Background(), h.principal, "cmd-kubernetes-retire", "payments", created.ID, created.RowVersion)
	if err != nil {
		t.Fatalf("retire mapping: %v", err)
	}
	if retired.State != "Retired" || retired.RowVersion != created.RowVersion+1 || retired.RetiredAt == nil || retired.RetiredBy == nil {
		t.Fatalf("retired mapping = %#v", retired)
	}

	remapped, err := h.systems.CreateKubernetesConnectionMapping(context.Background(), h.principal, "cmd-kubernetes-remap", "payments", connectionID)
	if err != nil {
		t.Fatalf("create mapping after retirement: %v", err)
	}
	if remapped.ID == created.ID || remapped.State != "Active" {
		t.Fatalf("remapped = %#v, original = %#v", remapped, created)
	}

	mappings, err := h.systems.ListKubernetesConnectionMappings(context.Background(), "payments")
	if err != nil {
		t.Fatalf("list mappings: %v", err)
	}
	if len(mappings) != 2 || mappings[0].ID != created.ID || mappings[1].ID != remapped.ID {
		t.Fatalf("mapping history = %#v", mappings)
	}
}

func TestKubernetesConnectionMappingRejectsNonKubernetesAndStaleRetire(t *testing.T) {
	h := newHarness(t)
	h.mustUpload(t, validSystemYAML, 1, "cmd-kubernetes-invalid-system")
	nonKubernetes, err := h.db.Exec(`INSERT INTO connections(name,type,enabled,row_version,revalidation_required,created_at) VALUES(?, 'thanos', 0, 1, 0, ?)`, "not-kubernetes", time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	nonKubernetesID, err := nonKubernetes.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.systems.CreateKubernetesConnectionMapping(context.Background(), h.principal, "cmd-kubernetes-non-kubernetes", "payments", nonKubernetesID); err == nil {
		t.Fatal("non-Kubernetes connection mapping was accepted")
	}
	assertRejectedMappingCommand(t, h, "cmd-kubernetes-non-kubernetes", "business_system.kubernetes_connection.create")
	if _, err := h.systems.CreateKubernetesConnectionMapping(context.Background(), h.principal, "cmd-kubernetes-non-kubernetes", "payments", nonKubernetesID); err == nil {
		t.Fatal("replayed non-Kubernetes rejection was accepted")
	}
	connectionID := seedKubernetesConnection(t, h)
	mapping, err := h.systems.CreateKubernetesConnectionMapping(context.Background(), h.principal, "cmd-kubernetes-stale-create", "payments", connectionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.systems.RetireKubernetesConnectionMapping(context.Background(), h.principal, "cmd-kubernetes-stale-retire", "payments", mapping.ID, mapping.RowVersion+1); err == nil {
		t.Fatal("stale row version was accepted")
	}
	assertRejectedMappingCommand(t, h, "cmd-kubernetes-stale-retire", "business_system.kubernetes_connection.retire")
	if _, err := h.systems.RetireKubernetesConnectionMapping(context.Background(), h.principal, "cmd-kubernetes-stale-retire", "payments", mapping.ID, mapping.RowVersion+1); err == nil {
		t.Fatal("replayed stale-row rejection was accepted")
	}
}

func TestKubernetesConnectionMappingJSONUsesDecimalLocators(t *testing.T) {
	createdBy := "9007199254740997"
	retiredBy := "9007199254740999"
	mapping := KubernetesConnectionMapping{ID: 9007199254740993, ConnectionID: 9007199254740995, CreatedBy: createdBy, RetiredBy: &retiredBy}
	body, err := json.Marshal(mapping)
	if err != nil {
		t.Fatal(err)
	}
	for _, locator := range []string{"id", "connectionId", "createdBy", "retiredBy"} {
		if !strings.Contains(string(body), `"`+locator+`":"`) {
			t.Fatalf("%s lost decimal-string representation: %s", locator, body)
		}
	}
}

func TestRetireKubernetesMappingFromAnotherSystemIsNotFound(t *testing.T) {
	h := newHarness(t)
	h.mustUpload(t, validSystemYAML, 1, "cmd-kubernetes-cross-system-payments")
	otherYAML := strings.ReplaceAll(validSystemYAML, "payments", "orders")
	h.mustUpload(t, otherYAML, 1, "cmd-kubernetes-cross-system-orders")
	connectionID := seedKubernetesConnection(t, h)
	mapping, err := h.systems.CreateKubernetesConnectionMapping(context.Background(), h.principal, "cmd-kubernetes-cross-system-create", "payments", connectionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.systems.RetireKubernetesConnectionMapping(context.Background(), h.principal, "cmd-kubernetes-cross-system-retire", "orders", mapping.ID, mapping.RowVersion); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-system retire error=%v, want ErrNotFound", err)
	}
	assertRejectedMappingCommand(t, h, "cmd-kubernetes-cross-system-retire", "business_system.kubernetes_connection.retire")
}

func assertRejectedMappingCommand(t *testing.T, h *harness, commandID, commandType string) {
	t.Helper()
	var outcome, storedType string
	if err := h.db.QueryRow(`SELECT outcome,command_type FROM client_commands WHERE principal_id=? AND client_command_id=?`, h.principal, commandID).Scan(&outcome, &storedType); err != nil {
		t.Fatalf("read rejection command ledger: %v", err)
	}
	if outcome != "rejected_known" || storedType != commandType {
		t.Fatalf("rejection ledger = outcome=%q type=%q", outcome, storedType)
	}
	var audits int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE actor_id=? AND action=? AND outcome='rejected'`, h.principal, commandType).Scan(&audits); err != nil {
		t.Fatalf("read rejection audit: %v", err)
	}
	if audits != 1 {
		t.Fatalf("rejection audit count = %d, want 1", audits)
	}
}

func seedKubernetesConnection(t *testing.T, h *harness) int64 {
	t.Helper()
	result, err := h.db.Exec(`INSERT INTO connections(name,type,enabled,row_version,revalidation_required,created_at) VALUES(?, 'kubernetes', 1, 1, 0, ?)`, "payments-kubernetes", time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("seed Kubernetes connection: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("read seeded Kubernetes connection ID: %v", err)
	}
	return id
}
