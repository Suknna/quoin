package novnc

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestDialVNCRetriesListenerStartup(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	if err := reserved.Close(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan net.Listener, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		listener, listenErr := net.Listen("tcp", address)
		if listenErr == nil {
			ready <- listener
		}
	}()
	connection, err := dialVNC(context.Background(), address)
	if err != nil {
		t.Fatalf("dial did not wait for listener startup: %v", err)
	}
	defer connection.Close()
	select {
	case listener := <-ready:
		defer listener.Close()
	case <-time.After(time.Second):
		t.Fatal("delayed listener did not start")
	}
}
