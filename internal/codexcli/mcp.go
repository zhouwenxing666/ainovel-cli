package codexcli

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/llmcontract"
)

// TerminalPredicate recognizes the successful business tool result that makes
// a Worker task durable. Process exit or final prose never implies success.
type TerminalPredicate func(tool string, result json.RawMessage) bool

type TerminalResult struct {
	Tool   string
	Result json.RawMessage
}

// ToolBridge is a private Unix-socket MCP server. A tiny stdio proxy spawned by
// Codex authenticates to this socket; the actual tools remain in the ainovel
// process and therefore retain their Store contracts and progress context.
type ToolBridge struct {
	ctx       context.Context
	cancel    context.CancelFunc
	listener  net.Listener
	socket    string
	socketDir string
	ownedDir  bool
	token     string
	tools     map[string]agentcore.Tool
	ordered   []agentcore.Tool
	terminal  TerminalPredicate
	termCh    chan TerminalResult

	mu        sync.Mutex
	locked    bool
	mutated   bool
	calls     int
	closeOnce sync.Once
}

func StartToolBridge(ctx context.Context, directory string, tools []agentcore.Tool, terminal TerminalPredicate) (*ToolBridge, error) {
	if len(tools) == 0 {
		return nil, errors.New("Codex Worker requires at least one MCP tool")
	}
	if terminal == nil {
		return nil, errors.New("Codex Worker requires a terminal tool predicate")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create MCP bridge directory: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("create MCP bridge token: %w", err)
	}
	socketDir := directory
	ownedDir := false
	socket := filepath.Join(socketDir, "bridge.sock")
	// Darwin's sockaddr_un path is very short. Test/project temp roots can
	// exceed it, so create a dedicated 0700 runtime directory under TMPDIR.
	if len(socket) >= 96 {
		shortDir, err := os.MkdirTemp("", "ainovel-mcp-")
		if err != nil {
			return nil, fmt.Errorf("create short MCP socket directory: %w", err)
		}
		socketDir = shortDir
		ownedDir = true
		socket = filepath.Join(socketDir, "bridge.sock")
	}
	_ = os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		if ownedDir {
			_ = os.RemoveAll(socketDir)
		}
		return nil, fmt.Errorf("listen on private MCP socket: %w", err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		listener.Close()
		return nil, fmt.Errorf("protect private MCP socket: %w", err)
	}
	bridgeCtx, cancel := context.WithCancel(ctx)
	bridge := &ToolBridge{
		ctx: bridgeCtx, cancel: cancel, listener: listener, socket: socket,
		socketDir: socketDir, ownedDir: ownedDir,
		token: hex.EncodeToString(secret), tools: make(map[string]agentcore.Tool, len(tools)),
		ordered: append([]agentcore.Tool(nil), tools...), terminal: terminal,
		termCh: make(chan TerminalResult, 1),
	}
	for _, tool := range tools {
		if tool == nil || tool.Name() == "" {
			bridge.Close()
			return nil, errors.New("MCP bridge tool name is required")
		}
		if _, exists := bridge.tools[tool.Name()]; exists {
			bridge.Close()
			return nil, fmt.Errorf("duplicate MCP bridge tool %q", tool.Name())
		}
		bridge.tools[tool.Name()] = tool
	}
	go bridge.serve()
	return bridge, nil
}

func (b *ToolBridge) SocketPath() string              { return b.socket }
func (b *ToolBridge) Token() string                   { return b.token }
func (b *ToolBridge) Terminal() <-chan TerminalResult { return b.termCh }
func (b *ToolBridge) Mutated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mutated
}
func (b *ToolBridge) ToolCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func (b *ToolBridge) Close() error {
	var err error
	b.closeOnce.Do(func() {
		b.cancel()
		err = b.listener.Close()
		_ = os.Remove(b.socket)
		if b.ownedDir {
			_ = os.RemoveAll(b.socketDir)
		}
	})
	return err
}

func (b *ToolBridge) serve() {
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			return
		}
		go b.serveConn(conn)
	}
}

func (b *ToolBridge) serveConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64<<10), maxJSONLEventBytes)
	if !scanner.Scan() {
		return
	}
	var auth struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(scanner.Bytes(), &auth) != nil || subtle.ConstantTimeCompare([]byte(auth.Token), []byte(b.token)) != 1 {
		return
	}
	encoder := json.NewEncoder(conn)
	for scanner.Scan() {
		response, reply := b.handle(scanner.Bytes())
		if reply {
			if err := encoder.Encode(response); err != nil {
				return
			}
		}
	}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (b *ToolBridge) handle(line []byte) (rpcResponse, bool) {
	var request rpcRequest
	if err := json.Unmarshal(line, &request); err != nil {
		return rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}}, true
	}
	if len(request.ID) == 0 {
		return rpcResponse{}, false
	}
	response := rpcResponse{JSONRPC: "2.0", ID: request.ID}
	switch request.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(request.Params, &params)
		if params.ProtocolVersion == "" {
			params.ProtocolVersion = "2026-07-28"
		}
		response.Result = map[string]any{
			"protocolVersion": params.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "ainovel-private-tools", "version": "1"},
		}
	case "ping":
		response.Result = map[string]any{}
	case "tools/list":
		tools := make([]map[string]any, 0, len(b.ordered))
		for _, tool := range b.ordered {
			readOnly := false
			if ro, ok := tool.(agentcore.ReadOnlyTool); ok {
				readOnly = ro.ReadOnly(json.RawMessage(`{}`))
			}
			tools = append(tools, map[string]any{
				"name": tool.Name(), "description": tool.Description(), "inputSchema": tool.Schema(),
				"annotations": map[string]any{"readOnlyHint": readOnly},
			})
		}
		response.Result = map[string]any{"tools": tools}
	case "tools/call":
		result, protocolErr := b.callTool(request.Params)
		if protocolErr != nil {
			response.Error = protocolErr
		} else {
			response.Result = result
		}
	default:
		response.Error = &rpcError{Code: -32601, Message: "method not found"}
	}
	return response, true
}

func (b *ToolBridge) callTool(rawParams json.RawMessage) (map[string]any, *rpcError) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(rawParams, &params); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid tools/call params"}
	}
	tool, ok := b.tools[params.Name]
	if !ok {
		return nil, &rpcError{Code: -32602, Message: fmt.Sprintf("unknown tool %q", params.Name)}
	}
	args, err := json.Marshal(params.Arguments)
	if err != nil {
		return toolErrorResult(err), nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	writeAttempt := !toolReadOnly(tool, args)
	if b.locked && writeAttempt {
		return toolErrorResult(errors.New("terminal checkpoint already succeeded; later write tools are locked")), nil
	}
	if schema := tool.Schema(); schema != nil {
		if err := llmcontract.ValidateJSON(schema, args); err != nil {
			return toolErrorResult(fmt.Errorf("tool argument validation: %w", err)), nil
		}
	}
	if validator, ok := tool.(agentcore.Validator); ok {
		verdict := validator.Validate(b.ctx, args)
		if !verdict.OK {
			message := verdict.Message
			if message == "" {
				message = "tool input validation failed"
			}
			return toolErrorResult(errors.New(message)), nil
		}
	}
	if previewer, ok := tool.(agentcore.Previewer); ok {
		if _, err := previewer.Preview(b.ctx, args); err != nil {
			return toolErrorResult(fmt.Errorf("tool preview: %w", err)), nil
		}
	}
	if writeAttempt {
		// Once a write-capable tool enters Execute, a returned error cannot prove
		// that no durable side effect occurred. Mark before execution so task-level
		// fallback is conservative across partial writes and Saga recovery paths.
		b.mutated = true
	}
	agentcore.ReportToolProgress(b.ctx, agentcore.ProgressPayload{
		Kind: agentcore.ProgressToolStart, Tool: params.Name, Args: append(json.RawMessage(nil), args...),
	})
	result, err := tool.Execute(b.ctx, args)
	if err != nil {
		agentcore.ReportToolProgress(b.ctx, agentcore.ProgressPayload{
			Kind: agentcore.ProgressToolError, Tool: params.Name, Message: err.Error(), IsError: true,
		})
		return toolErrorResult(err), nil
	}
	b.calls++
	agentcore.ReportToolProgress(b.ctx, agentcore.ProgressPayload{
		Kind: agentcore.ProgressToolEnd, Tool: params.Name, Summary: "工具执行完成",
	})
	toolResult := toolSuccessResult(result)
	if b.terminal(params.Name, result) && !b.locked {
		b.locked = true
		terminal := TerminalResult{Tool: params.Name, Result: append(json.RawMessage(nil), result...)}
		b.termCh <- terminal
	}
	return toolResult, nil
}

func toolReadOnly(tool agentcore.Tool, args json.RawMessage) bool {
	readOnly, ok := tool.(agentcore.ReadOnlyTool)
	return ok && readOnly.ReadOnly(args)
}

func toolErrorResult(err error) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": err.Error()}},
		"isError": true,
	}
}

func toolSuccessResult(raw json.RawMessage) map[string]any {
	text := string(raw)
	var structured any
	if len(raw) > 0 && json.Unmarshal(raw, &structured) != nil {
		structured = map[string]any{"result": text}
	}
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": text}},
		"structuredContent": structured,
		"isError":           false,
	}
}

// RunMCPProxy connects Codex's stdio MCP transport to the private parent
// socket. stdout remains protocol-only; diagnostics belong on stderr.
func RunMCPProxy(ctx context.Context, socket, token string, stdin io.Reader, stdout, stderr io.Writer) error {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return fmt.Errorf("connect private MCP bridge: %w", err)
	}
	defer conn.Close()
	auth, _ := json.Marshal(map[string]string{"token": token})
	if _, err := conn.Write(append(auth, '\n')); err != nil {
		return fmt.Errorf("authenticate private MCP bridge: %w", err)
	}
	copyErr := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, stdin)
		if unixConn, ok := conn.(*net.UnixConn); ok {
			_ = unixConn.CloseWrite()
		}
		copyErr <- err
	}()
	_, outErr := io.Copy(stdout, conn)
	if outErr != nil {
		return fmt.Errorf("relay private MCP response: %w", outErr)
	}
	if inErr := <-copyErr; inErr != nil {
		return fmt.Errorf("relay private MCP request: %w", inErr)
	}
	return nil
}
