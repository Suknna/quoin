package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
)

// uploadExplorationTrace makes the Stop acknowledgement truthful: every trace
// sent by Lintel has a concrete, operation-scoped local staging file that Stop
// subsequently removes. The file is never an authority; it exists only until
// Quoin accepts the content-addressed Artifact upload and sends Stop.
func (channel *Channel) uploadExplorationTrace(ctx context.Context, operationID, childAttemptID int64, body, digest []byte, integrity runtimev1.BrowserTraceIntegrity) (int64, error) {
	if err := channel.stageExplorationTrace(operationID, body); err != nil {
		return 0, err
	}
	return channel.UploadBrowserArtifact(ctx, BrowserArtifactUpload{
		OperationID: operationID, ChildAttemptID: childAttemptID,
		Kind: runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE, Body: body, SHA256: digest,
		MediaType: "application/json", Sensitive: true,
		TraceIntegrity: integrity,
	})
}

// cleanupExplorationTraceStaging is the boot boundary for this non-authority
// cache. A Lintel restart cannot replay a trace or prove a former operation;
// retaining its .part files would therefore leak sensitive metadata and make a
// later StopAck falsely appear to clean this boot's work.
func cleanupExplorationTraceStaging(stateDirectory string) error {
	if stateDirectory == "" {
		return nil
	}
	directory := filepath.Join(stateDirectory, "browser-trace-staging")
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read trace staging directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !isExplorationTraceStagingName(entry.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil {
			return fmt.Errorf("remove prior-boot trace staging file: %w", err)
		}
	}
	return nil
}

// isExplorationTraceStagingName recognizes both the current append-only .part
// files and the pre-v1 staging name used before the append log existed. A new
// boot cannot safely resume either cache, so it removes only this strict
// operation-ID namespace while preserving unrelated state-volume files.
func isExplorationTraceStagingName(name string) bool {
	base := strings.TrimSuffix(name, ".part")
	if !strings.HasSuffix(base, ".json") {
		return false
	}
	operationID := strings.TrimSuffix(base, ".json")
	id, err := strconv.ParseInt(operationID, 10, 64)
	return err == nil && id > 0
}

// appendExplorationTraceEntry persists each metadata entry at the action
// boundary. The terminal artifact remains the canonical sealed JSON body, but
// this append-only staging log prevents a process loss between actions from
// silently erasing the trace accumulated so far.
func (channel *Channel) appendExplorationTraceEntry(operationID int64, body []byte) error {
	return channel.appendTraceStaging(operationID, body)
}

func (channel *Channel) stageExplorationTrace(operationID int64, body []byte) error {
	return channel.appendTraceStaging(operationID, body)
}

func (channel *Channel) appendTraceStaging(operationID int64, body []byte) error {
	if operationID < 1 || channel.Config.StateDirectory == "" {
		return fmt.Errorf("trace staging requires operation id and state directory")
	}
	directory := filepath.Join(channel.Config.StateDirectory, "browser-trace-staging")
	path := filepath.Join(directory, fmt.Sprintf("%d.json.part", operationID))
	// Register the exact cleanup obligation before any filesystem side effect.
	// A failed create/write/sync can leave a sensitive partial file behind; Stop
	// must then fail rather than falsely acknowledge trace cleanup because the
	// old implementation only registered paths after a successful close.
	channel.traceStagingMu.Lock()
	if channel.traceStaging == nil {
		channel.traceStaging = make(map[int64]string)
	}
	channel.traceStaging[operationID] = path
	channel.traceStagingMu.Unlock()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create trace staging directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open trace staging file: %w", err)
	}
	if _, err := file.Write(append(append([]byte(nil), body...), '\n')); err != nil {
		_ = file.Close()
		return fmt.Errorf("append trace staging file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync trace staging file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close trace staging file: %w", err)
	}
	return nil
}

// deleteTraceStaging removes only a file actually staged by this process. A
// missing file is an error because claiming cleanup after losing the staging
// path would turn a cleanup fact into an assertion.
func (channel *Channel) deleteTraceStaging(operationID int64) error {
	channel.traceStagingMu.RLock()
	path := channel.traceStaging[operationID]
	channel.traceStagingMu.RUnlock()
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil {
		// A registered path is a cleanup obligation, including failed staging
		// attempts. Missing content therefore cannot be reported as a successful
		// Stop cleanup: it is evidence Lintel cannot prove the staging boundary.
		return fmt.Errorf("remove trace staging file: %w", err)
	}
	channel.traceStagingMu.Lock()
	delete(channel.traceStaging, operationID)
	channel.traceStagingMu.Unlock()
	return nil
}
