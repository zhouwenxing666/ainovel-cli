//go:build !windows

package codexcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRuntimeCancellationTerminatesTheWholeProcessGroup(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "child.pid")
	command := filepath.Join(tmp, "fake-codex")
	script := fmt.Sprintf(`#!/bin/sh
if [ "${1:-}" = "--version" ]; then echo "codex-cli 0.147.0"; exit 0; fi
if [ "${1:-}" = "exec" ] && [ "${2:-}" = "--help" ]; then echo %s; exit 0; fi
if [ "${1:-}" = "features" ] && [ "${2:-}" = "list" ]; then echo %s; exit 0; fi
sleep 60 &
child=$!
echo "$child" > %q
wait "$child"
`, requiredExecHelp(), requiredFeatureList(), pidFile)
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(tmp, "codex-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	runtime := NewRuntime(RuntimeConfig{Command: command, CodexHome: home, Platform: "darwin"})
	// Leave ample room for the two preflight subprocesses on loaded CI hosts;
	// the deadline must expire while the long-running exec task is active.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := runtime.Run(ctx, Request{Model: "gpt-test", Prompt: "wait"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error = %v, want deadline exceeded", err)
	}
	rawPID, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("fake CLI did not start child: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
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
			t.Fatalf("Codex child process %d survived group cancellation: %v", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
