package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

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
