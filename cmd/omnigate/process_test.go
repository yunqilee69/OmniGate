package main

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestIsProcessRunning_reportsCurrentProcessAlive(t *testing.T) {
	if !isProcessRunning(os.Getpid()) {
		t.Fatalf("isProcessRunning(%d)=false, want true for current process", os.Getpid())
	}
}

func TestIsProcessRunning_reportsMissingPIDDead(t *testing.T) {
	const missingPID = 1_000_000_000
	if isProcessRunning(missingPID) {
		t.Fatalf("isProcessRunning(%d)=true, want false for missing pid", missingPID)
	}
}

func TestTerminateProcess_stopsChild(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if runtime.GOOS == "windows" {
		cmd = exec.Command("ping", "127.0.0.1", "-n", "60")
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	if !isProcessRunning(pid) {
		t.Fatalf("child pid %d should be running", pid)
	}
	if err := terminateProcess(pid); err != nil {
		t.Fatalf("terminateProcess: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not exit after terminateProcess")
	}
	if isProcessRunning(pid) {
		t.Fatalf("child pid %d still running after terminateProcess", pid)
	}
}
