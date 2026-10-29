// Package discovery holds [cluster.Discovery] drivers.
package discovery

import (
	"context"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// Static returns a fixed peer list from config. Use for bare-metal and
// docker-compose setups where peers rarely change.
type Static struct {
	peers []cluster.Peer
}

var _ cluster.Discovery = (*Static)(nil)

// NewStatic clones the peer list defensively so callers may mutate it.
func NewStatic(peers []cluster.Peer) *Static {
	cp := make([]cluster.Peer, len(peers))
	copy(cp, peers)
	return &Static{peers: cp}
}

func (s *Static) Peers(_ context.Context) ([]cluster.Peer, error) {
	cp := make([]cluster.Peer, len(s.peers))
	copy(cp, s.peers)
	return cp, nil
}

// Watch emits exactly one snapshot then waits for ctx cancellation.
func (s *Static) Watch(ctx context.Context) <-chan []cluster.Peer {
	ch := make(chan []cluster.Peer, 1)
	cp := make([]cluster.Peer, len(s.peers))
	copy(cp, s.peers)
	ch <- cp
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch
}
