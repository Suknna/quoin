package auth

// Durable client-command ledger over the frozen client_commands table
// (DATA-COMMAND-001..004). Every authenticated external domain write records
// its non-secret request digest and deterministic result here so replays with
// the same (principal, client_command_id) return the original outcome and
// different requests conflict. Raw secrets never enter the ledger — the
// schema trigger rejects any nested forbidden key, and digests only carry
// non-secret semantic fields (SEC-KEY-008).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Ledger outcomes (client_commands.outcome CHECK).
const (
	OutcomeCommitted     = "committed"
	OutcomeRejectedKnown = "rejected_known"
)

// CommandRecord is one replayable ledger row.
type CommandRecord struct {
	CommandType      string
	RequestDigest    string
	Outcome          string
	ResultObjectType string
	ResultObjectID   int64
	ResultPayload    string
}

// commandReader is the smallest SQLite read seam shared by *sql.DB and an
// already-open *sql.Conn. A command writer must recheck its ledger key through
// the same connection after BEGIN IMMEDIATE: another same-key request may have
// committed while this request was waiting to become the writer.
type commandReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// LookupCommand reads the persisted outcome for a command key before a command
// opens its write transaction (HTTP-COMMAND-003/007).
func LookupCommand(ctx context.Context, db *sql.DB, principalID int64, clientCommandID string) (CommandRecord, bool, error) {
	return lookupCommand(ctx, db, principalID, clientCommandID)
}

// LookupCommandOn reads the same key through an open transaction connection.
// Writers use it after BEGIN IMMEDIATE to make concurrent same-key replay
// deterministic rather than relying on a stale pre-transaction lookup.
func LookupCommandOn(ctx context.Context, conn *sql.Conn, principalID int64, clientCommandID string) (CommandRecord, bool, error) {
	return lookupCommand(ctx, conn, principalID, clientCommandID)
}

func lookupCommand(ctx context.Context, reader commandReader, principalID int64, clientCommandID string) (CommandRecord, bool, error) {
	row := reader.QueryRowContext(ctx, `
		SELECT command_type, request_digest, outcome, result_object_type, COALESCE(result_object_id, 0), COALESCE(result_payload_json, '')
		FROM client_commands
		WHERE principal_type='user' AND principal_id=? AND client_command_id=?`, principalID, clientCommandID)
	var record CommandRecord
	var objectType, payload sql.NullString
	var objectID sql.NullInt64
	if err := row.Scan(&record.CommandType, &record.RequestDigest, &record.Outcome, &objectType, &objectID, &payload); err != nil {
		if err == sql.ErrNoRows {
			return CommandRecord{}, false, nil
		}
		return CommandRecord{}, false, fmt.Errorf("read command ledger: %w", err)
	}
	record.ResultObjectType = objectType.String
	record.ResultObjectID = objectID.Int64
	record.ResultPayload = payload.String
	return record, true, nil
}

// RecordCommand inserts the ledger row inside the caller's open IMMEDIATE
// transaction so the command outcome commits atomically with the domain write
// and its audit event (DATA-AUDIT-004/005).
func RecordCommand(ctx context.Context, conn *sql.Conn, principalID int64, clientCommandID, commandType, digest, outcome, objectType string, objectID int64, payload string) error {
	var objectTypeArg, payloadArg any
	if objectType != "" {
		objectTypeArg = objectType
	}
	if payload != "" {
		payloadArg = payload
	}
	_, err := conn.ExecContext(ctx, `INSERT INTO client_commands(principal_type,principal_id,client_command_id,command_type,request_digest,outcome,result_object_type,result_object_id,result_payload_json,created_at) VALUES('user',?,?,?,?,?,?,?,?,?)`,
		principalID, clientCommandID, commandType, digest, outcome, objectTypeArg, objectID, payloadArg, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// DigestCommand builds the non-secret canonical request digest: the command
// type plus the semantic fields, JSON-serialized with sorted keys, hashed to
// lowercase hex (DATA-COMMAND-002). Callers must never pass secret values in
// fields.
func DigestCommand(commandType string, fields map[string]any) string {
	body, err := json.Marshal(fields)
	if err != nil {
		// map[string]any with comparable scalar values never fails to
		// marshal; fall back to a stable error marker rather than panicking.
		body = []byte(`{"digest_error":true}`)
	}
	sum := sha256.Sum256(append([]byte(commandType+"\n"), body...))
	return hex.EncodeToString(sum[:])
}
