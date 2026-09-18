package main

import (
	"os"
)

// Terminal styling is applied only when output is going to an interactive
// terminal. The constitution forbids decorative ANSI in non-TTY contexts,
// where escapes would corrupt piped or redirected output.
type palette struct {
	group   string
	present string
	missing string
	dim     string
	reset   string
}

func newPalette(on bool) palette {
	if !on {
		return palette{}
	}
	return palette{
		group:   "\x1b[1;34m", // bold blue, like a directory
		present: "\x1b[32m",   // green: already checked out
		missing: "\x1b[33m",   // yellow: a sync would clone it
		dim:     "\x1b[2m",
		reset:   "\x1b[0m",
	}
}

// paint wraps s in style, and is a no-op for the zero palette.
func (p palette) paint(style, s string) string {
	if style == "" {
		return s
	}
	return style + s + p.reset
}

// isTerminal reports whether f is an interactive terminal.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// resolveColor turns --color=auto|always|never into a decision. "auto" means
// colour only for an interactive terminal, and honours NO_COLOR
// (https://no-color.org), which disables colour when set to any value.
func resolveColor(mode string, out *os.File) (bool, error) {
	switch mode {
	case "always":
		return true, nil
	case "never":
		return false, nil
	case "", "auto":
		if _, set := os.LookupEnv("NO_COLOR"); set {
			return false, nil
		}
		return isTerminal(out), nil
	}
	return false, usageErrf("--color must be auto, always, or never, got %q", mode)
}

// resolveLsFormat turns --format into a concrete renderer. Like ls(1), which
// prints columns to a terminal but one name per line when piped, "auto" gives
// a human tree on a terminal and the greppable event format otherwise, so
// scripts keep parsing stable output.
func resolveLsFormat(mode string, out *os.File) (string, error) {
	switch mode {
	case "tree", "text", "json":
		return mode, nil
	case "", "auto":
		if isTerminal(out) {
			return "tree", nil
		}
		return "text", nil
	}
	return "", usageErrf("--format must be auto, tree, text, or json, got %q", mode)
}
