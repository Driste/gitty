package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestNoBuiltBinaryIsTracked: the repository holds source. A binary built in
// place and added by accident is worse than clutter here: gitty runs the
// gitty-<name> it finds next to itself, so a committed gitty-up for one
// machine is the one every other machine tries, and cannot, run.
func TestNoBuiltBinaryIsTracked(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		t.Skipf("not a git checkout, or no git: %v", err)
	}
	magics := map[string]string{
		"\x7fELF":          "an ELF executable",
		"\xfe\xed\xfa\xce": "a Mach-O executable",
		"\xfe\xed\xfa\xcf": "a Mach-O executable",
		"\xce\xfa\xed\xfe": "a Mach-O executable",
		"\xcf\xfa\xed\xfe": "a Mach-O executable",
		"\xca\xfe\xba\xbe": "a universal Mach-O executable",
		"MZ\x90\x00":       "a Windows executable",
	}
	for _, name := range strings.Split(string(out), "\x00") {
		if name == "" {
			continue
		}
		f, err := os.Open(name)
		if err != nil {
			continue // tracked and deleted in the working tree: not this test's business
		}
		head := make([]byte, 4)
		n, _ := f.Read(head)
		f.Close()
		for magic, what := range magics {
			if bytes.HasPrefix(head[:n], []byte(magic)) {
				t.Errorf("%s is tracked by git and is %s: remove it with `git rm --cached %s` and build it instead", name, what, name)
			}
		}
	}
}
