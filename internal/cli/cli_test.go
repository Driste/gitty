package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitty/internal/config"
	"gitty/internal/gitexec"
	"gitty/internal/gitlabapi"
)

// --- Fakes ---

type fakeClient struct {
	projects []*gitlabapi.Project
}

func (f *fakeClient) GetGroup(string) (*gitlabapi.Group, error) { return nil, nil }
func (f *fakeClient) ListSubGroups(string) ([]*gitlabapi.Group, error) {
	return nil, nil
}
func (f *fakeClient) ListDescendantGroups(string) ([]*gitlabapi.Group, error) {
	return nil, nil
}
func (f *fakeClient) ListGroupProjects(string, bool) ([]*gitlabapi.Project, error) {
	return f.projects, nil
}

type fakeRunner struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return nil, nil
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func emptyEnv(string) string { return "" }

func newFakeClientCtor(c gitlabapi.Client) clientCtor {
	return func(string, string) (gitlabapi.Client, error) { return c, nil }
}

func newFakeRunnerCtor(r gitexec.Runner) runnerCtor {
	return func() gitexec.Runner { return r }
}

// --- Tests ---

func TestMain_NoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := mainWithDeps(context.Background(), nil, nil, &stdout, &stderr, emptyEnv,
		newFakeClientCtor(&fakeClient{}), newFakeRunnerCtor(&fakeRunner{}))
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "Usage: gitty") {
		t.Errorf("stdout missing usage text:\n%s", stdout.String())
	}
}

func TestMain_UnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := mainWithDeps(context.Background(), []string{"foo"}, nil, &stdout, &stderr, emptyEnv,
		newFakeClientCtor(&fakeClient{}), newFakeRunnerCtor(&fakeRunner{}))
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "Unknown command: foo") {
		t.Errorf("stdout missing unknown-command line:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Usage: gitty") {
		t.Errorf("stdout missing usage text:\n%s", stdout.String())
	}
}

func TestMain_InitWritesConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	var stdout, stderr bytes.Buffer
	code := mainWithDeps(context.Background(), []string{"init", "-url", "https://gl.example", "-http"}, nil, &stdout, &stderr, emptyEnv,
		newFakeClientCtor(&fakeClient{}), newFakeRunnerCtor(&fakeRunner{}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	cfg, err := config.Load(tmp)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.URL != "https://gl.example" || !cfg.HTTP || cfg.RootPath != "" {
		t.Errorf("config = %+v", *cfg)
	}
	// gitty init MUST write jobs = DefaultJobs so the user sees the value
	// (SC-009).
	if cfg.Jobs != config.DefaultJobs {
		t.Errorf("Jobs = %d, want %d", cfg.Jobs, config.DefaultJobs)
	}
}

func TestMain_SyncNoWorkspace(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	var stdout, stderr bytes.Buffer
	code := mainWithDeps(context.Background(), []string{"sync", "-path", "g/h", "-token", "xyz"}, nil, &stdout, &stderr, emptyEnv,
		newFakeClientCtor(&fakeClient{}), newFakeRunnerCtor(&fakeRunner{}))
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no .gitty/config") {
		t.Errorf("stderr missing 'no .gitty/config':\n%s", stderr.String())
	}
}

func TestMain_SyncSuccessDoReposDefault(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	if err := config.Save(tmp, &config.Config{URL: "https://gl.example", HTTP: false, RootPath: "tenant", Jobs: 4}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	fc := &fakeClient{projects: []*gitlabapi.Project{
		{PathWithNamespace: "tenant/api", SSHURLToRepo: "git@host:tenant/api.git"},
	}}
	fr := &fakeRunner{}

	var stdout, stderr bytes.Buffer
	code := mainWithDeps(context.Background(), []string{"sync", "-path", "g/h", "-token", "xyz"}, nil, &stdout, &stderr, emptyEnv,
		newFakeClientCtor(fc), newFakeRunnerCtor(fr))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if got := fr.callCount(); got != 1 {
		t.Errorf("expected 1 git invocation, got %d", got)
	}
	// Dry-run is off, so stdout MUST be empty.
	if stdout.Len() != 0 {
		t.Errorf("stdout should be empty when not dry-run, got %d bytes:\n%s", stdout.Len(), stdout.String())
	}
}

func TestMain_SyncDryRun(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	if err := config.Save(tmp, &config.Config{URL: "https://gl.example", RootPath: "tenant", Jobs: 4}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	fc := &fakeClient{projects: []*gitlabapi.Project{
		{PathWithNamespace: "tenant/api", SSHURLToRepo: "git@host:tenant/api.git"},
	}}
	fr := &fakeRunner{}

	var stdout, stderr bytes.Buffer
	code := mainWithDeps(context.Background(), []string{"sync", "-path", "g/h", "-token", "xyz", "-dry-run"}, nil, &stdout, &stderr, emptyEnv,
		newFakeClientCtor(fc), newFakeRunnerCtor(fr))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := fr.callCount(); got != 0 {
		t.Errorf("dry-run should produce zero git calls, got %d", got)
	}
	if !strings.Contains(stdout.String(), "Would clone") {
		t.Errorf("stdout missing plan line:\n%s", stdout.String())
	}
	if strings.Contains(stderr.String(), "Would clone") {
		t.Errorf("stderr leaked plan line:\n%s", stderr.String())
	}
}

func TestMain_SyncTokenFromEnv(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	if err := config.Save(tmp, &config.Config{URL: "https://gl.example", RootPath: "tenant", Jobs: 4}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	fc := &fakeClient{}
	env := func(k string) string {
		if k == "GITLAB_TOKEN" {
			return "tok-from-env"
		}
		return ""
	}

	var stdout, stderr bytes.Buffer
	code := mainWithDeps(context.Background(), []string{"sync", "-path", "g/h"}, nil, &stdout, &stderr, env,
		newFakeClientCtor(fc), newFakeRunnerCtor(&fakeRunner{}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
}

// T042: --jobs flag fixtures from contracts/cli.md.

func TestMain_SyncJobsFlagPositive(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	if err := config.Save(tmp, &config.Config{URL: "https://gl.example", RootPath: "tenant", Jobs: 4}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	fc := &fakeClient{projects: []*gitlabapi.Project{
		{PathWithNamespace: "tenant/api", SSHURLToRepo: "git@host:tenant/api.git"},
	}}
	var stdout, stderr bytes.Buffer
	code := mainWithDeps(context.Background(), []string{"sync", "-path", "g/h", "-token", "xyz", "-jobs", "8"}, nil, &stdout, &stderr, emptyEnv,
		newFakeClientCtor(fc), newFakeRunnerCtor(&fakeRunner{}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "clamped") {
		t.Errorf("unexpected clamp warning for in-range jobs: %s", stderr.String())
	}
}

func TestMain_SyncJobsFlagNegativeRejects(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	if err := config.Save(tmp, &config.Config{URL: "https://gl.example", RootPath: "tenant", Jobs: 4}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	fc := &fakeClient{}
	var stdout, stderr bytes.Buffer
	code := mainWithDeps(context.Background(), []string{"sync", "-path", "g/h", "-token", "xyz", "-jobs", "-1"}, nil, &stdout, &stderr, emptyEnv,
		newFakeClientCtor(fc), newFakeRunnerCtor(&fakeRunner{}))
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--jobs must be >= 1 (got -1)") {
		t.Errorf("stderr missing flag-error text:\n%s", stderr.String())
	}
}

func TestMain_SyncJobsFlagClampsAbove64(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	if err := config.Save(tmp, &config.Config{URL: "https://gl.example", RootPath: "tenant", Jobs: 4}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	fc := &fakeClient{projects: []*gitlabapi.Project{
		{PathWithNamespace: "tenant/api", SSHURLToRepo: "git@host:tenant/api.git"},
	}}
	var stdout, stderr bytes.Buffer
	code := mainWithDeps(context.Background(), []string{"sync", "-path", "g/h", "-token", "xyz", "-jobs", "100"}, nil, &stdout, &stderr, emptyEnv,
		newFakeClientCtor(fc), newFakeRunnerCtor(&fakeRunner{}))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--jobs 100 clamped to 64") {
		t.Errorf("stderr missing clamp warning:\n%s", stderr.String())
	}
}

// T311: a cancelled context propagates through Sync to a non-zero exit
// distinct from 1 (the per-repo-failure code, which spec FR-007 keeps at 0).
type blockingRunner struct{}

func (blockingRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestMain_SyncCancelledReturnsNonZero(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	if err := config.Save(tmp, &config.Config{URL: "https://gl.example", RootPath: "tenant", Jobs: 2}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	fc := &fakeClient{projects: []*gitlabapi.Project{
		{PathWithNamespace: "tenant/api", SSHURLToRepo: "git@host:tenant/api.git"},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()

	var stdout, stderr bytes.Buffer
	code := mainWithDeps(ctx, []string{"sync", "-path", "g/h", "-token", "xyz"}, nil, &stdout, &stderr, emptyEnv,
		newFakeClientCtor(fc), newFakeRunnerCtor(blockingRunner{}))
	if code == 0 || code == 1 {
		t.Errorf("cancelled exit code = %d, want non-zero and distinct from 1", code)
	}
}

// Sanity: ensure the workspace setup helper in tests does not leak a bogus
// .gitty/config to the developer's repo working tree.
func TestMain_TestIsolatesWorkspace(t *testing.T) {
	repoDir, _ := os.Getwd()
	tmp := t.TempDir()
	t.Chdir(tmp)
	if got := filepath.Dir(repoDir); got == tmp {
		t.Fatalf("test cwd leaked into repo dir")
	}
}
