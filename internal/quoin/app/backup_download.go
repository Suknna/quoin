package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/backup"
)

func (application *apiServer) downloadBackup(writer http.ResponseWriter, request *http.Request) {
	id, err := strconv.ParseInt(request.PathValue("backupId"), 10, 64)
	if err != nil || id < 1 {
		writeBackupProblem(writer, http.StatusNotFound, "not_found", "备份记录不存在", false)
		return
	}
	cookie, ok := findSessionCookie(request)
	if !ok {
		writeBackupProblem(writer, http.StatusUnauthorized, "unauthorized", "请重新登录", false)
		return
	}
	session, err := application.authorizeBackup(request.Context(), cookie, "下载备份归档")
	if err != nil {
		writeBackupProblemError(writer, err)
		return
	}
	service, err := application.backupService()
	if err != nil {
		writeBackupProblem(writer, http.StatusServiceUnavailable, "unavailable", "备份服务暂不可用", true)
		return
	}
	archive, cleanup, err := service.PrepareArchive(request.Context(), id)
	if errors.Is(err, backup.ErrNotFound) || errors.Is(err, backup.ErrArchiveNotReady) {
		writeBackupProblem(writer, http.StatusNotFound, "not_found", "备份归档尚不可下载", false)
		return
	}
	if errors.Is(err, backup.ErrArchiveUnavailable) {
		writeBackupProblem(writer, http.StatusGone, "backup_archive_unavailable", "备份归档不可用", false)
		return
	}
	var storageFailure *backup.StorageFailure
	if errors.As(err, &storageFailure) {
		writeBackupProblem(writer, http.StatusServiceUnavailable, "storage_unavailable", "备份归档存储暂不可用，请稍后重试。", true)
		return
	}
	if err != nil {
		writeBackupProblem(writer, http.StatusServiceUnavailable, "storage_unavailable", "备份归档存储暂不可用，请稍后重试。", true)
		return
	}
	defer cleanup()
	if err = service.RecordDownloadStart(request.Context(), session.User.ID, id); err != nil {
		writeBackupProblem(writer, http.StatusServiceUnavailable, "unavailable", "下载审计失败，请重试", true)
		return
	}
	info, statErr := archive.Stat()
	if statErr != nil {
		writeBackupProblem(writer, http.StatusServiceUnavailable, "storage_unavailable", "备份归档存储暂不可用，请稍后重试。", true)
		return
	}
	// A backup archive can legitimately outlive the public server's 30-second
	// WriteTimeout. Limit this exception to the already-authenticated streaming
	// response; authorizedBackupReader rechecks the current session before every
	// bounded chunk, so it cannot turn into an unrevocable long-lived transfer.
	if err := http.NewResponseController(writer).SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		writeBackupProblem(writer, http.StatusServiceUnavailable, "unavailable", "无法初始化备份归档传输。", true)
		return
	}
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Disposition", "attachment; filename=backup-"+strconv.FormatInt(id, 10)+".tar")
	writer.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Pragma", "no-cache")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	writer.WriteHeader(http.StatusOK)
	copyArchive := application.backupCopy
	if copyArchive == nil {
		copyArchive = io.Copy
	}
	authorized := &authorizedBackupReader{reader: archive, check: func() error {
		_, checkErr := application.authorizeBackup(request.Context(), cookie, "继续下载备份归档")
		return checkErr
	}}
	_, transferErr := copyArchive(writer, authorized)
	if transferErr != nil {
		sharedops.LogEvent("quoin", "error", "backup.download_stream_failed", "backup="+strconv.FormatInt(id, 10)+" "+transferErr.Error())
	}
	if auditErr := service.RecordDownloadCompletion(context.Background(), session.User.ID, id, transferErr); auditErr != nil {
		sharedops.LogEvent("quoin", "error", "backup.download_completion_audit_failed", "backup="+strconv.FormatInt(id, 10)+" "+auditErr.Error())
	}
}

// authorizedBackupReader caps each read so session revocation is observed before
// more than one bounded chunk can pass an authorization fence.
type authorizedBackupReader struct {
	reader io.Reader
	check  func() error
}

func (reader *authorizedBackupReader) Read(buffer []byte) (int, error) {
	if err := reader.check(); err != nil {
		return 0, err
	}
	if len(buffer) > 32*1024 {
		buffer = buffer[:32*1024]
	}
	return reader.reader.Read(buffer)
}
