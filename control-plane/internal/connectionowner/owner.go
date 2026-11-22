// Package connectionowner persists the monotonically increasing per-node
// session_epoch for the single active Controller.
package connectionowner

import "errors"

// ErrNotOwner is returned when a connection is no longer the active session
// for its node (superseded by a newer reconnect epoch or closed).
var ErrNotOwner = errors.New("connectionowner: session superseded or closed")

// ErrNoOwnerRow reports that the node has never opened a session.
var ErrNoOwnerRow = errors.New("connectionowner: node has no session row")

// Term represents one acquired per-node session epoch.
type Term struct {
	nodeID       [16]byte
	connectionID [16]byte
	epoch        int64
}

// Epoch returns the strictly increasing per-node session epoch.
func (t *Term) Epoch() int64 { return t.epoch }

// NodeID returns the node this term belongs to.
func (t *Term) NodeID() [16]byte { return t.nodeID }

// ConnectionID returns the connection identity bound to this term.
func (t *Term) ConnectionID() [16]byte { return t.connectionID }
