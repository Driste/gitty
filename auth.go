package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gitlab.com/gitlab-org/api/client-go"
)

// GitLab personal access token scopes that matter to gitty.
//
// Listing groups and projects needs api or read_api. Cloning over HTTP(S)
// needs api, read_repository or write_repository — read_api alone is not
// enough, which is the trap: such a token lists everything perfectly and then
// fails every clone.
const (
	scopeAPI       = "api"
	scopeReadAPI   = "read_api"
	scopeReadRepo  = "read_repository"
	scopeWriteRepo = "write_repository"
)

// scopesAllowAPI reports whether these scopes can list groups and projects.
func scopesAllowAPI(scopes []string) bool {
	return hasAnyScope(scopes, scopeAPI, scopeReadAPI)
}

// scopesAllowHTTPClone reports whether these scopes can clone over HTTP(S).
func scopesAllowHTTPClone(scopes []string) bool {
	return hasAnyScope(scopes, scopeAPI, scopeReadRepo, scopeWriteRepo)
}

func hasAnyScope(scopes []string, want ...string) bool {
	for _, s := range scopes {
		for _, w := range want {
			if s == w {
				return true
			}
		}
	}
	return false
}

// tokenCheck is what gitty could learn about the configured credential. Every
// field is best-effort: an instance may not support token introspection, and
// the check must never be the reason a command fails.
type tokenCheck struct {
	reachable    bool // the instance answered at all
	unauthorized bool // it answered, and rejected the token
	user         string
	scopes       []string
	scopesKnown  bool
	revoked      bool
	expiresAt    string
	// skipped explains why introspection was not attempted at all.
	skipped string
	err     error
}

// verifyToken asks the instance who the token belongs to and, where supported,
// what the token is allowed to do.
func verifyToken(ctx context.Context, client *gitlab.Client, cred credential) tokenCheck {
	var chk tokenCheck

	// CI job tokens are not personal access tokens: they cannot be
	// introspected, and the identity endpoints do not apply to them.
	if cred.source == "CI_JOB_TOKEN" {
		chk.skipped = "CI job tokens cannot be introspected; gitty will use it as-is"
		return chk
	}

	user, resp, err := client.Users.CurrentUser(gitlab.WithContext(ctx))
	if err != nil {
		if resp != nil && (resp.StatusCode == 401 || resp.StatusCode == 403) {
			chk.reachable = true
			chk.unauthorized = true
			return chk
		}
		chk.err = err
		return chk
	}
	chk.reachable = true
	if user != nil {
		chk.user = user.Username
	}

	// Scope introspection is only available on newer instances and only for
	// personal access tokens; treat any failure as "unknown", not an error.
	pat, _, err := client.PersonalAccessTokens.GetSinglePersonalAccessToken(gitlab.WithContext(ctx))
	if err != nil || pat == nil {
		return chk
	}
	chk.scopesKnown = true
	chk.scopes = pat.Scopes
	chk.revoked = pat.Revoked
	if pat.ExpiresAt != nil {
		chk.expiresAt = pat.ExpiresAt.String()
	}
	return chk
}

// tokenSetupHint tells the user how to provide a token, tailored to the
// workspace's transport.
func tokenSetupHint(cfg *Config) string {
	scopes := "the " + scopeReadAPI + " scope"
	if cfg.HTTP {
		scopes = "the " + scopeReadAPI + " and " + scopeReadRepo + " scopes"
	}
	return fmt.Sprintf(
		"  Create one at %s/-/user_settings/personal_access_tokens with %s, then:\n"+
			"    export GITLAB_TOKEN=<your token>\n"+
			"  Or sync public groups without a token using 'gitty sync --anon'.",
		strings.TrimSuffix(cfg.URL, "/"), scopes)
}

// reportAuth prints what the check found, and what to do about it. It writes
// to stderr: this is guidance, not the command's primary output.
func reportAuth(w io.Writer, cfg *Config, cred credential, chk tokenCheck) {
	if cred.token == "" {
		fmt.Fprintln(w, "No GitLab token found (checked --token, GITLAB_TOKEN, CI_JOB_TOKEN).")
		fmt.Fprintln(w, "  'gitty sync' needs one to list groups"+cloneNeedSuffix(cfg)+".")
		fmt.Fprintln(w, tokenSetupHint(cfg))
		return
	}

	if chk.skipped != "" {
		fmt.Fprintf(w, "Token from %s: %s.\n", cred.source, chk.skipped)
		return
	}
	if chk.err != nil {
		fmt.Fprintf(w, "Could not verify the token from %s against %s: %v\n", cred.source, cfg.URL, chk.err)
		fmt.Fprintln(w, "  Re-check once the instance is reachable.")
		return
	}
	if chk.unauthorized {
		fmt.Fprintf(w, "The token from %s was REJECTED by %s.\n", cred.source, cfg.URL)
		fmt.Fprintln(w, "  It is expired, revoked, or belongs to a different instance.")
		fmt.Fprintln(w, tokenSetupHint(cfg))
		return
	}

	who := "the token"
	if chk.user != "" {
		who = "@" + chk.user
	}
	fmt.Fprintf(w, "Token from %s authenticated as %s.\n", cred.source, who)

	if !chk.scopesKnown {
		fmt.Fprintln(w, "  This instance did not report the token's scopes, so gitty could not")
		fmt.Fprintln(w, "  check them. If clones fail with an authentication error, the token")
		fmt.Fprintln(w, "  likely lacks read_repository.")
		return
	}

	fmt.Fprintf(w, "  Scopes: %s\n", strings.Join(chk.scopes, ", "))
	if chk.revoked {
		fmt.Fprintln(w, "  WARNING: this token is marked revoked; syncing will fail.")
	}
	if chk.expiresAt != "" {
		fmt.Fprintf(w, "  Expires: %s\n", chk.expiresAt)
	}

	if !scopesAllowAPI(chk.scopes) {
		fmt.Fprintf(w, "  WARNING: no %s or %s scope — gitty cannot list groups or projects.\n",
			scopeAPI, scopeReadAPI)
	}
	if cfg.HTTP && !scopesAllowHTTPClone(chk.scopes) {
		fmt.Fprintf(w, "  WARNING: no %s scope — this workspace clones over HTTP(S), so every\n",
			scopeReadRepo)
		fmt.Fprintf(w, "  clone will fail even though listing works. Add %s to the token, or\n", scopeReadRepo)
		fmt.Fprintln(w, "  re-run 'gitty init --force --ssh' to clone with SSH keys instead.")
	}
}

func cloneNeedSuffix(cfg *Config) string {
	if cfg.HTTP {
		return " and to clone over HTTP(S)"
	}
	return ""
}

// checkAuth verifies the configured credential against the workspace's
// instance and reports the result to w. The check is a quick courtesy, not a
// sync: it skips the client's retry/backoff loop and is bounded by a short
// timeout, so an unreachable instance costs seconds, not minutes.
func checkAuth(w io.Writer, cfg *Config, tokenFlag string) (credential, tokenCheck) {
	cred := resolveCredential(tokenFlag)
	if cred.token == "" {
		reportAuth(w, cfg, cred, tokenCheck{})
		return cred, tokenCheck{}
	}

	client, err := gitlab.NewClient(cred.token,
		gitlab.WithBaseURL(cfg.URL),
		gitlab.WithoutRetries(),
	)
	if err != nil {
		chk := tokenCheck{err: err}
		reportAuth(w, cfg, cred, chk)
		return cred, chk
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	chk := verifyToken(ctx, client, cred)
	reportAuth(w, cfg, cred, chk)
	return cred, chk
}

// checkAuthForInit runs the check for a freshly created workspace. It is
// advisory: the workspace already exists, so a network failure or a bad token
// is reported, never fatal.
func checkAuthForInit(w io.Writer, cfg *Config, tokenFlag string) {
	checkAuth(w, cfg, tokenFlag)
}

// runAuth is `gitty auth`: the same check, on demand, for an existing
// workspace — after rotating a token, before a long sync, or when a sync just
// failed with an authentication error. Unlike at init it has a verdict: a
// missing, rejected or revoked token exits 2 (fix the configuration), an
// instance that could not be reached exits 1 (try again), and a token that
// works exits 0. The report itself is the command's output, so it goes to
// stdout.
func runAuth(tokenFlag string) error {
	cfg, err := DiscoverWorkspace()
	if err != nil {
		return err
	}
	fmt.Printf("Instance %s, cloning over %s\n", cfg.URL, transportName(cfg))
	cred, chk := checkAuth(os.Stdout, cfg, tokenFlag)
	switch {
	case cred.token == "":
		return usageErrf("no token configured")
	case chk.skipped != "":
		return nil // a CI job token: nothing to verify, nothing wrong
	case chk.err != nil:
		return fmt.Errorf("could not verify the token: %w", chk.err)
	case chk.unauthorized:
		return usageErrf("the token was rejected by %s", cfg.URL)
	case chk.revoked:
		return usageErrf("the token is revoked")
	case chk.scopesKnown && !scopesAllowAPI(chk.scopes):
		return usageErrf("the token cannot read the API")
	case chk.scopesKnown && cfg.HTTP && !scopesAllowHTTPClone(chk.scopes):
		return usageErrf("the token cannot clone over HTTP(S)")
	}
	return nil
}

// transportName describes a workspace's clone transport for humans.
func transportName(cfg *Config) string {
	if cfg.HTTP {
		return "HTTP(S)"
	}
	return "SSH"
}
