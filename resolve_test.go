package main

import (
	"testing"

	"gitty/internal/contract"
)

// testResolver builds a resolver over a small instance:
//
//	acme/lib, acme/lib-tools, acme/app, acme/platform/svc, acme/components,
//	acme/ops/deploy, acme/ops/release, acme/ci-templates, acme/app-data
//	acme/vendor/mirror   excluded by the workspace
//	acme/scratch         a checkout with no project upstream
func testResolver(t *testing.T, instance string) *resolver {
	t.Helper()
	idx := newIndex(&Config{URL: instance})
	for i, p := range []string{
		"acme/lib", "acme/lib-tools", "acme/app", "acme/platform/svc",
		"acme/components", "acme/ops/deploy", "acme/ops/release",
		"acme/ci-templates", "acme/app-data", "acme/vendor/mirror",
	} {
		idx.Projects[p] = &contract.Project{ID: int64(i + 1)}
	}
	idx.Projects["acme/scratch"] = &contract.Project{Local: &contract.Local{Dir: "scratch"}}
	idx.Hosts = []string{"ssh.gitlab.example.com"}
	return newResolver(idx, &Config{
		URL:        instance,
		CloneHosts: []string{"git.internal.example"},
		Exclude:    []string{"acme/vendor"},
	})
}

func TestResolveTokens(t *testing.T) {
	r := testResolver(t, "https://gitlab.example.com")

	// Each line is something a manifest, a script or a document might hold.
	// project is what it must resolve to; unresolved what it must be
	// reported as when it names the instance but no known project; both
	// empty means it is not gitty's business.
	tests := []struct {
		line       string
		project    string
		unresolved string
	}{
		// The forms a repository is referred to by.
		{line: `https://gitlab.example.com/acme/lib.git`, project: "acme/lib"},
		{line: `https://gitlab.example.com/acme/lib`, project: "acme/lib"},
		{line: `"lib": "git+https://gitlab.example.com/acme/lib.git#v1.2.0",`, project: "acme/lib"},
		{line: `git@gitlab.example.com:acme/lib.git`, project: "acme/lib"},
		{line: `ssh://git@gitlab.example.com:2222/acme/lib.git`, project: "acme/lib"},
		{line: `url = https://oauth2:s3cret@gitlab.example.com/acme/lib.git`, project: "acme/lib"},
		{line: `-e git+https://gitlab.example.com/acme/lib.git@v1.2#egg=lib`, project: "acme/lib"},
		{line: `source = "git::https://gitlab.example.com/acme/platform/svc.git//modules/x?ref=v1"`, project: "acme/platform/svc"},
		{line: `https://GitLab.Example.com/Acme/Lib`, project: "acme/lib"},
		{line: `see (https://gitlab.example.com/acme/lib).`, project: "acme/lib"},

		// Pages of a project are the project.
		{line: `https://gitlab.example.com/acme/lib/-/tree/main/pkg`, project: "acme/lib"},
		{line: `https://gitlab.example.com/acme/lib/-/raw/main/ci.yml`, project: "acme/lib"},

		// Go module paths: no scheme, and a major-version or package suffix.
		{line: "\tgitlab.example.com/acme/lib v1.4.0", project: "acme/lib"},
		{line: "\tgitlab.example.com/acme/lib/v2 v2.0.1", project: "acme/lib"},
		{line: `import "gitlab.example.com/acme/platform/svc/internal/api"`, project: "acme/platform/svc"},

		// The longest project wins, and only at a path boundary.
		{line: `gitlab.example.com/acme/lib-tools/cmd/x`, project: "acme/lib-tools"},
		{line: `gitlab.example.com/acme/library`, unresolved: "acme/library"},

		// The instance's other names: a clone host from the config, and one
		// the API advertises.
		{line: `https://git.internal.example/acme/app.git`, project: "acme/app"},
		{line: `git@ssh.gitlab.example.com:acme/app.git`, project: "acme/app"},

		// The API names a project by id or by encoded path.
		{line: `@acme:registry=https://gitlab.example.com/api/v4/projects/3/packages/npm/`, project: "acme/app"},
		{line: `https://gitlab.example.com/api/v4/projects/acme%2Flib/packages/generic/x/1/x.tgz`, project: "acme/lib"},
		{line: `https://gitlab.example.com/api/v4/projects/9999/packages/npm/`},
		{line: `https://gitlab.example.com/api/v4/groups/7/-/packages/npm/`},

		// On the instance, but no project the index knows.
		{line: `https://gitlab.example.com/acme/gone.git`, unresolved: "acme/gone"},
		{line: `https://gitlab.example.com/acme/scratch`, unresolved: "acme/scratch"}, // only a local checkout

		// On the instance, but not a project at all.
		{line: `https://gitlab.example.com/help/user/project/index.md`},
		{line: `https://gitlab.example.com/-/profile/keys`},
		{line: `https://gitlab.example.com/acme`},
		{line: `https://gitlab.example.com/`},

		// Excluded by the workspace: passed over, not reported.
		{line: `https://gitlab.example.com/acme/vendor/mirror.git`},
		{line: `gitlab.example.com/acme/vendor/whatever/else`},

		// Somebody else's.
		{line: `https://github.com/acme/lib`},
		{line: `https://notgitlab.example.com/acme/lib`},
		{line: `https://gitlab.example.com.evil.org/acme/lib`},
		{line: `https://sub.gitlab.example.com/acme/lib`},
		{line: `nothing to see here`},
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			toks := r.tokens(tt.line)
			var got ref
			if len(toks) > 0 {
				got = r.resolveToken(toks[0])
			}
			if got.Project != tt.project {
				t.Errorf("project = %q, want %q (tokens %q)", got.Project, tt.project, toks)
			}
			gotUnresolved := ""
			if got.unresolved() {
				gotUnresolved = got.Path
			}
			if gotUnresolved != tt.unresolved {
				t.Errorf("unresolved = %q, want %q (tokens %q)", gotUnresolved, tt.unresolved, toks)
			}
		})
	}
}

func TestResolvePath(t *testing.T) {
	r := testResolver(t, "https://gitlab.example.com")
	tests := []struct {
		in         string
		project    string
		unresolved string
	}{
		{in: "acme/lib", project: "acme/lib"},
		{in: "/acme/lib/", project: "acme/lib"},
		{in: "acme/lib.git", project: "acme/lib"},
		{in: "ACME/Lib", project: "acme/lib"},
		{in: "acme/platform/svc", project: "acme/platform/svc"},
		// A bare path names a project exactly: nothing follows one here.
		{in: "acme/lib/v2", unresolved: "acme/lib/v2"},
		{in: "acme/nope", unresolved: "acme/nope"},
		// A value that still holds a variable names nothing.
		{in: "$GROUP/lib"},
		{in: "${CI_UNKNOWN}/lib"},
		{in: "{project: acme/lib}"},
		{in: ""},
		{in: "acme/vendor/mirror"}, // excluded
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := r.resolvePath(tt.in)
			if got.Project != tt.project {
				t.Errorf("project = %q, want %q", got.Project, tt.project)
			}
			gotUnresolved := ""
			if got.unresolved() {
				gotUnresolved = got.Path
			}
			if gotUnresolved != tt.unresolved {
				t.Errorf("unresolved = %q, want %q", gotUnresolved, tt.unresolved)
			}
		})
	}
}

func TestResolveComponent(t *testing.T) {
	r := testResolver(t, "https://gitlab.example.com")
	tests := []struct {
		in         string
		project    string
		unresolved string
	}{
		{in: "gitlab.example.com/acme/components/sast@1.0", project: "acme/components"},
		{in: "gitlab.example.com/acme/components/sast@~latest", project: "acme/components"},
		{in: "gitlab.example.com/acme/components/go/build@v2", project: "acme/components"},
		{in: "gitlab.example.com/acme/unknown/sast@1.0", unresolved: "acme/unknown"},
		{in: "gitlab.com/components/sast/sast@1.0"}, // the public catalogue, not this instance
		{in: "$CI_SERVER_FQDN/acme/components/sast@1.0"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := r.resolveComponent(tt.in)
			if got.Project != tt.project {
				t.Errorf("project = %q, want %q", got.Project, tt.project)
			}
			gotUnresolved := ""
			if got.unresolved() {
				gotUnresolved = got.Path
			}
			if gotUnresolved != tt.unresolved {
				t.Errorf("unresolved = %q, want %q", gotUnresolved, tt.unresolved)
			}
		})
	}
}

// An instance served under a sub-path: the sub-path is not part of any
// project's name.
func TestResolveInstanceUnderSubPath(t *testing.T) {
	r := testResolver(t, "https://example.com/gitlab")
	for line, want := range map[string]string{
		`https://example.com/gitlab/acme/lib.git`:         "acme/lib",
		`git@example.com:gitlab/acme/lib.git`:             "acme/lib",
		`https://example.com/gitlab/acme/lib/-/tree/main`: "acme/lib",
	} {
		toks := r.tokens(line)
		if len(toks) == 0 {
			t.Errorf("%s: no token found", line)
			continue
		}
		if got := r.resolveToken(toks[0]); got.Project != want {
			t.Errorf("%s: project = %q, want %q", line, got.Project, want)
		}
	}
}

func TestInstancePathCuts(t *testing.T) {
	r := testResolver(t, "https://gitlab.example.com")
	for in, want := range map[string]string{
		"acme/lib.git":               "acme/lib",
		"acme/lib.git/":              "acme/lib",
		"acme/lib.git//modules/x":    "acme/lib",
		"acme/lib//modules/x":        "acme/lib",
		"acme/lib/-/tree/main":       "acme/lib",
		"acme/lib@v1.2.3":            "acme/lib",
		"acme/lib#readme":            "acme/lib",
		"acme/lib?ref=main":          "acme/lib",
		"acme%2Flib":                 "acme/lib",
		"/acme/lib/":                 "acme/lib",
		"acme/.github":               "acme/.github", // ".git" inside a name is not a suffix
		"acme/lib.gitignore-tooling": "acme/lib.gitignore-tooling",
		"acme/lib.":                  "acme/lib",
	} {
		if got := r.instancePath(in); got != want {
			t.Errorf("instancePath(%q) = %q, want %q", in, got, want)
		}
	}
}
