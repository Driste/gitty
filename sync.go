package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"gitlab.com/gitlab-org/api/client-go"
)

func runSync(groupPath, tokenFlag string, dryRun, doGroups, doRepos, nested bool) {
	if groupPath == "" {
		fmt.Println("Error: --group is a required flag for sync.")
		os.Exit(1)
	}

	if !doGroups && !doRepos {
		doRepos = true
	}

	cfg, err := LoadConfig()
	if err != nil {
		if os.IsNotExist(err) {
			log.Fatalf("Error: %s not found. Please run 'gitty init' in this directory.", ConfigPath)
		}
		log.Fatalf("Configuration error: %v", err)
	}

	destDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to get current directory: %v", err)
	}

	token := tokenFlag
	if token == "" {
		if envToken := os.Getenv("GITLAB_TOKEN"); envToken != "" {
			token = envToken
		} else if ciToken := os.Getenv("CI_JOB_TOKEN"); ciToken != "" {
			token = ciToken
		}
	}

	if token == "" {
		fmt.Println("Error: A token (via --token flag, GITLAB_TOKEN, or CI_JOB_TOKEN env var) is required.")
		os.Exit(1)
	}

	client, err := gitlab.NewClient(token, gitlab.WithBaseURL(cfg.URL))
	if err != nil {
		log.Fatalf("Failed to create GitLab client: %v", err)
	}

	if dryRun {
		fmt.Println("=== DRY RUN MODE ENABLED: No changes will be made ===")
	}

	if doGroups {
		fmt.Printf("\n--- Syncing Groups ---\n")
		fmt.Printf("Fetching subgroups for group: '%s' (Nested: %t)...\n", groupPath, nested)

		var allGroups []*gitlab.Group

		if nested {
			opts := &gitlab.ListDescendantGroupsOptions{
				ListOptions: gitlab.ListOptions{PerPage: 100, Page: 1},
			}
			for {
				groups, resp, err := client.Groups.ListDescendantGroups(groupPath, opts)
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
				groups, resp, err := client.Groups.ListSubGroups(groupPath, opts)
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

		rootGroup, _, err := client.Groups.GetGroup(groupPath, nil)
		if err == nil && rootGroup != nil {
			allGroups = append([]*gitlab.Group{rootGroup}, allGroups...)
		}

		fmt.Printf("Found %d groups. Creating directories in %s...\n", len(allGroups), destDir)

		for _, g := range allGroups {
			groupDest := filepath.Join(destDir, g.FullPath)
			if dryRun {
				fmt.Printf("  -> [DRY RUN] Would create directory: %s\n", groupDest)
			} else {
				if err := os.MkdirAll(groupDest, 0755); err != nil {
					fmt.Printf("  -> Error creating directory %s: %v\n", g.FullPath, err)
				} else {
					fmt.Printf("  -> Ensured directory exists: %s\n", groupDest)
				}
			}
		}
		fmt.Println("Finished group directories sync!")
	}

	if doRepos {
		fmt.Printf("\n--- Syncing Repositories ---\n")
		fmt.Printf("Fetching projects for group: '%s' (Nested: %t)...\n", groupPath, nested)

		var allProjects []*gitlab.Project

		opts := &gitlab.ListGroupProjectsOptions{
			IncludeSubGroups: &nested,
			ListOptions: gitlab.ListOptions{
				PerPage: 100,
				Page:    1,
			},
		}

		for {
			projects, resp, err := client.Groups.ListGroupProjects(groupPath, opts)
			if err != nil {
				log.Fatalf("Failed to fetch group projects: %v", err)
			}
			allProjects = append(allProjects, projects...)

			if resp.NextPage == 0 {
				break
			}
			opts.Page = resp.NextPage
		}

		fmt.Printf("Found %d projects. Starting clone/pull process in %s...\n", len(allProjects), destDir)

		for _, p := range allProjects {
			cloneURL := p.SSHURLToRepo
			if cfg.HTTP {
				cloneURL = p.HTTPURLToRepo
			}

			repoDest := filepath.Join(destDir, p.PathWithNamespace)
			fmt.Printf("\nProcessing %s...\n", p.PathWithNamespace)

			if _, err := os.Stat(repoDest); !os.IsNotExist(err) {
				if dryRun {
					fmt.Printf("  -> [DRY RUN] Would execute 'git pull' in %s\n", repoDest)
				} else {
					fmt.Printf("  -> Directory exists. Attempting 'git pull'\n")
					cmd := exec.Command("git", "pull")
					cmd.Dir = repoDest
					cmd.Stdout = os.Stdout
					cmd.Stderr = os.Stderr
					if err := cmd.Run(); err != nil {
						fmt.Printf("  -> Error pulling %s: %v\n", p.Name, err)
					}
				}
			} else {
				if dryRun {
					fmt.Printf("  -> [DRY RUN] Would create dirs and clone %s to %s\n", cloneURL, repoDest)
				} else {
					fmt.Printf("  -> Cloning %s\n", cloneURL)

					parentDir := filepath.Dir(repoDest)
					if err := os.MkdirAll(parentDir, 0755); err != nil {
						fmt.Printf("  -> Error creating parent directories: %v\n", err)
						continue
					}

					cmd := exec.Command("git", "clone", cloneURL, repoDest)
					cmd.Stdout = os.Stdout
					cmd.Stderr = os.Stderr
					if err := cmd.Run(); err != nil {
						fmt.Printf("  -> Error cloning %s: %v\n", p.Name, err)
					}
				}
			}
		}
		fmt.Println("\nFinished repository sync!")
	}
}
