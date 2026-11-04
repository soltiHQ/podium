package clientip

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// fakeDNS answers lookups from its records, which a test changes; a name
// without records does not resolve.
type fakeDNS struct {
	mu      sync.Mutex
	records map[string][]netip.Addr
}

func (d *fakeDNS) lookup(_ context.Context, name string) ([]netip.Addr, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	addrs, ok := d.records[name]
	if !ok {
		return nil, errors.New("no such host")
	}
	return addrs, nil
}

func (d *fakeDNS) set(name string, addrs ...netip.Addr) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.records == nil {
		d.records = map[string][]netip.Addr{}
	}
	if addrs == nil {
		delete(d.records, name)
		return
	}
	d.records[name] = addrs
}

// hostList is a list of hosts that a test changes.
type hostList struct {
	mu    sync.Mutex
	hosts []string
}

func (l *hostList) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.hosts...)
}

func (l *hostList) set(hosts ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hosts = hosts
}

var (
	node1   = netip.MustParseAddr("10.0.0.1")
	node2   = netip.MustParseAddr("10.0.0.2")
	node2v6 = netip.MustParseAddr("fd00::2")
	moved   = netip.MustParseAddr("10.0.0.22")
)

// The list counts as it is at each check: an IP it holds is trusted, written
// IPv4-mapped or not, and one it drops is not, at once. A name is trusted at the
// addresses its last lookup returned, all of them, IPv4-mapped ones unmapped,
// and follows the next lookup: a new answer moves the trust, a failed lookup ends
// it, and a name the list drops is not trusted, at once.
func TestHostsFollowTheListAndTheLookups(t *testing.T) {
	list := &hostList{}
	list.set("10.0.0.1", "podium-2.podium", "::ffff:10.0.0.4")
	dns := &fakeDNS{}
	dns.set("podium-2.podium", netip.MustParseAddr("::ffff:10.0.0.2"), node2v6)
	h := newHosts(list.get, dns.lookup)
	ctx := context.Background()
	check := func(step string, want map[netip.Addr]bool) {
		t.Helper()
		for a, trusted := range want {
			if got := h.Contains(a); got != trusted {
				t.Errorf("%s: Contains(%s) = %v, want %v", step, a, got, trusted)
			}
		}
	}

	check("before any lookup", map[netip.Addr]bool{node1: true, netip.MustParseAddr("10.0.0.4"): true, node2: false, node2v6: false})
	h.resolve(ctx)
	check("after a lookup", map[netip.Addr]bool{node1: true, node2: true, node2v6: true, moved: false, netip.MustParseAddr("10.0.0.9"): false})

	dns.set("podium-2.podium", moved)
	h.resolve(ctx)
	check("after the name moved", map[netip.Addr]bool{node2: false, node2v6: false, moved: true})

	dns.set("podium-2.podium")
	h.resolve(ctx)
	check("after a failed lookup", map[netip.Addr]bool{moved: false, node1: true})

	dns.set("podium-2.podium", moved)
	h.resolve(ctx)
	list.set("podium-2.podium")
	check("after the list dropped 10.0.0.1", map[netip.Addr]bool{node1: false, moved: true})
	list.set("10.0.0.1")
	check("after the list dropped the name", map[netip.Addr]bool{moved: false, node1: true})
}

// A check that meets a name not looked up yet wakes the loop, which looks it up
// at once rather than at its next period.
func TestHostsLookUpANewNameAtOnce(t *testing.T) {
	list := &hostList{}
	list.set("10.0.0.1")
	dns := &fakeDNS{}
	dns.set("podium-2.podium", node2)
	h := newHosts(list.get, dns.lookup)
	h.start(time.Hour)
	t.Cleanup(h.Close)

	list.set("10.0.0.1", "podium-2.podium")
	deadline := time.Now().Add(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !h.Contains(node2) {
		if time.Now().After(deadline) {
			t.Fatal("a new name was not looked up within 5s")
		}
		<-tick.C
	}
}

// Close stops the loop even while a lookup hangs, and checks still answer from
// the last lookups.
func TestHostsCloseStopsTheLoop(t *testing.T) {
	list := &hostList{}
	list.set("10.0.0.1", "podium-2.podium")
	entered := make(chan struct{}, 1)
	h := newHosts(list.get, func(ctx context.Context, _ string) ([]netip.Addr, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	h.start(time.Hour)
	<-entered
	closed := make(chan struct{})
	go func() {
		h.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(hostLookupTimeout / 2):
		t.Fatal("Close waited on a hanging lookup")
	}
	h.Close()
	if !h.Contains(node1) || h.Contains(node2) {
		t.Fatal("a closed Hosts no longer answers from the list")
	}
}

// The resolver trusts the hosts as it trusts trusted_proxies: their forwarding
// headers count, over HTTP and in gRPC metadata alike, and they are skipped in
// the walk. Anyone else is the client, whatever they send.
func TestResolverTrustsHosts(t *testing.T) {
	list := &hostList{}
	list.set("10.0.0.1", "podium-2.podium")
	dns := &fakeDNS{}
	dns.set("podium-2.podium", node2)
	hosts := newHosts(list.get, dns.lookup)
	hosts.resolve(context.Background())
	resolver, err := New([]string{"192.168.0.0/16"}, TrustHosts(hosts))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, peer string
		xff        []string
		want       string
	}{
		{"a host written as an IP", "10.0.0.1:50000", []string{"203.0.113.9"}, "203.0.113.9"},
		{"a host written as a name", "10.0.0.2:50000", []string{"203.0.113.9"}, "203.0.113.9"},
		{"a trusted proxy", "192.168.1.1:443", []string{"203.0.113.9"}, "203.0.113.9"},
		{"through a trusted proxy to a host", "10.0.0.1:50000", []string{"203.0.113.9, 192.168.1.1"}, "203.0.113.9"},
		{"hosts in the walk", "10.0.0.1:50000", []string{"203.0.113.9, 10.0.0.2"}, "203.0.113.9"},
		{"spoofed left-most entry", "10.0.0.1:50000", []string{"192.0.2.66, 203.0.113.9"}, "203.0.113.9"},
		{"another host", "10.0.0.9:50000", []string{"203.0.113.9"}, "10.0.0.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolver.IP(request(tc.peer, http.Header{"X-Forwarded-For": tc.xff})); got != tc.want {
				t.Errorf("IP = %q, want %q", got, tc.want)
			}
			if got := resolver.Forwarded(tc.peer, tc.xff); got != tc.want {
				t.Errorf("Forwarded = %q, want %q", got, tc.want)
			}
		})
	}
	if got := (*Resolver)(nil).Forwarded("10.0.0.1:50000", []string{"203.0.113.9"}); got != "10.0.0.1" {
		t.Fatalf("a nil resolver: Forwarded = %q, want the peer", got)
	}
}
