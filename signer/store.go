package main

import (
	"bytes"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

var invalidState = errors.New("invalid signer state")
var buckets = []string{"meta", "issued", "bindings", "audit"}

type issuance struct {
	CSR       []byte     `json:"csr"`
	Digest    string     `json:"csr_sha256"`
	Chain     string     `json:"certificate_chain_pem"`
	Serial    string     `json:"serial_number"`
	Reason    string     `json:"reason,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

type service struct {
	// ponytail: one bbolt writer serializes issuance; no HA or multi-writer mode.
	db     *bolt.DB
	ca     *authority
	token  string
	failed atomic.Bool
	slots  chan struct{}
	// Test-only crash injection; never configured by environment or HTTP.
	boundary func(string)
}

func privateFile(path string) ([]byte, error) {
	if err := protectedParent(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || stat.Nlink != 1 || (stat.Uid != uint32(os.Geteuid()) && stat.Uid != 0) || info.Size() > 1<<20 {
		return nil, errors.New("private file permissions rejected")
	}
	return os.ReadFile(path)
}

func protectedParent(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("absolute canonical path required")
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode().Perm()&0022 != 0 || (stat.Uid != uint32(os.Geteuid()) && stat.Uid != 0) {
			return errors.New("protected path ancestry required")
		}
		if dir == "/" {
			return nil
		}
	}
}

func openStore(path string, ca *authority, initialize bool) (*service, error) {
	if err := protectedParent(path); err != nil {
		return nil, err
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
		return nil, errors.New("state directory must exist with mode 0700")
	}
	if initialize {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		if err = f.Close(); err != nil {
			return nil, err
		}
	} else {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() == 0 {
			return nil, invalidState
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
			return nil, invalidState
		}
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	s := &service{db: db, ca: ca, slots: make(chan struct{}, 32)}
	if initialize {
		err = db.Update(func(tx *bolt.Tx) error {
			for _, name := range buckets {
				if _, err := tx.CreateBucket([]byte(name)); err != nil {
					return err
				}
			}
			for k, v := range map[string]string{"version": "1", "issuer": ca.id, "policy": policy} {
				if err := tx.Bucket([]byte("meta")).Put([]byte(k), []byte(v)); err != nil {
					return err
				}
			}
			return nil
		})
		if err == nil {
			d, e := os.Open(filepath.Dir(path))
			if e != nil {
				err = e
			} else {
				err = d.Sync()
				d.Close()
			}
		}
	}
	if err == nil {
		err = s.check()
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *service) check() error {
	return s.db.View(func(tx *bolt.Tx) error {
		var corrupt bool
		for err := range tx.Check() {
			if err != nil {
				corrupt = true
			}
		}
		if corrupt {
			return invalidState
		}
		for _, name := range buckets {
			if tx.Bucket([]byte(name)) == nil {
				return invalidState
			}
		}
		if b := tx.Bucket([]byte("audit")); b.Sequence() != uint64(b.Stats().KeyN) {
			return invalidState
		}
		m := tx.Bucket([]byte("meta"))
		if string(m.Get([]byte("version"))) != "1" || string(m.Get([]byte("issuer"))) != s.ca.id || string(m.Get([]byte("policy"))) != policy {
			return invalidState
		}
		for _, name := range buckets[1:] {
			if err := tx.Bucket([]byte(name)).ForEach(func(k, v []byte) error {
				if v == nil || string(m.Get([]byte("sha256:"+name+":"+string(k)))) != digest(v) {
					return invalidState
				}
				return nil
			}); err != nil {
				return err
			}
		}
		if err := m.ForEach(func(k, v []byte) error {
			if !strings.HasPrefix(string(k), "sha256:") {
				return nil
			}
			parts := strings.SplitN(string(k), ":", 3)
			if len(parts) != 3 || tx.Bucket([]byte(parts[1])) == nil || string(v) != digest(tx.Bucket([]byte(parts[1])).Get([]byte(parts[2]))) {
				return invalidState
			}
			return nil
		}); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("issued")).ForEach(func(k, v []byte) error {
			var r issuance
			id, err := uuid.Parse(string(k))
			if err != nil || json.Unmarshal(v, &r) != nil || r.Digest != digest(r.CSR) || r.Serial != new(big.Int).SetBytes(id[:]).String() {
				return invalidState
			}
			csr, err := parseCSR(r.CSR)
			if err != nil {
				return invalidState
			}
			block, rest := pem.Decode([]byte(r.Chain))
			if block == nil || block.Type != "CERTIFICATE" || !bytes.Equal(rest, s.ca.chain) {
				return invalidState
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil || cert.CheckSignatureFrom(s.ca.cert) != nil || cert.SerialNumber.String() != r.Serial || !bytes.Equal(cert.RawSubjectPublicKeyInfo, csr.RawSubjectPublicKeyInfo) || !bytes.Equal(cert.RawSubject, csr.RawSubject) || (r.RevokedAt == nil) != (r.Reason == "") {
				return invalidState
			}
			return nil
		}); err != nil {
			return err
		}
		return tx.Bucket([]byte("bindings")).ForEach(func(k, v []byte) error {
			var b binding
			if json.Unmarshal(v, &b) != nil || b.NodeID != string(k) || validateBinding(b) != nil {
				return invalidState
			}
			return nil
		})
	})
}

func (s *service) mark(stage string) {
	if s.boundary != nil {
		s.boundary(stage)
	}
}

func audit(tx *bolt.Tx, action, id string) error {
	b := tx.Bucket([]byte("audit"))
	n, err := b.NextSequence()
	if err != nil {
		return err
	}
	data, err := json.Marshal(map[string]any{"action": action, "id": id, "at": time.Now().UTC()})
	if err != nil {
		return err
	}
	return putRecord(tx, "audit", []byte(fmt.Sprintf("%020d", n)), data)
}

func putRecord(tx *bolt.Tx, bucket string, key, data []byte) error {
	if err := tx.Bucket([]byte(bucket)).Put(key, data); err != nil {
		return err
	}
	return tx.Bucket([]byte("meta")).Put([]byte("sha256:"+bucket+":"+string(key)), []byte(digest(data)))
}

func (s *service) sign(id uuid.UUID, der []byte) (issuance, error) {
	var result issuance
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("issued"))
		key := []byte(id.String())
		if old := b.Get(key); old != nil {
			if json.Unmarshal(old, &result) != nil {
				return invalidState
			}
			if result.Digest != digest(der) {
				return statusError(409)
			}
			return nil
		}
		csr, err := parseCSR(der)
		if err != nil {
			return statusError(422)
		}
		s.mark("prepare")
		serial := new(big.Int).SetBytes(id[:])
		chain, err := s.ca.issue(csr, serial)
		if err != nil {
			return err
		}
		s.mark("signed")
		result = issuance{CSR: der, Digest: digest(der), Chain: string(chain), Serial: serial.String()}
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if err := putRecord(tx, "issued", key, data); err != nil {
			return err
		}
		if err := audit(tx, "issue", id.String()); err != nil {
			return err
		}
		s.mark("commit")
		return nil
	})
	if err == nil {
		s.mark("reply")
	}
	return result, err
}

func (s *service) revoke(id, serial, reason string) error {
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("issued"))
		old := b.Get([]byte(id))
		if old == nil {
			return statusError(404)
		}
		var r issuance
		if json.Unmarshal(old, &r) != nil {
			return invalidState
		}
		if serial != r.Serial || (r.RevokedAt != nil && r.Reason != reason) {
			return statusError(409)
		}
		if r.RevokedAt != nil {
			return nil
		}
		now := time.Now().UTC()
		r.Reason = reason
		r.RevokedAt = &now
		data, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if err := putRecord(tx, "issued", []byte(id), data); err != nil {
			return err
		}
		if err := audit(tx, "revoke", id); err != nil {
			return err
		}
		s.mark("commit")
		return nil
	})
	if err == nil {
		s.mark("reply")
	}
	return err
}

func (s *service) backup(path string) error {
	if err := protectedParent(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := s.db.View(func(tx *bolt.Tx) error { _, err := tx.WriteTo(f); return err }); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *service) restore(path string, minimum int64) error {
	if minimum < 0 {
		return errors.New("restore requires an independently reconciled minimum revision")
	}
	if err := s.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("audit")).Sequence() < uint64(minimum) {
			return errors.New("snapshot is older than required state; reconcile before restore")
		}
		return nil
	}); err != nil {
		return err
	}
	return s.backup(path)
}

func (s *service) crl(w io.Writer) error {
	var entries []x509.RevocationListEntry
	var revision uint64
	err := s.db.Update(func(tx *bolt.Tx) error {
		if err := audit(tx, "crl", s.ca.id); err != nil {
			return err
		}
		revision = tx.Bucket([]byte("audit")).Sequence()
		return tx.Bucket([]byte("issued")).ForEach(func(_, v []byte) error {
			var r issuance
			if json.Unmarshal(v, &r) != nil {
				return invalidState
			}
			if r.RevokedAt != nil {
				serial, ok := new(big.Int).SetString(r.Serial, 10)
				if !ok {
					return invalidState
				}
				entries = append(entries, x509.RevocationListEntry{SerialNumber: serial, RevocationTime: *r.RevokedAt})
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if now.Before(s.ca.cert.NotBefore) || !now.Add(time.Hour).Before(s.ca.cert.NotAfter) {
		return invalidState
	}
	der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{Number: new(big.Int).SetUint64(revision), ThisUpdate: now, NextUpdate: now.Add(time.Hour), RevokedCertificateEntries: entries}, s.ca.cert, s.ca.key)
	if err != nil {
		return err
	}
	return pem.Encode(w, &pem.Block{Type: "X509 CRL", Bytes: der})
}
