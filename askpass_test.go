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
	if rec.callCount() != 1 {
		t.Fatalf("git calls: %v", rec.calls)
	}
	got := rec.calls[0] // dir + args
	want := []string{".", "-c", "credential.helper=", "clone", "https://gitlab.com/acme/repo.git", "acme/repo"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("injected clone argv = %v, want %v", got, want)
	}
	for _, a := range got {
		if strings.Contains(a, "glpat-sekret") {
			t.Errorf("token leaked into argv: %v", got)
		}
	}
	if strings.Contains(stdout.String(), "glpat-sekret") {
		t.Errorf("token leaked to stdout:\n%s", stdout.String())
	}
	env := rec.lastEnv()
	if !strings.Contains(strings.Join(env, "\n"), "GITTY_ASKPASS_TOKEN=glpat-sekret") {
		t.Errorf("expected token in child env handoff, got %v", env)
	}
}
