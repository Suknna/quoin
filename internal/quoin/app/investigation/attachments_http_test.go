package appinvestigation

// Raw multipart upload handler tests (T14): streaming field order both
// ways (command before and after the file), the frozen status codes
// (415/422/413/401/409) and the 201 summary shape over the real handler
// seam (the staging service runs on the real frozen schema).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/investigation"
	_ "modernc.org/sqlite"
)

type uploadHarness struct {
	handler   *Handler
	principal int64
}

// The handler needs the real service over the frozen schema; the
// investigation package owns its own harness, so this test wires a minimal
// one through the exported surface.
func TestUploadAttachmentHandler(t *testing.T) {
	db, service, principal, store := uploadService(t)
	handler := &Handler{
		Service: service,
		Authenticate: func(ctx context.Context, cookie string) (int64, error) {
			if cookie == "valid" {
				return principal, nil
			}
			return 0, investigation.ErrNotFound
		},
	}
	_ = db

	post := func(body string, contentType string, cookie string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/investigation-attachments", strings.NewReader(body))
		if contentType != "" {
			request.Header.Set("Content-Type", contentType)
		}
		request.Header.Set("Cookie", "__Host-quoin-session="+cookie)
		response := httptest.NewRecorder()
		handler.ServeUpload(response, request)
		return response
	}

	multipartBody := func(commandFirst bool, command, filename, content string) (string, string) {
		var buffer bytes.Buffer
		writer := multipart.NewWriter(&buffer)
		writeCommand := func() {
			field, _ := writer.CreateFormField("clientCommandId")
			io.WriteString(field, command)
		}
		writeFile := func() {
			file, _ := writer.CreateFormFile("file", filename)
			io.WriteString(file, content)
		}
		if commandFirst {
			writeCommand()
			writeFile()
		} else {
			writeFile()
			writeCommand()
		}
		writer.Close()
		return buffer.String(), writer.FormDataContentType()
	}

	// Command field after the file still works (multipart order is
	// client-controlled; the body streams into staging immediately).
	body, contentType := multipartBody(false, "upload-cmd-000001", "logs.txt", "附件正文内容")
	response := post(body, contentType, "valid")
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		ID               string `json:"id"`
		ArtifactID       string `json:"artifactId"`
		OriginalFilename string `json:"originalFilename"`
		MediaType        string `json:"mediaType"`
		SizeBytes        int64  `json:"sizeBytes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.OriginalFilename != "logs.txt" || created.MediaType != "text/plain" || created.SizeBytes != int64(len("附件正文内容")) {
		t.Fatalf("summary wrong: %+v", created)
	}

	// Same command, same content: replay returns the original object.
	body, contentType = multipartBody(true, "upload-cmd-000001", "logs.txt", "附件正文内容")
	response = post(body, contentType, "valid")
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), created.ID) {
		t.Fatalf("replay status=%d body=%s", response.Code, response.Body.String())
	}

	// Same command, different content: deterministic conflict.
	body, contentType = multipartBody(true, "upload-cmd-000001", "logs.txt", "不同内容")
	response = post(body, contentType, "valid")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "command_id_reused") {
		t.Fatalf("conflict status=%d body=%s", response.Code, response.Body.String())
	}

	// NUL body: 422 with ordinary language.
	var nulBuffer bytes.Buffer
	nulWriter := multipart.NewWriter(&nulBuffer)
	nulCommand, _ := nulWriter.CreateFormField("clientCommandId")
	io.WriteString(nulCommand, "upload-cmd-000002")
	nulFile, _ := nulWriter.CreateFormFile("file", "nul.txt")
	nulFile.Write([]byte("a\x00b"))
	nulWriter.Close()
	response = post(nulBuffer.String(), nulWriter.FormDataContentType(), "valid")
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "UTF-8") {
		t.Fatalf("NUL status=%d body=%s", response.Code, response.Body.String())
	}

	// Non-multipart content type: 415.
	response = post("plain", "application/json", "valid")
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("media status=%d", response.Code)
	}

	// No session: 401.
	body, contentType = multipartBody(true, "upload-cmd-000003", "x.txt", "内容")
	response = post(body, contentType, "")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("auth status=%d", response.Code)
	}

	// Invalid session: 401.
	response = post(body, contentType, "invalid")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("session status=%d", response.Code)
	}

	// Missing command id: 422.
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	file, _ := writer.CreateFormFile("file", "no-command.txt")
	io.WriteString(file, "正文")
	writer.Close()
	response = post(buffer.String(), writer.FormDataContentType(), "valid")
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "clientCommandId") {
		t.Fatalf("no-command status=%d body=%s", response.Code, response.Body.String())
	}

	// Oversized body: 413 (a tiny wired boundary keeps the fixture small).
	service.SetAttachmentStore(store, 8)
	body, contentType = multipartBody(true, "upload-cmd-000004", "big.txt", "超过边界的正文")
	response = post(body, contentType, "valid")
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), "payload_too_large") {
		t.Fatalf("oversize status=%d body=%s", response.Code, response.Body.String())
	}
}

func uploadService(t *testing.T) (*sql.DB, *investigation.Service, int64, *artifact.Store) {
	t.Helper()
	db := uploadSchema(t)
	principal := uploadSeedUser(t, db)
	store, err := artifact.NewStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := investigation.NewService(db)
	service.SetAttachmentStore(store, 0)
	return db, service, principal, store
}

func uploadSchema(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/test.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	return db
}

func uploadSeedUser(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	insert, err := db.Exec(`INSERT INTO users(username,display_name,role,enabled,password_phc,auth_revision,created_at,updated_at) VALUES(?,'Upload Admin','admin',1,'x',1,?,?)`,
		"upload-admin-"+now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := insert.LastInsertId()
	return id
}
