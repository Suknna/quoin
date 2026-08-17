// Package alerts owns the alert-domain intake, identity, lifecycle and
// delivery semantics on top of the frozen SQLite authority.
package alerts

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"
)

const (
	// FingerprintByteSeparator matches the prometheus/common separator that
	// Alertmanager uses when hashing label sets (empirically captured from a
	// real Alertmanager webhook payload and cross-checked against the
	// prometheus/common model source; DATA-ALERT-004 identity contract).
	FingerprintByteSeparator byte = 0xff
)

// AlertmanagerWebhook is the exact wire shape Alertmanager v0.28+ posts
// (version 4). Only the fields Quoin treats as machine semantics are
// decoded; unknown fields stay untouched inside the preserved body bytes.
type AlertmanagerWebhook struct {
	Status string `json:"status"`
	Alerts []struct {
		Status       string            `json:"status"`
		Labels       map[string]string `json:"labels"`
		Annotations  map[string]string `json:"annotations"`
		StartsAt     string            `json:"startsAt"`
		EndsAt       string            `json:"endsAt"`
		Fingerprint  string            `json:"fingerprint"`
		GeneratorURL string            `json:"generatorURL"`
	} `json:"alerts"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	TruncatedAlerts   int               `json:"truncatedAlerts"`
}

// ParseWebhook decodes the frozen Alertmanager webhook shape. The raw body is
// preserved byte-for-byte by the caller (DATA-ALERT-001); this function only
// derives the machine semantics Quoin needs for the intake transaction.
func ParseWebhook(body []byte) (*AlertmanagerWebhook, error) {
	var webhook AlertmanagerWebhook
	if err := json.Unmarshal(body, &webhook); err != nil {
		return nil, err
	}
	return &webhook, nil
}

// NormalizeStartsAt canonicalizes the source-declared startsAt to UTC
// RFC3339Nano so the identity triple is stable across representation changes.
// A missing or malformed startsAt is an intake problem, not an identity.
func NormalizeStartsAt(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("startsAt is empty")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", fmt.Errorf("startsAt is not RFC3339: %w", err)
	}
	return parsed.UTC().Format(time.RFC3339Nano), nil
}

// FingerprintOf computes the exact Alertmanager fingerprint for a label set
// (prometheus/common/model LabelSet.Fingerprint): FNV-1a 64 over sorted
// label-name, separator byte, label-value, separator byte. The result is the
// big-endian 8-byte encoding of that uint64 (schema fingerprint BLOB).
func FingerprintOf(labels map[string]string) []byte {
	hash := fnv.New64a()
	for _, name := range sortedLabelNames(labels) {
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{FingerprintByteSeparator})
		_, _ = hash.Write([]byte(labels[name]))
		_, _ = hash.Write([]byte{FingerprintByteSeparator})
	}
	value := hash.Sum64()
	out := make([]byte, 8)
	binary.BigEndian.PutUint64(out, value)
	return out
}

// FingerprintFromHex parses the 16-hex-char string Alertmanager sends in the
// webhook payload. The payload fingerprint is advisory identity metadata; the
// authoritative fingerprint is recomputed from the full labels snapshot and
// cross-checked (fingerprint_mismatch intake issue when they differ).
func FingerprintFromHex(value string) ([]byte, error) {
	if len(value) != 16 {
		return nil, fmt.Errorf("fingerprint must be 16 hex characters, got %q", value)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("fingerprint is not hex: %w", err)
	}
	return decoded, nil
}

// CanonicalLabels renders the frozen immutable full-labels snapshot: JSON
// object with keys in byte order and no insignificant whitespace.
func CanonicalLabels(labels map[string]string) (string, error) {
	names := sortedLabelNames(labels)
	var builder strings.Builder
	builder.WriteByte('{')
	for index, name := range names {
		if index > 0 {
			builder.WriteByte(',')
		}
		encodedName, err := json.Marshal(name)
		if err != nil {
			return "", err
		}
		encodedValue, err := json.Marshal(labels[name])
		if err != nil {
			return "", err
		}
		builder.Write(encodedName)
		builder.WriteByte(':')
		builder.Write(encodedValue)
	}
	builder.WriteByte('}')
	return builder.String(), nil
}

func sortedLabelNames(labels map[string]string) []string {
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// DigestLabels returns the hex SHA-256 of the canonical labels snapshot; the
// identity/labels snapshot comparison uses this digest (DATA-ALERT-004/011).
func DigestLabels(canonical string) string {
	// Implemented in identity.go to keep this file focused on wire shape.
	return digestHex([]byte(canonical))
}

// canonicalOccurrenceKey renders the frozen identity_conflict /
// fingerprint_mismatch issue signatures ("native_occurrence_key"): the
// `source_id + fingerprint-hex + normalized startsAt` composite the schema
// UNIQUE triple represents, as a stable machine string.
func canonicalOccurrenceKey(sourceID int64, fingerprint []byte, startsAt string) string {
	return fmt.Sprintf("%d:%s:%s", sourceID, hex.EncodeToString(fingerprint), startsAt)
}

// Sessionless transaction helpers are in transactions.go; this file only
// carries pure functions so the identity and wire-shape semantics stay unit
// testable without a database.
