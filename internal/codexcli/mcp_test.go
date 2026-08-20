package codexcli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/voocel/agentcore"
)

type bridgeTestTool struct {
	name     string
	readOnly bool
	calls    int
}

func TestRunMCPProxyRelaysProtocolWithoutPollutingStdout(t *testing.T) {
	tool := &bridgeTestTool{name: "read", readOnly: true}
	bridge, err := StartToolBridge(context.Background(), t.TempDir(), []agentcore.Tool{tool}, func(name string, _ json.RawMessage) bool {
		return name == "read"
	})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()

	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2026-07-28\"}}\n")
	var stdout, stderr bytes.Buffer
	if err := RunMCPProxy(context.Background(), bridge.SocketPath(), bridge.Token(), input, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 || !json.Valid([]byte(lines[0])) || !strings.Contains(lines[0], `"id":1`) {
		t.Fatalf("proxy stdout must contain only one JSON-RPC response: %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("proxy emitted unexpected stderr: %q", stderr.String())
	}
}

func (t *bridgeTestTool) Name() string        { return t.name }
func (t *bridgeTestTool) Description() string { return "test " + t.name }
func (t *bridgeTestTool) Schema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"value": map[string]any{"type": "string"}},
		"required":   []string{"value"},
	}
}
func (t *bridgeTestTool) ReadOnly(json.RawMessage) bool { return t.readOnly }
func (t *bridgeTestTool) Execute(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	t.calls++
	return json.Marshal(map[string]any{"ok": true, "tool": t.name, "args": json.RawMessage(args)})
}

func TestToolBridgeImplementsMCPAndLocksWritesAfterTerminalTool(t *testing.T) {
	write := &bridgeTestTool{name: "commit", readOnly: false}
	read := &bridgeTestTool{name: "read", readOnly: true}
	bridge, err := StartToolBridge(context.Background(), t.TempDir(), []agentcore.Tool{write, read}, func(name string, _ json.RawMessage) bool {
		return name == "commit"
	})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()

	conn, err := net.Dial("unix", bridge.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(`{"token":"` + bridge.Token() + `"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)

	response := mcpRoundTrip(t, conn, reader, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if response.Result == nil || response.Error != nil {
		t.Fatalf("initialize response = %#v", response)
	}
	response = mcpRoundTrip(t, conn, reader, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	encoded, _ := json.Marshal(response.Result)
	if !json.Valid(encoded) || !containsAll(string(encoded), `"name":"commit"`, `"name":"read"`, `"inputSchema"`) {
		t.Fatalf("tools/list = %s", encoded)
	}

	response = mcpRoundTrip(t, conn, reader, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"commit","arguments":{"value":"done"}}}`)
	encoded, _ = json.Marshal(response.Result)
	if !containsAll(string(encoded), `"isError":false`, `"ok":true`) {
		t.Fatalf("terminal tools/call = %s", encoded)
	}
	select {
	case terminal := <-bridge.Terminal():
		if terminal.Tool != "commit" || len(terminal.Result) == 0 {
			t.Fatalf("terminal = %#v", terminal)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal tool did not signal success")
	}

	response = mcpRoundTrip(t, conn, reader, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"commit","arguments":{"value":"again"}}}`)
	encoded, _ = json.Marshal(response.Result)
	if !containsAll(string(encoded), `"isError":true`, "terminal checkpoint") || write.calls != 1 {
		t.Fatalf("post-terminal write was not rejected: %s calls=%d", encoded, write.calls)
	}
	response = mcpRoundTrip(t, conn, reader, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"read","arguments":{"value":"facts"}}}`)
	encoded, _ = json.Marshal(response.Result)
	if !containsAll(string(encoded), `"isError":false`, `"tool":"read"`) || read.calls != 1 {
		t.Fatalf("post-terminal read should remain available during grace period: %s", encoded)
	}
}

type mcpTestResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
	Error   any             `json:"error"`
}

func mcpRoundTrip(t *testing.T, conn net.Conn, reader *bufio.Reader, request string) mcpTestResponse {
	t.Helper()
	if _, err := conn.Write([]byte(request + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var response mcpTestResponse
	if err := json.Unmarshal(line, &response); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	return response
}

func containsAll(text string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(text, part) {
			return false
		}
	}
	return true
}
