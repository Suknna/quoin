package main

import "time"

// The frozen exchange body mirrors the host-side faults package so the
// in-network upstream and the host verifier agree on the exact bytes.
const (
	chunkSize = 4096
	chunks    = 16
	pause     = 40 * time.Millisecond
)

func frozenBody() []byte {
	body := make([]byte, chunkSize*chunks)
	for index := range body {
		body[index] = byte(index % 251)
	}
	return body
}
