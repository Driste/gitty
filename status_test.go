package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestParseStatusPorcelainV2(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want repoStatus
	}{
		{
			name: "clean tracking branch in sync",
			out: "# branch.oid abc123\n" +
				"# branch.head main\n" +
				"# branch.upstream origin/main\n" +
				"# branch.ab +0 -0\n",
			want: repoStatus{Branch: "main", Upstream: true, Ahead: 0, Behind: 0, Dirty: false},
		},
		{
			name: "ahead and behind",
			out: "# branch.head feature\n" +
				"# branch.upstream origin/feature\n" +
				"# branch.ab +2 -5\n",
			want: repoStatus{Branch: "feature", Upstream: true, Ahead: 2, Behind: 5},
		},
		{
			name: "dirty working tree",
			out: "# branch.head main\n" +
				"# branch.upstream origin/main\n" +
				"# branch.ab +0 -0\n" +
				"1 .M N... 100644 100644 100644 abc abc README.md\n",
			want: repoStatus{Branch: "main", Upstream: true, Dirty: true},
		},
		{
			name: "untracked file counts as dirty",
			out: "# branch.head main\n" +
				"# branch.upstream origin/main\n" +
				"# branch.ab +0 -0\n" +
				"? newfile.txt\n",
			want: repoStatus{Branch: "main", Upstream: true, Dirty: true},
		},
		{
			name: "no upstream configured",
			out: "# branch.oid abc123\n" +
				"# branch.head local-only\n",
			want: repoStatus{Branch: "local-only", Upstream: false},
		},
		{
			name: "detached head",
			out: "# branch.oid abc123\n" +
				"# branch.head (detached)\n",
			want: repoStatus{Branch: "(detached)", Upstream: false},
		},
		{
			name: "empty output falls back to detached",
			out:  "",
			want: repoStatus{Branch: "(detached)"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseStatusPorcelainV2([]byte(tc.out))
			if got != tc.want {
				t.Errorf("parseStatusPorcelainV2() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestStatusDetail(t *testing.T) {
	tests := []struct {
		name string
		st   repoStatus
		want string
	}{
		{
			name: "tracking branch",
			st:   repoStatus{Branch: "main", Upstream: true, Ahead: 1, Behind: 2, Dirty: true},
			want: "branch=main ahead=1 behind=2 dirty=true",
		},
		{
			name: "no upstream is called out explicitly",
			st:   repoStatus{Branch: "local", Upstream: false},
			want: "branch=local ahead=0 behind=0 dirty=false upstream=none",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusDetail(tc.st); got != tc.want {
				t.Errorf("statusDetail() = %q, want %q", got, tc.want)
			}
		})
	}
}

// mkRepo creates a minimal but valid-looking checkout for findRepos.
func mkRepo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestFindRepos(t *testing.T) {
	root := t.TempDir()

	mkRepo(t, filepath.Join(root, "acme", "one"))
	mkRepo(t, filepath.Join(root, "acme", "team", "two"))
	// A submodule-like nested repo must not be reported separately.
	mkRepo(t, filepath.Join(root, "acme", "one", "vendor", "sub"))
	// An archived broken checkout is skipped.
	mkRepo(t, filepath.Join(root, "acme", "three.gitty-broken-1"))
	// A plain directory with no .git is not a repo.
	if err := os.MkdirAll(filepath.Join(root, "acme", "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	// gitty's own config dir is skipped.
	if err := os.MkdirAll(filepath.Join(root, ConfigDir), 0755); err != nil {
		t.Fatal(err)
	}

	repos, err := findRepos(root)
	if err != nil {
		t.Fatalf("findRepos: %v", err)
	}
	sort.Strings(repos)

	want := []string{"acme/one", "acme/team/two"}
	if strings.Join(repos, ",") != strings.Join(want, ",") {
		t.Errorf("findRepos() = %v, want %v", repos, want)
	}
}

func TestRunStatusRequiresWorkspace(t *testing.T) {
	t.Chdir(t.TempDir())
	err := runStatus(t.Context(), statusOptions{Jobs: 1})
	if err == nil || exitCode(err) != 2 {
		t.Errorf("status without a workspace should be a usage error (exit 2), got %v", err)
	}
}

func TestRunStatusFetchRequiresToken(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runInit("https://gitlab.com", true, false); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("CI_JOB_TOKEN", "")

	err := runStatus(t.Context(), statusOptions{Jobs: 1, Fetch: true})
	if err == nil || exitCode(err) != 2 {
		t.Errorf("--fetch without a token should be a usage error (exit 2), got %v", err)
	}
	// Without --fetch, no token is needed: a clean empty workspace succeeds.
	if err := runStatus(t.Context(), statusOptions{Jobs: 1}); err != nil {
		t.Errorf("status without --fetch should not need a token: %v", err)
	}
}

func TestRunStatusJobsValidation(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runInit("https://gitlab.com", true, false); err != nil {
		t.Fatal(err)
	}
	for _, jobs := range []int{0, -1, 17} {
		err := runStatus(t.Context(), statusOptions{Jobs: jobs})
		if err == nil || exitCode(err) != 2 {
			t.Errorf("jobs=%d: expected usage error (exit 2), got %v", jobs, err)
		}
	}
}
