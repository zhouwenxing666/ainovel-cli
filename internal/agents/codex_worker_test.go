package agents

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/codexcli"
)

// TestCodexCLIProcessHelper is re-executed by the fake-codex shell wrapper in
// TestHybridWorkerRunnerFakeCLIProcessE2E. Normal package test runs have no
// "--" payload and simply return.
func TestCodexCLIProcessHelper(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		return
	}
	args := os.Args[separator+1:]
	exit := func(code int, err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(code)
	}
	switch {
	case args[0] == "--version":
		fmt.Fprintln(os.Stdout, "codex-cli 0.147.0")
		exit(0, nil)
	case len(args) >= 2 && args[0] == "exec" && args[1] == "--help":
		fmt.Fprintln(os.Stdout, "--json --ephemeral --ignore-user-config --ignore-rules --skip-git-repo-check --output-schema --output-last-message --sandbox --cd --disable")
		exit(0, nil)
	case len(args) >= 2 && args[0] == "features" && args[1] == "list":
		fmt.Fprintln(os.Stdout, "shell_tool unified_exec shell_snapshot apps plugins hooks browser_use browser_use_external browser_use_full_cdp_access computer_use image_generation view_image standalone_web_search web_search_cached code_mode code_mode_host multi_agent multi_agent_v2 enable_fanout")
		exit(0, nil)
	case args[0] == "exec":
		if err := runCodexCLIProcessHelper(args); err != nil {
			exit(1, err)
		}
		exit(0, nil)
	default:
		exit(2, fmt.Errorf("unexpected fake Codex args: %v", args))
	}
}

func runCodexCLIProcessHelper(args []string) error {
	var lastMessage string
	var mcpArgs []string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "-o":
			lastMessage = args[i+1]
		case "-c":
			const prefix = "mcp_servers.ainovel.args="
			if strings.HasPrefix(args[i+1], prefix) {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(args[i+1], prefix)), &mcpArgs); err != nil {
					return err
				}
			}
		}
	}
	var socket, token string
	for i := 0; i+1 < len(mcpArgs); i++ {
		switch mcpArgs[i] {
		case "--socket":
			socket = mcpArgs[i+1]
		case "--token":
			token = mcpArgs[i+1]
		}
	}
	if lastMessage == "" || socket == "" || token == "" {
		return fmt.Errorf("fake Codex missing output/MCP boundary")
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(conn, "{\"token\":%q}\n", token); err != nil {
		conn.Close()
		return err
	}
	reader := bufio.NewReader(conn)
	if err := workerMCPCall(conn, reader, 1, "initialize", map[string]any{"protocolVersion": "2026-07-28"}); err != nil {
		conn.Close()
		return err
	}
	if err := workerMCPCall(conn, reader, 2, "tools/call", map[string]any{
		"name": "commit_test", "arguments": map[string]any{"value": "done"},
	}); err != nil {
		conn.Close()
		return err
	}
	if err := conn.Close(); err != nil {
		return err
	}
	if err := os.WriteFile(lastMessage, []byte("fake CLI completed"), 0o600); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, `{"type":"thread.started","thread_id":"fake-process"}`)
	fmt.Fprintln(os.Stdout, `{"type":"item.completed","item":{"type":"agent_message","text":"fake CLI completed"}}`)
	fmt.Fprintln(os.Stdout, `{"type":"turn.completed","usage":{"input_tokens":17,"cached_input_tokens":3,"output_tokens":5}}`)
	return nil
}

type codexWorkerTestTool struct {
	name  string
	calls int
	err   error
}

func (t *codexWorkerTestTool) Name() string {
	if t.name != "" {
		return t.name
	}
	return "commit_test"
}
func (t *codexWorkerTestTool) Description() string { return "commit test checkpoint" }
func (t *codexWorkerTestTool) Schema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"value": map[string]any{"type": "string"}},
		"required":   []string{"value"},
	}
}

func TestHybridWorkerRunnerFallsBackOnlyBeforeBusinessSideEffects(t *testing.T) {
	cfg := bootstrap.Config{
		Provider: "primary", ModelName: "gpt-primary",
		Providers: map[string]bootstrap.ProviderConfig{
			"primary": {Driver: "codex_cli", Command: "primary-codex"},
			"backup":  {Driver: "codex_cli", Command: "backup-codex"},
		},
		Roles: map[string]bootstrap.RoleConfig{
			"writer": {
				Provider: "primary", Model: "gpt-primary",
				Fallbacks: []bootstrap.ModelRef{{Provider: "backup", Model: "gpt-backup"}},
			},
		},
	}
	models, err := bootstrap.NewModelSet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	commit := &codexWorkerTestTool{}
	mutate := &codexWorkerTestTool{name: "write_progress"}
	definition := WorkerDefinition{
		Name: "writer", Role: "writer", SystemPrompt: "canonical",
		Tools: []agentcore.Tool{mutate, commit}, TerminalGuidance: "commit",
		Terminal: func(name string, _ json.RawMessage) bool { return name == "commit_test" },
	}

	primaryCalls, backupCalls := 0, 0
	factory := func(config codexcli.RuntimeConfig) codexcli.Executor {
		switch config.Command {
		case "primary-codex":
			return codexWorkerExecutorFunc(func(context.Context, codexcli.Request) (codexcli.Result, error) {
				primaryCalls++
				return codexcli.Result{}, errors.New("primary unavailable")
			})
		case "backup-codex":
			return codexWorkerExecutorFunc(func(_ context.Context, request codexcli.Request) (codexcli.Result, error) {
				backupCalls++
				if err := callWorkerMCPTool(t, request, "commit_test"); err != nil {
					return codexcli.Result{}, err
				}
				return codexcli.Result{Final: "backup completed"}, nil
			})
		default:
			t.Fatalf("unexpected command %q", config.Command)
			return nil
		}
	}
	runner := newHybridWorkerRunner(cfg, models, nil, []WorkerDefinition{definition}, factory)
	if _, err := runner.Run(context.Background(), "writer", "write"); err != nil {
		t.Fatalf("pre-side-effect fallback failed: %v", err)
	}
	if primaryCalls != 1 || backupCalls != 1 {
		t.Fatalf("fallback attempts primary=%d backup=%d", primaryCalls, backupCalls)
	}

	primaryCalls, backupCalls = 0, 0
	factory = func(config codexcli.RuntimeConfig) codexcli.Executor {
		if config.Command == "primary-codex" {
			return codexWorkerExecutorFunc(func(_ context.Context, request codexcli.Request) (codexcli.Result, error) {
				primaryCalls++
				if err := callWorkerMCPTool(t, request, "write_progress"); err != nil {
					return codexcli.Result{}, err
				}
				return codexcli.Result{}, errors.New("crashed after write")
			})
		}
		return codexWorkerExecutorFunc(func(context.Context, codexcli.Request) (codexcli.Result, error) {
			backupCalls++
			return codexcli.Result{}, errors.New("must not run")
		})
	}
	runner = newHybridWorkerRunner(cfg, models, nil, []WorkerDefinition{definition}, factory)
	if _, err := runner.Run(context.Background(), "writer", "write"); err == nil || !strings.Contains(err.Error(), "Engine must re-read Store facts") {
		t.Fatalf("post-side-effect failure must return to Engine, got %v", err)
	}
	if primaryCalls != 1 || backupCalls != 0 {
		t.Fatalf("unsafe fallback ran after side effect: primary=%d backup=%d", primaryCalls, backupCalls)
	}

	// An error result cannot prove the write stayed side-effect free. The
	// bridge must mark the attempt before Execute returns.
	mutate.err = errors.New("write failed after an unknown durability boundary")
	primaryCalls, backupCalls = 0, 0
	factory = func(config codexcli.RuntimeConfig) codexcli.Executor {
		if config.Command == "primary-codex" {
			return codexWorkerExecutorFunc(func(_ context.Context, request codexcli.Request) (codexcli.Result, error) {
				primaryCalls++
				return codexcli.Result{}, callWorkerMCPTool(t, request, "write_progress")
			})
		}
		return codexWorkerExecutorFunc(func(context.Context, codexcli.Request) (codexcli.Result, error) {
			backupCalls++
			return codexcli.Result{}, errors.New("must not run")
		})
	}
	runner = newHybridWorkerRunner(cfg, models, nil, []WorkerDefinition{definition}, factory)
	if _, err := runner.Run(context.Background(), "writer", "write"); err == nil || !strings.Contains(err.Error(), "Engine must re-read Store facts") {
		t.Fatalf("failed write attempt must return to Engine, got %v", err)
	}
	if primaryCalls != 1 || backupCalls != 0 {
		t.Fatalf("fallback ran after failed write attempt: primary=%d backup=%d", primaryCalls, backupCalls)
	}
	tracker := &toolSideEffectTracker{}
	if _, err := (&trackedWorkerTool{Tool: mutate, tracker: tracker}).Execute(context.Background(), json.RawMessage(`{"value":"done"}`)); err == nil {
		t.Fatal("HTTP tracker fixture should return the write error")
	}
	if !tracker.Mutated() {
		t.Fatal("HTTP adapter did not conservatively mark a failed write attempt")
	}
}

func TestHybridWorkerRunnerAppliesHotSwitchAtNextTaskBoundary(t *testing.T) {
	cfg := bootstrap.Config{
		Provider: "first", ModelName: "model-1",
		Providers: map[string]bootstrap.ProviderConfig{
			"first":  {Driver: "codex_cli", Command: "first-codex"},
			"second": {Driver: "codex_cli", Command: "second-codex"},
		},
	}
	models, err := bootstrap.NewModelSet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tool := &codexWorkerTestTool{}
	definition := WorkerDefinition{
		Name: "writer", Role: "writer", SystemPrompt: "canonical",
		Tools: []agentcore.Tool{tool}, TerminalGuidance: "commit",
		Terminal: func(name string, _ json.RawMessage) bool { return name == "commit_test" },
	}
	var commands []string
	runner := newHybridWorkerRunner(cfg, models, nil, []WorkerDefinition{definition}, func(config codexcli.RuntimeConfig) codexcli.Executor {
		commands = append(commands, config.Command)
		return codexWorkerExecutorFunc(func(_ context.Context, request codexcli.Request) (codexcli.Result, error) {
			if err := callWorkerMCPTool(t, request, "commit_test"); err != nil {
				return codexcli.Result{}, err
			}
			return codexcli.Result{Final: config.Command}, nil
		})
	})
	if _, err := runner.Run(context.Background(), "writer", "first task"); err != nil {
		t.Fatal(err)
	}
	if err := models.Swap("default", "second", "model-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), "writer", "second task"); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || commands[0] != "first-codex" || commands[1] != "second-codex" {
		t.Fatalf("task-boundary backend selections = %v", commands)
	}
}
func (t *codexWorkerTestTool) Execute(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	t.calls++
	if t.err != nil {
		return nil, t.err
	}
	return json.RawMessage(`{"checkpoint":true}`), nil
}

type codexWorkerExecutorFunc func(context.Context, codexcli.Request) (codexcli.Result, error)

func (f codexWorkerExecutorFunc) Run(ctx context.Context, request codexcli.Request) (codexcli.Result, error) {
	return f(ctx, request)
}

func TestHybridWorkerRunnerRequiresTerminalMCPCheckpoint(t *testing.T) {
	cfg := ConfigForCodexWorkerTest()
	models, err := bootstrap.NewModelSet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tool := &codexWorkerTestTool{}
	definition := WorkerDefinition{
		Name: "writer", Role: "writer", SystemPrompt: "canonical writer prompt",
		Tools: []agentcore.Tool{tool}, TerminalGuidance: "commit_test must succeed",
		Terminal: func(name string, result json.RawMessage) bool {
			return name == "commit_test" && strings.Contains(string(result), `"checkpoint":true`)
		},
	}
	executor := codexWorkerExecutorFunc(func(_ context.Context, request codexcli.Request) (codexcli.Result, error) {
		if request.MCPServer == nil || !strings.Contains(request.DeveloperInstructions, "canonical writer prompt") {
			return codexcli.Result{}, fmt.Errorf("missing private MCP or canonical prompt")
		}
		socket, token := mcpSocketToken(t, request.MCPServer.Args)
		conn, err := net.Dial("unix", socket)
		if err != nil {
			return codexcli.Result{}, err
		}
		defer conn.Close()
		if _, err := fmt.Fprintf(conn, "{\"token\":%q}\n", token); err != nil {
			return codexcli.Result{}, err
		}
		reader := bufio.NewReader(conn)
		if err := workerMCPCall(conn, reader, 1, "initialize", map[string]any{"protocolVersion": "2026-07-28"}); err != nil {
			return codexcli.Result{}, err
		}
		if err := workerMCPCall(conn, reader, 2, "tools/call", map[string]any{
			"name": "commit_test", "arguments": map[string]any{"value": "done"},
		}); err != nil {
			return codexcli.Result{}, err
		}
		return codexcli.Result{
			Final: "done",
			Usage: codexcli.Usage{InputTokens: 13, CachedInputTokens: 2, OutputTokens: 5},
		}, nil
	})
	runner := newHybridWorkerRunner(cfg, models, nil, []WorkerDefinition{definition}, func(codexcli.RuntimeConfig) codexcli.Executor { return executor })

	result, err := runner.Run(context.Background(), "writer", "write chapter")
	if err != nil {
		t.Fatal(err)
	}
	if string(result.TerminalResult) != `{"checkpoint":true}` || result.Output != "done" || result.Usage.Input != 13 || result.Usage.CacheRead != 2 || result.Usage.Output != 5 || result.Usage.Tools != 1 || tool.calls != 1 {
		t.Fatalf("result=%#v calls=%d", result, tool.calls)
	}

	noCheckpoint := codexWorkerExecutorFunc(func(context.Context, codexcli.Request) (codexcli.Result, error) {
		return codexcli.Result{Final: "I am finished"}, nil
	})
	runner = newHybridWorkerRunner(cfg, models, nil, []WorkerDefinition{definition}, func(codexcli.RuntimeConfig) codexcli.Executor { return noCheckpoint })
	if _, err := runner.Run(context.Background(), "writer", "write chapter"); err == nil || !strings.Contains(err.Error(), "terminal tool checkpoint") {
		t.Fatalf("exit 0/final prose must fail without checkpoint, got %v", err)
	}
}

func TestHybridWorkerRunnerRoutesEveryCreativeRoleThroughCodexBackend(t *testing.T) {
	cfg := ConfigForCodexWorkerTest()
	models, err := bootstrap.NewModelSet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	agents := []struct {
		name string
		role string
	}{
		{name: "architect_short", role: "architect"},
		{name: "architect_long", role: "architect"},
		{name: "writer", role: "writer"},
		{name: "reviewer", role: "reviewer"},
		{name: "editor", role: "editor"},
	}
	definitions := make([]WorkerDefinition, 0, len(agents))
	for _, agent := range agents {
		tool := &codexWorkerTestTool{}
		definitions = append(definitions, WorkerDefinition{
			Name: agent.name, Role: agent.role, SystemPrompt: "prompt-" + agent.name,
			Tools: []agentcore.Tool{tool}, TerminalGuidance: "commit",
			Terminal: func(name string, _ json.RawMessage) bool { return name == "commit_test" },
		})
	}
	seen := make(map[string]int, len(agents))
	executor := codexWorkerExecutorFunc(func(_ context.Context, request codexcli.Request) (codexcli.Result, error) {
		for _, agent := range agents {
			if strings.Contains(request.DeveloperInstructions, "prompt-"+agent.name) {
				seen[agent.name]++
			}
		}
		if err := callWorkerMCPTool(t, request, "commit_test"); err != nil {
			return codexcli.Result{}, err
		}
		return codexcli.Result{Final: "done"}, nil
	})
	runner := newHybridWorkerRunner(cfg, models, nil, definitions, func(codexcli.RuntimeConfig) codexcli.Executor { return executor })
	for _, agent := range agents {
		if _, err := runner.Run(context.Background(), agent.name, "task"); err != nil {
			t.Fatalf("%s Codex route: %v", agent.name, err)
		}
	}
	for _, agent := range agents {
		if seen[agent.name] != 1 {
			t.Errorf("%s canonical prompt seen %d times", agent.name, seen[agent.name])
		}
	}
}

func TestHybridWorkerRunnerFakeCLIProcessE2E(t *testing.T) {
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	command := filepath.Join(tmp, "fake-codex")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run='^TestCodexCLIProcessHelper$' -- \"$@\"\n", testBinary)
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(tmp, "codex-home")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := bootstrap.Config{
		Provider: "local", ModelName: "gpt-test",
		Providers: map[string]bootstrap.ProviderConfig{
			"local": {Driver: "codex_cli", Command: command, CodexHome: codexHome},
		},
	}
	models, err := bootstrap.NewModelSet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	agents := []struct{ name, role string }{
		{name: "architect_short", role: "architect"},
		{name: "architect_long", role: "architect"},
		{name: "writer", role: "writer"},
		{name: "reviewer", role: "reviewer"},
		{name: "editor", role: "editor"},
	}
	definitions := make([]WorkerDefinition, 0, len(agents))
	toolsByAgent := make(map[string]*codexWorkerTestTool, len(agents))
	for _, agent := range agents {
		tool := &codexWorkerTestTool{}
		toolsByAgent[agent.name] = tool
		definitions = append(definitions, WorkerDefinition{
			Name: agent.name, Role: agent.role, SystemPrompt: "canonical " + agent.name,
			Tools: []agentcore.Tool{tool}, TerminalGuidance: "commit",
			Terminal: func(name string, _ json.RawMessage) bool { return name == "commit_test" },
		})
	}
	runtime := codexcli.NewRuntime(codexcli.RuntimeConfig{
		Command: command, CodexHome: codexHome, Platform: "darwin",
	})
	runner := newHybridWorkerRunner(cfg, models, nil, definitions, func(codexcli.RuntimeConfig) codexcli.Executor {
		return runtime
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, agent := range agents {
		result, err := runner.Run(ctx, agent.name, "run through the fake CLI process")
		if err != nil {
			t.Fatalf("%s: %v", agent.name, err)
		}
		if result.Output != "fake CLI completed" || result.Usage.Input != 17 || result.Usage.CacheRead != 3 || result.Usage.Output != 5 || toolsByAgent[agent.name].calls != 1 {
			t.Fatalf("%s fake CLI E2E result=%+v tool_calls=%d", agent.name, result, toolsByAgent[agent.name].calls)
		}
	}
}

func ConfigForCodexWorkerTest() bootstrap.Config {
	return bootstrap.Config{
		Provider: "local", ModelName: "gpt-test",
		Providers: map[string]bootstrap.ProviderConfig{
			"local": {Driver: "codex_cli", Command: "codex"},
		},
	}
}

func mcpSocketToken(t *testing.T, args []string) (socket, token string) {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--socket":
			socket = args[i+1]
		case "--token":
			token = args[i+1]
		}
	}
	if socket == "" || token == "" {
		t.Fatalf("missing socket/token in %#v", args)
	}
	return socket, token
}

func workerMCPCall(conn net.Conn, reader *bufio.Reader, id int, method string, params any) error {
	request, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if _, err := conn.Write(append(request, '\n')); err != nil {
		return err
	}
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return err
	}
	var response struct {
		Error  any `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &response); err != nil {
		return err
	}
	if response.Error != nil || response.Result.IsError {
		return fmt.Errorf("MCP call failed: %s", line)
	}
	return nil
}

func callWorkerMCPTool(t *testing.T, request codexcli.Request, tool string) error {
	t.Helper()
	if request.MCPServer == nil {
		return errors.New("missing MCP server")
	}
	socket, token := mcpSocketToken(t, request.MCPServer.Args)
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "{\"token\":%q}\n", token); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	if err := workerMCPCall(conn, reader, 1, "initialize", map[string]any{"protocolVersion": "2026-07-28"}); err != nil {
		return err
	}
	return workerMCPCall(conn, reader, 2, "tools/call", map[string]any{
		"name": tool, "arguments": map[string]any{"value": "done"},
	})
}
