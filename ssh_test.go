package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.com/gitlab-org/api/client-go"
)

func TestInsteadOfOverride(t *testing.T) {
	got := insteadOfOverride("https://gitlab.com/acme/repo.git")
	want := []string{"-c", "url.https://gitlab.com/acme/repo.git.insteadOf=https://gitlab.com/acme/repo.git"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("insteadOfOverride() = %v, want %v", got, want)
	}
	if insteadOfOverride("") != nil {
		t.Error("insteadOfOverride(\"\") should produce no option")
	}
}

func TestSSHEnv(t *testing.T) {
	tests := []struct {
		name      string
		http      bool
		acceptNew bool
		existing  string
		want      string // "" means no environment entry at all
	}{
		{
			name: "http mode never touches ssh",
			http: true, acceptNew: true,
			want: "",
		},
		{
			name: "ssh mode without the flag leaves policy alone",
			http: false, acceptNew: false,
			want: "",
		},
		{
			name: "ssh mode with the flag sets accept-new",
			http: false, acceptNew: true,
			want: "GIT_SSH_COMMAND=ssh -o StrictHostKeyChecking=accept-new",
		},
		{
			name: "an existing GIT_SSH_COMMAND is preserved and extended",
			http: false, acceptNew: true, existing: "ssh -i /keys/id_ed25519 -p 2222",
			want: "GIT_SSH_COMMAND=ssh -i /keys/id_ed25519 -p 2222 -o StrictHostKeyChecking=accept-new",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GIT_SSH_COMMAND", tc.existing)
			s := &syncer{cfg: &Config{HTTP: tc.http}, acceptNewHostKeys: tc.acceptNew}
			got := s.sshEnv()
			if tc.want == "" {
				if got != nil {
					t.Errorf("sshEnv() = %v, want nil", got)
				}
				return
			}
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("sshEnv() = %v, want [%q]", got, tc.want)
			}
		})
	}
}

func TestNeedsHostKeyWarmup(t *testing.T) {
	tests := []struct {
		name   string
		http   bool
		dryRun bool
		jobs   int
		want   bool
	}{
		{name: "ssh with concurrency needs it", http: false, jobs: 4, want: true},
		{name: "http does not (askpass, no tty)", http: true, jobs: 4, want: false},
		{name: "serial sync cannot contend", http: false, jobs: 1, want: false},
		{name: "dry run runs no git at all", http: false, dryRun: true, jobs: 4, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &syncer{cfg: &Config{HTTP: tc.http}, dryRun: tc.dryRun, jobs: tc.jobs}
			if got := s.needsHostKeyWarmup(); got != tc.want {
				t.Errorf("needsHostKeyWarmup() = %v, want %v", got, tc.want)
			}
		})
	}
}

// concurrencyProbe is a gitRunner that observes how many git invocations
// overlap, so a test can prove the first SSH connection runs on its own.
type concurrencyProbe struct {
	mu          sync.Mutex
	started     int
	inflight    int
	maxInflight int
	// maxDuringFirst is the high-water mark observed at the moment the very
	// first invocation finished. 1 means nothing overlapped it.
	maxDuringFirst int
	envs           [][]string
	delay          time.Duration
}

func (p *concurrencyProbe) run(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
	p.mu.Lock()
	p.started++
	isFirst := p.started == 1
	p.inflight++
	if p.inflight > p.maxInflight {
		p.maxInflight = p.inflight
	}
	p.envs = append(p.envs, extraEnv)
	p.mu.Unlock()

	// Hold the "connection" open long enough that any overlap is observable.
	time.Sleep(p.delay)

	p.mu.Lock()
	if isFirst {
		p.maxDuringFirst = p.maxInflight
	}
	p.inflight--
	p.mu.Unlock()
	return nil, nil
}

func sshProjects(n int) []*gitlab.Project {
	var ps []*gitlab.Project
	for i := 0; i < n; i++ {
		name := string(rune('a' + i))
		ps = append(ps, &gitlab.Project{
			PathWithNamespace: "acme/" + name,
			SSHURLToRepo:      "git@gitlab.com:acme/" + name + ".git",
		})
	}
	return ps
}

// TestSSHFirstCloneRunsAlone is the regression guard for the host-key prompt
// storm: over SSH, several git processes starting at once all reach the same
// "Are you sure you want to continue connecting" prompt and then fight over
// /dev/tty for the answer, so the user is asked many times per repository.
// The first clone must therefore run with nothing else in flight.
func TestSSHFirstCloneRunsAlone(t *testing.T) {
	t.Chdir(t.TempDir())

	probe := &concurrencyProbe{delay: 25 * time.Millisecond}
	s, _, _ := newTestSyncer(
		&Config{URL: "https://gitlab.com", HTTP: false}, // SSH mode
		fakeSource{projects: map[string][]*gitlab.Project{"acme": sshProjects(8)}},
		probe.run,
	)
	s.jobs = 4

	s.syncRepos(context.Background(), "acme")

	probe.mu.Lock()
	defer probe.mu.Unlock()

	if probe.started != 8 {
		t.Fatalf("git invocations = %d, want 8", probe.started)
	}
	if probe.maxDuringFirst != 1 {
		t.Errorf("first SSH clone overlapped %d invocations, want it to run alone "+
			"(concurrent ssh processes contend for the tty host-key prompt)", probe.maxDuringFirst)
	}
	// The remaining repositories must still be synced concurrently — the
	// warmup must not quietly serialize the whole run.
	if probe.maxInflight < 2 {
		t.Errorf("max concurrent invocations = %d, want >= 2 after the warmup", probe.maxInflight)
	}
}

// Over HTTP there is no tty prompt to contend for (credentials come from the
// askpass helper), so no warmup is needed and the pool starts fanned out.
func TestHTTPCloneNeedsNoWarmup(t *testing.T) {
	t.Chdir(t.TempDir())

	var ps []*gitlab.Project
	for i := 0; i < 8; i++ {
		name := string(rune('a' + i))
		ps = append(ps, &gitlab.Project{
			PathWithNamespace: "acme/" + name,
			HTTPURLToRepo:     "https://gitlab.com/acme/" + name + ".git",
		})
	}

	probe := &concurrencyProbe{delay: 25 * time.Millisecond}
	s, _, _ := newTestSyncer(
		&Config{URL: "https://gitlab.com", HTTP: true},
		fakeSource{projects: map[string][]*gitlab.Project{"acme": ps}},
		probe.run,
	)
	s.jobs = 4

	s.syncRepos(context.Background(), "acme")

	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.started != 8 {
		t.Fatalf("git invocations = %d, want 8", probe.started)
	}
	if probe.maxInflight < 2 {
		t.Errorf("http sync should run concurrently, max inflight = %d", probe.maxInflight)
	}
}

// The ssh option must actually reach the git invocation, not just exist.
func TestSSHCloneCarriesAcceptNewOption(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GIT_SSH_COMMAND", "")

	probe := &concurrencyProbe{}
	s, _, _ := newTestSyncer(
		&Config{URL: "https://gitlab.com", HTTP: false},
		fakeSource{projects: map[string][]*gitlab.Project{"acme": sshProjects(1)}},
		probe.run,
	)
	s.jobs = 1
	s.acceptNewHostKeys = true

	s.syncRepos(context.Background(), "acme")

	probe.mu.Lock()
	defer probe.mu.Unlock()
	if len(probe.envs) != 1 {
		t.Fatalf("git invocations = %d, want 1", len(probe.envs))
	}
	joined := strings.Join(probe.envs[0], "\n")
	if !strings.Contains(joined, "GIT_SSH_COMMAND=ssh -o StrictHostKeyChecking=accept-new") {
		t.Errorf("clone env missing the ssh option: %v", probe.envs[0])
	}
}

// Without the flag gitty must not touch the user's ssh configuration at all.
func TestSSHCloneLeavesPolicyAloneByDefault(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GIT_SSH_COMMAND", "")

	probe := &concurrencyProbe{}
	s, _, _ := newTestSyncer(
		&Config{URL: "https://gitlab.com", HTTP: false},
		fakeSource{projects: map[string][]*gitlab.Project{"acme": sshProjects(1)}},
		probe.run,
	)
	s.jobs = 1

	s.syncRepos(context.Background(), "acme")

	probe.mu.Lock()
	defer probe.mu.Unlock()
	for _, env := range probe.envs {
		for _, e := range env {
			if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
				t.Errorf("gitty set %q without --accept-new-host-keys", e)
			}
		}
	}
}
