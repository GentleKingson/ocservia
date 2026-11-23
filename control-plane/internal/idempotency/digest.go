// Package idempotency encodes normalized HTTP mutation intents.
package idempotency

import (
	"bytes"

	"github.com/GentleKingson/ocservia/control-plane/internal/canonicalcbor"
)

type Digest struct {
	Current [32]byte
}

func New(domain string, value any) Digest {
	hash, err := canonicalcbor.Hash("ocservia/http-intent/"+domain+"/v1\x00", value)
	if err != nil {
		panic(err)
	}
	return Digest{Current: hash}
}

func (d Digest) Matches(stored []byte) bool {
	return bytes.Equal(d.Current[:], stored)
}
