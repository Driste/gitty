package sync

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"gitty/internal/gitexec"
)

// Job is one repo operation queued to the worker pool.
type Job struct {
	NamespacePath string
	LocalDir      string
	CloneURL      string
	Exists        bool // true ⇒ pull, false ⇒ clone
}

// pool is the bounded worker pool that runs Jobs concurrently.
//
// effectiveJobs is in [1, 64] — the cli package is responsible for input
// validation and clamping (data-model E6). sem is a counting semaphore
// implemented as a buffered channel; one slot per concurrent worker.
type pool struct {
	ctx           context.Context
	sem           chan struct{}
	wg            sync.WaitGroup
	out           *atomicWriter
	runner        gitexec.Runner
	effectiveJobs int
}

// newPool constructs a pool wrapping deps.Runner and deps.Stderr.
func newPool(ctx context.Context, runner gitexec.Runner, stderr io.Writer, jobs int) *pool {
	if jobs < 1 {
		jobs = 1
	}
	return &pool{
		ctx:           ctx,
		sem:           make(chan struct{}, jobs),
		out:           &atomicWriter{w: stderr},
		runner:        runner,
		effectiveJobs: jobs,
	}
}

// Submit blocks until a worker slot is available, then dispatches job to a
// new goroutine. Returns ctx.Err() if the context has been cancelled, so
// the caller can stop the dispatch loop.
func (p *pool) Submit(job Job) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	select {
	case p.sem <- struct{}{}:
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() { <-p.sem }()
		runOne(p, job)
	}()
	return nil
}

// Wait blocks until all in-flight workers return. Returns ctx.Err() if the
// pool was cancelled mid-run, nil otherwise.
func (p *pool) Wait() error {
	p.wg.Wait()
	if err := p.ctx.Err(); err != nil {
		return err
	}
	return nil
}

// runOne executes a single Job. Per FR-005, gitty's per-repo lines are
// prefixed [<namespace/path>] when effectiveJobs > 1. Per FR-006, git's
// captured stderr is forwarded only on non-zero git exit.
func runOne(p *pool, job Job) {
	prefix := ""
	if p.effectiveJobs > 1 {
		prefix = "[" + job.NamespacePath + "] "
	}

	if job.Exists {
		p.out.WriteLine("%s-> %s pulling", prefix, job.NamespacePath)
		stderr, err := p.runner.Run(p.ctx, job.LocalDir, "pull")
		if err != nil {
			p.out.WriteBlock(fmt.Sprintf("%s-> error: git pull failed in %s: %v", prefix, job.LocalDir, err), stderr)
			return
		}
		p.out.WriteLine("%s-> pulled", prefix)
		return
	}

	// Clone path: ensure parent dir exists before the git call.
	parent := filepath.Dir(job.LocalDir)
	if err := os.MkdirAll(parent, 0755); err != nil {
		p.out.WriteLine("%s-> error creating parent %s: %v", prefix, parent, err)
		return
	}
	p.out.WriteLine("%s-> cloning %s", prefix, job.CloneURL)
	stderr, err := p.runner.Run(p.ctx, ".", "clone", job.CloneURL, job.LocalDir)
	if err != nil {
		p.out.WriteBlock(fmt.Sprintf("%s-> error: git clone failed for %s: %v", prefix, job.CloneURL, err), stderr)
		return
	}
	p.out.WriteLine("%s-> cloned", prefix)
}

// atomicWriter serializes writes to a shared io.Writer so per-repo lines
// from concurrent workers never interleave mid-line.
type atomicWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// WriteLine writes one complete line (with trailing newline) under the
// mutex.
func (a *atomicWriter) WriteLine(format string, args ...any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fmt.Fprintf(a.w, format, args...)
	fmt.Fprintln(a.w)
}

// WriteBlock writes a header line followed by body bytes, all under one
// mutex acquisition. Used to keep a gitty error line and the captured git
// stderr contiguous in the output.
func (a *atomicWriter) WriteBlock(header string, body []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fmt.Fprintln(a.w, header)
	if len(body) == 0 {
		return
	}
	a.w.Write(body)
	if body[len(body)-1] != '\n' {
		fmt.Fprintln(a.w)
	}
}
