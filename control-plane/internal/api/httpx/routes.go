package httpx

import "net/http"

// Registrar installs a handler on the application's root ServeMux.
type Registrar interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

var _ Registrar = (*http.ServeMux)(nil)
