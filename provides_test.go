package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gitty/internal/contract"
)

// writeFiles lays files out under dir, for the detectors that are given a
// list of names and read what they need.
func writeFiles(t *testing.T, dir string, files map[string]string) []string {
	t.Helper()
	var names []string
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func providedSet(ps []contract.Provided) string {
	var out []string
	for _, p := range ps {
		line := fmt.Sprintf("%s %s @%s", p.Kind, p.Name, p.File)
		if len(p.Inputs) > 0 {
			line += " (" + strings.Join(p.Inputs, ", ") + ")"
		}
		if p.About != "" {
			line += " — " + p.About
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestProvides(t *testing.T) {
	dir := t.TempDir()
	names := writeFiles(t, dir, map[string]string{
		// A component: the header is its interface.
		"templates/build.yml": `spec:
  description: Builds and pushes a container image.
  inputs:
    image_name:
      description: What to call it
    context:
      default: .
    stage:
      default: build
      options: [build, release]
---
build:
  stage: $[[ inputs.stage ]]
  script: [make image]
`,
		// The directory form.
		"templates/scan/template.yml": "spec:\n  inputs:\n    severity:\n---\nscan:\n  script: [scan]\n",
		// Beside a component's template, not a component.
		"templates/scan/helpers.yml": ".helper:\n  script: [x]\n",
		// A template to include by file: no header.
		"templates/lint.yaml": "lint:\n  script: [lint]\n",
		// Not a pipeline at all.
		"templates/deployment.yml": "apiVersion: apps/v1\nkind: Deployment\n",
		// Deeper than GitLab looks.
		"ci/templates/other.yml": "other:\n  script: [x]\n",
		".gitlab/security-policies/policy.yml": `---
scan_execution_policy:
- name: Enforce SAST
  description: >-
    Runs SAST
  enabled: true
  rules:
  - type: pipeline
    branches: ['*']
  actions:
  - scan: sast
- description: Named second
  name: Secret detection on main
  enabled: true
pipeline_execution_policy:
  - name: "Signing: every image"
    description: 'Adds a signing job to every pipeline'
    content:
      include:
        - project: acme/platform/policy-ci
          file: sign.yml
approval_policy: []
unrelated:
- name: not a policy
`,
		"charts/web/Chart.yaml":       "apiVersion: v2\nname: web\ndescription: The storefront\nversion: 1.2.3\n",
		"charts/web/templates/a.yaml": "apiVersion: v1\nkind: ConfigMap\n",
		"Dockerfile":                  "FROM scratch\n",
		"docker/Dockerfile":           "FROM scratch\n",
		"main.tf":                     "resource \"x\" \"y\" {}\n",
		"variables.tf":                "variable \"region\" {\n  type = string\n}\nvariable \"name\" {}\n",
		"modules/vpc/main.tf":         "variable \"cidr\" {}\n",
		"modules/vpc/outputs.tf":      "output \"id\" {}\n",
		"modules/vpc/examples/x.tf":   "variable \"ignored\" {}\n",
		"vendor/templates/x.yml":      "x:\n  script: [x]\n",
		"node_modules/a/Chart.yaml":   "name: vendored\n",
	})
	want := strings.Join([]string{
		"ci-component build @templates/build.yml (image_name, context, stage) — Builds and pushes a container image.",
		"ci-component scan @templates/scan/template.yml (severity)",
		"ci-template lint @templates/lint.yaml",
		"helm-chart web @charts/web/Chart.yaml — The storefront",
		"image image @Dockerfile",
		"policy Enforce SAST @.gitlab/security-policies/policy.yml — scan execution policy: Runs SAST",
		"policy Secret detection on main @.gitlab/security-policies/policy.yml — scan execution policy: Named second",
		"policy Signing: every image @.gitlab/security-policies/policy.yml — pipeline execution policy: Adds a signing job to every pipeline",
		"terraform-module module @. (region, name)",
		"terraform-module vpc @modules/vpc (cidr)",
	}, "\n")
	if got := providedSet(provides(dir, names)); got != want {
		t.Errorf("provides:\n%s\n--- want:\n%s", got, want)
	}
}

// A chart's templates are manifests, and a root that configures a backend
// is infrastructure, not a module.
func TestProvidesIsNotFooled(t *testing.T) {
	dir := t.TempDir()
	names := writeFiles(t, dir, map[string]string{
		"Chart.yaml":           "name: app\n",
		"templates/deploy.yml": "{{- if .Values.enabled }}\nkind: Deployment\n{{- end }}\n",
		"templates/ci.yml":     "job:\n  script: [x]\n",
		"backend.tf":           "terraform {\n  backend \"s3\" {\n    bucket = \"state\"\n  }\n}\n",
		"variables.tf":         "variable \"region\" {}\n",
		"modules/db/main.tf":   "variable \"size\" {}\n",
	})
	want := "helm-chart app @Chart.yaml\nterraform-module db @modules/db (size)"
	if got := providedSet(provides(dir, names)); got != want {
		t.Errorf("provides:\n%s\n--- want:\n%s", got, want)
	}
	if got := provides(t.TempDir(), []string{"templates/missing.yml", ""}); len(got) != 0 {
		t.Errorf("a file that cannot be read provides nothing, got %v", got)
	}
}

func TestClassifyDeploymentConfiguration(t *testing.T) {
	for name, want := range map[string]fileClass{
		"values.yaml":                        classDeploy,
		"values-production.yaml":             classDeploy,
		"envs/prod/values.yml":               classDeploy,
		"apps/web/prod.values.yaml":          classDeploy,
		"overlays/prod/kustomization.yaml":   classDeploy,
		"helmfile.yaml":                      classDeploy,
		".gitlab/agents/prod/config.yaml":    classAgent,
		".gitlab/agents/prod/other.yaml":     classNone,
		"config.yaml":                        classNone,
		"deployment.yaml":                    classNone, // not by name alone
		"node_modules/x/values.yaml":         classNone,
		"docker-compose.values.yaml":         classImages, // a compose file first
		".gitlab/ci/values.gitlab-ci.yaml":   classCI,
		"valuesets.md":                       classDocs, // documentation, whatever it is called
		"charts/web/values.schema.json":      classNone,
		"charts/web/templates/values.txt":    classNone,
		"environments/values.staging.yaml":   classDeploy,
		"kustomization.yml":                  classDeploy,
		"docs/kustomization.md":              classDocs,
		"helmfile.d/values.yaml.gotmpl":      classNone,
		"clusters/a/helmfile.production.yml": classDeploy,
	} {
		if got := classifyFile(name); got != want {
			t.Errorf("classifyFile(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestScanDeploy(t *testing.T) {
	r := testResolver(t, "https://gitlab.example.com")
	values := strings.Join([]string{
		"image:",
		"  repository: registry.example.com:5050/acme/lib-tools",
		"  tag: 1.4.2",
		"sidecar:",
		"  image: registry.example.com:5050/acme/platform/svc/proxy:2",
		"chart:",
		"  repo: https://gitlab.example.com/acme/ops/deploy.git",
		"other: docker.io/library/nginx:1",
		"gone: registry.example.com:5050/acme/missing:1",
		"",
	}, "\n")
	links, unresolved := scanLines(t, r, "envs/prod/values.yaml", values)
	want := "deploys acme/lib-tools envs/prod/values.yaml:2\ndeploys acme/ops/deploy envs/prod/values.yaml:7\ndeploys acme/platform/svc envs/prod/values.yaml:5"
	if got := linkSet(links); got != want {
		t.Errorf("links:\n%s\n--- want:\n%s", got, want)
	}
	if got, want := unresolvedSet(unresolved), "deploys acme/missing envs/prod/values.yaml:9"; got != want {
		t.Errorf("unresolved:\n%s\n--- want:\n%s", got, want)
	}

	agent := strings.Join([]string{
		"ci_access:",
		"  projects:",
		"    - id: acme/components",
		"    - id: acme/nope",
		"  groups:",
		"    - id: acme/platform",
		"user_access:",
		"  projects:",
		`    - id: "acme/lib"`,
		"",
	}, "\n")
	links, unresolved = scanLines(t, r, ".gitlab/agents/prod/config.yaml", agent)
	if got, want := linkSet(links), "deploys acme/components .gitlab/agents/prod/config.yaml:3\ndeploys acme/lib .gitlab/agents/prod/config.yaml:9"; got != want {
		t.Errorf("agent links:\n%s\n--- want:\n%s", got, want)
	}
	// A group is not a project: it is named, and nothing is made of it.
	if got, want := unresolvedSet(unresolved), "deploys acme/nope .gitlab/agents/prod/config.yaml:4\ndeploys acme/platform .gitlab/agents/prod/config.yaml:6"; got != want {
		t.Errorf("agent unresolved:\n%s\n--- want:\n%s", got, want)
	}
	// The same `id:` anywhere else is not a project.
	links, unresolved = scanLines(t, r, "values.yaml", "service:\n  - id: acme/lib\n")
	if len(links)+len(unresolved) != 0 {
		t.Errorf("an id outside an agent's config was read as a project: %v %v", links, unresolved)
	}
}
