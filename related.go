package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gitty/internal/contract"
)

// subject is what `gitty related` and `gitty context` report on: one
// project, or a group — every project beneath a namespace path.
type subject struct {
	Group bool
	// Path is the project's namespace path, or the group's. A group path of
	// "" is the whole of a workspace anchored at the instance's top level.
	Path string
}

func (s subject) String() string {
	if s.Group && s.Path == "" {
		return "/"
	}
	return s.Path
}

// contains reports whether a project belongs to the subject.
func (s subject) contains(project string) bool {
	if !s.Group {
		return project == s.Path
	}
	return s.Path == "" || strings.HasPrefix(project, s.Path+"/")
}

// subjectAt returns the subject a directory inside the workspace stands for:
// the project whose checkout it is in, or else the group it is.
func (w *workspace) subjectAt(dir string) (subject, bool) {
	rel, err := filepath.Rel(w.Root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return subject{}, false
	}
	groupDir, below := contextBelow(w.Root, dir)
	group := w.s.cfg.RootPath
	if below != "" {
		group = path.Join(group, below)
	}
	if rest, err := filepath.Rel(groupDir, dir); err == nil && rest != "." {
		first, _, _ := strings.Cut(filepath.ToSlash(rest), "/")
		if classifyDest(filepath.Join(groupDir, first)) == destRepo {
			return subject{Path: path.Join(group, first)}, true
		}
		return subject{}, false // below a group, but not a checkout: nothing gitty knows
	}
	return subject{Group: true, Path: group}, true
}

// subjectDir resolves an argument that names a directory, relative to where
// the command was started.
func (w *workspace) subjectDir(arg string) (subject, bool) {
	dir := arg
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(w.StartDir, dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return subject{}, false
	}
	return w.subjectAt(filepath.Clean(dir))
}

// subjectNames returns the namespace paths an argument could mean, most
// likely first: relative to the group the command was started in, the way
// `gitty ls` and `gitty sync` read their argument, then as a full path.
// A leading slash says "full path" outright.
func (w *workspace) subjectNames(arg string) []string {
	clean := strings.Trim(path.Clean("/"+strings.TrimSpace(arg)), "/")
	if clean == "" {
		return nil
	}
	if strings.HasPrefix(strings.TrimSpace(arg), "/") || w.Group == "" {
		return []string{clean}
	}
	return []string{path.Join(w.Group, clean), clean}
}

// resolveSubject decides what an argument refers to:
//
//	(none)        the project the command was started in; in a group's
//	              directory, that group
//	a directory   the project checked out there, or the group it is
//	a path        a project of the index, else a group of it — relative to
//	              the current group first, then from the instance's root
func (w *workspace) resolveSubject(idx *contract.Index, arg string) (subject, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		if w.Project != "" {
			return subject{Path: w.Project}, nil
		}
		return subject{Group: true, Path: w.Group}, nil
	}
	if s, ok := w.subjectDir(arg); ok {
		return s, nil
	}
	names := w.subjectNames(arg)
	lower := make(map[string]string, len(idx.Projects))
	for p := range idx.Projects {
		lower[strings.ToLower(p)] = p
	}
	for _, name := range names {
		if p, ok := lower[strings.ToLower(name)]; ok {
			return subject{Path: p}, nil
		}
	}
	for _, name := range names {
		prefix := strings.ToLower(name) + "/"
		for p := range lower {
			if strings.HasPrefix(p, prefix) {
				return subject{Group: true, Path: name}, nil
			}
		}
	}
	return subject{}, usageErrf("no project or group %q in this workspace's index ('gitty index' refreshes it from the instance)", arg)
}

// rescanHints names the projects a subject argument might turn out to be,
// before the index is there to say: the ones to read again regardless of
// their commit, so that an uncommitted edit to the very project being asked
// about is seen.
func (w *workspace) rescanHints(arg string) map[string]bool {
	hints := map[string]bool{}
	arg = strings.TrimSpace(arg)
	if arg == "" {
		if w.Project != "" {
			hints[w.Project] = true
		}
		return hints
	}
	if s, ok := w.subjectDir(arg); ok {
		if !s.Group {
			hints[s.Path] = true
		}
		return hints
	}
	for _, name := range w.subjectNames(arg) {
		hints[name] = true
	}
	return hints
}

// ensureFresh returns the workspace's index, brought up to date first: built
// if there is none, checkouts whose commit moved reread, and the project
// named by arg reread whatever its commit. It needs the instance only to
// build the index, or to learn about a checkout it has never heard of.
func (w *workspace) ensureFresh(ctx context.Context, arg string) (*contract.Index, error) {
	idx, _, err := w.refreshIndex(ctx, refreshOptions{Rescan: w.rescanHints(arg)})
	if err != nil {
		return nil, fmt.Errorf("the workspace index could not be brought up to date: %w", err)
	}
	return idx, nil
}

// subjectLinks splits the index's links by how they touch a subject: uses
// (from it, to outside), usedBy (from outside, to it) and, for a group,
// within (between two of its own projects).
func subjectLinks(idx *contract.Index, s subject) (uses, usedBy, within []contract.Link) {
	for _, l := range idx.Links {
		from, to := s.contains(l.From), s.contains(l.To)
		switch {
		case from && to:
			within = append(within, l)
		case from:
			uses = append(uses, l)
		case to:
			usedBy = append(usedBy, l)
		}
	}
	sortLinks(uses)
	sortLinks(within)
	sort.Slice(usedBy, func(i, j int) bool {
		a, b := usedBy[i], usedBy[j]
		if a.To != b.To {
			return a.To < b.To
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.Kind < b.Kind
	})
	return uses, usedBy, within
}

// Directions a relationship can be asked about.
const (
	directionUses   = "uses"
	directionUsedBy = "used-by"
	directionBoth   = "both"
)

// parseDirection validates a --direction value.
func parseDirection(v string) (string, error) {
	switch v {
	case "", directionBoth:
		return directionBoth, nil
	case directionUses, directionUsedBy:
		return v, nil
	}
	return "", usageErrf("invalid --direction %q: choose %s, %s or %s", v, directionUses, directionUsedBy, directionBoth)
}

// relatedOptions bundles the related command's flags.
type relatedOptions struct {
	Subject   string
	Direction string
	Token     string
	Anon      bool
	Verbose   bool
	Jobs      int
}

// runRelated prints a project's links as greppable lines:
//
//	related <project> <kind> <target> <evidence>
//	related-by <project> <kind> <source> <evidence>
//	unresolved <project> <reference> <evidence>
//	summary uses=N used-by=N unresolved=N
//
// `related` is a project the subject uses; `related-by` one that uses it.
// The evidence is a file and line in whichever project holds the reference —
// the first-named one for `related`, the source for `related-by`.
func runRelated(ctx context.Context, opts relatedOptions) error {
	direction, err := parseDirection(opts.Direction)
	if err != nil {
		return err
	}
	if opts.Jobs < 1 || opts.Jobs > maxJobs {
		return usageErrf("--jobs must be between 1 and %d, got %d", maxJobs, opts.Jobs)
	}
	w, err := openWorkspace(opts.Token, opts.Anon)
	if err != nil {
		return err
	}
	w.s.verbose = opts.Verbose
	w.s.jobs = opts.Jobs

	idx, err := w.ensureFresh(ctx, opts.Subject)
	if err != nil {
		return err
	}
	subj, err := w.resolveSubject(idx, opts.Subject)
	if err != nil {
		return err
	}
	uses, usedBy, within := subjectLinks(idx, subj)

	out := w.s.out
	nUses, nUsedBy, nUnresolved := 0, 0, 0
	if direction != directionUsedBy {
		// Links inside a group are its projects using each other.
		for _, l := range append(within, uses...) {
			fmt.Fprintf(out, "related %s %s %s %s\n", l.From, l.Kind, l.To, l.Evidence)
			nUses++
		}
		for _, u := range idx.Unresolved {
			if subj.contains(u.From) {
				fmt.Fprintf(out, "unresolved %s %s %s\n", u.From, u.Ref, u.Evidence)
				nUnresolved++
			}
		}
	}
	if direction != directionUses {
		for _, l := range usedBy {
			fmt.Fprintf(out, "related-by %s %s %s %s\n", l.To, l.Kind, l.From, l.Evidence)
			nUsedBy++
		}
	}
	fmt.Fprintf(out, "summary uses=%d used-by=%d unresolved=%d\n", nUses, nUsedBy, nUnresolved)
	return nil
}
