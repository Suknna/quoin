package alerts

import (
	"testing"
)

func TestFingerprintMatchesPrometheusCommonVector(t *testing.T) {
	labels := map[string]string{
		"alertname": "FnvProbe",
		"instance":  "probe-1",
		"job":       "probejob",
		"severity":  "critical",
	}
	got := FingerprintOf(labels)
	// Captured from a real Alertmanager v0.28.1 webhook payload with these
	// exact labels and cross-checked against prometheus/common/model.
	const want = "1aa55da08eeecbbf"
	if hexEncode(got) != want {
		t.Fatalf("fingerprint=%s want=%s", hexEncode(got), want)
	}
}

func TestFingerprintIsOrderIndependent(t *testing.T) {
	first := FingerprintOf(map[string]string{"a": "1", "b": "2", "c": "3"})
	second := FingerprintOf(map[string]string{"c": "3", "a": "1", "b": "2"})
	if hexEncode(first) != hexEncode(second) {
		t.Fatal("fingerprint must not depend on label iteration order")
	}
	empty := FingerprintOf(map[string]string{})
	// FNV-1a 64 of the empty input is the offset basis; prometheus/common
	// defines an explicit emptyLabelSignature equal to that same value.
	if hexEncode(empty) != "cbf29ce484222325" {
		t.Fatalf("empty labels fingerprint=%s", hexEncode(empty))
	}
}

func TestIssueKeyIsVersionedCanonical(t *testing.T) {
	first, err := IssueKey("identity_conflict", map[string]string{
		"v": "1", "expected_labels_digest": "ab", "native_occurrence_key": "1:abcd:2026", "received_labels_digest": "cd",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 {
		t.Fatalf("issue key length=%d", len(first))
	}
	second, _ := IssueKey("identity_conflict", map[string]string{
		"received_labels_digest": "cd", "v": "1", "native_occurrence_key": "1:abcd:2026", "expected_labels_digest": "ab",
	})
	if first != second {
		t.Fatal("issue key must be byte-order canonical (keys sorted)")
	}
	other, _ := IssueKey("fingerprint_mismatch", map[string]string{
		"labels_digest": "ab", "native_occurrence_key": "1:abcd:2026", "received_fingerprint": "ef", "v": "1",
	})
	if first == other {
		t.Fatal("different kinds must produce different keys")
	}
}

func TestNormalizeStartsAt(t *testing.T) {
	got, err := NormalizeStartsAt("2026-08-17T12:24:59.124724277Z")
	if err != nil {
		t.Fatal(err)
	}
	if got != "2026-08-17T12:24:59.124724277Z" {
		t.Fatalf("normalized=%s", got)
	}
	shifted, err := NormalizeStartsAt("2026-08-17T14:24:59.124724277+02:00")
	if err != nil {
		t.Fatal(err)
	}
	if shifted != got {
		t.Fatalf("offset representation must normalize to UTC: %s vs %s", shifted, got)
	}
	for _, bad := range []string{"", "not-a-time", "2026-13-99T99:99:99Z"} {
		if _, err := NormalizeStartsAt(bad); err == nil {
			t.Fatalf("malformed startsAt %q accepted", bad)
		}
	}
}

func TestCanonicalLabelsAndDigest(t *testing.T) {
	canonical, err := CanonicalLabels(map[string]string{"z": "1", "a": "2"})
	if err != nil {
		t.Fatal(err)
	}
	if canonical != `{"a":"2","z":"1"}` {
		t.Fatalf("canonical=%s", canonical)
	}
	if DigestLabels(canonical) != digestHex([]byte(canonical)) {
		t.Fatal("digest must be sha256 of canonical labels")
	}
}
