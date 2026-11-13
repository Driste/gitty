package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"unicode"
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

// readmeAnchors returns the anchor of every heading in a Markdown file, as
// GitHub makes them: lower-cased, punctuation dropped, spaces to hyphens,
// and a number added to a slug already taken. Headings inside a code fence
// are text, not headings.
func readmeAnchors(md string) map[string]bool {
	anchors, fenced := map[string]bool{}, false
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		title := strings.TrimLeft(line, "#")
		if fenced || title == line || !strings.HasPrefix(title, " ") {
			continue
		}
		var slug strings.Builder
		for _, r := range strings.ToLower(strings.TrimSpace(title)) {
			switch {
			case r == ' ':
				slug.WriteByte('-')
			case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
				slug.WriteRune(r)
			}
		}
		anchor := slug.String()
		for n := 1; anchors[anchor]; n++ {
			anchor = fmt.Sprintf("%s-%d", slug.String(), n)
		}
		anchors[anchor] = true
	}
	return anchors
}

// TestReadmeCommandTable: the table at the top of the README is how a
// reader gets to a command. Every command has a row there, and every link
// in the file that points into the file lands on a heading — a section
// renamed without its links is the way such a table quietly stops working.
func TestReadmeCommandTable(t *testing.T) {
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	md := string(data)
	anchors := readmeAnchors(md)
	for _, m := range regexp.MustCompile(`\]\(#([^)\s]*)\)`).FindAllStringSubmatch(md, -1) {
		if !anchors[m[1]] {
			t.Errorf("README.md links to #%s, and no heading makes that anchor", m[1])
		}
	}

	start := strings.Index(md, "\n## Commands\n")
	if start < 0 {
		t.Fatal("README.md has no Commands section")
	}
	table := md[start+1:]
	if end := strings.Index(table[1:], "\n## "); end >= 0 {
		table = table[:end+1]
	}
	names := []string{"up"} // published with gitty, and run as one of its commands
	for _, c := range commands {
		names = append(names, c.name)
	}
	for _, name := range names {
		if !strings.Contains(table, "| [`gitty "+name+"`](#") {
			t.Errorf("the README's command table has no linked row for `gitty %s`", name)
		}
	}
}
