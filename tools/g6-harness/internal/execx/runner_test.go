package execx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunTerminatesEntireProcessGroupAfterGrace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pidPath := filepath.Join(root, "child.pid")
	ctx, cancel := context.WithTimeoutCause(context.Background(), 300*time.Millisecond, errors.New("test phase timeout"))
	defer cancel()
	outcome := Run(ctx, Spec{
		Executable:  "/bin/sh",
		Arguments:   []string{"-c", `trap '' TERM; sleep 30 & child=$!; printf '%s\n' "$child" >"$1"; wait`, "sh", pidPath},
		Directory:   root,
		Environment: os.Environ(),
		StdoutPath:  filepath.Join(root, "stdout.log"), StderrPath: filepath.Join(root, "stderr.log"),
		KillGrace: 50 * time.Millisecond,
	})
	if outcome.Cause == nil || outcome.Err == nil {
		t.Fatalf("timed process returned %+v", outcome)
	}
	content, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(content)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child process %d survived process-group escalation: %v", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunCapturesBoundedCommandResult(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outcome := Run(context.Background(), Spec{
		Executable: "/bin/sh", Arguments: []string{"-c", "printf success"}, Directory: root,
		Environment: os.Environ(), StdoutPath: filepath.Join(root, "stdout.log"), StderrPath: filepath.Join(root, "stderr.log"),
		KillGrace: time.Second,
	})
	if outcome.Err != nil || outcome.ExitCode != 0 {
		t.Fatalf("command failed: %+v", outcome)
	}
}

func TestRunClassifiesPreExecutionIOFailureAsInfrastructure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outcome := Run(context.Background(), Spec{
		Executable: "/bin/sh", Arguments: []string{"-c", "exit 0"}, Directory: root,
		Environment: os.Environ(), StdoutPath: filepath.Join(root, "missing", "stdout.log"),
		StderrPath: filepath.Join(root, "stderr.log"), KillGrace: time.Second,
	})
	if outcome.Err == nil || !outcome.Infrastructure {
		t.Fatalf("pre-execution IO failure was not classified as infrastructure: %+v", outcome)
	}
}
