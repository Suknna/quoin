package deployment

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// observeDrifts compares every frozen locator of an open invocation against
// the current authoritative objects and appends immutable subject-drift
// markers for what moved (VERIFY-DA-002). Deployment and UI cells are frozen
// from immutable release facts and cannot drift at runtime; connections,
// configs, and browser identities can. The unique key makes repeated
// observation idempotent, and a drift never rewrites a prior result — it only
// prevents a PASSED finalization.
func (service *Service) observeDrifts(ctx context.Context, invocationID int64) error {
	if service.binding == nil {
		return nil
	}
	return service.withConn(ctx, func(conn *sql.Conn) error {
		var receipt int64
		err := conn.QueryRowContext(ctx, `SELECT id FROM verification_finalization_receipts WHERE invocation_id=?`, invocationID).Scan(&receipt)
		if err == nil {
			return nil // the receipt is the terminal fence for all evidence writes
		}
		if err != sql.ErrNoRows {
			return err
		}
		observedAt := service.now().UTC().Format(time.RFC3339Nano)
		drift := func(itemID int64, objectKind, field, frozen, current string) error {
			if frozen == current {
				return nil
			}
			_, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO verification_subject_drifts(invocation_id,object_kind,drift_field,item_id,frozen_digest,current_digest,observed_at)
				VALUES(?,?,?,?,?,?,?)`, invocationID, objectKind, field, itemID, sha256Text(frozen), sha256Text(current), observedAt)
			return err
		}
		// Connection items: current revision/generation/root binding may rotate.
		rows, err := conn.QueryContext(ctx, `SELECT l.item_id,l.connection_revision_id,l.credential_generation_id,l.root_binding_revision,l.probe_contract_digest,
			c.current_revision_id,COALESCE(c.current_credential_generation_id,0),COALESCE(g.key_binding_revision,0)
			FROM verification_connection_item_locators l
			JOIN verification_invocation_items i ON i.id=l.item_id AND i.invocation_id=? AND i.object_kind='connection'
			JOIN connections c ON c.id=l.connection_id
			LEFT JOIN credential_generations g ON g.id=c.current_credential_generation_id`, invocationID)
		if err != nil {
			return err
		}
		type pending struct {
			itemID                                   int64
			revision, generation, root               int64
			probe                                    string
			currentRevision, currentGen, currentRoot int64
		}
		connections := []pending{}
		for rows.Next() {
			p := pending{}
			if err := rows.Scan(&p.itemID, &p.revision, &p.generation, &p.root, &p.probe, &p.currentRevision, &p.currentGen, &p.currentRoot); err != nil {
				rows.Close()
				return err
			}
			connections = append(connections, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, p := range connections {
			if err := drift(p.itemID, "connection", "connection_revision", fmt.Sprint(p.revision), fmt.Sprint(p.currentRevision)); err != nil {
				return err
			}
			if err := drift(p.itemID, "connection", "credential_generation", fmt.Sprint(p.generation), fmt.Sprint(p.currentGen)); err != nil {
				return err
			}
			if err := drift(p.itemID, "connection", "root_binding_revision", fmt.Sprint(p.root), fmt.Sprint(p.currentRoot)); err != nil {
				return err
			}
			if err := drift(p.itemID, "connection", "probe_contract_digest", p.probe, ProbeContractDigest()); err != nil {
				return err
			}
		}
		// Config items: current published version or contract may move.
		rows, err = conn.QueryContext(ctx, `SELECT l.item_id,l.config_version_id,l.label_contract_version_id,
			COALESCE(b.current_config_version_id,0),COALESCE(s.current_contract_id,0)
			FROM verification_config_item_locators l
			JOIN verification_invocation_items i ON i.id=l.item_id AND i.invocation_id=? AND i.object_kind='config'
			JOIN business_systems b ON b.id=l.business_system_id
			LEFT JOIN label_contract_state s ON s.id=1`, invocationID)
		if err != nil {
			return err
		}
		type configPending struct {
			itemID                          int64
			version, contract               int64
			currentVersion, currentContract int64
		}
		configs := []configPending{}
		for rows.Next() {
			p := configPending{}
			if err := rows.Scan(&p.itemID, &p.version, &p.contract, &p.currentVersion, &p.currentContract); err != nil {
				rows.Close()
				return err
			}
			configs = append(configs, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, p := range configs {
			if err := drift(p.itemID, "config", "config_version", fmt.Sprint(p.version), fmt.Sprint(p.currentVersion)); err != nil {
				return err
			}
			if err := drift(p.itemID, "config", "label_contract_version", fmt.Sprint(p.contract), fmt.Sprint(p.currentContract)); err != nil {
				return err
			}
		}
		// Browser identity items: current revision/generation/inventory may move.
		rows, err = conn.QueryContext(ctx, `SELECT l.item_id,l.identity_revision_id,l.profile_generation_id,l.current_inventory_digest,
			COALESCE(b.current_revision_id,0),COALESCE(b.current_profile_generation_id,0),COALESCE(g.profile_manifest_digest,'')
			FROM verification_browser_identity_item_locators l
			JOIN verification_invocation_items i ON i.id=l.item_id AND i.invocation_id=? AND i.object_kind='browser_identity'
			JOIN browser_identities b ON b.id=l.browser_identity_id
			LEFT JOIN browser_profile_generations g ON g.id=b.current_profile_generation_id`, invocationID)
		if err != nil {
			return err
		}
		type browserPending struct {
			itemID                             int64
			revision, generation               int64
			inventory                          string
			currentRevision, currentGeneration int64
			currentInventory                   string
		}
		browsers := []browserPending{}
		for rows.Next() {
			p := browserPending{}
			if err := rows.Scan(&p.itemID, &p.revision, &p.generation, &p.inventory, &p.currentRevision, &p.currentGeneration, &p.currentInventory); err != nil {
				rows.Close()
				return err
			}
			browsers = append(browsers, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, p := range browsers {
			if err := drift(p.itemID, "browser_identity", "browser_identity_revision", fmt.Sprint(p.revision), fmt.Sprint(p.currentRevision)); err != nil {
				return err
			}
			if err := drift(p.itemID, "browser_identity", "browser_profile_generation", fmt.Sprint(p.generation), fmt.Sprint(p.currentGeneration)); err != nil {
				return err
			}
			if err := drift(p.itemID, "browser_identity", "browser_inventory_observation", p.inventory, p.currentInventory); err != nil {
				return err
			}
		}
		return nil
	})
}
