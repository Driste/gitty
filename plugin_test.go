package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeStub writes an executable shell script and returns its path.
func writeStub(t *testing.T, dir, name, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("plugin stubs are shell scripts")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeConfigAt writes a workspace config into dir.
func writeConfigAt(t *testing.T, dir string, cfg Config) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfigTo(dir, &cfg); err != nil {
		t.Fatal(err)
	}
}

// realDir resolves symlinks in a temp dir path (macOS's /var → /private/var),
// so paths compare equal to what the code under test derives.
func realDir(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestFindPlugin(t *testing.T) {
	root := realDir(t, t.TempDir())
	beside := filepath.Join(root, "bin")
	onPath := filepath.Join(root, "path")
	exe := filepath.Join(beside, "gitty")
	if err := os.MkdirAll(beside, 0o755); err != nil {
		t.Fatal(err)
	}

	writeStub(t, onPath, "gitty-both", "exit 0")
	wantBeside := writeStub(t, beside, "gitty-both", "exit 0")
	wantPath := writeStub(t, onPath, "gitty-pathonly", "exit 0")
	// Present but not executable: not a plugin.
	if err := os.WriteFile(filepath.Join(onPath, "gitty-data"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", onPath)

	t.Run("beside the binary wins over PATH", func(t *testing.T) {
		got, ok := findPlugin("both", exe)
		if !ok || got != wantBeside {
			t.Errorf("findPlugin(both) = %q, %v; want %q", got, ok, wantBeside)
		}
	})
	t.Run("found on PATH", func(t *testing.T) {
		got, ok := findPlugin("pathonly", exe)
		if !ok || got != wantPath {
			t.Errorf("findPlugin(pathonly) = %q, %v; want %q", got, ok, wantPath)
		}
	})
	t.Run("PATH alone when the binary's own path is unknown", func(t *testing.T) {
		if got, ok := findPlugin("both", ""); !ok || got != filepath.Join(onPath, "gitty-both") {
			t.Errorf("findPlugin(both, no exe) = %q, %v", got, ok)
		}
	})
	t.Run("a non-executable file is not a plugin", func(t *testing.T) {
		if got, ok := findPlugin("data", exe); ok {
			t.Errorf("findPlugin(data) = %q, want not found", got)
		}
	})
	t.Run("absent", func(t *testing.T) {
		if got, ok := findPlugin("nope", exe); ok {
			t.Errorf("findPlugin(nope) = %q, want not found", got)
		}
	})
	t.Run("names that are not commands are never looked up", func(t *testing.T) {
		for _, name := range []string{"", "--help", "-h", "../both", "a/b", "both.sh", "a b", `a\b`} {
			if got, ok := findPlugin(name, exe); ok {
				t.Errorf("findPlugin(%q) = %q, want not found", name, got)
			}
		}
	})
	t.Run("the current directory is not searched", func(t *testing.T) {
		cwd := filepath.Join(root, "cwd")
		writeStub(t, cwd, "gitty-here", "exit 0")
		t.Chdir(cwd)
		t.Setenv("PATH", "."+string(os.PathListSeparator)+onPath)
		if got, ok := findPlugin("here", exe); ok {
			t.Errorf("findPlugin(here) = %q: picked a plugin up from the working directory", got)
		}
	})
	t.Run("a symlinked gitty finds plugins beside its target", func(t *testing.T) {
		linkDir := filepath.Join(root, "links")
		if err := os.MkdirAll(linkDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(linkDir, "gitty")
		if err := os.Symlink(exe, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Setenv("PATH", filepath.Join(root, "empty"))
		got, ok := findPlugin("both", link)
		if !ok || got != wantBeside {
			t.Errorf("findPlugin(both) via symlink = %q, %v; want %q", got, ok, wantBeside)
		}
	})
}

func TestInstalledPlugins(t *testing.T) {
	root := realDir(t, t.TempDir())
	beside := filepath.Join(root, "bin")
	onPath := filepath.Join(root, "path")
	exe := filepath.Join(beside, "gitty")

	writeStub(t, beside, "gitty-mcp", "exit 0")
	writeStub(t, onPath, "gitty-mcp", "exit 0") // shadowed by the one beside gitty
	writeStub(t, onPath, "gitty-audit", "exit 0")
	writeStub(t, onPath, "gitty-sync", "exit 0")     // a built-in: never a plugin
	writeStub(t, onPath, "gitty-notes.sh", "exit 0") // not a command name
	if err := os.WriteFile(filepath.Join(onPath, "gitty-readme"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", onPath+string(os.PathListSeparator)+"relative/dir")

	got := installedPlugins(exe)
	want := []plugin{
		{Name: "audit", Path: filepath.Join(onPath, "gitty-audit")},
		{Name: "mcp", Path: filepath.Join(beside, "gitty-mcp")},
	}
	if len(got) != len(want) {
		t.Fatalf("installedPlugins = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("installedPlugins[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// envValue returns the last value of name in an environment list, and
// whether it is set at all.
func envValue(env []string, name string) (string, bool) {
	value, found := "", false
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			value, found = v, true
		}
	}
	return value, found
}

func TestPluginEnv(t *testing.T) {
	root := realDir(t, t.TempDir())
	ws := filepath.Join(root, "ws")
	const instance = "https://gitlab.example.com"
	writeConfigAt(t, ws, Config{URL: instance, HTTP: true, RootPath: "acme"})
	// What `sync --groups` leaves in a group directory.
	writeConfigAt(t, filepath.Join(ws, "platform"), Config{URL: instance, HTTP: true, RootPath: "acme/platform"})
	deep := filepath.Join(ws, "platform", "team", "sub")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	base := []string{"HOME=/home/someone", "GITTY_GROUP=stale", "GITTY_WORKSPACE=/elsewhere", "GITLAB_TOKEN=secret"}

	t.Run("inside a workspace", func(t *testing.T) {
		env, note := pluginEnv(base, "/opt/gitty/gitty", deep)
		if note != nil {
			t.Fatalf("unexpected note: %v", note)
		}
		for name, want := range map[string]string{
			envPluginBin:       "/opt/gitty/gitty",
			envPluginWorkspace: ws, // the outermost config, not platform's
			envPluginGroup:     "acme/platform/team/sub",
			envPluginInstance:  instance,
			"HOME":             "/home/someone",
			// The caller's environment is passed through untouched: gitty
			// adds nothing credential-shaped and removes nothing either.
			"GITLAB_TOKEN": "secret",
		} {
			if got, ok := envValue(env, name); !ok || got != want {
				t.Errorf("%s = %q (set=%v), want %q", name, got, ok, want)
			}
		}
		for _, name := range []string{envPluginGroup, envPluginWorkspace} {
			n := 0
			for _, kv := range env {
				if strings.HasPrefix(kv, name+"=") {
					n++
				}
			}
			if n != 1 {
				t.Errorf("%s appears %d times, want exactly once (stale value must be replaced)", name, n)
			}
		}
	})

	t.Run("outside any workspace", func(t *testing.T) {
		env, note := pluginEnv(base, "/opt/gitty/gitty", root)
		if note != nil {
			t.Fatalf("being outside a workspace is not worth a note, got: %v", note)
		}
		if got, _ := envValue(env, envPluginBin); got != "/opt/gitty/gitty" {
			t.Errorf("%s = %q", envPluginBin, got)
		}
		for _, name := range []string{envPluginWorkspace, envPluginGroup, envPluginInstance} {
			if got, ok := envValue(env, name); !ok || got != "" {
				t.Errorf("%s = %q (set=%v), want set and empty", name, got, ok)
			}
		}
	})

	t.Run("an unreadable workspace is reported, not hidden", func(t *testing.T) {
		broken := filepath.Join(root, "broken")
		if err := os.MkdirAll(filepath.Join(broken, ConfigDir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(broken, ConfigDir, ConfigName), []byte("url = [not toml"), 0o644); err != nil {
			t.Fatal(err)
		}
		env, note := pluginEnv(base, "/opt/gitty/gitty", broken)
		if note == nil {
			t.Fatal("want a note explaining why the workspace could not be used")
		}
		if got, ok := envValue(env, envPluginWorkspace); !ok || got != "" {
			t.Errorf("%s = %q (set=%v), want set and empty", envPluginWorkspace, got, ok)
		}
	})
}

func TestWorkspaceRoot(t *testing.T) {
	const instance = "https://gitlab.example.com"
	read := func(t *testing.T, dir string) *Config {
		t.Helper()
		cfg, err := readConfigIn(dir)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	t.Run("climbs through configs written by sync --groups", func(t *testing.T) {
		ws := realDir(t, t.TempDir())
		writeConfigAt(t, ws, Config{URL: instance, RootPath: "acme"})
		writeConfigAt(t, filepath.Join(ws, "a"), Config{URL: instance + "/", RootPath: "acme/a"})
		inner := filepath.Join(ws, "a", "b", "c") // a/b has no config: they may be sparse
		writeConfigAt(t, inner, Config{URL: instance, RootPath: "acme/a/b/c"})

		top, cfg := workspaceRoot(inner, read(t, inner))
		if top != ws || cfg.RootPath != "acme" {
			t.Errorf("workspaceRoot = %q (root_path %q), want %q (acme)", top, cfg.RootPath, ws)
		}
	})

	t.Run("a workspace anchored at the instance top level", func(t *testing.T) {
		ws := realDir(t, t.TempDir())
		writeConfigAt(t, ws, Config{URL: instance, RootPath: ""})
		inner := filepath.Join(ws, "acme")
		writeConfigAt(t, inner, Config{URL: instance, RootPath: "acme"})

		top, cfg := workspaceRoot(inner, read(t, inner))
		if top != ws || cfg.RootPath != "" {
			t.Errorf("workspaceRoot = %q (root_path %q), want %q (empty)", top, cfg.RootPath, ws)
		}
	})

	t.Run("an enclosing workspace for another instance is not this one", func(t *testing.T) {
		outer := realDir(t, t.TempDir())
		writeConfigAt(t, outer, Config{URL: "https://gitlab.com", RootPath: ""})
		inner := filepath.Join(outer, "work")
		writeConfigAt(t, inner, Config{URL: instance, RootPath: "work"})

		if top, _ := workspaceRoot(inner, read(t, inner)); top != inner {
			t.Errorf("workspaceRoot = %q, want %q", top, inner)
		}
	})

	t.Run("an enclosing config whose paths do not line up is not this one", func(t *testing.T) {
		outer := realDir(t, t.TempDir())
		writeConfigAt(t, outer, Config{URL: instance, RootPath: "acme"})
		// A second, independent workspace of the same instance placed inside
		// the first: its directory name is not the group its root_path names.
		inner := filepath.Join(outer, "scratch")
		writeConfigAt(t, inner, Config{URL: instance, RootPath: "other"})

		if top, _ := workspaceRoot(inner, read(t, inner)); top != inner {
			t.Errorf("workspaceRoot = %q, want %q", top, inner)
		}
	})
}

// runGittyAt runs a gitty binary other than the shared e2e one, with stdin.
func runGittyAt(t *testing.T, bin, dir, stdin string, extraEnv []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = gittyEnv(extraEnv)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return stdout.String(), stderr.String(), ee.ExitCode()
		}
		t.Fatalf("running %s %v: %v\nstderr:\n%s", bin, args, err, stderr.String())
	}
	return stdout.String(), stderr.String(), 0
}

// TestE2EPluginDispatch drives the real binary: `gitty <cmd>` must hand the
// whole invocation — arguments, stdin, stdout, exit code, working directory —
// to gitty-<cmd>, with the workspace described in its environment.
func TestE2EPluginDispatch(t *testing.T) {
	skipIfShort(t)

	root := realDir(t, t.TempDir())
	pathDir := filepath.Join(root, "path")
	writeStub(t, pathDir, "gitty-echo", `
echo "args=$*"
echo "bin=$GITTY_BIN"
echo "workspace=$GITTY_WORKSPACE"
echo "group=$GITTY_GROUP"
echo "instance=$GITTY_INSTANCE"
echo "pwd=$(pwd)"
echo "stdin=$(cat)"
echo "diagnostic" >&2
exit 3`)
	withPath := []string{"PATH=" + pathDir + string(os.PathListSeparator) + os.Getenv("PATH")}

	ws := filepath.Join(root, "ws")
	const instance = "https://gitlab.example.com"
	writeConfigAt(t, ws, Config{URL: instance, HTTP: true, RootPath: "acme"})
	checkout := filepath.Join(ws, "platform", "app")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, checkout, "init", "-q", ".")
	inside := filepath.Join(checkout, "src")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("runs the plugin in place of gitty", func(t *testing.T) {
		stdout, stderr, code := runGittyAt(t, gittyBin, inside, "from stdin", withPath, "echo", "one", "--two", "-x=3")
		if code != 3 {
			t.Errorf("exit = %d, want the plugin's own 3\nstderr:\n%s", code, stderr)
		}
		for _, want := range []string{
			"args=one --two -x=3",
			"bin=" + gittyBin,
			"workspace=" + ws,
			// Inside a checkout the group is the one holding the project.
			"group=acme/platform",
			"instance=" + instance,
			"pwd=" + inside, // the plugin starts where the user was
			"stdin=from stdin",
		} {
			if !strings.Contains(stdout, want+"\n") {
				t.Errorf("stdout missing %q:\n%s", want, stdout)
			}
		}
		if strings.TrimSpace(stderr) != "diagnostic" {
			t.Errorf("stderr = %q, want only the plugin's own output", stderr)
		}
	})

	t.Run("outside a workspace the plugin still runs", func(t *testing.T) {
		stdout, _, code := runGittyAt(t, gittyBin, root, "", withPath, "echo")
		if code != 3 || !strings.Contains(stdout, "workspace=\n") || !strings.Contains(stdout, "bin="+gittyBin+"\n") {
			t.Errorf("exit = %d, stdout:\n%s", code, stdout)
		}
	})

	t.Run("a plugin beside the binary wins over PATH", func(t *testing.T) {
		binDir := filepath.Join(root, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(gittyBin)
		if err != nil {
			t.Fatal(err)
		}
		ownBin := filepath.Join(binDir, "gitty")
		if err := os.WriteFile(ownBin, data, 0o755); err != nil {
			t.Fatal(err)
		}
		writeStub(t, binDir, "gitty-echo", `echo "beside"`)
		stdout, stderr, code := runGittyAt(t, ownBin, root, "", withPath, "echo")
		if code != 0 || strings.TrimSpace(stdout) != "beside" {
			t.Errorf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
	})

	t.Run("a plugin cannot replace a built-in command", func(t *testing.T) {
		writeStub(t, pathDir, "gitty-version", `echo "shadowed"`)
		stdout, _, code := runGittyAt(t, gittyBin, root, "", withPath, "version")
		if code != 0 || strings.Contains(stdout, "shadowed") {
			t.Errorf("exit = %d, stdout = %q: a plugin shadowed a built-in", code, stdout)
		}
	})

	t.Run("no plugin: unknown command", func(t *testing.T) {
		stdout, stderr, code := runGittyAt(t, gittyBin, root, "", withPath, "frobnicate")
		if code != 2 || stdout != "" {
			t.Errorf("exit = %d, stdout = %q; want 2 and nothing on stdout", code, stdout)
		}
		for _, want := range []string{"Unknown command: frobnicate", "gitty-frobnicate", "on PATH", "Usage:", "echo"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr missing %q:\n%s", want, stderr)
			}
		}
	})

	t.Run("mcp without its plugin says how to get it", func(t *testing.T) {
		stdout, stderr, code := runGittyAt(t, gittyBin, root, "", withPath, "mcp")
		if code != 2 || stdout != "" {
			t.Errorf("exit = %d, stdout = %q; want 2 and nothing on stdout", code, stdout)
		}
		for _, want := range []string{"gitty-mcp", "not installed", releasesURL, "next to gitty"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr missing %q:\n%s", want, stderr)
			}
		}
		if strings.Contains(stderr, "Usage:") {
			t.Errorf("a known plugin's hint should not be buried under usage:\n%s", stderr)
		}
	})

	t.Run("a plugin that cannot be executed is an error, not a hang", func(t *testing.T) {
		// Executable bit set, but nothing the kernel can run.
		if err := os.WriteFile(filepath.Join(pathDir, "gitty-garbage"), []byte{0x00, 0x01, 0x02, 0x03}, 0o755); err != nil {
			t.Fatal(err)
		}
		_, stderr, code := runGittyAt(t, gittyBin, root, "", withPath, "garbage")
		if code != 2 || !strings.Contains(stderr, "gitty-garbage") {
			t.Errorf("exit = %d, stderr:\n%s", code, stderr)
		}
	})
}
