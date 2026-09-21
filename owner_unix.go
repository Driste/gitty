//go:build !windows

package main

import (
	"os"
	"syscall"
)

// ownedByCurrentUser reports whether path belongs to the user running gitty.
// Root's files count as the user's own, as they do for git: a system-managed
// workspace root is not a hostile one.
func ownedByCurrentUser(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return true // no ownership information to check against
	}
	uid := uint32(os.Geteuid())
	return st.Uid == uid || st.Uid == 0
}

// deviceOf identifies the filesystem holding path, so a workspace search can
// stop at a mount boundary. 0 means unknown.
func deviceOf(path string) uint64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev)
	}
	return 0
}
