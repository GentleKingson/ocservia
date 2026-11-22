// Package idempotency encodes normalized HTTP mutation intents.
package idempotency

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"

	"github.com/GentleKingson/ocservia/control-plane/internal/canonicalcbor"
)

type Digest struct {
	Current [32]byte
	legacy  func() [32]byte
}

func New(domain string, value any, legacy func() [32]byte) Digest {
	hash, err := canonicalcbor.Hash("ocservia/http-intent/"+domain+"/v1\x00", value)
	if err != nil {
		panic(err)
	}
	return Digest{Current: hash, legacy: legacy}
}

// LegacyJSON reproduces persisted receipts during replay of older intents.
func LegacyJSON(domain string, value any) [32]byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return sha256.Sum256(append([]byte(domain), data...))
}

func (d Digest) Matches(stored []byte) bool {
	if bytes.Equal(d.Current[:], stored) {
		return true
	}
	if d.legacy == nil || len(stored) == 0 {
		return false
	}
	old := d.legacy()
	return bytes.Equal(old[:], stored)
}
