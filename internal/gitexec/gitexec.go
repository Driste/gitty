// Package gitexec wraps the local `git` binary behind a single replaceable
// surface. All Gitty code paths that invoke git go through Runner.Run.
package gitexec

import (
	"io"
	"os"
	"os/exec"
)

// Runner is the contract for invoking the git binary.
type Runner interface {
	Run(dir string, args ...string) error
}

// Real is the production Runner. It shells out to `git` with the user's
// environment passed through (so SSH agent and global gitconfig work).
type Real struct {
	Stdout, Stderr io.Writer
	Env            []string
}

// Run executes `git args...` with cmd.Dir = dir. When Env is nil the
// process inherits os.Environ(); when Stdout/Stderr are nil they default
// to os.Stdout/os.Stderr.
func (r *Real) Run(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if r.Env != nil {
		cmd.Env = r.Env
	} else {
		cmd.Env = os.Environ()
	}
	if r.Stdout != nil {
		cmd.Stdout = r.Stdout
	} else {
		cmd.Stdout = os.Stdout
	}
	if r.Stderr != nil {
		cmd.Stderr = r.Stderr
	} else {
		cmd.Stderr = os.Stderr
	}
	return cmd.Run()
}
