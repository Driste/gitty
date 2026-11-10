//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
)

// execPlugin runs the plugin as a child and waits for it: Windows cannot
// replace a process image. The child inherits the console, so Ctrl-C reaches
// it directly; gitty ignores the interrupt in order to outlive the plugin and
// pass its exit code on.
func execPlugin(path string, argv, env []string) (int, error) {
	cmd := exec.Command(path, argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = env
	signal.Ignore(os.Interrupt)
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 2, err
	}
	return 0, nil
}
