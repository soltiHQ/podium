package discovery

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// DefaultDNSInterval is the poll interval used by DNS.Watch unless overridden.
const DefaultDNSInterval = 10 * time.Second

// Resolver is the narrow slice of [net.Resolver] the driver uses. Declared
// here so tests can plug in a stub.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// DNS resolves a hostname to a peer list. Works anywhere DNS works —
// k8s headless services, /etc/hosts, Consul DNS.
type DNS struct {
	hostname string
	port     int
	interval time.Duration
	resolver Resolver
}

var _ cluster.Discovery = (*DNS)(nil)

// DNSOption tunes the driver.
type DNSOption func(*DNS)

// WithDNSInterval overrides the Watch poll interval.
func WithDNSInterval(d time.Duration) DNSOption {
	return func(o *DNS) {
		if d > 0 {
			o.interval = d
		}
	}
}

// WithDNSResolver plugs in a non-default resolver.
func WithDNSResolver(r Resolver) DNSOption {
	return func(o *DNS) {
		if r != nil {
			o.resolver = r
		}
	}
}

// NewDNS builds a DNS driver. port is the RPC port shared by every replica.
func NewDNS(hostname string, port int, opts ...DNSOption) (*DNS, error) {
	if hostname == "" {
		return nil, fmt.Errorf("discovery/dns: empty hostname")
	}
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("discovery/dns: invalid port %d", port)
	}
	d := &DNS{
		hostname: hostname,
		port:     port,
		interval: DefaultDNSInterval,
		resolver: net.DefaultResolver,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d, nil
}

func (d *DNS) Peers(ctx context.Context) ([]cluster.Peer, error) {
	ips, err := d.resolver.LookupIPAddr(ctx, d.hostname)
	if err != nil {
		return nil, fmt.Errorf("discovery/dns: lookup %q: %w", d.hostname, err)
	}
	peers := make([]cluster.Peer, 0, len(ips))
	for _, ip := range ips {
		addr := net.JoinHostPort(ip.IP.String(), strconv.Itoa(d.port))
		peers = append(peers, cluster.Peer{ID: addr, Address: addr})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Address < peers[j].Address })
	return peers, nil
}

// Watch polls at Interval, emits only when the resolved set changes.
// Resolve errors keep the last snapshot in effect.
func (d *DNS) Watch(ctx context.Context) <-chan []cluster.Peer {
	ch := make(chan []cluster.Peer, 1)
	go func() {
		defer close(ch)
		var last []cluster.Peer
		tryEmit := func() {
			peers, err := d.Peers(ctx)
			if err != nil {
				return
			}
			if samePeerSet(last, peers) {
				return
			}
			last = peers
			select {
			case ch <- peers:
			case <-ctx.Done():
			}
		}
		tryEmit()

		t := time.NewTicker(d.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				tryEmit()
			}
		}
	}()
	return ch
}

func samePeerSet(a, b []cluster.Peer) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
