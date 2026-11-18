package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

type authLogKind int

const (
	localDisabled authLogKind = iota
	localOriginRejected
	localInvalidRequest
	localCredentialsRejected
	localAccountLimited
	localCapacityLimited
	localUnavailable
	localSucceeded
	localSourceLimited
	localGlobalLimited
	localConcurrencyLimited
	localSourceCapacityLimited
	authLogKinds
)

var authLogDefinitions = [authLogKinds]struct{ method, outcome, reason string }{
	{"local", "rejected", "disabled"},
	{"local", "rejected", "origin_rejected"},
	{"local", "rejected", "invalid_request"},
	{"local", "rejected", "credentials_rejected"},
	{"local", "rejected", "account_limited"},
	{"local", "unavailable", "account_capacity"},
	{"local", "unavailable", "infrastructure_failure"},
	{"local", "succeeded", "session_created"},
	{"local", "rejected", "source_limited"},
	{"local", "rejected", "global_limited"},
	{"local", "rejected", "concurrency_limited"},
	{"local", "rejected", "source_capacity"},
}

const authLogSamples = 10

type authLogState struct {
	mu     sync.Mutex
	until  time.Time
	counts [authLogKinds]uint64
}

// Fixed categories, not attacker-controlled account/IP keys. Summaries are
// flushed by the next authentication event after the window, including idle gaps.
func (a *authLogState) admit(kind authLogKind, now time.Time) (bool, [authLogKinds]uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var suppressed [authLogKinds]uint64
	if !now.Before(a.until) {
		for i, count := range a.counts {
			if count > authLogSamples {
				suppressed[i] = count - authLogSamples
			}
		}
		a.counts = [authLogKinds]uint64{}
		a.until = now.Add(time.Minute)
	}
	if a.counts[kind] != ^uint64(0) {
		a.counts[kind]++
	}
	return a.counts[kind] <= authLogSamples, suppressed
}

func authLogAttrs(kind authLogKind, event string) []any {
	d := authLogDefinitions[kind]
	return []any{"event", event, "auth_method", d.method, "outcome", d.outcome, "reason_code", d.reason}
}

func (s *Server) logAuth(r *http.Request, kind authLogKind, identity uuid.UUID, account string) {
	emit, suppressed := s.authLogs.admit(kind, time.Now())
	for i, count := range suppressed {
		if count != 0 {
			attrs := append(authLogAttrs(authLogKind(i), "auth.summary"), "suppressed_count", count, "window_seconds", 60)
			s.writeAuthLog(r.Context(), slog.LevelWarn, attrs)
		}
	}
	if !emit {
		return
	}
	attrs := append(authLogAttrs(kind, "auth.result"), "request_id", boundedLogField(requestID(r), 128), "source_ip", boundedLogField(s.authSource(r).String(), 64))
	if identity != uuid.Nil && kind == localSucceeded {
		attrs = append(attrs, "identity_id", identity.String())
	}
	if account != "" {
		attrs = append(attrs, "account_ref", boundedLogField(account, 67))
	}
	level := slog.LevelWarn
	if kind == localSucceeded {
		level = slog.LevelInfo
	}
	s.writeAuthLog(r.Context(), level, attrs)
}

func (s *Server) writeAuthLog(ctx context.Context, level slog.Level, attrs []any) {
	defer func() { _ = recover() }()
	if s.logger != nil {
		s.logger.Log(ctx, level, "authentication security event", attrs...)
	}
}

func boundedLogField(value string, limit int) string {
	if len(value) > limit {
		value = value[:limit]
	}
	bytes := []byte(value)
	for i, b := range bytes {
		if b < 0x20 || b > 0x7e {
			bytes[i] = '_'
		}
	}
	return string(bytes)
}

func authResultRoute(r *http.Request) bool {
	return r.URL.Path == "/api/v1/auth/login" && r.Method == http.MethodPost
}
