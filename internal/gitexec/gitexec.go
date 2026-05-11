// Package gitexec wraps the local `git` binary behind a single replaceable
// surface. All Gitty code paths that invoke git go through Runner.Run.
package gitexec

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
)

// Runner is the contract for invoking the git binary.
//
// Run executes `git <args...>` with cmd.Dir = dir. The returned byte slice is
// whatever git wrote to stderr during the invocation; the caller decides
// whether to forward it to the user (e.g., on a non-nil error). Git's stdout
// is discarded — gitty never reads it. The provided context cancels the
// child process if it expires.
type Runner interface {
	Run(ctx context.Context, dir string, args ...string) ([]byte, error)
}

// Real is the production Runner. It shells out to `git` with the user's
// environment passed through (so SSH agent and global gitconfig work) and
// captures stderr into an in-memory buffer per call. Stateless beyond Env;
// safe to share across goroutines.
type Real struct {
	// Env optionally overrides the child process environment. When nil
	// the process inherits os.Environ().
	Env []string
}

// Run executes `git args...` in dir, discards git's stdout, captures git's
// stderr, and returns the captured bytes plus the command's exit error.
func (r *Real) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if r.Env != nil {
		cmd.Env = r.Env
	} else {
		cmd.Env = os.Environ()
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stderr.Bytes(), err
}
