package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gitty/internal/contract"
)

// subject is what `gitty sync --related` starts from: one
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
	return subject{}, usageErrf("no project or group %q is known in this workspace", arg)
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

// Directions a relationship can be asked about.
const (
	directionUses   = "uses"
	directionUsedBy = "used-by"
	directionBoth   = "both"
)

// parseDirection validates a direction as written after --related=.
func parseDirection(v string) (string, error) {
	switch v {
	case "", directionBoth:
		return directionBoth, nil
	case directionUses, directionUsedBy:
		return v, nil
	}
	return "", usageErrf("invalid --related=%q: choose %s, %s or %s", v, directionUses, directionUsedBy, directionBoth)
}

// maxSubjectLookups bounds how many of a subject's unsettled references one
// run asks the instance about.
const maxSubjectLookups = 20

// describeRoot names a workspace's root for a person.
func describeRoot(rootPath string) string {
	if rootPath == "" {
		return "every group in this workspace"
	}
	return "'" + rootPath + "'"
}
