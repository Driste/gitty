package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"gitty/internal/contract"
)

// What gitty talks to: three plain JSON endpoints beside the MCP one. They
// are how a repository's own account reaches the hub — offered by whoever
// has a checkout, taken when it is newer than what is held.

// maxFactsBody bounds one project's facts.
const maxFactsBody = 8 << 20

// routes wires the endpoints together.
func (h *hub) routes(allowOrigins []string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", h.handleMCP)
	mux.HandleFunc("/v1/info", h.api(http.MethodGet, h.handleInfo))
	mux.HandleFunc("/v1/want", h.api(http.MethodPost, h.handleWant))
	mux.HandleFunc("/v1/facts", h.api(http.MethodPut, h.handleFacts))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gitty-up: MCP is served at /mcp", http.StatusNotFound)
	})

	allowed := map[string]bool{}
	for _, o := range allowOrigins {
		if n := webOrigin(o); n != "" {
			allowed[n] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A browser says where a request comes from, and a page on another
		// site has no business talking to this server. Anything that is not
		// a browser sends no Origin and is not affected.
		if origin := r.Header.Get("Origin"); origin != "" && !allowed[webOrigin(origin)] {
			if u, err := url.Parse(origin); err != nil || !strings.EqualFold(u.Host, r.Host) {
				writeRPCError(w, http.StatusForbidden, nil, codeInvalidRequest, "this origin may not use the hub", nil)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

type apiHandler func(w http.ResponseWriter, r *http.Request, c *caller)

func apiFail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// api wraps an endpoint for gitty: one method, a known caller.
func (h *hub) api(method string, next apiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			apiFail(w, http.StatusMethodNotAllowed, "this endpoint takes "+method)
			return
		}
		c, status, why := h.auth.identify(r.Context(), r)
		if c == nil {
			if status == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", `Bearer realm="gitty-up"`)
			}
			apiFail(w, status, why)
			return
		}
		next(w, r, c)
	}
}

func (h *hub) handleInfo(w http.ResponseWriter, r *http.Request, _ *caller) {
	h.st.mu.RLock()
	info := contract.HubInfo{
		Schema: contract.HubSchema, Name: serverName, Version: version, Instance: h.st.instance,
		Projects: len(h.st.projects), Groups: len(h.st.groups), ListedAt: h.st.listedAt,
	}
	h.st.mu.RUnlock()
	writeJSON(w, http.StatusOK, info)
}

func (h *hub) handleWant(w http.ResponseWriter, r *http.Request, _ *caller) {
	var req contract.HubWant
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFactsBody)).Decode(&req); err != nil {
		apiFail(w, http.StatusBadRequest, "the body is not what /v1/want takes")
		return
	}
	if err := contract.Supported("what gitty sent", req.Schema, contract.HubSchema); err != nil {
		apiFail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, h.st.wants(req.Have))
}

func (h *hub) handleFacts(w http.ResponseWriter, r *http.Request, c *caller) {
	var f contract.Facts
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFactsBody)).Decode(&f); err != nil {
		apiFail(w, http.StatusBadRequest, "the body is not one project's facts")
		return
	}
	// Someone the instance treats as external sees public projects only,
	// and may speak for no others.
	if c.External {
		h.st.mu.RLock()
		p := h.st.projID[f.ID]
		h.st.mu.RUnlock()
		if p == nil || p.Visibility != "public" {
			writeJSON(w, http.StatusOK, contract.HubReceipt{Schema: contract.HubSchema, Project: f.Project, Reason: "not a project this hub covers"})
			return
		}
	}
	receipt, err := h.st.putFacts(&f)
	var refused *errRefused
	switch {
	case errors.As(err, &refused):
		receipt.Reason = refused.reason
		writeJSON(w, http.StatusOK, receipt)
	case err != nil:
		h.logf("storing facts for %s: %v", f.Project, err)
		apiFail(w, http.StatusInternalServerError, "the hub could not store that")
	default:
		if receipt.Stored {
			h.logf("facts: %s at %s from @%s (%d links)", receipt.Project, short(f.Commit), c.Name, receipt.Links)
		}
		writeJSON(w, http.StatusOK, receipt)
	}
}
