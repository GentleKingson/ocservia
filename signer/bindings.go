package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

type descriptor struct {
	Purpose string `json:"purpose"`
	Version int    `json:"version"`
	KeyID   string `json:"key_id"`
	Digest  string `json:"public_key_sha256"`
	DER     []byte `json:"public_key_der,omitempty"`
}

type binding struct {
	WorkspaceID  string       `json:"workspace_id"`
	NodeID       string       `json:"node_id"`
	EndpointID   string       `json:"endpoint_id"`
	ApprovalID   string       `json:"approval_id"`
	ApprovalHash string       `json:"approval_hash"`
	ExportedAt   time.Time    `json:"exported_at"`
	Keys         []descriptor `json:"keys"`
	Disabled     bool         `json:"disabled,omitempty"`
}

func validUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}
func validDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func purpose(s string) bool { return s == "user_password" || s == "certificate_p12_password" }

func validateBinding(b binding) error {
	if !validUUID(b.WorkspaceID) || !validUUID(b.NodeID) || !validUUID(b.ApprovalID) || !validDigest(b.ApprovalHash) || !validDigest(b.EndpointID) || b.ExportedAt.IsZero() || len(b.Keys) != 2 {
		return errors.New("invalid approved binding")
	}
	for _, k := range b.Keys {
		if !purpose(k.Purpose) || k.Version != 1 || !validName(k.KeyID, 128) || !validDigest(k.Digest) || digest(k.DER) != k.Digest {
			return errors.New("key descriptor mismatch")
		}
		pub, err := x509.ParsePKIXPublicKey(k.DER)
		if err != nil {
			return err
		}
		key, ok := pub.(*rsa.PublicKey)
		if !ok || key.N.BitLen() < 2048 || key.N.BitLen() > 4096 || key.E != 65537 {
			return errors.New("invalid sealing RSA key")
		}
	}
	if b.Keys[0].Purpose == b.Keys[1].Purpose || b.Keys[0].KeyID == b.Keys[1].KeyID || b.Keys[0].Digest == b.Keys[1].Digest {
		return errors.New("purpose keys must be distinct")
	}
	return nil
}

func (s *service) importBinding(approved, exported binding) error {
	if approved.Disabled || approved.ExportedAt.After(time.Now().Add(time.Minute)) || time.Since(approved.ExportedAt) > 15*time.Minute || approved.NodeID != exported.NodeID || approved.EndpointID != exported.EndpointID || len(approved.Keys) != 2 || len(exported.Keys) != 2 {
		return errors.New("stale or mismatched enrollment export")
	}
	for i, k := range approved.Keys {
		matched := false
		for _, public := range exported.Keys {
			if public.Purpose == k.Purpose && public.Version == k.Version && public.KeyID == k.KeyID && public.Digest == k.Digest && digest(public.DER) == k.Digest {
				approved.Keys[i].DER = public.DER
				matched = true
			}
		}
		if !matched {
			return errors.New("public key does not match approved enrollment")
		}
	}
	if err := validateBinding(approved); err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("bindings"))
		key := []byte(approved.NodeID)
		if old := b.Get(key); old != nil {
			var previous binding
			if json.Unmarshal(old, &previous) != nil || previous.Disabled {
				return errors.New("disabled binding cannot be replaced")
			}
			approved.ExportedAt = previous.ExportedAt
			want, _ := json.Marshal(approved)
			if !bytes.Equal(old, want) {
				return errors.New("binding replacement forbidden")
			}
			return nil
		}
		data, err := json.Marshal(approved)
		if err != nil {
			return err
		}
		if err := putRecord(tx, "bindings", key, data); err != nil {
			return err
		}
		return audit(tx, "import:"+approved.ApprovalID, approved.NodeID)
	})
}

func (s *service) disable(node string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("bindings"))
		var v binding
		if json.Unmarshal(b.Get([]byte(node)), &v) != nil {
			return errors.New("binding not found")
		}
		if v.Disabled {
			return nil
		}
		v.Disabled = true
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if err := putRecord(tx, "bindings", []byte(node), data); err != nil {
			return err
		}
		return audit(tx, "disable", node)
	})
}

func (s *service) seal(node, use string, plaintext []byte) (map[string]any, error) {
	var selected descriptor
	err := s.db.View(func(tx *bolt.Tx) error {
		var b binding
		data := tx.Bucket([]byte("bindings")).Get([]byte(node))
		if data == nil {
			return statusError(403)
		}
		if json.Unmarshal(data, &b) != nil {
			return invalidState
		}
		if b.Disabled {
			return statusError(403)
		}
		for _, k := range b.Keys {
			if k.Purpose == use {
				selected = k
				return nil
			}
		}
		return statusError(403)
	})
	if err != nil {
		return nil, err
	}
	pub, err := x509.ParsePKIXPublicKey(selected.DER)
	if err != nil {
		return nil, invalidState
	}
	key, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, invalidState
	}
	if len(plaintext) == 0 {
		return nil, statusError(400)
	}
	if len(plaintext) > key.Size()-2*sha256.Size-2 {
		return nil, statusError(413)
	}
	encrypted, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, key, plaintext, nil)
	if err != nil {
		return nil, err
	}
	return map[string]any{"sealed": encrypted, "key_id": selected.KeyID, "version": selected.Version, "purpose": use}, nil
}
