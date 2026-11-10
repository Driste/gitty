package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/gitlab-org/api/client-go"

	"gitty/internal/contract"
)

func TestIndexRoundTrip(t *testing.T) {
	root := t.TempDir()
	cfg := &Config{URL: "https://gitlab.example.com", RootPath: "acme"}
	writeConfigAt(t, root, *cfg)

	if idx, why, err := loadIndex(root, cfg); idx != nil || why == "" || err != nil {
		t.Fatalf("no file: idx=%v why=%q err=%v; want nil, a reason, no error", idx, why, err)
	}

	idx := newIndex(cfg)
	idx.Hosts = []string{"git.internal.example"}
	idx.GeneratedAt = time.Date(2026, 11, 6, 10, 12, 3, 0, time.UTC)
	idx.Projects["acme/app"] = &contract.Project{
		ID: 1, Description: "The app", DefaultBranch: "main", Topics: []string{"go"},
		Local: &contract.Local{Dir: "app", Head: strings.Repeat("a", 40), Docs: []string{"README.md"}, Summary: "An app."},
	}
	idx.Projects["acme/lib"] = &contract.Project{ID: 2, Archived: true, ForkedFrom: "acme/upstream"}
	// Deliberately out of order: the file is written sorted.
	idx.Links = []contract.Link{
		{From: "acme/lib", To: "acme/app", Kind: contract.KindFork, Evidence: "api:forked_from_project"},
		{From: "acme/app", To: "acme/lib", Kind: contract.KindDepends, Evidence: "go.mod:12"},
	}
	idx.Unresolved = []contract.Unresolved{{From: "acme/app", Ref: "acme/old", Kind: contract.KindDepends, Evidence: "go.mod:14"}}
	if err := saveIndex(root, idx); err != nil {
		t.Fatal(err)
	}

	got, why, err := loadIndex(root, cfg)
	if err != nil || got == nil {
		t.Fatalf("loadIndex: idx=%v why=%q err=%v", got, why, err)
	}
	if got.Version != contract.IndexVersion || got.Instance != cfg.URL || got.Root != "acme" || !got.GeneratedAt.Equal(idx.GeneratedAt) {
		t.Errorf("header = %+v", got)
	}
	if app := got.Projects["acme/app"]; app == nil || app.Local == nil || app.Local.Dir != "app" || app.Local.Docs[0] != "README.md" || app.Topics[0] != "go" {
		t.Errorf("acme/app = %+v", app)
	}
	if lib := got.Projects["acme/lib"]; lib == nil || lib.Local != nil || !lib.Archived || lib.ForkedFrom != "acme/upstream" {
		t.Errorf("acme/lib = %+v", lib)
	}
	if len(got.Links) != 2 || got.Links[0].From != "acme/app" || got.Links[1].Kind != contract.KindFork {
		t.Errorf("links = %+v, want sorted by source", got.Links)
	}
	if len(got.Unresolved) != 1 || got.Unresolved[0].Ref != "acme/old" {
		t.Errorf("unresolved = %+v", got.Unresolved)
	}

	// A trailing slash or different case in the configured URL is still the
	// same instance; a different instance or root is a different workspace.
	if again, _, _ := loadIndex(root, &Config{URL: "https://GitLab.example.com/", RootPath: "acme"}); again == nil {
		t.Error("the same instance, spelled differently, should load")
	}
	for name, other := range map[string]*Config{
		"another instance": {URL: "https://gitlab.com", RootPath: "acme"},
		"another root":     {URL: cfg.URL, RootPath: "acme/sub"},
	} {
		if stale, why, err := loadIndex(root, other); stale != nil || why == "" || err != nil {
			t.Errorf("%s: idx=%v why=%q err=%v; want nil with a reason", name, stale, why, err)
		}
	}

	// The directory holds the config and the index, and nothing left over.
	entries, err := os.ReadDir(filepath.Join(root, ConfigDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf(".gitty holds %v, want only the config and the index", entries)
	}
}

func TestLoadIndexRejectsWhatItCannotTrust(t *testing.T) {
	cfg := &Config{URL: "https://gitlab.example.com"}
	tests := map[string]string{
		"another format version": `{"version": 99, "instance": "https://gitlab.example.com", "root": "", "projects": {}}`,
		"not JSON":               `{"version": 1, "inst`,
		"empty":                  ``,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeConfigAt(t, root, *cfg)
			if err := os.WriteFile(indexPath(root), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			idx, why, err := loadIndex(root, cfg)
			if idx != nil || why == "" || err != nil {
				t.Errorf("idx=%v why=%q err=%v; want nil, a reason to rebuild, no error", idx, why, err)
			}
		})
	}
}

// A write that cannot complete leaves the previous index readable and no
// debris; a stray temporary file from a killed write is ignored.
func TestSaveIndexIsAtomic(t *testing.T) {
	root := t.TempDir()
	cfg := &Config{URL: "https://gitlab.example.com"}
	writeConfigAt(t, root, *cfg)

	first := newIndex(cfg)
	first.Projects["acme/app"] = &contract.Project{ID: 1}
	if err := saveIndex(root, first); err != nil {
		t.Fatal(err)
	}
	before := readFileT(t, indexPath(root))

	// What a write killed half-way leaves behind.
	stray := filepath.Join(root, ConfigDir, indexFileName+".tmp-123")
	if err := os.WriteFile(stray, []byte(`{"version": 1, "proj`), 0o644); err != nil {
		t.Fatal(err)
	}
	if idx, _, err := loadIndex(root, cfg); err != nil || idx == nil || idx.Projects["acme/app"] == nil {
		t.Errorf("a stray temporary file disturbed loading: idx=%v err=%v", idx, err)
	}
	os.Remove(stray)

	// Make the final rename impossible: the destination is a non-empty
	// directory. (Permissions would not do: the suite may run as root.)
	if err := os.Remove(indexPath(root)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(indexPath(root), "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	second := newIndex(cfg)
	second.Projects["acme/other"] = &contract.Project{ID: 2}
	if err := saveIndex(root, second); err == nil {
		t.Fatal("saveIndex should fail when the index cannot be replaced")
	}
	entries, err := os.ReadDir(filepath.Join(root, ConfigDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a failed write left %s behind", e.Name())
		}
	}

	// And with nowhere to write at all, it says so.
	if err := saveIndex(filepath.Join(root, "missing"), second); err == nil {
		t.Error("saveIndex into a workspace without .gitty should fail")
	}
	_ = before
}

func TestSaveIndexKeepsThePreviousOneOnFailure(t *testing.T) {
	root := t.TempDir()
	cfg := &Config{URL: "https://gitlab.example.com"}
	writeConfigAt(t, root, *cfg)
	first := newIndex(cfg)
	first.Projects["acme/app"] = &contract.Project{ID: 1}
	if err := saveIndex(root, first); err != nil {
		t.Fatal(err)
	}
	before := readFileT(t, indexPath(root))

	// An index that cannot be encoded: the failure comes before any file is
	// touched.
	bad := newIndex(cfg)
	bad.GeneratedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) // outside what JSON time allows
	if err := saveIndex(root, bad); err == nil {
		t.Fatal("saveIndex should fail on an index it cannot encode")
	}
	if after := readFileT(t, indexPath(root)); after != before {
		t.Error("a failed save changed the index on disk")
	}
}

// gitOut runs git and returns its trimmed output.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// TestHeadOf: the commit is read from git's files where they settle it, and
// git is asked where they do not — the answer is always git's.
func TestHeadOf(t *testing.T) {
	ctx := context.Background()
	// countingGit records whether the fallback to a git process was taken.
	calls := 0
	git := func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
		calls++
		return execGit(ctx, dir, env, args...)
	}
	check := func(t *testing.T, dir string, wantFallback bool) {
		t.Helper()
		calls = 0
		got := headOf(ctx, git, dir)
		if want := gitOut(t, dir, "rev-parse", "HEAD"); got != want {
			t.Errorf("headOf = %q, git says %q", got, want)
		}
		if (calls > 0) != wantFallback {
			t.Errorf("asked git %d times, want fallback=%v", calls, wantFallback)
		}
	}

	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main", ".")

	t.Run("no commits yet", func(t *testing.T) {
		if got := headOf(ctx, git, repo); got != "" {
			t.Errorf("headOf = %q, want empty for an unborn branch", got)
		}
	})

	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "one")

	t.Run("a loose ref", func(t *testing.T) { check(t, repo, false) })

	t.Run("packed refs", func(t *testing.T) {
		gitRun(t, repo, "pack-refs", "--all")
		if _, err := os.Stat(filepath.Join(repo, ".git", "refs", "heads", "main")); !os.IsNotExist(err) {
			t.Skip("this git keeps the loose ref after packing")
		}
		check(t, repo, false)
	})

	t.Run("a new commit after packing", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("b"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, repo, "commit", "-q", "-am", "two")
		check(t, repo, false)
	})

	t.Run("detached", func(t *testing.T) {
		gitRun(t, repo, "checkout", "-q", "--detach", "HEAD~1")
		check(t, repo, false)
		gitRun(t, repo, "checkout", "-q", "main")
	})

	t.Run("a worktree, whose .git is a file", func(t *testing.T) {
		wt := filepath.Join(t.TempDir(), "wt")
		gitRun(t, repo, "worktree", "add", "-q", "-b", "side", wt)
		check(t, wt, true)
	})

	t.Run("not a repository", func(t *testing.T) {
		if got := headOf(ctx, git, t.TempDir()); got != "" {
			t.Errorf("headOf = %q, want empty", got)
		}
	})
}

func TestApplyListing(t *testing.T) {
	cfg := &Config{URL: "https://gitlab.external.example"}
	idx := newIndex(cfg)
	kept := &contract.Local{Dir: "acme/app", Head: "h1"}
	gone := &contract.Local{Dir: "acme/gone", Head: "h2"}
	idx.Projects["acme/app"] = &contract.Project{ID: 1, Description: "old words", Local: kept, ForkedFrom: "acme/upstream"}
	idx.Projects["acme/gone"] = &contract.Project{ID: 2, Description: "deleted upstream", Local: gone}
	idx.Projects["acme/unlisted"] = &contract.Project{ID: 3} // never checked out, no longer listed

	applyListing(idx, cfg, []*gitlab.Project{
		{
			ID: 1, PathWithNamespace: "acme/app", Description: "  new words \n", DefaultBranch: "main",
			Topics: []string{"go"}, Archived: true,
			WebURL:        "https://gitlab.external.example/acme/app",
			HTTPURLToRepo: "https://gitlab.internal.example/acme/app.git",
			SSHURLToRepo:  "git@ssh.internal.example:acme/app.git",
		},
		{ID: 9, PathWithNamespace: "acme/new", WebURL: "https://gitlab.external.example/acme/new"},
		nil,
	})

	app := idx.Projects["acme/app"]
	if app.Description != "new words" || !app.Archived || app.ForkedFrom != "acme/upstream" || app.Local != kept {
		t.Errorf("acme/app = %+v (local %+v): metadata must be replaced; the checkout and the fork origin, which a listing does not carry, kept", app, app.Local)
	}

	// A per-project lookup supplies what the listing leaves out.
	applyDetail(idx, "acme/app", &gitlab.Project{
		ContainerRegistryImagePrefix: "Registry.Example:5050/acme/app",
		ForkedFromProject:            &gitlab.ForkParent{PathWithNamespace: "acme/origin"},
	})
	if app.ForkedFrom != "acme/origin" || idx.Registry != "registry.example:5050" {
		t.Errorf("after the lookup: forked from %q, registry %q", app.ForkedFrom, idx.Registry)
	}
	applyDetail(idx, "acme/app", &gitlab.Project{}) // no longer a fork, registry disabled for it
	if app.ForkedFrom != "" || idx.Registry != "registry.example:5050" {
		t.Errorf("after a lookup with neither: forked from %q, registry %q", app.ForkedFrom, idx.Registry)
	}
	applyDetail(idx, "acme/unknown", &gitlab.Project{}) // not in the index: ignored
	if p := idx.Projects["acme/new"]; p == nil || p.ID != 9 || p.Local != nil {
		t.Errorf("acme/new = %+v", p)
	}
	// Deleted upstream but still on disk: it stays, as a checkout only.
	if p := idx.Projects["acme/gone"]; p == nil || p.ID != 0 || p.Description != "" || p.Local != gone {
		t.Errorf("acme/gone = %+v, want a checkout with no project behind it", p)
	}
	if _, ok := idx.Projects["acme/unlisted"]; ok {
		t.Error("a project neither listed nor checked out should be forgotten")
	}
	// The hosts the API advertises besides the configured one.
	if got := strings.Join(idx.Hosts, " "); got != "gitlab.internal.example ssh.internal.example" {
		t.Errorf("hosts = %q", got)
	}
	if idx.ListedAt.IsZero() {
		t.Error("ListedAt not set")
	}
}

func TestRebuildAPILinks(t *testing.T) {
	cfg := &Config{URL: "https://gitlab.example.com", Exclude: []string{"acme/vendor"}}
	idx := newIndex(cfg)
	idx.Projects["acme/app"] = &contract.Project{ID: 1}
	idx.Projects["acme/app-fork"] = &contract.Project{ID: 2, ForkedFrom: "acme/app"}
	idx.Projects["acme/orphan-fork"] = &contract.Project{ID: 3, ForkedFrom: "other/upstream"}
	idx.Projects["acme/vendor/copy"] = &contract.Project{ID: 4, ForkedFrom: "acme/app"} // excluded
	idx.Projects["acme/self"] = &contract.Project{ID: 5, ForkedFrom: "acme/self"}
	scanned := contract.Link{From: "acme/app", To: "acme/app-fork", Kind: contract.KindDepends, Evidence: "go.mod:3"}
	stale := contract.Link{From: "acme/app", To: "acme/app-fork", Kind: contract.KindFork, Evidence: "api:forked_from_project"}
	idx.Links = []contract.Link{scanned, stale}

	if !rebuildAPILinks(idx, cfg) {
		t.Fatal("rebuildAPILinks reported no change")
	}
	sortLinks(idx.Links)
	want := "depends acme/app-fork go.mod:3\nfork acme/app api:forked_from_project"
	if got := linkSet(idx.Links); got != want {
		t.Errorf("links:\n%s\n--- want:\n%s", got, want)
	}
	if got := unresolvedSet(idx.Unresolved); got != "fork other/upstream api:forked_from_project" {
		t.Errorf("unresolved: %s", got)
	}
	if rebuildAPILinks(idx, cfg) {
		t.Error("a second rebuild with nothing new reported a change")
	}
}

func TestReconcile(t *testing.T) {
	cfg := &Config{URL: "https://gitlab.example.com", Exclude: []string{"acme/vendor"}}
	idx := newIndex(cfg)
	for i, p := range []string{"acme/app", "acme/lib", "acme/new", "acme/vendor/mirror"} {
		idx.Projects[p] = &contract.Project{ID: int64(i + 1)}
	}
	idx.Links = []contract.Link{
		{From: "acme/app", To: "acme/lib", Kind: contract.KindDepends, Evidence: "go.mod:3"},
		// Its target was deleted upstream since the checkout was read.
		{From: "acme/app", To: "acme/deleted", Kind: contract.KindCIInclude, Evidence: ".gitlab-ci.yml:2"},
		// Its target is now excluded.
		{From: "acme/app", To: "acme/vendor/mirror", Kind: contract.KindDepends, Evidence: "go.mod:4"},
	}
	idx.Unresolved = []contract.Unresolved{
		// Created upstream since: a module path, so the project is its prefix.
		{From: "acme/app", Ref: "acme/new/v2", Kind: contract.KindDepends, Evidence: "go.mod:5"},
		// A bare project path names a project exactly, or nothing.
		{From: "acme/app", Ref: "acme/lib/sub", Kind: contract.KindCIInclude, Evidence: ".gitlab-ci.yml:9"},
		{From: "acme/app", Ref: "acme/still-missing", Kind: contract.KindDepends, Evidence: "go.mod:6"},
		// Would resolve to a link that is already there.
		{From: "acme/app", Ref: "acme/lib", Kind: contract.KindDepends, Evidence: "package.json:2"},
		// A reference a project makes to itself is no link.
		{From: "acme/lib", Ref: "acme/lib", Kind: contract.KindDepends, Evidence: "go.mod:1"},
	}

	if !reconcile(idx, newResolver(idx, cfg)) {
		t.Fatal("reconcile reported no change")
	}
	sortLinks(idx.Links)
	sortUnresolved(idx.Unresolved)
	wantLinks := "depends acme/lib go.mod:3\ndepends acme/new go.mod:5"
	if got := linkSet(idx.Links); got != wantLinks {
		t.Errorf("links:\n%s\n--- want:\n%s", got, wantLinks)
	}
	wantUnresolved := "ci-include acme/deleted .gitlab-ci.yml:2\nci-include acme/lib/sub .gitlab-ci.yml:9\ndepends acme/still-missing go.mod:6"
	if got := unresolvedSet(idx.Unresolved); got != wantUnresolved {
		t.Errorf("unresolved:\n%s\n--- want:\n%s", got, wantUnresolved)
	}
	if reconcile(idx, newResolver(idx, cfg)) {
		t.Error("a second reconcile with nothing new reported a change")
	}
}

// newTestWorkspace builds a workspace over fakes, rooted at a real directory
// the process is moved into (as openWorkspace would).
func newTestWorkspace(t *testing.T, cfg *Config, src gitlabSource) (*workspace, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	root := realDir(t, t.TempDir())
	writeConfigAt(t, root, *cfg)
	t.Chdir(root)
	s, stdout, stderr := newTestSyncer(cfg, src, execGit)
	s.jobs = 2
	return &workspace{s: s, Root: root, StartDir: root, Group: cfg.RootPath, anon: true}, stdout, stderr
}

// TestRefreshIndexOffline: once an index exists, bringing it up to date
// needs no instance — and without one, a checkout the index has never heard
// of is still indexed, as a checkout.
func TestRefreshIndexOffline(t *testing.T) {
	cfg := &Config{URL: "https://gitlab.example.com", RootPath: "acme"}
	src := fakeSource{projects: map[string][]*gitlab.Project{
		"acme": {
			{ID: 1, PathWithNamespace: "acme/app"},
			{ID: 2, PathWithNamespace: "acme/lib"},
		},
	}}
	w, _, stderr := newTestWorkspace(t, cfg, src)
	writeTree(t, filepath.Join(w.Root, "app"), map[string]string{
		"go.mod": "module gitlab.example.com/acme/app\n\nrequire gitlab.example.com/acme/lib v1.0.0\n",
	})
	gitRun(t, filepath.Join(w.Root, "app"), "commit", "-q", "-m", "one")
	ctx := context.Background()

	idx, stats, err := w.refreshIndex(ctx, refreshOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Written || stats.Scanned != 1 || linkSet(idx.Links) != "depends acme/lib go.mod:3" {
		t.Fatalf("first refresh: stats %+v, links %q", stats, linkSet(idx.Links))
	}

	// From here on the instance is unreachable.
	w.s.src = fakeSource{projErr: context.DeadlineExceeded}

	_, stats, err = w.refreshIndex(ctx, refreshOptions{})
	if err != nil || stats.Written || stats.Scanned != 0 {
		t.Errorf("an unchanged workspace: err=%v stats=%+v; want nothing read, nothing written", err, stats)
	}

	// Asked to look at a project again whatever its commit: it is read, and
	// with nothing new found the file is left alone.
	_, stats, err = w.refreshIndex(ctx, refreshOptions{Rescan: map[string]bool{"acme/app": true}})
	if err != nil || stats.Written || stats.Scanned != 1 {
		t.Errorf("a forced reread: err=%v stats=%+v; want one read, nothing written", err, stats)
	}

	// A checkout appears that the index does not know. The listing that
	// would explain it fails; the checkout is indexed all the same.
	writeTree(t, filepath.Join(w.Root, "handmade"), map[string]string{"README.md": "# x\n\nMade by hand.\n"})
	stderr.Reset()
	idx, stats, err = w.refreshIndex(ctx, refreshOptions{})
	if err != nil {
		t.Fatalf("an unreachable instance must not fail a refresh that does not need it: %v", err)
	}
	hand := idx.Projects["acme/handmade"]
	if hand == nil || hand.ID != 0 || hand.Local == nil || hand.Local.Summary != "Made by hand." || !stats.Written {
		t.Errorf("acme/handmade = %+v, stats %+v", hand, stats)
	}
	if !strings.Contains(stderr.String(), "not refreshed") {
		t.Errorf("the failed listing should be noted on stderr:\n%s", stderr.String())
	}
	// It is never a link target: nothing upstream answers to that name.
	if newResolver(idx, cfg).knows("acme/handmade") {
		t.Error("a checkout with no project upstream must not be resolvable")
	}

	// The checkout goes away: so does everything it contributed.
	if err := os.RemoveAll(filepath.Join(w.Root, "app")); err != nil {
		t.Fatal(err)
	}
	var events []string
	idx, _, err = w.refreshIndex(ctx, refreshOptions{Report: func(ev indexEvent) {
		events = append(events, ev.Kind+" "+ev.Project+" "+ev.Detail)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Links) != 0 || idx.Projects["acme/app"] == nil || idx.Projects["acme/app"].Local != nil {
		t.Errorf("after removing the checkout: links %q, app %+v", linkSet(idx.Links), idx.Projects["acme/app"])
	}
	if got := strings.Join(events, "\n"); got != "index acme/app removed\nindex acme/handmade unchanged" {
		t.Errorf("events:\n%s", got)
	}

	// With no index and no instance there is nothing to answer from.
	if err := os.Remove(indexPath(w.Root)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.refreshIndex(ctx, refreshOptions{}); err == nil {
		t.Error("building an index without the instance should fail, not produce an empty one")
	}
	if _, statErr := os.Stat(indexPath(w.Root)); !os.IsNotExist(statErr) {
		t.Error("a failed build left an index behind")
	}
}

// TestRefreshIndexArchived: an archived project is described but not asked
// what it uses, unless archived projects are asked for.
func TestRefreshIndexArchived(t *testing.T) {
	cfg := &Config{URL: "https://gitlab.example.com", RootPath: "acme"}
	src := fakeSource{projects: map[string][]*gitlab.Project{
		"acme": {
			{ID: 1, PathWithNamespace: "acme/old", Archived: true},
			{ID: 2, PathWithNamespace: "acme/lib"},
		},
	}}
	w, _, _ := newTestWorkspace(t, cfg, src)
	writeTree(t, filepath.Join(w.Root, "old"), map[string]string{
		"go.mod":    "module x\n\nrequire gitlab.example.com/acme/lib v1.0.0\n",
		"README.md": "# old\n\nRetired.\n",
	})
	ctx := context.Background()

	idx, _, err := w.refreshIndex(ctx, refreshOptions{List: true})
	if err != nil {
		t.Fatal(err)
	}
	if old := idx.Projects["acme/old"]; len(idx.Links) != 0 || old.Local == nil || old.Local.Summary != "Retired." {
		t.Errorf("archived by default: links %q, local %+v", linkSet(idx.Links), old.Local)
	}

	w.s.includeArchived = true
	idx, _, err = w.refreshIndex(ctx, refreshOptions{List: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := linkSet(idx.Links); got != "depends acme/lib go.mod:3" {
		t.Errorf("with archived projects asked for: links %q", got)
	}

	w.s.includeArchived = false
	idx, _, err = w.refreshIndex(ctx, refreshOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Links) != 0 {
		t.Errorf("back to the default: links %q, want none", linkSet(idx.Links))
	}
}

func TestNormalExcludes(t *testing.T) {
	got := normalExcludes([]string{" acme/b/ ", "acme/a", "acme/b", "", "/acme/a"})
	if strings.Join(got, ",") != "acme/a,acme/b" {
		t.Errorf("normalExcludes = %v", got)
	}
	if normalExcludes(nil) != nil {
		t.Error("no patterns should stay nil, so an index without excludes omits the field")
	}
}
