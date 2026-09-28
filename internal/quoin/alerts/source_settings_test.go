package alerts

// Source instance settings (ADR-0014 story 2): authoritative non-secret
// settings_json with schema validation, row-version fenced updates, global
// monotone settings_version and snapshot invalidation. Two instances of the
// same kind must stay independent, and settings writes must never move the
// delivery identity.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

func TestSourceSettingsSnapshotAdvancesBeyondAllCredentialGenerations(t *testing.T) {
	service, database, done := newTestService(t)
	defer done()
	registry := settingsRegistry(t)
	if err := registry.Register(plugins.Plugin{
		ID: "plain-plugin", Version: "1", DefaultEnabled: true,
		EventSource: plainSettingsSource{}, EventTypes: []string{"alerts.batch"},
		AlertNormalizer: settingsStubNormalizer{}, AlertIdentity: plugins.AlertIdentityExternal,
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.UseSourceRegistry(registry); err != nil {
		t.Fatal(err)
	}
	ctx := adminCommandContext(t, context.Background())
	created, _, err := service.CreateSource(ctx, "settings-create-churn", "settings-churn", "settings-source", []byte(`{"site":"eu-west"}`), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	// Other instances consume the same credential-ID sequence without
	// advancing this source's settings counter.
	for generation := byte(1); generation <= 8; generation++ {
		digest := make([]byte, 32)
		digest[0] = generation
		if _, _, err := service.CreateSource(ctx, fmt.Sprintf("plain-create-%d", generation), fmt.Sprintf("plain-%d", generation), "plain-source", nil, digest); err != nil {
			t.Fatalf("create plain source %d: %v", generation, err)
		}
	}
	before, _, err := service.CredentialSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	updated, _, err := service.SetSourceSettings(ctx, "settings-update-churn", "settings-churn", []byte(`{"site":"ap-east"}`), 1)
	if err != nil {
		t.Fatal(err)
	}
	after, snapshot, err := service.CredentialSnapshot(ctx)
	if err != nil || after <= before {
		t.Fatalf("snapshot version failed to advance over %d credential generations: before=%d after=%d err=%v", 8, before, after, err)
	}
	var storedVersion int64
	if err := database.SQL.QueryRow(`SELECT settings_version FROM alert_sources WHERE id=?`, created.SourceID).Scan(&storedVersion); err != nil {
		t.Fatal(err)
	}
	if storedVersion != 2 || string(updated.Settings) != `{"site":"ap-east"}` || len(snapshot) != 9 || string(snapshot[0].Settings) != `{"site":"ap-east"}` {
		t.Fatalf("settings update/snapshot drifted: updated=%+v snapshot=%+v version=%d", updated, snapshot, after)
	}
	// A credential ID may catch up with (or equal) a settings revision. The
	// shared snapshot epoch must still advance for both sorts of mutation.
	rotatedDigest := make([]byte, 32)
	rotatedDigest[0] = 99
	if _, _, err := service.RotateCredential(ctx, "settings-rotate-after-update", "settings-churn", rotatedDigest); err != nil {
		t.Fatal(err)
	}
	afterRotate, _, err := service.CredentialSnapshot(ctx)
	if err != nil || afterRotate <= after {
		t.Fatalf("credential rotation did not advance snapshot after settings update: %d -> %d, err=%v", after, afterRotate, err)
	}
	if _, _, err := service.SetSourceSettings(ctx, "settings-update-after-rotate", "settings-churn", []byte(`{"site":"eu-central"}`), updated.RowVersion); err != nil {
		t.Fatal(err)
	}
	afterSettings, _, err := service.CredentialSnapshot(ctx)
	if err != nil || afterSettings <= afterRotate {
		t.Fatalf("settings update did not advance snapshot after credential rotation: %d -> %d, err=%v", afterRotate, afterSettings, err)
	}
}

// settingsPlugin is an inline compiled plugin declaring a closed event-source
// settings schema, so the service-level tests exercise real schema validation
// without importing the acceptance plugin package.
func settingsRegistry(t *testing.T) *plugins.Registry {
	t.Helper()
	registry := plugins.NewRegistry()
	type normalizer struct{}
	if err := registry.Register(plugins.Plugin{
		ID: "settings-plugin", Version: "1", DefaultEnabled: true,
		EventSource: settingsStubSource{},
		EventTypes:  []string{"alerts.batch"},
		EventSourceConfigSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"site":   map[string]any{"type": "string", "maxLength": 100},
				"filter": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			"required": []any{"site"},
		},
		AlertNormalizer: settingsStubNormalizer{},
		AlertIdentity:   plugins.AlertIdentityExternal,
	}); err != nil {
		t.Fatalf("register settings plugin: %v", err)
	}
	_ = normalizer{}
	return registry
}

type settingsStubSource struct{}

type plainSettingsSource struct{}

func (plainSettingsSource) Kind() string { return "plain-source" }

func (plainSettingsSource) VerifyAndParse(_ context.Context, _ plugins.InboundRequest) ([]plugins.Event, error) {
	return nil, nil
}

func (settingsStubSource) Kind() string { return "settings-source" }

func (settingsStubSource) VerifyAndParse(_ context.Context, _ plugins.InboundRequest) ([]plugins.Event, error) {
	return nil, nil
}

type settingsStubNormalizer struct{}

func (settingsStubNormalizer) NormalizeAlert(payload []byte) ([]plugins.NormalizedAlert, error) {
	return nil, nil
}

func TestCreateSourcePersistsValidatedSettings(t *testing.T) {
	service, database, done := newTestService(t)
	defer done()
	if err := service.UseSourceRegistry(settingsRegistry(t)); err != nil {
		t.Fatal(err)
	}
	ctx := adminCommandContext(t, context.Background())
	digest := make([]byte, 32)
	result, replayed, err := service.CreateSource(ctx, "settings-create-0001", "settings-src", "settings-source", []byte(`{"site": "eu-west"}`), digest)
	if err != nil || replayed {
		t.Fatalf("create = (%+v, %v, %v)", result, replayed, err)
	}
	var stored string
	if err := database.SQL.QueryRow(`SELECT settings_json FROM alert_sources WHERE source_key='settings-src'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	// Canonical bytes: whitespace variants collapse onto one document.
	if stored != `{"site":"eu-west"}` {
		t.Fatalf("stored settings = %q", stored)
	}
	var version int64
	if err := database.SQL.QueryRow(`SELECT settings_version FROM alert_sources WHERE source_key='settings-src'`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("first settings write must set settings_version=1, got %d err=%v", version, err)
	}
	// The settings write advanced the snapshot version beyond the credential
	// id: MAX(credential ids, settings_version) picked the settings counter.
	snapshotVersion, sources, err := service.CredentialSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshotVersion < uint64(version) {
		t.Fatalf("snapshot version %d does not span settings_version %d", snapshotVersion, version)
	}
	if len(sources) != 1 || string(sources[0].Settings) != `{"site":"eu-west"}` {
		t.Fatalf("snapshot source settings = %+v", sources)
	}
	if _, err := database.SQL.Exec(`UPDATE alert_source_settings_history SET settings_json='{}' WHERE source_id=?`, result.SourceID); err == nil {
		t.Fatal("accepted source settings history must be immutable")
	}
	if _, err := database.SQL.Exec(`DELETE FROM alert_source_settings_history WHERE source_id=?`, result.SourceID); err == nil {
		t.Fatal("accepted source settings history must not be deleted")
	}
}

func TestCreateSourceRejectsInvalidSettings(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	if err := service.UseSourceRegistry(settingsRegistry(t)); err != nil {
		t.Fatal(err)
	}
	ctx := adminCommandContext(t, context.Background())
	for name, settings := range map[string]string{
		"unknown field":    `{"site":"eu","extra":1}`,
		"missing required": `{"filter":["a"]}`,
		"wrong type":       `{"site":7}`,
		"not json":         `{`,
		"bound overflow":   `{"site":"` + string(make([]byte, plugins.MaxEventSourceSettingsBytes)) + `"}`,
	} {
		if _, _, err := service.CreateSource(ctx, "settings-invalid-"+name, "settings-src", "settings-source", []byte(settings), make([]byte, 32)); err == nil {
			t.Fatalf("%s: invalid settings accepted", name)
		}
		var count int
		if err := service.runner.Reader().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM alert_sources`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s: rejected create left rows: %d err=%v", name, count, err)
		}
	}
}

func TestSchemalessSourceRejectsSettings(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	ctx := adminCommandContext(t, context.Background())
	if _, _, err := service.CreateSource(ctx, "am-settings-0001", "plain-am", "alertmanager", []byte(`{"anything":1}`), make([]byte, 32)); err == nil {
		t.Fatal("alertmanager (no schema) accepted a non-empty settings document")
	}
	if _, _, err := service.CreateSource(ctx, "am-settings-0002", "plain-am", "alertmanager", nil, make([]byte, 32)); err != nil {
		t.Fatalf("alertmanager with absent settings rejected: %v", err)
	}
}

func TestSetSourceSettingsFencesAndAdvancesSnapshot(t *testing.T) {
	service, database, done := newTestService(t)
	defer done()
	if err := service.UseSourceRegistry(settingsRegistry(t)); err != nil {
		t.Fatal(err)
	}
	ctx := adminCommandContext(t, context.Background())
	digest := make([]byte, 32)
	created, _, err := service.CreateSource(ctx, "settings-update-0001", "settings-src", "settings-source", []byte(`{"site":"eu"}`), digest)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := service.GetSource(ctx, "settings-src")
	if err != nil {
		t.Fatal(err)
	}

	updated, replayed, err := service.SetSourceSettings(ctx, "settings-update-0002", "settings-src", []byte(`{"filter":["noise"],"site":"us"}`), detail.RowVersion)
	if err != nil || replayed {
		t.Fatalf("update = (%+v, %v, %v)", updated, replayed, err)
	}
	if string(updated.Settings) != `{"filter":["noise"],"site":"us"}` {
		t.Fatalf("updated settings = %s", updated.Settings)
	}
	// Delivery identity must not move.
	var key, protocol string
	if err := database.SQL.QueryRow(`SELECT source_key, protocol FROM alert_sources WHERE id=?`, created.SourceID).Scan(&key, &protocol); err != nil || key != "settings-src" || protocol != "settings-source" {
		t.Fatalf("delivery identity moved: (%q,%q) err=%v", key, protocol, err)
	}

	// Stale row version is a recorded deterministic rejection.
	stale, _, err := service.SetSourceSettings(ctx, "settings-update-0003", "settings-src", []byte(`{"site":"jp"}`), detail.RowVersion)
	var rejection *execution.Rejection
	if err == nil || !errors.As(err, &rejection) || rejection.Code != CodeRowVersionConflict {
		t.Fatalf("stale fence = (%+v, %v)", stale, err)
	}

	// Replay returns the stored projection without re-execution.
	replayDetail, replayed, err := service.SetSourceSettings(ctx, "settings-update-0002", "settings-src", []byte(`{"filter":["noise"],"site":"us"}`), detail.RowVersion)
	if err != nil || !replayed || string(replayDetail.Settings) != `{"filter":["noise"],"site":"us"}` {
		t.Fatalf("replay = (%+v, %v, %v)", replayDetail, replayed, err)
	}

	// Each accepted write strictly advances the global settings counter.
	var versions []int64
	rows, err := database.SQL.Query(`SELECT settings_version FROM alert_sources WHERE source_key='settings-src'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	if len(versions) != 1 || versions[0] != 2 {
		t.Fatalf("settings_version after one accepted update = %v", versions)
	}
	snapshotVersion, sources, err := service.CredentialSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshotVersion < 2 || len(sources) != 1 || string(sources[0].Settings) != `{"filter":["noise"],"site":"us"}` {
		t.Fatalf("snapshot did not invalidate on settings change: version=%d sources=%+v", snapshotVersion, sources)
	}
}

func TestTwoInstancesSameKindKeepIndependentSettings(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	if err := service.UseSourceRegistry(settingsRegistry(t)); err != nil {
		t.Fatal(err)
	}
	ctx := adminCommandContext(t, context.Background())
	if _, _, err := service.CreateSource(ctx, "independent-eu-0001", "src-eu", "settings-source", []byte(`{"site":"eu"}`), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.CreateSource(ctx, "independent-us-0002", "src-us", "settings-source", []byte(`{"filter":["x"],"site":"us"}`), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	eu, err := service.GetSource(ctx, "src-eu")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.SetSourceSettings(ctx, "independent-eu-0003", "src-eu", []byte(`{"site":"eu-2"}`), eu.RowVersion); err != nil {
		t.Fatal(err)
	}
	euAfter, err := service.GetSource(ctx, "src-eu")
	if err != nil {
		t.Fatal(err)
	}
	usAfter, err := service.GetSource(ctx, "src-us")
	if err != nil {
		t.Fatal(err)
	}
	if string(euAfter.Settings) != `{"site":"eu-2"}` || string(usAfter.Settings) != `{"filter":["x"],"site":"us"}` {
		t.Fatalf("instances leaked settings into each other: eu=%s us=%s", euAfter.Settings, usAfter.Settings)
	}
	_, sources, err := service.CredentialSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 {
		t.Fatalf("snapshot source count = %d", len(sources))
	}
	byKey := map[string]string{}
	for _, source := range sources {
		byKey[source.SourceKey] = string(source.Settings)
	}
	if byKey["src-eu"] != `{"site":"eu-2"}` || byKey["src-us"] != `{"filter":["x"],"site":"us"}` {
		t.Fatalf("snapshot lost per-instance settings: %v", byKey)
	}
}

func TestSetSourceSettingsRejectsUnknownSourceAndInvalidDocument(t *testing.T) {
	service, _, done := newTestService(t)
	defer done()
	if err := service.UseSourceRegistry(settingsRegistry(t)); err != nil {
		t.Fatal(err)
	}
	ctx := adminCommandContext(t, context.Background())
	if _, _, err := service.SetSourceSettings(ctx, "settings-ghost-0001", "ghost", []byte(`{"site":"eu"}`), 1); err == nil {
		t.Fatal("unknown source accepted")
	}
	digest := make([]byte, 32)
	if _, _, err := service.CreateSource(ctx, "settings-valid-0001", "settings-src", "settings-source", []byte(`{"site":"eu"}`), digest); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.SetSourceSettings(ctx, "settings-invalid-0002", "settings-src", []byte(`{"nope":1}`), 1); err == nil {
		t.Fatal("invalid document accepted on update")
	}
}
