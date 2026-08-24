// Package novnc provides the byte-transparent, operation-scoped RFB bridge
// used behind Quoin's BrowserTunnel. It never parses RFB or stores bytes.
package novnc

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"time"
)

// Bridge copies bytes between the Quoin-authorized tunnel and Lintel's
// loopback-only x0vncserver. Cancellation closes both directions promptly.
func Bridge(ctx context.Context, tunnel io.ReadWriteCloser, vncAddress string) error {
	vnc, err := dialVNC(ctx, vncAddress)
	if err != nil {
		return err
	}
	defer vnc.Close()
	defer tunnel.Close()
	done := make(chan error, 2)
	go func() { _, err := io.Copy(vnc, tunnel); done <- err }()
	go func() { _, err := io.Copy(tunnel, vnc); done <- err }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// dialVNC tolerates the short interval between a successful x0vncserver exec
// and its loopback listener becoming ready. A Browser Operation Ack must not
// turn that local startup race into a closed Quoin BrowserTunnel.
func dialVNC(ctx context.Context, address string) (net.Conn, error) {
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		if err == nil {
			return connection, nil
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, err
		case <-time.After(25 * time.Millisecond):
		}
	}
}
