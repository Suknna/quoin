package appinvestigation

// Opaque keyset cursors (HTTP-PAGE-001/005, DATA-INVEST-005): the
// investigation list cursor carries the returned activity value and the
// locator so the next page compares on the same derived expression.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/investigation"
)

type listCursorWire struct {
	Activity string `json:"a"`
	ID       int64  `json:"i"`
}

func encodeListCursor(cursor investigation.InvestigationListCursor) string {
	encoded, _ := json.Marshal(listCursorWire{Activity: cursor.LastActivityAt, ID: cursor.ID})
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeListCursor(token string) (*investigation.InvestigationListCursor, error) {
	if token == "" {
		return nil, nil
	}
	body, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("malformed cursor")
	}
	var wire listCursorWire
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("malformed cursor")
	}
	if wire.ID <= 0 || wire.Activity == "" {
		return nil, fmt.Errorf("malformed cursor")
	}
	return &investigation.InvestigationListCursor{ID: wire.ID, LastActivityAt: wire.Activity}, nil
}

func encodeIDCursor(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(id, 10)))
}

func decodeIDCursor(token string) (int64, error) {
	if token == "" {
		return 0, nil
	}
	body, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, fmt.Errorf("malformed cursor")
	}
	id, err := strconv.ParseInt(string(body), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("malformed cursor")
	}
	return id, nil
}

func encodeSeqCursor(seq int64) string {
	return encodeIDCursor(seq)
}

func decodeSeqCursor(token string) (int64, error) {
	return decodeIDCursor(token)
}
