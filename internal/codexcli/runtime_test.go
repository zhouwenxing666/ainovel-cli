package codexcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voocel/agentcore"
)

func TestRuntimeRunUsesIsolatedEphemeralCodexExec(t *testing.T) {
	tmp := t.TempDir()
	record := filepath.Join(tmp, "record.txt")
	command := filepath.Join(tmp, "fake-codex")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
if [ "${1:-}" = "--version" ]; then
  echo "codex-cli 0.147.0"
  exit 0
fi
if [ "${1:-}" = "exec" ] && [ "${2:-}" = "--help" ]; then
  echo --json --ephemeral --ignore-user-config --ignore-rules --skip-git-repo-check --output-schema --output-last-message --sandbox --cd --disable
  exit 0
fi
if [ "${1:-}" = "features" ] && [ "${2:-}" = "list" ]; then
  echo %s
  exit 0
fi
{
  printf 'cwd=%%s\n' "$PWD"
  printf 'codex_home=%%s\n' "${CODEX_HOME:-}"
  if [ -f "${CODEX_HOME:-}/auth.json" ]; then echo auth=copied; fi
  printf 'arg=%%s\n' "$@"
  printf 'prompt='
  cat
  printf '\n'
} > %q
last=''
prev=''
for arg in "$@"; do
  if [ "$prev" = '-o' ]; then last="$arg"; fi
  prev="$arg"
done
printf '{"decision":"continue"}' > "$last"
printf '%%s\n' '{"type":"thread.started","thread_id":"thread-test"}'
printf '%%s\n' '{"type":"item.completed","item":{"id":"item-1","type":"agent_message","text":"event fallback"}}'
printf '%%s\n' '{"type":"turn.completed","usage":{"input_tokens":11,"cached_input_tokens":3,"output_tokens":7}}'
`, requiredFeatureList(), record)
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	authHome := filepath.Join(tmp, "real-codex-home")
	if err := os.MkdirAll(authHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authHome, "auth.json"), []byte(`{"token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	runtime := NewRuntime(RuntimeConfig{
		Command:   command,
		CodexHome: authHome,
		Platform:  "darwin",
	})
	if err := runtime.Preflight(context.Background()); err != nil {
		t.Fatalf("preflight: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := runtime.Run(ctx, Request{
		Model:                 "gpt-5.3-codex",
		ReasoningEffort:       "high",
		DeveloperInstructions: "You are the Arbiter.",
		Prompt:                "Choose the next action.",
		OutputSchema:          []byte(`{"type":"object"}`),
		MCPServer: &MCPServer{
			Command:      "/usr/local/bin/ainovel-cli",
			Args:         []string{"__codex-mcp", "--socket", "/tmp/bridge.sock", "--token", "token"},
			EnabledTools: []string{"save_foundation"},
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Final != `{"decision":"continue"}` {
		t.Fatalf("final = %q", result.Final)
	}
	if result.ThreadID != "thread-test" || result.Usage.InputTokens != 11 || result.Usage.CachedInputTokens != 3 || result.Usage.OutputTokens != 7 {
		t.Fatalf("unexpected result: %#v", result)
	}

	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, required := range []string{
		"auth=copied",
		"arg=--json",
		"arg=--ephemeral",
		"arg=--ignore-user-config",
		"arg=--ignore-rules",
		"arg=--skip-git-repo-check",
		"arg=--sandbox",
		"arg=read-only",
		"arg=--disable",
		"arg=shell_tool",
		"arg=--enable\narg=code_mode_host",
		"arg=mcp_servers.ainovel.default_tools_approval_mode=\"approve\"",
		"arg=-m",
		"arg=gpt-5.3-codex",
		"prompt=Choose the next action.",
	} {
		if !strings.Contains(got, required) {
			t.Errorf("record missing %q:\n%s", required, got)
		}
	}
	if strings.Contains(got, "arg=--disable\narg=code_mode_host") {
		t.Fatalf("private MCP tool host must not be disabled:\n%s", got)
	}
	if strings.Contains(got, "codex_home="+authHome) {
		t.Fatalf("运行时直接暴露了真实 CODEX_HOME:\n%s", got)
	}
	isolatedHome := valueForLine(got, "codex_home=")
	if isolatedHome == "" {
		t.Fatalf("missing isolated CODEX_HOME:\n%s", got)
	}
	if _, err := os.Stat(isolatedHome); !os.IsNotExist(err) {
		t.Fatalf("临时 CODEX_HOME 应在调用后清理，stat err=%v", err)
	}
}

func TestClassifyRuntimeErrorMarksOnlyTransientProviderFailuresRetryable(t *testing.T) {
	tests := []struct {
		message   string
		sentinel  error
		retryable bool
	}{
		{"rate limit exceeded (429)", agentcore.ErrProviderRateLimit, true},
		{"connection reset by peer", agentcore.ErrProviderNetwork, true},
		{"service overloaded", agentcore.ErrProviderOverloaded, true},
		{"authentication failed (401)", agentcore.ErrProviderAuth, false},
		{"quota exhausted", agentcore.ErrProviderQuota, false},
		{"invalid JSONL", nil, false},
	}
	for _, test := range tests {
		err := classifyRuntimeError(fmt.Errorf("%s", test.message))
		var retryable agentcore.RetryableError
		if !errors.As(err, &retryable) || retryable.Retryable() != test.retryable {
			t.Errorf("%q retryable mismatch: %v", test.message, err)
		}
		if test.sentinel != nil && !errors.Is(err, test.sentinel) {
			t.Errorf("%q missing sentinel %v: %v", test.message, test.sentinel, err)
		}
	}
}

func TestRuntimePreflightFailsClosedOnPlatformVersionCapabilityAndAuth(t *testing.T) {
	tests := []struct {
		name       string
		platform   string
		version    string
		help       string
		createAuth bool
		want       string
	}{
		{name: "platform", platform: "linux", version: "0.147.0", help: requiredExecHelp(), createAuth: true, want: "macOS only"},
		{name: "old version", platform: "darwin", version: "0.146.9", help: requiredExecHelp(), createAuth: true, want: "too old"},
		{name: "missing isolation flag", platform: "darwin", version: "0.147.0", help: strings.ReplaceAll(requiredExecHelp(), "--ignore-rules", ""), createAuth: true, want: "--ignore-rules"},
		{name: "missing native tool gate", platform: "darwin", version: "0.147.0", help: requiredExecHelp(), createAuth: true, want: "shell_tool"},
		{name: "missing auth", platform: "darwin", version: "0.147.0", help: requiredExecHelp(), createAuth: false, want: "codex login"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmp := t.TempDir()
			command := filepath.Join(tmp, "fake-codex")
			script := fmt.Sprintf(`#!/bin/sh
if [ "${1:-}" = "--version" ]; then echo "codex-cli %s"; exit 0; fi
if [ "${1:-}" = "exec" ] && [ "${2:-}" = "--help" ]; then echo %q; exit 0; fi
if [ "${1:-}" = "features" ] && [ "${2:-}" = "list" ]; then echo %q; exit 0; fi
exit 1
`, test.version, test.help, func() string {
				if test.name == "missing native tool gate" {
					return strings.ReplaceAll(requiredFeatureList(), "shell_tool", "")
				}
				return requiredFeatureList()
			}())
			if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(tmp, "codex-home")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			if test.createAuth {
				if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			runtime := NewRuntime(RuntimeConfig{Command: command, CodexHome: home, Platform: test.platform})
			if err := runtime.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Preflight error = %v, want %q", err, test.want)
			}
		})
	}
}

func requiredExecHelp() string {
	return "--json --ephemeral --ignore-user-config --ignore-rules --skip-git-repo-check --output-schema --output-last-message --sandbox --cd --disable"
}

func requiredFeatureList() string {
	return strings.Join(append(append([]string(nil), disabledNativeFeatures...), enabledNativeFeatures...), " ")
}

func TestRuntimeRejectsRelativeExecutablePaths(t *testing.T) {
	if _, err := NewRuntime(RuntimeConfig{Command: "./codex"}).commandPath(); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("relative executable path was not rejected: %v", err)
	}
}

func TestRuntimeDiagnosticsAreNormalizedWithoutSecretsOrNovelText(t *testing.T) {
	secret := "private-mcp-token"
	novel := "尚未公开的小说正文"
	diagnostic := "server returned 401 while launching mcp --token " + secret + ": " + novel
	got := normalizeRuntimeDiagnostic(diagnostic)
	if got != "Codex authentication failed" {
		t.Fatalf("normalized diagnostic = %q", got)
	}
	if strings.Contains(got, secret) || strings.Contains(got, novel) {
		t.Fatalf("normalized diagnostic leaked sensitive input: %q", got)
	}
	unknown := normalizeRuntimeDiagnostic("fatal: " + novel)
	if strings.Contains(unknown, novel) || !strings.Contains(unknown, "raw diagnostic omitted") {
		t.Fatalf("unknown diagnostic was not safely normalized: %q", unknown)
	}
}

func TestRuntimeReportsRejectedOutputSchemaWithoutLeakingRawDiagnostic(t *testing.T) {
	tmp := t.TempDir()
	command := filepath.Join(tmp, "fake-codex")
	secret := "private-mcp-token"
	novel := "尚未公开的小说正文"
	script := fmt.Sprintf(`#!/bin/sh
if [ "${1:-}" = "--version" ]; then echo "codex-cli 0.147.0"; exit 0; fi
if [ "${1:-}" = "exec" ] && [ "${2:-}" = "--help" ]; then echo %q; exit 0; fi
if [ "${1:-}" = "features" ] && [ "${2:-}" = "list" ]; then echo %q; exit 0; fi
printf '%%s\n' %q
exit 1
`, requiredExecHelp(), requiredFeatureList(), `{"type":"turn.failed","error":{"message":"invalid_json_schema: additionalProperties is required; private-mcp-token 尚未公开的小说正文"}}`)
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

	_, err := NewRuntime(RuntimeConfig{Command: command, CodexHome: home, Platform: "darwin"}).Run(
		context.Background(), Request{Model: "gpt-test", Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "Codex output JSON Schema was rejected") {
		t.Fatalf("runtime error = %v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), novel) {
		t.Fatalf("runtime error leaked raw diagnostic: %v", err)
	}
}

func TestRuntimePreflightPreservesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := NewRuntime(RuntimeConfig{Platform: "darwin"}).Preflight(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("preflight cancellation = %v", err)
	}
}

func valueForLine(text, prefix string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}
