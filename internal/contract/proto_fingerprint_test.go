package contract_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
)

// TestProtoAuthorityFingerprintIsTheDigestOfBothAuthoritativeSources ensures
// every component derives its admission identity from the complete, ordered
// authority set rather than a release label or one RPC's generated output.
func TestProtoAuthorityFingerprintIsTheDigestOfBothAuthoritativeSources(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	paths := []string{
		"docs/specs/quoin-v1/contracts/runtime.proto",
		"docs/specs/quoin-v1/contracts/quoin/plinth/worker/v1/agent_worker.proto",
	}

	hash := sha256.New()
	for _, path := range paths {
		body, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		// The path length prefix makes the ordered file set unambiguous.
		hash.Write([]byte{byte(len(path) >> 8), byte(len(path))})
		hash.Write([]byte(path))
		hash.Write(body)
	}
	want := hex.EncodeToString(hash.Sum(nil))
	if got := contract.ProtoAuthorityFingerprint; got != want {
		t.Fatalf("fingerprint=%q, want complete authority digest %q", got, want)
	}
}

func TestProtoAuthorityFingerprintIsValid(t *testing.T) {
	if !contract.ValidProtoAuthorityFingerprint(contract.ProtoAuthorityFingerprint) {
		t.Fatal("generated proto authority fingerprint must be a lowercase SHA-256 hex digest")
	}
	for _, invalid := range []string{"", "not-a-digest", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		if contract.ValidProtoAuthorityFingerprint(invalid) {
			t.Fatalf("invalid fingerprint %q was accepted", invalid)
		}
	}
}
