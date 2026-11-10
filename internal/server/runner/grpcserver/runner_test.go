package grpcserver

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
)

// waitTimeout bounds every wait of these tests; it only turns a hang into a failure.
const waitTimeout = 10 * time.Second

// recv receives from ch, and fails the test when nothing comes within waitTimeout.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitTimeout):
		t.Fatalf("%s: nothing within %v", what, waitTimeout)
		panic("unreachable")
	}
}

// freeAddr returns a loopback address that nothing listens on when it returns.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// A Stop that comes before Start, as server.Run's does for a runner whose
// goroutine has not reached Start yet, keeps the runner from ever binding: Start
// then returns nil at once, Bound is closed, Addr is "", and the address stays
// free.
func TestStopBeforeStartNeverBinds(t *testing.T) {
	addr := freeAddr(t)
	srv := grpc.NewServer()
	// A server that serves all the same is stopped here, so that it does not
	// serve past the test.
	t.Cleanup(srv.Stop)
	r, err := New(Config{Addr: addr}, zerolog.Nop(), srv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}
	started := make(chan error, 1)
	go func() { started <- r.Start(context.Background()) }()
	if err := recv(t, started, "Start after Stop returns"); err != nil {
		t.Fatalf("Start after Stop: %v, want nil", err)
	}
	select {
	case <-r.Bound():
	default:
		t.Fatal("Bound is not closed after Stop")
	}
	if got := r.Addr(); got != "" {
		t.Fatalf("Addr = %q after Stop, want none", got)
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the runner's address is taken after Start: %v", err)
	}
	_ = l.Close()
}
