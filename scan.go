package main

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gitty/internal/contract"
)

// Scanning: reading a checkout for references to other projects of the
// instance.
//
// It is deliberately textual. gitty reads the files git tracks, picks the few
// whose names say they declare dependencies or pipeline configuration, and
// looks at their lines. It does not evaluate build files, expand version
// ranges, run a package manager, or parse YAML and TOML in general — so it
// needs nothing installed, cannot be made to execute anything, and every
// link it reports points at a file and a line a person can open and check.
// What it cannot see that way, it does not claim.

// maxScanFileSize bounds a single file. A manifest or pipeline file larger
// than this is generated, vendored or not what its name suggests.
const maxScanFileSize = 1 << 20

// maxDocs bounds how many documentation files a project is recorded with.
const maxDocs = 6

// maxSummaryRunes bounds a project's README summary.
const maxSummaryRunes = 300

// scanOutcome is what one checkout yielded.
type scanOutcome struct {
	Links      []contract.Link
	Unresolved []contract.Unresolved
	Docs       []string
	Summary    string
	Files      int // files whose lines were read
}

// fileClass says which extractors read a tracked file.
type fileClass int

const (
	classNone     fileClass = iota
	classCI                 // GitLab CI configuration
	classManifest           // a package or build manifest
)

// manifestNames are the manifests recognised by exact file name.
var manifestNames = map[string]bool{
	"go.mod": true, "go.work": true,
	"package.json": true, ".npmrc": true, ".yarnrc.yml": true,
	"pyproject.toml": true, "setup.py": true, "setup.cfg": true, "Pipfile": true,
	"Gemfile": true, "Cargo.toml": true, "composer.json": true,
	"pom.xml": true, "build.gradle": true, "build.gradle.kts": true,
	"settings.gradle": true, "settings.gradle.kts": true,
	"Chart.yaml": true,
}

// manifestGlobs are the manifests recognised by pattern.
var manifestGlobs = []string{"requirements*.txt", "*.tf", "*.gemspec"}

// skippedDirs are directories whose contents are somebody else's code, or
// fixtures: a reference inside them is not this project's dependency.
var skippedDirs = map[string]bool{
	"node_modules": true, "vendor": true, "third_party": true,
	"testdata": true, ".terraform": true,
}

// classifyFile decides, from a tracked file's path alone, whether it is read.
func classifyFile(name string) fileClass {
	dir, base := path.Split(name)
	for _, seg := range strings.Split(strings.Trim(dir, "/"), "/") {
		if skippedDirs[seg] {
			return classNone
		}
	}
	isYAML := strings.HasSuffix(base, ".yml") || strings.HasSuffix(base, ".yaml")
	switch {
	case base == ".gitlab-ci.yml" || base == ".gitlab-ci.yaml":
		return classCI
	case isYAML && strings.Contains(base, ".gitlab-ci."):
		return classCI // build.gitlab-ci.yml, the convention for split pipelines
	case isYAML && strings.HasPrefix(name, ".gitlab/ci/"):
		return classCI
	case manifestNames[base]:
		return classManifest
	}
	for _, g := range manifestGlobs {
		if ok, _ := path.Match(g, base); ok {
			return classManifest
		}
	}
	return classNone
}

// scanner reads checkouts against one resolver.
type scanner struct {
	git      gitRunner
	r        *resolver
	instance string // the instance URL, for the CI variables that name it
}

// scan reads the checkout of project in dir. With links false only the
// project's documentation is gathered: an archived project is still worth
// describing as someone's dependency, but is not asked what it depends on.
func (sc *scanner) scan(ctx context.Context, dir, project string, links bool) (scanOutcome, error) {
	out, err := sc.git(ctx, dir, nil, "ls-files", "-z")
	if err != nil {
		return scanOutcome{}, fmt.Errorf("listing tracked files: %w", err)
	}
	names := strings.Split(string(out), "\x00")
	sort.Strings(names)

	c := newCollector(project, sc.r)
	vars := ciVariables(project, sc.instance)
	var rootFiles []string
	var result scanOutcome
	for _, name := range names {
		if name == "" {
			continue
		}
		if ctx.Err() != nil {
			return scanOutcome{}, ctx.Err()
		}
		if !strings.Contains(name, "/") {
			rootFiles = append(rootFiles, name)
		}
		if !links {
			continue
		}
		class := classifyFile(name)
		if class == classNone {
			continue
		}
		lines, ok := readLines(filepath.Join(dir, filepath.FromSlash(name)))
		if !ok {
			continue
		}
		result.Files++
		c.file = name
		switch class {
		case classCI:
			c.scanCI(lines, vars)
		case classManifest:
			c.scanTokens(lines, nil)
		}
	}

	result.Links, result.Unresolved = c.result()
	result.Docs = pickDocs(rootFiles)
	result.Summary = readmeSummary(dir, rootFiles)
	return result, nil
}

// readLines reads a tracked file's lines, or reports that it is not one to
// read: not a regular file (a symlink is followed by nobody here), too
// large, or binary.
func readLines(file string) ([]string, bool) {
	fi, err := os.Lstat(file)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxScanFileSize {
		return nil, false
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, false
	}
	head := data
	if len(head) > 8000 {
		head = head[:8000]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return nil, false
	}
	return strings.Split(string(data), "\n"), true
}

// --- collecting ---

// collector gathers one project's links, keeping the first evidence for each
// (target, kind) and dropping references a project makes to itself.
type collector struct {
	from string
	r    *resolver
	file string

	links      map[string]contract.Link
	unresolved map[string]contract.Unresolved
	// claimed records, per file line, the targets a specific extractor has
	// already accounted for, so the general URL scan does not report the
	// same reference a second time under a vaguer kind.
	claimed map[string]bool
}

func newCollector(from string, r *resolver) *collector {
	return &collector{
		from:       from,
		r:          r,
		links:      map[string]contract.Link{},
		unresolved: map[string]contract.Unresolved{},
		claimed:    map[string]bool{},
	}
}

func (c *collector) claimKey(line int, target string) string {
	return fmt.Sprintf("%s:%d:%s", c.file, line, strings.ToLower(target))
}

// add records what a candidate turned out to be: a link, an unresolved
// reference, or nothing.
func (c *collector) add(kind string, x ref, line int) {
	evidence := fmt.Sprintf("%s:%d", c.file, line)
	switch {
	case x.resolved():
		c.claimed[c.claimKey(line, x.Project)] = true
		if x.Project == c.from {
			return
		}
		key := x.Project + "\x00" + kind
		if _, seen := c.links[key]; !seen {
			c.links[key] = contract.Link{From: c.from, To: x.Project, Kind: kind, Evidence: evidence}
		}
	case x.unresolved():
		c.claimed[c.claimKey(line, x.Path)] = true
		key := strings.ToLower(x.Path)
		if _, seen := c.unresolved[key]; !seen {
			c.unresolved[key] = contract.Unresolved{From: c.from, Ref: x.Path, Kind: kind, Evidence: evidence}
		}
	}
}

// result returns everything collected, in a stable order.
func (c *collector) result() ([]contract.Link, []contract.Unresolved) {
	links := make([]contract.Link, 0, len(c.links))
	for _, l := range c.links {
		links = append(links, l)
	}
	sortLinks(links)
	unresolved := make([]contract.Unresolved, 0, len(c.unresolved))
	for _, u := range c.unresolved {
		unresolved = append(unresolved, u)
	}
	sortUnresolved(unresolved)
	return links, unresolved
}

func sortLinks(links []contract.Link) {
	sort.Slice(links, func(i, j int) bool {
		a, b := links[i], links[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		return a.Kind < b.Kind
	})
}

func sortUnresolved(us []contract.Unresolved) {
	sort.Slice(us, func(i, j int) bool {
		a, b := us[i], us[j]
		if a.From != b.From {
			return a.From < b.From
		}
		return a.Ref < b.Ref
	})
}

// --- general extractor: anything that names the instance ---

// scanTokens reports every place a line names the instance — a URL, an
// scp-like remote, a Go module path — as a dependency on the project that
// path belongs to. vars, when given, are expanded first.
func (c *collector) scanTokens(lines []string, vars map[string]string) {
	for i, line := range lines {
		c.scanTokensLine(expandVars(line, vars), i+1)
	}
}

func (c *collector) scanTokensLine(line string, lineNo int) {
	for _, tok := range c.r.tokens(line) {
		x := c.r.resolveToken(tok)
		target := x.Project
		if target == "" {
			target = x.Path
		}
		if target == "" || c.claimed[c.claimKey(lineNo, target)] {
			continue
		}
		c.add(contract.KindDepends, x, lineNo)
	}
}

// --- GitLab CI extractor ---

// ciFrame is one open mapping key in a CI file: a key whose value is the
// block of more-indented lines that follow it.
type ciFrame struct {
	indent int
	key    string
}

// scanCI reads a GitLab CI file for the places it reaches into other
// projects:
//
//	include:
//	  - project: acme/ci-templates          ci-include
//	    file: /templates/build.yml
//	  - component: host/acme/components/sast@1.0   ci-component
//	  - remote: https://host/acme/x/-/raw/main/t.yml   ci-include
//	deploy:
//	  trigger:
//	    project: acme/ops/deploy            ci-trigger
//	build:
//	  needs:
//	    - project: acme/lib                 depends (its artifacts)
//	      job: package
//
// It follows the file's structure by indentation alone — enough to know
// which block a `project:` key sits in, which is all that distinguishes an
// include from a trigger — and reads nothing inside script blocks as keys.
// Every line is also given to the general extractor, so a clone URL in a
// script is a dependency too.
func (c *collector) scanCI(lines []string, vars map[string]string) {
	var stack []ciFrame
	for i, raw := range lines {
		lineNo := i + 1
		line := strings.TrimRight(raw, "\r")
		body := strings.TrimLeft(line, " ")
		if body == "" || body[0] == '#' {
			continue
		}
		indent := len(line) - len(body)

		// A list item's dash sits at the item's indentation; what follows it
		// is the content, further in. A list may be indented exactly as far
		// as the key it belongs to, so a dash at a frame's own indentation
		// does not close that frame, while a key there does.
		dashIndent, item := indent, false
		for body == "-" || strings.HasPrefix(body, "- ") {
			item = true
			rest := strings.TrimLeft(body[1:], " ")
			indent += len(body) - len(rest)
			body = rest
		}
		for len(stack) > 0 {
			top := stack[len(stack)-1]
			if top.indent > dashIndent || (top.indent == dashIndent && !item) {
				stack = stack[:len(stack)-1]
				continue
			}
			break
		}
		if body == "" {
			continue
		}

		block := enclosingCIBlock(stack)
		key, value, isKey := splitYAMLKey(body)
		if !isKey {
			// A plain scalar in a list: under include it is a file to
			// include, by path or by URL.
			if block == "include" {
				c.add(contract.KindCIInclude, c.r.resolveURL(expandVars(cleanYAMLScalar(body), vars)), lineNo)
			}
			c.scanTokensLine(expandVars(line, vars), lineNo)
			continue
		}
		value = expandVars(cleanYAMLScalar(value), vars)
		if value == "" || strings.ContainsAny(value[:1], "|>&") {
			stack = append(stack, ciFrame{indent: indent, key: key})
			continue
		}

		switch key {
		case "project":
			switch block {
			case "include":
				c.add(contract.KindCIInclude, c.r.resolvePath(value), lineNo)
			case "trigger":
				c.add(contract.KindCITrigger, c.r.resolvePath(value), lineNo)
			case "needs":
				c.add(contract.KindDepends, c.r.resolvePath(value), lineNo)
			}
		case "component":
			if block == "include" {
				c.add(contract.KindCIComponent, c.r.resolveComponent(value), lineNo)
			}
		case "remote":
			if block == "include" {
				c.add(contract.KindCIInclude, c.r.resolveURL(value), lineNo)
			}
		case "trigger":
			// The short form: `trigger: group/project`.
			if block != "script" {
				c.add(contract.KindCITrigger, c.r.resolvePath(value), lineNo)
			}
		case "include":
			// The short form: `include: <one file or URL>`.
			if block != "script" {
				c.add(contract.KindCIInclude, c.r.resolveURL(value), lineNo)
			}
		}
		c.scanTokensLine(expandVars(line, vars), lineNo)
	}
}

// enclosingCIBlock names the block a line sits in, as far as this extractor
// cares: "include", "trigger" or "needs" for the nearest of those, "script"
// when a script or a variables block is nearer (nothing in there is
// configuration), "" otherwise.
func enclosingCIBlock(stack []ciFrame) string {
	for i := len(stack) - 1; i >= 0; i-- {
		switch stack[i].key {
		case "include", "trigger", "needs":
			return stack[i].key
		case "script", "before_script", "after_script", "variables":
			return "script"
		}
	}
	return ""
}

// splitYAMLKey splits "key: value" (or "key:") into its halves. YAML wants a
// space or the end of the line after the colon, which is what keeps a URL
// from being read as a key.
func splitYAMLKey(body string) (key, value string, ok bool) {
	for i := 0; i < len(body); i++ {
		if body[i] != ':' {
			continue
		}
		if i+1 == len(body) || body[i+1] == ' ' {
			key = strings.Trim(strings.TrimSpace(body[:i]), `"'`)
			if key == "" {
				return "", "", false
			}
			return key, strings.TrimSpace(body[i+1:]), true
		}
	}
	return "", "", false
}

// cleanYAMLScalar returns a scalar's text: the inside of a quoted one, or a
// plain one without a trailing comment.
func cleanYAMLScalar(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if q := v[0]; q == '"' || q == '\'' {
		if end := strings.IndexByte(v[1:], q); end >= 0 {
			return v[1 : 1+end]
		}
		return v[1:]
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// ciVariables are the predefined CI/CD variables whose value follows from
// where a project lives, so that
//
//	include:
//	  - project: $CI_PROJECT_NAMESPACE/ci-templates
//
// resolves for the project it is written in. Variables set anywhere else —
// in the file, in the project's settings — are unknown here, and a value
// still holding one is left unresolved rather than guessed.
func ciVariables(project, instance string) map[string]string {
	vars := map[string]string{
		"CI_PROJECT_PATH":           project,
		"CI_PROJECT_NAMESPACE":      parentPath(project),
		"CI_PROJECT_ROOT_NAMESPACE": strings.SplitN(project, "/", 2)[0],
		"CI_PROJECT_NAME":           lastSegment(project),
		"CI_SERVER_URL":             strings.TrimRight(instance, "/"),
		"CI_PROJECT_URL":            strings.TrimRight(instance, "/") + "/" + project,
	}
	if u, err := url.Parse(instance); err == nil && u.Host != "" {
		vars["CI_SERVER_HOST"] = u.Hostname()
		vars["CI_SERVER_FQDN"] = u.Host
	}
	return vars
}

var varRe = regexp.MustCompile(`\$(?:\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))`)

// expandVars replaces the variables it knows and leaves the rest as written.
func expandVars(s string, vars map[string]string) string {
	if len(vars) == 0 || !strings.Contains(s, "$") {
		return s
	}
	return varRe.ReplaceAllStringFunc(s, func(m string) string {
		name := strings.Trim(m, "${}")
		if v, ok := vars[name]; ok {
			return v
		}
		return m
	})
}

// --- documentation ---

// docNoise are root-level documents that say nothing about how a project
// works or is used.
var docNoise = []string{
	"CHANGELOG", "CHANGES", "HISTORY", "LICENSE", "LICENCE", "COPYING",
	"CODE_OF_CONDUCT", "SECURITY", "AUTHORS", "CONTRIBUTORS", "NOTICE",
	"MAINTAINERS", "CODEOWNERS",
}

// pickDocs chooses, from a checkout's root-level files, the documents worth
// pointing a reader at, most useful first: instructions written for agents,
// then design and contribution notes, then anything else, and the README
// last — it is the general introduction, read when nothing more specific
// answers the question.
func pickDocs(rootFiles []string) []string {
	rank := func(name string) int {
		upper := strings.ToUpper(name)
		stem := strings.TrimSuffix(upper, path.Ext(upper))
		for _, noise := range docNoise {
			if strings.HasPrefix(stem, noise) {
				return -1
			}
		}
		switch {
		case strings.HasPrefix(stem, "README"):
			return 3
		case !strings.HasSuffix(upper, ".MD"):
			return -1
		case strings.HasPrefix(stem, "AGENT"):
			return 0
		case strings.HasPrefix(stem, "ARCHITECTURE"), strings.HasPrefix(stem, "DESIGN"),
			strings.HasPrefix(stem, "CONTRIBUTING"), strings.HasPrefix(stem, "DEVELOPMENT"):
			return 1
		}
		return 2
	}
	type doc struct {
		name string
		rank int
	}
	var docs []doc
	for _, name := range rootFiles {
		if r := rank(name); r >= 0 {
			docs = append(docs, doc{name, r})
		}
	}
	sort.SliceStable(docs, func(i, j int) bool {
		if docs[i].rank != docs[j].rank {
			return docs[i].rank < docs[j].rank
		}
		return docs[i].name < docs[j].name
	})
	// When there are more than fit, the README is the one to keep.
	if len(docs) > maxDocs {
		kept := docs[:maxDocs-1]
		for _, d := range docs[maxDocs-1:] {
			if d.rank == 3 {
				kept = append(kept, d)
				break
			}
		}
		docs = kept
	}
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.name)
	}
	return out
}

// readmeSummary returns the opening paragraph of a checkout's README: the
// first run of prose after the title, badges and markup.
func readmeSummary(dir string, rootFiles []string) string {
	var readme string
	for _, name := range rootFiles {
		upper := strings.ToUpper(name)
		if upper == "README.MD" || (readme == "" && strings.HasPrefix(upper, "README")) {
			readme = name
		}
	}
	if readme == "" {
		return ""
	}
	lines, ok := readLines(filepath.Join(dir, readme))
	if !ok {
		return ""
	}
	var para []string
	inFence := false
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		// A line of = or - under text makes that text a heading, not prose.
		if len(para) > 0 && len(line) >= 2 && strings.Trim(line, "=-") == "" {
			para = nil
			continue
		}
		prose := line != "" && !isMarkupLine(line)
		if prose {
			para = append(para, line)
			continue
		}
		if len(para) > 0 {
			break
		}
	}
	return truncateRunes(strings.Join(para, " "), maxSummaryRunes)
}

// isMarkupLine reports whether a README line is structure rather than prose:
// a heading or its underline, a badge or image, HTML, a rule, a table row.
func isMarkupLine(line string) bool {
	switch line[0] {
	case '#', '<', '|', '>':
		return true
	}
	if strings.HasPrefix(line, "[![") || strings.HasPrefix(line, "![") {
		return true
	}
	return strings.Trim(line, "=-*_ ") == ""
}

// truncateRunes shortens s to at most max runes, on a word boundary where
// there is one, marking the cut.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)[:max]
	cut := string(runes)
	if i := strings.LastIndexByte(cut, ' '); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:") + "…"
}
