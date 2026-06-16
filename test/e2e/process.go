package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const (
	driverStopTimeout  = 30 * time.Second
	processReapTimeout = 2 * time.Second
)

var errProcessUnreaped = errors.New("process did not exit after kill")

func stopProcess(ctx context.Context, cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if err := ctx.Err(); err != nil {
		killErr := cmd.Process.Kill()
		return errors.Join(err, killErr, unreapedOnly(reapProcess(done)))
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		if waitErr := reapProcess(done); errors.Is(waitErr, errProcessUnreaped) {
			if processAlreadyGone(err) {
				return waitErr
			}
			return fmt.Errorf("signal process: %w", err)
		}
		// The child is already gone. Its exit status is not a stop failure.
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, driverStopTimeout)
	defer cancel()
	select {
	case <-done:
		return nil
	case <-shutdownCtx.Done():
		killErr := cmd.Process.Kill()
		return errors.Join(shutdownCtx.Err(), killErr, unreapedOnly(reapProcess(done)))
	}
}

func processAlreadyGone(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
}

func reapProcess(done <-chan error) error {
	select {
	case err := <-done:
		return err
	case <-time.After(processReapTimeout):
		return errProcessUnreaped
	}
}

func unreapedOnly(err error) error {
	if errors.Is(err, errProcessUnreaped) {
		return err
	}
	return nil
}
