package inspection

// Post-commit fact emission for the inspection authority transactions
// (ADR-0014, issue #110): the daily window due, a committed check result and
// a sealed daily report each become one bounded fact of the finite
// vocabulary inside the SAME runner transaction that committed the domain
// row. Facts carry stable versioned references and closed labels only —
// never report content, credentials or unbounded bodies.

import (
	"context"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/pluginevents"
)

// SetPostCommitPublisher installs the post-commit fact publisher (ADR-0014).
// The publisher's subscriber index is the frozen assembly plus the resolved
// deployment enablement — the same inputs the dispatcher consumes.
func (s *Service) SetPostCommitPublisher(publisher *pluginevents.Publisher) {
	s.publisher = publisher
}

// notifyPostCommit wakes the dispatcher after the authority commit returned.
// Nil-safe: unwired assemblies never signal.
func (s *Service) notifyPostCommit() { s.publisher.Notify() }

// emitDailyWindowDueFact persists one inspection.daily_window_due fact for
// the freshly created Collecting report (the frozen window's trigger
// boundary committed).
func (s *Service) emitDailyWindowDueFact(ctx context.Context, tx execution.Executor, configID, configRowVersion, reportID int64, localDate, windowStartUTC, windowEndUTC string) error {
	_, err := s.publisher.Emit(ctx, tx, pluginevents.Fact{
		Type: plugins.FactInspectionDailyWindowDue,
		Refs: []plugins.PostCommitFactRef{
			{Name: "configId", ID: configID, Version: configRowVersion},
			{Name: "reportId", ID: reportID, Version: 1},
		},
		Labels: map[string]string{
			"localDate":  localDate,
			"windowStart": windowStartUTC,
			"windowEnd":  windowEndUTC,
		},
	})
	return err
}

// emitCheckEvidenceFact persists one inspection.check_evidence.committed fact
// for one committed check result. The evidence reference exists only when the
// check actually captured evidence — a gap row states its gap, never a
// fabricated evidence id.
func (s *Service) emitCheckEvidenceFact(ctx context.Context, tx execution.Executor, runID, checkResultID, attemptID, evidenceID int64, checkKey, status string) error {
	refs := []plugins.PostCommitFactRef{
		{Name: "runId", ID: runID},
		{Name: "checkResultId", ID: checkResultID},
		{Name: "attemptId", ID: attemptID},
	}
	if evidenceID > 0 {
		refs = append(refs, plugins.PostCommitFactRef{Name: "evidenceId", ID: evidenceID})
	}
	_, err := s.publisher.Emit(ctx, tx, pluginevents.Fact{
		Type: plugins.FactInspectionCheckEvidenceCommitted,
		Refs: refs,
		Labels: map[string]string{
			"checkKey": checkKey,
			"status":   status,
		},
	})
	return err
}

// emitReportSealedFact persists one inspection.report.sealed fact for the
// immutable sealed version committed by the seal transaction.
func (s *Service) emitReportSealedFact(ctx context.Context, tx execution.Executor, reportID, versionRowID int64, configKey, localDate, sealedAt string) error {
	_, err := s.publisher.Emit(ctx, tx, pluginevents.Fact{
		Type: plugins.FactInspectionReportSealed,
		Refs: []plugins.PostCommitFactRef{
			{Name: "reportId", ID: reportID},
			{Name: "versionId", ID: versionRowID},
		},
		Labels: map[string]string{
			"configKey": configKey,
			"localDate": localDate,
			"sealedAt":  sealedAt,
		},
	})
	return err
}
