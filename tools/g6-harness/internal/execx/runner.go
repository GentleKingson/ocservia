package execx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type Spec struct {
	Executable  string
	Arguments   []string
	Directory   string
	Environment []string
	StdoutPath  string
	StderrPath  string
	KillGrace   time.Duration
}

type Outcome struct {
	ExitCode       int
	Err            error
	Cause          error
	Infrastructure bool
}

func Run(ctx context.Context, spec Spec) Outcome {
	if spec.KillGrace <= 0 || spec.KillGrace > time.Minute {
		return Outcome{ExitCode: -1, Err: errors.New("process kill grace is outside the bounded contract"), Infrastructure: true}
	}
	stdout, err := os.OpenFile(spec.StdoutPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Outcome{ExitCode: -1, Err: err, Infrastructure: true}
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(spec.StderrPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Outcome{ExitCode: -1, Err: err, Infrastructure: true}
	}
	defer stderr.Close()

	command := exec.CommandContext(ctx, spec.Executable, spec.Arguments...)
	command.Dir = spec.Directory
	command.Env = spec.Environment
	command.Stdout = stdout
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = spec.KillGrace + time.Second

	var timerMu sync.Mutex
	var killTimer *time.Timer
	var killDone chan struct{}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		pid := command.Process.Pid
		signalErr := syscall.Kill(-pid, syscall.SIGTERM)
		if signalErr != nil && !errors.Is(signalErr, syscall.ESRCH) {
			return signalErr
		}
		timerMu.Lock()
		killDone = make(chan struct{})
		killTimer = time.AfterFunc(spec.KillGrace, func() {
			defer close(killDone)
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		})
		timerMu.Unlock()
		return nil
	}

	runErr := command.Run()
	timerMu.Lock()
	timer := killTimer
	done := killDone
	timerMu.Unlock()
	if timer != nil && !timer.Stop() {
		<-done
	}
	flushErr := errors.Join(stdout.Sync(), stderr.Sync())
	infrastructure := false
	if runErr == nil && flushErr != nil {
		runErr = flushErr
		infrastructure = true
	}
	outcome := Outcome{ExitCode: 0, Err: runErr, Infrastructure: infrastructure}
	if runErr != nil {
		outcome.ExitCode = -1
		var exitError *exec.ExitError
		if errors.As(runErr, &exitError) {
			outcome.ExitCode = exitError.ExitCode()
		}
		var execError *exec.Error
		if errors.As(runErr, &execError) {
			outcome.Infrastructure = true
		}
	}
	if ctx.Err() != nil {
		outcome.Cause = context.Cause(ctx)
		if outcome.Cause == nil {
			outcome.Cause = ctx.Err()
		}
	}
	return outcome
}

func ExitDescription(outcome Outcome) string {
	if outcome.Cause != nil {
		return outcome.Cause.Error()
	}
	if outcome.Err != nil {
		return outcome.Err.Error()
	}
	return fmt.Sprintf("exit code %d", outcome.ExitCode)
}
