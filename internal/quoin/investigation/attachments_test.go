package investigation

// Attachment domain tests (T14): staging validation (size/UTF-8/NUL),
// command replay, message combinations (text-only, attachment-only,
// multi-attachment, empty rejection, duplicate rejection, aggregate limit),
// input snapshot determinism with attachments, the input_snapshot read
// grants the frozen closure requires, and attachment reuse across turns
// without re-upload (DATA-ATTACH-001, HTTP-FILE-001/002).

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/artifact"
)

func newAttachmentService(t *testing.T) (*Service, *sql.DB, int64, *artifact.Store) {
	t.Helper()
	db := newTestDB(t)
	principal := seedUser(t, db)
	seedProviderChain(t, db)
	store, err := artifact.NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(db)
	service.SetAttachmentStore(store, 0)
	return service, db, principal, store
}

func mustStage(t *testing.T, service *Service, principal int64, command, filename, body string) AttachmentView {
	t.Helper()
	view, err := service.StageAttachment(context.Background(), principal, command, filename, strings.NewReader(body))
	if err != nil {
		t.Fatalf("stage %s: %v", command, err)
	}
	return view
}

func TestAttachmentStagingValidatesBody(t *testing.T) {
	service, _, principal, _ := newAttachmentService(t)
	ctx := context.Background()
	if _, err := service.StageAttachment(ctx, principal, "stage-nul-0001", "nul.txt", strings.NewReader("a\x00b")); !errors.Is(err, ErrAttachmentText) {
		t.Fatalf("NUL body must reject with ErrAttachmentText: %v", err)
	}
	if _, err := service.StageAttachment(ctx, principal, "stage-badutf-0001", "bad.txt", strings.NewReader("a\xffb")); !errors.Is(err, ErrAttachmentText) {
		t.Fatalf("invalid UTF-8 must reject: %v", err)
	}
	// A multi-byte rune split across reads still validates (the streaming
	// validator pairs chunk tails).
	split := "调查日志" + strings.Repeat("x", 64*1024) + "结束"
	view := mustStage(t, service, principal, "stage-split-0001", "split.txt", split)
	if view.MediaType != "text/plain" || view.SizeBytes != int64(len(split)) {
		t.Fatalf("view wrong: %+v", view)
	}
	if view.OriginalFilename != "split.txt" {
		t.Fatalf("filename=%q", view.OriginalFilename)
	}
	// Oversized: the limit defaults to 10 MiB; a custom small limit proves
	// the boundary trips without a 10 MiB fixture.
	small, err := artifact.NewStore(service.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	smallService := NewService(service.db)
	smallService.SetAttachmentStore(small, 16)
	if _, err := smallService.StageAttachment(ctx, principal, "stage-big-00001", "big.txt", strings.NewReader(strings.Repeat("x", 17))); !errors.Is(err, ErrAttachmentTooLarge) {
		t.Fatalf("oversized body must reject: %v", err)
	}
}

func TestAttachmentStagingReplayAndReuse(t *testing.T) {
	service, _, principal, _ := newAttachmentService(t)
	ctx := context.Background()
	first := mustStage(t, service, principal, "stage-replay-0001", "logs.txt", "same body")
	replayed := mustStage(t, service, principal, "stage-replay-0001", "logs.txt", "same body")
	if first.ID != replayed.ID || first.ArtifactID != replayed.ArtifactID {
		t.Fatalf("replay diverged: %+v vs %+v", first, replayed)
	}
	// Same command id with a different body is a deterministic conflict.
	if _, err := service.StageAttachment(ctx, principal, "stage-replay-0001", "logs.txt", strings.NewReader("different")); !errors.Is(err, ErrCommandReused) {
		t.Fatalf("divergent replay must conflict: %v", err)
	}
	// Foreign principals never see another user's staging object.
	other := seedUser(t, service.db)
	if _, err := service.AttachmentFor(ctx, other, parseOrFatal(t, first.ID)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign read must be not found: %v", err)
	}
	if _, err := service.AttachmentFor(ctx, principal, parseOrFatal(t, first.ID)); err != nil {
		t.Fatalf("own read: %v", err)
	}
}

func parseOrFatal(t *testing.T, value string) int64 {
	t.Helper()
	parsed, err := ParseAttachmentIDs([]string{value})
	if err != nil {
		t.Fatal(err)
	}
	return parsed[0]
}

func TestAttachmentMessageCombinations(t *testing.T) {
	service, db, principal, _ := newAttachmentService(t)
	ctx := context.Background()
	first := mustStage(t, service, principal, "combo-stage-0001", "a.txt", "内容 A")
	second := mustStage(t, service, principal, "combo-stage-0002", "b.txt", "内容 B")

	// Attachment-only first turn creates atomically (DATA-INVEST-001).
	created, err := service.Create(ctx, principal, "combo-create-0001", "", []int64{parseOrFatal(t, first.ID)}, nil)
	if err != nil {
		t.Fatalf("attachment-only create: %v", err)
	}
	// Seal the first turn so later sends pass the one-active-attempt fence
	// (DATA-INVEST-003); the combination under test is the send, not the
	// fence (commit_test.go owns that).
	if err := bindRunning(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := seedModelCall(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	content := `"已阅读附件"`
	digest := sha256Sum([]byte(content))
	if err := service.CommitResult(ctx, Result{
		AttemptID: created.AttemptID, BootID: "boot-t", Epoch: 1, Succeeded: true,
		SchemaKind: OutputSchemaKind, Canonical: []byte(content), Digest: digest[:],
	}); err != nil {
		t.Fatal(err)
	}
	var head int64
	if err := db.QueryRow(`SELECT current_head_message_id FROM investigations WHERE id=?`, created.InvestigationID).Scan(&head); err != nil {
		t.Fatal(err)
	}
	// Empty-everything rejects.
	if _, err := service.Create(ctx, principal, "combo-blank-0001", " ", nil, nil); !errors.Is(err, ErrMessageInvalid) {
		t.Fatalf("blank must reject: %v", err)
	}
	// Duplicate ids reject (uniqueItems).
	dup := []int64{parseOrFatal(t, first.ID), parseOrFatal(t, first.ID)}
	if _, err := service.Send(ctx, principal, "combo-dup-00001", created.InvestigationID, int64Ptr(head), "重复", dup); !errors.Is(err, ErrAttachmentInvalidRef) {
		t.Fatalf("duplicate attachment must reject: %v", err)
	}
	// Unknown / foreign ids reject.
	if _, err := service.Send(ctx, principal, "combo-unknown-001", created.InvestigationID, int64Ptr(head), "未知", []int64{999999}); !errors.Is(err, ErrAttachmentInvalidRef) {
		t.Fatalf("unknown attachment must reject: %v", err)
	}
	// Multi-attachment send persists ordered references.
	_, err = service.Send(ctx, principal, "combo-multi-0001", created.InvestigationID, int64Ptr(head), "带两个附件", []int64{parseOrFatal(t, second.ID), parseOrFatal(t, first.ID)})
	if err != nil {
		t.Fatalf("multi send: %v", err)
	}
	items, _, err := service.ListMessages(ctx, created.InvestigationID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("messages=%d", len(items))
	}
	if len(items[0].Attachments) != 1 || items[0].Attachments[0].OriginalFilename != "a.txt" {
		t.Fatalf("first turn attachments wrong: %+v", items[0].Attachments)
	}
	if len(items[1].Attachments) != 0 || items[1].Role != "assistant" {
		t.Fatalf("assistant turn must carry no attachments: %+v", items[1])
	}
	if len(items[2].Attachments) != 2 || items[2].Attachments[0].OriginalFilename != "b.txt" || items[2].Attachments[1].OriginalFilename != "a.txt" {
		t.Fatalf("third turn attachments order wrong: %+v", items[2].Attachments)
	}
	// Attachment reuse: the same staging object referenced again without a
	// new upload (the artifact count stays at two).
	var artifactCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM text_attachments`).Scan(&artifactCount); err != nil {
		t.Fatal(err)
	}
	if artifactCount != 2 {
		t.Fatalf("staging objects=%d want 2", artifactCount)
	}
}

func TestAttachmentAggregateBoundary(t *testing.T) {
	service, _, principal, store := newAttachmentService(t)
	// A tiny boundary proves the aggregate rule without a 10 MiB fixture:
	// two 8-byte files pass individually, together they cross a 12-byte
	// boundary (both services share one content-addressed store).
	small := NewService(service.db)
	small.SetAttachmentStore(store, 12)
	ctx := context.Background()
	first := mustStage(t, small, principal, "agg-stage-000001", "one.txt", "12345678")
	second := mustStage(t, small, principal, "agg-stage-000002", "two.txt", "abcdefgh")
	if _, err := small.Create(ctx, principal, "agg-create-000001", "", []int64{parseOrFatal(t, first.ID), parseOrFatal(t, second.ID)}, nil); !errors.Is(err, ErrAttachmentTooLarge) {
		t.Fatalf("aggregate over boundary must reject: %v", err)
	}
	if _, err := small.Create(ctx, principal, "agg-create-000002", "只带一个", []int64{parseOrFatal(t, first.ID)}, nil); err != nil {
		t.Fatalf("single within boundary: %v", err)
	}
}

func TestAttachmentInputSnapshotAndGrants(t *testing.T) {
	service, db, principal, _ := newAttachmentService(t)
	ctx := context.Background()
	view := mustStage(t, service, principal, "snap-stage-000001", "logs.txt", "T14-ATTACHMENT-BODY")
	created, err := service.Create(ctx, principal, "snap-create-000001", "请阅读附件", []int64{parseOrFatal(t, view.ID)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The frozen input renders the attachment locator block deterministically.
	input, err := service.RebuildInput(ctx, created.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	body := string(input)
	if !strings.Contains(body, `"filename":"logs.txt"`) || !strings.Contains(body, `"artifactId":"`+view.ArtifactID+`"`) {
		t.Fatalf("input lacks attachment locators: %s", body)
	}
	if strings.Contains(body, "T14-ATTACHMENT-BODY") {
		t.Fatalf("input must never inline the attachment body: %s", body)
	}
	// The dispatch-facing artifact ref derives from the input_snapshot grant.
	dispatch, err := service.Attempts().DispatchInputFor(ctx, created.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dispatch.ArtifactRefs) != 1 || dispatch.ArtifactRefs[0].ArtifactID != parseOrFatal(t, view.ArtifactID) {
		t.Fatalf("dispatch artifact refs wrong: %+v", dispatch.ArtifactRefs)
	}
	// The lineage items carry both the message and the attachment artifact.
	var itemCount, artifactItems int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempt_input_items WHERE snapshot_id=(SELECT id FROM attempt_input_snapshots WHERE attempt_id=?)`, created.AttemptID).Scan(&itemCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempt_input_items WHERE snapshot_id=(SELECT id FROM attempt_input_snapshots WHERE attempt_id=?) AND artifact_id IS NOT NULL`, created.AttemptID).Scan(&artifactItems); err != nil {
		t.Fatal(err)
	}
	if itemCount != 2 || artifactItems != 1 {
		t.Fatalf("items=%d artifactItems=%d want 2/1", itemCount, artifactItems)
	}
	// A rebuild is byte-stable (dispatch verifies the digest; this pins it).
	again, err := service.RebuildInput(ctx, created.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != body {
		t.Fatalf("input rebuild is not deterministic")
	}
}
