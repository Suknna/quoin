package deployment

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/auth"
)

const commandStart = "deployment_verification.start"
const commandCancel = "deployment_verification.cancel"
const commandObservation = "deployment_verification.observation"

// Start freezes one immutable invocation in a single SQLite transaction:
// manifest, complete item set, and the server-generated typed locators
// (VERIFY-DA-001). Everything except the client command ID is discovered
// mechanically from the deployment binding, the frozen catalog, and the
// current authoritative objects; later object changes only append drift.
func (service *Service) Start(ctx context.Context, sessionID, principalID int64, clientCommandID string) (int64, error) {
	requestDigest := auth.DigestCommand(commandStart, map[string]any{"clientCommandId": clientCommandID, "session": sessionID})
	if record, found, err := auth.LookupCommand(ctx, service.db, principalID, clientCommandID); err != nil {
		return 0, err
	} else if found {
		if record.RequestDigest != requestDigest || record.ResultObjectType != "verification_invocation" {
			return 0, service.fail(errConflict, "client command %q was already used by a different request", clientCommandID)
		}
		return record.ResultObjectID, nil
	}
	var invocationID int64
	err := service.withConn(ctx, func(conn *sql.Conn) error {
		if record, found, lookupErr := auth.LookupCommandOn(ctx, conn, principalID, clientCommandID); lookupErr != nil {
			return lookupErr
		} else if found {
			if record.RequestDigest != requestDigest || record.ResultObjectType != "verification_invocation" {
				return service.fail(errConflict, "client command %q was already used by a different request", clientCommandID)
			}
			invocationID = record.ResultObjectID
			return nil
		}
		if err := service.requireActiveAdmin(ctx, conn, sessionID, principalID); err != nil {
			return err
		}
		id, err := service.startOn(ctx, conn, sessionID, principalID)
		if err != nil {
			return err
		}
		invocationID = id
		payload, _ := json.Marshal(map[string]any{"invocationId": id})
		if err := auth.RecordCommand(ctx, conn, principalID, clientCommandID, commandStart, requestDigest, "committed", "verification_invocation", id, string(payload)); err != nil {
			return err
		}
		return service.recordAudit(ctx, conn, "user", principalID, "deployment_verification.start", clientCommandID, "success", "verification_invocation", id)
	})
	return invocationID, err
}

func (service *Service) startOn(ctx context.Context, conn *sql.Conn, sessionID, principalID int64) (int64, error) {
	if service.binding == nil {
		return 0, service.ErrBindingUnavailable()
	}
	items, err := service.resolveApplicable(ctx, conn)
	if err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, service.fail(errState, "no applicable Deployment Acceptance scenario for this deployment")
	}
	startedAt := service.now().UTC().Truncate(time.Second)
	deadlineAt := startedAt.Add(8 * time.Hour).Format(time.RFC3339Nano)
	startedText := startedAt.Format(time.RFC3339Nano)
	createdAt := service.now().UTC().Format(time.RFC3339Nano)
	applicableSet := make([]map[string]any, 0, len(items))
	itemSet := make([]map[string]any, 0, len(items))
	for index, item := range items {
		applicableSet = append(applicableSet, map[string]any{"scenarioId": item.ScenarioID, "cellId": item.CellID, "objectKind": item.ObjectKind, "inputDigest": item.InputDigest})
		itemSet = append(itemSet, map[string]any{"itemSeq": index + 1, "scenarioId": item.ScenarioID, "cellId": item.CellID, "objectKind": item.ObjectKind, "inputDigest": item.InputDigest})
	}
	applicableSetDigest, err := canonicalDigest(applicableSet)
	if err != nil {
		return 0, err
	}
	itemSetDigest, err := canonicalDigest(itemSet)
	if err != nil {
		return 0, err
	}
	originDigest := sha256Text(service.publicOrigin)
	manifestDigest, err := canonicalDigest(map[string]any{
		"adminSessionId": sessionID, "principalUserId": principalID,
		"releaseSubjectDigest": service.binding.ReleaseSubjectDigest, "catalogDigest": CatalogDigest(),
		"resultProfileDigest": ResultProfileDigest(), "deploymentConfigDigest": service.binding.DeploymentConfigDigest,
		"publicOriginDigest": originDigest, "applicableSetDigest": applicableSetDigest,
		"itemCount": len(items), "itemSetDigest": itemSetDigest, "startedAt": startedText,
	})
	if err != nil {
		return 0, err
	}
	insert, err := conn.ExecContext(ctx, `INSERT INTO verification_invocation_manifests(
		admin_session_id,principal_user_id,release_subject_digest,catalog_digest,result_profile_digest,
		deployment_config_digest,public_origin_digest,applicable_set_digest,item_count,item_set_digest,
		manifest_digest,canonical_input_digest,started_at,deadline_at,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sessionID, principalID, service.binding.ReleaseSubjectDigest, CatalogDigest(), ResultProfileDigest(),
		service.binding.DeploymentConfigDigest, originDigest, applicableSetDigest, len(items), itemSetDigest,
		manifestDigest, requestStartDigest(service.binding, originDigest), startedText, deadlineAt, createdAt)
	if err != nil {
		return 0, translateInsert(err, "verification invocation manifest")
	}
	invocationID, err := insert.LastInsertId()
	if err != nil {
		return 0, err
	}
	for index, item := range items {
		result, err := conn.ExecContext(ctx, `INSERT INTO verification_invocation_items(
			invocation_id,item_seq,scenario_id,cell_id,object_kind,input_digest,created_at) VALUES(?,?,?,?,?,?,?)`,
			invocationID, index+1, item.ScenarioID, item.CellID, item.ObjectKind, item.InputDigest, createdAt)
		if err != nil {
			return 0, translateInsert(err, "verification invocation item")
		}
		itemID, err := result.LastInsertId()
		if err != nil {
			return 0, err
		}
		if err := insertLocator(ctx, conn, itemID, item); err != nil {
			return 0, err
		}
	}
	return invocationID, nil
}

// requestStartDigest is the canonical input digest of the start request: the
// full frozen identity the invocation was derived from.
func requestStartDigest(binding *contract.DeploymentBinding, originDigest string) string {
	digest, _ := canonicalDigest(map[string]any{
		"releaseSubjectDigest": binding.ReleaseSubjectDigest, "deploymentConfigDigest": binding.DeploymentConfigDigest,
		"backend": binding.Backend, "publicOriginDigest": originDigest, "catalogDigest": CatalogDigest(),
	})
	return digest
}

func insertLocator(ctx context.Context, conn *sql.Conn, itemID int64, item resolvedItem) error {
	switch {
	case item.Deployment != nil:
		_, err := conn.ExecContext(ctx, `INSERT INTO verification_deployment_item_locators(
			item_id,release_subject_digest,deployment_config_digest,public_origin_digest,backend,architecture) VALUES(?,?,?,?,?,?)`,
			itemID, item.Deployment.ReleaseSubjectDigest, item.Deployment.DeploymentConfigDigest, item.Deployment.PublicOriginDigest, item.Deployment.Backend, item.Deployment.Architecture)
		return err
	case item.Connection != nil:
		_, err := conn.ExecContext(ctx, `INSERT INTO verification_connection_item_locators(
			item_id,connection_id,connection_revision_id,credential_generation_id,root_binding_revision,probe_contract_digest) VALUES(?,?,?,?,?,?)`,
			itemID, item.Connection.ConnectionID, item.Connection.RevisionID, item.Connection.CredentialGeneration, item.Connection.RootBindingRevision, item.Connection.ProbeContractDigest)
		return err
	case item.Config != nil:
		_, err := conn.ExecContext(ctx, `INSERT INTO verification_config_item_locators(
			item_id,business_system_id,config_version_id,label_contract_version_id) VALUES(?,?,?,?)`,
			itemID, item.Config.BusinessSystemID, item.Config.ConfigVersionID, item.Config.LabelContractVersion)
		return err
	case item.Browser != nil:
		_, err := conn.ExecContext(ctx, `INSERT INTO verification_browser_identity_item_locators(
			item_id,browser_identity_id,identity_revision_id,profile_generation_id,current_inventory_digest) VALUES(?,?,?,?,?)`,
			itemID, item.Browser.BrowserIdentityID, item.Browser.IdentityRevisionID, item.Browser.CurrentGenerationID, item.Browser.CurrentInventoryDigest)
		return err
	case item.Observation != nil:
		_, err := conn.ExecContext(ctx, `INSERT INTO verification_ui_observation_item_locators(
			item_id,browser_artifact,browser_version,architecture,viewport_css_px,motion) VALUES(?,?,?,?,?,?)`,
			itemID, item.Observation.BrowserArtifact, item.Observation.BrowserVersion, item.Observation.Architecture, item.Observation.ViewportCssPx, item.Observation.Motion)
		return err
	}
	return fmt.Errorf("resolved item %s/%s carries no typed locator", item.ScenarioID, item.CellID)
}

// requireActiveAdmin revalidates the acting session against the same
// conditions the manifest trigger freezes (active Admin Session bound to the
// principal). The trigger remains the last line of defense.
func (service *Service) requireActiveAdmin(ctx context.Context, conn *sql.Conn, sessionID, principalID int64) error {
	var role string
	var enabled int
	var revoked sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT u.role,u.enabled,s.revoked_at FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.id=? AND s.user_id=?`, sessionID, principalID).Scan(&role, &enabled, &revoked); err != nil {
		if err == sql.ErrNoRows {
			return service.fail(errState, "acting session is not valid")
		}
		return err
	}
	if role != "admin" || enabled != 1 || revoked.Valid {
		return service.fail(errState, "acting session is not an active Admin Session")
	}
	return nil
}

func (service *Service) recordAudit(ctx context.Context, conn *sql.Conn, actorType string, actorID int64, action, clientCommandID, outcome, refType string, refID int64) error {
	_, err := conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,client_command_id,outcome,domain_ref_type,domain_ref_id,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		actorType, actorID, action, clientCommandID, outcome, refType, refID, service.now().UTC().Format(time.RFC3339Nano))
	return err
}

// translateInsert surfaces unique-constraint collisions as typed conflicts.
func translateInsert(err error, what string) error {
	if err == nil {
		return nil
	}
	if isUniqueViolation(err) {
		return &Error{Family: errConflict, Message: fmt.Sprintf("%s already exists for this frozen content and time window", what)}
	}
	return err
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
