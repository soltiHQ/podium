package clientip

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// hostRefresh is how often Hosts looks up its names again.
	hostRefresh = 10 * time.Second
	// hostLookupTimeout bounds one lookup of a name.
	hostLookupTimeout = 5 * time.Second
)

// Hosts is a changing list of trusted hosts, each written as an IP address or as
// a name: the hosts of the raft configuration. The list is asked anew on every
// check, so a host it drops is trusted no more, and an IP it gains is trusted, at
// once. A name is trusted at the addresses its last lookup returned: a loop looks
// up every name on the list every 10 seconds, and at once when a check meets a
// name it has not looked up yet, through the system resolver, as the nodes dial
// one another. A name whose last lookup failed is trusted at no address. So the
// trust of a name follows the DNS answers this node gets, up to 10 seconds late.
type Hosts struct {
	list   func() []string
	lookup func(ctx context.Context, name string) ([]netip.Addr, error)

	// names holds the answer of the last lookup of each name, nil when it failed.
	// A name it does not hold has not been looked up.
	names atomic.Pointer[map[string][]netip.Addr]
	wake  chan struct{}
	stop  context.CancelFunc
	done  chan struct{}
}

// NewHosts returns the Hosts of list and starts the loop that looks up its
// names, until Close. list is asked on every check, so it must be cheap and safe
// for concurrent use.
func NewHosts(list func() []string) *Hosts {
	h := newHosts(list, lookupIP)
	h.start(hostRefresh)
	return h
}

// newHosts returns the Hosts of list, whose names lookup resolves, without its
// loop.
func newHosts(list func() []string, lookup func(context.Context, string) ([]netip.Addr, error)) *Hosts {
	return &Hosts{list: list, lookup: lookup, wake: make(chan struct{}, 1)}
}

// lookupIP returns the IP addresses of name, as the system resolver answers.
func lookupIP(ctx context.Context, name string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", name)
}

// start runs the loop that looks up the names every period.
func (h *Hosts) start(every time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	h.stop, h.done = cancel, make(chan struct{})
	go h.run(ctx, every)
}

func (h *Hosts) run(ctx context.Context, every time.Duration) {
	defer close(h.done)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		h.resolve(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-h.wake:
		}
	}
}

// Close stops the loop, a lookup in flight included, and waits for it to end.
// Checks still work after it, on the last answers.
func (h *Hosts) Close() {
	if h == nil || h.stop == nil {
		return
	}
	h.stop()
	<-h.done
}

// Contains reports whether a, a canonical address, is the address of a host on the
// list: the host itself, or an address its name was last looked up at. A name not
// looked up yet wakes the loop.
func (h *Hosts) Contains(a netip.Addr) bool {
	if h == nil {
		return false
	}
	var names map[string][]netip.Addr
	if p := h.names.Load(); p != nil {
		names = *p
	}
	unresolved := false
	for _, host := range h.list() {
		if host == "" {
			continue
		}
		if ip, err := netip.ParseAddr(host); err == nil {
			if canonical(ip) == a {
				return true
			}
			continue
		}
		addrs, looked := names[host]
		if !looked {
			unresolved = true
			continue
		}
		if slices.Contains(addrs, a) {
			return true
		}
	}
	if unresolved {
		select {
		case h.wake <- struct{}{}:
		default:
		}
	}
	return false
}

// resolve looks up every name on the list, all at once, and keeps their answers in
// place of the last ones: a name the list no longer holds is forgotten.
func (h *Hosts) resolve(ctx context.Context) {
	var names []string
	for _, host := range h.list() {
		if _, err := netip.ParseAddr(host); err != nil && host != "" && !slices.Contains(names, host) {
			names = append(names, host)
		}
	}
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		answers = make(map[string][]netip.Addr, len(names))
	)
	for _, name := range names {
		wg.Go(func() {
			lookupCtx, cancel := context.WithTimeout(ctx, hostLookupTimeout)
			defer cancel()
			var trusted []netip.Addr
			if addrs, err := h.lookup(lookupCtx, name); err == nil {
				for _, a := range addrs {
					trusted = append(trusted, canonical(a))
				}
			}
			mu.Lock()
			answers[name] = trusted
			mu.Unlock()
		})
	}
	wg.Wait()
	h.names.Store(&answers)
}
