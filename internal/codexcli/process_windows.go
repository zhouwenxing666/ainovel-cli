//go:build windows

package codexcli

import (
	"os"
	"os/exec"
)

// The Codex backend fails preflight on Windows. These stubs keep the rest of
// ainovel-cli cross-compilable without pretending that Windows has Unix-style
// process-group semantics.
func configureProcessGroup(_ *exec.Cmd) {}

func terminateProcessGroup(pid int) {
	if process, err := os.FindProcess(pid); err == nil {
		_ = process.Kill()
	}
}

func killProcessGroup(pid int) { terminateProcessGroup(pid) }
