# gitty

A minimal, configurable Go CLI tool to synchronize (clone/pull) GitLab groups, subgroups, and repositories directly to your local machine.

`gitty` uses a local `gitty.toml` configuration file to anchor your workspace, preserving the exact namespace directory structure of your GitLab environment to prevent naming collisions.

## Features
* **Workspace Config**: Initialize a workspace with `gitty init` so you don't have to repeatedly pass your GitLab URL or SSH/HTTP preferences.
* **Granular Syncing**: Choose to sync only repositories, only empty group directory structures, or both.
* **Recursive or Flat**: Sync only the immediate group, or use the `--nested` flag to recursively pull everything underneath it.
* **Smart Updates**: Automatically runs `git pull` if the local directory exists, or `git clone` if it doesn't.
* **Dry Runs**: Test your sync commands safely with `--dry-run` to see exactly what folders will be created and which repos will be cloned.
* **CI/CD Ready**: Automatically detects `GITLAB_TOKEN` or `CI_JOB_TOKEN` environment variables.

---

## Installation

Ensure you have Go installed, then clone this repository and build the binary:

```bash
# Initialize module and download dependencies
go mod tidy

# Build the executable
go build -o gitty main.go

# (Optional) Install globally
sudo mv gitty /usr/local/bin/
```

---

## Configuration (`gitty init`)

Before syncing, you need to initialize your workspace. Navigate to the root folder where you want your GitLab directory structure to live and run:

```bash
gitty init [flags]
```

### Init Flags
| Flag | Default | Description |
| :--- | :--- | :--- |
| `--url` | `https://gitlab.com` | The base URL of your GitLab instance (change this if using self-hosted GitLab). |
| `--http` | `false` | Use HTTP(S) for cloning (`https://...`) instead of the default SSH (`git@...`). |

**Example:**
```bash
cd ~/my-workspace
gitty init --url="https://gitlab.mycompany.com" --http
```
This generates a `gitty.toml` file in the current directory. `gitty` will use this directory as the root destination for all future sync commands.

---

## Usage (`gitty sync`)

Once your workspace is initialized, you can pull down your groups and repositories. 

```bash
gitty sync --group="your/gitlab/group/path" [flags]
```

### Sync Flags
| Flag | Default | Description |
| :--- | :--- | :--- |
| `--group` | `""` | **(Required)** The GitLab group or subgroup path (e.g., `tenant/images`). |
| `--token` | `""` | Your GitLab Access Token. Falls back to `GITLAB_TOKEN` or `CI_JOB_TOKEN` env vars. |
| `--groups` | `false` | Only fetch groups/subgroups and create their directory structure locally. |
| `--repos` | `false` | Only fetch and clone/pull repositories. *(Note: If neither `--groups` nor `--repos` is passed, it defaults to `--repos`)*. |
| `--nested` | `false` | Include nested subgroups and projects recursively. |
| `--dry-run`| `false` | Print what would happen without creating directories or executing git commands. |

### How `--groups` and `--repos` work together:
* `gitty sync --group="tenant"`: Syncs **only** the immediate repositories inside `tenant`.
* `gitty sync --group="tenant" --groups`: Creates **only** the empty directory structure for the `tenant` group and its immediate subgroups.
* `gitty sync --group="tenant" --groups --repos`: Creates the empty directory structure for subgroups, **and** syncs the immediate repositories.

---

## Examples

### 1. Standard Sync (Flat)
Sync all repositories directly inside `tenant/images` (does not pull repos inside nested subgroups).
```bash
export GITLAB_TOKEN="glpat-YOUR_PERSONAL_TOKEN"

gitty sync --group="tenant/images"
```

### 2. Full Recursive Sync (Nested)
Sync **everything** (all repositories in the group and all repositories in every subgroup beneath it).
```bash
gitty sync --group="tenant/images" --nested
```

### 3. Recreate Group Hierarchy
Only create the folder structure for all subgroups beneath `engineering`, leaving them empty.
```bash
gitty sync --group="engineering" --groups --nested
```

### 4. Dry Run
Safely check what repositories would be downloaded recursively before actually doing it.
```bash
gitty sync --group="tenant/images" --nested --dry-run
```

### 5. GitLab CI/CD Pipeline
`gitty` will automatically pick up the ephemeral `CI_JOB_TOKEN`. Just ensure you configured your workspace to use HTTP during the `init` step, as CI runners typically can't use SSH.
```yaml
stages:
  - sync

clone_all_repos:
  stage: sync
  image: golang:latest
  script:
    - go build -o gitty main.go
    - ./gitty init --http
    - ./gitty sync --group="tenant/images" --nested
```

## TODO

- [ ] For gitlab pipelines, use the CI_ var for the git repo
- [ ] Async pull down repos
- [ ] Show repos current branches and if they are out of date, maybe a cache
- [ ] Show how many projects are in each group
- [ ] Show the current groups/projects and which will be removed or added when doing a subsequent run