package api

import (
	"net/http"
	"slices"
	"strings"
)

type methodRegistrar struct {
	mux   *http.ServeMux
	paths map[string]map[string]http.HandlerFunc
}

func (r *methodRegistrar) HandleFunc(pattern string, handler http.HandlerFunc) {
	method, path, _ := strings.Cut(pattern, " ")
	handlers := r.paths[path]
	if handlers == nil {
		handlers = make(map[string]http.HandlerFunc)
		r.paths[path] = handlers
	}
	if _, exists := handlers[method]; exists {
		panic("duplicate route " + pattern)
	}
	handlers[method] = handler
}

func (r *methodRegistrar) register() {
	for path, handlers := range r.paths {
		methods := make([]string, 0, len(handlers))
		for method := range handlers {
			methods = append(methods, method)
		}
		slices.Sort(methods)
		allow := strings.Join(methods, ", ")
		r.mux.HandleFunc(path, func(w http.ResponseWriter, req *http.Request) {
			handler, ok := handlers[req.Method]
			if !ok {
				methodNotAllowed(w, req, allow)
				return
			}
			handler(w, req)
		})
	}
}

// action routes share a wildcard with adjacent resource paths. Pick the
// action from the path value before checking the method or authenticating.
func (r *methodRegistrar) action(path, param string, read, mutate http.HandlerFunc, actions ...string) {
	r.mux.HandleFunc(path, func(w http.ResponseWriter, req *http.Request) {
		value := req.PathValue(param)
		separator := strings.LastIndexByte(value, ':')
		if separator >= 0 && slices.Contains(actions, value[separator+1:]) {
			if req.Method != http.MethodPost {
				methodNotAllowed(w, req, http.MethodPost)
				return
			}
			mutate(w, req)
			return
		}
		if read == nil {
			routeNotFound(w, req)
			return
		}
		if req.Method != http.MethodGet {
			methodNotAllowed(w, req, http.MethodGet)
			return
		}
		read(w, req)
	})
}

func routeNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request, allow string) {
	w.Header().Set("Allow", allow)
	writeProblem(w, r, http.StatusMethodNotAllowed, "https://ocservia.dev/problems/method-not-allowed", "Method not allowed", "the requested method is not supported")
}

func routePaths(mux *http.ServeMux) http.Handler {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if trimmed := strings.TrimRight(r.URL.Path, "/"); trimmed != "" && trimmed != r.URL.Path {
			canonical := r.Clone(r.Context())
			canonical.URL.Path, canonical.URL.RawPath = trimmed, ""
			_, pattern := mux.Handler(canonical)
			if strings.Contains(pattern, "{") && pattern != "/api/v1/operations/{operation_id}" {
				http.NotFound(w, r)
				return
			}
		}
		routeNotFound(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(strings.Trim(r.URL.Path, "/"), "//") ||
			strings.Contains(strings.ToLower(r.URL.RawPath), "%2f") ||
			strings.Contains(r.URL.Path+"/", "/./") || strings.Contains(r.URL.Path+"/", "/../") {
			routeNotFound(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
