package businesssystem

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/config"
)

// These tests exercise declaration upload and publish as the public seam. They
// deliberately use named Alertmanager sources so reference resolution, rather
// than an internal projection fixture, establishes the published rule scope.
func TestPublishRejectsDuplicateEnabledAlertRule(t *testing.T) {
	h := newHarness(t)
	seedDeclarationAlertSource(t, h, "alerts-primary")
	seedDeclarationAlertSource(t, h, "alerts-secondary")

	first := h.mustUpload(t, declarationWithAlerts("payments", true, []string{"alerts-primary"}, "service: checkout"), "cmd-alert-rule-upload-1")
	if _, err := h.systems.Publish(context.Background(), h.principal, "cmd-alert-rule-publish-1", "payments", mustID(t, first.ID), nil); err != nil {
		t.Fatalf("publish first alert rule: %v", err)
	}
	second := h.mustUpload(t, declarationWithAlerts("checkout", true, []string{"alerts-primary", "alerts-secondary"}, "service: checkout"), "cmd-alert-rule-upload-2")

	_, err := h.systems.Publish(context.Background(), h.principal, "cmd-alert-rule-publish-2", "checkout", mustID(t, second.ID), nil)
	var validation *config.ValidationError
	if !errors.As(err, &validation) || len(validation.Errors) != 1 || validation.Errors[0].Path != "spec.alerts.matchLabels" || !strings.Contains(validation.Errors[0].Reason, "业务冲突") {
		t.Fatalf("duplicate alert rule must be a matchLabels validation error: %v", err)
	}
	var current sql.NullInt64
	if err := h.db.QueryRow(`SELECT current_config_version_id FROM business_systems WHERE key='checkout'`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current.Valid {
		t.Fatalf("rejected publish must not move the pointer: %d", current.Int64)
	}
	var commands int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM client_commands WHERE command_type='business_system.config.publish'`).Scan(&commands); err != nil {
		t.Fatal(err)
	}
	if commands != 1 {
		t.Fatalf("rejected publish must not record a command: %d", commands)
	}
}

func TestPublishAllowsDistinctOrDisabledAlertRules(t *testing.T) {
	h := newHarness(t)
	seedDeclarationAlertSource(t, h, "alerts-primary")
	seedDeclarationAlertSource(t, h, "alerts-secondary")

	first := h.mustUpload(t, declarationWithAlerts("payments", true, []string{"alerts-primary"}, "service: checkout"), "cmd-alert-rule-upload-3")
	if _, err := h.systems.Publish(context.Background(), h.principal, "cmd-alert-rule-publish-3", "payments", mustID(t, first.ID), nil); err != nil {
		t.Fatalf("publish first alert rule: %v", err)
	}
	// No source intersection means the equal label condition is not a duplicate.
	distinctSource := h.mustUpload(t, declarationWithAlerts("orders", true, []string{"alerts-secondary"}, "service: checkout"), "cmd-alert-rule-upload-4")
	if _, err := h.systems.Publish(context.Background(), h.principal, "cmd-alert-rule-publish-4", "orders", mustID(t, distinctSource.ID), nil); err != nil {
		t.Fatalf("publish distinct source rule: %v", err)
	}
	// Disabled declarations are not active attribution rules even when equal.
	disabled := h.mustUpload(t, declarationWithAlerts("checkout", false, []string{"alerts-primary"}, "service: checkout"), "cmd-alert-rule-upload-5")
	if _, err := h.systems.Publish(context.Background(), h.principal, "cmd-alert-rule-publish-5", "checkout", mustID(t, disabled.ID), nil); err != nil {
		t.Fatalf("publish disabled duplicate-shaped rule: %v", err)
	}
}

func seedDeclarationAlertSource(t *testing.T, h *harness, sourceKey string) {
	t.Helper()
	if _, err := h.db.Exec(`INSERT INTO alert_sources(source_key,protocol,enabled,created_at) VALUES(?,'alertmanager',1,?)`, sourceKey, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}

func declarationWithAlerts(systemKey string, enabled bool, sourceRefs []string, label string) string {
	sources := make([]string, len(sourceRefs))
	for index, source := range sourceRefs {
		sources[index] = "    - " + source
	}
	return strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(validSystemYAML,
		"  alerts:\n    sourceRefs: []\n    matchLabels: {}",
		"  enabled: "+strconv.FormatBool(enabled)+"\n  alerts:\n    sourceRefs:\n"+strings.Join(sources, "\n")+"\n    matchLabels: {"+label+"}"),
		"  name: payments", "  name: "+systemKey),
		"  displayName: 支付系统", "  displayName: "+systemKey)
}
