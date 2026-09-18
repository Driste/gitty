package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestScopesAllowAPI(t *testing.T) {
	tests := []struct {
		name   string
		scopes []string
		want   bool
	}{
		{name: "api", scopes: []string{"api"}, want: true},
		{name: "read_api", scopes: []string{"read_api"}, want: true},
		{name: "read_repository alone cannot list", scopes: []string{"read_repository"}, want: false},
		{name: "unrelated scopes", scopes: []string{"read_user", "profile"}, want: false},
		{name: "none", scopes: nil, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := scopesAllowAPI(tc.scopes); got != tc.want {
				t.Errorf("scopesAllowAPI(%v) = %v, want %v", tc.scopes, got, tc.want)
			}
		})
	}
}

func TestScopesAllowHTTPClone(t *testing.T) {
	tests := []struct {
		name   string
		scopes []string
		want   bool
	}{
		// GitLab's `api` scope grants git-over-HTTPS as well as API access.
		{name: "api covers git too", scopes: []string{"api"}, want: true},
		{name: "read_repository", scopes: []string{"read_repository"}, want: true},
		{name: "write_repository", scopes: []string{"write_repository"}, want: true},
		// The trap: lists groups perfectly, then fails every clone.
		{name: "read_api alone cannot clone", scopes: []string{"read_api"}, want: false},
		{name: "none", scopes: nil, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := scopesAllowHTTPClone(tc.scopes); got != tc.want {
				t.Errorf("scopesAllowHTTPClone(%v) = %v, want %v", tc.scopes, got, tc.want)
			}
		})
	}
}

func TestReportAuth(t *testing.T) {
	httpCfg := &Config{URL: "https://gitlab.example.com", HTTP: true}
	sshCfg := &Config{URL: "https://gitlab.example.com", HTTP: false}

	tests := []struct {
		name     string
		cfg      *Config
		cred     credential
		chk      tokenCheck
		want     []string
		unwanted []string
	}{
		{
			name: "no token explains how to set one",
			cfg:  httpCfg,
			want: []string{
				"No GitLab token found",
				"export GITLAB_TOKEN=",
				"read_api and read_repository",
				"--anon",
			},
		},
		{
			name:     "ssh workspace does not demand a repo scope",
			cfg:      sshCfg,
			want:     []string{"the read_api scope"},
			unwanted: []string{"read_repository"},
		},
		{
			name: "rejected token says so plainly",
			cfg:  httpCfg,
			cred: credential{token: "t", source: "GITLAB_TOKEN"},
			chk:  tokenCheck{reachable: true, unauthorized: true},
			want: []string{"REJECTED", "expired, revoked", "export GITLAB_TOKEN="},
		},
		{
			name: "unreachable instance is not treated as a bad token",
			cfg:  httpCfg,
			cred: credential{token: "t", source: "GITLAB_TOKEN"},
			chk:  tokenCheck{err: errDial},
			want: []string{"Could not verify", "workspace was still created"},
			// Must not accuse the token of being invalid.
			unwanted: []string{"REJECTED"},
		},
		{
			name: "api-only token is warned about for an http workspace",
			cfg:  httpCfg,
			cred: credential{token: "t", source: "GITLAB_TOKEN"},
			chk: tokenCheck{
				reachable: true, user: "alice",
				scopesKnown: true, scopes: []string{"read_api"},
			},
			want: []string{"@alice", "read_api", "WARNING", "read_repository", "--force --ssh"},
		},
		{
			name: "full api scope passes both checks",
			cfg:  httpCfg,
			cred: credential{token: "t", source: "flag"},
			chk: tokenCheck{
				reachable: true, user: "bob",
				scopesKnown: true, scopes: []string{"api"},
			},
			want:     []string{"@bob", "Scopes: api"},
			unwanted: []string{"WARNING"},
		},
		{
			name: "read_api is enough for an ssh workspace",
			cfg:  sshCfg,
			cred: credential{token: "t", source: "GITLAB_TOKEN"},
			chk: tokenCheck{
				reachable: true, user: "carol",
				scopesKnown: true, scopes: []string{"read_api"},
			},
			want:     []string{"@carol"},
			unwanted: []string{"WARNING"},
		},
		{
			name: "revoked token is flagged",
			cfg:  httpCfg,
			cred: credential{token: "t", source: "GITLAB_TOKEN"},
			chk: tokenCheck{
				reachable: true, user: "dave", scopesKnown: true,
				scopes: []string{"api"}, revoked: true, expiresAt: "2026-01-01",
			},
			want: []string{"revoked", "2026-01-01"},
		},
		{
			name: "ci job tokens are not introspected",
			cfg:  httpCfg,
			cred: credential{token: "t", source: "CI_JOB_TOKEN"},
			chk:  tokenCheck{skipped: "CI job tokens cannot be introspected; gitty will use it as-is"},
			want: []string{"CI_JOB_TOKEN", "cannot be introspected"},
		},
		{
			name: "unknown scopes are reported as unknown, not as a failure",
			cfg:  httpCfg,
			cred: credential{token: "t", source: "GITLAB_TOKEN"},
			chk:  tokenCheck{reachable: true, user: "erin"},
			want: []string{"@erin", "did not report the token's scopes", "read_repository"},
			// "unknown" must not be dressed up as a definite problem.
			unwanted: []string{"WARNING"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			reportAuth(&buf, tc.cfg, tc.cred, tc.chk)
			out := buf.String()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("report missing %q:\n%s", want, out)
				}
			}
			for _, unwanted := range tc.unwanted {
				if strings.Contains(out, unwanted) {
					t.Errorf("report should not contain %q:\n%s", unwanted, out)
				}
			}
		})
	}
}

// errDial stands in for a transport-level failure reaching the instance.
var errDial = &dialError{}

type dialError struct{}

func (e *dialError) Error() string { return "dial tcp: connection refused" }
