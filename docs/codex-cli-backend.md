# 本地 Codex CLI Backend 实施计划

> 状态：已完成  
> 日期：2026-08-09  
> 范围：让用户通过本机 Codex CLI 登录，为 Arbiter、Architect、Writer、Reviewer、Editor 以及正常流程中的单次语义调用提供模型能力；不要求 API Key。

## 1. 目标与完成定义

用户可以把 `driver: "codex_cli"` 的具名 provider 配置为顶层默认模型或任意角色模型。未显式覆盖的角色继续继承顶层 default；HTTP provider 与 Codex provider 可以混用，并可出现在同一角色的显式 fallback 链中。

功能只有在以下能力全部可用时才标记为正式支持：

- Arbiter 通过无工具、ephemeral、JSON Schema 约束的 `codex exec` 完成裁定，结果继续通过现有业务校验和审计。
- Architect、Writer、Reviewer、Editor 通过独立 `CodexWorkerBackend` 运行，只能调用该角色当前已有的小说工具。
- 用户规则归一化、共创、导入、仿写等单次调用可以使用 Codex default，不产生隐藏的 API Key 依赖。
- setup、`/config`、`/model`、热切换、fallback、usage、诊断、恢复和文档完整支持。
- fake Codex CLI 的离线端到端测试覆盖成功、失败、取消、超时、JSONL、Schema、MCP 和部分副作用恢复。

官方契约依据：

- [Codex 非交互模式](https://learn.chatgpt.com/docs/non-interactive-mode)：`codex exec`、`--ephemeral`、`--json`、`--output-schema`、saved CLI auth 和 JSONL usage。
- [Codex MCP 配置](https://learn.chatgpt.com/docs/extend/mcp)：stdio server、`required`、`enabled_tools`、启动与工具超时。
- [Codex sandbox](https://learn.chatgpt.com/docs/sandboxing)：sandbox 与 approval 是独立控制，自动化必须采用最小权限。
- [MCP stdio 规范](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/stdio)：stdout 只能输出逐行 JSON-RPC，日志只能写 stderr，关闭 stdin 后须优雅退出。
- [MCP tools 规范](https://modelcontextprotocol.io/specification/2026-07-28/server/tools)：确定性 `tools/list`、JSON Schema 输入、结构化工具结果和 `isError`。

## 2. 锁定决策

### 2.1 配置与兼容性

- `providers.<name>.driver` 缺省为现有 HTTP 模式；旧配置不自动重写。选中的 Local Codex 若缺少模型或推理强度，则在加载后的有效配置中补 `gpt-5.6-sol` / `xhigh`，显式值优先。
- Codex provider 允许作为顶层 default；新增合法角色 `roles.arbiter`。
- Codex provider 只开放结构化字段：裸命令名或绝对路径 `command`、可选绝对路径 `codex_home`、模型库、单次调用超时和 Worker 超时。不接受相对可执行路径、shell 字符串、任意参数或任意环境变量。
- `driver`、`command`、`codex_home` 只能来自全局配置。项目配置可以引用全局 Codex provider并覆盖角色、模型、推理强度、timeout 和 fallback，但不能创建或替换可执行文件。
- 保存 provider、启动和热切换时预检实际引用到的 primary/fallback；未引用的坏配置不阻止启动。
- 热切换只影响下一个 Engine 任务边界。

目标配置形状：

```jsonc
{
  "provider": "local-codex",
  "model": "gpt-5.6-sol",
  "reasoning_effort": "xhigh",
  "providers": {
    "local-codex": {
      "driver": "codex_cli",
      "command": "codex",
      "models": [{ "name": "gpt-5.6-sol" }],
      "single_call_timeout": "3m",
      "worker_timeout": "30m"
    }
  },
  "roles": {
    "arbiter": {
      "provider": "local-codex",
      "model": "gpt-5.6-sol"
    },
    "writer": {
      "provider": "local-codex",
      "model": "gpt-5.6-sol",
      "fallbacks": [
        { "provider": "my-api", "model": "fallback-model" }
      ]
    }
  }
}
```

### 2.2 运行时与事实层

- `codex exec` 是自治 Agent runtime，不伪装成现有 `agentcore.ChatModel` Worker 循环。
- Engine/Router/Store 继续拥有流程与事实；Codex 只替换一次角色任务的执行 adapter。
- 每次调用使用全新 ephemeral session；Store 是唯一长期记忆。
- Worker 成功以终态业务工具和新 Checkpoint 为准，退出码或 final text 不能单独证明成功。
- 终态工具成功后，MCP bridge 标记任务完成并拒绝后续写操作；给予短暂退出窗口，超时后终止子进程，已存在的终态 Checkpoint 仍视为成功。
- Arbiter 和其他单次语义调用不注入 MCP，不访问项目或小说文件；事实全部进入 prompt。

### 2.3 隔离与平台

- 仅复用 saved CLI authentication；运行时使用空临时目录、`--ignore-user-config`、`--ignore-rules`、`--sandbox read-only`、`-c approval_policy="never"` 和 `--ephemeral`。
- 仅临时注入本次角色的私有 stdio MCP server，不执行 `codex mcp add`，不修改 `~/.codex`，不运行常驻 daemon。
- 能禁用 Codex 原生工具时禁用；否则它们只能看到空临时目录，不能挂载源码或小说目录。
- 隔离能力预检失败即拒绝启用，不降级为“警告后继续”。
- 正式支持范围仅 macOS；其他 OS 明确报错。检测到 Docker/容器环境时禁止 Codex provider。

### 2.4 失败、fallback 与用量

- Arbiter和无工具单次调用可在总体 timeout 内按现有结构化反馈语义重试。
- Worker 只有在尚无 MCP 业务副作用时可以透明 retry/fallback。发生副作用后，错误交回 Engine，重新读取 Store/Checkpoint 后再路由。
- 跨 HTTP/Codex driver 的 fallback 按完整角色任务切换，不拼接两个 backend 的半截会话。
- 取消时关闭管道，向整个进程组发送 TERM，宽限后 KILL；stdout/stderr 必须并发 drain 且有尺寸上限。
- 记录规范化事件、工具结果、token、调用次数、耗时、CLI 版本与错误摘要；不长期保存原始 JSONL、完整 stderr、临时 Schema 或 Codex session。
- ChatGPT/Codex 订阅调用不伪造 API 美元价格；美元成本显示 N/A，不计入 API 金额预算，并明确提示预算覆盖范围。

## 3. 深模块与测试 seam

以下 seam 已在设计访谈中确认，测试只通过这些 interface 验证行为：

1. **配置 seam**：`bootstrap.LoadConfig / ValidateBase / NewModelSet`。验证旧配置兼容、全局/项目信任边界、`roles.arbiter`、Codex default、fallback 和热切换。
2. **Codex runtime seam**：`CodexRuntime.Run(ctx, Request) (Result, error)`。生产 adapter 启动真实进程；fake CLI adapter 通过同一 interface 输出已知 JSONL、stderr、退出码和时序。
3. **MCP seam**：真正的逐行 stdio JSON-RPC `tools/list` 与 `tools/call`。验证角色 allowlist、Schema、结构化结果、错误、取消、stdout 纯净和终态封锁。
4. **Worker seam**：`WorkerRunner.Run(ctx, agent, task)`。现有 agentcore adapter 与 Codex adapter 是两个真实实现；验证任务结果只由 Store/Checkpoint 决定。
5. **Host seam**：现有 Engine/Store 测试保持 backend 无关；fake CLI 子进程测试穿透真实 Worker adapter、MCP bridge 与业务工具，组合验证四角色路由、Codex Arbiter 结构化边界、跨 backend fallback、部分副作用恢复和下一边界热切换。

不测试私有 helper 的调用次数，不 mock 仓库内部模块，不断言进程模块的内部函数分解。

## 4. 实施切片

### Slice A：配置与解析

- 扩展 `ProviderConfig`：driver、command、codex_home、single_call_timeout、worker_timeout。
- 扩展 global/project merge，禁止项目覆盖进程启动字段。
- 让 Codex provider 无需 API key/type；加入 `roles.arbiter`。
- 为 default/role/fallback 暴露 backend selection facts，旧 HTTP ModelSet 行为不变。
- 更新根目录与 embedded `config.example.jsonc`，保持字节一致性测试。

### Slice B：Codex process runtime

- 新增 `internal/codexcli` 深模块：预检、Request/Result、参数构建、stdin prompt、JSONL parser、Schema 临时文件、usage、stderr 上限、进程组取消、临时目录清理。
- 所有启动使用 `exec.CommandContext`/参数数组，不经过 shell；prompt 只走 stdin。
- 未知非关键事件忽略并记录；缺失关键 final/turn completion/tool result 时返回稳定协议错误。
- fake CLI 是系统边界 adapter，覆盖 auth/version/capability 预检和主要失败分类。

### Slice C：单次模型 adapter

- 实现无工具 Codex completion adapter，满足 default、Arbiter和辅助调用所需的 `Generate`/结构化能力。
- Arbiter 改用 `models.ForRole("arbiter")`，保持现有 `llmcontract.Execute` 与 `Validate`。
- 模型/推理强度按调用显式下发；用户原始 reasoning intent 保留，实际值按能力钳制。

### Slice D：私有 MCP bridge

- 实现所需的最小 MCP JSON-RPC server（initialize、ping、tools/list、tools/call），以父进程私有 Unix socket + 短生命周期 stdio proxy 承载；不写用户 Codex 配置。
- 将现有 `agentcore.Tool` 适配为 MCP tool，保留原名称、描述和输入 Schema，返回文本与结构化内容。
- 每次 Worker 只注册角色已有工具，顺序确定；事实层、终态和失败约束作为 canonical role prompt 的薄 transport 后缀注入。
- 工具执行沿用现有 Store、前置条件、原子写入和 progress observer；annotations 仅用于描述，不能替代服务端授权。

### Slice E：Codex WorkerBackend

- 引入小型 `WorkerRunner` interface；现有 `subagent.Runner` 作为 HTTP adapter。
- Hybrid runner 在每个任务开始时解析角色当前 backend，整次任务固定使用一个 adapter。
- Codex prompt 复用 canonical role prompt，只追加 transport/终态说明；Architect short/long 继续共享 architect 配置。
- 以任务前后 Checkpoint delta 验证完成；副作用标记来自 MCP bridge，而不是 JSONL 文本猜测。

### Slice F：Host、UI 与运行策略

- 接入跨 backend fallback、Engine 重读事实、并发 Arbiter和 next-boundary hot switch。
- 将 Codex usage 映射为现有 observer/session/usage 事件；成本标记 unavailable。
- setup、`/config`、`/model` 展示 driver、command、模型和角色（含 Arbiter）；无模型请求预检明确报告版本、登录与隔离能力状态。
- 保存/启动/切换时做无模型请求的预检；不自动安装或登录。
- 加入 macOS 与 Docker 门禁、最低版本/能力检测和可操作错误信息。

### Slice G：发布验证

- 单包测试逐 slice 红→绿；随后运行 `go test ./...`。
- fake CLI E2E 必须覆盖 Arbiter、Architect、Writer、Reviewer、Editor 各一条成功路径。
- 可选真实 smoke test必须显式 opt-in，默认测试绝不消耗 Codex quota。
- 更新 README、architecture、配置示例和诊断说明；说明 token/预算、平台与隐私限制。

## 5. 实施结果与验证

上述切片已经全部落地：配置、macOS 预检、隔离进程 runtime、无工具 completion adapter、私有 MCP bridge、Hybrid WorkerRunner、Arbiter 角色模型、task-level fallback、热切换、setup/TUI、usage 与文档均已接通。Codex completion adapter 会递归把共享输出契约规范化为 Codex strict JSON Schema，用户只需配置模型，不需要设置 `json_schema:false`。fake CLI 通过真实子进程边界覆盖 Arbiter 风格的 Schema 调用以及 Architect short/long、Writer、Reviewer、Editor 五条 Worker 路径；副作用前 fallback、写入尝试后的 fail-closed 恢复、取消、进程组回收和终态锁定均有回归测试。

发布前验证结果：

- `go vet ./...`
- `go test ./...`
- `go test -race ./internal/codexcli ./internal/agents ./internal/bootstrap ./internal/arbiter ./internal/host ./internal/llmcontract`
- `GOOS=windows GOARCH=amd64 go test -c -o /tmp/ainovel-codexcli-windows.test.exe ./internal/codexcli`（仅验证可编译错误门面；运行时仍明确拒绝非 macOS）
- `AINOVEL_CODEX_PREFLIGHT=1` 的本机无模型请求预检
- 真实模型 smoke：`gpt-5.4` 纯文本、`gpt-5.6-sol` 嵌套 structured output，以及实际 `arbiter_plan_start` 契约均通过；这些测试仍保持显式 opt-in，默认测试不消耗额度
- 根目录与 embedded `config.example.jsonc` 字节一致，`git diff --check` 通过

## 6. 威胁模型

| 威胁 | 严重度 | 强制缓解 |
|---|---:|---|
| 项目配置替换 command 执行任意程序 | Critical | 启动字段只接受全局配置；无 shell；解析/固定可执行路径 |
| 用户 AGENTS/skills/MCP 污染小说角色 | High | ignore user config/rules、空 cwd、临时 MCP 配置、隔离预检失败关闭 |
| Codex 直接修改 Store 或源码 | High | read-only sandbox；不挂载目录；所有写入仅经角色 allowlist MCP |
| MCP 工具越权或终态后重复写 | High | 每任务静态工具集；服务端授权；终态原子锁定并拒绝后续写 |
| 崩溃后 fallback 重复提交 | High | 记录是否发生业务副作用；有副作用时必须回 Engine 重读事实 |
| stdout 日志污染 JSON-RPC/JSONL | Medium | stdout 严格逐行解析；日志仅 stderr；尺寸上限；缺关键事件失败 |
| auth/token 或小说内容进入日志 | High | 不记录环境、auth、完整 prompt/JSONL/stderr；临时文件 0600 并清理 |
| 子进程或 MCP 进程泄漏 | Medium | 进程组 TERM→KILL；关闭 stdin；等待回收；测试孤儿进程 |

## 7. 非目标

- 不支持 Windows、Linux 或 Docker 中的 Codex provider。
- 不继承 Codex profile、skills、AGENTS、rules、hooks 或已有 MCP。
- 不支持 persistent/resume/`--last` session。
- 不把订阅 token 换算成虚构的 API 美元成本。
- 不创建第二套 Engine、Router 或 Store。
