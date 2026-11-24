package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/platform/app"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
	"golang.org/x/sys/unix"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	cfg, err := config.Load(os.Args[1:], os.LookupEnv)
	if err != nil {
		slog.Error("configuration rejected", "error", err)
		os.Exit(2)
	}
	if cfg.ResetLocalAdmin {
		if err := resetLocalAdmin(cfg); err != nil {
			fmt.Fprintln(os.Stderr, "reset Local administrator:", err)
			os.Exit(1)
		}
		return
	}

	stdout := logSink(os.Stdout)
	stderr := logSink(os.Stderr)
	if stdout != os.Stdout {
		defer stdout.Close()
	}
	if stderr != os.Stderr {
		defer stderr.Close()
	}
	output := newLogOutput(stdout, stderr)
	newLogger := func(w io.Writer) *slog.Logger {
		return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: cfg.LogLevel()})).With(
			"service.name", "ocserv-control", "service.version", version, "environment", cfg.Environment)
	}
	logger := newLogger(output)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownDeadline := sync.OnceValue(func() time.Time {
		deadline := time.Now().Add(cfg.ShutdownTimeout)
		// Regular files have no poll deadline. Pipes and sockets can also
		// interrupt a Write that was already blocked before shutdown.
		_ = stdout.SetWriteDeadline(deadline)
		_ = stderr.SetWriteDeadline(deadline)
		return deadline
	})
	stopWatch := context.AfterFunc(ctx, func() { shutdownDeadline() })
	defer stopWatch()
	err = app.Run(ctx, cfg, app.BuildInfo{Version: version, Commit: commit}, logger, newLogger(accessOutput{output}), shutdownDeadline)
	shutdownDeadline()
	failed := err != nil && !errors.Is(err, context.Canceled)
	if failed {
		logger.Error("control plane stopped", "error", err)
	}
	if err := output.Close(); err != nil {
		fmt.Fprintln(stderr, "write controller logs:", err)
		failed = true
	}
	if failed {
		os.Exit(1)
	}
}

func resetLocalAdmin(cfg config.Config) error {
	for _, descriptor := range []uintptr{os.Stdin.Fd(), os.Stdout.Fd()} {
		if _, err := unix.IoctlGetWinsize(int(descriptor), unix.TIOCGWINSZ); err != nil {
			return errors.New("an interactive terminal is required for input and output")
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	username, password, err := app.ResetLocalAdminPassword(ctx, cfg)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(os.Stdout, "Username: %s\nNew password: %s\n", username, password); err != nil {
		return errors.New("password changed but terminal delivery failed; run the reset again from an interactive terminal")
	}
	return nil
}

// Go may inherit blocking pipes or sockets without registering them with
// the poller. Prefer reopening a pipe without changing its shared flags.
func logSink(file *os.File) *os.File {
	if file.SetWriteDeadline(time.Time{}) == nil {
		return file
	}
	info, err := file.Stat()
	if err != nil || info.Mode().Type() != os.ModeNamedPipe && info.Mode().Type() != os.ModeSocket {
		return file
	}
	if info.Mode().Type() == os.ModeNamedPipe {
		if pipe, err := os.OpenFile(fmt.Sprintf("/dev/fd/%d", file.Fd()), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			return pipe
		}
	}
	// Sockets and pipes inherited from another UID may not be reopenable.
	fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return file
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return file
	}
	return os.NewFile(uintptr(fd), file.Name())
}
