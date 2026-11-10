//go:build !windows

package main

import "syscall"

// execPlugin replaces gitty with the plugin. The plugin then *is* the process
// the user (or an agent host) started: it owns stdin and stdout directly,
// receives every signal first-hand, and its exit code is the command's — with
// no intermediate gitty to relay any of it, or to be left behind if the
// plugin is killed. It returns only when the exec itself failed.
func execPlugin(path string, argv, env []string) (int, error) {
	return 2, syscall.Exec(path, argv, env)
}
