package exploration

import (
	"crypto/sha256"
	"errors"
	"testing"
)

func TestParseAcceptsFrozenActionAndNeverReturnsAnExecutor(t *testing.T) {
	body := []byte(`{"action":"read","sessionId":"s-1","locator":{"kind":"role","role":"button"}}`)
	digest := sha256.Sum256(body)
	action, err := Parse(body, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if action.Name != "read" || action.SessionID != "s-1" {
		t.Fatalf("action = %#v", action)
	}
}

func TestParseRejectsUntypedBrowserInstruction(t *testing.T) {
	body := []byte(`{"action":"evaluate","javascript":"document.cookie"}`)
	digest := sha256.Sum256(body)
	if _, err := Parse(body, digest[:]); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Parse untyped action error = %v, want ErrInvalidInput", err)
	}
}

func TestParseRejectsTamperedDigest(t *testing.T) {
	body := []byte(`{"action":"open","businessSystemKey":"payments"}`)
	if _, err := Parse(body, make([]byte, sha256.Size)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Parse tampered action error = %v, want ErrInvalidInput", err)
	}
}

func TestParseRejectsShapeEscapesAndIncompleteLocators(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"action":"click","sessionId":"1","locator":{"kind":"role","role":"button"},"javascript":"alert(1)"}`),
		[]byte(`{"action":"goto","sessionId":"1","url":"file:///etc/passwd"}`),
		[]byte(`{"action":"fill","sessionId":"1","locator":{"kind":"css","selector":"body"},"value":"x"}`),
		[]byte(`{"action":"read","sessionId":"1","locator":{"kind":"elementRef","ref":"e:1:1"}}`),
	} {
		digest := sha256.Sum256(body)
		if _, err := Parse(body, digest[:]); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("Parse(%s) error=%v, want ErrInvalidInput", body, err)
		}
	}
}

func TestParseAcceptsWholeSessionReadFromFrozenSchema(t *testing.T) {
	body := []byte(`{"action":"read","sessionId":"12"}`)
	digest := sha256.Sum256(body)
	if _, err := Parse(body, digest[:]); err != nil {
		t.Fatalf("whole-session read rejected: %v", err)
	}
}
