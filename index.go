package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gitlab.com/gitlab-org/api/client-go"

	"gitty/internal/contract"
)

// The workspace index: which projects exist in the workspace's part of the
// instance, which of them are checked out, and every evidenced reference
// between them. It is a cache — everything in it can be recomputed from the
// API and the checkouts — kept in one file beside the workspace config so
// that answering "what does this project relate to" needs neither the
// network nor a walk through every repository.

// indexFileName is the index's file name inside the workspace's .gitty
// directory.
const indexFileName = "index.json"

// apiEvidence prefixes the evidence of a link that comes from the API rather
// than from a file: such links are replaced when projects are listed, not
// when a checkout is rescanned.
const apiEvidence = "api:"

// indexPath is where a workspace's index lives.
func indexPath(root string) string {
	return filepath.Join(root, ConfigDir, indexFileName)
}

// newIndex returns an empty index for a workspace.
func newIndex(cfg *Config) *contract.Index {
	return &contract.Index{
		Version:  contract.IndexVersion,
		Scanner:  contract.ScannerVersion,
		Instance: cfg.URL,
		Root:     cfg.RootPath,
		Projects: map[string]*contract.Project{},
		Links:    []contract.Link{},
	}
}

// loadIndex reads a workspace's index. A nil index with a reason means there
// is nothing usable and it must be built: no file yet, a file from another
// format version, or one written for a different instance or root (a
// workspace that was re-initialised). Only a file that exists and cannot be
// read at all is an error.
func loadIndex(root string, cfg *Config) (idx *contract.Index, why string, err error) {
	data, err := os.ReadFile(indexPath(root))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "no index yet", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("reading the index: %w", err)
	}
	var loaded contract.Index
	if err := json.Unmarshal(data, &loaded); err != nil {
		return nil, fmt.Sprintf("the index is not readable (%v)", err), nil
	}
	switch {
	case loaded.Version != contract.IndexVersion:
		return nil, fmt.Sprintf("the index has format version %d, this gitty writes %d", loaded.Version, contract.IndexVersion), nil
	case !sameInstance(loaded.Instance, cfg.URL):
		return nil, fmt.Sprintf("the index is for %s", loaded.Instance), nil
	case loaded.Root != cfg.RootPath:
		return nil, fmt.Sprintf("the index is rooted at %q", loaded.Root), nil
	}
	if loaded.Projects == nil {
		loaded.Projects = map[string]*contract.Project{}
	}
	return &loaded, "", nil
}

// saveIndex writes the index atomically: to a temporary file in the same
// directory, then renamed over the old one. A reader — another gitty, a
// plugin — sees the previous index or the new one, never part of either, and
// an interrupted or failed write leaves the previous index in place.
func saveIndex(root string, idx *contract.Index) error {
	sortLinks(idx.Links)
	sortUnresolved(idx.Unresolved)
	if idx.Links == nil {
		idx.Links = []contract.Link{}
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the index: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Join(root, ConfigDir)
	tmp, err := os.CreateTemp(dir, indexFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("writing the index: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing the index: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("writing the index: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing the index: %w", err)
	}
	if err := os.Rename(tmp.Name(), indexPath(root)); err != nil {
		return fmt.Errorf("writing the index: %w", err)
	}
	committed = true
	return nil
}

// headOf returns the commit a checkout is at, or "" when it has none yet.
//
// It is asked of every checkout each time the index is brought up to date,
// so the usual case is answered from git's own files — HEAD, then the ref it
// names, loose or packed — without starting a process. Anything those files
// do not settle (a worktree or submodule, whose .git is a file; a ref
// backend that keeps refs elsewhere) is asked of git.
func headOf(ctx context.Context, git gitRunner, dir string) string {
	if h := headFromFiles(dir); h != "" {
		return h
	}
	out, err := git(ctx, dir, nil, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		return ""
	}
	return objectID(strings.TrimSpace(string(out)))
}

func headFromFiles(dir string) string {
	gitDir := filepath.Join(dir, ".git")
	data, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "" // .git is a file, or not there
	}
	head := strings.TrimSpace(string(data))
	name, symbolic := strings.CutPrefix(head, "ref: ")
	if !symbolic {
		return objectID(head) // detached
	}
	name = strings.TrimSpace(name)
	if !strings.HasPrefix(name, "refs/") || strings.Contains(name, "..") {
		return ""
	}
	if data, err := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(name))); err == nil {
		return objectID(strings.TrimSpace(string(data)))
	}
	f, err := os.Open(filepath.Join(gitDir, "packed-refs"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if id, ref, ok := strings.Cut(sc.Text(), " "); ok && ref == name {
			return objectID(id)
		}
	}
	return ""
}

// objectID returns s if it is a full object id, "" otherwise.
func objectID(s string) string {
	if len(s) != 40 && len(s) != 64 {
		return ""
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return ""
		}
	}
	return s
}

// --- bringing the index up to date ---

// indexEvent is one thing a refresh did or found, for the caller to print.
type indexEvent struct {
	Kind    string // "index", "link", "unresolved", "skip" or "error"
	Project string
	Detail  string // index: new, changed, unchanged, removed; skip and error: why
	Link    contract.Link
	Unres   contract.Unresolved
}

// refreshOptions say how far a refresh goes.
type refreshOptions struct {
	// List reads project metadata from the API. Without it the API is used
	// only when the index does not exist yet, or to find out about a
	// checkout it has never heard of.
	List bool
	// RescanAll rescans every checkout; Rescan names projects to rescan
	// whatever their HEAD. A checkout whose HEAD moved is always rescanned.
	RescanAll bool
	Rescan    map[string]bool
	// DryRun does everything except write the index file.
	DryRun bool
	// Report receives each event as it is decided; nil discards them.
	Report func(indexEvent)
}

// refreshStats summarise an index after a refresh.
type refreshStats struct {
	Projects   int  // projects the index knows
	Scanned    int  // checkouts read by this refresh
	Links      int  // links in the index
	Unresolved int  // unresolved references in the index
	Errors     int  // checkouts that could not be scanned
	Written    bool // the index file was rewritten
}

// refreshIndex brings the workspace's index up to date and returns it.
//
// Project metadata comes from the API; links come from the checkouts. A
// checkout is read again only when the commit it is at has moved since it
// was last read, so on a workspace where nothing changed this is a directory
// walk and one small file read per checkout, and writes nothing.
func (w *workspace) refreshIndex(ctx context.Context, opts refreshOptions) (*contract.Index, refreshStats, error) {
	cfg := w.s.cfg
	report := opts.Report
	if report == nil {
		report = func(indexEvent) {}
	}
	var stats refreshStats

	idx, why, err := loadIndex(w.Root, cfg)
	if err != nil {
		return nil, stats, err
	}
	changed := false
	mustList := opts.List
	if idx == nil {
		if w.s.verbose {
			w.s.diagf("building the index: %s", why)
		}
		idx = newIndex(cfg)
		mustList, changed = true, true
	}
	rescanAll := opts.RescanAll
	if idx.Scanner != contract.ScannerVersion {
		idx.Scanner = contract.ScannerVersion
		rescanAll, changed = true, true
	}
	// Links are found with the exclude list in effect, so a different list
	// means looking again: an unchanged checkout may hold a reference that
	// is now a link, or one that no longer is.
	if exclude := normalExcludes(cfg.Exclude); !equalStrings(idx.Exclude, exclude) {
		idx.Exclude = exclude
		rescanAll, changed = true, true
	}

	dirs, err := findRepos(".")
	if err != nil {
		return nil, stats, fmt.Errorf("walking the workspace: %w", err)
	}
	sort.Strings(dirs)
	type checkout struct{ dir, project string }
	checkouts := make([]checkout, 0, len(dirs))
	onDisk := make(map[string]bool, len(dirs))
	unknown := false
	for _, d := range dirs {
		project := path.Join(cfg.RootPath, d)
		if cfg.Excluded(project) {
			report(indexEvent{Kind: "skip", Project: project, Detail: "excluded"})
			continue
		}
		checkouts = append(checkouts, checkout{dir: d, project: project})
		onDisk[project] = true
		if _, known := idx.Projects[project]; !known {
			unknown = true
		}
	}

	// A checkout the index has never heard of may be a project created
	// upstream since the last listing. That is worth one listing to find
	// out — but not worth failing over: offline, or without a token, the
	// checkout is indexed as it stands and the next `gitty index` settles it.
	optional := false
	if unknown && !mustList && w.requireAPI() == nil {
		mustList, optional = true, true
	}
	if mustList {
		err := w.requireAPI()
		var listed []*gitlab.Project
		if err == nil {
			listed, err = w.listProjects(ctx)
		}
		switch {
		case err == nil:
			applyListing(idx, cfg, listed)
			var known []string
			for _, c := range checkouts {
				if meta := idx.Projects[c.project]; meta != nil && meta.ID != 0 {
					known = append(known, c.project)
				}
			}
			w.lookUpCheckouts(ctx, idx, known)
			changed = true
		case optional:
			w.s.diagf("note: project metadata was not refreshed (%v); checkouts unknown to the index are listed without it until 'gitty index' can reach the instance", err)
		default:
			return nil, stats, err
		}
	}
	if ctx.Err() != nil {
		return nil, stats, errInterrupted
	}

	// Checkouts that are gone take their links with them: a link is evidence
	// in a file, and the file is no longer here.
	var removed []string
	for project, meta := range idx.Projects {
		if meta.Local != nil && !onDisk[project] {
			removed = append(removed, project)
		}
	}
	sort.Strings(removed)
	for _, project := range removed {
		dropScanned(idx, project)
		if idx.Projects[project].ID == 0 {
			delete(idx.Projects, project) // it only ever existed as a checkout
		} else {
			idx.Projects[project].Local = nil
		}
		changed = true
		report(indexEvent{Kind: "index", Project: project, Detail: "removed"})
	}

	// Decide what to read.
	type job struct {
		checkout
		head  string
		state string
		links bool
		out   scanOutcome
		err   error
	}
	jobs := make([]*job, 0, len(checkouts))
	var toScan []*job
	for _, c := range checkouts {
		meta := idx.Projects[c.project]
		if meta == nil {
			meta = &contract.Project{} // a checkout with no project upstream
			idx.Projects[c.project] = meta
			changed = true
		}
		j := &job{checkout: c, head: headOf(ctx, w.s.git, c.dir)}
		// An archived project is described but not asked what it uses,
		// unless archived projects were asked for.
		j.links = !meta.Archived || w.s.includeArchived
		switch {
		case meta.Local == nil:
			j.state = "new"
		case meta.Local.Dir != c.dir || meta.Local.Head != j.head:
			j.state = "changed"
		case rescanAll || opts.Rescan[c.project]:
			j.state = "rescan"
		case meta.Archived && (w.s.includeArchived || hasScanned(idx, c.project)):
			// Whether an archived project is asked what it uses depends on
			// the run, not on its commit: look again whenever the answer
			// could differ from what is recorded.
			j.state = "rescan"
		default:
			j.state = "unchanged"
		}
		jobs = append(jobs, j)
		if j.state != "unchanged" {
			toScan = append(toScan, j)
		}
	}

	// Read them, several at a time. Results are applied afterwards, in
	// order, so the events and the index come out the same every run.
	sc := &scanner{git: w.s.git, r: newResolver(idx, cfg), instance: cfg.URL}
	var mu sync.Mutex
	forEachConcurrent(ctx, w.s.jobs, toScan, func(j *job) {
		out, err := sc.scan(ctx, j.dir, j.project, j.links)
		mu.Lock()
		j.out, j.err = out, err
		mu.Unlock()
	})
	if ctx.Err() != nil {
		return nil, stats, errInterrupted
	}

	for _, j := range jobs {
		if j.state == "unchanged" {
			report(indexEvent{Kind: "index", Project: j.project, Detail: "unchanged"})
			continue
		}
		if j.err != nil {
			stats.Errors++
			report(indexEvent{Kind: "error", Project: j.project, Detail: "scanning failed"})
			w.s.diagf("%s: %v", j.project, j.err)
			continue
		}
		stats.Scanned++
		differs := applyScan(idx, j.project, j.dir, j.head, j.out)
		state := j.state
		if state == "rescan" {
			// Asked to look again whatever the commit: say what was found.
			state = "unchanged"
			if differs {
				state = "changed"
			}
		}
		changed = changed || differs
		report(indexEvent{Kind: "index", Project: j.project, Detail: state})
		for _, l := range j.out.Links {
			report(indexEvent{Kind: "link", Project: j.project, Link: l})
		}
		for _, u := range j.out.Unresolved {
			report(indexEvent{Kind: "unresolved", Project: j.project, Unres: u})
		}
	}

	// Fork edges come from metadata, not from a scan; report the ones this
	// refresh is the first to record.
	known := map[contract.Link]bool{}
	for _, l := range idx.Links {
		if strings.HasPrefix(l.Evidence, apiEvidence) {
			known[l] = true
		}
	}
	if rebuildAPILinks(idx, cfg) {
		changed = true
		for _, l := range idx.Links {
			if strings.HasPrefix(l.Evidence, apiEvidence) && !known[l] {
				report(indexEvent{Kind: "link", Project: l.From, Link: l})
			}
		}
	}
	if reconcile(idx, newResolver(idx, cfg)) {
		changed = true
	}

	stats.Projects = len(idx.Projects)
	stats.Links = len(idx.Links)
	stats.Unresolved = len(idx.Unresolved)
	sortLinks(idx.Links)
	sortUnresolved(idx.Unresolved)
	if changed {
		idx.GeneratedAt = time.Now().UTC().Truncate(time.Second)
		if !opts.DryRun {
			if err := saveIndex(w.Root, idx); err != nil {
				return nil, stats, err
			}
			stats.Written = true
		}
	}
	return idx, stats, nil
}

// listProjects reads the metadata of every project in the workspace's part
// of the instance. A workspace anchored at the instance's top level has no
// single group to ask about, so each top-level directory it holds is listed
// as the group it stands for; one that is not a group (a user's namespace)
// is noted and passed over.
func (w *workspace) listProjects(ctx context.Context) ([]*gitlab.Project, error) {
	root := w.s.cfg.RootPath
	if root != "" {
		projects, err := w.s.src.NamespaceProjects(ctx, root)
		if err != nil {
			return nil, fmt.Errorf("listing projects for %s: %w", root, err)
		}
		return projects, nil
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("reading the workspace root: %w", err)
	}
	var all []*gitlab.Project
	var firstErr error
	listed := 0
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || name == ConfigDir || strings.Contains(name, brokenAsideMarker) || strings.Contains(name, orphanAsideMarker) {
			continue
		}
		if w.s.cfg.Excluded(name) || classifyDest(name) == destRepo {
			continue
		}
		projects, err := w.s.src.NamespaceProjects(ctx, name)
		if err != nil {
			if ctx.Err() != nil {
				return nil, errInterrupted
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("listing projects for %s: %w", name, err)
			}
			w.s.diagf("note: could not list projects for %s (%v); its checkouts are indexed without metadata", name, err)
			continue
		}
		listed++
		all = append(all, projects...)
	}
	if listed == 0 && firstErr != nil {
		return nil, firstErr
	}
	return all, nil
}

// lookUpCheckouts asks the instance about each checked-out project on its
// own, for the two things a listing leaves out: where it was forked from,
// and — from any one of them — where the instance's container registry is.
// It is one request per checkout, several at a time, and none of it is
// essential: a lookup that fails leaves that project as the listing had it.
func (w *workspace) lookUpCheckouts(ctx context.Context, idx *contract.Index, projects []string) {
	var mu sync.Mutex
	forEachConcurrent(ctx, listConcurrency, projects, func(project string) {
		p, err := w.s.src.Project(ctx, project)
		if err != nil || p == nil {
			if w.s.verbose && ctx.Err() == nil {
				w.s.diagf("%s: could not be looked up (%v)", project, err)
			}
			return
		}
		mu.Lock()
		defer mu.Unlock()
		applyDetail(idx, project, p)
	})
}

// applyDetail records what a single project's full representation adds to
// its listing entry.
func applyDetail(idx *contract.Index, project string, p *gitlab.Project) {
	meta := idx.Projects[project]
	if meta == nil {
		return
	}
	meta.ForkedFrom = ""
	if p.ForkedFromProject != nil {
		meta.ForkedFrom = p.ForkedFromProject.PathWithNamespace
	}
	// The registry names a project's images <registry>/<path, lower-cased>;
	// take the path off and what is left is the registry, the same for
	// every project of the instance.
	if prefix := p.ContainerRegistryImagePrefix; prefix != "" {
		suffix := "/" + strings.ToLower(project)
		if registry, ok := strings.CutSuffix(strings.ToLower(prefix), suffix); ok && registry != "" {
			idx.Registry = registry
		}
	}
}

// applyListing replaces the index's project metadata with a fresh listing,
// keeping what is known about each project's checkout.
func applyListing(idx *contract.Index, cfg *Config, listed []*gitlab.Project) {
	old := idx.Projects
	idx.Projects = make(map[string]*contract.Project, len(listed))
	hosts := map[string]bool{}
	for _, p := range listed {
		if p == nil || p.PathWithNamespace == "" {
			continue
		}
		meta := &contract.Project{
			ID:            p.ID,
			Description:   strings.TrimSpace(p.Description),
			DefaultBranch: p.DefaultBranch,
			Topics:        p.Topics,
			Archived:      p.Archived,
			WebURL:        p.WebURL,
		}
		// What only a checkout or a per-project lookup knows is kept: the
		// listing has no opinion on it.
		if prev := old[p.PathWithNamespace]; prev != nil {
			meta.Local = prev.Local
			meta.ForkedFrom = prev.ForkedFrom
		}
		idx.Projects[p.PathWithNamespace] = meta
		for _, raw := range []string{p.WebURL, p.HTTPURLToRepo, p.SSHURLToRepo} {
			if h := extractHost(raw); h != "" {
				hosts[h] = true
			}
		}
	}
	// A checkout whose project is no longer listed stays, as a checkout.
	for project, prev := range old {
		if _, still := idx.Projects[project]; !still && prev.Local != nil {
			idx.Projects[project] = &contract.Project{Local: prev.Local}
		}
	}

	delete(hosts, extractHost(cfg.URL))
	idx.Hosts = idx.Hosts[:0]
	for h := range hosts {
		idx.Hosts = append(idx.Hosts, h)
	}
	sort.Strings(idx.Hosts)
	idx.ListedAt = time.Now().UTC().Truncate(time.Second)
}

// rebuildAPILinks recomputes the links that come from project metadata
// rather than from files — fork edges, the one relationship that is a fact
// about a project and not something written in it — and reports whether
// they changed. It runs on every refresh, not only after a listing: what
// counts as a link also depends on the exclude list.
func rebuildAPILinks(idx *contract.Index, cfg *Config) bool {
	const evidence = apiEvidence + "forked_from_project"
	var links []contract.Link
	var unresolved []contract.Unresolved
	for project, meta := range idx.Projects {
		origin := meta.ForkedFrom
		if origin == "" || origin == project || cfg.Excluded(project) || cfg.Excluded(origin) {
			continue
		}
		if target := idx.Projects[origin]; target != nil && target.ID != 0 {
			links = append(links, contract.Link{From: project, To: origin, Kind: contract.KindFork, Evidence: evidence})
		} else {
			unresolved = append(unresolved, contract.Unresolved{From: project, Ref: origin, Kind: contract.KindFork, Evidence: evidence})
		}
	}
	sortLinks(links)
	sortUnresolved(unresolved)

	var oldLinks, keptLinks []contract.Link
	for _, l := range idx.Links {
		if strings.HasPrefix(l.Evidence, apiEvidence) {
			oldLinks = append(oldLinks, l)
		} else {
			keptLinks = append(keptLinks, l)
		}
	}
	var oldUnresolved, keptUnresolved []contract.Unresolved
	for _, u := range idx.Unresolved {
		if strings.HasPrefix(u.Evidence, apiEvidence) {
			oldUnresolved = append(oldUnresolved, u)
		} else {
			keptUnresolved = append(keptUnresolved, u)
		}
	}
	sortLinks(oldLinks)
	sortUnresolved(oldUnresolved)
	if equalLinks(oldLinks, links) && equalUnresolved(oldUnresolved, unresolved) {
		return false
	}
	idx.Links = append(keptLinks, links...)
	idx.Unresolved = append(keptUnresolved, unresolved...)
	return true
}

// normalExcludes puts an exclude list in a form that compares equal whenever
// it excludes the same things: trimmed, deduplicated, sorted.
func normalExcludes(patterns []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, pat := range patterns {
		pat = strings.Trim(strings.TrimSpace(pat), "/")
		if pat != "" && !seen[pat] {
			seen[pat] = true
			out = append(out, pat)
		}
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// dropScanned removes everything a project's checkout contributed to the
// index: the links and unresolved references found in its files. What the
// API said about it stays.
func dropScanned(idx *contract.Index, project string) {
	links := idx.Links[:0]
	for _, l := range idx.Links {
		if l.From != project || strings.HasPrefix(l.Evidence, apiEvidence) {
			links = append(links, l)
		}
	}
	idx.Links = links
	unresolved := idx.Unresolved[:0]
	for _, u := range idx.Unresolved {
		if u.From != project || strings.HasPrefix(u.Evidence, apiEvidence) {
			unresolved = append(unresolved, u)
		}
	}
	idx.Unresolved = unresolved
}

// hasScanned reports whether a project's checkout contributes anything.
func hasScanned(idx *contract.Index, project string) bool {
	links, unresolved := scannedOf(idx, project)
	return len(links)+len(unresolved) > 0
}

// scannedOf returns what a project's checkout currently contributes.
func scannedOf(idx *contract.Index, project string) (links []contract.Link, unresolved []contract.Unresolved) {
	for _, l := range idx.Links {
		if l.From == project && !strings.HasPrefix(l.Evidence, apiEvidence) {
			links = append(links, l)
		}
	}
	for _, u := range idx.Unresolved {
		if u.From == project && !strings.HasPrefix(u.Evidence, apiEvidence) {
			unresolved = append(unresolved, u)
		}
	}
	sortLinks(links)
	sortUnresolved(unresolved)
	return links, unresolved
}

// applyScan records what a checkout yielded and reports whether that
// differs from what the index already held for it.
func applyScan(idx *contract.Index, project, dir, head string, out scanOutcome) bool {
	meta := idx.Projects[project]
	local := &contract.Local{Dir: dir, Head: head, Docs: out.Docs, Summary: out.Summary}
	oldLinks, oldUnresolved := scannedOf(idx, project)
	same := meta.Local != nil && sameLocal(meta.Local, local) &&
		equalLinks(oldLinks, out.Links) && equalUnresolved(oldUnresolved, out.Unresolved)
	if same {
		return false
	}
	meta.Local = local
	dropScanned(idx, project)
	idx.Links = append(idx.Links, out.Links...)
	idx.Unresolved = append(idx.Unresolved, out.Unresolved...)
	return true
}

// sameLocal compares two descriptions of a checkout by content: a list that
// is empty and one that is absent are the same thing once written to disk.
func sameLocal(a, b *contract.Local) bool {
	return a.Dir == b.Dir && a.Head == b.Head && a.Summary == b.Summary && equalStrings(a.Docs, b.Docs)
}

func equalLinks(a, b []contract.Link) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalUnresolved(a, b []contract.Unresolved) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// reconcile squares the links of checkouts that were not rescanned with the
// projects the index now knows. A link whose target is gone (deleted or
// moved upstream) becomes an unresolved reference; an unresolved reference
// whose target has appeared becomes a link. Without this a project created
// upstream would stay "unresolved" in every unchanged checkout until each
// one moved. Nothing that starts or ends at an excluded project survives.
// It reports whether anything changed.
func reconcile(idx *contract.Index, r *resolver) bool {
	changed := false
	have := map[string]bool{}
	key := func(from, to, kind string) string { return from + "\x00" + to + "\x00" + kind }

	links := idx.Links[:0:0]
	var demoted []contract.Unresolved
	for _, l := range idx.Links {
		switch {
		case r.leftOut(l.From) || r.leftOut(l.To):
			changed = true
		case !r.knows(l.To):
			changed = true
			demoted = append(demoted, contract.Unresolved{From: l.From, Ref: l.To, Kind: l.Kind, Evidence: l.Evidence})
		default:
			have[key(l.From, l.To, l.Kind)] = true
			links = append(links, l)
		}
	}

	unresolved := idx.Unresolved[:0:0]
	seen := map[string]bool{}
	for _, u := range append(idx.Unresolved[:len(idx.Unresolved):len(idx.Unresolved)], demoted...) {
		if r.leftOut(u.From) || r.leftOut(u.Ref) {
			changed = true
			continue
		}
		target, ok := r.exact(u.Ref)
		if !ok && (u.Kind == contract.KindDepends || u.Kind == contract.KindCIComponent || u.Kind == contract.KindMentions) {
			// These came out of a longer path: the project is its prefix.
			target, ok = r.lookup(u.Ref)
		}
		if ok {
			changed = true
			if target != u.From && !have[key(u.From, target, u.Kind)] {
				have[key(u.From, target, u.Kind)] = true
				links = append(links, contract.Link{From: u.From, To: target, Kind: u.Kind, Evidence: u.Evidence})
			}
			continue
		}
		k := key(u.From, strings.ToLower(u.Ref), "")
		if seen[k] {
			changed = true
			continue
		}
		seen[k] = true
		unresolved = append(unresolved, u)
	}
	idx.Links, idx.Unresolved = links, unresolved
	return changed
}
