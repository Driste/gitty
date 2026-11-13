package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
)

// Plugins.
//
// A command gitty does not implement is looked up as an executable named
// gitty-<command> — beside the running gitty binary first, then on PATH — and
// run in gitty's place, the way git runs git-<command>. A plugin is therefore
// installed by putting a file somewhere, and removed by deleting it; gitty
// itself neither ships nor requires any.
//
// Built-in commands are dispatched before this is ever consulted, so a plugin
// can add a command but never replace one.
//
// A plugin starts in the directory the user ran gitty in, with the user's
// environment plus four variables describing what gitty's own workspace
// discovery found (see pluginEnv). No credential is among them: a plugin that
// needs the API runs gitty, which resolves a token the way it always does.

// pluginPrefix is what a plugin's executable name starts with.
const pluginPrefix = "gitty-"

// Environment variables a plugin is started with.
const (
	envPluginBin       = "GITTY_BIN"       // the gitty that launched the plugin
	envPluginWorkspace = "GITTY_WORKSPACE" // the workspace's root directory
	envPluginGroup     = "GITTY_GROUP"     // namespace path of the group the working directory is in
	envPluginInstance  = "GITTY_INSTANCE"  // the instance URL from the workspace config
)

// pluginNameRe is what a command must look like to be looked up as a plugin.
// It keeps flags (`gitty --help`), paths and anything with a separator from
// ever being turned into a file name to execute.
var pluginNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// knownPlugins are the plugins gitty's own releases publish. A missing one is
// answered with what it is and where to get it, rather than "unknown command".
var knownPlugins = map[string]string{
	"up": "is the instance's shared context: it serves what is known about its projects to coding agents, over the Model Context Protocol",
}

// releasesURL is where a plugin published with gitty is downloaded from.
const releasesURL = "https://github.com/Driste/gitty/releases"

// pluginDirs returns the directories searched before PATH: the one holding
// the gitty binary as it was invoked and, when that is a symlink, the one
// holding what it points at. An archive unpacked in one place just works, and
// so does a gitty linked into a bin directory from somewhere else.
func pluginDirs(exe string) []string {
	if exe == "" {
		return nil
	}
	dirs := []string{filepath.Dir(exe)}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		if d := filepath.Dir(resolved); d != dirs[0] {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// findPlugin returns the executable implementing a plugin command. exe is the
// path of the running gitty binary ("" when it could not be determined, which
// leaves the PATH lookup).
//
// exec.LookPath does the checking in both places: it requires an executable
// file (trying the platform's executable extensions on Windows), and it
// refuses a match that PATH only reaches through the current directory — so a
// file named gitty-something in the directory a user happens to be in is
// never picked up.
func findPlugin(name, exe string) (string, bool) {
	if !pluginNameRe.MatchString(name) {
		return "", false
	}
	bin := pluginPrefix + name
	for _, dir := range pluginDirs(exe) {
		if p, err := exec.LookPath(filepath.Join(dir, bin)); err == nil && !isSameFile(p, exe) {
			return p, true
		}
	}
	if p, err := exec.LookPath(bin); err == nil && !isSameFile(p, exe) {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		return p, true
	}
	return "", false
}

// isSameFile reports whether two paths are the same file. A gitty binary
// kept under a name like gitty-dev is not a plugin of itself: running it as
// one would start gitty again, with the same arguments, for ever.
func isSameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// plugin is one installed plugin: the command it adds and the file behind it.
type plugin struct {
	Name string
	Path string
}

// installedPlugins lists every plugin findPlugin could run, in name order.
// Where two files provide the same command the one findPlugin would choose is
// reported. It is what usage and shell completion show.
func installedPlugins(exe string) []plugin {
	dirs := append(pluginDirs(exe), filepath.SplitList(os.Getenv("PATH"))...)
	seen := map[string]bool{}
	var out []plugin
	for _, dir := range dirs {
		if dir == "" || !filepath.IsAbs(dir) {
			continue // a relative PATH entry is the current directory in disguise
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			base := e.Name()
			if !strings.HasPrefix(base, pluginPrefix) {
				continue
			}
			name := strings.TrimPrefix(base, pluginPrefix)
			// On Windows the file carries an executable extension the
			// command does not.
			if ext := filepath.Ext(name); ext != "" && strings.EqualFold(ext, ".exe") {
				name = strings.TrimSuffix(name, ext)
			}
			if seen[name] || !pluginNameRe.MatchString(name) || isBuiltinCommand(name) {
				continue
			}
			p, ok := findPlugin(name, exe)
			if !ok {
				continue // not executable
			}
			seen[name] = true
			out = append(out, plugin{Name: name, Path: p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// isBuiltinCommand reports whether gitty implements a command itself.
func isBuiltinCommand(name string) bool {
	for _, c := range commands {
		if c.name == name {
			return true
		}
	}
	return false
}

// pluginEnv builds the environment a plugin starts with: the caller's, with
// the four GITTY_ variables set from what discovery finds for wd.
//
// The variables are always set, so a plugin can tell "started by gitty,
// outside any workspace" (GITTY_BIN set, GITTY_WORKSPACE empty) from "not
// started by gitty" (neither set), and a value inherited from an enclosing
// gitty can never leak through. Being outside a workspace is not an error
// here — whether a workspace is needed is the plugin's business — but a
// workspace that exists and was refused or could not be read is returned as
// a note for the user, since the plugin would otherwise only be able to say
// "no workspace".
func pluginEnv(base []string, exe, wd string) (env []string, note error) {
	values := map[string]string{
		envPluginBin:       exe,
		envPluginWorkspace: "",
		envPluginGroup:     "",
		envPluginInstance:  "",
	}
	if loc, err := locateWorkspace(wd); err == nil {
		root, rootCfg := workspaceRoot(loc.ConfigDir, loc.Config)
		values[envPluginWorkspace] = root
		values[envPluginGroup] = loc.Group()
		values[envPluginInstance] = rootCfg.URL
	} else if !errors.Is(err, errNoWorkspace) {
		note = err
	}

	env = make([]string, 0, len(base)+len(values))
	for _, kv := range base {
		if name, _, ok := strings.Cut(kv, "="); ok {
			if _, ours := values[name]; ours {
				continue
			}
		}
		env = append(env, kv)
	}
	for _, name := range []string{envPluginBin, envPluginWorkspace, envPluginGroup, envPluginInstance} {
		env = append(env, name+"="+values[name])
	}
	return env, note
}

// runPluginCommand handles a command gitty does not implement: it runs the
// plugin of that name, or explains that there is none. The return value is
// the process exit code; on platforms where gitty can replace itself with the
// plugin, a successful launch never returns.
func runPluginCommand(errOut io.Writer, name string, args []string) int {
	exe, err := os.Executable()
	if err != nil {
		exe = "" // PATH is still searched; GITTY_BIN is left empty
	}
	pluginPath, ok := findPlugin(name, exe)
	if !ok {
		reportUnknownCommand(errOut, name, exe)
		return 2
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(errOut, "Error: determining working directory: %v\n", err)
		return 2
	}
	env, note := pluginEnv(os.Environ(), exe, wd)
	if note != nil {
		fmt.Fprintf(errOut, "note: %v\n", note)
	}
	code, err := execPlugin(pluginPath, append([]string{pluginPath}, args...), env)
	if err != nil {
		reportPluginWontRun(errOut, name, pluginPath, err)
		return 2
	}
	return code
}

// reportPluginWontRun says why a plugin that was found could not be started.
// The commonest reason deserves more than the system's two words for it: a
// file that this machine cannot execute at all, which is nearly always a
// binary built for another operating system or processor — one downloaded
// for the wrong platform, or left over from a build somewhere else.
func reportPluginWontRun(errOut io.Writer, name, pluginPath string, err error) {
	if !errors.Is(err, syscall.ENOEXEC) {
		fmt.Fprintf(errOut, "Error: running %s: %v\n", pluginPath, err)
		return
	}
	bin := "gitty-" + name
	fmt.Fprintf(errOut, "Error: %s cannot be run on this machine (%v).\n", pluginPath, err)
	fmt.Fprintf(errOut, "It was built for another operating system or processor than this one (%s/%s), or is a script with no #! line.\n", runtime.GOOS, runtime.GOARCH)
	if _, known := knownPlugins[name]; known {
		fmt.Fprintf(errOut, "Replace it with the %s for this platform from %s,\nor build it here from gitty's source:\n\n    go build -o %s ./cmd/%s\n", bin, releasesURL, pluginPath, bin)
	}
}

// reportUnknownCommand says that a command is neither built in nor provided
// by a plugin, naming the file that was looked for and where. For a plugin
// gitty's releases publish it says how to get it instead of listing commands.
func reportUnknownCommand(errOut io.Writer, name, exe string) {
	if !pluginNameRe.MatchString(name) {
		fmt.Fprintf(errOut, "Unknown command: %s\n", name)
		printUsage()
		return
	}
	bin := pluginPrefix + name
	where := "on PATH"
	if dirs := pluginDirs(exe); len(dirs) > 0 {
		where = "in " + strings.Join(dirs, ", ") + " or on PATH"
	}
	if what, known := knownPlugins[name]; known {
		fmt.Fprintf(errOut, "gitty %s needs the %s plugin, which is not installed.\n", name, bin)
		fmt.Fprintf(errOut, "Looked for %s %s.\n", bin, where)
		fmt.Fprintf(errOut, "%s %s.\n", bin, what)
		fmt.Fprintf(errOut, "Every gitty release publishes it at %s:\nput it next to gitty, or anywhere on PATH.\n", releasesURL)
		return
	}
	fmt.Fprintf(errOut, "Unknown command: %s (no %s %s)\n", name, bin, where)
	printUsage()
}
