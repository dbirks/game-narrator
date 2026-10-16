package api

import (
	"fmt"
	"net/http"
	"strings"
)

// route records a registered (method, path) pair so the fallback handler can
// tell "unknown path" (404) from "wrong method on a known path" (405).
type route struct {
	method string
	path   string
}

// apiRoot is the prefix of every versioned endpoint (docs/spec/08 "Base
// /api/v1").
const apiRoot = "/api/"

// routeTable indexes registered routes by path.
type routeTable []route

// register adds the pattern to mux and records it for 405 detection.
func (t *routeTable) register(mux *http.ServeMux, method, path string, h http.HandlerFunc) {
	mux.HandleFunc(method+" "+path, h)
	*t = append(*t, route{method: method, path: path})
}

// allowedMethods lists the methods valid for path, expanding GET to HEAD the
// way ServeMux does ("GET matches both GET and HEAD").
func (t routeTable) allowedMethods(path string) []string {
	var out []string
	for _, rt := range t {
		if rt.path != path {
			continue
		}
		out = append(out, rt.method)
		if rt.method == http.MethodGet {
			out = append(out, http.MethodHead)
		}
	}
	return out
}

// matches reports whether a route claims this exact method+path pair. GET
// routes also answer HEAD, like ServeMux.
func (t routeTable) matches(method, path string) bool {
	for _, rt := range t {
		if rt.path != path {
			continue
		}
		if rt.method == method || (rt.method == http.MethodGet && method == http.MethodHead) {
			return true
		}
	}
	return false
}

// fallback answers everything ServeMux could not route. ServeMux renders
// plain-text 404/405 responses that cannot be overridden through its handler
// fields in this Go build, and docs/spec/08 requires RFC 9457 problem+json
// with a request_id, so unmatched requests land on the "/" pattern here.
//
// The embedded web UI (docs/spec/09) is dispatched earlier, in Handler, so
// this only sees /api/ paths and wrong-method calls on registered routes.
func (s *Server) fallback(routes routeTable) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if methods := routes.allowedMethods(r.URL.Path); len(methods) > 0 {
			w.Header().Set("Allow", strings.Join(methods, ", "))
			s.writeProblem(w, r, http.StatusMethodNotAllowed, "method-not-allowed",
				"Method Not Allowed",
				fmt.Sprintf("method %s is not allowed on %s; allowed methods: %s",
					r.Method, r.URL.Path, strings.Join(methods, ", ")))
			return
		}

		detail := "no route matches the requested path"
		if !strings.HasPrefix(r.URL.Path, apiRoot) {
			detail = "no route or embedded asset matches the requested path"
		}
		s.writeProblem(w, r, http.StatusNotFound, "not-found", "Not Found", detail)
	})
}
