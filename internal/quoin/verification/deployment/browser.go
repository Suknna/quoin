package deployment

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// EnqueueBrowserOperations creates one Queued deployment verification
// operation per frozen browser_identity item (idempotent per item). The
// global FIFO dispatcher owns physical admission; this only owns the durable
// binding: manifest item, initiating Admin Session, frozen identity revision
// and current profile generation, and the deterministic clone identity.
func (service *Service) EnqueueBrowserOperations(ctx context.Context, invocationID int64) error {
	return service.withConn(ctx, func(conn *sql.Conn) error {
		var receipt int64
		err := conn.QueryRowContext(ctx, `SELECT id FROM verification_finalization_receipts WHERE invocation_id=?`, invocationID).Scan(&receipt)
		if err == nil {
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		manifest, err := service.loadManifestOn(ctx, conn, invocationID)
		if err != nil {
			return err
		}
		now := service.now().UTC().Format(time.RFC3339Nano)
		rows, err := conn.QueryContext(ctx, `SELECT i.id,l.browser_identity_id,l.identity_revision_id,l.profile_generation_id
			FROM verification_invocation_items i JOIN verification_browser_identity_item_locators l ON l.item_id=i.id
			WHERE i.invocation_id=? ORDER BY i.item_seq`, invocationID)
		if err != nil {
			return err
		}
		type frozenBrowserItem struct {
			itemID, identityID, revisionID, generationID int64
		}
		items := []frozenBrowserItem{}
		for rows.Next() {
			entry := frozenBrowserItem{}
			if err := rows.Scan(&entry.itemID, &entry.identityID, &entry.revisionID, &entry.generationID); err != nil {
				rows.Close()
				return err
			}
			items = append(items, entry)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, entry := range items {
			var catalogDigest, catalogVersion string
			if err := conn.QueryRowContext(ctx, `SELECT journey_catalog_digest,journey_catalog_version FROM browser_identity_revisions WHERE id=?`, entry.revisionID).Scan(&catalogDigest, &catalogVersion); err != nil {
				return err
			}
			cloneIdentity := fmt.Sprintf("deployment-verification-%d-%d", invocationID, entry.itemID)
			if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO browser_operations(
				identity_id,identity_revision_id,profile_generation_id,owner_attempt_id,kind,actor_user_id,actor_session_id,
				verification_manifest_item_id,clone_identity,state,journey_catalog_digest,journey_catalog_version,
				journey_id,journey_version,probe_phase,requested_at)
				VALUES(?,?,?,NULL,'deployment_verification',NULL,?,?,?,?,?,?,NULL,NULL,NULL,?)`,
				entry.identityID, entry.revisionID, entry.generationID, manifest.sessionID, entry.itemID, cloneIdentity,
				"Queued", catalogDigest, catalogVersion, now); err != nil {
				return translateInsert(err, "deployment verification operation")
			}
		}
		return nil
	})
}

// BrowserCompletion is the terminal fact delivered by the runtime surface.
type BrowserCompletion struct {
	OperationID     int64
	Outcome         string // Succeeded | Failed
	TerminalReason  string
	ResultDigest    []byte
	EndedAt         time.Time
	ProbeResult     string // Authenticated | Unauthenticated | Indeterminate | ""
	ProbeObservedAt string
}

// HandleBrowserCompletion records the functional side of one deployment
// verification operation. The happy path waits for the typed Stop
// acknowledgment before writing the coupled typed result; a technical
// failure records the infrastructure-interrupted item result directly and
// lets stop convergence release the fences (VERIFY-BROWSER-003).
func (service *Service) HandleBrowserCompletion(ctx context.Context, completion BrowserCompletion) error {
	return service.withConn(ctx, func(conn *sql.Conn) error {
		var itemID int64
		var state string
		if err := conn.QueryRowContext(ctx, `SELECT verification_manifest_item_id,state FROM browser_operations WHERE id=? AND kind='deployment_verification'`,
			completion.OperationID).Scan(&itemID, &state); err != nil {
			if err == sql.ErrNoRows {
				return nil
			}
			return err
		}
		if itemID == 0 {
			return nil
		}
		var inputDigest string
		if err := conn.QueryRowContext(ctx, `SELECT input_digest FROM verification_invocation_items WHERE id=?`, itemID).Scan(&inputDigest); err != nil {
			return err
		}
		if completion.Outcome == "Succeeded" && completion.ProbeResult == "Authenticated" {
			// The typed result is written by the Stop acknowledgment path once
			// same-boot cleanup evidence exists.
			return nil
		}
		if completion.Outcome == "Succeeded" && completion.ProbeResult == "Unauthenticated" {
			return nil // also closed by the stop path with functional=failed
		}
		// Technical failure: infrastructure interruption is a warned fact.
		if state != "Failed" && state != "Cancelled" && state != "Interrupted" {
			return nil
		}
		return service.appendQuoinResult(ctx, conn, itemID, inputDigest, "warned", "infrastructure_interrupted", completion.EndedAt.UTC().Format(time.RFC3339Nano),
			map[string]any{"operationId": completion.OperationID, "terminalReason": completion.TerminalReason})
	})
}

func nullabl(value any, nullify bool) any {
	if nullify {
		return nil
	}
	return value
}

func insertItemResult(ctx context.Context, conn *sql.Conn, itemID int64, inputDigest, resultDigest, producer, outcome, category, observedAt string) (int64, error) {
	committedAt := time.Now().UTC().Format(time.RFC3339Nano)
	insert, err := conn.ExecContext(ctx, `INSERT INTO verification_item_results(item_id,input_digest,result_digest,producer_type,outcome,category,observed_at,committed_at,evidence_index_digest)
		VALUES(?,?,?,?,?,?,?,?,?)`, itemID, inputDigest, resultDigest, producer, outcome, category, observedAt, committedAt, resultDigest)
	if err != nil {
		return 0, err
	}
	return insert.LastInsertId()
}

func (service *Service) appendQuoinResult(ctx context.Context, conn *sql.Conn, itemID int64, inputDigest, outcome, category, observedAt string, evidence map[string]any) error {
	digest, err := canonicalDigest(evidence)
	if err != nil {
		return err
	}
	_, err = insertItemResult(ctx, conn, itemID, inputDigest, digest, "quoin", outcome, category, observedAt)
	return translateInsert(err, "deployment verification result")
}
