package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gitty/internal/contract"
)

// A briefing is the index seen from one project (or group): what it uses,
// what uses it, where each of those is on this machine, and the file and
// line that says so. It is what `gitty context` prints and what the MCP
// plugin hands an agent — built here once, rendered as Markdown or JSON.

// Caps on a briefing's lists. A briefing is read by an agent with a budget;
// a project with four hundred dependents does not need them all spelled out
// to know that it has four hundred, and `gitty related` lists every one.
const (
	maxBriefingUses    = 60
	maxBriefingUsedBy  = 30
	maxBriefingMembers = 100
	maxBriefingWithin  = 100
)

// buildBriefing assembles the briefing for a subject from an up-to-date
// index. direction limits which of the two lists is filled; the totals are
// always given, so a reader of one side knows the other exists.
func (w *workspace) buildBriefing(idx *contract.Index, subj subject, direction string) (*contract.Briefing, error) {
	b := &contract.Briefing{
		Schema:    contract.Schema,
		Instance:  idx.Instance,
		Workspace: w.Root,
		Uses:      []contract.Related{},
		UsedBy:    []contract.Related{},
		IndexedAt: idx.GeneratedAt,
	}

	if subj.Group {
		b.Subject = contract.Subject{Kind: "group", Group: subj.Path}
		if dir, ok := w.groupDir(subj.Path); ok {
			b.Subject.Present = true
			b.Subject.Path = dir
			b.Subject.RelativePath = w.relToStart(dir)
		}
		var members []string
		for p := range idx.Projects {
			if subj.contains(p) && !w.s.cfg.Excluded(p) {
				members = append(members, p)
			}
		}
		sort.Strings(members)
		for _, p := range members {
			if len(b.Subject.Projects) == maxBriefingMembers {
				b.Notes = append(b.Notes, fmt.Sprintf("%d of the group's %d projects are listed; 'gitty ls' lists them all", maxBriefingMembers, len(members)))
				break
			}
			meta := idx.Projects[p]
			m := contract.Member{Project: p, Description: meta.Description, Archived: meta.Archived}
			if meta.Local != nil {
				m.Present = true
				m.Path = w.absDir(meta.Local.Dir)
				m.RelativePath = w.relToStart(m.Path)
			}
			b.Subject.Projects = append(b.Subject.Projects, m)
		}
	} else {
		meta := idx.Projects[subj.Path]
		if meta == nil {
			if w.s.cfg.Excluded(subj.Path) {
				return nil, usageErrf("%s is left out by the workspace's exclude list", subj.Path)
			}
			return nil, usageErrf("%s is not in this workspace's index ('gitty index' refreshes it from the instance)", subj.Path)
		}
		b.Subject = contract.Subject{
			Kind:          "project",
			Project:       subj.Path,
			Description:   meta.Description,
			DefaultBranch: meta.DefaultBranch,
			Topics:        meta.Topics,
			Archived:      meta.Archived,
			WebURL:        meta.WebURL,
			LocalOnly:     meta.ID == 0,
		}
		if meta.Local != nil {
			b.Subject.Present = true
			b.Subject.Path = w.absDir(meta.Local.Dir)
			b.Subject.RelativePath = w.relToStart(b.Subject.Path)
			b.Subject.Docs = meta.Local.Docs
			b.Subject.Summary = meta.Local.Summary
		}
	}

	for p, meta := range idx.Projects {
		if w.s.cfg.Excluded(p) {
			continue
		}
		b.Coverage.Projects++
		if meta.Local != nil {
			b.Coverage.CheckedOut++
		}
	}

	uses, usedBy, within := subjectLinks(idx, subj)
	usesRel := w.relatedFrom(idx, uses, func(l contract.Link) string { return l.To })
	usedByRel := w.relatedFrom(idx, usedBy, func(l contract.Link) string { return l.From })
	// What a link points at is in the project it leads to: for the ones
	// the subject uses, that is the related project's own checkout.
	for i := range usesRel {
		if usesRel[i].Present {
			usesRel[i].Files = linkedFiles(usesRel[i].Path, usesRel[i].Links)
		}
	}
	b.UsesTotal, b.UsedByTotal = len(usesRel), len(usedByRel)

	if direction != directionUsedBy {
		if len(usesRel) > maxBriefingUses {
			b.Notes = append(b.Notes, fmt.Sprintf("%d of %d used projects are listed; 'gitty related %s --direction=uses' lists them all", maxBriefingUses, len(usesRel), subj))
			usesRel = usesRel[:maxBriefingUses]
		}
		b.Uses = usesRel
		if len(within) > maxBriefingWithin {
			b.Notes = append(b.Notes, fmt.Sprintf("%d of %d links within the group are listed; 'gitty related %s' lists them all", maxBriefingWithin, len(within), subj))
			within = within[:maxBriefingWithin]
		}
		b.Within = within
		for _, u := range idx.Unresolved {
			if subj.contains(u.From) {
				b.Unresolved = append(b.Unresolved, u)
			}
		}
	}
	if direction != directionUses {
		if len(usedByRel) > maxBriefingUsedBy {
			b.Notes = append(b.Notes, fmt.Sprintf("%d of %d projects using it are listed; 'gitty related %s --direction=used-by' lists them all", maxBriefingUsedBy, len(usedByRel), subj))
			usedByRel = usedByRel[:maxBriefingUsedBy]
		}
		b.UsedBy = usedByRel
	}
	return b, nil
}

// relatedFrom groups links by the project on their far side — the one that
// is not the subject — and describes each such project once, with every
// link that relates it.
func (w *workspace) relatedFrom(idx *contract.Index, links []contract.Link, other func(contract.Link) string) []contract.Related {
	byProject := map[string]*contract.Related{}
	var order []string
	for _, l := range links {
		p := other(l)
		rel := byProject[p]
		if rel == nil {
			rel = w.describe(idx, p)
			byProject[p] = rel
			order = append(order, p)
		}
		rel.Links = append(rel.Links, l)
	}
	// Projects related by something real come first; one that is only
	// mentioned in documentation comes after them.
	sort.Slice(order, func(i, j int) bool {
		a, b := onlyMentioned(byProject[order[i]].Links), onlyMentioned(byProject[order[j]].Links)
		if a != b {
			return b
		}
		return order[i] < order[j]
	})
	out := make([]contract.Related, 0, len(order))
	for _, p := range order {
		out = append(out, *byProject[p])
	}
	return out
}

// onlyMentioned reports whether every link is a mention in documentation.
func onlyMentioned(links []contract.Link) bool {
	for _, l := range links {
		if l.Kind != contract.KindMentions {
			return false
		}
	}
	return true
}

// describe fills in everything a briefing says about one related project
// except the links.
func (w *workspace) describe(idx *contract.Index, project string) *contract.Related {
	rel := &contract.Related{Project: project}
	meta := idx.Projects[project]
	if meta == nil {
		return rel
	}
	rel.Description = meta.Description
	rel.Archived = meta.Archived
	rel.WebURL = meta.WebURL
	if meta.Local != nil {
		rel.Present = true
		rel.Path = w.absDir(meta.Local.Dir)
		rel.RelativePath = w.relToStart(rel.Path)
		rel.Docs = meta.Local.Docs
		rel.Summary = meta.Local.Summary
		return rel
	}
	if _, ok := w.dirOf(project); ok && !w.s.cfg.Excluded(project) {
		rel.FetchCommand = "gitty fetch " + project
	}
	return rel
}

// maxLinkedFiles bounds how many pointed-at files one related project lists.
const maxLinkedFiles = 8

// linkedFiles returns the files in a checkout that the links to it point at
// and that are really there, relative to the checkout: the template a
// pipeline includes, the definition of a component it uses. Naming the file
// saves the reader a search; naming one that does not exist would cost them
// one, so each is checked.
func linkedFiles(dir string, links []contract.Link) []string {
	var out []string
	seen := map[string]bool{}
	add := func(rel string) {
		// Cleaned against a root, so no target can name anything outside
		// the checkout it is supposed to be in.
		rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
		if rel == "" || seen[rel] || len(out) == maxLinkedFiles {
			return
		}
		seen[rel] = true
		if fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err == nil && fi.Mode().IsRegular() {
			out = append(out, rel)
		}
	}
	for _, l := range links {
		if l.Target == "" {
			continue
		}
		for _, target := range strings.Split(l.Target, ", ") {
			if l.Kind == contract.KindCIComponent {
				// Where GitLab looks for a component named <target>.
				add("templates/" + target + ".yml")
				add("templates/" + target + "/template.yml")
				continue
			}
			add(target)
		}
	}
	return out
}

// groupDir returns the directory that stands for a group in this workspace,
// if there is one.
func (w *workspace) groupDir(group string) (string, bool) {
	root := w.s.cfg.RootPath
	var rel string
	switch {
	case group == root:
		rel = ""
	case root == "":
		rel = group
	case strings.HasPrefix(group, root+"/"):
		rel = strings.TrimPrefix(group, root+"/")
	default:
		return "", false // not in the workspace's part of the namespace
	}
	if !isWithinWorkspace(rel) {
		return "", false
	}
	dir := w.absDir(filepath.FromSlash(rel))
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", false
	}
	return dir, true
}

// absentUses lists the projects a briefing's subject uses that are not
// checked out and could be: what `--fetch` goes and gets. A project that is
// only mentioned in the subject's documentation is not among them: a link in
// a README is not a reason to clone a repository.
func absentUses(b *contract.Briefing) []string {
	var out []string
	for _, r := range b.Uses {
		if !r.Present && r.FetchCommand != "" && !onlyMentioned(r.Links) {
			out = append(out, r.Project)
		}
	}
	return out
}

// --- Markdown ---

// renderBriefing writes a briefing as Markdown. The headings are stable —
// "Uses", "Used by", "References that are not projects here", "Where to
// read next" — so the output can be searched as well as read, and each
// related project is one line: what it is, where it is, and why it matters.
func renderBriefing(sb *strings.Builder, b *contract.Briefing) {
	host := extractHost(b.Instance)
	s := b.Subject

	if s.Kind == "group" {
		name := s.Group
		if name == "" {
			name = "/"
		}
		present := 0
		for _, m := range s.Projects {
			if m.Present {
				present++
			}
		}
		fmt.Fprintf(sb, "# %s (group)\n", name)
		header := []string{host}
		if s.Present {
			header = append(header, "`"+s.RelativePath+"`")
		}
		header = append(header, fmt.Sprintf("%d %s, %d checked out", len(s.Projects), plural(len(s.Projects), "project"), present))
		fmt.Fprintf(sb, "%s\n", strings.Join(header, " · "))

		fmt.Fprintf(sb, "\n## Projects (%d)\n", len(s.Projects))
		for _, m := range s.Projects {
			fmt.Fprintf(sb, "- **%s**%s · %s\n", m.Project, dashed(m.Description), whereIs(m.Present, m.RelativePath, false, "", m.Archived))
		}
		if len(b.Within) > 0 {
			fmt.Fprintf(sb, "\n## Links within the group (%d)\n", len(b.Within))
			for _, l := range b.Within {
				what := l.Kind
				if l.Target != "" {
					what += " " + l.Target
				}
				fmt.Fprintf(sb, "- %s → %s · %s (%s)\n", l.From, l.To, what, l.Evidence)
			}
		}
	} else {
		fmt.Fprintf(sb, "# %s%s\n", s.Project, dashed(s.Description))
		header := []string{host}
		switch {
		case s.Present:
			header = append(header, "`"+s.RelativePath+"`")
		default:
			header = append(header, "*not checked out*")
		}
		if s.DefaultBranch != "" {
			header = append(header, s.DefaultBranch)
		}
		if len(s.Topics) > 0 {
			header = append(header, "topics: "+strings.Join(s.Topics, ", "))
		}
		if len(s.Docs) > 0 {
			header = append(header, "docs: "+strings.Join(s.Docs, ", "))
		}
		if s.Archived {
			header = append(header, "archived")
		}
		if s.LocalOnly {
			header = append(header, "no project upstream")
		}
		fmt.Fprintf(sb, "%s\n", strings.Join(header, " · "))
		if s.Summary != "" && s.Summary != s.Description {
			fmt.Fprintf(sb, "\n%s\n", s.Summary)
		}
	}

	group := s.Kind == "group"
	// A side is shown when it was asked for: it has entries, or there are
	// none to have. An empty side is said to be empty, in words — silence
	// would read the same as "not looked at".
	writeRelated := func(title, none string, total int, list []contract.Related) {
		if len(list) == 0 && total > 0 {
			return // the other direction was asked for
		}
		fmt.Fprintf(sb, "\n## %s (%d)\n", title, total)
		if total == 0 {
			fmt.Fprintf(sb, "- %s\n", none)
			return
		}
		for _, r := range list {
			var why []string
			for _, l := range r.Links {
				evidence := l.Evidence
				// For a group, say which of its projects the link touches.
				if group {
					member := l.From
					if title == "Used by" {
						member = l.To
						evidence += " → " + member
					} else {
						evidence = member + ": " + evidence
					}
				}
				what := l.Kind
				if l.Target != "" {
					what += " " + l.Target
				}
				why = append(why, fmt.Sprintf("%s (%s)", what, evidence))
			}
			fmt.Fprintf(sb, "- **%s**%s · %s · %s\n", r.Project, dashed(r.Description),
				whereIs(r.Present, r.RelativePath, r.Fetched, r.FetchCommand, r.Archived), strings.Join(why, ", "))
			if r.FetchError != "" {
				fmt.Fprintf(sb, "  - fetching it failed: %s\n", r.FetchError)
			}
		}
		if total > len(list) {
			fmt.Fprintf(sb, "- … and %d more\n", total-len(list))
		}
	}
	usesNone := "Nothing in its pipeline configuration or manifests refers to another project of the instance."
	usedByNone := "No checked-out project refers to it."
	if group {
		usesNone = "Nothing in the group refers to a project outside it."
		usedByNone = "No checked-out project outside the group refers to it."
	}
	if s.Kind == "project" && !s.Present {
		usesNone = "It is not checked out, so what it uses has not been read."
	}
	writeRelated("Uses", usesNone, b.UsesTotal, b.Uses)
	writeRelated("Used by", usedByNone, b.UsedByTotal, b.UsedBy)

	if len(b.Unresolved) > 0 {
		fmt.Fprintf(sb, "\n## References that are not projects here (%d)\n", len(b.Unresolved))
		for _, u := range b.Unresolved {
			evidence := u.Evidence
			if group {
				evidence = u.From + ": " + evidence
			}
			fmt.Fprintf(sb, "- %s (%s)\n", u.Ref, evidence)
		}
	}

	var next []string
	for _, list := range [][]contract.Related{b.Uses, b.UsedBy} {
		for _, r := range list {
			if !r.Present || len(r.Files)+len(r.Docs) == 0 {
				continue
			}
			// The files the links point at first, then the documentation.
			var files []string
			for _, f := range append(append([]string(nil), r.Files...), r.Docs...) {
				files = append(files, joinRel(r.RelativePath, f))
			}
			next = append(next, fmt.Sprintf("- %s: %s", r.Project, strings.Join(files, ", ")))
		}
	}
	if len(next) > 0 {
		fmt.Fprintf(sb, "\n## Where to read next\n%s\n", strings.Join(next, "\n"))
	}

	// What the answer rests on. Links come from reading checkouts, so the
	// part of the namespace that is not checked out is a blind spot for
	// "used by" — worth one line every time, because the list above looks
	// the same whether it is complete or not.
	notes := b.Notes
	usedByShown := len(b.UsedBy) > 0 || b.UsedByTotal == 0
	if c := b.Coverage; usedByShown && c.Projects > c.CheckedOut {
		notes = append([]string{fmt.Sprintf("Only checked-out projects are read for links: %d of %d here. \"Used by\" may be incomplete.",
			c.CheckedOut, c.Projects)}, notes...)
	}
	if len(notes) > 0 {
		fmt.Fprintf(sb, "\n## Notes\n")
		for _, n := range notes {
			fmt.Fprintf(sb, "- %s\n", n)
		}
	}
}

// dashed renders an optional description after a name.
func dashed(description string) string {
	if description == "" {
		return ""
	}
	return " — " + oneLine(description)
}

// oneLine keeps a description on the line it is part of.
func oneLine(s string) string {
	return truncateRunes(strings.Join(strings.Fields(s), " "), 160)
}

// whereIs says where a project is on this machine, or how to get it there.
func whereIs(present bool, relPath string, fetched bool, fetchCommand string, archived bool) string {
	var out string
	switch {
	case present && fetched:
		out = "`" + relPath + "` (fetched)"
	case present:
		out = "`" + relPath + "`"
	case fetchCommand != "":
		out = "*not checked out* (`" + fetchCommand + "`)"
	default:
		out = "*not checked out*"
	}
	if archived {
		out += " · archived"
	}
	return out
}

// joinRel joins a relative directory and a file name with forward slashes,
// keeping "./x" as "x".
func joinRel(dir, name string) string {
	if dir == "" || dir == "." {
		return name
	}
	return strings.TrimSuffix(dir, "/") + "/" + name
}
