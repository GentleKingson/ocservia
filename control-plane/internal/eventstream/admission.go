package eventstream

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrGlobalLimit    = errors.New("global event stream capacity reached")
	ErrIdentityLimit  = errors.New("identity event stream capacity reached")
	ErrSessionLimit   = errors.New("session event stream capacity reached")
	ErrWorkspaceLimit = errors.New("workspace event stream capacity reached")
	ErrResourceLimit  = errors.New("resource event stream capacity reached")
	ErrClosed         = errors.New("event stream manager is closed")
)

// Config bounds every stream, watcher, and subscriber-owned queue. The
// defaults reserve database connections for ordinary API traffic: watchers
// poll using short-lived queries and can never outnumber GlobalStreams.
type Config struct {
	GlobalStreams      int
	IdentityStreams    int
	SessionStreams     int
	WorkspaceStreams   int
	ResourceStreams    int
	Watchers           int
	SubscriberQueue    int
	PollInterval       time.Duration
	DatabaseMaxBackoff time.Duration
	MaxLifetime        time.Duration
	RevalidateInterval time.Duration
	RetryAfter         time.Duration
}

func DefaultConfig() Config {
	return Config{
		GlobalStreams: 128, IdentityStreams: 8, SessionStreams: 4,
		WorkspaceStreams: 32, ResourceStreams: 16, Watchers: 64,
		SubscriberQueue: 128, PollInterval: 250 * time.Millisecond,
		DatabaseMaxBackoff: 10 * time.Second, MaxLifetime: 30 * time.Minute,
		RevalidateInterval: 30 * time.Second, RetryAfter: 5 * time.Second,
	}
}

func (c Config) Validate() error {
	if c.GlobalStreams < 1 || c.GlobalStreams > 4096 ||
		c.IdentityStreams < 1 || c.IdentityStreams > c.GlobalStreams ||
		c.SessionStreams < 1 || c.SessionStreams > c.IdentityStreams ||
		c.WorkspaceStreams < 1 || c.WorkspaceStreams > c.GlobalStreams ||
		c.ResourceStreams < 1 || c.ResourceStreams > c.WorkspaceStreams ||
		c.Watchers < 1 || c.Watchers > c.GlobalStreams ||
		c.SubscriberQueue < 8 || c.SubscriberQueue > 4096 ||
		c.PollInterval < 100*time.Millisecond || c.PollInterval > 5*time.Second ||
		c.DatabaseMaxBackoff < c.PollInterval || c.DatabaseMaxBackoff > time.Minute ||
		c.MaxLifetime < time.Minute || c.MaxLifetime > 24*time.Hour ||
		c.RevalidateInterval < 5*time.Second || c.RevalidateInterval > c.MaxLifetime ||
		c.RetryAfter < time.Second || c.RetryAfter > time.Minute {
		return errors.New("event stream configuration is outside the safe range")
	}
	return nil
}

type AdmissionKey struct {
	Identity  string
	Session   string
	Workspace string
	Resource  string
}

func (k AdmissionKey) valid() bool {
	return k.Identity != "" && k.Session != "" && k.Workspace != "" && k.Resource != ""
}

type AdmissionSnapshot struct {
	Active            int
	IdentityEntries   int
	SessionEntries    int
	WorkspaceEntries  int
	ResourceEntries   int
	RejectedGlobal    uint64
	RejectedIdentity  uint64
	RejectedSession   uint64
	RejectedWorkspace uint64
	RejectedResource  uint64
}

type Manager struct {
	mu         sync.Mutex
	config     Config
	closed     bool
	active     int
	identities map[string]int
	sessions   map[string]int
	workspaces map[string]int
	resources  map[string]int
	rejected   [5]uint64
}

func NewManager(config Config) (*Manager, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Manager{
		config: config, identities: make(map[string]int), sessions: make(map[string]int),
		workspaces: make(map[string]int), resources: make(map[string]int),
	}, nil
}

func (m *Manager) Acquire(key AdmissionKey) (*Lease, error) {
	if !key.valid() {
		return nil, errors.New("event stream admission key is incomplete")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	if m.active >= m.config.GlobalStreams {
		m.rejected[0]++
		return nil, ErrGlobalLimit
	}
	if m.identities[key.Identity] >= m.config.IdentityStreams {
		m.rejected[1]++
		return nil, ErrIdentityLimit
	}
	if m.sessions[key.Session] >= m.config.SessionStreams {
		m.rejected[2]++
		return nil, ErrSessionLimit
	}
	if m.workspaces[key.Workspace] >= m.config.WorkspaceStreams {
		m.rejected[3]++
		return nil, ErrWorkspaceLimit
	}
	if m.resources[key.Resource] >= m.config.ResourceStreams {
		m.rejected[4]++
		return nil, ErrResourceLimit
	}
	m.active++
	m.identities[key.Identity]++
	m.sessions[key.Session]++
	m.workspaces[key.Workspace]++
	m.resources[key.Resource]++
	return &Lease{manager: m, key: key}, nil
}

func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
}

func (m *Manager) Snapshot() AdmissionSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return AdmissionSnapshot{
		Active: m.active, IdentityEntries: len(m.identities), SessionEntries: len(m.sessions),
		WorkspaceEntries: len(m.workspaces), ResourceEntries: len(m.resources),
		RejectedGlobal: m.rejected[0], RejectedIdentity: m.rejected[1],
		RejectedSession: m.rejected[2], RejectedWorkspace: m.rejected[3],
		RejectedResource: m.rejected[4],
	}
}

type Lease struct {
	once    sync.Once
	manager *Manager
	key     AdmissionKey
}

func (l *Lease) Release() {
	if l == nil || l.manager == nil {
		return
	}
	l.once.Do(func() {
		m := l.manager
		m.mu.Lock()
		defer m.mu.Unlock()
		m.active--
		decrement(m.identities, l.key.Identity)
		decrement(m.sessions, l.key.Session)
		decrement(m.workspaces, l.key.Workspace)
		decrement(m.resources, l.key.Resource)
	})
}

func decrement(values map[string]int, key string) {
	if values[key] <= 1 {
		delete(values, key)
		return
	}
	values[key]--
}
