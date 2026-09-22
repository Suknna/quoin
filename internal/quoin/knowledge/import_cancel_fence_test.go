package knowledge

// import_cancel_fence_test.go 回归 2026-09-21 实机验收：取消导入批次后，其
// 候选在数据层保持 AwaitingConfirmation（围栏在批次状态，不在候选状态），
// 所有写路径必须确定性拒绝，且读投影必须把批次终态带给 UI——否则页面会
// 为已冻结的候选渲染可勾选、可编辑的入口，与"本批候选将不能再编辑或确认"
// 的取消承诺相矛盾。

import (
	"errors"
	"testing"
	"time"
)

// seedAwaitingBatchWithCandidates 走生产 StartImport 建批次，再按已提交抽取
// 结果的形态落两条候选并把批次推到 AwaitingConfirmation（与既有批次测试的
// 播种方式一致），返回批次 id 与候选 id。
func seedAwaitingBatchWithCandidates(t *testing.T, f *fixture, commandID string, titles ...string) (int64, []int64) {
	t.Helper()
	ctx := f.ctx(t)
	started, err := f.service.StartImport(ctx, f.userID, commandID, "取消边界回归导入原文。")
	if err != nil {
		t.Fatal(err)
	}
	batchID := parseID(t, started.Batch.ID)
	if _, err := f.db.Exec(`UPDATE knowledge_import_batches SET state='AwaitingConfirmation',row_version=row_version+1 WHERE id=?`, batchID); err != nil {
		t.Fatal(err)
	}
	candidateIDs := make([]int64, 0, len(titles))
	for _, title := range titles {
		result, insertErr := f.db.Exec(`INSERT INTO knowledge_candidates(import_batch_id,source_type,source_id,generation,state,original_suggestion_json,draft_title,draft_body,draft_revision,created_by,created_at)
			VALUES(?, 'source_material', (SELECT source_material_id FROM knowledge_import_batches WHERE id=?), 1, 'AwaitingConfirmation', '{"v":1}', ?, ?, 0, ?, ?)`,
			batchID, batchID, title, title+"正文", f.userID, time.Now().UTC().Format(time.RFC3339Nano))
		if insertErr != nil {
			t.Fatal(insertErr)
		}
		candidateID, lastErr := result.LastInsertId()
		if lastErr != nil {
			t.Fatal(lastErr)
		}
		candidateIDs = append(candidateIDs, candidateID)
	}
	return batchID, candidateIDs
}

// TestCancelledBatchFreezesItsCandidates 证明后端边界：批次取消后单条
// Confirm/Exclude/EditDraft 与整批 ConfirmBatch 全部确定性拒绝，候选与知识
// 库保持原状（无 reusable_knowledge 产生、候选仍 AwaitingConfirmation）。
func TestCancelledBatchFreezesItsCandidates(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx(t)
	batchID, candidateIDs := seedAwaitingBatchWithCandidates(t, f, "cmd-cancel-fence-0", "取消边界第一条", "取消边界第二条")
	detail, err := f.service.GetImportBatch(ctx, batchID)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, dispatchID, err := f.service.CancelBatch(ctx, f.userID, "cmd-cancel-fence-1", batchID, detail.RowVersion)
	if err != nil || dispatchID != 0 || cancelled.State != "Cancelled" {
		t.Fatalf("cancel = %+v dispatch=%d err=%v", cancelled, dispatchID, err)
	}

	first := candidateIDs[0]
	assertBatchConflict := func(operation string, err error) {
		t.Helper()
		var conflict *StateConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("%s on cancelled batch error = %v, want StateConflict", operation, err)
		}
		if conflict.State != "Cancelled" {
			t.Fatalf("%s conflict state = %q, want Cancelled", operation, conflict.State)
		}
	}
	assertBatchConflict("confirm", func() error {
		_, err := f.service.Confirm(ctx, f.userID, "cmd-cancel-fence-confirm", first, 0)
		return err
	}())
	assertBatchConflict("exclude", func() error {
		_, err := f.service.Exclude(ctx, f.userID, "cmd-cancel-fence-exclude", first, 1)
		return err
	}())
	title := "取消后不可写入的草稿标题"
	assertBatchConflict("edit", func() error {
		_, err := f.service.EditDraft(ctx, f.userID, "cmd-cancel-fence-edit", first, 0, &title, nil, nil)
		return err
	}())
	assertBatchConflict("batch confirm", func() error {
		_, err := f.service.ConfirmBatch(ctx, f.userID, "cmd-cancel-fence-batch", batchID, []BatchConfirmation{
			{CandidateID: first, ExpectedRevision: 0},
			{CandidateID: candidateIDs[1], ExpectedRevision: 0},
		})
		return err
	}())

	// 数据面：没有任何知识被创建，候选原样冻结在待确认。
	var knowledge int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM reusable_knowledge`).Scan(&knowledge); err != nil {
		t.Fatal(err)
	}
	if knowledge != 0 {
		t.Fatalf("cancelled batch published knowledge: %d aggregates", knowledge)
	}
	var versions int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM knowledge_versions`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 0 {
		t.Fatalf("cancelled batch created versions: %d", versions)
	}
	for _, candidateID := range candidateIDs {
		var state string
		var draftTitle string
		if err := f.db.QueryRow(`SELECT state,COALESCE(draft_title,'') FROM knowledge_candidates WHERE id=?`, candidateID).Scan(&state, &draftTitle); err != nil {
			t.Fatal(err)
		}
		if state != StateAwaiting {
			t.Fatalf("candidate %d state = %q, want unchanged %q", candidateID, state, StateAwaiting)
		}
		if draftTitle == "取消后不可写入的草稿标题" {
			t.Fatalf("candidate %d accepted a draft edit after batch cancellation", candidateID)
		}
	}
}

// TestCandidateProjectionsCarryBatchState 证明读模型把批次围栏带给 UI：
// 详情与列表投影中，取消批次的候选携带 batchState=Cancelled，非批次候选为空。
func TestCandidateProjectionsCarryBatchState(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx(t)
	batchID, candidateIDs := seedAwaitingBatchWithCandidates(t, f, "cmd-cancel-proj-0", "投影边界候选")
	detail, err := f.service.GetImportBatch(ctx, batchID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.service.CancelBatch(ctx, f.userID, "cmd-cancel-proj-1", batchID, detail.RowVersion); err != nil {
		t.Fatal(err)
	}

	got, err := f.service.GetCandidate(ctx, candidateIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.BatchState != "Cancelled" {
		t.Fatalf("candidate detail batchState = %q, want Cancelled", got.BatchState)
	}
	items, _, err := f.service.ListCandidates(ctx, ListFilter{State: StateAwaiting}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		if item.ID != got.ID {
			continue
		}
		found = true
		if item.BatchState != "Cancelled" {
			t.Fatalf("candidate list batchState = %q, want Cancelled", item.BatchState)
		}
	}
	if !found {
		t.Fatal("awaiting list does not contain the cancelled-batch candidate")
	}

	// 非批次候选（诊断来源）没有批次围栏：batchState 必须为空。
	diagnosis := confirmFromSource(t, f, "proj-diagnosis", false)
	_ = diagnosis
	if items, _, err = f.service.ListCandidates(ctx, ListFilter{}, nil, 50); err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.SourceType != "source_material" && item.BatchState != "" {
			t.Fatalf("non-batch candidate %s carries batchState %q", item.ID, item.BatchState)
		}
	}
}
