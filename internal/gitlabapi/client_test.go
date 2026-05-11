package gitlabapi

import "testing"

// fakeClient satisfies Client without making HTTP calls.
type fakeClient struct {
	Group        *Group
	SubGroups    []*Group
	Descendants  []*Group
	Projects     []*Project
	Recursive    bool
	LastListPath string
}

func (f *fakeClient) GetGroup(path string) (*Group, error) {
	f.LastListPath = path
	return f.Group, nil
}

func (f *fakeClient) ListSubGroups(parent string) ([]*Group, error) {
	f.LastListPath = parent
	return f.SubGroups, nil
}

func (f *fakeClient) ListDescendantGroups(parent string) ([]*Group, error) {
	f.LastListPath = parent
	return f.Descendants, nil
}

func (f *fakeClient) ListGroupProjects(parent string, recursive bool) ([]*Project, error) {
	f.LastListPath = parent
	f.Recursive = recursive
	return f.Projects, nil
}

// TestClientInterfaceContract proves that any code accepting the Client
// interface can be driven by a hand-written fake — no GitLab network or
// SDK objects required. This is the executable example future contributors
// copy when adding tests for code paths that fetch GitLab data.
func TestClientInterfaceContract(t *testing.T) {
	fake := &fakeClient{
		Group: &Group{FullPath: "tenant/images", Name: "images"},
		Projects: []*Project{
			{PathWithNamespace: "tenant/images/api", SSHURLToRepo: "git@host:tenant/images/api.git"},
			{PathWithNamespace: "tenant/images/web", HTTPURLToRepo: "https://host/tenant/images/web.git"},
		},
	}
	var c Client = fake

	g, err := c.GetGroup("tenant/images")
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if g == nil || g.FullPath != "tenant/images" {
		t.Fatalf("GetGroup returned %+v", g)
	}

	projects, err := c.ListGroupProjects("tenant/images", true)
	if err != nil {
		t.Fatalf("ListGroupProjects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(projects))
	}
	if !fake.Recursive {
		t.Errorf("recursive flag was not propagated to fake")
	}
	if fake.LastListPath != "tenant/images" {
		t.Errorf("parent path mismatch: %q", fake.LastListPath)
	}
}
