package main

import (
	"errors"
	"fmt"
)

// Exit-code taxonomy (part of the public CLI contract, documented in the
// agent schema):
//
//	0   clean run
//	1   sync completed but one or more items failed
//	2   usage or configuration error (bad flags, missing workspace, no token)
//	130 interrupted (SIGINT/SIGTERM)
//
// The flag package's ExitOnError FlagSets independently exit 2 on unparseable
// flags, which this taxonomy deliberately matches.

// usageError marks a problem the user must fix before a sync can even start.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usageErrf(format string, a ...any) error {
	return &usageError{msg: fmt.Sprintf(format, a...)}
}

// syncFailedError reports a run that finished but had per-item failures.
type syncFailedError struct{ failures int }

func (e *syncFailedError) Error() string {
	return fmt.Sprintf("completed with %d error(s)", e.failures)
}

// errInterrupted reports a run cut short by SIGINT/SIGTERM. It wins over
// syncFailedError: an interrupted run exits 130 even if items had failed.
var errInterrupted = errors.New("interrupted")

// exitCode maps an error from a command function onto the process exit code.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, errInterrupted) {
		return 130
	}
	var ue *usageError
	if errors.As(err, &ue) {
		return 2
	}
	return 1
}
