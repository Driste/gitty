package sync

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitty/internal/config"
	"gitty/internal/gitlabapi"
)

// timingRunner is a fake Runner that sleeps to simulate work and records
// peak concurrency. Safe for parallel calls.
type timingRunner struct {
	mu       sync.Mutex
	calls    int
	inFlight atomic.Int64
	peak     atomic.Int64
	delay    time.Duration
	stderr   []byte
	err      error
}

func (t *timingRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	n := t.inFlight.Add(1)
	defer t.inFlight.Add(-1)
	for {
		p := t.peak.Load()
		if n <= p {
			break
		}
		if t.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if t.delay > 0 {
		select {
		case <-time.After(t.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	return t.stderr, t.err
}

func (t *timingRunner) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

// T110: with jobs=4 and 12 fast fake-runner jobs, peak in-flight is >1 and ≤4.
func TestPool_FanOut(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)

	projects := make([]*gitlabapi.Project, 12)
	for i := range projects {
		projects[i] = &gitlabapi.Project{
			PathWithNamespace: "tenant/repo" + string(rune('a'+i)),
			SSHURLToRepo:      "git@host:tenant/repo.git",
		}
	}
	c := &fakeClient{projects: projects}
	runner := &timingRunner{delay: 30 * time.Millisecond}
	var stdout, stderr bytes.Buffer

	cfg := &config.Config{URL: "https://example.com", RootPath: "tenant", Jobs: 4}
	deps := Deps{
		Client: c, Runner: runner,
		Stdout: &stdout, Stderr: &stderr,
		Exists: func(string) bool { return false },
	}
	req := Request{DoRepos: true, Jobs: 4}

	if err := Sync(context.Background(), req, cfg, deps); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := runner.callCount(); got != 12 {
		t.Fatalf("expected 12 calls, got %d", got)
	}
	peak := runner.peak.Load()
	if peak < 2 {
		t.Errorf("peak in-flight = %d, want >=2 (parallelism not observed)", peak)
	}
	if peak > 4 {
		t.Errorf("peak in-flight = %d, want <=4 (concurrency exceeded jobs)", peak)
	}
}

// T111: re-running Sync against an existing workspace performs only pulls
// (no clones, no mkdir conflicts) — SC-003 idempotency.
func TestSync_Idempotency(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)

	c := &fakeClient{projects: []*gitlabapi.Project{
		{PathWithNamespace: "tenant/api", SSHURLToRepo: "git@host:tenant/api.git"},
	}}
	cfg := &config.Config{URL: "https://example.com", RootPath: "tenant", Jobs: 2}

	// First run: clone (Exists=false).
	runner1 := &fakeRunner{}
	var stdout1, stderr1 bytes.Buffer
	deps1 := Deps{
		Client: c, Runner: runner1,
		Stdout: &stdout1, Stderr: &stderr1,
		Exists: func(string) bool { return false },
	}
	if err := Sync(context.Background(), Request{DoRepos: true, Jobs: 2}, cfg, deps1); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	first := runner1.snapshot()
	if len(first) != 1 || first[0].args[0] != "clone" {
		t.Fatalf("first run should be one clone, got %+v", first)
	}

	// Second run: same input, but Exists=true for everything (pre-existing).
	runner2 := &fakeRunner{}
	var stdout2, stderr2 bytes.Buffer
	deps2 := Deps{
		Client: c, Runner: runner2,
		Stdout: &stdout2, Stderr: &stderr2,
		Exists: func(string) bool { return true },
	}
	if err := Sync(context.Background(), Request{DoRepos: true, Jobs: 2}, cfg, deps2); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	second := runner2.snapshot()
	if len(second) != 1 || second[0].args[0] != "pull" {
		t.Fatalf("second run should be one pull, got %+v", second)
	}
}

// T200: prefix toggle — jobs=1 ⇒ no [namespace] prefix on per-repo lines;
//
//	jobs=2 ⇒ every per-repo line starts with [namespace].
func TestPool_PrefixToggle(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)

	c := &fakeClient{projects: []*gitlabapi.Project{
		{PathWithNamespace: "tenant/api", SSHURLToRepo: "git@host:tenant/api.git"},
	}}
	cfg := &config.Config{URL: "https://example.com", RootPath: "tenant", Jobs: 1}

	// Jobs=1: no prefix.
	var stderr1 bytes.Buffer
	deps1 := Deps{
		Client: c, Runner: &fakeRunner{},
		Stdout: &bytes.Buffer{}, Stderr: &stderr1,
		Exists: func(string) bool { return false },
	}
	if err := Sync(context.Background(), Request{DoRepos: true, Jobs: 1}, cfg, deps1); err != nil {
		t.Fatalf("Sync jobs=1: %v", err)
	}
	if strings.Contains(stderr1.String(), "[tenant/api]") {
		t.Errorf("jobs=1 should not prefix lines, got:\n%s", stderr1.String())
	}

	// Jobs=2: prefix every per-repo line.
	var stderr2 bytes.Buffer
	deps2 := Deps{
		Client: c, Runner: &fakeRunner{},
		Stdout: &bytes.Buffer{}, Stderr: &stderr2,
		Exists: func(string) bool { return false },
	}
	if err := Sync(context.Background(), Request{DoRepos: true, Jobs: 2}, cfg, deps2); err != nil {
		t.Fatalf("Sync jobs=2: %v", err)
	}
	out2 := stderr2.String()
	// Every line emitted by runOne for this project starts with the prefix.
	if !strings.Contains(out2, "[tenant/api] -> cloning") {
		t.Errorf("jobs=2 missing prefixed cloning line:\n%s", out2)
	}
	if !strings.Contains(out2, "[tenant/api] -> cloned") {
		t.Errorf("jobs=2 missing prefixed cloned line:\n%s", out2)
	}
}

// T201: atomic lines — with many small concurrent writes, each gitty line
// appears whole in the stderr buffer (no mid-line splits).
func TestPool_AtomicLines(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)

	const N = 32
	projects := make([]*gitlabapi.Project, N)
	for i := range projects {
		projects[i] = &gitlabapi.Project{
			PathWithNamespace: "tenant/repo" + string(rune('a'+(i%26))) + string(rune('a'+(i/26))),
			SSHURLToRepo:      "git@host:tenant/x.git",
		}
	}
	c := &fakeClient{projects: projects}
	cfg := &config.Config{URL: "https://example.com", RootPath: "tenant", Jobs: 8}

	var stderr bytes.Buffer
	deps := Deps{
		Client: c, Runner: &fakeRunner{},
		Stdout: &bytes.Buffer{}, Stderr: &stderr,
		Exists: func(string) bool { return false },
	}
	if err := Sync(context.Background(), Request{DoRepos: true, Jobs: 8}, cfg, deps); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// Every line emitted by the worker pool that starts with `[` MUST also
	// contain `] ` later in the same line (no split between the prefix and
	// the message).
	for _, line := range strings.Split(stderr.String(), "\n") {
		if !strings.HasPrefix(line, "[") {
			continue
		}
		if !strings.Contains(line, "] ") {
			t.Errorf("split mid-line: %q", line)
		}
	}
}

// T202: on git failure, the captured git stderr appears in gitty's stderr
// right after the gitty error line.
func TestPool_StderrCaptureOnError(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)

	c := &fakeClient{projects: []*gitlabapi.Project{
		{PathWithNamespace: "tenant/api", SSHURLToRepo: "git@host:tenant/api.git"},
	}}
	cfg := &config.Config{URL: "https://example.com", RootPath: "tenant", Jobs: 2}

	runner := &fakeRunner{
		stderr: []byte("fatal: repository not found\n"),
		err:    errors.New("exit status 128"),
	}
	var stderr bytes.Buffer
	deps := Deps{
		Client: c, Runner: runner,
		Stdout: &bytes.Buffer{}, Stderr: &stderr,
		Exists: func(string) bool { return false },
	}
	if err := Sync(context.Background(), Request{DoRepos: true, Jobs: 2}, cfg, deps); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	out := stderr.String()
	if !strings.Contains(out, "[tenant/api] -> error:") {
		t.Errorf("missing gitty error line:\n%s", out)
	}
	if !strings.Contains(out, "fatal: repository not found") {
		t.Errorf("missing captured git stderr in output:\n%s", out)
	}
	// Order: error line precedes captured stderr.
	errIdx := strings.Index(out, "[tenant/api] -> error:")
	captIdx := strings.Index(out, "fatal: repository not found")
	if errIdx < 0 || captIdx < 0 || captIdx < errIdx {
		t.Errorf("error line should precede captured stderr; got errIdx=%d captIdx=%d", errIdx, captIdx)
	}
}

// T203: on git success, captured git stderr is NOT forwarded.
func TestPool_StderrSuppressOnSuccess(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)

	c := &fakeClient{projects: []*gitlabapi.Project{
		{PathWithNamespace: "tenant/api", SSHURLToRepo: "git@host:tenant/api.git"},
	}}
	cfg := &config.Config{URL: "https://example.com", RootPath: "tenant", Jobs: 2}

	runner := &fakeRunner{
		stderr: []byte("Cloning into 'api'...\n"),
		err:    nil,
	}
	var stderr bytes.Buffer
	deps := Deps{
		Client: c, Runner: runner,
		Stdout: &bytes.Buffer{}, Stderr: &stderr,
		Exists: func(string) bool { return false },
	}
	if err := Sync(context.Background(), Request{DoRepos: true, Jobs: 2}, cfg, deps); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if strings.Contains(stderr.String(), "Cloning into 'api'") {
		t.Errorf("captured stderr leaked on success:\n%s", stderr.String())
	}
}

// T310: cancelling the context mid-run stops dispatching new jobs and
// causes Sync to return context.Canceled. The runner records strictly
// fewer invocations than the total job count.
func TestPool_ContextCancellation(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)

	const N = 20
	projects := make([]*gitlabapi.Project, N)
	for i := range projects {
		projects[i] = &gitlabapi.Project{
			PathWithNamespace: "tenant/r" + string(rune('a'+(i%26))),
			SSHURLToRepo:      "git@host:tenant/r.git",
		}
	}
	c := &fakeClient{projects: projects}
	runner := &timingRunner{delay: 100 * time.Millisecond}
	cfg := &config.Config{URL: "https://example.com", RootPath: "tenant", Jobs: 4}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	var stderr bytes.Buffer
	deps := Deps{
		Client: c, Runner: runner,
		Stdout: &bytes.Buffer{}, Stderr: &stderr,
		Exists: func(string) bool { return false },
	}
	err := Sync(ctx, Request{DoRepos: true, Jobs: 4}, cfg, deps)
	if err == nil {
		t.Fatal("expected non-nil error from cancelled Sync, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled wrapping", err)
	}
	// Cancellation must short-circuit dispatch; fewer than all N projects
	// should have been handed to the runner.
	if got := runner.callCount(); got >= N {
		t.Errorf("cancellation should have stopped dispatch; got %d/%d invocations", got, N)
	}
}

// T204: dry-run bypasses the worker pool entirely; stdout has plan lines
// in stable repo-order; runner is never invoked.
func TestPool_DryRunBypassed(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)

	c := &fakeClient{projects: []*gitlabapi.Project{
		{PathWithNamespace: "tenant/api", SSHURLToRepo: "git@host:tenant/api.git"},
		{PathWithNamespace: "tenant/web", SSHURLToRepo: "git@host:tenant/web.git"},
		{PathWithNamespace: "tenant/db", SSHURLToRepo: "git@host:tenant/db.git"},
	}}
	cfg := &config.Config{URL: "https://example.com", RootPath: "tenant", Jobs: 4}

	runner := &fakeRunner{}
	var stdout, stderr bytes.Buffer
	deps := Deps{
		Client: c, Runner: runner,
		Stdout: &stdout, Stderr: &stderr,
		Exists: func(string) bool { return false },
	}
	if err := Sync(context.Background(), Request{DoRepos: true, DryRun: true, Jobs: 4}, cfg, deps); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := len(runner.snapshot()); got != 0 {
		t.Errorf("dry-run should produce zero git calls, got %d", got)
	}
	// Plan lines on stdout, in stable repo-order from the fake client.
	expected := []string{"tenant/api", "tenant/web", "tenant/db"}
	last := -1
	for _, want := range expected {
		idx := strings.Index(stdout.String(), want)
		if idx < 0 {
			t.Fatalf("missing %q in stdout:\n%s", want, stdout.String())
		}
		if idx < last {
			t.Errorf("plan lines out of order; %q at %d but earlier line was at %d", want, idx, last)
		}
		last = idx
	}
}

