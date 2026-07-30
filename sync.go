package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gitlab.com/gitlab-org/api/client-go"
)

func runSync(groupFlag, tokenFlag string, dryRun, doGroups, doRepos, nested, anon bool) {

	if !doGroups && !doRepos {
		doRepos = true
	}

	cfg, err := LoadLocalConfig()
	if err != nil {
		log.Fatalf("Error: No .gitty/config found in this directory. Run 'gitty init' first.")
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
		log.Fatal("Error: Target group path is empty. Provide --path or sync from a managed subgroup directory.")
	}

	token := resolveToken(tokenFlag)
	if token == "" && !anon {
		log.Fatal("Error: A token (via --token flag, GITLAB_TOKEN, or CI_JOB_TOKEN env var) is required. Use --anon to sync public resources without a token.")
	}
	if token == "" {
		fmt.Println("Running anonymously (--anon): only public groups and repositories are accessible.")
	}

	client, err := gitlab.NewClient(token, gitlab.WithBaseURL(cfg.URL))
	if err != nil {
		log.Fatalf("Failed to create GitLab client: %v", err)
	}

	if dryRun {
		fmt.Println("=== DRY RUN MODE ENABLED: No changes will be made ===")
	}

	if doGroups {
		syncGroups(client, fullTarget, cfg, dryRun, nested)
	}
	if doRepos {
		syncRepos(client, fullTarget, cfg, dryRun, nested)
	}
}

// syncGroups fetches subgroups and creates directories with their own .gitty/configs
func syncGroups(client *gitlab.Client, target string, cfg *Config, dryRun, nested bool) {
	fmt.Printf("\n--- Syncing Groups ---\n")
	fmt.Printf("Fetching subgroups for: '%s' (Nested: %t)...\n", target, nested)

	var allGroups []*gitlab.Group

	if nested {
		opts := &gitlab.ListDescendantGroupsOptions{
			ListOptions: gitlab.ListOptions{PerPage: 100, Page: 1},
		}
		for {
			groups, resp, err := client.Groups.ListDescendantGroups(target, opts)
			if err != nil {
				log.Fatalf("Failed to fetch descendant groups: %v", err)
			}
			allGroups = append(allGroups, groups...)
			if resp.NextPage == 0 {
				break
			}
			opts.Page = resp.NextPage
		}
	} else {
		opts := &gitlab.ListSubGroupsOptions{
			ListOptions: gitlab.ListOptions{PerPage: 100, Page: 1},
		}
		for {
			groups, resp, err := client.Groups.ListSubGroups(target, opts)
			if err != nil {
				log.Fatalf("Failed to fetch immediate subgroups: %v", err)
			}
			allGroups = append(allGroups, groups...)
			if resp.NextPage == 0 {
				break
			}
			opts.Page = resp.NextPage
		}
	}

	rootGroup, _, err := client.Groups.GetGroup(target, nil)
	if err == nil && rootGroup != nil {
		allGroups = append([]*gitlab.Group{rootGroup}, allGroups...)
	}

	fmt.Printf("Found %d groups to sync.\n", len(allGroups))

	for _, g := range allGroups {
		relPath := getLocalRelPath(g.FullPath, cfg.RootPath)
		if !isWithinWorkspace(relPath) {
			fmt.Printf("  -> Skipping %s: resolved path %q escapes the workspace\n", g.FullPath, relPath)
			continue
		}
		groupDest := filepath.Join(".", relPath)

		if dryRun {
			fmt.Printf("  -> [DRY RUN] Would create group and config for: %s\n", g.FullPath)
		} else {
			if err := os.MkdirAll(groupDest, 0755); err != nil {
				fmt.Printf("  -> Error creating directory %s: %v\n", groupDest, err)
				continue
			}

			subCfg := &Config{
				URL:      cfg.URL,
				HTTP:     cfg.HTTP,
				RootPath: g.FullPath,
			}
			
			if err := SaveConfigTo(groupDest, subCfg); err != nil {
				fmt.Printf("  -> Error saving config to %s: %v\n", groupDest, err)
			} else {
				fmt.Printf("  -> Ensured context for: %s\n", g.FullPath)
			}
		}
	}
	fmt.Println("Finished group directory sync!")
}

// syncRepos fetches projects and clones or pulls them
func syncRepos(client *gitlab.Client, target string, cfg *Config, dryRun, nested bool) {
	fmt.Printf("\n--- Syncing Repositories ---\n")
	fmt.Printf("Fetching projects for: '%s' (Nested: %t)...\n", target, nested)

	var allProjects []*gitlab.Project

	opts := &gitlab.ListGroupProjectsOptions{
		IncludeSubGroups: &nested,
		ListOptions: gitlab.ListOptions{
			PerPage: 100,
			Page:    1,
		},
	}

	for {
		projects, resp, err := client.Groups.ListGroupProjects(target, opts)
		if err != nil {
			log.Fatalf("Failed to fetch group projects: %v", err)
		}
		allProjects = append(allProjects, projects...)

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	fmt.Printf("Found %d projects.\n", len(allProjects))

	for _, p := range allProjects {
		cloneURL := p.SSHURLToRepo
		if cfg.HTTP {
			cloneURL = p.HTTPURLToRepo
		}

		// Calculate destination relative to where we ran the command
		relPath := getLocalRelPath(p.PathWithNamespace, cfg.RootPath)
		if !isWithinWorkspace(relPath) {
			fmt.Printf("\nSkipping %s: resolved path %q escapes the workspace\n", p.PathWithNamespace, relPath)
			continue
		}
		repoDest := filepath.Join(".", relPath)

		fmt.Printf("\nProcessing %s...\n", p.PathWithNamespace)

		if _, err := os.Stat(repoDest); !os.IsNotExist(err) {
			if dryRun {
				fmt.Printf("  -> [DRY RUN] Would execute 'git pull' in %s\n", repoDest)
			} else {
				fmt.Printf("  -> Directory exists. Attempting 'git pull'\n")
				runGit(repoDest, "pull")
			}
		} else {
			if dryRun {
				fmt.Printf("  -> [DRY RUN] Would clone %s to %s\n", cloneURL, repoDest)
			} else {
				fmt.Printf("  -> Cloning %s\n", cloneURL)

				parentDir := filepath.Dir(repoDest)
				if err := os.MkdirAll(parentDir, 0755); err != nil {
					fmt.Printf("  -> Error creating parent directories: %v\n", err)
					continue
				}

				runGit(".", "clone", cloneURL, repoDest)
			}
		}
	}
	fmt.Println("\nFinished repository sync!")
}

// runGit executes a git command ensuring the user's environment is preserved
func runGit(dir string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = os.Environ() // Preserves SSH_AUTH_SOCK, global ~/.gitconfig, etc.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("  -> Git execution error: %v\n", err)
	}
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