package main

import (
	"context"
	"strings"
	"testing"

	"gitlab.com/gitlab-org/api/client-go"
)

func TestAskpassAnswer(t *testing.T) {
	env := map[string]string{
		"GITTY_ASKPASS_USERNAME": "oauth2",
		"GITTY_ASKPASS_TOKEN":    "glpat-tok",
	}
	getenv := func(k string) string { return env[k] }

	tests := []struct {
		name   string
		prompt string
		want   string
		wantOK bool
	}{
		{name: "username prompt", prompt: "Username for 'https://gitlab.com': ", want: "oauth2", wantOK: true},
		{name: "password prompt", prompt: "Password for 'https://oauth2@gitlab.com': ", want: "glpat-tok", wantOK: true},
		{name: "unknown prompt refused", prompt: "Passphrase for key '/root/.ssh/id_rsa': ", want: "", wantOK: false},
		{name: "empty prompt refused", prompt: "", want: "", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := askpassAnswer(tc.prompt, getenv)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("askpassAnswer(%q) = (%q, %v), want (%q, %v)", tc.prompt, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestResolveCredential(t *testing.T) {
	tests := []struct {
		name         string
		flagToken    string
		gitlabEnv    string
		ciEnv        string
		wantToken    string
		wantUsername string
		wantSource   string
	}{
		{name: "flag is a PAT", flagToken: "ft", gitlabEnv: "gt", ciEnv: "ct", wantToken: "ft", wantUsername: "oauth2", wantSource: "flag"},
		{name: "GITLAB_TOKEN is a PAT", gitlabEnv: "gt", ciEnv: "ct", wantToken: "gt", wantUsername: "oauth2", wantSource: "GITLAB_TOKEN"},
		{name: "CI_JOB_TOKEN uses ci username", ciEnv: "ct", wantToken: "ct", wantUsername: "gitlab-ci-token", wantSource: "CI_JOB_TOKEN"},
		{name: "nothing set", wantToken: "", wantUsername: "", wantSource: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITLAB_TOKEN", tc.gitlabEnv)
			t.Setenv("CI_JOB_TOKEN", tc.ciEnv)
			got := resolveCredential(tc.flagToken)
			if got.token != tc.wantToken || got.username != tc.wantUsername || got.source != tc.wantSource {
				t.Errorf("resolveCredential(%q) = %+v, want token=%q username=%q source=%q",
					tc.flagToken, got, tc.wantToken, tc.wantUsername, tc.wantSource)
			}
		})
	}
}

func TestCredentialEnv(t *testing.T) {
	t.Run("nil without HTTP", func(t *testing.T) {
		s := &syncer{cfg: &Config{HTTP: false}, cred: credential{token: "t", username: "oauth2"}, exePath: "/bin/gitty"}
		if env := s.credentialEnv(); env != nil {
			t.Errorf("SSH mode must not inject: %v", env)
		}
	})
	t.Run("nil without token", func(t *testing.T) {
		s := &syncer{cfg: &Config{HTTP: true}, exePath: "/bin/gitty"}
		if env := s.credentialEnv(); env != nil {
			t.Errorf("anonymous mode must not inject: %v", env)
		}
	})
	t.Run("populated for http with token", func(t *testing.T) {
		s := &syncer{cfg: &Config{HTTP: true}, cred: credential{token: "sekret", username: "gitlab-ci-token"}, exePath: "/bin/gitty"}
		env := s.credentialEnv()
		joined := strings.Join(env, "\n")
		for _, want := range []string{
			"GIT_ASKPASS=/bin/gitty",
			"GITTY_ASKPASS_MODE=1",
			"GITTY_ASKPASS_USERNAME=gitlab-ci-token",
			"GITTY_ASKPASS_TOKEN=sekret",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("credentialEnv missing %q: %v", want, env)
			}
		}
	})
}

// TestInjectedCloneInvocation verifies the argv shape and env of an
// authenticated clone: helper-list reset, no token anywhere in argv.
func TestInjectedCloneInvocation(t *testing.T) {
	t.Chdir(t.TempDir())

	rec := &recordingGit{}
	s, stdout, _ := newTestSyncer(
		&Config{URL: "https://gitlab.com", HTTP: true},
		fakeSource{
			projects: map[string][]*gitlab.Project{
				"acme": {{PathWithNamespace: "acme/repo", HTTPURLToRepo: "https://gitlab.com/acme/repo.git"}},
			},
		},
		rec.run,
	)
	s.cred = credential{token: "glpat-sekret", username: "oauth2", source: "flag"}
	s.exePath = "/bin/gitty"

	s.syncRepos(context.Background(), "acme")
	net := rec.networkCalls()
	if len(net) != 1 {
		t.Fatalf("network git calls: %v (all: %v)", net, rec.calls)
	}
	got := net[0] // dir + args
	// The fetch runs inside the new repository and carries the one option
	// pair gitty prepends — the credential helper reset.
	if sub := gitSubArgs(got); len(sub) != 3 || sub[0] != "fetch" || got[0] != "acme/repo" {
		t.Errorf("injected fetch = %v, want a fetch inside acme/repo", got)
	}
	if u := rec.remoteAddURL(); u != "https://gitlab.com/acme/repo.git" {
		t.Errorf("origin URL = %q, want the https URL", u)
	}
	joinedArgs := strings.Join(got, " ")
	if !strings.Contains(joinedArgs, "-c credential.helper=") {
		t.Errorf("injected clone missing the credential-helper reset: %v", got)
	}
	// gitty adds no insteadOf override of its own: the user's git config is
	// what decides where a URL points.
	if strings.Contains(joinedArgs, ".insteadOf=") {
		t.Errorf("injected clone should not pin the URL: %v", got)
	}
	for _, a := range got {
		if strings.Contains(a, "glpat-sekret") {
			t.Errorf("token leaked into argv: %v", got)
		}
	}
	if strings.Contains(stdout.String(), "glpat-sekret") {
		t.Errorf("token leaked to stdout:\n%s", stdout.String())
	}
	env := rec.envOf("fetch")
	if !strings.Contains(strings.Join(env, "\n"), "GITTY_ASKPASS_TOKEN=glpat-sekret") {
		t.Errorf("expected token in child env handoff, got %v", env)
	}
}
