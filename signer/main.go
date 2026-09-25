package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	bolt "go.etcd.io/bbolt"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "signer operation failed:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return errors.New("expected serve, init, import, disable, backup, inspect, crl or health")
	}
	command := os.Args[1]
	f := flag.NewFlagSet("ocserv-signer", flag.ContinueOnError)
	state := f.String("state", "/var/lib/ocservia-signer/ledger.db", "ledger")
	chain := f.String("ca-chain", "/run/secrets/issuer-chain.pem", "intermediate-first CA chain")
	key := f.String("ca-key", "/run/secrets/issuer-key.pem", "online intermediate private key")
	cert := f.String("tls-cert", "/run/secrets/tls-cert.pem", "HTTPS certificate")
	tlsKey := f.String("tls-key", "/run/secrets/tls-key.pem", "HTTPS private key")
	token := f.String("token-file", "/run/secrets/api-token", "Bearer token file")
	listen := f.String("listen", ":9443", "HTTPS listener")
	approved := f.String("approved", "", "protected Controller export")
	public := f.String("public", "", "node public-key export")
	node := f.String("node", "", "expected node UUID")
	workspace := f.String("workspace", "", "expected workspace UUID")
	endpoint := f.String("endpoint", "", "expected endpoint hex identity")
	output := f.String("output", "", "new backup destination")
	minimumRevision := f.Int64("minimum-revision", -1, "independently reconciled minimum restore revision")
	healthURL := f.String("url", "https://signer:9443/healthz", "health URL with verified SAN")
	trustFile := f.String("tls-ca", "/run/secrets/tls-ca.pem", "health client trust")
	if err := f.Parse(os.Args[2:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	if command == "health" {
		b, err := os.ReadFile(*trustFile)
		if err != nil {
			return err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(b) {
			return errors.New("invalid HTTPS CA")
		}
		client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect forbidden") }}
		resp, err := client.Get(*healthURL)
		if err != nil {
			return errors.New("health request failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != 204 {
			return errors.New("not ready")
		}
		return nil
	}
	switch command {
	case "serve", "init", "import", "disable", "backup", "restore", "inspect", "crl":
	default:
		return errors.New("unknown command")
	}
	ca, err := loadAuthority(*chain, *key)
	if err != nil {
		return errors.New("CA configuration rejected")
	}
	s, err := openStore(*state, ca, command == "init")
	if err != nil {
		return err
	}
	defer s.db.Close()
	switch command {
	case "init":
		return nil
	case "inspect":
		return s.db.View(func(tx *bolt.Tx) error {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"state_version": 1, "issuer_sha256": ca.id, "policy": policy, "revision": tx.Bucket([]byte("audit")).Sequence()})
		})
	case "import":
		var a, p binding
		b, err := privateFile(*approved)
		if err != nil {
			return err
		}
		if json.Unmarshal(b, &a) != nil {
			return errors.New("invalid approval export")
		}
		b, err = privateFile(*public)
		if err != nil {
			return err
		}
		if json.Unmarshal(b, &p) != nil {
			return errors.New("invalid public export")
		}
		if !validUUID(*workspace) || !validUUID(*node) || !validDigest(*endpoint) || a.WorkspaceID != *workspace || a.NodeID != *node || a.EndpointID != *endpoint {
			return errors.New("expected identity mismatch")
		}
		return s.importBinding(a, p)
	case "disable":
		if !validUUID(*node) {
			return errors.New("invalid node")
		}
		return s.disable(*node)
	case "backup":
		return s.backup(*output)
	case "restore":
		return s.restore(*output, *minimumRevision)
	case "crl":
		return s.crl(os.Stdout)
	}
	b, err := privateFile(*token)
	if err != nil {
		return err
	}
	s.token = strings.TrimSpace(string(b))
	clear(b)
	if len(s.token) < 32 || len(s.token) > 256 || strings.ContainsAny(s.token, " \t\r\n") {
		return errors.New("API token rejected")
	}
	tlsPEM, err := privateFile(*tlsKey)
	if err != nil {
		return err
	}
	clear(tlsPEM)
	pair, err := tls.LoadX509KeyPair(*cert, *tlsKey)
	if err != nil {
		return errors.New("HTTPS identity rejected")
	}
	server := &http.Server{Addr: *listen, Handler: s, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServeTLS("", "") }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			server.Close()
			return err
		}
		<-done
	}
	return nil
}
