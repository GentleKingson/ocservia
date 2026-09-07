package api

import (
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

const authSourceCapacity = 4096

type authWindow struct {
	until time.Time
	count int
}

type authAdmission struct {
	mu          sync.Mutex
	sources     map[netip.Addr]authWindow
	nextSweep   time.Time
	global      authWindow
	perSource   int
	globalLimit int
	active      chan struct{}
}

func newAuthAdmission(perSource, globalLimit, concurrent int) *authAdmission {
	return &authAdmission{sources: make(map[netip.Addr]authWindow), perSource: perSource, globalLimit: globalLimit, active: make(chan struct{}, concurrent)}
}

func (a *authAdmission) admit(source netip.Addr, now time.Time, emergency bool) (func(), time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !emergency {
		if !now.Before(a.nextSweep) {
			for key, window := range a.sources {
				if !now.Before(window.until) {
					delete(a.sources, key)
				}
			}
			a.nextSweep = now.Add(time.Minute)
		}
		window, exists := a.sources[source]
		// Do not evict live windows: cycling source addresses must not reset limits.
		if !exists && len(a.sources) >= authSourceCapacity {
			return nil, time.Minute
		}
		if !now.Before(window.until) {
			window = authWindow{until: now.Add(time.Minute)}
		}
		if window.count >= a.perSource {
			return nil, window.until.Sub(now)
		}
		if !now.Before(a.global.until) {
			a.global = authWindow{until: now.Add(time.Minute)}
		}
		if a.globalLimit > 0 && a.global.count >= a.globalLimit {
			return nil, a.global.until.Sub(now)
		}
		window.count++
		a.sources[source] = window
		a.global.count++
	}
	select {
	case a.active <- struct{}{}:
		return func() { <-a.active }, 0
	default:
		return nil, time.Second
	}
}

// ConfigureAuthProxies must be called before serving. Only these peers may
// supply the single client address overwritten by the production gateway.
func (s *Server) ConfigureAuthProxies(prefixes []netip.Prefix) {
	s.authProxies = append([]netip.Prefix(nil), prefixes...)
}

func (s *Server) authSource(r *http.Request) netip.Addr {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	address := peer.Addr().Unmap()
	for _, prefix := range s.authProxies {
		if !prefix.Contains(address) {
			continue
		}
		values := r.Header.Values("X-Ocservia-Client-IP")
		if len(values) == 1 {
			client, err := netip.ParseAddr(values[0])
			if err == nil && client.Zone() == "" {
				return client.Unmap()
			}
		}
		break
	}
	return address
}

func (s *Server) admitAuthentication(w http.ResponseWriter, r *http.Request, budget *authAdmission, emergency bool) func() {
	release, retry := budget.admit(s.authSource(r), time.Now(), emergency)
	if release == nil {
		w.Header().Set("Retry-After", strconv.Itoa(int((retry+time.Second-1)/time.Second)))
		w.Header().Set("Cache-Control", "no-store")
		writeProblem(w, r, http.StatusTooManyRequests, "https://ocservia.dev/problems/authentication-limit", "Authentication limit reached", "retry authentication after the indicated delay")
	}
	return release
}

func (s *Server) limitAuthentication(budget *authAdmission, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Preserve the disabled-auth response without spending a budget.
		if s.auth == nil {
			next(w, r)
			return
		}
		release := s.admitAuthentication(w, r, budget, false)
		if release == nil {
			return
		}
		defer release()
		next(w, r)
	}
}
