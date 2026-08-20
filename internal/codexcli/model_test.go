package codexcli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/voocel/agentcore"
)

type recordingExecutor struct {
	request Request
	result  Result
	err     error
}

func (e *recordingExecutor) Run(_ context.Context, request Request) (Result, error) {
	e.request = request
	return e.result, e.err
}

func TestCompletionModelMapsMessagesSchemaThinkingAndUsage(t *testing.T) {
	executor := &recordingExecutor{result: Result{
		Final: `{"decision":"continue"}`,
		Usage: Usage{InputTokens: 21, CachedInputTokens: 5, OutputTokens: 9},
	}}
	model := NewCompletionModel(executor, CompletionModelConfig{
		Provider:        "local-codex",
		Model:           "gpt-5.3-codex",
		ReasoningEffort: "medium",
		Timeout:         time.Minute,
	})
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"decision": map[string]any{"type": "string"}},
	}
	response, err := model.Generate(context.Background(), []agentcore.Message{
		agentcore.SystemMsg("You are the Arbiter."),
		agentcore.UserMsg("Choose the next action."),
	}, nil,
		agentcore.WithThinking(agentcore.ThinkingHigh),
		agentcore.WithJSONSchema("arbiter", "decision", schema, true),
	)
	if err != nil {
		t.Fatal(err)
	}
	if executor.request.Model != "gpt-5.3-codex" || executor.request.ReasoningEffort != "high" {
		t.Fatalf("request identity/thinking mismatch: %#v", executor.request)
	}
	if !strings.Contains(executor.request.DeveloperInstructions, "You are the Arbiter.") {
		t.Fatalf("canonical system prompt not preserved: %q", executor.request.DeveloperInstructions)
	}
	if executor.request.Prompt != "Choose the next action." {
		t.Fatalf("prompt = %q", executor.request.Prompt)
	}
	var gotSchema map[string]any
	if err := json.Unmarshal(executor.request.OutputSchema, &gotSchema); err != nil {
		t.Fatalf("output schema: %v", err)
	}
	if gotSchema["type"] != "object" {
		t.Fatalf("schema = %#v", gotSchema)
	}
	message := response.Message
	if message.TextContent() != `{"decision":"continue"}` || message.StopReason != agentcore.StopReasonStop {
		t.Fatalf("message = %#v", message)
	}
	if message.Usage == nil || message.Usage.Provider != "local-codex" || message.Usage.Model != "gpt-5.3-codex" || message.Usage.Input != 21 || message.Usage.CacheRead != 5 || message.Usage.Output != 9 || message.Usage.TotalTokens != 30 {
		t.Fatalf("usage = %#v", message.Usage)
	}
	if model.SupportsTools() {
		t.Fatal("single-call Codex adapter must not claim agentcore tool support")
	}
}

func TestCompletionModelMakesNestedOutputSchemaCodexStrict(t *testing.T) {
	executor := &recordingExecutor{result: Result{Final: `{"ok":true}`}}
	model := NewCompletionModel(executor, CompletionModelConfig{Model: "gpt-5.6-sol"})
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"nested": map[string]any{
				"type":       "object",
				"properties": map[string]any{"name": map[string]any{"type": "string"}},
				"required":   []string{"name"},
			},
			"nullable": map[string]any{
				"type":       []string{"object", "null"},
				"properties": map[string]any{"reason": map[string]any{"type": "string"}},
				"required":   []string{"reason"},
			},
			"items": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":       "object",
					"properties": map[string]any{"value": map[string]any{"type": "integer"}},
					"required":   []string{"value"},
				},
			},
		},
		"required": []string{"nested", "nullable", "items"},
	}
	if _, err := model.Generate(context.Background(), []agentcore.Message{agentcore.UserMsg("x")}, nil,
		agentcore.WithJSONSchema("nested", "nested schema", schema, true)); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(executor.request.OutputSchema, &got); err != nil {
		t.Fatal(err)
	}
	properties := got["properties"].(map[string]any)
	nested := properties["nested"].(map[string]any)
	nullable := properties["nullable"].(map[string]any)
	arrayItems := properties["items"].(map[string]any)["items"].(map[string]any)
	for path, object := range map[string]map[string]any{
		"$": got, "$.nested": nested, "$.nullable": nullable, "$.items[]": arrayItems,
	} {
		if value, ok := object["additionalProperties"].(bool); !ok || value {
			t.Errorf("%s additionalProperties = %#v, want false", path, object["additionalProperties"])
		}
	}
}

func TestCompletionModelRejectsAgentcoreTools(t *testing.T) {
	model := NewCompletionModel(&recordingExecutor{}, CompletionModelConfig{Model: "gpt-5.3-codex"})
	_, err := model.Generate(context.Background(), []agentcore.Message{agentcore.UserMsg("x")}, []agentcore.ToolSpec{{Name: "unsafe"}})
	if err == nil {
		t.Fatal("completion adapter must reject dynamic tools instead of impersonating a Worker backend")
	}
}

func TestNormalizeReasoningEffortDropsUnsupportedValues(t *testing.T) {
	for _, supported := range []string{"low", "medium", "high", "xhigh", "max"} {
		if got := NormalizeReasoningEffort(supported); got != supported {
			t.Errorf("NormalizeReasoningEffort(%q) = %q", supported, got)
		}
	}
	for _, unsupported := range []string{"", "off", "minimal", "turbo", " HIGH "} {
		want := ""
		if unsupported == " HIGH " {
			want = "high"
		}
		if got := NormalizeReasoningEffort(unsupported); got != want {
			t.Errorf("NormalizeReasoningEffort(%q) = %q, want %q", unsupported, got, want)
		}
	}
}

func TestCompletionModelDoesNotForwardUnsupportedReasoningEffort(t *testing.T) {
	executor := &recordingExecutor{result: Result{Final: "ok"}}
	model := NewCompletionModel(executor, CompletionModelConfig{Model: "gpt-test", ReasoningEffort: "off"})
	if _, err := model.Generate(context.Background(), []agentcore.Message{agentcore.UserMsg("x")}, nil); err != nil {
		t.Fatal(err)
	}
	if executor.request.ReasoningEffort != "" {
		t.Fatalf("unsupported Codex reasoning effort was forwarded: %q", executor.request.ReasoningEffort)
	}
}
