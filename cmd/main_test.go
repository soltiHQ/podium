package main

import (
	"context"
	"errors"
	"testing"
)

func TestExitCodeReflectsRunAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name         string
		run, cleanup error
		want         int
	}{{"clean", nil, nil, 0}, {"signal", context.Canceled, nil, 0}, {"cleanup timeout", nil, context.DeadlineExceeded, 1}, {"runner failure", errors.New("failure"), nil, 1}, {"signal with incomplete drain", errors.Join(context.Canceled, context.DeadlineExceeded), nil, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(tc.run, tc.cleanup); got != tc.want {
				t.Fatalf("exit %d want %d", got, tc.want)
			}
		})
	}
}
