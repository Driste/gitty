package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"

	"gitlab.com/gitlab-org/api/client-go"

	"gitty/internal/contract"
)

// Fetching: bringing named projects into the workspace, each at the place
// its namespace path gives it — where a sync of its group would have put it.
//
// It is the one thing the context commands do that changes anything, and it
// is deliberately narrow: a project that is named and absent is cloned;
// nothing else is touched. A project that is already checked out is left
// exactly as it is — not pulled, because the caller may be an agent in the
// middle of work that a moving dependency would disturb, and bringing a
// workspace up to date is what `gitty sync` is for. The cloning itself is
// sync's own code path, so credentials, the user's git configuration, the
// handling of interrupts and of half-made repositories are all inherited.

// maxFetchArgs bounds how many projects one fetch may be asked for. It is
// far above any real request and well below a mistake that names a whole
// namespace.
const maxFetchArgs = 200

// cleanProjectArg turns a project named on the command line into a namespace
// path: a leading or trailing slash and a ".git" suffix are tolerated.
func cleanProjectArg(arg string) (string, error) {
	p := strings.Trim(strings.TrimSpace(arg), "/")
	p = strings.TrimSuffix(p, ".git")
	if p == "" || path.Clean(p) != p || strings.HasPrefix(p, "../") || p == ".." ||
		!strings.Contains(p, "/") || strings.ContainsAny(p, " \t\\") {
		return "", usageErrf("%q is not a project path: name a project by its full namespace path, like group/subgroup/project", arg)
	}
	return p, nil
}

// fetchProjects makes sure each named project is checked out, cloning the
// ones that are not, and reports what became of each, in the order given.
// Events go to the syncer's output as a sync's would.
//
// With update, a project that is already checked out is brought up to date
// as a sync would (a fast-forward pull) instead of being left alone. That is
// `gitty sync --related`; everything else here only ever adds.
func (w *workspace) fetchProjects(ctx context.Context, projects []string, update bool) []contract.FetchResult {
	s := w.s
	results := make([]contract.FetchResult, len(projects))
	type pending struct {
		i       int
		dir     string
		p       *gitlab.Project
		present bool // checked out before this call
	}
	var todo []*pending
	seen := map[string]bool{}

	for i, project := range projects {
		res := &results[i]
		res.Project = project
		if seen[strings.ToLower(project)] {
			res.Error = "named more than once"
			continue
		}
		seen[strings.ToLower(project)] = true

		if s.cfg.Excluded(project) {
			res.Error = "excluded by the workspace's exclude list"
			s.event("skip", project, "excluded")
			continue
		}
		dir, ok := w.dirOf(project)
		if !ok {
			res.Error = fmt.Sprintf("outside this workspace, which holds %s", describeRoot(s.cfg.RootPath))
			s.event("error", project, "outside this workspace")
			continue
		}
		if classifyDest(dir) == destRepo && !isUnborn(dir) {
			res.Present, res.Path = true, w.absDir(dir)
			if !update {
				s.event("skip", project, "present")
				continue
			}
		}
		todo = append(todo, &pending{i: i, dir: dir, present: res.Present})
	}
	if len(todo) == 0 {
		return results
	}

	// Everything left needs the instance: to learn where the project is
	// cloned from, and that it exists at all.
	if err := w.requireAPI(); err != nil {
		for _, t := range todo {
			results[t.i].Error = err.Error()
			s.event("error", results[t.i].Project, "no credential")
		}
		return results
	}
	var mu sync.Mutex
	forEachConcurrent(ctx, listConcurrency, todo, func(t *pending) {
		p, err := s.src.Project(ctx, results[t.i].Project)
		mu.Lock()
		defer mu.Unlock()
		if err != nil || p == nil {
			results[t.i].Error = fmt.Sprintf("looking the project up failed: %v", err)
			return
		}
		t.p = p
	})
	var ready []*pending
	for _, t := range todo {
		res := &results[t.i]
		switch {
		case t.p == nil:
			if res.Error == "" {
				res.Error = "interrupted"
				continue
			}
			s.event("error", res.Project, "looking the project up failed")
			s.diagf("%s: %s", res.Project, res.Error)
		case !strings.EqualFold(t.p.PathWithNamespace, res.Project):
			// GitLab answers for a project's old path after it moves. Cloning
			// it here would put it where no sync would ever look for it.
			res.Error = fmt.Sprintf("the project now lives at %s", t.p.PathWithNamespace)
			s.event("error", res.Project, "moved to "+t.p.PathWithNamespace)
		default:
			ready = append(ready, t)
		}
	}

	bring := func(t *pending) {
		before := len(s.failuresFor(results[t.i].Project))
		s.syncOneRepo(ctx, t.p)
		res := &results[t.i]
		if classifyDest(t.dir) == destRepo {
			res.Present, res.Path = true, w.absDir(t.dir)
			res.Fetched = !s.dryRun && !t.present
		}
		if failures := s.failuresFor(res.Project); len(failures) > before {
			res.Error = failures[len(failures)-1]
			res.Fetched = false
		} else if !res.Present && !s.dryRun && ctx.Err() != nil {
			res.Error = "interrupted"
		}
	}
	s.progressTotal(len(ready))
	// One repository first when ssh may need to ask about a host key, as a
	// sync does: see needsHostKeyWarmup.
	if len(ready) > 1 && s.needsHostKeyWarmup(ctx, ".", s.cloneURL(ready[0].p)) {
		bring(ready[0])
		ready = ready[1:]
	}
	forEachConcurrent(ctx, s.jobs, ready, bring)
	s.progressStop()
	return results
}

// failuresFor returns the details of the error events emitted for a path.
func (s *syncer) failuresFor(project string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, f := range s.failures {
		if detail, ok := strings.CutPrefix(f, project+" "); ok {
			out = append(out, detail)
		} else if f == project {
			out = append(out, "failed")
		}
	}
	return out
}

// fetchOptions bundles the fetch command's flags.
type fetchOptions struct {
	Projects          []string
	Token             string
	Anon              bool
	DryRun            bool
	Verbose           bool
	Jobs              int
	Format            string
	AcceptNewHostKeys bool
	Exclude           []string
}

// runFetch makes sure the named projects are checked out.
//
// Events, one per line on stdout:
//
//	clone <project>             it was absent and has been cloned
//	skip <project> present      it was already checked out (left as it is)
//	skip <project> excluded     the workspace leaves it out
//	error <project> <reason>
//	summary cloned=N present=N errors=N
//
// With --format=json stdout carries one JSON document instead (see
// contract.FetchReport) and the events go to stderr.
func runFetch(ctx context.Context, opts fetchOptions) error {
	if opts.Jobs < 1 || opts.Jobs > maxJobs {
		return usageErrf("--jobs must be between 1 and %d, got %d", maxJobs, opts.Jobs)
	}
	asJSON, err := parseDataFormat(opts.Format, "text")
	if err != nil {
		return err
	}
	if len(opts.Projects) == 0 {
		return usageErrf("fetch needs at least one project: gitty fetch <group/project>...")
	}
	if len(opts.Projects) > maxFetchArgs {
		return usageErrf("fetch takes at most %d projects at a time, got %d", maxFetchArgs, len(opts.Projects))
	}
	projects := make([]string, 0, len(opts.Projects))
	for _, arg := range opts.Projects {
		p, err := cleanProjectArg(arg)
		if err != nil {
			return err
		}
		projects = append(projects, p)
	}

	w, err := openWorkspace(opts.Token, opts.Anon)
	if err != nil {
		return err
	}
	s := w.s
	s.dryRun = opts.DryRun
	s.verbose = opts.Verbose
	s.jobs = opts.Jobs
	s.acceptNewHostKeys = opts.AcceptNewHostKeys
	s.cfg.AddExcludes(opts.Exclude)
	if asJSON {
		s.out = os.Stderr // stdout is the document
	}
	s.progress.enable(s.errOut)
	if opts.DryRun {
		s.diagf("=== DRY RUN MODE ENABLED: No changes will be made ===")
	}

	// Cloning needs the instance. Without a credential that is a mistake in
	// the invocation, said once and before anything is attempted — unless
	// everything named is already here, which needs nothing.
	if err := w.requireAPI(); err != nil {
		for _, p := range projects {
			dir, ok := w.dirOf(p)
			if ok && !s.cfg.Excluded(p) && (classifyDest(dir) != destRepo || isUnborn(dir)) {
				return err
			}
		}
	}

	results := w.fetchProjects(ctx, projects, false)

	cloned, present, failed := 0, 0, 0
	for _, r := range results {
		switch {
		case r.Error != "":
			failed++
		case r.Fetched || (opts.DryRun && !r.Present):
			cloned++
		default:
			present++
		}
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(contract.FetchReport{Schema: contract.Schema, Results: results}); err != nil {
			return fmt.Errorf("rendering the fetch report: %w", err)
		}
	} else {
		fmt.Fprintf(s.out, "summary cloned=%d present=%d errors=%d\n", cloned, present, failed)
	}
	s.recapFailures()

	if ctx.Err() != nil {
		return errInterrupted
	}
	if failed > 0 {
		return &syncFailedError{failures: failed}
	}
	return nil
}

// parseDataFormat validates a --format value for the commands whose other
// format is JSON, and reports whether JSON was chosen.
func parseDataFormat(v, other string) (asJSON bool, err error) {
	switch v {
	case "", other:
		return false, nil
	case "json":
		return true, nil
	}
	return false, usageErrf("invalid --format %q: choose %s or json", v, other)
}
