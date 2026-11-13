package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gitty/internal/contract"
)

// What the hub holds.
//
//	<data>/tree.json        groups and projects, from the instance's listing
//	<data>/facts/<id>.json  one project's repository, as gitty read it
//	<data>/notes.jsonl      every note ever recorded, one a line
//
// Three kinds of knowledge, kept apart because they are trusted apart: what
// a repository says, what GitLab says, what an agent noted. Everything is
// keyed by GitLab's numeric ids, so a project that moves keeps what is known
// about it. All of it is small, plain, and read whole into memory at start.

const (
	treeVersion  = 1
	notesVersion = 1
)

// node is a group or a project as the listing describes it.
type node struct {
	ID          int64  `json:"id"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
	Visibility  string `json:"visibility,omitempty"`

	// Projects only.
	Topics        []string  `json:"topics,omitempty"`
	Archived      bool      `json:"archived,omitempty"`
	LastActivity  time.Time `json:"last_activity,omitempty"`
	Stars         int       `json:"stars,omitempty"`
	Forks         int       `json:"forks,omitempty"`
	DefaultBranch string    `json:"default_branch,omitempty"`
	WebURL        string    `json:"web_url,omitempty"`
}

func (n *node) name() string {
	if i := strings.LastIndexByte(n.Path, '/'); i >= 0 {
		return n.Path[i+1:]
	}
	return n.Path
}

// treeFile is tree.json.
type treeFile struct {
	Version  int       `json:"version"`
	Instance string    `json:"instance"`
	ListedAt time.Time `json:"listed_at"`
	Groups   []*node   `json:"groups"`
	Projects []*node   `json:"projects"`
}

// use is one project's evidenced link to another, as seen from the target.
type use struct {
	From     *node
	Kind     string
	Evidence string
	Target   string
}

// store is everything the hub knows, in memory, with the files behind it.
type store struct {
	dir      string
	instance string

	mu       sync.RWMutex
	listedAt time.Time
	groups   map[string]*node // by lower-cased path
	projects map[string]*node
	groupID  map[int64]*node
	projID   map[int64]*node
	kids     map[string][]*node // group path (lower) → groups directly in it
	members  map[string][]*node // group path (lower) → projects directly in it
	beneath  map[string]int     // group path (lower) → projects anywhere under it
	roots    []*node

	facts  map[int64]*contract.Facts // by project id
	usedBy map[int64][]use           // by target project id; rebuilt when facts or the tree change

	book *notebook
	// notesRead is how much of the notes log has been replayed. The log is
	// only ever appended to, so everything past it is news — written by
	// this process or by another one working from the same directory.
	notesRead int64
	// several is set when other processes may be using this directory at
	// the same time: one person's machine, a server per agent session.
	several   bool
	factsSeen time.Time
}

func openStore(dir, instance string) (*store, error) {
	s := &store{dir: dir, instance: instance, facts: map[int64]*contract.Facts{}, book: newNotebook()}
	s.setTreeLocked(nil, nil, time.Time{})
	if err := os.MkdirAll(filepath.Join(dir, "facts"), 0o755); err != nil {
		return nil, fmt.Errorf("preparing %s: %w", dir, err)
	}
	if err := s.loadTree(); err != nil {
		return nil, err
	}
	if err := s.loadFacts(); err != nil {
		return nil, err
	}
	if err := s.loadNotes(); err != nil {
		return nil, err
	}
	s.rebuildUses()
	return s, nil
}

func (s *store) close() {}

// writeFileAtomic writes a file so that a reader sees the old one or the
// new one, never half of either.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// --- the tree ---

func (s *store) loadTree() error {
	data, err := os.ReadFile(filepath.Join(s.dir, "tree.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f treeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("%s is damaged (%w); remove it and the instance is listed again", filepath.Join(s.dir, "tree.json"), err)
	}
	if f.Version != treeVersion {
		return nil // an older or newer shape: list again
	}
	// A hub's data is about one instance. Pointed at another, what is here
	// would be attached to the wrong projects: ids are only unique within one.
	if f.Instance != "" && !strings.EqualFold(strings.TrimRight(f.Instance, "/"), strings.TrimRight(s.instance, "/")) {
		return fmt.Errorf("%s holds what is known about %s, not %s: use another --data directory", s.dir, f.Instance, s.instance)
	}
	s.setTreeLocked(f.Groups, f.Projects, f.ListedAt)
	return nil
}

// setTree replaces the listing and saves it.
func (s *store) setTree(groups, projects []*node, at time.Time) error {
	s.mu.Lock()
	s.setTreeLocked(groups, projects, at)
	s.rebuildUses()
	f := treeFile{Version: treeVersion, Instance: s.instance, ListedAt: at, Groups: groups, Projects: projects}
	s.mu.Unlock()
	data, err := json.MarshalIndent(f, "", " ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, "tree.json"), data)
}

func (s *store) setTreeLocked(groups, projects []*node, at time.Time) {
	s.listedAt = at
	s.groups, s.projects = map[string]*node{}, map[string]*node{}
	s.groupID, s.projID = map[int64]*node{}, map[int64]*node{}
	s.kids, s.members, s.beneath = map[string][]*node{}, map[string][]*node{}, map[string]int{}
	s.roots = nil
	for _, g := range groups {
		s.groups[strings.ToLower(g.Path)] = g
		s.groupID[g.ID] = g
	}
	for _, g := range groups {
		parent := parentOf(g.Path)
		if _, known := s.groups[strings.ToLower(parent)]; parent == "" || !known {
			s.roots = append(s.roots, g)
			continue
		}
		s.kids[strings.ToLower(parent)] = append(s.kids[strings.ToLower(parent)], g)
	}
	for _, p := range projects {
		s.projects[strings.ToLower(p.Path)] = p
		s.projID[p.ID] = p
		parent := strings.ToLower(parentOf(p.Path))
		s.members[parent] = append(s.members[parent], p)
		for g := parent; g != ""; g = parentOf(g) {
			s.beneath[g]++
		}
	}
	byPath := func(ns []*node) {
		sort.Slice(ns, func(i, j int) bool { return ns[i].Path < ns[j].Path })
	}
	byPath(s.roots)
	for _, ns := range s.kids {
		byPath(ns)
	}
	for _, ns := range s.members {
		byPath(ns)
	}
}

func parentOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return ""
}

// project and group look a node up by path, whatever its case.
func (s *store) project(path string) *node { return s.projects[strings.ToLower(cleanPath(path))] }
func (s *store) group(path string) *node   { return s.groups[strings.ToLower(cleanPath(path))] }

// cleanPath accepts a path the way people and agents write one.
func cleanPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".git")
	return p
}

// --- facts ---

func (s *store) factsPath(id int64) string {
	return filepath.Join(s.dir, "facts", fmt.Sprintf("%d.json", id))
}

func (s *store) loadFacts() error {
	entries, err := os.ReadDir(filepath.Join(s.dir, "facts"))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, "facts", e.Name()))
		if err != nil {
			return err
		}
		var f contract.Facts
		if json.Unmarshal(data, &f) != nil || f.Schema != contract.HubSchema || f.ID == 0 {
			continue // not something this version wrote: it will be sent again
		}
		s.facts[f.ID] = &f
	}
	return nil
}

// Bounds on what one project's facts may hold. gitty stays well inside
// them; they are here because the hub takes what it is sent.
const (
	maxFactLinks = 600
	maxFactFiles = 3000
	maxFactText  = 700
)

// errRefused is why facts were not stored: an answer for the sender, not a
// failure of the hub.
type errRefused struct{ reason string }

func (e *errRefused) Error() string { return e.reason }

// putFacts takes one project's facts. They replace what is held when they
// are of a commit that is not older; links to projects the hub does not
// cover are dropped, since nothing could be said about their far end.
func (s *store) putFacts(f *contract.Facts) (contract.HubReceipt, error) {
	receipt := contract.HubReceipt{Schema: contract.HubSchema, Project: f.Project}
	if f.Schema != contract.HubSchema {
		return receipt, &errRefused{fmt.Sprintf("facts are in format %d, this hub reads format %d: gitty and gitty-up come from different releases", f.Schema, contract.HubSchema)}
	}
	if f.Commit == "" || f.CommittedAt.IsZero() {
		return receipt, &errRefused{"facts must say which commit they were read at, and when it was made"}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.projID[f.ID]
	if p == nil {
		return receipt, &errRefused{"not a project this hub covers"}
	}
	if !strings.EqualFold(p.Path, f.Project) {
		// The id is what identifies a project. A path that differs means
		// the sender's listing is older than a move, or is wrong.
		return receipt, &errRefused{fmt.Sprintf("project %d is %s here, not %s", f.ID, p.Path, f.Project)}
	}
	if old := s.facts[f.ID]; old != nil {
		switch {
		case old.Commit == f.Commit && old.Scanner >= f.Scanner:
			receipt.Reason = "already known at this commit"
			receipt.Links = len(old.Links)
			return receipt, nil
		case f.CommittedAt.Before(old.CommittedAt):
			receipt.Reason = "a newer commit is already known"
			receipt.Links = len(old.Links)
			return receipt, nil
		}
	}

	clean := &contract.Facts{
		Schema: contract.HubSchema, Scanner: f.Scanner, Project: p.Path, ID: p.ID,
		Commit: clip(f.Commit, 64), CommittedAt: f.CommittedAt.UTC(),
		Summary: clip(oneLine(f.Summary), maxFactText),
	}
	for _, d := range f.Docs {
		if len(clean.Docs) < 20 && safeRelPath(d) {
			clean.Docs = append(clean.Docs, d)
		}
	}
	for _, pr := range f.Provides {
		if len(clean.Provides) >= 60 || pr.Name == "" || !knownProvided[pr.Kind] {
			continue
		}
		q := contract.Provided{Kind: pr.Kind, Name: clip(oneLine(pr.Name), 120), File: clip(pr.File, 300), About: clip(oneLine(pr.About), 300)}
		for _, in := range pr.Inputs {
			if len(q.Inputs) < 30 {
				q.Inputs = append(q.Inputs, clip(oneLine(in), 80))
			}
		}
		clean.Provides = append(clean.Provides, q)
	}
	for _, l := range f.Links {
		to := s.projects[strings.ToLower(l.To)]
		if to == nil || to.ID == p.ID || !knownKinds[l.Kind] || len(clean.Links) >= maxFactLinks {
			continue
		}
		clean.Links = append(clean.Links, contract.Link{
			From: p.Path, To: to.Path, Kind: l.Kind,
			Evidence: clip(oneLine(l.Evidence), 300), Target: clip(oneLine(l.Target), 240),
		})
	}
	if len(f.Files) > 0 {
		clean.Files = make(map[string]string, len(f.Files))
		for name, blob := range f.Files {
			if len(clean.Files) >= maxFactFiles {
				break
			}
			if safeRelPath(name) && len(blob) <= 64 {
				clean.Files[name] = blob
			}
		}
	}

	data, err := json.MarshalIndent(clean, "", " ")
	if err != nil {
		return receipt, err
	}
	if err := writeFileAtomic(s.factsPath(p.ID), data); err != nil {
		return receipt, fmt.Errorf("saving facts for %s: %w", p.Path, err)
	}
	s.facts[p.ID] = clean
	s.rebuildUses()
	receipt.Stored, receipt.Links = true, len(clean.Links)
	return receipt, nil
}

var knownKinds = map[string]bool{
	contract.KindDepends: true, contract.KindCIInclude: true, contract.KindCITrigger: true,
	contract.KindCIComponent: true, contract.KindImage: true, contract.KindSubmodule: true,
	contract.KindFork: true, contract.KindMentions: true, contract.KindDeploys: true,
}

var knownProvided = map[string]bool{
	contract.ProvidesCIComponent: true, contract.ProvidesCITemplate: true, contract.ProvidesPolicy: true,
	contract.ProvidesHelmChart: true, contract.ProvidesTerraform: true, contract.ProvidesImage: true,
}

// wants says which of the offered checkouts the hub would take.
func (s *store) wants(have []contract.HubHead) contract.HubWanted {
	out := contract.HubWanted{Schema: contract.HubSchema, Want: []string{}, Skipped: map[string]string{}}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range have {
		p := s.projID[h.ID]
		switch {
		case p == nil:
			out.Skipped[h.Project] = "not covered"
		case !strings.EqualFold(p.Path, h.Project):
			out.Skipped[h.Project] = "known here as " + p.Path
		case s.facts[h.ID] != nil && s.facts[h.ID].Commit == h.Commit && s.facts[h.ID].Scanner >= contract.ScannerVersion:
			out.Skipped[h.Project] = "known"
		default:
			out.Want = append(out.Want, h.Project)
		}
	}
	return out
}

// rebuildUses works out, for every project, who links to it. Callers hold
// the write lock (or are still alone, at start).
func (s *store) rebuildUses() {
	s.usedBy = map[int64][]use{}
	for id, f := range s.facts {
		from := s.projID[id]
		if from == nil {
			continue // the project has left the listing; its facts wait for it
		}
		for _, l := range f.Links {
			to := s.projects[strings.ToLower(l.To)]
			if to == nil || to.ID == id {
				continue
			}
			s.usedBy[to.ID] = append(s.usedBy[to.ID], use{From: from, Kind: l.Kind, Evidence: l.Evidence, Target: l.Target})
		}
	}
	for _, us := range s.usedBy {
		sort.Slice(us, func(i, j int) bool {
			if us[i].From.Path != us[j].From.Path {
				return us[i].From.Path < us[j].From.Path
			}
			return us[i].Kind < us[j].Kind
		})
	}
}

// --- notes ---

func (s *store) notesPath() string { return filepath.Join(s.dir, "notes.jsonl") }

func (s *store) loadNotes() error {
	unlock, err := s.lockNotes()
	if err != nil {
		return err
	}
	defer unlock()
	return s.catchUp(true)
}

// catchUp replays what has been appended to the notes log since it was last
// read. At start that is all of it. Later it is whatever another process on
// the same directory has recorded in the meantime: the log is the one place
// notes live, and every process reading it in order arrives at the same
// state.
//
// A last line with no end is a write cut short. With mend, at start and
// under the lock, it is cut off, so that the log stays one note a line; at
// any other time it is left alone, since its writer may not have finished.
// Callers hold the store's write lock.
func (s *store) catchUp(mend bool) error {
	path := s.notesPath()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() < s.notesRead {
		// Shorter than what was read: not the log this process knew.
		s.book, s.notesRead = newNotebook(), 0
	}
	if info.Size() == s.notesRead {
		return nil
	}
	data := make([]byte, info.Size()-s.notesRead)
	if _, err := f.ReadAt(data, s.notesRead); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	for len(data) > 0 {
		end := strings.IndexByte(string(data), '\n')
		if end < 0 {
			if mend {
				return os.Truncate(path, s.notesRead)
			}
			return nil
		}
		line := strings.TrimSpace(string(data[:end]))
		if line != "" {
			var n note
			if err := json.Unmarshal([]byte(line), &n); err != nil {
				// Notes are the one thing here that cannot be rebuilt, so
				// a line that is whole and still not a note stops the hub
				// rather than being read past.
				return fmt.Errorf("%s is damaged at byte %d (%w)", path, s.notesRead, err)
			}
			if n.V == notesVersion {
				s.book.apply(&n)
			}
		}
		s.notesRead += int64(end) + 1
		data = data[end+1:]
	}
	return nil
}

// lockNotes takes the lock on the notes log, so that two processes on one
// directory record one after the other and number their notes apart. It is
// a file that exists while the lock is held: the one kind of lock every
// platform has. One left behind by a process that died is taken over.
func (s *store) lockNotes() (unlock func(), err error) {
	path := filepath.Join(s.dir, "notes.lock")
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > 30*time.Second {
			os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the notes log in %s is locked by another gitty-up and stayed so", s.dir)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// record makes a note durable, then current. make is given the notebook as
// it stands once everything recorded so far, by anyone, has been read, and
// returns the note to add. Callers hold the store's write lock.
func (s *store) record(make func(*notebook) (*note, error)) (*note, error) {
	unlock, err := s.lockNotes()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := s.catchUp(false); err != nil {
		return nil, err
	}
	n, err := make(s.book)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(n)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.notesPath(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("recording the note: %w", err)
	}
	_, werr := f.Write(append(data, '\n'))
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, fmt.Errorf("recording the note: %w", werr)
	}
	return n, s.catchUp(false)
}

// seeOthers brings in what other processes on the same directory have
// written since this one last looked: notes, and facts their gitty sent
// them. It does nothing for a hub that has its directory to itself.
func (s *store) seeOthers() {
	if !s.several {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.catchUp(false)
	if info, err := os.Stat(filepath.Join(s.dir, "facts")); err == nil && !info.ModTime().Equal(s.factsSeen) {
		s.factsSeen = info.ModTime()
		s.facts = map[int64]*contract.Facts{}
		if s.loadFacts() == nil {
			s.rebuildUses()
		}
	}
}

// --- listing ---

// list reads the instance's groups and projects and makes them the tree.
// Only what is public or internal is kept: nothing the hub says may tell a
// user of the instance something they could not have browsed to.
func (s *store) list(ctx context.Context, gl *gitlab, rootPaths []string, now time.Time) error {
	var roots []glGroup
	if len(rootPaths) == 0 {
		all, err := gl.rootGroups(ctx)
		if err != nil {
			return fmt.Errorf("listing the instance's top-level groups: %w", err)
		}
		roots = all
	}
	for _, p := range rootPaths {
		g, err := gl.group(ctx, p)
		if err != nil {
			return fmt.Errorf("looking up group %s: %w", p, err)
		}
		roots = append(roots, g)
	}

	var groups, projects []*node
	seenGroup, seenProject := map[int64]bool{}, map[int64]bool{}
	open := func(v string) bool { return v == "public" || v == "internal" }
	addGroup := func(g glGroup) {
		if !seenGroup[g.ID] && open(g.Visibility) {
			seenGroup[g.ID] = true
			groups = append(groups, &node{ID: g.ID, Path: g.FullPath, Description: oneLine(g.Description), Visibility: g.Visibility})
		}
	}
	for _, root := range roots {
		addGroup(root)
		below, err := gl.descendants(ctx, root.ID)
		if err != nil {
			return fmt.Errorf("listing the groups beneath %s: %w", root.FullPath, err)
		}
		for _, g := range below {
			addGroup(g)
		}
		// Live and archived projects are two listings; they are asked for
		// side by side.
		var lists [2][]glProject
		var errs [2]error
		var wg sync.WaitGroup
		for i, archived := range []bool{false, true} {
			wg.Add(1)
			go func(i int, archived bool) {
				defer wg.Done()
				lists[i], errs[i] = gl.projects(ctx, root.ID, archived)
			}(i, archived)
		}
		wg.Wait()
		for i, archived := range []bool{false, true} {
			if errs[i] != nil {
				return fmt.Errorf("listing the projects beneath %s: %w", root.FullPath, errs[i])
			}
			for _, p := range lists[i] {
				if seenProject[p.ID] || !open(p.Visibility) {
					continue
				}
				seenProject[p.ID] = true
				projects = append(projects, &node{
					ID: p.ID, Path: p.PathWithNamespace, Description: oneLine(p.Description), Visibility: p.Visibility,
					Topics: p.Topics, Archived: archived, LastActivity: p.LastActivityAt.UTC(),
					Stars: p.StarCount, Forks: p.ForksCount, DefaultBranch: p.DefaultBranch, WebURL: p.WebURL,
				})
			}
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Path < groups[j].Path })
	sort.Slice(projects, func(i, j int) bool { return projects[i].Path < projects[j].Path })
	return s.setTree(groups, projects, now.UTC().Truncate(time.Second))
}

// --- small text helpers ---

// oneLine flattens text to a single line without control characters: what
// is stored here is shown to a model, and a line is the unit it is shown in.
func oneLine(text string) string {
	return strings.Join(strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029
	}), " ")
}

// clip shortens text to at most n runes.
func clip(text string, n int) string {
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	if n <= 1 {
		return string(r[:n])
	}
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}

// safeRelPath reports whether p is a plain relative path inside a
// repository: what a file reference is allowed to look like.
func safeRelPath(p string) bool {
	if p == "" || len(p) > 400 || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00\n\r\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}
