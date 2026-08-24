package novnc

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestBridgeClosesBothDirectionsAfterTunnelEOF(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	peerClosed := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			peerClosed <- acceptErr
			return
		}
		defer connection.Close()
		_, readErr := connection.Read(make([]byte, 1))
		peerClosed <- readErr
	}()

	tunnel, peer := net.Pipe()
	result := make(chan error, 1)
	go func() { result <- Bridge(context.Background(), tunnel, listener.Addr().String()) }()
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-peerClosed:
		if err != io.EOF {
			t.Fatalf("VNC peer remained open or failed unexpectedly: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Bridge did not close the opposite VNC direction after tunnel EOF")
	}
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("Bridge did not wait for both copy directions to stop")
	}
}
