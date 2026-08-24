package runtime

import (
	"context"
	"testing"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
)

func TestClaimBrowserTunnelRetainsFirstOwnerAcrossReplay(t *testing.T) {
	request := &runtimev1.StartBrowserOperation{OperationId: 17}
	channel := &Channel{
		started:           map[int64]*runtimev1.StartBrowserOperation{17: request},
		tunnelCancels:     make(map[int64]context.CancelFunc),
		tunnelDones:       make(map[int64]chan struct{}),
		tunnelGenerations: make(map[int64]uint64),
		tunnelBinding:     &browserTunnelBinding{context: context.Background(), epoch: 1},
	}
	firstDone := make(chan struct{})
	firstContext, firstCancel := context.WithCancel(context.Background())
	defer firstCancel()
	generation, claimed := channel.claimBrowserTunnelForOperation(request, 1, firstCancel, firstDone)
	if !claimed || generation != 1 {
		t.Fatalf("first tunnel claim = (%d, %t), want (1, true)", generation, claimed)
	}

	secondDone := make(chan struct{})
	_, secondCancel := context.WithCancel(context.Background())
	if _, claimed := channel.claimBrowserTunnelForOperation(request, 1, secondCancel, secondDone); claimed {
		t.Fatal("replayed Start/control reconnect replaced the live tunnel owner")
	}
	secondCancel()
	select {
	case <-firstContext.Done():
		t.Fatal("replayed attempt cancelled the live tunnel owner")
	default:
	}
	if channel.tunnelDones[17] != firstDone {
		t.Fatal("replayed attempt replaced the live tunnel completion fence")
	}

	channel.releaseBrowserTunnel(17, generation, firstDone)
	select {
	case <-firstDone:
	default:
		t.Fatal("releasing first tunnel did not close its completion fence")
	}
	if channel.tunnelDones[17] != nil || channel.tunnelCancels[17] != nil {
		t.Fatal("released tunnel owner remained registered")
	}
	if next, claimed := channel.claimBrowserTunnelForOperation(request, 1, firstCancel, make(chan struct{})); !claimed || next != 2 {
		t.Fatalf("next generation claim = (%d, %t), want (2, true)", next, claimed)
	}
}

func TestInstallBrowserTunnelBindingCancelsAndJoinsPriorEpoch(t *testing.T) {
	request := &runtimev1.StartBrowserOperation{OperationId: 17}
	channel := &Channel{
		started:           map[int64]*runtimev1.StartBrowserOperation{17: request},
		tunnelCancels:     make(map[int64]context.CancelFunc),
		tunnelDones:       make(map[int64]chan struct{}),
		tunnelGenerations: make(map[int64]uint64),
		tunnelBinding:     &browserTunnelBinding{context: context.Background(), epoch: 1},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	generation, claimed := channel.claimBrowserTunnelForOperation(request, 1, cancel, done)
	if !claimed {
		t.Fatal("claim prior-epoch tunnel")
	}
	go func() {
		<-ctx.Done()
		channel.releaseBrowserTunnel(request.GetOperationId(), generation, done)
	}()

	channel.installBrowserTunnelBinding(browserTunnelBinding{context: context.Background(), epoch: 2})
	if channel.tunnelBinding == nil || channel.tunnelBinding.epoch != 2 {
		t.Fatalf("current binding = %#v, want epoch 2", channel.tunnelBinding)
	}
	if channel.tunnelDones[request.GetOperationId()] != nil || channel.tunnelCancels[request.GetOperationId()] != nil {
		t.Fatal("prior-epoch tunnel owner survived binding replacement")
	}
	if _, claimed := channel.claimBrowserTunnelForOperation(request, 1, func() {}, make(chan struct{})); claimed {
		t.Fatal("stale epoch claimed a tunnel after replacement")
	}
}
