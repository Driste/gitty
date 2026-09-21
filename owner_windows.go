//go:build windows

package main

// ownedByCurrentUser has no cheap portable answer on Windows; the workspace
// search does not apply an ownership check there.
func ownedByCurrentUser(path string) bool { return true }

// deviceOf reports no filesystem identity on Windows, so a workspace search
// crosses drive boundaries there.
func deviceOf(path string) uint64 { return 0 }
