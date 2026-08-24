package browser

import (
	"context"
	"fmt"
	"time"
)

// InventoryObservation is Lintel's non-content observation of one current
// profile generation. It is deliberately limited to the values that the
// durable reconciliation ledger is allowed to retain.
type InventoryObservation struct {
	IdentityID        int64
	Status            string
	ChromiumRevision  string
	ManifestDigestHex string
}

// ReconcileInventory validates a complete Lintel inventory report and writes
// its append-only results atomically. A malformed report creates no partial
// reconciliation history and never opens the browser-dispatch fence.
func (service *Service) ReconcileInventory(ctx context.Context, bootID string, epoch uint64, observations map[int64]InventoryObservation) error {
	if bootID == "" || epoch == 0 {
		return ErrInvalid
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	rows, err := conn.QueryContext(ctx, `SELECT g.identity_id,g.id,g.generation,g.chromium_revision,g.profile_manifest_digest FROM browser_profile_generations g JOIN browser_identities i ON i.current_profile_generation_id=g.id ORDER BY g.identity_id`)
	if err != nil {
		return err
	}
	var expected []InventoryItem
	for rows.Next() {
		var item InventoryItem
		if err := rows.Scan(&item.IdentityID, &item.ProfileGenerationID, &item.Generation, &item.ChromiumRevision, &item.ManifestDigest); err != nil {
			rows.Close()
			return err
		}
		expected = append(expected, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(expected) != len(observations) {
		return fmt.Errorf("%w: inventory generation set does not match", ErrInvalid)
	}

	now := service.now().UTC().Format(time.RFC3339Nano)
	for _, item := range expected {
		observation, ok := observations[item.ProfileGenerationID]
		if !ok {
			return fmt.Errorf("%w: missing profile generation", ErrInvalid)
		}
		if observation.IdentityID != item.IdentityID {
			return fmt.Errorf("%w: profile generation identity does not match", ErrInvalid)
		}
		if observation.Status != "compatible" && observation.Status != "missing" && observation.Status != "manifest_invalid" && observation.Status != "chromium_revision_mismatch" {
			return fmt.Errorf("%w: invalid inventory status", ErrInvalid)
		}
		if observation.Status == "compatible" && (observation.ChromiumRevision != item.ChromiumRevision || observation.ManifestDigestHex != item.ManifestDigest) {
			return fmt.Errorf("%w: incompatible compatible observation", ErrInvalid)
		}
		if observation.Status == "chromium_revision_mismatch" && (observation.ChromiumRevision == item.ChromiumRevision || observation.ManifestDigestHex != item.ManifestDigest) {
			return fmt.Errorf("%w: invalid revision mismatch", ErrInvalid)
		}
		if observation.Status == "missing" && (observation.ChromiumRevision != "" || observation.ManifestDigestHex != "") {
			return fmt.Errorf("%w: missing observation includes profile data", ErrInvalid)
		}
		if observation.Status == "manifest_invalid" && observation.ManifestDigestHex == item.ManifestDigest {
			return fmt.Errorf("%w: manifest-invalid observation has expected digest", ErrInvalid)
		}
		var revision any
		var digest any
		if observation.ChromiumRevision != "" {
			revision = observation.ChromiumRevision
		}
		if observation.ManifestDigestHex != "" {
			digest = observation.ManifestDigestHex
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO browser_profile_reconciliations(boot_id,connection_epoch,identity_id,profile_generation_id,result,observed_chromium_revision,observed_manifest_digest,reconciled_at) VALUES(?,?,?,?,?,?,?,?)`, bootID, epoch, item.IdentityID, item.ProfileGenerationID, observation.Status, revision, digest, now); err != nil {
			return err
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}
