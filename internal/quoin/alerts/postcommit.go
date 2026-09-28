package alerts

// Post-commit fact emission for the alert intake authority transaction
// (ADR-0014, issue #110): every committed normalized observation becomes one
// bounded fact of the finite vocabulary, persisted in the SAME runner
// transaction as the observation itself — same commit, same rollback. The
// fact carries stable versioned references and closed labels only; never the
// raw body, labels snapshot, annotations or any credential material.

import (
	"context"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/pluginevents"
)

// emitObservationFact persists one alert.observation.committed fact on the
// open authority transaction. Without enabled subscribers the publisher is a
// row-free no-op; a bounded-shape violation fails the transaction loudly —
// an unbounded fact must never ride a committed delivery.
func (service *Service) emitObservationFact(ctx context.Context, tx execution.Executor, commit observationCommit) (int64, error) {
	return service.publisher.Emit(ctx, tx, pluginevents.Fact{
		Type: plugins.FactAlertObservationCommitted,
		Refs: []plugins.PostCommitFactRef{
			{Name: "sourceId", ID: commit.sourceID},
			{Name: "deliveryId", ID: commit.deliveryID},
			{Name: "deliveryItemId", ID: commit.deliveryItemID},
			{Name: "occurrenceId", ID: commit.occurrenceID, Version: commit.rowVersion},
			{Name: "observationId", ID: commit.observationID},
		},
		Labels: map[string]string{
			"state":     commit.state,
			"effect":    commit.effect,
			"sourceKey": commit.sourceKey,
		},
	})
}
