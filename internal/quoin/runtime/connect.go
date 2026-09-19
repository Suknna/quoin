package runtime

import (
	"context"
	"time"

	"github.com/Suknna/quoin/internal/contract"
)

// HelloDecision is the adjudication result of a Connect handshake.
type HelloDecision struct {
	Accepted            bool
	Reason              string // empty when accepted; else CONTRACT_MISMATCH | EPOCH_STALE
	LastConnectionEpoch uint64
}

// Adjudicate validates the Hello fields (RUNTIME-CTRL-002..004). The
// client identity itself was already proven by the mTLS handshake (ADR-0009):
// the caller verified the peer certificate's CN names this slot.
func (service *Service) Adjudicate(_ context.Context, slotName, bootID string, epoch uint64, contractFingerprint, currentFingerprint string) (HelloDecision, error) {
	decision := HelloDecision{}
	if !validContractFingerprint(contractFingerprint) || contractFingerprint != currentFingerprint {
		decision.Reason = "CONTRACT_MISMATCH"
		return decision, nil
	}
	// Epoch monotonicity inside (slot, boot): new boots may restart at 1.
	service.mu.Lock()
	lastEpoch := service.bootEpochs[slotName+"\x00"+bootID]
	service.mu.Unlock()
	decision.LastConnectionEpoch = lastEpoch
	if epoch <= lastEpoch {
		decision.Reason = "EPOCH_STALE"
		return decision, nil
	}
	decision.Accepted = true
	return decision, nil
}

// validContractFingerprint accepts only the canonical, complete Proto
// authority fingerprint. Empty and malformed peer values are rejected.
func validContractFingerprint(value string) bool {
	return contract.ValidProtoAuthorityFingerprint(value)
}

// AttachStream records an accepted control stream as the slot's single active
// connection. Adjudicate and attachment are intentionally separate because two
// admitted Hellos can reach this boundary out of order. A stale attachment
// returns nil and, crucially, leaves the current owner open (RUNTIME-CTRL-001/004).
func (service *Service) AttachStream(slotName, bootID string, epoch uint64) <-chan struct{} {
	closing, _ := service.attachStream(slotName, bootID, epoch, "", nil)
	return closing
}

// Touch updates the transient lastSeen projection (Heartbeat) without any
// persistent write (RUNTIME-CTRL-005).
// WithCurrent serializes a stateful inbound message with control-stream
// replacement. The callback may commit durable state only while this exact
// boot/epoch remains the slot authority; a replacement either happens before
// the callback (and it is rejected) or after its transaction commits.
func (service *Service) WithCurrent(slotName, bootID string, epoch uint64, apply func() error) error {
	return service.WithCurrentClosing(slotName, bootID, epoch, func(_ <-chan struct{}) error {
		return apply()
	})
}

// WithCurrentClosing is the data-plane counterpart of WithCurrent. The
// callback can atomically bind transient state to the exact control owner;
// its returned fence closes when that owner is replaced or detached.
func (service *Service) WithCurrentClosing(slotName, bootID string, epoch uint64, apply func(closing <-chan struct{}) error) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	conn, live := service.conns[slotName]
	if !live || conn.bootID != bootID || conn.epoch != epoch {
		return ErrNotConnected
	}
	return apply(conn.closing)
}

func (service *Service) Touch(slotName string) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if conn, live := service.conns[slotName]; live {
		conn.updated = time.Now()
	}
}

// DetachStream removes the projection only when the ending handler still owns
// it. A superseded stream must never erase its successor's live authority.
func (service *Service) DetachStream(slotName, bootID string, epoch uint64) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if conn, live := service.conns[slotName]; live && conn.bootID == bootID && conn.epoch == epoch {
		conn.close()
		delete(service.conns, slotName)
	}
}

// CloseSlot signals the slot's live control stream to end. Idempotent.
func (service *Service) CloseSlot(slotName string) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if conn, live := service.conns[slotName]; live {
		conn.close()
	}
}

// CloseAll signals every live control stream to end (shutdown).
func (service *Service) CloseAll() {
	service.mu.Lock()
	defer service.mu.Unlock()
	for _, conn := range service.conns {
		conn.close()
	}
}
