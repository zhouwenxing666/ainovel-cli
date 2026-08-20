// Package codexcli provides the isolated subprocess boundary used to run a
// locally authenticated Codex CLI without inheriting the user's Codex rules,
// configuration, skills, plugins, or workspace.
package codexcli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/voocel/agentcore"
)

const (
	defaultMinimumVersion = "0.147.0"
	maxJSONLEventBytes    = 8 << 20
	maxStderrBytes        = 64 << 10
)

var versionPattern = regexp.MustCompile(`(?m)(\d+)\.(\d+)\.(\d+)`)

var disabledNativeFeatures = []string{
	"shell_tool", "unified_exec", "shell_snapshot", "apps", "plugins", "hooks",
	"browser_use", "browser_use_external", "browser_use_full_cdp_access",
	"computer_use", "image_generation", "view_image", "standalone_web_search", "web_search_cached",
	"code_mode", "multi_agent", "multi_agent_v2", "enable_fanout",
}

// The private MCP bridge is executed by Codex's tool host. Keep the host on
// while all user-facing native tools remain explicitly disabled.
var enabledNativeFeatures = []string{"code_mode_host"}

// RuntimeConfig defines the trusted, global-only process boundary.
type RuntimeConfig struct {
	Command        string
	CodexHome      string
	MinimumVersion string
	// Platform is an injection seam for deterministic tests. Production leaves
	// it empty and therefore uses runtime.GOOS.
	Platform string
}

// Runtime launches isolated codex exec processes.
type Runtime struct {
	config RuntimeConfig
}

// MCPServer describes one private stdio MCP bridge for a Worker task.
type MCPServer struct {
	Command      string
	Args         []string
	EnabledTools []string
}

// Request is one ephemeral Codex task.
type Request struct {
	Model                 string
	ReasoningEffort       string
	DeveloperInstructions string
	Prompt                string
	OutputSchema          []byte
	MCPServer             *MCPServer
}

// Usage contains token counts emitted by turn.completed.
type Usage struct {
	InputTokens       int64
	CachedInputTokens int64
	OutputTokens      int64
}

// Result is the normalized, non-sensitive result of one codex exec process.
type Result struct {
	Final    string
	ThreadID string
	Usage    Usage
	Duration time.Duration
	ExitCode int
}

func NewRuntime(config RuntimeConfig) *Runtime { return &Runtime{config: config} }

// Preflight verifies the macOS-only runtime, executable version, isolation
// flags, and reusable file-based authentication before a configuration is
// accepted or activated.
func (r *Runtime) Preflight(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	platform := r.config.Platform
	if platform == "" {
		platform = runtime.GOOS
	}
	if platform != "darwin" {
		return fmt.Errorf("codex_cli currently supports macOS only (got %s)", platform)
	}
	command, err := r.commandPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(r.codexHome(), "auth.json")); err != nil {
		return fmt.Errorf("Codex auth not found at %s/auth.json; run `codex login` first: %w", r.codexHome(), err)
	}

	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	versionOut, err := exec.CommandContext(checkCtx, command, "--version").CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if checkCtx.Err() != nil {
			return fmt.Errorf("Codex CLI preflight timed out: %w", checkCtx.Err())
		}
		return fmt.Errorf("run codex --version: %w", err)
	}
	minimum := strings.TrimSpace(r.config.MinimumVersion)
	if minimum == "" {
		minimum = defaultMinimumVersion
	}
	if err := requireMinimumVersion(string(versionOut), minimum); err != nil {
		return err
	}

	helpOut, err := exec.CommandContext(checkCtx, command, "exec", "--help").CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if checkCtx.Err() != nil {
			return fmt.Errorf("Codex CLI preflight timed out: %w", checkCtx.Err())
		}
		return fmt.Errorf("run codex exec --help: %w", err)
	}
	for _, flag := range []string{
		"--json", "--ephemeral", "--ignore-user-config", "--ignore-rules",
		"--skip-git-repo-check", "--output-schema", "--output-last-message",
		"--sandbox", "--cd", "--disable",
	} {
		if !bytes.Contains(helpOut, []byte(flag)) {
			return fmt.Errorf("codex CLI lacks required isolation capability %s", flag)
		}
	}
	featuresOut, err := exec.CommandContext(checkCtx, command, "features", "list").CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if checkCtx.Err() != nil {
			return fmt.Errorf("Codex CLI preflight timed out: %w", checkCtx.Err())
		}
		return fmt.Errorf("run codex features list: %w", err)
	}
	available := make(map[string]bool)
	for _, field := range strings.Fields(string(featuresOut)) {
		available[field] = true
	}
	for _, feature := range append(append([]string(nil), disabledNativeFeatures...), enabledNativeFeatures...) {
		if !available[feature] {
			return fmt.Errorf("codex CLI lacks required feature gate %s needed for native-tool isolation", feature)
		}
	}
	return nil
}

// Run executes one isolated, ephemeral Codex task. It copies only auth.json
// into a temporary CODEX_HOME and deletes the entire task directory afterward.
func (r *Runtime) Run(ctx context.Context, request Request) (Result, error) {
	started := time.Now()
	if err := r.Preflight(ctx); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(request.Model) == "" {
		return Result{}, errors.New("codex request model is required")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	taskRoot, err := os.MkdirTemp("", "ainovel-codex-")
	if err != nil {
		return Result{}, fmt.Errorf("create codex task directory: %w", err)
	}
	defer os.RemoveAll(taskRoot)
	if err := os.Chmod(taskRoot, 0o700); err != nil {
		return Result{}, fmt.Errorf("protect codex task directory: %w", err)
	}
	workspace := filepath.Join(taskRoot, "workspace")
	isolatedHome := filepath.Join(taskRoot, "codex-home")
	tmpDir := filepath.Join(taskRoot, "tmp")
	for _, dir := range []string{workspace, isolatedHome, tmpDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Result{}, fmt.Errorf("create isolated codex directory: %w", err)
		}
	}
	if err := copyAuth(filepath.Join(r.codexHome(), "auth.json"), filepath.Join(isolatedHome, "auth.json")); err != nil {
		return Result{}, err
	}

	lastMessage := filepath.Join(taskRoot, "last-message.txt")
	args := []string{
		"exec", "--json", "--ephemeral", "--ignore-user-config", "--ignore-rules",
		"--skip-git-repo-check", "--color", "never", "--sandbox", "read-only",
		"-C", workspace, "-m", request.Model, "-o", lastMessage,
		"-c", `approval_policy="never"`,
		"-c", `shell_environment_policy.inherit="none"`,
	}
	for _, feature := range disabledNativeFeatures {
		args = append(args, "--disable", feature)
	}
	for _, feature := range enabledNativeFeatures {
		args = append(args, "--enable", feature)
	}
	if request.DeveloperInstructions != "" {
		args = append(args, "-c", "developer_instructions="+tomlString(request.DeveloperInstructions))
	}
	if request.ReasoningEffort != "" {
		args = append(args, "-c", "model_reasoning_effort="+tomlString(request.ReasoningEffort))
	}
	if len(request.OutputSchema) > 0 {
		schemaPath := filepath.Join(taskRoot, "output-schema.json")
		if !json.Valid(request.OutputSchema) {
			return Result{}, errors.New("codex output schema is not valid JSON")
		}
		if err := os.WriteFile(schemaPath, request.OutputSchema, 0o600); err != nil {
			return Result{}, fmt.Errorf("write codex output schema: %w", err)
		}
		args = append(args, "--output-schema", schemaPath)
	}
	if request.MCPServer != nil {
		if err := validateMCPServer(*request.MCPServer); err != nil {
			return Result{}, err
		}
		args = append(args,
			"-c", "mcp_servers.ainovel.command="+tomlString(request.MCPServer.Command),
			"-c", "mcp_servers.ainovel.args="+tomlStrings(request.MCPServer.Args),
			"-c", "mcp_servers.ainovel.required=true",
			"-c", "mcp_servers.ainovel.enabled_tools="+tomlStrings(request.MCPServer.EnabledTools),
			"-c", `mcp_servers.ainovel.default_tools_approval_mode="approve"`,
		)
	}

	command, err := r.commandPath()
	if err != nil {
		return Result{}, err
	}
	cmd := exec.Command(command, args...)
	cmd.Dir = workspace
	cmd.Env = isolatedEnvironment(taskRoot, isolatedHome, tmpDir)
	cmd.Stdin = strings.NewReader(request.Prompt)
	configureProcessGroup(cmd)
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		return Result{}, fmt.Errorf("create codex stdout pipe: %w", err)
	}
	stderr, stderrWriter, err := os.Pipe()
	if err != nil {
		stdout.Close()
		stdoutWriter.Close()
		return Result{}, fmt.Errorf("create codex stderr pipe: %w", err)
	}
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter
	if err := cmd.Start(); err != nil {
		stdout.Close()
		stdoutWriter.Close()
		stderr.Close()
		stderrWriter.Close()
		return Result{}, fmt.Errorf("start codex exec: %w", err)
	}
	// The parent must close its copies of the write ends so child exit produces
	// EOF for the readers. These are caller-owned OS pipes, so cmd.Wait cannot
	// race by closing the read ends before the drain goroutines finish.
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	defer stdout.Close()
	defer stderr.Close()

	parsedCh := make(chan parsedEvents, 1)
	stderrCh := make(chan string, 1)
	go func() { parsedCh <- parseJSONL(stdout) }()
	go func() { stderrCh <- readCapped(stderr, maxStderrBytes) }()
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-waitCh:
	case <-ctx.Done():
		terminateProcessGroup(cmd.Process.Pid)
		select {
		case waitErr = <-waitCh:
		case <-time.After(2 * time.Second):
			killProcessGroup(cmd.Process.Pid)
			waitErr = <-waitCh
		}
	}
	parsed := <-parsedCh
	stderrText := <-stderrCh
	result := parsed.result
	result.Duration = time.Since(started)
	result.ExitCode = exitCode(waitErr)

	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if parsed.err != nil {
		return result, classifyRuntimeError(errors.New(normalizeRuntimeDiagnostic(parsed.err.Error())))
	}
	if waitErr != nil {
		return result, classifyRuntimeError(fmt.Errorf("codex exec failed (exit %d): %s", result.ExitCode, normalizeRuntimeDiagnostic(stderrText)))
	}
	if final, err := os.ReadFile(lastMessage); err == nil && strings.TrimSpace(string(final)) != "" {
		result.Final = strings.TrimSpace(string(final))
	}
	if strings.TrimSpace(result.Final) == "" {
		return result, errors.New("codex exec completed without a final message")
	}
	return result, nil
}

type providerRuntimeError struct {
	cause     error
	sentinel  error
	retryable bool
}

func (e *providerRuntimeError) Error() string   { return e.cause.Error() }
func (e *providerRuntimeError) Unwrap() error   { return e.cause }
func (e *providerRuntimeError) Retryable() bool { return e.retryable }
func (e *providerRuntimeError) Is(target error) bool {
	return e.sentinel != nil && target == e.sentinel
}

func classifyRuntimeError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	sentinel := agentcore.ClassifyProvider(err)
	if sentinel == err {
		return &providerRuntimeError{cause: err}
	}
	retryable := errors.Is(sentinel, agentcore.ErrProviderRateLimit) ||
		errors.Is(sentinel, agentcore.ErrProviderNetwork) ||
		errors.Is(sentinel, agentcore.ErrProviderOverloaded) ||
		errors.Is(sentinel, agentcore.ErrProviderStreamIdle)
	return &providerRuntimeError{cause: err, sentinel: sentinel, retryable: retryable}
}

func (r *Runtime) commandPath() (string, error) {
	command := strings.TrimSpace(r.config.Command)
	if command == "" {
		command = "codex"
	}
	if strings.ContainsAny(command, `/\`) && !filepath.IsAbs(command) {
		return "", fmt.Errorf("Codex CLI command must be a bare command name or absolute path, got %q", command)
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("find Codex CLI command %q: %w", command, err)
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("Codex CLI command %q resolved to non-absolute path %q; remove relative entries such as '.' from PATH or configure an absolute command path", command, path)
	}
	return path, nil
}

func (r *Runtime) codexHome() string {
	if home := strings.TrimSpace(r.config.CodexHome); home != "" {
		return filepath.Clean(home)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

func requireMinimumVersion(output, minimum string) error {
	got, ok := parseVersion(output)
	if !ok {
		return fmt.Errorf("cannot parse Codex CLI version from %q", strings.TrimSpace(output))
	}
	want, ok := parseVersion(minimum)
	if !ok {
		return fmt.Errorf("invalid minimum Codex CLI version %q", minimum)
	}
	for i := range got {
		if got[i] > want[i] {
			return nil
		}
		if got[i] < want[i] {
			return fmt.Errorf("Codex CLI %d.%d.%d is too old; need >= %d.%d.%d", got[0], got[1], got[2], want[0], want[1], want[2])
		}
	}
	return nil
}

func parseVersion(text string) ([3]int, bool) {
	match := versionPattern.FindStringSubmatch(text)
	if len(match) != 4 {
		return [3]int{}, false
	}
	var result [3]int
	for i := 1; i <= 3; i++ {
		value, err := strconv.Atoi(match[i])
		if err != nil {
			return [3]int{}, false
		}
		result[i-1] = value
	}
	return result, true
}

func copyAuth(source, target string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read Codex auth: %w", err)
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		return fmt.Errorf("copy Codex auth into isolated runtime: %w", err)
	}
	return nil
}

func isolatedEnvironment(taskRoot, codexHome, tmpDir string) []string {
	env := []string{
		"HOME=" + taskRoot,
		"CODEX_HOME=" + codexHome,
		"TMPDIR=" + tmpDir,
	}
	for _, key := range []string{
		"PATH", "LANG", "LC_ALL", "HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY",
		"https_proxy", "http_proxy", "all_proxy", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR",
	} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func tomlString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func tomlStrings(values []string) string {
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

func validateMCPServer(server MCPServer) error {
	if strings.TrimSpace(server.Command) == "" {
		return errors.New("private MCP command is required")
	}
	if !filepath.IsAbs(server.Command) {
		return errors.New("private MCP command must be an absolute path")
	}
	if len(server.EnabledTools) == 0 {
		return errors.New("private MCP enabled_tools must not be empty")
	}
	return nil
}

type parsedEvents struct {
	result Result
	err    error
}

func parseJSONL(reader io.Reader) parsedEvents {
	var parsed parsedEvents
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxJSONLEventBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Message  string `json:"message"`
			Item     struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Usage struct {
				InputTokens       int64 `json:"input_tokens"`
				CachedInputTokens int64 `json:"cached_input_tokens"`
				OutputTokens      int64 `json:"output_tokens"`
			} `json:"usage"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			if parsed.err == nil {
				parsed.err = fmt.Errorf("invalid codex JSONL event: %w", err)
			}
			continue
		}
		switch event.Type {
		case "thread.started":
			parsed.result.ThreadID = event.ThreadID
		case "item.completed":
			if event.Item.Type == "agent_message" && event.Item.Text != "" {
				parsed.result.Final = event.Item.Text
			}
		case "turn.completed":
			parsed.result.Usage = Usage{
				InputTokens:       event.Usage.InputTokens,
				CachedInputTokens: event.Usage.CachedInputTokens,
				OutputTokens:      event.Usage.OutputTokens,
			}
		case "turn.failed", "error":
			message := event.Message
			if message == "" {
				message = event.Error.Message
			}
			if message == "" {
				message = "Codex reported a failed turn"
			}
			if parsed.err == nil {
				parsed.err = errors.New(message)
			}
		}
	}
	if err := scanner.Err(); err != nil && parsed.err == nil {
		parsed.err = fmt.Errorf("read codex JSONL: %w", err)
	}
	return parsed
}

func readCapped(reader io.Reader, limit int64) string {
	var buffer bytes.Buffer
	_, _ = io.CopyN(&buffer, reader, limit)
	_, _ = io.Copy(io.Discard, reader)
	return buffer.String()
}

// normalizeRuntimeDiagnostic maps untrusted CLI/remote diagnostics to a small
// actionable vocabulary. Raw stderr and turn.failed messages can echo prompts,
// temp paths, MCP tokens, or provider payloads, so they never cross the process
// boundary into logs or persisted Engine errors.
func normalizeRuntimeDiagnostic(diagnostic string) string {
	lower := strings.ToLower(diagnostic)
	switch {
	case strings.Contains(lower, "invalid codex jsonl"):
		return "Codex CLI emitted invalid JSONL"
	case strings.Contains(lower, "read codex jsonl"):
		return "Codex CLI JSONL stream failed"
	case strings.Contains(lower, "invalid_json_schema"), strings.Contains(lower, "text.format.schema"),
		strings.Contains(lower, "additionalproperties"):
		return "Codex output JSON Schema was rejected"
	case strings.Contains(lower, "authentication"), strings.Contains(lower, "unauthorized"), strings.Contains(lower, "401"):
		return "Codex authentication failed"
	case strings.Contains(lower, "quota"), strings.Contains(lower, "insufficient credit"), strings.Contains(lower, "billing"):
		return "Codex quota exhausted"
	case strings.Contains(lower, "rate limit"), strings.Contains(lower, "429"):
		return "Codex rate limit exceeded"
	case strings.Contains(lower, "overload"), strings.Contains(lower, "service unavailable"),
		strings.Contains(lower, "502"), strings.Contains(lower, "503"), strings.Contains(lower, "504"):
		return "Codex service overloaded"
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "timed out"):
		return "Codex network timeout"
	case strings.Contains(lower, "connection"), strings.Contains(lower, "network"), strings.Contains(lower, "dns"), strings.Contains(lower, "tls"):
		return "Codex network request failed"
	case strings.Contains(lower, "model") && (strings.Contains(lower, "not found") || strings.Contains(lower, "404")):
		return "Codex model not found"
	case strings.Contains(lower, "mcp"):
		return "Codex private MCP startup or protocol failure"
	case strings.TrimSpace(diagnostic) == "":
		return "Codex CLI exited without diagnostic output"
	default:
		return "Codex CLI reported an error; raw diagnostic omitted"
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
