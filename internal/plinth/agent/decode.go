package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// New agent versions reject unexpected frozen-input fields instead of
// silently discarding facts. Legacy modes keep their original decoders.
func decodeCurrentInput(canonical []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("input has trailing content: %v", err)
	}
	return nil
}
