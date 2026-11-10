package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Every shell's script names every command and every flag of every command,
// so the scripts cannot drift from the binary they describe.
func TestCompletionCoversCommandsAndFlags(t *testing.T) {
	for _, shell := range completionShells {
		t.Run(shell, func(t *testing.T) {
			var out bytes.Buffer
			if err := runCompletion(&out, []string{shell}); err != nil {
				t.Fatal(err)
			}
			script := out.String()
			for _, c := range commands {
				if !strings.Contains(script, c.name) {
					t.Errorf("command %q missing", c.name)
				}
				for _, f := range commandFlags(c) {
					if !strings.Contains(script, "--"+f.name) && !strings.Contains(script, "-l "+f.name) {
						t.Errorf("%s: flag --%s missing", c.name, f.name)
					}
				}
			}
		})
	}
}

func TestCompletionGroupArgumentsCompleteAsDirectories(t *testing.T) {
	want := map[string]string{
		"bash": "compgen -d",
		"zsh":  "_path_files -/",
		"fish": "__fish_complete_directories",
	}
	for shell, marker := range want {
		var out bytes.Buffer
		if err := runCompletion(&out, []string{shell}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), marker) {
			t.Errorf("%s: directory completion marker %q missing", shell, marker)
		}
	}
}

func TestCompletionRejectsUnknownShell(t *testing.T) {
	var out bytes.Buffer
	if err := runCompletion(&out, []string{"powershell"}); err == nil || exitCode(err) != 2 {
		t.Errorf("want a usage error, got %v", err)
	}
	if err := runCompletion(&out, nil); err == nil || exitCode(err) != 2 {
		t.Errorf("want a usage error for no shell, got %v", err)
	}
}

// The generated bash and zsh scripts must at least parse, where those shells
// are available.
func TestCompletionScriptsParse(t *testing.T) {
	for _, shell := range []string{"bash", "zsh"} {
		path, err := exec.LookPath(shell)
		if err != nil {
			t.Logf("%s not installed; skipping syntax check", shell)
			continue
		}
		var out bytes.Buffer
		if err := runCompletion(&out, []string{shell}); err != nil {
			t.Fatal(err)
		}
		f, err := os.CreateTemp(t.TempDir(), "gitty-completion-*")
		if err != nil {
			t.Fatal(err)
		}
		f.Write(out.Bytes())
		f.Close()
		cmd := exec.Command(path, "-n", f.Name())
		if msg, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s -n rejected the generated script: %v\n%s\n--- script ---\n%s", shell, err, msg, out.String())
		}
	}
}

// Flag descriptions carry characters that would break the scripts' quoting;
// the sanitiser removes exactly those.
func TestPlainDescription(t *testing.T) {
	in := "Move aside 'broken' dirs [renamed]: never \"deleted\"\n  (really)"
	got := plainDescription(in)
	for _, bad := range []string{"'", "\"", "[", "]", ":", "\n"} {
		if strings.Contains(got, bad) {
			t.Errorf("sanitised text still contains %q: %q", bad, got)
		}
	}
	if !strings.Contains(got, "(renamed)") || !strings.Contains(got, "never deleted") {
		t.Errorf("sanitised text lost its words: %q", got)
	}
}
