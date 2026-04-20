package proxy

import "testing"

func TestNormalizeGrpcTarget(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare host:port unchanged", "localhost:50052", "localhost:50052"},
		{"ipv4 host:port unchanged", "10.0.0.1:50051", "10.0.0.1:50051"},
		{"ipv6 host:port unchanged", "[::1]:50051", "[::1]:50051"},
		{"strips http scheme", "http://localhost:50052", "localhost:50052"},
		{"strips https scheme", "https://agent.example.com:443", "agent.example.com:443"},
		{"strips trailing slash", "localhost:50052/", "localhost:50052"},
		{"strips scheme + slash", "http://localhost:50052/", "localhost:50052"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeGrpcTarget(tc.in)
			if got != tc.want {
				t.Errorf("normalizeGrpcTarget(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
