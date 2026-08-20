package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/agentcore/subagent"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/codexcli"
)

const codexWorkerGracePeriod = 2 * time.Second

// WorkerRunner is the Engine-facing worker boundary. agentcore's Runner and
// HybridWorkerRunner both satisfy it.
type WorkerRunner interface {
	Run(context.Context, string, string) (subagent.RunResult, error)
}

type WorkerDefinition struct {
	Name             string
	Role             string
	SystemPrompt     string
	Tools            []agentcore.Tool
	Terminal         codexcli.TerminalPredicate
	TerminalGuidance string
	OnMessage        func(agentName, task string, msg agentcore.AgentMessage)
	HTTPConfig       subagent.Config
}

type codexExecutorFactory func(codexcli.RuntimeConfig) codexcli.Executor

// HybridWorkerRunner selects the backend at each Engine task boundary, so a
// hot model switch never mutates an in-flight Worker.
type HybridWorkerRunner struct {
	models      *bootstrap.ModelSet
	http        *subagent.Runner
	definitions map[string]WorkerDefinition
	factory     codexExecutorFactory
}

func NewHybridWorkerRunner(cfg bootstrap.Config, models *bootstrap.ModelSet, http *subagent.Runner, definitions []WorkerDefinition) *HybridWorkerRunner {
	return newHybridWorkerRunner(cfg, models, http, definitions, func(config codexcli.RuntimeConfig) codexcli.Executor {
		return codexcli.NewRuntime(config)
	})
}

func newHybridWorkerRunner(_ bootstrap.Config, models *bootstrap.ModelSet, http *subagent.Runner, definitions []WorkerDefinition, factory codexExecutorFactory) *HybridWorkerRunner {
	indexed := make(map[string]WorkerDefinition, len(definitions))
	for _, definition := range definitions {
		indexed[definition.Name] = definition
	}
	return &HybridWorkerRunner{models: models, http: http, definitions: indexed, factory: factory}
}

func (r *HybridWorkerRunner) Run(ctx context.Context, agent, task string) (subagent.RunResult, error) {
	definition, ok := r.definitions[agent]
	if !ok {
		available := make([]string, 0, len(r.definitions))
		for name := range r.definitions {
			available = append(available, name)
		}
		sort.Strings(available)
		return subagent.RunResult{}, &subagent.UnknownAgentError{Agent: agent, Available: available}
	}
	cfg, provider, model, _ := r.models.WorkerSelection(definition.Role)
	providerConfig, ok := cfg.Providers[provider]
	if !ok {
		return subagent.RunResult{}, fmt.Errorf("worker %s selected unknown provider %q", agent, provider)
	}
	roleConfig := cfg.Roles[definition.Role]
	if len(roleConfig.Fallbacks) == 0 {
		if !providerConfig.IsCodexCLI() {
			return r.http.Run(ctx, agent, task)
		}
		result, _, err := r.runCodex(ctx, cfg, definition, task, provider, model, providerConfig)
		return result, err
	}

	targets := []bootstrap.ModelRef{{Provider: provider, Model: model}}
	targets = append(targets, roleConfig.Fallbacks...)
	var lastResult subagent.RunResult
	var lastErr error
	for i, target := range targets {
		targetConfig, ok := cfg.Providers[target.Provider]
		if !ok {
			return lastResult, fmt.Errorf("worker fallback references unknown provider %q", target.Provider)
		}
		var sideEffects bool
		if targetConfig.IsCodexCLI() {
			lastResult, sideEffects, lastErr = r.runCodex(ctx, cfg, definition, task, target.Provider, target.Model, targetConfig)
		} else {
			lastResult, sideEffects, lastErr = r.runHTTPAttempt(ctx, definition, task, target.Provider, target.Model)
		}
		if lastErr == nil {
			return lastResult, nil
		}
		if sideEffects || i == len(targets)-1 {
			return lastResult, lastErr
		}
		slog.Warn("Worker task-level provider fallback", "module", "agent", "role", definition.Role,
			"from", target.Provider+"/"+target.Model, "to", targets[i+1].Provider+"/"+targets[i+1].Model,
			"err", lastErr)
	}
	return lastResult, lastErr
}

func (r *HybridWorkerRunner) runCodex(ctx context.Context, cfg bootstrap.Config, definition WorkerDefinition, task, provider, model string, providerConfig bootstrap.ProviderConfig) (subagent.RunResult, bool, error) {
	timeout, err := providerConfig.WorkerTimeoutValue()
	if err != nil {
		return subagent.RunResult{}, false, fmt.Errorf("Codex Worker timeout: %w", err)
	}
	if roleConfig, ok := cfg.Roles[definition.Role]; ok && strings.TrimSpace(roleConfig.Timeout) != "" {
		timeout, err = time.ParseDuration(strings.TrimSpace(roleConfig.Timeout))
		if err != nil || timeout <= 0 {
			return subagent.RunResult{}, false, fmt.Errorf("Codex Worker role timeout %q is invalid", roleConfig.Timeout)
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bridgeRoot, err := os.MkdirTemp("", "ainovel-worker-bridge-")
	if err != nil {
		return subagent.RunResult{}, false, fmt.Errorf("create Codex Worker bridge: %w", err)
	}
	defer os.RemoveAll(bridgeRoot)
	bridge, err := codexcli.StartToolBridge(runCtx, bridgeRoot, definition.Tools, definition.Terminal)
	if err != nil {
		return subagent.RunResult{}, false, err
	}
	defer bridge.Close()

	executable, err := os.Executable()
	if err != nil {
		return subagent.RunResult{}, false, fmt.Errorf("locate ainovel executable for private MCP bridge: %w", err)
	}
	toolNames := make([]string, 0, len(definition.Tools))
	for _, tool := range definition.Tools {
		toolNames = append(toolNames, tool.Name())
	}
	developer := strings.TrimSpace(definition.SystemPrompt) + "\n\n" +
		"ainovel Codex Worker transport: use only the private ainovel MCP tools. " +
		"Native shell, filesystem, web, apps, plugins, and skills are unavailable. " +
		"The task succeeds only after the business terminal tool reports success; final prose or exit code is not success. " +
		definition.TerminalGuidance

	executor := r.factory(codexcli.RuntimeConfig{Command: providerConfig.Command, CodexHome: providerConfig.CodexHome})
	resultCh := make(chan struct {
		result codexcli.Result
		err    error
	}, 1)
	go func() {
		result, err := executor.Run(runCtx, codexcli.Request{
			Model:                 model,
			ReasoningEffort:       codexcli.NormalizeReasoningEffort(cfg.ResolveReasoningEffort(definition.Role)),
			DeveloperInstructions: developer,
			Prompt:                task,
			MCPServer: &codexcli.MCPServer{
				Command: executable,
				Args: []string{
					"__codex-mcp", "--socket", bridge.SocketPath(), "--token", bridge.Token(),
				},
				EnabledTools: toolNames,
			},
		})
		resultCh <- struct {
			result codexcli.Result
			err    error
		}{result: result, err: err}
	}()

	var terminal codexcli.TerminalResult
	var execution struct {
		result codexcli.Result
		err    error
	}
	select {
	case terminal = <-bridge.Terminal():
		select {
		case execution = <-resultCh:
		case <-time.After(codexWorkerGracePeriod):
			cancel()
			execution = <-resultCh
		}
	case execution = <-resultCh:
		select {
		case terminal = <-bridge.Terminal():
		default:
		}
	case <-runCtx.Done():
		execution = <-resultCh
	}

	usage := subagent.Usage{
		Input:     int(execution.result.Usage.InputTokens),
		Output:    int(execution.result.Usage.OutputTokens),
		CacheRead: int(execution.result.Usage.CachedInputTokens),
		Turns:     1,
		Tools:     bridge.ToolCalls(),
	}
	runResult := subagent.RunResult{
		Agent: definition.Name, Output: execution.result.Final,
		TerminalResult: append(json.RawMessage(nil), terminal.Result...), Usage: usage,
	}
	slog.Info("Codex Worker task completed",
		"module", "agent", "role", definition.Role, "provider", provider, "model", model,
		"duration", execution.result.Duration, "exit_code", execution.result.ExitCode,
		"input_tokens", execution.result.Usage.InputTokens,
		"cached_input_tokens", execution.result.Usage.CachedInputTokens,
		"output_tokens", execution.result.Usage.OutputTokens,
		"tool_calls", bridge.ToolCalls(), "terminal_checkpoint", len(terminal.Result) > 0,
		"failed", len(terminal.Result) == 0 && execution.err != nil,
	)
	if len(terminal.Result) > 0 {
		// A durable terminal checkpoint outranks the expected grace-period
		// cancellation. Only normalized final text and usage are retained.
		if definition.OnMessage != nil {
			definition.OnMessage(definition.Name, task, codexAssistantMessage(execution.result, provider, model))
		}
		return runResult, true, nil
	}
	if execution.err != nil {
		if bridge.Mutated() {
			return runResult, true, fmt.Errorf("Codex Worker failed after business side effects; Engine must re-read Store facts: %w", execution.err)
		}
		return runResult, false, execution.err
	}
	return runResult, bridge.Mutated(), errors.New("Codex Worker exited without a successful terminal tool checkpoint")
}

func (r *HybridWorkerRunner) runHTTPAttempt(ctx context.Context, definition WorkerDefinition, task, provider, model string) (subagent.RunResult, bool, error) {
	configuredModel, err := r.models.ModelForRef(provider, model, definition.Role)
	if err != nil {
		return subagent.RunResult{}, false, err
	}
	tracker := &toolSideEffectTracker{}
	config := definition.HTTPConfig
	config.Model = configuredModel
	config.Tools = make([]agentcore.Tool, 0, len(definition.Tools))
	for _, tool := range definition.Tools {
		config.Tools = append(config.Tools, &trackedWorkerTool{Tool: tool, tracker: tracker})
	}
	runner := subagent.NewRunner(config)
	result, err := runner.Run(ctx, definition.Name, task)
	return result, tracker.Mutated(), err
}

func codexAssistantMessage(result codexcli.Result, provider, model string) agentcore.Message {
	return agentcore.Message{
		Role:       agentcore.RoleAssistant,
		Content:    []agentcore.ContentBlock{agentcore.TextBlock(result.Final)},
		StopReason: agentcore.StopReasonStop,
		Usage: &agentcore.Usage{
			Provider: provider, Model: model,
			Input: int(result.Usage.InputTokens), Output: int(result.Usage.OutputTokens),
			CacheRead:   int(result.Usage.CachedInputTokens),
			TotalTokens: int(result.Usage.InputTokens + result.Usage.OutputTokens),
		},
	}
}

func workerDefinition(role, terminalGuidance string, config subagent.Config) WorkerDefinition {
	predicate := codexcli.TerminalPredicate(config.StopAfterToolResult)
	if predicate == nil && len(config.StopAfterTools) > 0 {
		terminalTools := make(map[string]bool, len(config.StopAfterTools))
		for _, name := range config.StopAfterTools {
			terminalTools[name] = true
		}
		predicate = func(name string, _ json.RawMessage) bool { return terminalTools[name] }
	}
	return WorkerDefinition{
		Name: config.Name, Role: role, SystemPrompt: config.SystemPrompt,
		Tools: config.Tools, Terminal: predicate, TerminalGuidance: terminalGuidance,
		OnMessage:  config.OnMessage,
		HTTPConfig: config,
	}
}

type toolSideEffectTracker struct {
	mu      sync.Mutex
	mutated bool
}

func (t *toolSideEffectTracker) Mark() {
	t.mu.Lock()
	t.mutated = true
	t.mu.Unlock()
}
func (t *toolSideEffectTracker) Mutated() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.mutated
}

type trackedWorkerTool struct {
	agentcore.Tool
	tracker *toolSideEffectTracker
}

func (t *trackedWorkerTool) Execute(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if !t.ReadOnly(args) {
		// A failed write tool may have crossed a durable boundary before returning
		// its error. Mark the attempt up front so a second backend never starts
		// transparently over potentially changed Store facts.
		t.tracker.Mark()
	}
	result, err := t.Tool.Execute(ctx, args)
	return result, err
}
func (t *trackedWorkerTool) ReadOnly(args json.RawMessage) bool {
	readOnly, ok := t.Tool.(agentcore.ReadOnlyTool)
	return ok && readOnly.ReadOnly(args)
}
func (t *trackedWorkerTool) ConcurrencySafe(args json.RawMessage) bool {
	if safe, ok := t.Tool.(agentcore.ConcurrencySafeTool); ok {
		return safe.ConcurrencySafe(args)
	}
	return t.ReadOnly(args)
}
func (t *trackedWorkerTool) StrictSchema() bool {
	strict, ok := t.Tool.(agentcore.StrictSchemaTool)
	return ok && strict.StrictSchema()
}
func (t *trackedWorkerTool) Label() string {
	if label, ok := t.Tool.(agentcore.ToolLabeler); ok {
		return label.Label()
	}
	return t.Tool.Name()
}
