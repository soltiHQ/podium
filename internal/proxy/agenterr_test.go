package proxy

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/soltiHQ/control-plane/internal/transport/errkind"
)

type httpStatusErr struct{ code int }

func (e httpStatusErr) Error() string   { return "http error" }
func (e httpStatusErr) HTTPStatus() int { return e.code }

func TestClassifyAgentErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want errkind.Kind
	}{
		{"grpc not found", grpcstatus.Error(codes.NotFound, "x"), errkind.NotFound},
		{"grpc invalid arg", grpcstatus.Error(codes.InvalidArgument, "x"), errkind.InvalidArgument},
		{"grpc unavailable", grpcstatus.Error(codes.Unavailable, "x"), errkind.Unavailable},
		{"http 404", httpStatusErr{code: 404}, errkind.NotFound},
		{"http 409", httpStatusErr{code: 409}, errkind.Conflict},
		{"cp-side (no status)", errors.New("dial failed"), errkind.Internal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyAgentErr(tt.err); got != tt.want {
				t.Fatalf("classifyAgentErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestAgentErrorCarriesKindAndSentinel(t *testing.T) {
	inner := grpcstatus.Error(codes.NotFound, "task gone")
	err := agentError(ErrGetTask, inner)

	// errkind reads the tagged Kind through the single contract.
	if got := errkind.Classify(err); got != errkind.NotFound {
		t.Fatalf("Classify = %v, want NotFound", got)
	}
	// The op sentinel stays in the chain for errors.Is.
	if !errors.Is(err, ErrGetTask) {
		t.Fatalf("errors.Is(ErrGetTask) = false, want true")
	}
}

// A text an agent produced is cut to MaxAgentTextBytes on a UTF-8 boundary, with a
// marker where it was cut. A text within the limit is left as it is.
func TestCapAgentText(t *testing.T) {
	const marker = "…"
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"no text", ""},
		{"a short text", "agent said no"},
		{"a text of the most", strings.Repeat("x", MaxAgentTextBytes)},
		{"a text one byte over", strings.Repeat("x", MaxAgentTextBytes+1)},
		{"a text of a MiB", strings.Repeat("x", 1<<20)},
		{"two-byte runes", "a" + strings.Repeat("é", MaxAgentTextBytes)},
		{"two-byte runes, cut the other way", strings.Repeat("é", MaxAgentTextBytes)},
		{"three-byte runes", strings.Repeat("世", MaxAgentTextBytes)},
		{"three-byte runes, shifted by one", "a" + strings.Repeat("世", MaxAgentTextBytes)},
		{"three-byte runes, shifted by two", "ab" + strings.Repeat("世", MaxAgentTextBytes)},
		{"four-byte runes", strings.Repeat("😀", MaxAgentTextBytes)},
		{"four-byte runes, shifted by one", "a" + strings.Repeat("😀", MaxAgentTextBytes)},
		{"four-byte runes, shifted by two", "ab" + strings.Repeat("😀", MaxAgentTextBytes)},
		{"four-byte runes, shifted by three", "abc" + strings.Repeat("😀", MaxAgentTextBytes)},
		{"bytes that are no UTF-8", strings.Repeat("\x80", 2*MaxAgentTextBytes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CapAgentText(tc.in)

			if len(tc.in) <= MaxAgentTextBytes {
				if got != tc.in {
					t.Fatalf("CapAgentText cut a text of %d bytes", len(tc.in))
				}
				return
			}
			if len(got) > MaxAgentTextBytes+len(marker) || !strings.HasSuffix(got, marker) {
				t.Fatalf("got %d bytes ending %q, want at most %d, ending with the marker", len(got), got[len(got)-4:], MaxAgentTextBytes+len(marker))
			}
			kept := strings.TrimSuffix(got, marker)
			if !strings.HasPrefix(tc.in, kept) {
				t.Fatal("what is kept is not the start of the text")
			}
			if utf8.ValidString(tc.in) && !utf8.ValidString(got) {
				t.Fatalf("a valid text was cut inside a rune: %q", got[len(got)-8:])
			}
			if utf8.ValidString(tc.in) && len(kept) <= MaxAgentTextBytes-utf8.UTFMax {
				t.Fatalf("kept %d bytes, a rune boundary is at most %d bytes below the limit", len(kept), utf8.UTFMax-1)
			}
			if again := CapAgentText(got); again != got {
				t.Fatal("capping a capped text changed it")
			}
		})
	}
}

// The error of an agent call is capped when it is read, wherever it is read:
// stored, recorded or logged. What lies behind it stays in the chain.
func TestAgentErrorCapsItsText(t *testing.T) {
	huge := strings.Repeat("x", 1<<20)
	t.Run("short text is the text", func(t *testing.T) {
		if got := agentError(ErrGetTask, errors.New("gone")).Error(); got != "proxy: get task: gone" {
			t.Fatalf("Error() = %q", got)
		}
	})
	t.Run("a long text is cut", func(t *testing.T) {
		err := agentError(ErrGetTask, errors.New(huge))
		if len(err.Error()) > MaxAgentTextBytes+len("…") || !strings.HasPrefix(err.Error(), "proxy: get task: xxx") {
			t.Fatalf("Error() is %d bytes: %.40q", len(err.Error()), err.Error())
		}
		if !errors.Is(err, ErrGetTask) {
			t.Fatal("the operation left the chain")
		}
	})
	t.Run("the status and the kind behind a long text stay", func(t *testing.T) {
		err := agentError(ErrGetTask, grpcstatus.Error(codes.NotFound, huge))
		if len(err.Error()) > MaxAgentTextBytes+len("…") {
			t.Fatalf("Error() is %d bytes", len(err.Error()))
		}
		if kind := errkind.Classify(err); kind != errkind.NotFound {
			t.Fatalf("kind = %v, want NotFound", kind)
		}
		if code := grpcstatus.Code(err); code != codes.NotFound {
			t.Fatalf("code = %v, want NotFound", code)
		}
	})
	t.Run("a wrapper of the error is capped through it", func(t *testing.T) {
		err := fmt.Errorf("apply task: %w", agentError(ErrApplyTask, errors.New(huge)))
		if len(err.Error()) > MaxAgentTextBytes+len("apply task: ")+len("…") {
			t.Fatalf("Error() is %d bytes", len(err.Error()))
		}
	})
}
