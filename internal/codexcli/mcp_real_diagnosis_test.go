package codexcli

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/voocel/agentcore"
)

type diagnosisPingTool struct{}

func (diagnosisPingTool) Name() string { return "diagnostic_ping" }
func (diagnosisPingTool) Description() string {
	return "Required diagnostic tool; call it once with value ok."
}
func (diagnosisPingTool) Schema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"value": map[string]any{"type": "string"}},
		"required":   []string{"value"},
	}
}
func (diagnosisPingTool) Execute(context.Context, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"checkpoint":true}`), nil
}

func TestRealMCPDiagnosis(t *testing.T) {
	if os.Getenv("AINOVEL_CODEX_MCP_DIAGNOSIS") != "1" {
		t.Skip("diagnosis only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	bridge, err := StartToolBridge(ctx, t.TempDir(), []agentcore.Tool{diagnosisPingTool{}},
		func(name string, _ json.RawMessage) bool { return name == "diagnostic_ping" })
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	result, err := NewRuntime(RuntimeConfig{Command: "/tmp/ainovel-codex-mcp-diag-wrapper"}).Run(ctx, Request{
		Model:                 "gpt-5.6-sol",
		DeveloperInstructions: "You must call diagnostic_ping exactly once. Do not answer with prose before calling it.",
		Prompt:                "Complete the diagnostic by calling diagnostic_ping with value ok.",
		MCPServer: &MCPServer{
			Command:      "/usr/local/bin/ainovel-cli",
			Args:         []string{"__codex-mcp", "--socket", bridge.SocketPath(), "--token", bridge.Token()},
			EnabledTools: []string{"diagnostic_ping"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case terminal := <-bridge.Terminal():
		t.Logf("MCP terminal=%s result=%s", terminal.Tool, terminal.Result)
	default:
		t.Fatalf("Codex exited without MCP call; final=%q thread=%s", result.Final, result.ThreadID)
	}
}
