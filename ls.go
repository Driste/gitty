package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gitlab.com/gitlab-org/api/client-go"
)

// lsOptions bundles the ls command's flags.
type lsOptions struct {
	// Target is the positional argument (or --path), resolved like a shell
	// path against the workspace's current context.
	Target string
	Token  string
	Anon   bool
	Nested bool
	// IncludeArchived lists archived projects and groups too; they are left
	// out by default, as sync leaves them out.
	IncludeArchived bool
	Format          string
	Color           string
}

// lsProject is one remote project and whether it is already checked out.
type lsProject struct {
	Path     string `json:"path"`
	Present  bool   `json:"present"`
	Archived bool   `json:"archived,omitempty"`
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
	Archived int `json:"archived,omitempty"`
}

// resolveLsTarget maps an ls argument onto a GitLab group path the way a shell
// resolves a directory argument:
//
//	"" or "."    the current context (the workspace's root_path)
//	"/"          the instance root, i.e. the top-level groups
//	"/a/b"       absolute, from the instance root
//	"a/b"        relative to the current context
//	".."         the parent group; going past the root lands on the root
//
// topLevel reports that there is no single group to list, so the caller should
// list the instance's top-level groups instead — which is also what a bare
// `gitty ls` does in a freshly initialized workspace.
func resolveLsTarget(arg, rootPath string) (target string, topLevel bool) {
	arg = strings.TrimSpace(arg)
	switch arg {
	case "", ".":
		if rootPath == "" {
			return "", true
		}
		return rootPath, false
	case "/":
		return "", true
	}

	if strings.HasPrefix(arg, "/") {
		cleaned := strings.Trim(path.Clean(arg), "/")
		if cleaned == "" || cleaned == "." {
			return "", true
		}
		return cleaned, false
	}

	joined := path.Join(rootPath, arg)
	if joined == "" || joined == "." || joined == ".." || strings.HasPrefix(joined, "../") {
		return "", true
	}
	return joined, false
}

// buildLsReport assembles the remote inventory, marking each project by
// whether a usable checkout already exists locally.
func buildLsReport(ctx context.Context, s *syncer, target string, nested bool) (lsReport, error) {
	// The three listings are independent round trips; run them together.
	var (
		wg       sync.WaitGroup
		groups   []*gitlab.Group
		root     *gitlab.Group
		projects []*gitlab.Project
		gErr     error
		pErr     error
	)
	wg.Add(3)
	go func() { defer wg.Done(); groups, gErr = s.src.Subgroups(ctx, target, nested, s.includeArchived) }()
	go func() { defer wg.Done(); root, _ = s.src.Group(ctx, target) }()
	go func() { defer wg.Done(); projects, pErr = s.src.Projects(ctx, target, nested, s.includeArchived) }()
	wg.Wait()
	if gErr != nil {
		return lsReport{}, fmt.Errorf("listing subgroups for %s: %w", target, gErr)
	}
	if pErr != nil {
		return lsReport{}, fmt.Errorf("listing projects for %s: %w", target, pErr)
	}
	if root != nil {
		groups = append([]*gitlab.Group{root}, groups...)
	}
	return assembleReport(s, target, nested, groups, projects), nil
}

// buildTopLevelReport lists the instance's top-level groups — the equivalent
// of `ls /`. Their project counts are not fetched: that would be one API call
// per group, and the useful answer here is which namespaces exist.
func buildTopLevelReport(ctx context.Context, s *syncer) (lsReport, error) {
	groups, err := s.src.TopLevelGroups(ctx)
	if err != nil {
		return lsReport{}, fmt.Errorf("listing top-level groups: %w", err)
	}
	return assembleReport(s, "/", false, groups, nil), nil
}

func assembleReport(s *syncer, target string, nested bool, groups []*gitlab.Group, projects []*gitlab.Project) lsReport {
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
		byGroup[parent] = append(byGroup[parent], lsProject{Path: p.PathWithNamespace, Present: present, Archived: p.Archived})
		report.Summary.Projects++
		if p.Archived {
			report.Summary.Archived++
		}
		if present {
			report.Summary.Present++
		} else {
			report.Summary.New++
		}
	}

	// Every known group appears, including those with no projects, so the
	// tree mirrors the remote namespace rather than only its populated parts.
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

	for _, p := range paths {
		ps := byGroup[p]
		sort.Slice(ps, func(i, j int) bool { return ps[i].Path < ps[j].Path })
		report.Groups = append(report.Groups, lsGroup{Path: p, Projects: ps})
	}
	report.Summary.Groups = len(report.Groups)
	return report
}

// --- rendering ---

// lsNode is one group in the rendered tree.
type lsNode struct {
	name     string
	path     string
	projects []lsProject
	children []*lsNode
}

// buildLsTree arranges the report's flat group list into a forest, attaching
// each group to the nearest ancestor that is also in the listing.
func buildLsTree(r lsReport) []*lsNode {
	nodes := make(map[string]*lsNode, len(r.Groups))
	paths := make([]string, 0, len(r.Groups))
	for _, g := range r.Groups {
		nodes[g.Path] = &lsNode{name: lastSegment(g.Path), path: g.Path, projects: g.Projects}
		paths = append(paths, g.Path)
	}
	sort.Strings(paths)

	var roots []*lsNode
	for _, p := range paths {
		node := nodes[p]
		attached := false
		for parent := parentPath(p); parent != ""; parent = parentPath(parent) {
			if pn, ok := nodes[parent]; ok {
				pn.children = append(pn.children, node)
				attached = true
				break
			}
		}
		if !attached {
			roots = append(roots, node)
		}
	}
	return roots
}

func parentPath(p string) string {
	if i := strings.LastIndex(p, "/"); i != -1 {
		return p[:i]
	}
	return ""
}

func lastSegment(p string) string {
	if i := strings.LastIndex(p, "/"); i != -1 {
		return p[i+1:]
	}
	return p
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// writeLsTree renders an indented tree: subgroups nest under their parent and
// each group's projects hang beneath it, marked by whether they are already
// checked out.
func writeLsTree(w io.Writer, r lsReport, p palette) {
	for _, root := range buildLsTree(r) {
		writeGroupLine(w, root, "", p)
		writeChildren(w, root, "", p)
	}

	fmt.Fprintln(w)
	summary := fmt.Sprintf("%d %s", r.Summary.Groups, plural(r.Summary.Groups, "group"))
	if r.Summary.Projects > 0 {
		summary += fmt.Sprintf(", %d %s: %s, %s",
			r.Summary.Projects, plural(r.Summary.Projects, "project"),
			p.paint(p.present, fmt.Sprintf("%d present", r.Summary.Present)),
			p.paint(p.missing, fmt.Sprintf("%d to clone", r.Summary.New)))
		if r.Summary.Archived > 0 {
			summary += p.paint(p.dim, fmt.Sprintf(" (%d archived)", r.Summary.Archived))
		}
	}
	fmt.Fprintln(w, summary)
}

func writeGroupLine(w io.Writer, n *lsNode, prefix string, p palette) {
	line := p.paint(p.group, n.name+"/")
	if len(n.projects) > 0 {
		line += " " + p.paint(p.dim, fmt.Sprintf("(%d %s)", len(n.projects), plural(len(n.projects), "project")))
	}
	fmt.Fprintln(w, prefix+line)
}

// writeChildren renders a group's subgroups and then its projects, drawing the
// connector for each and extending the prefix for anything nested below.
func writeChildren(w io.Writer, n *lsNode, prefix string, p palette) {
	total := len(n.children) + len(n.projects)
	i := 0

	for _, child := range n.children {
		branch, extend := connectors(i, total)
		writeGroupLine(w, child, prefix+branch, p)
		writeChildren(w, child, prefix+extend, p)
		i++
	}
	for _, proj := range n.projects {
		branch, _ := connectors(i, total)
		state, style := "new", p.missing
		if proj.Present {
			state, style = "present", p.present
		}
		if proj.Archived {
			state += ", archived"
		}
		fmt.Fprintf(w, "%s%s  %s\n",
			prefix+branch, p.paint(style, lastSegment(proj.Path)), p.paint(p.dim, state))
		i++
	}
}

func connectors(i, total int) (branch, extend string) {
	if i == total-1 {
		return "└── ", "    "
	}
	return "├── ", "│   "
}

// writeLsText emits the greppable form: one event line per group and per
// project, closing with the standard summary line.
func writeLsText(s *syncer, r lsReport) {
	for _, g := range r.Groups {
		s.event("group", g.Path, fmt.Sprintf("projects=%d", len(g.Projects)))
		for _, proj := range g.Projects {
			state := "new"
			if proj.Present {
				state = "present"
			}
			if proj.Archived {
				s.event("project", proj.Path, state, "archived")
				continue
			}
			s.event("project", proj.Path, state)
		}
	}
	line := fmt.Sprintf("summary groups=%d projects=%d new=%d present=%d",
		r.Summary.Groups, r.Summary.Projects, r.Summary.New, r.Summary.Present)
	if r.Summary.Archived > 0 {
		line += fmt.Sprintf(" archived=%d", r.Summary.Archived)
	}
	fmt.Fprintln(s.out, line)
}

// runLs prints the remote group/project inventory for a target, marking which
// projects a sync would clone. It never invokes git and never writes to the
// workspace.
func runLs(ctx context.Context, opts lsOptions) error {
	format, err := resolveLsFormat(opts.Format, os.Stdout)
	if err != nil {
		return err
	}
	colorOn, err := resolveColor(opts.Color, os.Stdout)
	if err != nil {
		return err
	}

	// ls resolves its argument itself, so setupWorkspace is asked only for the
	// workspace and credentials, not for a target.
	s, err := newWorkspaceSyncer(opts.Token, opts.Anon)
	if err != nil {
		return err
	}
	s.nested = opts.Nested
	s.includeArchived = opts.IncludeArchived

	target, topLevel := resolveLsTarget(opts.Target, s.cfg.RootPath)

	if s.cred.token == "" {
		s.diagf("Running anonymously (--anon): only public groups and repositories are visible.")
	}
	if topLevel {
		s.diagf("Listing top-level groups on %s...", s.cfg.URL)
	} else {
		s.diagf("Listing '%s' (Nested: %t)...", target, opts.Nested)
	}

	var report lsReport
	if topLevel {
		report, err = buildTopLevelReport(ctx, s)
	} else {
		report, err = buildLsReport(ctx, s, target, opts.Nested)
	}
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return errInterrupted
	}

	switch format {
	case "json":
		enc := json.NewEncoder(s.out)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(report); err != nil {
			return fmt.Errorf("rendering ls report: %w", err)
		}
	case "tree":
		writeLsTree(s.out, report, newPalette(colorOn))
	default:
		writeLsText(s, report)
	}
	return nil
}
