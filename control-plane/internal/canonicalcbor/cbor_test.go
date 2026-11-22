package canonicalcbor

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestMarshalMatchesRFC8949CoreDeterministicBytes(t *testing.T) {
	type sample struct {
		ZStr       string   `cbor:"z_str"`
		A          uint64   `cbor:"a"`
		Neg        int64    `cbor:"neg"`
		Raw        []byte   `cbor:"raw"`
		OptPresent []byte   `cbor:"opt_present"`
		OptAbsent  []byte   `cbor:"opt_absent"`
		Flag       bool     `cbor:"flag"`
		List       []string `cbor:"list"`
	}
	raw, err := Marshal("ocservia/test/v1\x00", sample{
		ZStr:       "hello",
		A:          42,
		Neg:        -7,
		Raw:        []byte{0xde, 0xad, 0xbe, 0xef},
		OptPresent: []byte{1, 2},
		OptAbsent:  nil,
		Flag:       true,
		List:       []string{"alpha", "beta"},
	})
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	domain := []byte("ocservia/test/v1\x00")
	if !bytes.HasPrefix(raw, domain) {
		t.Fatalf("missing domain prefix")
	}
	const wantHex = "a86161182a636e6567266372617744deadbeef64666c6167f5646c6973748265616c7068616462657461657a5f7374726568656c6c6f6a6f70745f616273656e74f66b6f70745f70726573656e74420102"
	gotHex := hex.EncodeToString(raw[len(domain):])
	if gotHex != wantHex {
		t.Fatalf("CBOR mismatch:\n got: %s\nwant: %s", gotHex, wantHex)
	}
}
