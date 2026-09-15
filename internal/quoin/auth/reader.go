package auth

// Authentication reads use the trusted read-only capability installed at
// startup. Mutation authority stays with the execution runner. Reconfiguration
// after serving starts is unsupported.

import (
	"errors"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// SetReader installs a live capability created by execution.OpenReadOnly.
// Until it is installed, every pure read fails closed.
func (service *Service) SetReader(reader audit.Reader) error {
	service.authMu.Lock()
	defer service.authMu.Unlock()
	if err := service.runner.SetReader(reader); err != nil {
		return err
	}
	service.reader = service.runner.Reader()
	return nil
}

// ErrReaderNotWired marks an authentication read issued before the runtime
// installed the read-only pool.
var ErrReaderNotWired = errors.New("auth: read-only pool is not installed")

// read returns the configured read-only source. Without one it returns the
// zero-value execution.Reader: query-only by type and fail-closed unwired —
// the writer database is never a read fallback.
func (service *Service) read() audit.Reader {
	service.authMu.RLock()
	defer service.authMu.RUnlock()
	if service.reader != nil {
		return service.reader
	}
	return execution.Reader{}
}

// readPoolWired reports whether a read-only pool was installed; unwired
// callers translate dead-reader failures into the typed ErrReaderNotWired
// (503 class, never a 401).
func (service *Service) readPoolWired() bool {
	service.authMu.RLock()
	defer service.authMu.RUnlock()
	return service.reader != nil
}
