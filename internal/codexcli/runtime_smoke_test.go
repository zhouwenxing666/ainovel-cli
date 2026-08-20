package codexcli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/voocel/agentcore"
)

// TestRuntimeRealPreflightOptIn performs no model request and consumes no
// quota. It validates the installed CLI/auth boundary used for release smoke.
func TestRuntimeRealPreflightOptIn(t *testing.T) {
	if os.Getenv("AINOVEL_CODEX_PREFLIGHT") != "1" {
		t.Skip("set AINOVEL_CODEX_PREFLIGHT=1 to validate the installed Codex CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := NewRuntime(RuntimeConfig{Command: "codex"}).Preflight(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestRuntimeRealSmokeOptIn is deliberately skipped unless explicitly
// enabled. It consumes the user's saved-login Codex quota and exists only for
// release validation on a macOS developer machine.
func TestRuntimeRealSmokeOptIn(t *testing.T) {
	if os.Getenv("AINOVEL_CODEX_SMOKE") != "1" {
		t.Skip("set AINOVEL_CODEX_SMOKE=1 to run the real Codex CLI smoke test")
	}
	model := strings.TrimSpace(os.Getenv("AINOVEL_CODEX_SMOKE_MODEL"))
	if model == "" {
		t.Fatal("AINOVEL_CODEX_SMOKE_MODEL is required when the real smoke test is enabled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	result, err := NewRuntime(RuntimeConfig{Command: "codex"}).Run(ctx, Request{
		Model:                 model,
		DeveloperInstructions: "Return exactly the requested short text. Do not use tools.",
		Prompt:                "Reply with: ainovel codex smoke ok",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(result.Final), "ainovel codex smoke ok") {
		t.Fatalf("unexpected smoke response: %q", result.Final)
	}
}

// TestRuntimeRealStructuredSmokeOptIn covers the production completion seam
// that Arbiter and semantic helpers use. The input deliberately omits
// additionalProperties so the adapter must make every object Codex-strict.
func TestRuntimeRealStructuredSmokeOptIn(t *testing.T) {
	if os.Getenv("AINOVEL_CODEX_STRUCTURED_SMOKE") != "1" {
		t.Skip("set AINOVEL_CODEX_STRUCTURED_SMOKE=1 to run the real Codex structured-output smoke test")
	}
	modelName := strings.TrimSpace(os.Getenv("AINOVEL_CODEX_SMOKE_MODEL"))
	if modelName == "" {
		t.Fatal("AINOVEL_CODEX_SMOKE_MODEL is required when the real structured smoke test is enabled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	model := NewCompletionModel(NewRuntime(RuntimeConfig{Command: "codex"}), CompletionModelConfig{
		Provider: "local-codex", Model: modelName, Timeout: 3 * time.Minute,
	})
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"result": map[string]any{
				"type":       "object",
				"properties": map[string]any{"status": map[string]any{"type": "string", "enum": []string{"ok"}}},
				"required":   []string{"status"},
			},
		},
		"required": []string{"result"},
	}
	response, err := model.Generate(ctx, []agentcore.Message{agentcore.UserMsg("Return status ok.")}, nil,
		agentcore.WithJSONSchema("ainovel_codex_smoke", "structured smoke", schema, true))
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Result struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(response.Message.TextContent()), &body); err != nil {
		t.Fatal(err)
	}
	if body.Result.Status != "ok" {
		t.Fatalf("unexpected structured smoke response: %q", response.Message.TextContent())
	}
}
