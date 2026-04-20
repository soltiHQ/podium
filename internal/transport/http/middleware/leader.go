package middleware

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// defaultWriteMethods: mutations require leader authority.
var defaultWriteMethods = map[string]struct{}{
	http.MethodPost:   {},
	http.MethodPut:    {},
	http.MethodPatch:  {},
	http.MethodDelete: {},
}

// LeaderOptions tunes the Leader middleware.
type LeaderOptions struct {
	// IsWrite overrides the default (HTTP verb based) write detection.
	IsWrite func(*http.Request) bool

	// ForwardPort is the TCP port on which the OTHER replicas serve this
	// HTTP endpoint. When non-zero, the middleware transparently reverse-
	// proxies write requests to the current leader's host at this port.
	// When zero, the middleware returns 503 + X-Leader and expects the
	// client (or an ingress with retry) to recover.
	//
	// Leadership.CurrentLeader() returns the Raft TCP address
	// (host:raftPort) — we take only the host and substitute ForwardPort
	// so the same middleware works for main API (8080) and discovery (8082).
	ForwardPort int
}

// Leader routes write requests to the cluster leader.
//
//   - Reads: pass through.
//   - Writes on leader: pass through.
//   - Writes on follower with ForwardPort == 0: 503 + X-Leader header.
//   - Writes on follower with ForwardPort > 0: transparent reverse-proxy
//     to http://<leader-host>:<ForwardPort>.
//
// In a standalone deployment AmLeader is always true, so the middleware
// adds essentially zero overhead.
func Leader(leadership cluster.Leadership, opts LeaderOptions) func(http.Handler) http.Handler {
	isWrite := opts.IsWrite
	if isWrite == nil {
		isWrite = defaultIsWrite
	}

	client := &http.Client{Timeout: 30 * time.Second}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isWrite(r) || leadership.AmLeader() {
				next.ServeHTTP(w, r)
				return
			}
			leaderAddr := leadership.CurrentLeader()
			if leaderAddr == "" {
				http.Error(w, "cluster: no leader", http.StatusServiceUnavailable)
				return
			}
			if opts.ForwardPort == 0 {
				w.Header().Set("X-Leader", leaderAddr)
				http.Error(w, "cluster: not leader", http.StatusServiceUnavailable)
				return
			}
			// Transparent proxy. Build target URL by substituting the
			// leader host with our HTTP port.
			target, err := buildTarget(leaderAddr, opts.ForwardPort, r.TLS != nil)
			if err != nil {
				http.Error(w, "cluster: bad leader addr", http.StatusBadGateway)
				return
			}
			proxy := httputil.NewSingleHostReverseProxy(target)
			proxy.Transport = client.Transport
			proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
				w.Header().Set("X-Leader", leaderAddr)
				http.Error(w, "cluster: leader unreachable", http.StatusBadGateway)
			}
			proxy.ServeHTTP(w, r)
		})
	}
}

func buildTarget(leaderAddr string, port int, tls bool) (*url.URL, error) {
	// leaderAddr is "host:raftPort" or "host" — take just the host.
	host := leaderAddr
	if i := strings.LastIndex(leaderAddr, ":"); i > 0 {
		host = leaderAddr[:i]
	}
	// strip any [brackets] from IPv6 literal
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")

	scheme := "http"
	if tls {
		scheme = "https"
	}
	u, err := url.Parse(scheme + "://" + hostWithPort(host, port))
	if err != nil {
		return nil, err
	}
	return u, nil
}

func hostWithPort(host string, port int) string {
	// IPv6 needs brackets.
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + itoa(port)
	}
	return host + ":" + itoa(port)
}

func itoa(n int) string {
	// Avoid strconv import for a trivial positive int-to-string.
	if n == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func defaultIsWrite(r *http.Request) bool {
	_, ok := defaultWriteMethods[r.Method]
	return ok
}
