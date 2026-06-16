package e2e

import (
	"bufio"
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestStopProcessUsesTheCallerShutdownBudget(t *testing.T) {
	cmd := exec.Command("sh", "-c", `trap '' TERM; echo ready; while :; do :; done`)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("open subprocess output: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start subprocess: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
		}
	})
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatalf("wait for subprocess readiness: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = stopProcess(ctx, cmd)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stopProcess() = %v, want caller deadline", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("stopProcess() took %s, want the short caller budget", elapsed)
	}
	if cmd.ProcessState == nil {
		t.Fatal("stopProcess() returned before the subprocess was reaped")
	}
}

func TestStopProcessKillsWhenTheCallerContextAlreadyEnded(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start subprocess: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := stopProcess(ctx, cmd)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stopProcess() = %v, want the caller's cancellation", err)
	}
	if cmd.ProcessState == nil {
		t.Fatal("stopProcess() returned before the killed subprocess was reaped")
	}
}

func TestStopProcessAcceptsAnAlreadyDeadProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start subprocess: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill subprocess: %v", err)
	}

	err := stopProcess(t.Context(), cmd)
	if err != nil {
		t.Fatalf("stopProcess() = %v, want nil for an already-dead process", err)
	}
	if cmd.ProcessState == nil {
		t.Fatal("stopProcess() did not reap the already-dead process")
	}
}
