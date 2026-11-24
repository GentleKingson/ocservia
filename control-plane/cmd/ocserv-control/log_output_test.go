package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestLogOutputDrainsOwnedRecords(t *testing.T) {
	var dst bytes.Buffer
	out := newLogOutput(&dst, io.Discard)
	record := []byte("first\n")
	if _, err := out.Write(record); err != nil {
		t.Fatal(err)
	}
	copy(record, "other\n")
	if _, err := out.Write(record); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil || dst.String() != "first\nother\n" {
		t.Fatalf("drained %q, error %v", dst.String(), err)
	}
	if _, err := out.Write(record); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write after shutdown: %v", err)
	}
}

func TestLogOutputFlushesToStderrOnSinkFailure(t *testing.T) {
	reader, dst := io.Pipe()
	_ = reader.Close()
	fallback, stderr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer fallback.Close()
	defer stderr.Close()
	out := newLogOutput(dst, stderr)
	defer out.Close()
	if _, err := out.Write([]byte("record\n")); err != nil {
		t.Fatal(err)
	}
	if err := fallback.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var record [7]byte
	if _, err := io.ReadFull(fallback, record[:]); err != nil || string(record[:]) != "record\n" {
		t.Fatalf("periodic flush wrote %q, error %v", record, err)
	}
}

func TestAccessLogSaturation(t *testing.T) {
	reader, dst := io.Pipe()
	defer reader.Close()
	defer dst.Close()
	out := newLogOutput(dst, io.Discard)
	access := accessOutput{out}
	// Reading one byte leaves the worker blocked inside its first flush.
	_, _ = out.Write(bytes.Repeat([]byte("x"), 32<<10))
	var first [1]byte
	if _, err := io.ReadFull(reader, first[:]); err != nil {
		t.Fatal(err)
	}
	for range cap(out.records) {
		_, _ = access.Write([]byte("access\n"))
	}
	critical := make(chan error, 1)
	go func() { _, err := out.Write([]byte("critical\n")); critical <- err }()
	const extra = 13
	returned := make(chan struct{})
	go func() {
		for range extra {
			_, _ = access.Write([]byte("excess\n"))
		}
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("full log queue blocked access logging")
	}
	drained := make(chan []byte, 1)
	go func() { data, _ := io.ReadAll(reader); drained <- data }()
	if err := <-critical; err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	data := <-drained
	if bytes.Count(data, []byte("access\n")) != 256 || !bytes.Contains(data, []byte("critical\n")) ||
		bytes.Contains(data, []byte("excess\n")) || bytes.Count(data, []byte(`"skipped_count":13}`)) != 1 {
		t.Fatal("incorrect access loss accounting or missing critical log")
	}
}

func TestLogShutdownInterruptsBlockedOutputs(t *testing.T) {
	sinks := make([]*os.File, 2)
	for i := range sinks {
		var fds [2]int
		if err := syscall.Pipe(fds[:]); err != nil {
			t.Fatal(err)
		}
		reader := os.NewFile(uintptr(fds[0]), "reader")
		writer := os.NewFile(uintptr(fds[1]), "inherited")
		defer reader.Close()
		defer writer.Close()
		sinks[i] = logSink(writer)
		if sinks[i] != writer {
			defer sinks[i].Close()
		}
	}
	out := newLogOutput(sinks[0], sinks[1])
	_, _ = out.Write(bytes.Repeat([]byte("x"), 2<<20))
	deadline := time.Now().Add(100 * time.Millisecond)
	for _, sink := range sinks {
		if err := sink.SetWriteDeadline(deadline); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- out.Close() }()
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("wanted sink timeout, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not interrupt blocked log outputs")
	}
}

func TestLogShutdownUnblocksFullQueueWriter(t *testing.T) {
	reader, dst := io.Pipe()
	defer reader.Close()
	defer dst.Close()
	out := newLogOutput(dst, io.Discard)
	// Block the consumer goroutine inside its first flush to dst.
	if _, err := out.Write(bytes.Repeat([]byte("x"), 32<<10)); err != nil {
		t.Fatal(err)
	}
	var first [1]byte
	if _, err := io.ReadFull(reader, first[:]); err != nil {
		t.Fatal(err)
	}
	for range cap(out.records) {
		if _, err := out.Write([]byte("fill\n")); err != nil {
			t.Fatal(err)
		}
	}
	writeErr := make(chan error, 1)
	go func() {
		_, err := out.Write([]byte("blocked\n"))
		writeErr <- err
	}()
	// Give the blocked writer a moment to enter the channel send under RLock.
	time.Sleep(20 * time.Millisecond)

	// Unblock dst concurrently only after Close starts so Close must interrupt the
	// blocked writer rather than deadlocking waiting for RLock.
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- out.Close()
	}()
	select {
	case err := <-writeErr:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("blocked writer returned %v, want io.ErrClosedPipe", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close deadlocked waiting on blocked full-queue writer")
	}
	_ = reader.Close()
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not complete after sink closed")
	}
	// Calling Close a second time must remain safe and idempotent.
	_ = out.Close()
}
