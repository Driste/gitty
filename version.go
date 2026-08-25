package main

import (
	"fmt"
	"runtime/debug"
)

// version is the release version of this binary. Release builds set it at
// link time with -ldflags "-X main.version=<tag>"; everything else keeps the
// "dev" default and falls back to the VCS revision Go stamps in.
var version = "dev"

// devVersion is the value of version in an unstamped build.
const devVersion = "dev"

// versionString reports the binary's version. A release build returns the tag
// exactly as it was passed at link time, so CI can assert on it byte-for-byte;
// a development build appends the commit Go recorded at build time, when there
// is one, so locally-built binaries are still identifiable.
func versionString() string {
	if version != devVersion {
		return version
	}
	return version + buildRevisionSuffix()
}

// buildRevisionSuffix returns "+<short-commit>[-dirty]" from the VCS
// information Go stamps into binaries built inside a repository, or "" when
// the build carries none.
func buildRevisionSuffix() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	rev, dirty := "", false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			rev = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if rev == "" {
		return ""
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return "+" + rev
}

// runVersion prints the version as a single bare line, so scripts and release
// smoke tests can compare it directly against the tag.
func runVersion() {
	fmt.Println(versionString())
}
