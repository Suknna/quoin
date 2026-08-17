package alerts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// hexEncode renders bytes as lowercase hex (no allocation surprises).
func hexEncode(value []byte) string {
	return hex.EncodeToString(value)
}

func digestHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// IssueKey computes the kind-specific versioned canonical signature digest for
// alert intake issues (DATA-ALERT-011). Each payload is a UTF-8 JSON object
// with keys in byte order and no insignificant whitespace, then SHA-256
// lower-hex. The source is partitioned by source_id at the row level, so
// source_id itself is never part of the signature.
func IssueKey(kind string, fields map[string]string) (string, error) {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sortStrings(names)
	var builder stringBuilder
	builder.WriteByte('{')
	for index, name := range names {
		if index > 0 {
			_ = builder.WriteByte(',')
		}
		encodedName, err := json.Marshal(name)
		if err != nil {
			return "", err
		}
		encodedValue, err := json.Marshal(fields[name])
		if err != nil {
			return "", err
		}
		_, _ = builder.Write(encodedName)
		_ = builder.WriteByte(':')
		_, _ = builder.Write(encodedValue)
	}
	builder.WriteByte('}')
	return digestHex(builder.Bytes()), nil
}

func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for cursor := index; cursor > 0 && values[cursor] < values[cursor-1]; cursor-- {
			values[cursor], values[cursor-1] = values[cursor-1], values[cursor]
		}
	}
}

type stringBuilder struct {
	data []byte
}

func (builder *stringBuilder) WriteByte(value byte) error {
	builder.data = append(builder.data, value)
	return nil
}

func (builder *stringBuilder) Write(value []byte) (int, error) {
	builder.data = append(builder.data, value...)
	return len(value), nil
}

func (builder *stringBuilder) Bytes() []byte {
	return builder.data
}
