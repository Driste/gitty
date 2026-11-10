package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitty/internal/contract"
)

// scanLines runs the CI extractor over a file's text as project acme/app.
func scanLines(t *testing.T, r *resolver, file, text string) ([]contract.Link, []contract.Unresolved) {
	t.Helper()
	c := newCollector("acme/app", r)
	c.file = file
	lines := strings.Split(text, "\n")
	switch classifyFile(file) {
	case classCI:
		c.scanCI(lines, ciVariables("acme/app", "https://gitlab.example.com", r.registry))
	case classManifest:
		c.scanTokens(lines, nil)
	case classImages:
		c.scanImages(lines)
	case classSubmodules:
		c.scanSubmodules(lines)
	case classDocs:
		c.scanMentions(lines)
	default:
		t.Fatalf("%s is not a file the scanner reads", file)
	}
	return c.result()
}

// linkSet renders links as "kind to evidence" lines for comparison.
func linkSet(links []contract.Link) string {
	var out []string
	for _, l := range links {
		out = append(out, l.Kind+" "+l.To+" "+l.Evidence)
	}
	return strings.Join(out, "\n")
}

func unresolvedSet(us []contract.Unresolved) string {
	var out []string
	for _, u := range us {
		out = append(out, u.Kind+" "+u.Ref+" "+u.Evidence)
	}
	return strings.Join(out, "\n")
}

const ciFixture = `# The pipeline of acme/app.
include:
  - project: 'acme/ci-templates'
    ref: main
    file: '/templates/build.yml'
  - project: $CI_PROJECT_NAMESPACE/lib   # the group's own library
    file: x.yml
  - component: $CI_SERVER_FQDN/acme/components/sast@1.0
  - remote: 'https://gitlab.example.com/acme/platform/svc/-/raw/main/ci.yml'
  - local: /ci/local.yml
  - template: Jobs/Build.gitlab-ci.yml
  - 'https://gitlab.example.com/acme/lib-tools/-/raw/main/t.yml'
  - project: $SOME_GROUP/thing
    file: y.yml

variables:
  UPSTREAM: acme/never
  project: acme/ops/release

build:
  needs:
    - project: acme/gone
      job: package
      ref: main
      artifacts: true
  script:
    - git clone https://gitlab.example.com/acme/app-data.git data
    - echo "project: acme/ops/release"
    - |
      cat <<EOF
      trigger: acme/ops/release
      EOF

deploy:
  trigger:
    project: acme/ops/deploy
    branch: main

child:
  trigger:
    include:
      - project: acme/ci-templates
        file: child.yml
`

func TestScanCI(t *testing.T) {
	r := testResolver(t, "https://gitlab.example.com")
	links, unresolved := scanLines(t, r, ".gitlab-ci.yml", ciFixture)

	want := strings.Join([]string{
		"depends acme/app-data .gitlab-ci.yml:27",
		"ci-include acme/ci-templates .gitlab-ci.yml:3",
		"ci-component acme/components .gitlab-ci.yml:8",
		"ci-include acme/lib .gitlab-ci.yml:6",
		"ci-include acme/lib-tools .gitlab-ci.yml:12",
		"ci-trigger acme/ops/deploy .gitlab-ci.yml:36",
		"ci-include acme/platform/svc .gitlab-ci.yml:9",
	}, "\n")
	if got := linkSet(links); got != want {
		t.Errorf("links:\n%s\n--- want:\n%s", got, want)
	}
	// acme/ops/release appears three times and is never a link: as a
	// variable's value, inside a script, and inside a heredoc.
	if wantU := "depends acme/gone .gitlab-ci.yml:22"; unresolvedSet(unresolved) != wantU {
		t.Errorf("unresolved:\n%s\n--- want:\n%s", unresolvedSet(unresolved), wantU)
	}
}

func TestScanCIShapes(t *testing.T) {
	r := testResolver(t, "https://gitlab.example.com")
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "a list indented no further than its key",
			text: "include:\n- project: acme/ci-templates\n  file: a.yml\n- project: acme/lib\n  file: b.yml\nbuild:\n  script: make\n",
			want: "ci-include acme/ci-templates ci.gitlab-ci.yml:2\nci-include acme/lib ci.gitlab-ci.yml:4",
		},
		{
			name: "the short trigger form",
			text: "downstream:\n  trigger: acme/ops/release\n",
			want: "ci-trigger acme/ops/release ci.gitlab-ci.yml:2",
		},
		{
			name: "a single include given as a URL",
			text: "include: 'https://gitlab.example.com/acme/ci-templates/-/raw/main/all.yml'\n",
			want: "ci-include acme/ci-templates ci.gitlab-ci.yml:1",
		},
		{
			name: "a project key outside include, trigger and needs means nothing",
			text: "release:\n  project: acme/lib\n  environment:\n    project: acme/ops/deploy\n",
			want: "",
		},
		{
			name: "the include block ends where the next key starts",
			text: "include:\n  - local: a.yml\nstages:\n  - project: acme/lib\n",
			want: "",
		},
		{
			name: "windows line endings",
			text: "include:\r\n  - project: acme/lib\r\n    file: a.yml\r\n",
			want: "ci-include acme/lib ci.gitlab-ci.yml:2",
		},
		{
			name: "a project's own path is not a link",
			text: "include:\n  - project: $CI_PROJECT_PATH\n    file: a.yml\n  - project: acme/app\n    file: b.yml\n",
			want: "",
		},
		{
			name: "a component of the public catalogue is somebody else's",
			text: "include:\n  - component: gitlab.com/components/sast/sast@1.0\n",
			want: "",
		},
		{
			name: "one reference on one line is reported once",
			text: "include:\n  - component: gitlab.example.com/acme/components/sast@1.0\n  - remote: https://gitlab.example.com/acme/lib/-/raw/main/a.yml\n",
			want: "ci-component acme/components ci.gitlab-ci.yml:2\nci-include acme/lib ci.gitlab-ci.yml:3",
		},
		{
			name: "the same target in two ways is two links",
			text: "include:\n  - project: acme/lib\n    file: a.yml\nx:\n  trigger:\n    project: acme/lib\n",
			want: "ci-include acme/lib ci.gitlab-ci.yml:2\nci-trigger acme/lib ci.gitlab-ci.yml:6",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			links, unresolved := scanLines(t, r, "ci.gitlab-ci.yml", tt.text)
			if got := linkSet(links); got != tt.want {
				t.Errorf("links:\n%s\n--- want:\n%s", got, tt.want)
			}
			if len(unresolved) != 0 {
				t.Errorf("unexpected unresolved: %s", unresolvedSet(unresolved))
			}
		})
	}
}

func TestScanManifests(t *testing.T) {
	r := testResolver(t, "https://gitlab.example.com")
	tests := []struct {
		file string
		text string
		want string
		unre string
	}{
		{
			file: "go.mod",
			text: "module gitlab.example.com/acme/app\n\ngo 1.24\n\nrequire (\n\tgitlab.example.com/acme/lib/v2 v2.3.0\n\tgithub.com/pelletier/go-toml/v2 v2.2.0\n\tgitlab.example.com/acme/gone v0.1.0\n)\n\nreplace gitlab.example.com/acme/lib/v2 => ../lib\n",
			want: "depends acme/lib go.mod:6", // its own module line is not a link; the first evidence is kept
			unre: "depends acme/gone go.mod:8",
		},
		{
			file: "package.json",
			text: "{\n  \"dependencies\": {\n    \"lib\": \"git+https://gitlab.example.com/acme/lib.git#semver:^1.2\",\n    \"svc\": \"git+ssh://git@gitlab.example.com:acme/platform/svc.git\",\n    \"left-pad\": \"^1.3.0\"\n  }\n}\n",
			want: "depends acme/lib package.json:3\ndepends acme/platform/svc package.json:4",
		},
		{
			file: "requirements-dev.txt",
			text: "requests==2.31\ngit+https://gitlab.example.com/acme/lib.git@v1.2#egg=lib\n--extra-index-url https://gitlab.example.com/api/v4/projects/5/packages/pypi/simple\n",
			want: "depends acme/components requirements-dev.txt:3\ndepends acme/lib requirements-dev.txt:2",
		},
		{
			file: "infra/main.tf",
			text: "module \"net\" {\n  source = \"git::https://gitlab.example.com/acme/platform/svc.git//modules/net?ref=v3\"\n}\n",
			want: "depends acme/platform/svc infra/main.tf:2",
		},
		{
			file: ".npmrc",
			text: "@acme:registry=https://gitlab.example.com/api/v4/projects/1/packages/npm/\n",
			want: "depends acme/lib .npmrc:1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			links, unresolved := scanLines(t, r, tt.file, tt.text)
			if got := linkSet(links); got != tt.want {
				t.Errorf("links:\n%s\n--- want:\n%s", got, tt.want)
			}
			if got := unresolvedSet(unresolved); got != tt.unre {
				t.Errorf("unresolved:\n%s\n--- want:\n%s", got, tt.unre)
			}
		})
	}
}

// A reference that resolves to nothing is reported once, as the project it
// names — not a second time as the longer path it was written with.
func TestScanCIUnresolvedOnce(t *testing.T) {
	r := testResolver(t, "https://gitlab.example.com")
	text := "include:\n  - component: $CI_SERVER_FQDN/catalog/sast/scan@1.0\n  - project: acme/missing\n    file: a.yml\n" +
		"job:\n  script:\n    - git clone https://gitlab.example.com/acme/a.git && git clone https://gitlab.example.com/acme/b.git\n"
	links, unresolved := scanLines(t, r, ".gitlab-ci.yml", text)
	if len(links) != 0 {
		t.Errorf("links: %s", linkSet(links))
	}
	want := "depends acme/a .gitlab-ci.yml:7\ndepends acme/b .gitlab-ci.yml:7\nci-include acme/missing .gitlab-ci.yml:3\nci-component catalog/sast .gitlab-ci.yml:2"
	if got := unresolvedSet(unresolved); got != want {
		t.Errorf("unresolved:\n%s\n--- want:\n%s", got, want)
	}
}

// targetSet renders links with what they point at, for the tests of that.
func targetSet(links []contract.Link) string {
	var out []string
	for _, l := range links {
		out = append(out, l.Kind+" "+l.To+" -> "+l.Target)
	}
	return strings.Join(out, "\n")
}

// TestScanTargets: a link says not only that a project is referred to but,
// where the reference says so, what in it — which is the file to open.
func TestScanTargets(t *testing.T) {
	r := testResolver(t, "https://gitlab.example.com")
	ci := strings.Join([]string{
		"include:",
		"  - project: acme/ci-templates",
		"    ref: main",
		"    file: '/templates/build.yml'",
		"  - project: acme/lib",
		"    file:",
		"      - /ci/test.yml",
		"      - ci/lint.yml # with a comment",
		"  - project: acme/ops/deploy",
		"    file: [ '/a.yml', \"/b.yml\" ]",
		"  - project: acme/ci-templates", // the same project again: one link, both files
		"    file: /templates/deploy.yml",
		"  - project: acme/ops/release",
		"    file: /$PIPELINE.yml", // a variable gitty cannot know
		"  - component: $CI_SERVER_FQDN/acme/components/sast@1.0",
		"  - remote: 'https://gitlab.example.com/acme/platform/svc/-/raw/main/ci/shared.yml'",
		"  - local: /ci/own.yml",
		"build:",
		"  file: not-an-include.yml", // a key named file somewhere else
		"  trigger:",
		"    project: acme/app-data",
		"    file: ignored.yml",
		"",
	}, "\n")
	links, _ := scanLines(t, r, ".gitlab-ci.yml", ci)
	want := strings.Join([]string{
		"ci-trigger acme/app-data -> ",
		"ci-include acme/ci-templates -> templates/build.yml, templates/deploy.yml",
		"ci-component acme/components -> sast",
		"ci-include acme/lib -> ci/test.yml, ci/lint.yml",
		"ci-include acme/ops/deploy -> a.yml, b.yml",
		"ci-include acme/ops/release -> ",
		"ci-include acme/platform/svc -> ci/shared.yml",
	}, "\n")
	if got := targetSet(links); got != want {
		t.Errorf("targets:\n%s\n--- want:\n%s", got, want)
	}

	tf, _ := scanLines(t, r, "main.tf", "source = \"git::https://gitlab.example.com/acme/platform/svc.git//modules/net?ref=v3\"\nsource = \"git::https://gitlab.example.com/acme/lib.git\"\n")
	if got, want := targetSet(tf), "depends acme/lib -> \ndepends acme/platform/svc -> modules/net"; got != want {
		t.Errorf("terraform targets:\n%s\n--- want:\n%s", got, want)
	}
}

func TestScanImages(t *testing.T) {
	r := testResolver(t, "https://gitlab.example.com") // registry.example.com:5050
	tests := []struct {
		name string
		file string
		text string
		want string
		unre string
	}{
		{
			name: "a Dockerfile",
			file: "Dockerfile",
			text: strings.Join([]string{
				"ARG BASE=registry.example.com:5050/acme/lib/base:1.2",
				"FROM registry.example.com:5050/acme/lib/base:1.2 AS build",
				"FROM --platform=linux/amd64 registry.example.com:5050/acme/platform/svc@sha256:0123abcd",
				"FROM golang:1.24",
				"FROM registry.example.com:5050/acme/gone/x:1",
				"FROM docker.io/library/alpine:3",
				"RUN git clone https://gitlab.example.com/acme/app-data.git /data",
				"FROM registry.example.com:5050/acme/dependency_proxy/containers/alpine:3",
				"FROM registry.example.com:5050/acme/vendor/mirror:1",
				"FROM notregistry.example.com:5050/acme/ops/deploy:1",
				"",
			}, "\n"),
			// The image of a project is the project; a sub-image name and a
			// tag or digest are not part of its path. The first evidence wins.
			want: "depends acme/app-data Dockerfile:7\nimage acme/lib Dockerfile:1\nimage acme/platform/svc Dockerfile:3",
			unre: "image acme/gone/x Dockerfile:5",
		},
		{
			name: "a pipeline: image, services, and a pull in a script",
			file: ".gitlab-ci.yml",
			text: strings.Join([]string{
				"default:",
				"  image: $CI_REGISTRY/acme/lib/base:1.2",
				"test:",
				"  image: ${CI_REGISTRY_IMAGE}/ci:latest", // its own image
				"  services:",
				"    - name: registry.example.com:5050/acme/components/db:15",
				"      alias: db",
				"    - registry.example.com:5050/acme/platform/svc:stable",
				"  script:",
				"    - docker pull $CI_REGISTRY/acme/ops/deploy/tool:1",
				"",
			}, "\n"),
			want: "image acme/components .gitlab-ci.yml:6\nimage acme/lib .gitlab-ci.yml:2\nimage acme/ops/deploy .gitlab-ci.yml:10\nimage acme/platform/svc .gitlab-ci.yml:8",
		},
		{
			name: "a compose file",
			file: "deploy/docker-compose.prod.yml",
			text: "services:\n  api:\n    image: registry.example.com:5050/acme/lib:2\n  db:\n    image: postgres:16\n",
			want: "image acme/lib deploy/docker-compose.prod.yml:3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			links, unresolved := scanLines(t, r, tt.file, tt.text)
			if got := linkSet(links); got != tt.want {
				t.Errorf("links:\n%s\n--- want:\n%s", got, tt.want)
			}
			if got := unresolvedSet(unresolved); got != tt.unre {
				t.Errorf("unresolved:\n%s\n--- want:\n%s", got, tt.unre)
			}
		})
	}

	t.Run("before the registry is known, its conventional name is tried", func(t *testing.T) {
		idx := newIndex(&Config{URL: "https://gitlab.example.com"})
		idx.Projects["acme/lib"] = &contract.Project{ID: 1}
		guess := newResolver(idx, &Config{URL: "https://gitlab.example.com"})
		links, _ := scanLines(t, guess, "Dockerfile", "FROM registry.gitlab.example.com/acme/lib:1\nFROM registry.example.com:5050/acme/lib:1\n")
		if got := linkSet(links); got != "image acme/lib Dockerfile:1" {
			t.Errorf("links: %s", got)
		}
		// An instance addressed by number has no name to derive one from.
		byIP := newResolver(idx, &Config{URL: "http://10.0.0.5:8080"})
		if byIP.registry != "" {
			t.Errorf("registry guessed for an IP address: %q", byIP.registry)
		}
	})
}

func TestScanSubmodules(t *testing.T) {
	r := testResolver(t, "https://gitlab.example.com")
	text := strings.Join([]string{
		`[submodule "lib"]`,
		"\tpath = vendor/lib",
		"\turl = ../lib.git", // relative to this project's own URL: acme/app → acme/lib
		`[submodule "svc"]`,
		"\tpath = svc",
		"\turl = https://gitlab.example.com/acme/platform/svc.git",
		`[submodule "elsewhere"]`,
		"\turl = git@github.com:someone/thing.git",
		`[submodule "gone"]`,
		"\turl = ../../other/missing.git",
		`[submodule "climbs"]`,
		"\turl = ../../../../etc/passwd",
		`[submodule "same-group"]`,
		"\turl = ./deploy",
		"",
	}, "\n")
	links, unresolved := scanLines(t, r, ".gitmodules", text)
	if got, want := linkSet(links), "submodule acme/lib .gitmodules:3\nsubmodule acme/platform/svc .gitmodules:6"; got != want {
		t.Errorf("links:\n%s\n--- want:\n%s", got, want)
	}
	// "./deploy" from acme/app is acme/app/deploy: a path, but no project.
	if got, want := unresolvedSet(unresolved), "submodule acme/app/deploy .gitmodules:14\nsubmodule other/missing .gitmodules:10"; got != want {
		t.Errorf("unresolved:\n%s\n--- want:\n%s", got, want)
	}
}

func TestScanMentions(t *testing.T) {
	r := testResolver(t, "https://gitlab.example.com")
	text := strings.Join([]string{
		"# app",
		"",
		"Built on [lib](https://gitlab.example.com/acme/lib) and deployed by",
		"<https://gitlab.example.com/acme/ops/deploy/-/pipelines>. See also https://gitlab.example.com/acme/lib/-/issues/4.",
		"Our own issues: https://gitlab.example.com/acme/app/-/issues",
		"Something that is gone: https://gitlab.example.com/acme/gone",
		"And elsewhere: https://github.com/acme/lib",
		"",
	}, "\n")
	links, unresolved := scanLines(t, r, "README.md", text)
	// Two projects on one line are two mentions; a second mention of one
	// already mentioned adds nothing.
	if got, want := linkSet(links), "mentions acme/lib README.md:3\nmentions acme/ops/deploy README.md:4"; got != want {
		t.Errorf("links:\n%s\n--- want:\n%s", got, want)
	}
	// A link in prose that leads nowhere is not worth reporting.
	if len(unresolved) != 0 {
		t.Errorf("unresolved: %s", unresolvedSet(unresolved))
	}
}

func TestClassifyFile(t *testing.T) {
	for name, want := range map[string]fileClass{
		".gitlab-ci.yml":               classCI,
		".gitlab-ci.yaml":              classCI,
		"deploy/build.gitlab-ci.yml":   classCI,
		".gitlab/ci/test.yml":          classCI,
		".gitlab/ci/jobs/lint.yaml":    classCI,
		".gitlab/issue_templates/x.md": classNone,
		"docker-compose.yml":           classImages,
		"compose.prod.yaml":            classImages,
		"Dockerfile":                   classImages,
		"Dockerfile.dev":               classImages,
		"build/worker.dockerfile":      classImages,
		"Containerfile":                classImages,
		"dockerfiles.txt":              classNone,
		".gitmodules":                  classSubmodules,
		"sub/.gitmodules":              classNone, // git only reads the one at the root
		"README.md":                    classDocs,
		"AGENTS.md":                    classDocs,
		"docs/design/overview.md":      classDocs,
		"CHANGELOG.md":                 classNone, // every merge request it lists is not a relation
		"src/notes.md":                 classNone,
		"docs/api.yaml":                classNone,
		"go.mod":                       classManifest,
		"services/api/go.mod":          classManifest,
		"package.json":                 classManifest,
		"requirements.txt":             classManifest,
		"requirements-dev.txt":         classManifest,
		"infra/main.tf":                classManifest,
		"my.gemspec":                   classManifest,
		"Chart.yaml":                   classManifest,
		"package-lock.json":            classNone, // lockfiles restate the manifest, at length
		"go.sum":                       classNone,
		"main.go":                      classNone,
		// Other people's code, and fixtures.
		"node_modules/x/package.json": classNone,
		"vendor/acme/lib/go.mod":      classNone,
		"pkg/testdata/go.mod":         classNone,
		"third_party/x/Cargo.toml":    classNone,
	} {
		if got := classifyFile(name); got != want {
			t.Errorf("classifyFile(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestPickDocs(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{
			name: "agent instructions first, the README last, noise left out",
			in:   []string{"README.md", "LICENSE", "CHANGELOG.md", "AGENTS.md", "CONTRIBUTING.md", "NOTES.md", "main.go", "Makefile", "SECURITY.md"},
			want: "AGENTS.md CONTRIBUTING.md NOTES.md README.md",
		},
		{
			name: "a README in another format still counts",
			in:   []string{"README.rst", "setup.py"},
			want: "README.rst",
		},
		{
			name: "more than fit: the README is the one that stays",
			in:   []string{"A.md", "B.md", "C.md", "D.md", "E.md", "F.md", "G.md", "README.md"},
			want: "A.md B.md C.md D.md E.md README.md",
		},
		{name: "nothing worth reading", in: []string{"main.go", "LICENSE"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := strings.Join(pickDocs(tt.in), " "); got != tt.want {
				t.Errorf("pickDocs = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadmeSummary(t *testing.T) {
	tests := []struct {
		name   string
		readme string
		want   string
	}{
		{
			name:   "the first prose after title and badges",
			readme: "# lib\n\n[![pipeline](a.svg)](b) ![x](y)\n<p align=\"center\">logo</p>\n\nShared domain types,\nused by every service.\n\n## Install\n\nMore text.\n",
			want:   "Shared domain types, used by every service.",
		},
		{
			name:   "a setext title and a fenced block are skipped",
			readme: "lib\n===\n\n```\nmake\n```\n\nThe library.\n",
			want:   "The library.",
		},
		{
			name:   "long openings are cut on a word",
			readme: "# x\n\n" + strings.Repeat("word ", 100) + "\n",
			want:   strings.TrimSpace(strings.Repeat("word ", 60)) + "…",
		},
		{name: "only structure", readme: "# title\n\n## section\n", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(tt.readme), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := readmeSummary(dir, []string{"README.md"}); got != tt.want {
				t.Errorf("summary = %q, want %q", got, tt.want)
			}
		})
	}
	if got := readmeSummary(t.TempDir(), []string{"main.go"}); got != "" {
		t.Errorf("summary without a README = %q", got)
	}
}

// writeTree creates files under dir and stages them, so git lists them as
// tracked without a commit being needed.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "init", "-q", "-b", "main", ".")
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", "-A")
}

// TestScanCheckout reads a real git checkout: only tracked files of the
// right names are opened, and what is untracked, vendored, oversized, binary
// or a symlink is left alone.
func TestScanCheckout(t *testing.T) {
	const instance = "https://gitlab.example.com"
	r := testResolver(t, instance)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		".gitlab-ci.yml":            "include:\n  - project: acme/ci-templates\n    file: a.yml\n",
		"go.mod":                    "module gitlab.example.com/acme/app\n\nrequire gitlab.example.com/acme/lib v1.0.0\n",
		"vendor/x/go.mod":           "module x\n\nrequire gitlab.example.com/acme/platform/svc v1.0.0\n",
		"docs/notes.md":             "See https://gitlab.example.com/acme/ops/deploy for deployment.\n",
		"main.go":                   "package main // gitlab.example.com/acme/ops/release\n",
		"README.md":                 "# app\n\nThe app.\n",
		"AGENTS.md":                 "Use make.\n",
		"services/api/package.json": "{\"dependencies\": {\"c\": \"git+https://gitlab.example.com/acme/components.git\"}}\n",
	})
	// Tracked, but not something to read.
	big := strings.Repeat("gitlab.example.com/acme/ops/deploy\n", maxScanFileSize/30)
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "setup.cfg"), []byte("gitlab.example.com/acme/ops/deploy\x00binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "docs", "notes.md"), filepath.Join(dir, "Pipfile")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	gitRun(t, dir, "add", "-A")
	// Present on disk, never added: not the project's.
	if err := os.WriteFile(filepath.Join(dir, "Gemfile"), []byte("gem 'x', git: 'https://gitlab.example.com/acme/ops/release.git'\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sc := &scanner{git: execGit, r: r, instance: instance}
	out, err := sc.scan(context.Background(), dir, "acme/app", true)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"ci-include acme/ci-templates .gitlab-ci.yml:2",
		"depends acme/components services/api/package.json:1",
		"depends acme/lib go.mod:3",
		"mentions acme/ops/deploy docs/notes.md:1",
	}, "\n")
	if got := linkSet(out.Links); got != want {
		t.Errorf("links:\n%s\n--- want:\n%s", got, want)
	}
	if len(out.Unresolved) != 0 {
		t.Errorf("unexpected unresolved: %s", unresolvedSet(out.Unresolved))
	}
	if got := strings.Join(out.Docs, " "); got != "AGENTS.md README.md" {
		t.Errorf("docs = %q", got)
	}
	if out.Summary != "The app." {
		t.Errorf("summary = %q", out.Summary)
	}
	if out.Files != 6 {
		t.Errorf("files read = %d, want 6 (.gitlab-ci.yml, go.mod, services/api/package.json and three documents)", out.Files)
	}

	// An archived project is described, but not asked what it uses.
	out, err = sc.scan(context.Background(), dir, "acme/app", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Links) != 0 || out.Files != 0 || out.Summary != "The app." || len(out.Docs) != 2 {
		t.Errorf("docs-only scan = %+v", out)
	}

	// Not a repository: an error, not an empty answer.
	if _, err := sc.scan(context.Background(), t.TempDir(), "acme/app", true); err == nil {
		t.Error("scanning a directory that is not a repository should fail")
	}
}

func TestExpandVars(t *testing.T) {
	vars := ciVariables("acme/platform/svc", "https://gitlab.example.com:8443/", "registry.example.com")
	for in, want := range map[string]string{
		"$CI_PROJECT_NAMESPACE/lib":           "acme/platform/lib",
		"${CI_PROJECT_ROOT_NAMESPACE}/shared": "acme/shared",
		"$CI_PROJECT_PATH":                    "acme/platform/svc",
		"$CI_SERVER_HOST/acme/x":              "gitlab.example.com/acme/x",
		"$CI_SERVER_FQDN/acme/x":              "gitlab.example.com:8443/acme/x",
		"$CI_SERVER_URL/acme/x":               "https://gitlab.example.com:8443/acme/x",
		"$UNKNOWN/lib":                        "$UNKNOWN/lib",
		"no variables":                        "no variables",
		"$CI_PROJECT_NAMESPACEX/lib":          "$CI_PROJECT_NAMESPACEX/lib",
	} {
		if got := expandVars(in, vars); got != want {
			t.Errorf("expandVars(%q) = %q, want %q", in, got, want)
		}
	}
}
