package canonicalcbor

import (
	"crypto/sha256"
	"fmt"

	"github.com/fxamacker/cbor/v2"
)

var coreDetMode = func() cbor.EncMode {
	mode, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(fmt.Sprintf("initialize RFC 8949 core deterministic CBOR encoder: %v", err))
	}
	return mode
}()

// Marshal encodes value using RFC 8949 Section 4.2.1 Core Deterministic CBOR
// prefixed with domain (typically NUL-terminated for domain separation).
func Marshal(domain string, value any) ([]byte, error) {
	encoded, err := coreDetMode.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode canonical CBOR: %w", err)
	}
	out := make([]byte, 0, len(domain)+len(encoded))
	out = append(out, domain...)
	out = append(out, encoded...)
	return out, nil
}

// Hash computes SHA-256 over Marshal(domain, value).
func Hash(domain string, value any) ([32]byte, error) {
	raw, err := Marshal(domain, value)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}
