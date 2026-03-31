package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
	"gitlab.com/gitlab-org/api/client-go"
)

const ConfigFileName = "gitty.toml"

// Config represents the structure of our gitty.toml file
type Config struct {
	URL  string `toml:"url"`
	HTTP bool   `toml:"http"`
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	// 1. Define Subcommands
	initCmd := flag.NewFlagSet("init", flag.ExitOnError)
	initURL := initCmd.String("url", "https://gitlab.com", "GitLab Base URL")
	initHTTP := initCmd.Bool("http", false, "Use HTTP(S) for cloning instead of SSH")

	syncCmd := flag.NewFlagSet("sync", flag.ExitOnError)
	syncGroup := syncCmd.String("group", "", "GitLab Group Path (e.g., tenant/images) (required)")
	syncToken := syncCmd.String("token", "", "GitLab Access Token (falls back to env vars)")
	syncDryRun := syncCmd.Bool("dry-run", false, "Print what would happen without actually making changes")
	syncGroups := syncCmd.Bool("groups", false, "Fetch and create group/subgroup directory structures")
	syncRepos := syncCmd.Bool("repos", false, "Fetch and clone/pull repositories")
	syncNested := syncCmd.Bool("nested", false, "Include nested subgroups/projects recursively")

	// 2. Route Subcommands
	switch os.Args[1] {
	case "init":
		initCmd.Parse(os.Args[2:])
		runInit(*initURL, *initHTTP)
	case "sync":
		syncCmd.Parse(os.Args[2:])
		runSync(*syncGroup, *syncToken, *syncDryRun, *syncGroups, *syncRepos, *syncNested)
	default:
		fmt.Printf("Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Usage: gitty <command> [flags]")
	fmt.Println("\nCommands:")
	fmt.Println("  init    Initialize a gitty.toml config file in the current directory")
	fmt.Println("  sync    Sync (clone/pull) a GitLab group based on the gitty.toml config")
	fmt.Println("\nRun 'gitty <command> -h' for specific flags.")
}

// runInit handles 'gitty init'
func runInit(url string, useHTTP bool) {
	if _, err := os.Stat(ConfigFileName); err == nil {
		fmt.Printf("Error: %s already exists in this directory.\n", ConfigFileName)
		os.Exit(1)
	}

	cfg := Config{
		URL:  url,
		HTTP: useHTTP,
	}

	data, err := toml.Marshal(cfg)
	if err != nil {
		log.Fatalf("Failed to generate TOML: %v", err)
	}

	err = os.WriteFile(ConfigFileName, data, 0644)
	if err != nil {
		log.Fatalf("Failed to write %s: %v", ConfigFileName, err)
	}

	fmt.Printf("Successfully initialized %s!\n", ConfigFileName)
	fmt.Printf("URL: %s\nHTTP: %t\n", cfg.URL, cfg.HTTP)
	fmt.Println("You can now run 'gitty sync --group=<path>' to pull down repositories.")
}

// runSync handles 'gitty sync'
func runSync(groupPath, tokenFlag string, dryRun, doGroups, doRepos, nested bool) {
	if groupPath == "" {
		fmt.Println("Error: --group is a required flag for sync.")
		os.Exit(1)
	}

	// Default to syncing repos if neither flag was explicitly provided
	if !doGroups && !doRepos {
		doRepos = true
	}

	// 1. Load Config
	data, err := os.ReadFile(ConfigFileName)
	if err != nil {
		if os.IsNotExist(err) {
			log.Fatalf("Error: %s not found. Please run 'gitty init' first.", ConfigFileName)
		}
		log.Fatalf("Failed to read %s: %v", ConfigFileName, err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("Failed to parse %s: %v", ConfigFileName, err)
	}

	// The current directory (where gitty.toml lives) is our root destination
	destDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to get current directory: %v", err)
	}

	// 2. Resolve Token
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

	// 3. Initialize GitLab Client using config
	client, err := gitlab.NewClient(token, gitlab.WithBaseURL(cfg.URL))
	if err != nil {
		log.Fatalf("Failed to create GitLab client: %v", err)
	}

	if dryRun {
		fmt.Println("=== DRY RUN MODE ENABLED: No changes will be made ===")
	}

	// ==========================================
	// GROUP SYNC LOGIC
	// ==========================================
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

		// Grab the parent group itself to ensure the root directory is also created
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

	// ==========================================
	// REPOSITORY SYNC LOGIC
	// ==========================================
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