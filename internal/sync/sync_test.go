package sync

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"gitty/internal/config"
	"gitty/internal/gitlabapi"
)

// --- Fakes ---

type fakeClient struct {
	root        *gitlabapi.Group
	subGroups   []*gitlabapi.Group
	descendants []*gitlabapi.Group
	projects    []*gitlabapi.Project
}

func (f *fakeClient) GetGroup(path string) (*gitlabapi.Group, error) {
	return f.root, nil
}
func (f *fakeClient) ListSubGroups(parent string) ([]*gitlabapi.Group, error) {
	return f.subGroups, nil
}
func (f *fakeClient) ListDescendantGroups(parent string) ([]*gitlabapi.Group, error) {
	return f.descendants, nil
}
func (f *fakeClient) ListGroupProjects(parent string, recursive bool) ([]*gitlabapi.Project, error) {
	return f.projects, nil
}

type fakeRunCall struct {
	dir  string
	args []string
}

type fakeRunner struct {
	mu     sync.Mutex
	calls  []fakeRunCall
	stderr []byte
	err    error
}

func (f *fakeRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeRunCall{dir: dir, args: append([]string{}, args...)})
	return f.stderr, f.err
}

func (f *fakeRunner) snapshot() []fakeRunCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeRunCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// --- Tests ---

func TestResolveToken(t *testing.T) {
	cases := []struct {
		name     string
		explicit string
		env      map[string]string
		want     string
		wantErr  error
	}{
		{"explicit-wins", "tok-exp", map[string]string{"GITLAB_TOKEN": "tok-env", "CI_JOB_TOKEN": "tok-ci"}, "tok-exp", nil},
		{"gitlab-token-fallback", "", map[string]string{"GITLAB_TOKEN": "tok-env", "CI_JOB_TOKEN": "tok-ci"}, "tok-env", nil},
		{"ci-job-token-fallback", "", map[string]string{"CI_JOB_TOKEN": "tok-ci"}, "tok-ci", nil},
		{"all-empty", "", map[string]string{}, "", ErrNoToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(k string) string { return tc.env[k] }
			got, err := ResolveToken(tc.explicit, lookup)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSync_DoGroupsAndRepos(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)

	c := &fakeClient{
		root: &gitlabapi.Group{FullPath: "tenant", Name: "tenant"},
		subGroups: []*gitlabapi.Group{
			{FullPath: "tenant/images", Name: "images"},
		},
		projects: []*gitlabapi.Project{
			{PathWithNamespace: "tenant/api", SSHURLToRepo: "git@host:tenant/api.git"},
			{PathWithNamespace: "tenant/web", SSHURLToRepo: "git@host:tenant/web.git"},
		},
	}
	runner := &fakeRunner{}
	var stdout, stderr bytes.Buffer

	cfg := &config.Config{URL: "https://example.com", HTTP: false, RootPath: "tenant", Jobs: 1}
	deps := Deps{
		Client: c,
		Runner: runner,
		Stdout: &stdout,
		Stderr: &stderr,
		Exists: func(string) bool { return false }, // every project missing → clone
	}
	req := Request{DoGroups: true, DoRepos: true, Jobs: 1}

	if err := Sync(context.Background(), req, cfg, deps); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Group materialization: workspace root and the one subgroup both get a config.
	if _, err := os.Stat(filepath.Join(tmp, ".gitty", "config")); err != nil {
		t.Errorf("workspace .gitty/config missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "images", ".gitty", "config")); err != nil {
		t.Errorf("images/.gitty/config missing: %v", err)
	}

	// Repo materialization: two clone invocations recorded.
	calls := runner.snapshot()
	if len(calls) != 2 {
		t.Fatalf("expected 2 git invocations, got %d: %+v", len(calls), calls)
	}
	// With Jobs=1 the pool is effectively serial and preserves project order.
	for i, want := range []string{"git@host:tenant/api.git", "git@host:tenant/web.git"} {
		call := calls[i]
		if call.dir != "." {
			t.Errorf("call %d dir = %q, want \".\"", i, call.dir)
		}
		if len(call.args) < 2 || call.args[0] != "clone" || call.args[1] != want {
			t.Errorf("call %d args = %v, want clone %s …", i, call.args, want)
		}
	}

	// Stream split: dry-run is OFF, so stdout MUST be empty.
	if got := stdout.Len(); got != 0 {
		t.Errorf("stdout should be empty for non-dry-run sync, got %d bytes:\n%s", got, stdout.String())
	}
	if stderr.Len() == 0 {
		t.Error("stderr should contain progress, got empty")
	}
}

func TestSync_DryRunStreamSplit(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)

	c := &fakeClient{
		projects: []*gitlabapi.Project{
			{PathWithNamespace: "tenant/api", SSHURLToRepo: "git@host:tenant/api.git"},
		},
	}
	runner := &fakeRunner{}
	var stdout, stderr bytes.Buffer

	cfg := &config.Config{URL: "https://example.com", HTTP: false, RootPath: "tenant", Jobs: 4}
	deps := Deps{
		Client: c,
		Runner: runner,
		Stdout: &stdout,
		Stderr: &stderr,
		Exists: func(string) bool { return false },
	}
	req := Request{DryRun: true, DoRepos: true, Jobs: 4}

	if err := Sync(context.Background(), req, cfg, deps); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// FR-010: dry-run plan lines on stdout, no plan lines on stderr,
	// no real git invocations recorded.
	calls := runner.snapshot()
	if len(calls) != 0 {
		t.Errorf("dry-run should record zero git calls, got %+v", calls)
	}
	if !contains(stdout.String(), "DRY RUN") {
		t.Errorf("stdout missing DRY RUN banner:\n%s", stdout.String())
	}
	if !contains(stdout.String(), "Would clone") {
		t.Errorf("stdout missing 'Would clone' line:\n%s", stdout.String())
	}
	if contains(stderr.String(), "Would clone") {
		t.Errorf("stderr should NOT contain plan lines:\n%s", stderr.String())
	}
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
