package main

import (
	"fmt"
	"os"
	"strings"
)

// gitty acts as its own GIT_ASKPASS helper: when a sync needs to authenticate
// an HTTP clone/pull, it re-execs this same binary (trigger: the
// GITTY_ASKPASS_MODE env var, checked first thing in main) with the token
// passed via the child git process's environment. This keeps the token out of
// argv (ps-safe), out of every git config file, and off disk entirely; the
// answer travels over git's private askpass pipe, not gitty's stdout/stderr.

// askpassAnswer selects the response for a git credential prompt. Git invokes
// the askpass helper once for the username and once for the password, passing
// the prompt text as the sole argument. Unrecognized prompts return ok=false
// so git fails fast rather than receiving a wrong secret.
func askpassAnswer(prompt string, getenv func(string) string) (string, bool) {
	switch {
	case strings.HasPrefix(prompt, "Username"):
		return getenv("GITTY_ASKPASS_USERNAME"), true
	case strings.HasPrefix(prompt, "Password"):
		return getenv("GITTY_ASKPASS_TOKEN"), true
	}
	return "", false
}

// runAskpass implements the GIT_ASKPASS protocol: answer on stdout, exit 0;
// exit 1 for anything we don't recognize.
func runAskpass(args []string) {
	prompt := ""
	if len(args) > 1 {
		prompt = args[1]
	}
	answer, ok := askpassAnswer(prompt, os.Getenv)
	if !ok {
		os.Exit(1)
	}
	fmt.Println(answer)
}
