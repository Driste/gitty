// Package sync orchestrates a single `gitty sync` invocation. It composes
// the leaf packages (config, gitlabapi, gitexec, paths) and is the only
// internal package allowed to depend on more than one other internal
// package.
//
// The data-model document for this feature describes a future explicit
// Plan/Step value type. The current implementation keeps planning and
// applying interleaved per section so the section banners ("--- Syncing
// Groups ---", "Found N groups...", "Finished group directory sync!")
// emit in the same order they did before the cleanup. Promoting Plan to
// an exported value type is straightforward and can land in a follow-up
// once async sync (a separate spec) is on the table.
package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gitty/internal/config"
	"gitty/internal/gitexec"
	"gitty/internal/gitlabapi"
	"gitty/internal/paths"
)

// Request is the parsed CLI input that drives one sync invocation.
//
// Jobs is the resolved effective concurrency, already validated and clamped
// to [1, 64] by the cli package. Sync itself does no further validation.
type Request struct {
	GroupFlag string
	Token     string
	DryRun    bool
	DoGroups  bool
	DoRepos   bool
	Nested    bool
	Jobs      int
}

// Deps is the set of injectable collaborators Sync needs. The cli package
// wires the production set; tests build their own.
type Deps struct {
	Client gitlabapi.Client
	Runner gitexec.Runner
	Stdout io.Writer
	Stderr io.Writer
	// Exists reports whether a local path is already present on disk.
	// Production: !os.IsNotExist(os.Stat err). Tests inject a closure.
	Exists func(path string) bool
}

// Sync executes one `gitty sync` invocation given an already-loaded Config.
// Stream destinations follow Constitution Principle I (FR-010): the
// `--dry-run` plan goes to deps.Stdout; banners, progress, and errors go
// to deps.Stderr. The provided context cancels in-flight git children.
func Sync(ctx context.Context, req Request, cfg *config.Config, deps Deps) error {
	target := composeTarget(cfg.RootPath, req.GroupFlag)
	if target == "" {
		return errors.New("Error: Target group path is empty. Provide --path or sync from a managed subgroup directory.")
	}

	if req.DryRun {
		fmt.Fprintln(deps.Stdout, "=== DRY RUN MODE ENABLED: No changes will be made ===")
	}

	if req.DoGroups {
		if err := syncGroupsSection(ctx, req, cfg, target, deps); err != nil {
			return err
		}
	}
	if req.DoRepos {
		if err := syncReposSection(ctx, req, cfg, target, deps); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// composeTarget joins the workspace's anchored root path with the
// per-invocation --path flag, preserving the original behavior:
//
//	root="" group=""    -> ""
//	root="" group="g"   -> "g"
//	root="x" group=""   -> "x"
//	root="x" group="g"  -> "x/g"
func composeTarget(rootPath, groupFlag string) string {
	if groupFlag == "" {
		return rootPath
	}
	if rootPath == "" {
		return groupFlag
	}
	return rootPath + "/" + groupFlag
}

func syncGroupsSection(ctx context.Context, req Request, cfg *config.Config, target string, deps Deps) error {
	fmt.Fprintf(deps.Stderr, "\n--- Syncing Groups ---\n")
	fmt.Fprintf(deps.Stderr, "Fetching subgroups for: '%s' (Nested: %t)...\n", target, req.Nested)

	var groups []*gitlabapi.Group
	var err error
	if req.Nested {
		groups, err = deps.Client.ListDescendantGroups(target)
		if err != nil {
			return fmt.Errorf("Failed to fetch descendant groups: %w", err)
		}
	} else {
		groups, err = deps.Client.ListSubGroups(target)
		if err != nil {
			return fmt.Errorf("Failed to fetch immediate subgroups: %w", err)
		}
	}
	if root, _ := deps.Client.GetGroup(target); root != nil {
		groups = append([]*gitlabapi.Group{root}, groups...)
	}
	fmt.Fprintf(deps.Stderr, "Found %d groups to sync.\n", len(groups))

	for _, g := range groups {
		local := filepath.Join(".", paths.Local(g.FullPath, cfg.RootPath))
		if req.DryRun {
			fmt.Fprintf(deps.Stdout, "  -> [DRY RUN] Would create group and config for: %s\n", g.FullPath)
			continue
		}
		if err := os.MkdirAll(local, 0755); err != nil {
			fmt.Fprintf(deps.Stderr, "  -> Error creating directory %s: %v\n", local, err)
			continue
		}
		sub := &config.Config{URL: cfg.URL, HTTP: cfg.HTTP, RootPath: g.FullPath, Jobs: cfg.Jobs}
		if err := config.Save(local, sub); err != nil {
			fmt.Fprintf(deps.Stderr, "  -> Error saving config to %s: %v\n", local, err)
		} else {
			fmt.Fprintf(deps.Stderr, "  -> Ensured context for: %s\n", g.FullPath)
		}
	}
	fmt.Fprintln(deps.Stderr, "Finished group directory sync!")
	return nil
}

func syncReposSection(ctx context.Context, req Request, cfg *config.Config, target string, deps Deps) error {
	fmt.Fprintf(deps.Stderr, "\n--- Syncing Repositories ---\n")
	fmt.Fprintf(deps.Stderr, "Fetching projects for: '%s' (Nested: %t)...\n", target, req.Nested)

	projects, err := deps.Client.ListGroupProjects(target, req.Nested)
	if err != nil {
		return fmt.Errorf("Failed to fetch group projects: %w", err)
	}
	fmt.Fprintf(deps.Stderr, "Found %d projects.\n", len(projects))

	if req.DryRun {
		for _, prj := range projects {
			url := prj.SSHURLToRepo
			if cfg.HTTP {
				url = prj.HTTPURLToRepo
			}
			if url == "" {
				fmt.Fprintf(deps.Stderr, "  -> Skipping %s: clone URL is empty\n", prj.PathWithNamespace)
				continue
			}
			local := filepath.Join(".", paths.Local(prj.PathWithNamespace, cfg.RootPath))
			if deps.Exists(local) {
				fmt.Fprintf(deps.Stdout, "  -> [DRY RUN] Would execute 'git pull' in %s\n", local)
			} else {
				fmt.Fprintf(deps.Stdout, "  -> [DRY RUN] Would clone %s to %s\n", url, local)
			}
		}
		fmt.Fprintln(deps.Stderr, "\nFinished repository sync!")
		return nil
	}

	// Build the job list, filtering empty-URL projects (defense from 001).
	jobs := make([]Job, 0, len(projects))
	for _, prj := range projects {
		url := prj.SSHURLToRepo
		if cfg.HTTP {
			url = prj.HTTPURLToRepo
		}
		if url == "" {
			fmt.Fprintf(deps.Stderr, "  -> Skipping %s: clone URL is empty\n", prj.PathWithNamespace)
			continue
		}
		local := filepath.Join(".", paths.Local(prj.PathWithNamespace, cfg.RootPath))
		jobs = append(jobs, Job{
			NamespacePath: prj.PathWithNamespace,
			LocalDir:      local,
			CloneURL:      url,
			Exists:        deps.Exists(local),
		})
	}

	effectiveJobs := req.Jobs
	if effectiveJobs < 1 {
		effectiveJobs = 1
	}

	pool := newPool(ctx, deps.Runner, deps.Stderr, effectiveJobs)
	for _, job := range jobs {
		if err := pool.Submit(job); err != nil {
			break // context cancelled; stop dispatching
		}
	}
	poolErr := pool.Wait()

	fmt.Fprintln(deps.Stderr, "\nFinished repository sync!")
	return poolErr
}
