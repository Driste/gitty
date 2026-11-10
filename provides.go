package main

import (
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gitty/internal/contract"
)

// What a project provides.
//
// Links say what a project leans on. This says what it offers, read the
// same way — from the names of tracked files, opening only the few whose
// names say they define something: a CI component, a pipeline template, a
// security policy, a Helm chart, a Terraform module, a container image.
//
// It is what lets "is there already something here that builds an image?"
// be answered from the repositories themselves, with a file to open.

// The most a project is recorded as providing, and the most parameters
// recorded for any one thing: a catalogue entry, not a reference manual.
const (
	maxProvided = 60
	maxInputs   = 30
)

// provides lists what the checkout in dir offers, given its tracked files.
func provides(dir string, names []string) []contract.Provided {
	read := func(name string) []string {
		lines, _ := readLines(filepath.Join(dir, filepath.FromSlash(name)))
		return lines
	}
	isChart := false // then templates/ holds manifests, not pipelines
	for _, n := range names {
		if n == "Chart.yaml" {
			isChart = true
		}
	}

	var out []contract.Provided
	var tfRoot []string
	tfModules := map[string][]string{}
	for _, name := range names {
		if name == "" {
			continue
		}
		dirName, base := path.Split(name)
		dirName = strings.TrimSuffix(dirName, "/")
		skipped := false
		for _, seg := range strings.Split(dirName, "/") {
			if skippedDirs[seg] {
				skipped = true
			}
		}
		if skipped {
			continue
		}
		isYAML := strings.HasSuffix(base, ".yml") || strings.HasSuffix(base, ".yaml")
		stem := strings.TrimSuffix(base, path.Ext(base))
		switch {
		// templates/<name>.yml and templates/<name>/template.yml: the two
		// places GitLab looks for a component, and where teams keep the
		// templates they include by file.
		case isYAML && !isChart && (dirName == "templates" ||
			(stem == "template" && path.Dir(dirName) == "templates")):
			component := stem
			if dirName != "templates" {
				component = path.Base(dirName)
			}
			if p, ok := pipelineTemplate(component, name, read(name)); ok {
				out = append(out, p)
			}
		case name == ".gitlab/security-policies/policy.yml" || name == ".gitlab/security-policies/policy.yaml":
			out = append(out, policies(name, read(name))...)
		case base == "Chart.yaml":
			if p, ok := helmChart(name, read(name)); ok {
				out = append(out, p)
			}
		case name == "Dockerfile" || name == "Containerfile":
			out = append(out, contract.Provided{Kind: contract.ProvidesImage, Name: "image", File: name})
		case strings.HasSuffix(base, ".tf") && dirName == "":
			tfRoot = append(tfRoot, name)
		case strings.HasSuffix(base, ".tf") && path.Dir(dirName) == "modules":
			tfModules[dirName] = append(tfModules[dirName], name)
		}
	}

	// A root that configures a backend is somebody's infrastructure, not a
	// module for others; what it keeps under modules/ still is.
	if len(tfRoot) > 0 {
		if inputs, live := terraformInputs(tfRoot, read); !live {
			out = append(out, contract.Provided{Kind: contract.ProvidesTerraform, Name: "module", File: ".", Inputs: inputs})
		}
	}
	for moduleDir, files := range tfModules {
		inputs, _ := terraformInputs(files, read)
		out = append(out, contract.Provided{Kind: contract.ProvidesTerraform, Name: path.Base(moduleDir), File: moduleDir, Inputs: inputs})
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].File < out[j].File
	})
	if len(out) > maxProvided {
		out = out[:maxProvided]
	}
	return out
}

// scalarText is the text of a "key: value" scalar on one line. A block
// scalar (`>` or `|`) has its text on the lines that follow: block is then
// true and the caller takes the next line down.
func scalarText(value string) (text string, block bool) {
	v := strings.TrimSpace(value)
	if v != "" && (v[0] == '>' || v[0] == '|') && len(strings.TrimRight(v[1:], "+-0123456789")) == 0 {
		return "", true
	}
	return strings.Join(strings.Fields(cleanYAMLScalar(v)), " "), false
}

// yamlIndent is how far a line is indented, or -1 for a line that holds
// nothing: blank, or only a comment.
func yamlIndent(line string) int {
	trimmed := strings.TrimLeft(line, " ")
	if strings.TrimSpace(trimmed) == "" || trimmed[0] == '#' {
		return -1
	}
	return len(line) - len(trimmed)
}

// pipelineTemplate describes a file under templates/. With a `spec:` header
// it is a CI/CD component and its inputs are its interface; without one it
// is a template to be included by file. A Kubernetes manifest that happens
// to live in a directory of that name is neither.
func pipelineTemplate(name, file string, lines []string) (contract.Provided, bool) {
	if lines == nil {
		return contract.Provided{}, false
	}
	p := contract.Provided{Kind: contract.ProvidesCITemplate, Name: name, File: file}
	inSpec := false
	inputsAt := -1 // indentation of `inputs:` while inside it
	namesAt := -1  // indentation of the input names beneath it
	aboutNext := false
	for _, line := range lines {
		if inSpec && strings.HasPrefix(line, "---") {
			break // the header ends where the pipeline begins
		}
		indent := yamlIndent(line)
		if indent < 0 {
			continue
		}
		if aboutNext {
			aboutNext = false
			if indent > 0 {
				p.About = truncateRunes(strings.Join(strings.Fields(line), " "), 200)
				continue
			}
		}
		key, value, ok := splitYAMLKey(strings.TrimSpace(line))
		if indent == 0 {
			if ok && (key == "apiVersion" || key == "kind") {
				return contract.Provided{}, false
			}
			inSpec, inputsAt, namesAt = ok && key == "spec", -1, -1
			if inSpec {
				p.Kind = contract.ProvidesCIComponent
			}
			continue
		}
		if !inSpec || !ok {
			continue
		}
		if inputsAt >= 0 && indent <= inputsAt {
			inputsAt, namesAt = -1, -1 // back out of the inputs
		}
		switch {
		case inputsAt < 0 && key == "inputs":
			inputsAt = indent
		case inputsAt >= 0:
			if namesAt < 0 {
				namesAt = indent
			}
			if indent == namesAt && len(p.Inputs) < maxInputs {
				p.Inputs = append(p.Inputs, key)
			}
		case key == "description" && p.About == "":
			text, block := scalarText(value)
			p.About, aboutNext = truncateRunes(text, 200), block
		}
	}
	return p, true
}

// policies lists the security policies a policy project defines: each
// `- name:` beneath a top-level `<type>_policy:` key.
func policies(file string, lines []string) []contract.Provided {
	var out []contract.Provided
	kind := ""   // the policy type being read, in words
	itemAt := -1 // indentation of the list items beneath it
	aboutNext := false
	describe := func(p *contract.Provided, value string) {
		text, block := scalarText(value)
		aboutNext = block
		if text != "" {
			p.About = kind + ": " + truncateRunes(text, 160)
		}
	}
	for _, line := range lines {
		indent := yamlIndent(line)
		if indent < 0 {
			continue
		}
		body := strings.TrimSpace(line)
		if aboutNext {
			aboutNext = false
			if len(out) > 0 && itemAt >= 0 && indent > itemAt+2 {
				out[len(out)-1].About = kind + ": " + truncateRunes(strings.Join(strings.Fields(body), " "), 160)
				continue
			}
		}
		if indent == 0 && !strings.HasPrefix(body, "- ") {
			kind, itemAt = "", -1
			if key, _, ok := splitYAMLKey(body); ok && strings.HasSuffix(key, "_policy") {
				kind = strings.ReplaceAll(key, "_", " ")
			}
			continue
		}
		if kind == "" {
			continue
		}
		item := strings.HasPrefix(body, "- ")
		if item && (itemAt < 0 || indent == itemAt) {
			itemAt = indent
			// An item need not open with its name: keep a place for it,
			// and take the name when it comes.
			out = append(out, contract.Provided{Kind: contract.ProvidesPolicy, File: file, About: kind})
			last := &out[len(out)-1]
			switch key, value, ok := splitYAMLKey(strings.TrimSpace(body[2:])); {
			case !ok:
			case key == "name":
				last.Name = cleanYAMLScalar(value)
			case key == "description":
				describe(last, value)
			}
			continue
		}
		if itemAt < 0 || len(out) == 0 || indent != itemAt+2 {
			continue
		}
		last := &out[len(out)-1]
		switch key, value, ok := splitYAMLKey(body); {
		case !ok:
		case key == "name" && last.Name == "":
			last.Name = cleanYAMLScalar(value)
		case key == "description":
			describe(last, value)
		}
	}
	named := out[:0]
	for _, p := range out {
		if p.Name != "" {
			named = append(named, p)
		}
	}
	return named
}

// helmChart reads a chart's name and description from its Chart.yaml.
func helmChart(file string, lines []string) (contract.Provided, bool) {
	p := contract.Provided{Kind: contract.ProvidesHelmChart, File: file}
	for _, line := range lines {
		if yamlIndent(line) != 0 {
			continue
		}
		switch key, value, ok := splitYAMLKey(strings.TrimSpace(line)); {
		case !ok:
		case key == "name":
			p.Name = cleanYAMLScalar(value)
		case key == "description":
			text, _ := scalarText(value)
			p.About = truncateRunes(text, 200)
		}
	}
	return p, p.Name != ""
}

var (
	tfVariableRe = regexp.MustCompile(`^\s*variable\s+"([^"]+)"`)
	tfBackendRe  = regexp.MustCompile(`^\s*(backend\s+"[^"]+"|cloud)\s*\{`)
)

// terraformInputs lists the variables a set of .tf files declares, and
// reports whether they configure a backend — the mark of a live
// configuration rather than a module.
func terraformInputs(files []string, read func(string) []string) (inputs []string, live bool) {
	sort.Strings(files)
	seen := map[string]bool{}
	for _, f := range files {
		for _, line := range read(f) {
			if tfBackendRe.MatchString(line) {
				live = true
			}
			if m := tfVariableRe.FindStringSubmatch(line); m != nil && !seen[m[1]] && len(inputs) < maxInputs {
				seen[m[1]] = true
				inputs = append(inputs, m[1])
			}
		}
	}
	return inputs, live
}
