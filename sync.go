package main

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gitlab.com/gitlab-org/api/client-go"
)

// gitlabSource is the subset of the GitLab API that gitty needs. Defining it as
// an interface lets the sync logic be exercised with a fake in tests instead of
// talking to a live GitLab instance. Implementations are responsible for
// aggregating paginated responses into a single slice.
type gitlabSource interface {
	// Subgroups returns the immediate subgroups of target, or every descendant
	// group when nested is true.
	Subgroups(target string, nested bool) ([]*gitlab.Group, error)
	// Group returns the group identified by target.
	Group(target string) (*gitlab.Group, error)
	// Projects returns the projects directly in target, or all projects
	// including those in subgroups when nested is true.
	Projects(target string, nested bool) ([]*gitlab.Project, error)
}

// gitRunner executes a git command in dir. Injecting it as a function makes the
// clone/pull decision logic testable without shelling out to real git.
type gitRunner func(dir string, args ...string) error

// syncer carries the resolved configuration and collaborators for a single
// sync run.
type syncer struct {
	cfg    *Config
	src    gitlabSource
	git    gitRunner
	dryRun bool
	nested bool
}

// runSync wires up the real GitLab client and git runner and performs a sync.
// It returns an error when setup fails or when one or more groups/repositories
// could not be synced, so the caller can surface a non-zero exit code.
func runSync(groupFlag, tokenFlag string, dryRun, doGroups, doRepos, nested, anon bool) error {
	if !doGroups && !doRepos {
		doRepos = true
	}

	cfg, err := LoadLocalConfig()
	if err != nil {
		return fmt.Errorf("no .gitty/config found in this directory; run 'gitty init' first")
	}

	fullTarget := cfg.RootPath
	if groupFlag != "" {
		if fullTarget != "" {
			fullTarget = fullTarget + "/" + groupFlag
		} else {
			fullTarget = groupFlag
		}
	}

	if fullTarget == "" {
		return fmt.Errorf("target group path is empty; provide --path or sync from a managed subgroup directory")
	}

	token := resolveToken(tokenFlag)
	if token == "" && !anon {
		return fmt.Errorf("a token (via --token flag, GITLAB_TOKEN, or CI_JOB_TOKEN env var) is required; use --anon to sync public resources without a token")
	}
	if token == "" {
		fmt.Println("Running anonymously (--anon): only public groups and repositories are accessible.")
	}

	client, err := gitlab.NewClient(token, gitlab.WithBaseURL(cfg.URL))
	if err != nil {
		return fmt.Errorf("failed to create GitLab client: %w", err)
	}

	if dryRun {
		fmt.Println("=== DRY RUN MODE ENABLED: No changes will be made ===")
	}

	s := &syncer{
		cfg:    cfg,
		src:    gitlabClientSource{client: client},
		git:    execGit,
		dryRun: dryRun,
		nested: nested,
	}

	failures := 0
	if doGroups {
		failures += s.syncGroups(fullTarget)
	}
	if doRepos {
		failures += s.syncRepos(fullTarget)
	}

	if failures > 0 {
		return fmt.Errorf("completed with %d error(s)", failures)
	}
	return nil
}

// syncGroups fetches subgroups and creates directories with their own
// .gitty/configs. It returns the number of groups that could not be synced so
// callers can aggregate failures across the run.
func (s *syncer) syncGroups(target string) int {
	fmt.Printf("\n--- Syncing Groups ---\n")
	fmt.Printf("Fetching subgroups for: '%s' (Nested: %t)...\n", target, s.nested)

	allGroups, err := s.src.Subgroups(target, s.nested)
	if err != nil {
		fmt.Printf("  -> Error fetching subgroups for %s: %v\n", target, err)
		return 1
	}

	if root, err := s.src.Group(target); err == nil && root != nil {
		allGroups = append([]*gitlab.Group{root}, allGroups...)
	}

	fmt.Printf("Found %d groups to sync.\n", len(allGroups))

	failures := 0
	for _, g := range allGroups {
		relPath := getLocalRelPath(g.FullPath, s.cfg.RootPath)
		if !isWithinWorkspace(relPath) {
			fmt.Printf("  -> Skipping %s: resolved path %q escapes the workspace\n", g.FullPath, relPath)
			failures++
			continue
		}
		groupDest := filepath.Join(".", relPath)

		if s.dryRun {
			fmt.Printf("  -> [DRY RUN] Would create group and config for: %s\n", g.FullPath)
			continue
		}

		if err := os.MkdirAll(groupDest, 0755); err != nil {
			fmt.Printf("  -> Error creating directory %s: %v\n", groupDest, err)
			failures++
			continue
		}

		subCfg := &Config{
			URL:      s.cfg.URL,
			HTTP:     s.cfg.HTTP,
			RootPath: g.FullPath,
		}
		if err := SaveConfigTo(groupDest, subCfg); err != nil {
			fmt.Printf("  -> Error saving config to %s: %v\n", groupDest, err)
			failures++
			continue
		}
		fmt.Printf("  -> Ensured context for: %s\n", g.FullPath)
	}

	fmt.Println("Finished group directory sync!")
	return failures
}

// syncRepos fetches projects and clones or pulls them. It returns the number of
// repositories that could not be synced.
func (s *syncer) syncRepos(target string) int {
	fmt.Printf("\n--- Syncing Repositories ---\n")
	fmt.Printf("Fetching projects for: '%s' (Nested: %t)...\n", target, s.nested)

	allProjects, err := s.src.Projects(target, s.nested)
	if err != nil {
		fmt.Printf("  -> Error fetching projects for %s: %v\n", target, err)
		return 1
	}

	fmt.Printf("Found %d projects.\n", len(allProjects))

	failures := 0
	for _, p := range allProjects {
		cloneURL := p.SSHURLToRepo
		if s.cfg.HTTP {
			cloneURL = p.HTTPURLToRepo
		}

		// Calculate destination relative to where we ran the command.
		relPath := getLocalRelPath(p.PathWithNamespace, s.cfg.RootPath)
		if !isWithinWorkspace(relPath) {
			fmt.Printf("\nSkipping %s: resolved path %q escapes the workspace\n", p.PathWithNamespace, relPath)
			failures++
			continue
		}
		repoDest := filepath.Join(".", relPath)

		fmt.Printf("\nProcessing %s...\n", p.PathWithNamespace)

		if _, statErr := os.Stat(repoDest); !os.IsNotExist(statErr) {
			// Directory already exists: fast-forward it. --ff-only refuses to
			// create a merge commit, so a diverged or dirty checkout fails
			// loudly instead of leaving the repo in a surprising state.
			if s.dryRun {
				fmt.Printf("  -> [DRY RUN] Would execute 'git pull --ff-only' in %s\n", repoDest)
				continue
			}
			fmt.Printf("  -> Directory exists. Attempting 'git pull --ff-only'\n")
			if err := s.git(repoDest, "pull", "--ff-only"); err != nil {
				fmt.Printf("  -> git pull failed in %s: %v\n", repoDest, err)
				fmt.Printf("     (local changes or a diverged branch may require manual resolution)\n")
				failures++
			}
			continue
		}

		// New checkout: verify the clone URL points at the configured instance
		// before handing it to git, so a compromised or misconfigured API
		// response cannot redirect the clone to an attacker-controlled host.
		if ok, err := hostsMatch(s.cfg.URL, cloneURL); err != nil || !ok {
			fmt.Printf("  -> Skipping %s: clone URL %q does not match the configured instance %q\n", p.PathWithNamespace, cloneURL, s.cfg.URL)
			failures++
			continue
		}

		if s.dryRun {
			fmt.Printf("  -> [DRY RUN] Would clone %s to %s\n", cloneURL, repoDest)
			continue
		}

		fmt.Printf("  -> Cloning %s\n", cloneURL)
		parentDir := filepath.Dir(repoDest)
		if err := os.MkdirAll(parentDir, 0755); err != nil {
			fmt.Printf("  -> Error creating parent directories: %v\n", err)
			failures++
			continue
		}
		if err := s.git(".", "clone", cloneURL, repoDest); err != nil {
			fmt.Printf("  -> git clone failed for %s: %v\n", cloneURL, err)
			failures++
		}
	}

	fmt.Println("\nFinished repository sync!")
	return failures
}

// execGit runs a git command, streaming output, while preserving the user's
// environment (SSH_AUTH_SOCK, global ~/.gitconfig, etc.).
func execGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// resolveToken picks the GitLab access token from the --token flag first, then
// the GITLAB_TOKEN environment variable, then CI_JOB_TOKEN. It returns "" when
// none is set.
func resolveToken(flagToken string) string {
	if flagToken != "" {
		return flagToken
	}
	if t := os.Getenv("GITLAB_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("CI_JOB_TOKEN")
}

// getLocalRelPath strips the local context's RootPath from the GitLab API path
// so that folders are built correctly relative to the current directory. The
// prefix is only stripped on a path-segment boundary, so a configRoot of
// "acme/team" does not accidentally match "acme/team-x/repo".
func getLocalRelPath(apiFullPath, configRoot string) string {
	if configRoot == "" {
		return apiFullPath
	}
	if apiFullPath == configRoot {
		return ""
	}
	if rel := strings.TrimPrefix(apiFullPath, configRoot+"/"); rel != apiFullPath {
		return rel
	}
	// configRoot is not a path-segment prefix of apiFullPath; leave it as-is
	// rather than mangling the path.
	return apiFullPath
}

// isWithinWorkspace reports whether a relative destination path stays inside the
// current workspace. It rejects absolute paths and any path that escapes the
// workspace root via "..". This guards against a malicious or misconfigured
// GitLab instance returning namespace paths that would write outside the tree.
func isWithinWorkspace(rel string) bool {
	if rel == "" {
		return true
	}
	if filepath.IsAbs(rel) {
		return false
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// extractHost returns the lower-cased host of a git remote, understanding both
// URL forms (https://host/path, ssh://git@host/path) and the scp-like SSH
// syntax (git@host:path). It returns "" when no host can be determined.
func extractHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		// scp-like syntax: [user@]host:path
		if at := strings.LastIndex(raw, "@"); at != -1 {
			raw = raw[at+1:]
		}
		if colon := strings.Index(raw, ":"); colon != -1 {
			raw = raw[:colon]
		}
		return strings.ToLower(raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// hostsMatch reports whether a clone URL targets the same host as the
// configured GitLab instance. It returns an error when either host cannot be
// determined, which callers treat as a mismatch.
func hostsMatch(configURL, cloneURL string) (bool, error) {
	ch := extractHost(configURL)
	rh := extractHost(cloneURL)
	if ch == "" || rh == "" {
		return false, fmt.Errorf("could not determine host (config %q, clone %q)", configURL, cloneURL)
	}
	return ch == rh, nil
}

// gitlabClientSource adapts a *gitlab.Client to the gitlabSource interface,
// handling pagination for each listing.
type gitlabClientSource struct {
	client *gitlab.Client
}

func (s gitlabClientSource) Subgroups(target string, nested bool) ([]*gitlab.Group, error) {
	var all []*gitlab.Group
	if nested {
		opts := &gitlab.ListDescendantGroupsOptions{
			ListOptions: gitlab.ListOptions{PerPage: 100, Page: 1},
		}
		for {
			groups, resp, err := s.client.Groups.ListDescendantGroups(target, opts)
			if err != nil {
				return nil, err
			}
			all = append(all, groups...)
			if resp.NextPage == 0 {
				break
			}
			opts.Page = resp.NextPage
		}
		return all, nil
	}
	opts := &gitlab.ListSubGroupsOptions{
		ListOptions: gitlab.ListOptions{PerPage: 100, Page: 1},
	}
	for {
		groups, resp, err := s.client.Groups.ListSubGroups(target, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, groups...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

func (s gitlabClientSource) Group(target string) (*gitlab.Group, error) {
	g, _, err := s.client.Groups.GetGroup(target, nil)
	return g, err
}

func (s gitlabClientSource) Projects(target string, nested bool) ([]*gitlab.Project, error) {
	var all []*gitlab.Project
	opts := &gitlab.ListGroupProjectsOptions{
		IncludeSubGroups: &nested,
		ListOptions: gitlab.ListOptions{
			PerPage: 100,
			Page:    1,
		},
	}
	for {
		projects, resp, err := s.client.Groups.ListGroupProjects(target, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, projects...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}
