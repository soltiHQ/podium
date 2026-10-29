package middleware

import (
	"net/http"
	"testing"

	"github.com/soltiHQ/control-plane/internal/transport/clientip"
)

func TestStreamLimitKeysByResolvedClientIP(t *testing.T) {
	resolver, err := clientip.New([]string{"10.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	const proxy, holder = "10.0.0.1:443", "203.0.113.9"
	var (
		entered = make(chan struct{})
		release = make(chan struct{})
		done    = make(chan struct{})
	)
	h := StreamLimit(NewStreamLimiter(1, resolver))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Forwarded-For") == holder {
			close(entered)
			<-release
		}
		w.WriteHeader(http.StatusOK)
	}))
	go func() {
		defer close(done)
		serve(h, "/", proxy, holder)
	}()
	<-entered
	if code := serve(h, "/", proxy, holder); code != http.StatusTooManyRequests {
		t.Errorf("second stream of the same client: HTTP %d, want 429", code)
	}
	if code := serve(h, "/", proxy, "198.51.100.7"); code != http.StatusOK {
		t.Errorf("stream of another client behind the same proxy: HTTP %d, want 200", code)
	}
	close(release)
	<-done
}
