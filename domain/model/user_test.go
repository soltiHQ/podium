package model

import "testing"

func TestNormalizeSubject(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"alice", "alice"},
		{" Alice ", "alice"},
		{"\tBOB\n", "bob"},
		{"   ", ""},
	} {
		if got := NormalizeSubject(tc.in); got != tc.want {
			t.Errorf("NormalizeSubject(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
