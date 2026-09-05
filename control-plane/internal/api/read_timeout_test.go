package api

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRequestReadTimeout(t *testing.T) {
	s := New("127.0.0.1:0", nil, BuildInfo{}, slog.Default(), 1024, 50*time.Millisecond, false, "", 0)
	// Scale the production deadline down; keep it longer than the handler timeout.
	s.http.ReadTimeout /= 150
	readErrors := make(chan error, 2)
	s.http.Handler = s.timeout(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		readErrors <- err
	}))
	listener, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.http.Close()
	go s.http.Serve(listener)

	for _, framing := range []string{"Content-Length: 100\r\n\r\nx", "Transfer-Encoding: chunked\r\n\r\n64\r\nx"} {
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.WriteString(conn, "POST / HTTP/1.1\r\nHost: localhost\r\n"+framing)
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		select {
		case err := <-readErrors:
			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() {
				t.Errorf("incomplete body read = %v, want timeout", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("body read outlived both handler and server deadlines")
		}
		conn.Close()
	}
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Post("http://"+listener.Addr().String(), "text/plain", strings.NewReader("complete"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if err := <-readErrors; err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("complete request: status %d, read error %v", response.StatusCode, err)
	}
}

func TestReadTimeoutPreservesStreamingWrites(t *testing.T) {
	s := New("127.0.0.1:0", nil, BuildInfo{}, slog.Default(), 1024, 50*time.Millisecond, false, "", 0)
	s.http.ReadTimeout /= 150
	s.http.Handler = s.timeout(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		time.Sleep(2 * s.http.ReadTimeout)
		_, _ = io.WriteString(w, "data: still open\n\n")
	}))
	listener, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.http.Close()
	go s.http.Serve(listener)
	client := &http.Client{Timeout: 2 * time.Second}
	for _, path := range []string{"/api/v1/events/stream", "/api/v1/operations/example/events"} {
		response, err := client.Get("http://" + listener.Addr().String() + path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || string(body) != "data: still open\n\n" {
			t.Fatalf("%s stream after read deadline: body %q, error %v", path, body, err)
		}
	}
}
