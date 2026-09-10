package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/alerts"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
)

// TestRuntimeConnectionProjectionFencesSupersededDetach drives the Runtime
// connection boundary through SlotService's authoritative attach/detach fence.
// An old handler ending after its successor attaches must not reopen a resolved
// fault; only the successor's final detach may start the next lifecycle.
func TestRuntimeConnectionProjectionFencesSupersededDetach(t *testing.T) {
	root := t.TempDir()
	config := contract.QuoinConfig{
		Component:                 "quoin",
		PublicOrigin:              "https://quoin.test",
		DataDirectory:             filepath.Join(root, "data"),
		RootKeyFile:               filepath.Join(root, "secrets", "root-key"),
		RuntimeTLSCertificateFile: filepath.Join(root, "secrets", "runtime.crt"),
		RuntimeTLSPrivateKeyFile:  filepath.Join(root, "secrets", "runtime.key"),
		SteleServiceTokenFile:     filepath.Join(root, "secrets", "stele"),
	}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	slots := qruntime.NewService(database.SQL)
	alertService := alerts.NewService(database.SQL)
	service := NewRuntimeControl(slots, "test", "catalog", nil)
	service.PlatformFaults = alerts.NewPlatformFaultReporter(alertService)
	ctx := context.Background()

	slots.AttachStream(qruntime.SlotPlinth, "old", 1)
	if err := service.projectRuntimeConnection(ctx, qruntime.SlotPlinth); err != nil {
		t.Fatal(err)
	}
	assertPlatformFaultState(t, alertService, "Firing", 0)

	// The normal disconnect opens the first lifecycle.
	slots.DetachStream(qruntime.SlotPlinth, "old", 1)
	if err := service.projectRuntimeConnection(ctx, qruntime.SlotPlinth); err != nil {
		t.Fatal(err)
	}
	assertPlatformFaultState(t, alertService, "Firing", 1)

	// A successor resolves it. The old handler's delayed detach is fenced and
	// leaves the successor authoritative, so it cannot revive the fault.
	slots.AttachStream(qruntime.SlotPlinth, "new", 2)
	if err := service.projectRuntimeConnection(ctx, qruntime.SlotPlinth); err != nil {
		t.Fatal(err)
	}
	assertPlatformFaultState(t, alertService, "Resolved", 1)
	slots.DetachStream(qruntime.SlotPlinth, "old", 1)
	if err := service.projectRuntimeConnection(ctx, qruntime.SlotPlinth); err != nil {
		t.Fatal(err)
	}
	assertPlatformFaultState(t, alertService, "Firing", 0)

	// Once the actual owner disconnects, a distinct second lifecycle opens.
	slots.DetachStream(qruntime.SlotPlinth, "new", 2)
	if err := service.projectRuntimeConnection(ctx, qruntime.SlotPlinth); err != nil {
		t.Fatal(err)
	}
	assertPlatformFaultState(t, alertService, "Firing", 1)

	events, err := alertService.ChangesAfter(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events=%+v, want create/resolve/create", events)
	}
	for index, event := range events {
		if event.PlatformFaultID == 0 || event.OccurrenceID != 0 || event.Seq != int64(index+1) {
			t.Fatalf("event[%d]=%+v, platform identities must remain ordered", index, event)
		}
	}
	if events[0].ChangeType != "created" || events[1].ChangeType != "state_changed" || events[2].ChangeType != "created" {
		t.Fatalf("unexpected ordered lifecycle events: %+v", events)
	}
}

func assertPlatformFaultState(t *testing.T, service *alerts.Service, state string, want int) {
	t.Helper()
	snapshot, err := service.AlertSnapshot(context.Background(), state, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != want {
		t.Fatalf("state=%s items=%+v, want %d", state, snapshot.Items, want)
	}
}
