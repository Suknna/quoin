package deployment

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ExecutionHooks carries the product-owned execution entries the invocation
// drives per frozen item. The app wires them; nil hooks leave the items to
// the helper/observation/deadline paths.
type ExecutionHooks struct {
	// StartConnectionProbe starts one connection probe by connection name.
	StartConnectionProbe func(ctx context.Context, connectionName string) error
	// RunConfigVerification creates the deployment_acceptance run bound to
	// the frozen manifest item.
	RunConfigVerification func(ctx context.Context, principalID, businessSystemID, versionID, contractVersionID, manifestItemID int64) error
}

// SetExecutionHooks wires the execution entries (Run-time state).
func (service *Service) SetExecutionHooks(hooks ExecutionHooks) {
	service.execution = hooks
}

// EnqueueExecution drives the product-executed items of one open invocation:
// connection items get a probe (unless one already ran or is active), config
// items get their deployment_acceptance run (idempotent per item). Retries
// are safe; the sweeper calls this again for invocations whose runtime was
// not yet connected.
func (service *Service) EnqueueExecution(ctx context.Context, invocationID int64) error {
	if service.execution.StartConnectionProbe == nil && service.execution.RunConfigVerification == nil {
		return nil
	}
	manifest, err := service.loadManifest(ctx, invocationID)
	if err != nil {
		if err == ErrNotFound {
			return nil
		}
		return err
	}
	if _, ok, err := service.receiptFor(ctx, invocationID); err != nil {
		return err
	} else if ok {
		return nil
	}
	// Connections: probe unless a matching result already exists or a probe
	// is active for the frozen binding.
	rows, err := service.db.QueryContext(ctx, `SELECT i.id,c.name FROM verification_invocation_items i
		JOIN verification_connection_item_locators l ON l.item_id=i.id
		JOIN connections c ON c.id=l.connection_id
		WHERE i.invocation_id=? AND i.object_kind='connection'
		AND NOT EXISTS (SELECT 1 FROM verification_item_results r WHERE r.item_id=i.id)`, invocationID)
	if err != nil {
		return err
	}
	type connectionItem struct {
		id   int64
		name string
	}
	connections := []connectionItem{}
	for rows.Next() {
		item := connectionItem{}
		if err := rows.Scan(&item.id, &item.name); err != nil {
			rows.Close()
			return err
		}
		connections = append(connections, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range connections {
		if service.execution.StartConnectionProbe == nil {
			continue
		}
		if err := service.execution.StartConnectionProbe(ctx, item.name); err != nil {
			// An active probe for the same connection is not an error; a
			// disconnected Plinth retries through the sweeper.
			continue
		}
	}
	// Configs: one deployment_acceptance run per frozen item.
	rows, err = service.db.QueryContext(ctx, `SELECT i.id,l.business_system_id,l.config_version_id,l.label_contract_version_id
		FROM verification_invocation_items i JOIN verification_config_item_locators l ON l.item_id=i.id
		WHERE i.invocation_id=? AND i.object_kind='config'
		AND NOT EXISTS (SELECT 1 FROM verification_item_results r WHERE r.item_id=i.id)
		AND NOT EXISTS (SELECT 1 FROM config_verification_runs v WHERE v.verification_manifest_item_id=i.id AND v.state IN ('Queued','Running'))`, invocationID)
	if err != nil {
		return err
	}
	type configItem struct {
		itemID, systemID, versionID, contractID int64
	}
	configs := []configItem{}
	for rows.Next() {
		item := configItem{}
		if err := rows.Scan(&item.itemID, &item.systemID, &item.versionID, &item.contractID); err != nil {
			rows.Close()
			return err
		}
		configs = append(configs, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range configs {
		if service.execution.RunConfigVerification == nil {
			continue
		}
		if err := service.execution.RunConfigVerification(ctx, manifest.principalID, item.systemID, item.versionID, item.contractID, item.itemID); err != nil {
			continue
		}
	}
	return nil
}

// ReconcileResults maps finished product execution onto item results for one
// open invocation: terminal deployment_acceptance config runs and connection
// probe results that match the frozen binding and fall inside the window.
// Idempotent by result digest; a conflicting digest forms a verifier
// conflict exactly like any other producer.
func (service *Service) ReconcileResults(ctx context.Context, invocationID int64) error {
	if _, ok, err := service.receiptFor(ctx, invocationID); err != nil {
		return err
	} else if ok {
		return nil
	}
	manifest, err := service.loadManifest(ctx, invocationID)
	if err != nil {
		if err == ErrNotFound {
			return nil
		}
		return err
	}
	if service.now().UTC().After(manifest.deadline) {
		return nil
	}
	return service.withConn(ctx, func(conn *sql.Conn) error {
		// Config runs: terminal deployment_acceptance runs bound to this
		// invocation's items.
		rows, err := conn.QueryContext(ctx, `SELECT i.id,v.state FROM verification_invocation_items i
			JOIN config_verification_runs v ON v.verification_manifest_item_id=i.id AND v.purpose='deployment_acceptance'
			WHERE i.invocation_id=? AND i.object_kind='config' AND v.state IN ('Passed','Failed','Cancelled','Interrupted')
			AND NOT EXISTS (SELECT 1 FROM verification_item_results r WHERE r.item_id=i.id)`, invocationID)
		if err != nil {
			return err
		}
		type terminalConfig struct {
			itemID int64
			state  string
		}
		configs := []terminalConfig{}
		for rows.Next() {
			entry := terminalConfig{}
			if err := rows.Scan(&entry.itemID, &entry.state); err != nil {
				rows.Close()
				return err
			}
			configs = append(configs, entry)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, entry := range configs {
			var inputDigest string
			if err := conn.QueryRowContext(ctx, `SELECT input_digest FROM verification_invocation_items WHERE id=?`, entry.itemID).Scan(&inputDigest); err != nil {
				return err
			}
			outcome, category := "warned", "infrastructure_interrupted"
			switch entry.state {
			case "Passed":
				outcome, category = "passed", "passed"
			case "Failed":
				outcome, category = "failed", "functional_assertion_failed"
			case "Cancelled":
				outcome, category = "warned", "operator_cancelled"
			}
			observedAt := service.now().UTC().Format(time.RFC3339Nano)
			digest, err := canonicalDigest(map[string]any{"itemId": entry.itemID, "kind": "config_verification_run", "state": entry.state})
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO verification_item_results(item_id,input_digest,result_digest,producer_type,outcome,category,observed_at,committed_at,evidence_index_digest)
				VALUES(?,?,?,'quoin',?,?,?,?,?)`, entry.itemID, inputDigest, digest, outcome, category, observedAt, observedAt, digest); err != nil {
				return translateInsert(err, "config verification result")
			}
		}
		// Connection probes: newest finished result per frozen binding inside
		// the observation window.
		rows, err = conn.QueryContext(ctx, `SELECT i.id,p.outcome,p.finished_at FROM verification_invocation_items i
			JOIN verification_connection_item_locators l ON l.item_id=i.id
			JOIN connection_probe_results p ON p.connection_id=l.connection_id
				AND p.connection_revision_id=l.connection_revision_id
				AND p.credential_generation_id=l.credential_generation_id
				AND p.root_binding_revision=l.root_binding_revision
			WHERE i.invocation_id=? AND i.object_kind='connection'
			AND julianday(p.finished_at) >= julianday(?)
			AND NOT EXISTS (SELECT 1 FROM verification_item_results r WHERE r.item_id=i.id)`, invocationID, manifest.startedAt)
		if err != nil {
			return err
		}
		type probeResult struct {
			itemID   int64
			outcome  string
			finished string
		}
		probes := []probeResult{}
		for rows.Next() {
			entry := probeResult{}
			if err := rows.Scan(&entry.itemID, &entry.outcome, &entry.finished); err != nil {
				rows.Close()
				return err
			}
			probes = append(probes, entry)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		seen := map[int64]bool{}
		for _, entry := range probes {
			if seen[entry.itemID] {
				continue
			}
			seen[entry.itemID] = true
			var inputDigest string
			if err := conn.QueryRowContext(ctx, `SELECT input_digest FROM verification_invocation_items WHERE id=?`, entry.itemID).Scan(&inputDigest); err != nil {
				return err
			}
			outcome, category := "warned", "infrastructure_interrupted"
			switch entry.outcome {
			case "passed":
				outcome, category = "passed", "passed"
			case "failed":
				outcome, category = "failed", "functional_assertion_failed"
			case "cancelled":
				outcome, category = "warned", "operator_cancelled"
			}
			finished, err := time.Parse(time.RFC3339Nano, entry.finished)
			if err != nil {
				return fmt.Errorf("probe finished_at: %w", err)
			}
			observedAt := finished.UTC().Format(time.RFC3339Nano)
			digest, err := canonicalDigest(map[string]any{"itemId": entry.itemID, "kind": "connection_probe", "outcome": entry.outcome})
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO verification_item_results(item_id,input_digest,result_digest,producer_type,outcome,category,observed_at,committed_at,evidence_index_digest)
				VALUES(?,?,?,'runtime',?,?,?,?,?)`, entry.itemID, inputDigest, digest, outcome, category, observedAt, service.now().UTC().Format(time.RFC3339Nano), digest); err != nil {
				return translateInsert(err, "connection probe result")
			}
		}
		return nil
	})
}
