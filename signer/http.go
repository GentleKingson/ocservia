package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

type statusError int

func (e statusError) Error() string { return http.StatusText(int(e)) }

func (s *service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/healthz" {
		if r.Method != "GET" {
			http.Error(w, "method rejected", 405)
			return
		}
		if s.failed.Load() || time.Now().Before(s.ca.cert.NotBefore) || !time.Now().Add(24*time.Hour).Before(s.ca.cert.NotAfter) || s.db.View(func(tx *bolt.Tx) error {
			if tx.Bucket([]byte("meta")) == nil {
				return invalidState
			}
			return nil
		}) != nil {
			http.Error(w, "not ready", 503)
			return
		}
		w.WriteHeader(204)
		return
	}
	if r.URL.Path != "/sign" && r.URL.Path != "/sign/revoke" && r.URL.Path != "/sign/seal" {
		http.NotFound(w, r)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method rejected", 405)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.token)) != 1 {
		http.Error(w, "unauthorized", 401)
		return
	}
	if s.failed.Load() {
		http.Error(w, "not ready", 503)
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		http.Error(w, "busy", 503)
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	want := "application/json"
	limit := int64(64 << 10)
	if r.URL.Path == "/sign/seal" {
		want = "application/octet-stream"
		limit = 512
	}
	if err != nil || media != want {
		http.Error(w, "media type rejected", 415)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	defer clear(body)
	if err != nil {
		var large *http.MaxBytesError
		code := 400
		if errors.As(err, &large) {
			code = 413
		}
		http.Error(w, http.StatusText(code), code)
		return
	}
	var response any
	code := 200
	if r.URL.Path == "/sign/seal" {
		node, use := r.Header.Get("X-Ocservia-Node-ID"), r.Header.Get("X-Ocservia-Seal-Purpose")
		if !validUUID(node) || !purpose(use) {
			err = statusError(400)
		} else {
			response, err = s.seal(node, use, body)
		}
	} else {
		var req struct {
			ID     string `json:"certificate_id"`
			CSR    string `json:"csr_der"`
			Serial string `json:"serial_number"`
			Reason string `json:"reason"`
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF || !validUUID(req.ID) {
			err = statusError(400)
		} else if r.URL.Path == "/sign" {
			der, decodeErr := base64.StdEncoding.Strict().DecodeString(req.CSR)
			if decodeErr != nil || len(der) == 0 || req.Serial != "" || req.Reason != "" || r.Header.Get("Idempotency-Key") != req.ID {
				err = statusError(400)
			} else {
				var record issuance
				record, err = s.sign(uuid.MustParse(req.ID), der)
				response = map[string]string{"certificate_chain_pem": record.Chain}
			}
		} else {
			serial, ok := new(big.Int).SetString(req.Serial, 10)
			if r.Header.Get("Idempotency-Key") != req.ID+":revoke" || req.CSR != "" || !ok || serial.Sign() <= 0 || serial.String() != req.Serial || len(req.Serial) > 40 || strings.TrimSpace(req.Reason) == "" || len(req.Reason) > 512 {
				err = statusError(400)
			} else {
				err = s.revoke(req.ID, req.Serial, req.Reason)
				code = 204
			}
		}
	}
	if err != nil {
		var status statusError
		if errors.As(err, &status) {
			code = int(status)
		} else {
			s.failed.Store(true)
			code = 503
		}
		http.Error(w, http.StatusText(code), code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if code != 204 {
		_ = json.NewEncoder(w).Encode(response)
	}
}
