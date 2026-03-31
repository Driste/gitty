# Gitty

A lightweight Go CLI tool to recursively clone or update (pull) all repositories within a specific GitLab group or subgroup. 

This tool uses the official GitLab `client-go` API to automatically handle pagination and fetches all projects under a specified group path. It preserves the original GitLab namespace directory structure locally, preventing naming collisions between subgroups.

## Features
* **Recursive Cloning**: Fetches all projects in a group, including those in subgroups.
* **Smart Updates**: If the local directory already exists, it runs `git pull` instead of `git clone`.
* **Namespace Preservation**: Mirrors the GitLab path structure locally (e.g., `./dest/group-1/group-1a/my-repo`).
* **CI/CD Ready**: Automatically detects `GITLAB_TOKEN` or `CI_JOB_TOKEN` environment variables when running in GitLab pipelines.

## Installation

Ensure you have Go installed, then clone this repository and build the binary:

```bash
# Initialize module and download dependencies
go mod tidy

# Build the executable
go build -o gitty main.go
```

## Usage
```bash
./gitlab-cloner --group="your/gitlab/group/path" [flags]
```

## Available Flags
| Flag | Default | Description |
| ---- | ---- | ---- |
| `--group` | `""` | (Required) The GitLab group or subgroup path (e.g., `group/nested-group`).
| `--token` | `` | Your GitLab Access Token. Falls back to GITLAB_TOKEN or CI_JOB_TOKEN environment variables if omitted. |
| `--url` | `https://gitlab.com` | The base URL of your GitLab instance (change this if using self-hosted GitLab). |
| `--dest` | `./` | The local directory where the repository structure will be created. |
| `--http` | `false` | Use HTTP(S) for cloning (https://...) instead of the default SSH (git@...). |
| `--dry-run` | `false` | Print what would happen (clone or pull) without actually executing git commands or creating directories. |

### Dry Run
If you want to safely check which repositories will be fetched and whether they will be cloned or pulled, use the `--dry-run` flag:

```bash
./gitty --group="group/images" --dry-run
```