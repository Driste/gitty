package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The handful of GitLab calls the hub makes.
//
// It asks the instance two kinds of question. With its own token: which
// groups and projects exist — the listing, the only thing it ever fetches on
// its own account. With a caller's token: who is this, and how do they stand
// on that project — so that what is written here is signed by someone the
// instance knows, and weighed the way the instance itself would weigh them.
//
// Neither needs more than GET and JSON, so there is no client library here.

// gitlab talks to one instance's REST API.
type gitlab struct {
	api   string // <instance>/api/v4
	host  string // for messages
	token string // the hub's own; empty when it lists anonymously
	http  *http.Client
	logf  func(format string, args ...any)
	// wait pauses between attempts. It is a field so tests need not sleep.
	wait func(ctx context.Context, d time.Duration) error
}

func newGitLab(instance, token string, logf func(string, ...any)) (*gitlab, error) {
	u, err := url.Parse(strings.TrimRight(instance, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%q is not an http(s) URL of a GitLab instance", instance)
	}
	return &gitlab{
		api:   u.String() + "/api/v4",
		host:  u.Host,
		token: token,
		http:  &http.Client{Timeout: 60 * time.Second},
		logf:  logf,
		wait: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
	}, nil
}

// apiError is an answer from the instance that is not the one asked for.
type apiError struct {
	Status int
	Path   string
	Body   string
}

func (e *apiError) Error() string {
	msg := strings.TrimSpace(e.Body)
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return fmt.Sprintf("GitLab answered %d for %s: %s", e.Status, e.Path, msg)
}

// statusOf is the HTTP status behind an error from get, or 0.
func statusOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// How patient a request is. A throttled instance says how long to wait, and
// is believed up to a limit; the waiting is said out loud, because a service
// that has gone quiet for a minute looks exactly like one that has hung.
const (
	maxThrottleWaits = 6
	maxThrottleWait  = 90 * time.Second
	maxFaultRetries  = 3
)

// get performs one GET as token (which may be empty) and decodes the JSON
// answer into out. It returns the number of the next page, "" when there is
// none.
func (g *gitlab) get(ctx context.Context, token, path string, query url.Values, out any) (next string, err error) {
	target := g.api + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	throttled, faults := 0, 0
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "gitty-up")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := g.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if faults++; faults > maxFaultRetries {
				return "", fmt.Errorf("reaching %s: %w", g.host, scrub(err, token))
			}
			if err := g.wait(ctx, time.Duration(faults)*time.Second); err != nil {
				return "", err
			}
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			if throttled++; throttled > maxThrottleWaits {
				return "", &apiError{Status: resp.StatusCode, Path: path, Body: "still throttled after waiting; giving up"}
			}
			d := 5 * time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
				d = time.Duration(s) * time.Second
			}
			if d > maxThrottleWait {
				d = maxThrottleWait
			}
			g.logf("throttled by %s: waiting %s before asking again (%d of %d)", g.host, d, throttled, maxThrottleWaits)
			if err := g.wait(ctx, d); err != nil {
				return "", err
			}
			continue
		case resp.StatusCode >= 500 || readErr != nil:
			if faults++; faults > maxFaultRetries {
				if readErr != nil {
					return "", fmt.Errorf("reading the answer from %s: %w", g.host, readErr)
				}
				return "", &apiError{Status: resp.StatusCode, Path: path, Body: string(body)}
			}
			if err := g.wait(ctx, time.Duration(faults)*time.Second); err != nil {
				return "", err
			}
			continue
		case resp.StatusCode < 200 || resp.StatusCode > 299:
			return "", &apiError{Status: resp.StatusCode, Path: path, Body: string(body)}
		}
		if out != nil {
			if err := json.Unmarshal(body, out); err != nil {
				return "", fmt.Errorf("%s answered something that is not the JSON expected for %s: %w", g.host, path, err)
			}
		}
		return resp.Header.Get("X-Next-Page"), nil
	}
}

// scrub keeps a token out of an error's text, should a transport ever put
// request headers there.
func scrub(err error, token string) error {
	if token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "[token]"))
}

// pages calls each with every page of a listing.
func (g *gitlab) pages(ctx context.Context, path string, query url.Values, page any, each func()) error {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("per_page", "100")
	next := "1"
	for next != "" {
		q.Set("page", next)
		var err error
		if next, err = g.get(ctx, g.token, path, q, page); err != nil {
			return err
		}
		each()
	}
	return nil
}

// glGroup and glProject are the fields of GitLab's answers that are kept.
type glGroup struct {
	ID          int64  `json:"id"`
	FullPath    string `json:"full_path"`
	Description string `json:"description"`
	Visibility  string `json:"visibility"`
}

type glProject struct {
	ID                int64     `json:"id"`
	PathWithNamespace string    `json:"path_with_namespace"`
	Description       string    `json:"description"`
	Topics            []string  `json:"topics"`
	Visibility        string    `json:"visibility"`
	LastActivityAt    time.Time `json:"last_activity_at"`
	StarCount         int       `json:"star_count"`
	ForksCount        int       `json:"forks_count"`
	DefaultBranch     string    `json:"default_branch"`
	WebURL            string    `json:"web_url"`
}

// rootGroups lists the top-level groups the hub's token can see.
func (g *gitlab) rootGroups(ctx context.Context) ([]glGroup, error) {
	var all, page []glGroup
	err := g.pages(ctx, "/groups", url.Values{"top_level_only": {"true"}, "all_available": {"true"}}, &page, func() {
		all = append(all, page...)
		page = nil
	})
	return all, err
}

// group fetches one group by path.
func (g *gitlab) group(ctx context.Context, path string) (glGroup, error) {
	var out glGroup
	_, err := g.get(ctx, g.token, "/groups/"+url.PathEscape(path), url.Values{"with_projects": {"false"}}, &out)
	return out, err
}

// descendants lists every group beneath one.
func (g *gitlab) descendants(ctx context.Context, id int64) ([]glGroup, error) {
	var all, page []glGroup
	err := g.pages(ctx, fmt.Sprintf("/groups/%d/descendant_groups", id), nil, &page, func() {
		all = append(all, page...)
		page = nil
	})
	return all, err
}

// projects lists the projects beneath a group, in GitLab's trimmed form —
// which carries everything the hub keeps except whether a project is
// archived, so the two kinds are asked for apart.
func (g *gitlab) projects(ctx context.Context, id int64, archived bool) ([]glProject, error) {
	var all, page []glProject
	q := url.Values{
		"include_subgroups": {"true"}, "simple": {"true"}, "with_shared": {"false"},
		"archived": {strconv.FormatBool(archived)}, "order_by": {"id"}, "sort": {"asc"},
	}
	err := g.pages(ctx, fmt.Sprintf("/groups/%d/projects", id), q, &page, func() {
		all = append(all, page...)
		page = nil
	})
	return all, err
}

// glUser is who a token belongs to.
type glUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	State    string `json:"state"`
	External bool   `json:"external"`
}

// whoIs asks the instance whose token this is.
func (g *gitlab) whoIs(ctx context.Context, token string) (glUser, error) {
	var u glUser
	_, err := g.get(ctx, token, "/user", nil, &u)
	return u, err
}

// GitLab's access levels, as far as they matter here.
const (
	levelNone       = 0  // can see it, and no more
	levelDeveloper  = 30 // can push to it
	levelMaintainer = 40
)

// projectAccess asks, as the caller, whether they can see a project and how
// they stand on it. A project they cannot see is reported as such, not as
// an error.
func (g *gitlab) projectAccess(ctx context.Context, token string, id int64) (level int, visible bool, err error) {
	var p struct {
		Permissions struct {
			ProjectAccess *struct {
				AccessLevel int `json:"access_level"`
			} `json:"project_access"`
			GroupAccess *struct {
				AccessLevel int `json:"access_level"`
			} `json:"group_access"`
		} `json:"permissions"`
	}
	if _, err := g.get(ctx, token, fmt.Sprintf("/projects/%d", id), nil, &p); err != nil {
		if s := statusOf(err); s == http.StatusNotFound || s == http.StatusForbidden {
			return 0, false, nil
		}
		return 0, false, err
	}
	if a := p.Permissions.ProjectAccess; a != nil && a.AccessLevel > level {
		level = a.AccessLevel
	}
	if a := p.Permissions.GroupAccess; a != nil && a.AccessLevel > level {
		level = a.AccessLevel
	}
	return level, true, nil
}

// groupAccess is the same question about a group. Membership is asked about
// separately, and not being a member is an answer, not a failure.
func (g *gitlab) groupAccess(ctx context.Context, token string, id, userID int64) (level int, visible bool, err error) {
	if _, err := g.get(ctx, token, fmt.Sprintf("/groups/%d", id), url.Values{"with_projects": {"false"}}, nil); err != nil {
		if s := statusOf(err); s == http.StatusNotFound || s == http.StatusForbidden {
			return 0, false, nil
		}
		return 0, false, err
	}
	var m struct {
		AccessLevel int `json:"access_level"`
	}
	if _, err := g.get(ctx, token, fmt.Sprintf("/groups/%d/members/all/%d", id, userID), nil, &m); err != nil {
		if s := statusOf(err); s >= 400 && s < 500 {
			return 0, true, nil
		}
		return 0, true, err
	}
	return m.AccessLevel, true, nil
}
