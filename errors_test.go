package main

import (
	"errors"
	"fmt"
	"testing"
)

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "nil is success", err: nil, want: 0},
		{name: "plain error is 1", err: fmt.Errorf("boom"), want: 1},
		{name: "sync failure is 1", err: &syncFailedError{failures: 3}, want: 1},
		{name: "usage error is 2", err: usageErrf("bad flag"), want: 2},
		{name: "wrapped usage error is 2", err: fmt.Errorf("context: %w", usageErrf("bad")), want: 2},
		{name: "interrupted is 130", err: errInterrupted, want: 130},
		{name: "wrapped interrupted is 130", err: fmt.Errorf("run: %w", errInterrupted), want: 130},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(tc.err); got != tc.want {
				t.Errorf("exitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestSyncFailedErrorMessage(t *testing.T) {
	err := &syncFailedError{failures: 2}
	if err.Error() != "completed with 2 error(s)" {
		t.Errorf("message = %q", err.Error())
	}
	if errors.Is(err, errInterrupted) {
		t.Error("syncFailedError must not match errInterrupted")
	}
}
