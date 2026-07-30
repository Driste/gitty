package main

import "testing"

func TestGetLocalRelPath(t *testing.T) {
	tests := []struct {
		name        string
		apiFullPath string
		configRoot  string
		want        string
	}{
		{
			name:        "empty root returns full path",
			apiFullPath: "acme/team/repo",
			configRoot:  "",
			want:        "acme/team/repo",
		},
		{
			name:        "strips root prefix on segment boundary",
			apiFullPath: "acme/team/repo",
			configRoot:  "acme/team",
			want:        "repo",
		},
		{
			name:        "root equal to path yields empty",
			apiFullPath: "acme/team",
			configRoot:  "acme/team",
			want:        "",
		},
		{
			name:        "nested remainder is preserved",
			apiFullPath: "acme/team/sub/repo",
			configRoot:  "acme",
			want:        "team/sub/repo",
		},
		{
			// Regression: a prefix that is not a full path segment must not
			// be stripped. "acme/team" is not a parent of "acme/team-x".
			name:        "does not strip a non-boundary prefix",
			apiFullPath: "acme/team-x/repo",
			configRoot:  "acme/team",
			want:        "acme/team-x/repo",
		},
		{
			name:        "unrelated root leaves path untouched",
			apiFullPath: "other/group/repo",
			configRoot:  "acme",
			want:        "other/group/repo",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := getLocalRelPath(tc.apiFullPath, tc.configRoot)
			if got != tc.want {
				t.Errorf("getLocalRelPath(%q, %q) = %q, want %q", tc.apiFullPath, tc.configRoot, got, tc.want)
			}
		})
	}
}

func TestResolveToken(t *testing.T) {
	tests := []struct {
		name      string
		flagToken string
		gitlabEnv string
		ciEnv     string
		want      string
	}{
		{name: "flag wins over everything", flagToken: "flagtok", gitlabEnv: "envtok", ciEnv: "citok", want: "flagtok"},
		{name: "gitlab env used when no flag", flagToken: "", gitlabEnv: "envtok", ciEnv: "citok", want: "envtok"},
		{name: "ci token used as last resort", flagToken: "", gitlabEnv: "", ciEnv: "citok", want: "citok"},
		{name: "empty when nothing is set", flagToken: "", gitlabEnv: "", ciEnv: "", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITLAB_TOKEN", tc.gitlabEnv)
			t.Setenv("CI_JOB_TOKEN", tc.ciEnv)
			if got := resolveToken(tc.flagToken); got != tc.want {
				t.Errorf("resolveToken(%q) = %q, want %q", tc.flagToken, got, tc.want)
			}
		})
	}
}

func TestIsWithinWorkspace(t *testing.T) {
	tests := []struct {
		name string
		rel  string
		want bool
	}{
		{name: "empty is allowed (workspace root)", rel: "", want: true},
		{name: "simple nested path", rel: "acme/team/repo", want: true},
		{name: "current dir marker", rel: ".", want: true},
		{name: "parent escape", rel: "../evil", want: false},
		{name: "deep parent escape", rel: "acme/../../evil", want: false},
		{name: "bare parent", rel: "..", want: false},
		{name: "absolute path", rel: "/etc/passwd", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWithinWorkspace(tc.rel); got != tc.want {
				t.Errorf("isWithinWorkspace(%q) = %v, want %v", tc.rel, got, tc.want)
			}
		})
	}
}
