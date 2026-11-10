package main

import (
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gitty/internal/contract"
)

// resolver turns a reference found in a file into the project of the
// instance it names — or says that it names none.
//
// A reference becomes a link only through here, and only when it lands on a
// project the index actually knows. That is the feature's guard against
// inventing relationships: a path that merely looks like a project is
// reported as unresolved, never presented as one.
type resolver struct {
	// hosts are the host names that are this instance: the configured one,
	// the workspace's clone_hosts, and any other the API advertises
	// repositories on (an instance reached under an external name whose API
	// hands out internal ones is both).
	hosts map[string]bool
	// basePath is the path an instance is served under when it does not sit
	// at its host's root ("gitlab" for https://example.com/gitlab).
	basePath string
	// projects maps a lower-cased namespace path to the path as GitLab
	// spells it. GitLab paths are unique regardless of case.
	projects map[string]string
	byID     map[int64]string
	// moved maps a lower-cased path a project used to have onto its path
	// now, for references that have not caught up.
	moved map[string]string
	// excluded says whether the workspace leaves a namespace path out. A
	// reference to an excluded project is not a link and not unresolved
	// either: the user asked gitty to leave it alone, so it is passed over.
	excluded func(string) bool

	tokenRe *regexp.Regexp
}

// reservedTop are first path segments GitLab keeps for itself. A URL on the
// instance whose path starts with one is a page, not a project.
var reservedTop = map[string]bool{
	"-": true, "admin": true, "api": true, "assets": true, "dashboard": true,
	"explore": true, "groups": true, "help": true, "jwt": true, "oauth": true,
	"profile": true, "projects": true, "public": true, "search": true,
	"snippets": true, "uploads": true, "users": true, "v2": true,
}

// newResolver builds a resolver over an index's projects.
func newResolver(idx *contract.Index, cfg *Config) *resolver {
	r := &resolver{
		hosts:    map[string]bool{},
		projects: make(map[string]string, len(idx.Projects)),
		byID:     make(map[int64]string, len(idx.Projects)),
		moved:    idx.Moved,
		excluded: cfg.Excluded,
	}
	addHost := func(raw string) {
		if h := extractHost(raw); h != "" {
			r.hosts[h] = true
		}
	}
	addHost(cfg.URL)
	if u, err := url.Parse(cfg.URL); err == nil {
		r.basePath = strings.ToLower(strings.Trim(u.Path, "/"))
	}
	for _, h := range cfg.CloneHosts {
		addHost(h)
	}
	for _, h := range idx.Hosts {
		r.hosts[strings.ToLower(h)] = true
	}
	for p, meta := range idx.Projects {
		if meta.ID == 0 || cfg.Excluded(p) {
			continue // unknown upstream, or left out: never a link target
		}
		r.projects[strings.ToLower(p)] = p
		r.byID[meta.ID] = p
	}

	// One expression finds every token on a line that names this instance:
	// an optional scheme and userinfo, one of the hosts, an optional port,
	// then "/" (URL, Go module path) or ":" (scp-like) and the path. The
	// leading group stands in for a lookbehind, so "notgitlab.example.com"
	// does not match "gitlab.example.com".
	hosts := make([]string, 0, len(r.hosts))
	for h := range r.hosts {
		hosts = append(hosts, regexp.QuoteMeta(h))
	}
	sort.Slice(hosts, func(i, j int) bool { return len(hosts[i]) > len(hosts[j]) })
	if len(hosts) > 0 {
		r.tokenRe = regexp.MustCompile(`(?i)(?:^|[^a-z0-9.\-])(?:[a-z][a-z0-9+.\-]*://)?(?:[a-z0-9._~%!$&+\-]+(?::[^@\s/]*)?@)?(?:` +
			strings.Join(hosts, "|") + `)(?::[0-9]+)?[/:]([a-z0-9_.~%+/@#?=&\-]+)`)
	}
	return r
}

// ref is what a candidate reference turned out to be.
type ref struct {
	// Project is the namespace path of the project the candidate names;
	// empty when it names none the index knows.
	Project string
	// Path is the candidate reduced to a path on the instance. For an
	// unresolved candidate it is what gets reported.
	Path string
	// OnInstance says the candidate points at this instance at all. A
	// candidate that does not is somebody else's and is ignored.
	OnInstance bool
}

// resolved reports whether the candidate landed on a known project.
func (x ref) resolved() bool { return x.Project != "" }

// unresolved reports whether the candidate points at this instance, looks
// like a project path, and matches nothing — worth telling the user about.
func (x ref) unresolved() bool {
	return x.OnInstance && x.Project == "" && strings.Contains(x.Path, "/")
}

// lookup finds the project a path on the instance belongs to: the longest
// known project path that is the path itself or a prefix of it at a "/"
// boundary. "acme/lib/v2/internal" belongs to "acme/lib"; "acme/lib-tools"
// does not.
func (r *resolver) lookup(p string) (string, bool) {
	p = strings.ToLower(strings.Trim(p, "/"))
	for p != "" {
		if canonical, ok := r.at(p); ok {
			return canonical, true
		}
		i := strings.LastIndex(p, "/")
		if i < 0 {
			break
		}
		p = p[:i]
	}
	return "", false
}

// exact finds the project with exactly this path.
func (r *resolver) exact(p string) (string, bool) {
	return r.at(strings.ToLower(strings.Trim(p, "/")))
}

// at finds the project at a lower-cased path: the one that has it, or the
// one that used to and is known to have moved.
func (r *resolver) at(p string) (string, bool) {
	if canonical, ok := r.projects[p]; ok {
		return canonical, true
	}
	if now, ok := r.moved[p]; ok {
		canonical, ok := r.projects[strings.ToLower(now)]
		return canonical, ok
	}
	return "", false
}

// knows reports whether a namespace path is a project of the instance.
func (r *resolver) knows(p string) bool {
	_, ok := r.exact(p)
	return ok
}

// leftOut reports whether a path on the instance is, or lies beneath,
// something the workspace excludes.
func (r *resolver) leftOut(p string) bool {
	if r.excluded == nil {
		return false
	}
	for p = strings.Trim(p, "/"); p != ""; {
		if r.excluded(p) {
			return true
		}
		i := strings.LastIndex(p, "/")
		if i < 0 {
			break
		}
		p = p[:i]
	}
	return false
}

// tokens returns the instance paths named on a line: for every occurrence of
// one of the instance's hosts, what follows it.
func (r *resolver) tokens(line string) []string {
	if r.tokenRe == nil {
		return nil
	}
	var out []string
	for _, m := range r.tokenRe.FindAllStringSubmatch(line, -1) {
		out = append(out, m[1])
	}
	return out
}

// resolvePath resolves a bare project path, as written under `project:` in a
// CI file. It is on the instance by construction and must match a project
// exactly: there is nothing after a project path in that position.
func (r *resolver) resolvePath(candidate string) ref {
	p := cleanProjectPath(candidate)
	if p == "" || r.leftOut(p) {
		return ref{}
	}
	if canonical, ok := r.exact(p); ok {
		return ref{Project: canonical, Path: canonical, OnInstance: true}
	}
	return ref{Path: p, OnInstance: true}
}

// resolveToken resolves what followed the instance's host in a URL, a Go
// module path or an scp-like remote: the path part of
//
//	https://host/acme/lib.git            git@host:acme/lib.git
//	host/acme/lib/v2                     https://host/acme/lib/-/raw/main/x.yml
//	https://host/api/v4/projects/1201/packages/npm/
//
// The project is the longest known one the path starts with.
func (r *resolver) resolveToken(token string) ref {
	p := r.instancePath(token)
	if p == "" {
		return ref{}
	}
	// The API addresses a project by id or by its URL-encoded path; either
	// names the project as surely as its web URL does.
	if rest, ok := strings.CutPrefix(strings.ToLower(p), "api/v4/projects/"); ok {
		rest = p[len(p)-len(rest):]
		first, _, _ := strings.Cut(rest, "/")
		if id, err := strconv.ParseInt(first, 10, 64); err == nil {
			if canonical, ok := r.byID[id]; ok {
				return ref{Project: canonical, Path: canonical, OnInstance: true}
			}
			return ref{} // an id says nothing a reader could act on
		}
		p = rest
	}
	first, _, _ := strings.Cut(p, "/")
	if reservedTop[strings.ToLower(first)] || r.leftOut(p) {
		return ref{}
	}
	if canonical, ok := r.lookup(p); ok {
		return ref{Project: canonical, Path: canonical, OnInstance: true}
	}
	return ref{Path: p, OnInstance: true}
}

// resolveURL resolves a candidate that carries its own host: a full URL, an
// scp-like remote, or host/path. A host that is not this instance's makes it
// somebody else's.
func (r *resolver) resolveURL(candidate string) ref {
	candidate = strings.TrimSpace(candidate)
	for _, tok := range r.tokens(candidate) {
		return r.resolveToken(tok)
	}
	return ref{}
}

// resolveComponent resolves a CI/CD component reference,
// <host>/<project path>/<component name>[@<version>]: the project is
// everything between the host and the component's name.
func (r *resolver) resolveComponent(candidate string) ref {
	candidate = strings.TrimSpace(candidate)
	if i := strings.Index(candidate, "@"); i >= 0 {
		candidate = candidate[:i]
	}
	toks := r.tokens(candidate)
	if len(toks) == 0 {
		return ref{}
	}
	p := r.instancePath(toks[0])
	i := strings.LastIndex(p, "/")
	if i < 0 || r.leftOut(p) {
		return ref{}
	}
	if canonical, ok := r.exact(p[:i]); ok {
		return ref{Project: canonical, Path: canonical, OnInstance: true}
	}
	// Not where the format says it should be; a nested component directory
	// is the likeliest reason, and the longest match still names the project.
	if canonical, ok := r.lookup(p); ok {
		return ref{Project: canonical, Path: canonical, OnInstance: true}
	}
	return ref{Path: p[:i], OnInstance: true}
}

// instancePath reduces what followed the host to a clean path on the
// instance: percent-decoded, without the instance's own base path, and cut
// where a project path can no longer continue — at "/-/" (GitLab's separator
// before a project's pages), at ".git", at a version, query or fragment.
func (r *resolver) instancePath(token string) string {
	p := token
	// "@", "#" and "?" cannot occur in a namespace path: what follows is a
	// version (lib.git@v1.2), a fragment (#egg=lib) or a query (?ref=main).
	if i := strings.IndexAny(p, "@#?"); i >= 0 {
		p = p[:i]
	}
	if decoded, err := url.PathUnescape(p); err == nil {
		p = decoded
	}
	p = strings.TrimLeft(p, "/:")
	if r.basePath != "" {
		if rest, ok := cutPrefixFold(p, r.basePath+"/"); ok {
			p = rest
		}
	}
	if i := strings.Index(p, "/-/"); i >= 0 {
		p = p[:i]
	}
	// Terraform writes host/group/project.git//modules/x, and a bare
	// host/group/project//modules/x.
	if i := strings.Index(p, "//"); i >= 0 {
		p = p[:i]
	}
	if i := indexGitSuffix(p); i >= 0 {
		p = p[:i]
	}
	return strings.TrimRight(p, "/.:")
}

// indexGitSuffix returns where a ".git" that ends a repository path starts:
// one followed by the end of the string or by "/" — not the ".git" inside
// "acme/.github" or "acme/lib.gitignore".
func indexGitSuffix(p string) int {
	for from := 0; ; {
		i := strings.Index(p[from:], ".git")
		if i < 0 {
			return -1
		}
		i += from
		end := i + len(".git")
		if end == len(p) || p[end] == '/' {
			return i
		}
		from = end
	}
}

// cutPrefixFold is strings.CutPrefix, ignoring case.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

// cleanProjectPath normalises a bare project path: no surrounding slashes or
// ".git", and nothing at all if it is not shaped like a path — a value that
// still holds an unexpanded variable, or spaces, names nothing.
func cleanProjectPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".git")
	if p == "" || strings.ContainsAny(p, " \t$\"'{}()*?[]\\") {
		return ""
	}
	return p
}
