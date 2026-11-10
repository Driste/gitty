package main

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Who is asking.
//
// The hub has no accounts of its own. A request carries a token of the
// GitLab instance, and the instance is asked whose it is; that name signs
// whatever the caller records. The token is used for that question and, for
// a note, for one more — can they see this, and how do they stand on it —
// and then forgotten. What is remembered, briefly, is a hash of it and the
// name it answered to.

// caller is who a request is from.
type caller struct {
	Name     string
	ID       int64
	External bool // an external user of the instance: sees public projects only
	// token is the caller's own, held for the length of the request so that
	// questions about them can be asked as them. It is never stored.
	token string
}

type knownCaller struct {
	who   caller
	ok    bool
	until time.Time
}

type authenticator struct {
	gl   *gitlab
	open bool // --no-auth: everyone is "anonymous"
	now  func() time.Time

	mu    sync.Mutex
	known map[[sha256.Size]byte]knownCaller
}

func newAuthenticator(gl *gitlab, open bool) *authenticator {
	return &authenticator{gl: gl, open: open, now: time.Now, known: map[[sha256.Size]byte]knownCaller{}}
}

// How long an answer about a token is believed. Short enough that a revoked
// token stops working soon; long enough that an agent making twenty calls
// costs the instance one.
const (
	callerTTL  = 5 * time.Minute
	refusalTTL = 30 * time.Second
)

// tokenOf takes the token from a request, in either of the two headers
// GitLab itself accepts.
func tokenOf(r *http.Request) string {
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return strings.TrimSpace(r.Header.Get("PRIVATE-TOKEN"))
}

// identify says who a request is from. When it cannot, it returns the HTTP
// status to answer with and a sentence for the caller.
func (a *authenticator) identify(ctx context.Context, r *http.Request) (*caller, int, string) {
	if a.open {
		return &caller{Name: "anonymous"}, 0, ""
	}
	token := tokenOf(r)
	if token == "" {
		return nil, http.StatusUnauthorized, "this hub needs a token of " + a.gl.host + ": send it as `Authorization: Bearer <token>`"
	}
	key := sha256.Sum256([]byte(token))
	now := a.now()
	a.mu.Lock()
	k, seen := a.known[key]
	a.mu.Unlock()
	if seen && now.Before(k.until) {
		if !k.ok {
			return nil, http.StatusUnauthorized, a.gl.host + " does not accept that token"
		}
		c := k.who
		c.token = token
		return &c, 0, ""
	}

	u, err := a.gl.whoIs(ctx, token)
	switch status := statusOf(err); {
	case err == nil && u.Username != "" && (u.State == "" || u.State == "active"):
		c := caller{Name: u.Username, ID: u.ID, External: u.External}
		a.remember(key, knownCaller{who: c, ok: true, until: now.Add(callerTTL)})
		c.token = token
		return &c, 0, ""
	case err == nil, status == http.StatusUnauthorized, status == http.StatusForbidden:
		a.remember(key, knownCaller{until: now.Add(refusalTTL)})
		return nil, http.StatusUnauthorized, a.gl.host + " does not accept that token"
	}
	// The instance could not be asked. That is not the caller's fault and
	// not a reason to let them in.
	a.gl.logf("could not ask %s who a caller is: %v", a.gl.host, err)
	return nil, http.StatusServiceUnavailable, "could not reach " + a.gl.host + " to check the token; try again"
}

func (a *authenticator) remember(key [sha256.Size]byte, k knownCaller) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.known) > 10_000 {
		a.known = map[[sha256.Size]byte]knownCaller{} // nothing in it is needed
	}
	a.known[key] = k
}

// standing asks how a caller stands on a node: whether they can see it at
// all, and their access level on it, in GitLab's own terms.
func (a *authenticator) standing(ctx context.Context, c *caller, kind string, n *node) (level int, visible bool, err error) {
	if a.open {
		return 0, true, nil
	}
	if c.External && n.Visibility != "public" {
		return 0, false, nil
	}
	if kind == nodeKindGroup {
		return a.gl.groupAccess(ctx, c.token, n.ID, c.ID)
	}
	return a.gl.projectAccess(ctx, c.token, n.ID)
}
