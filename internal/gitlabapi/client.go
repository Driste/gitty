// Package gitlabapi is gitty's narrow surface to the GitLab REST API.
//
// It exposes a Client interface with the four methods sync orchestration
// needs, returning POGO Group and Project types so consumers (and their
// tests) do not depend on the upstream client-go SDK types.
package gitlabapi

import (
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// Group is a GitLab namespace as gitty consumes it.
type Group struct {
	FullPath string
	Name     string
}

// Project is a GitLab repository as gitty consumes it.
type Project struct {
	PathWithNamespace string
	SSHURLToRepo      string
	HTTPURLToRepo     string
}

// Client is the narrow GitLab data-access surface used by sync orchestration.
type Client interface {
	GetGroup(path string) (*Group, error)
	ListSubGroups(parent string) ([]*Group, error)
	ListDescendantGroups(parent string) ([]*Group, error)
	ListGroupProjects(parent string, recursive bool) ([]*Project, error)
}

// Real is the production Client backed by the upstream client-go SDK.
type Real struct {
	c *gitlab.Client
}

// NewReal constructs a Real Client targeting baseURL with the given token.
func NewReal(baseURL, token string) (Client, error) {
	c, err := gitlab.NewClient(token, gitlab.WithBaseURL(baseURL))
	if err != nil {
		return nil, err
	}
	return &Real{c: c}, nil
}

// GetGroup fetches a single group by full path.
func (r *Real) GetGroup(path string) (*Group, error) {
	g, _, err := r.c.Groups.GetGroup(path, nil)
	if err != nil {
		return nil, err
	}
	if g == nil || g.FullPath == "" {
		return nil, nil
	}
	return &Group{FullPath: g.FullPath, Name: g.Name}, nil
}

// ListSubGroups returns the immediate subgroups of parent.
func (r *Real) ListSubGroups(parent string) ([]*Group, error) {
	items, err := paginate(func(opts gitlab.ListOptions) ([]*gitlab.Group, *gitlab.Response, error) {
		return r.c.Groups.ListSubGroups(parent, &gitlab.ListSubGroupsOptions{ListOptions: opts})
	})
	if err != nil {
		return nil, err
	}
	return convertGroups(items), nil
}

// ListDescendantGroups returns all groups beneath parent recursively.
func (r *Real) ListDescendantGroups(parent string) ([]*Group, error) {
	items, err := paginate(func(opts gitlab.ListOptions) ([]*gitlab.Group, *gitlab.Response, error) {
		return r.c.Groups.ListDescendantGroups(parent, &gitlab.ListDescendantGroupsOptions{ListOptions: opts})
	})
	if err != nil {
		return nil, err
	}
	return convertGroups(items), nil
}

// ListGroupProjects returns the projects in parent. When recursive is true,
// projects in subgroups are included.
func (r *Real) ListGroupProjects(parent string, recursive bool) ([]*Project, error) {
	items, err := paginate(func(opts gitlab.ListOptions) ([]*gitlab.Project, *gitlab.Response, error) {
		return r.c.Groups.ListGroupProjects(parent, &gitlab.ListGroupProjectsOptions{
			IncludeSubGroups: &recursive,
			ListOptions:      opts,
		})
	})
	if err != nil {
		return nil, err
	}
	out := make([]*Project, 0, len(items))
	for _, p := range items {
		if p == nil {
			continue
		}
		out = append(out, &Project{
			PathWithNamespace: p.PathWithNamespace,
			SSHURLToRepo:      p.SSHURLToRepo,
			HTTPURLToRepo:     p.HTTPURLToRepo,
		})
	}
	return out, nil
}

func convertGroups(items []*gitlab.Group) []*Group {
	out := make([]*Group, 0, len(items))
	for _, g := range items {
		if g == nil || g.FullPath == "" {
			continue
		}
		out = append(out, &Group{FullPath: g.FullPath, Name: g.Name})
	}
	return out
}
