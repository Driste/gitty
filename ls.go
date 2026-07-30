package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"gitlab.com/gitlab-org/api/client-go"
)

// lsOptions bundles the ls command's flags.
type lsOptions struct {
	Path   string
	Token  string
	Anon   bool
	Nested bool
	Format string
}

// lsProject is one remote project and whether it is already checked out.
type lsProject struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
}

// lsGroup is one remote group with the projects directly inside it.
type lsGroup struct {
	Path     string      `json:"path"`
	Projects []lsProject `json:"projects"`
}

// lsReport is the machine-readable form of an ls run.
type lsReport struct {
	Target  string    `json:"target"`
	Nested  bool      `json:"nested"`
	Groups  []lsGroup `json:"groups"`
	Summary lsSummary `json:"summary"`
}

type lsSummary struct {
	Groups   int `json:"groups"`
	Projects int `json:"projects"`
	New      int `json:"new"`
	Present  int `json:"present"`
}

// buildLsReport assembles the remote inventory, marking each project by
// whether a usable checkout already exists locally.
func buildLsReport(s *syncer, target string, nested bool) (lsReport, error) {
	groups, err := s.src.Subgroups(target, nested)
	if err != nil {
		return lsReport{}, fmt.Errorf("listing subgroups for %s: %w", target, err)
	}
	if root, err := s.src.Group(target); err == nil && root != nil {
		groups = append([]*gitlab.Group{root}, groups...)
	}

	projects, err := s.src.Projects(target, nested)
	if err != nil {
		return lsReport{}, fmt.Errorf("listing projects for %s: %w", target, err)
	}

	byGroup := map[string][]lsProject{}
	report := lsReport{Target: target, Nested: nested}
	for _, p := range projects {
		present := false
		rel := getLocalRelPath(p.PathWithNamespace, s.cfg.RootPath)
		if isWithinWorkspace(rel) && classifyDest(filepath.Join(".", rel)) == destRepo {
			present = true
		}
		parent := p.PathWithNamespace
		if i := strings.LastIndex(parent, "/"); i != -1 {
			parent = parent[:i]
		}
		byGroup[parent] = append(byGroup[parent], lsProject{Path: p.PathWithNamespace, Present: present})
		report.Summary.Projects++
		if present {
			report.Summary.Present++
		} else {
			report.Summary.New++
		}
	}

	// Every known group appears, including those whose project list is empty,
	// so the tree mirrors the remote namespace rather than only its populated
	// parts. A project in an unlisted namespace still gets a group entry.
	seen := map[string]bool{}
	var paths []string
	for _, g := range groups {
		if !seen[g.FullPath] {
			seen[g.FullPath] = true
			paths = append(paths, g.FullPath)
		}
	}
	for parent := range byGroup {
		if !seen[parent] {
			seen[parent] = true
			paths = append(paths, parent)
		}
	}
	sort.Strings(paths)

	for _, path := range paths {
		ps := byGroup[path]
		sort.Slice(ps, func(i, j int) bool { return ps[i].Path < ps[j].Path })
		report.Groups = append(report.Groups, lsGroup{Path: path, Projects: ps})
	}
	report.Summary.Groups = len(report.Groups)
	return report, nil
}

// writeLsText emits the greppable default form: one event line per group and
// per project, closing with the standard summary line.
func writeLsText(s *syncer, r lsReport) {
	for _, g := range r.Groups {
		s.event("group", g.Path, fmt.Sprintf("projects=%d", len(g.Projects)))
		for _, p := range g.Projects {
			state := "new"
			if p.Present {
				state = "present"
			}
			s.event("project", p.Path, state)
		}
	}
	fmt.Fprintf(s.out, "summary groups=%d projects=%d new=%d present=%d\n",
		r.Summary.Groups, r.Summary.Projects, r.Summary.New, r.Summary.Present)
}

// writeLsTree emits an indented namespace tree for human reading: each group
// carries its project count, and each project is marked '+' (would be cloned)
// or '=' (already present).
func writeLsTree(s *syncer, r lsReport) {
	depthOf := func(path string) int {
		rel := getLocalRelPath(path, r.Target)
		if rel == "" || rel == path {
			return 0
		}
		return strings.Count(rel, "/") + 1
	}
	for _, g := range r.Groups {
		indent := strings.Repeat("  ", depthOf(g.Path))
		fmt.Fprintf(s.out, "%s%s (%d %s)\n", indent, lastSegment(g.Path), len(g.Projects), plural(len(g.Projects), "project"))
		for _, p := range g.Projects {
			mark := "+"
			if p.Present {
				mark = "="
			}
			fmt.Fprintf(s.out, "%s  %s %s\n", indent, mark, lastSegment(p.Path))
		}
	}
	fmt.Fprintf(s.out, "\n%d %s, %d to clone, %d present\n",
		r.Summary.Projects, plural(r.Summary.Projects, "project"), r.Summary.New, r.Summary.Present)
}

func lastSegment(path string) string {
	if i := strings.LastIndex(path, "/"); i != -1 {
		return path[i+1:]
	}
	return path
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// runLs prints the remote group/project inventory for a target, marking which
// projects a sync would clone. It never invokes git and never writes to the
// workspace.
func runLs(ctx context.Context, opts lsOptions) error {
	switch opts.Format {
	case "text", "tree", "json":
	default:
		return usageErrf("--format must be text, tree, or json, got %q", opts.Format)
	}

	s, target, err := setupWorkspace(opts.Path, opts.Token, opts.Anon)
	if err != nil {
		return err
	}
	s.nested = opts.Nested

	if s.cred.token == "" {
		s.diagf("Running anonymously (--anon): only public groups and repositories are visible.")
	}
	s.diagf("Listing '%s' (Nested: %t)...", target, opts.Nested)

	report, err := buildLsReport(s, target, opts.Nested)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return errInterrupted
	}

	switch opts.Format {
	case "json":
		enc := json.NewEncoder(s.out)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(report); err != nil {
			return fmt.Errorf("rendering ls report: %w", err)
		}
	case "tree":
		writeLsTree(s, report)
	default:
		writeLsText(s, report)
	}
	return nil
}
