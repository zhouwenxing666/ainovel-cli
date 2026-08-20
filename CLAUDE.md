# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
go build ./cmd/ainovel-cli    # Build the CLI binary
go vet ./...                   # Static analysis (GOWORK=off in CI)
go test -count=1 ./...         # Run all tests (count=1 disables cache)
go test -count=1 ./internal/flow  # Run a single package's tests
go test -count=1 -run TestRoute ./internal/flow  # Run a specific test
go test -race -count=1 ./internal/host ./internal/store ./internal/tools  # Race detection
gofmt -l .                     # Format check (must return empty)
```

**Important**: CI runs with `GOWORK=off` — the `go.work` file (referencing `../agentcore` and `../litellm`) is only for local development. When running tests, pass `GOWORK=off` if sibling modules are unavailable.

## Architecture Overview

**ainovel-cli** is a Go-based AI novel-writing engine — given a one-line prompt, it autonomously writes a complete novel (200–500+ chapters) using a deterministic Engine + multiple LLM agents.

### Core Principle: "事实层确定，语义层自主" (Deterministic facts, autonomous semantics)

The system separates decisions by nature:
- **Enumerated state transitions → code** (Engine loop + Route decision table)
- **Bounded semantic judgments → LLM functions** (Arbiter — structured input/output, auditable)
- **Open-ended creative work → LLM loops** (Workers — Architect, Writer, Editor)

### Layer Diagram

```
Entry (TUI / headless)
  → Host (lifecycle, intervention, observer)
    → Engine (deterministic loop: LoadState → Route → run Worker → check boundary)
      → Workers (Architect/Writer/Editor — each independent subagent.Runner)
        → Tools (11 file-system tools, atomic writes + idempotent replay)
          → Store (flat file system: Progress, Checkpoints, Artifacts)
```

### Key Packages

| Package | Role |
|---|---|
| `internal/domain/` | Pure data types: Phase, FlowState, Progress, Checkpoint, Story, Scope |
| `internal/store/` | File-system persistence (tmp+rename atomic writes, commit uses Saga pattern) |
| `internal/tools/` | 11 agent tools: novel_context, read/plan/draft/edit/commit_chapter, check_consistency, save_review, save_arc/volume_summary, save_foundation |
| `internal/flow/` | Route decision table — pure function, 120k-combination exhaustive test |
| `internal/arbiter/` | Semantic arbitration: plan_start, intervention, worker_failure, deadlock |
| `internal/agents/` | Worker assembly (BuildWorkers), context packing, CheckpointDeltaGuard |
| `internal/host/` | Host lifecycle, Engine loop, observer/events, budget, advance gate |
| `internal/host/imp/` | Novel import pipeline (ingest → segment → analyze → synthesize → publish) |
| `internal/host/exp/` | Export to TXT/EPUB 3 |
| `internal/entry/tui/` | Bubble Tea TUI — panels, command palette, status bar |
| `internal/entry/headless/` | Non-interactive mode |
| `internal/diag/` | Read-only diagnostics — quality rules, runtime inspection, redacted export |
| `internal/eval/` | Offline evaluation harness |
| `internal/llmcontract/` | LLM response contract validation (JSON Schema enforcement) |
| `internal/userrules/` | User-authored rules runtime (snapshot per book) |
| `internal/stylestat/` | Book-level style statistics (sentence patterns, repeated phrases, chapter-end patterns) |

### Three Workers

1. **Architect** (short/long) — premise, outline, character profiles, world rules, arc expansion. Tools: `novel_context`, `save_foundation`
2. **Writer** — one chapter: context → read previous → plan → draft → check consistency → commit. Fixed tool order enforced by code guards.
3. **Editor** — arc/volume review from 7 dimensions. Tools: `novel_context`, `read_chapter`, `save_review`, `save_arc_summary`, `save_volume_summary`

### Data Model

- **Progress** — single JSON file tracking phase, chapter counts, flow state, pending rewrites
- **Checkpoints** — `meta/checkpoints.jsonl`, append-only, step-level (plan/draft/commit/review/...)
- **Artifacts** — flat files under `chapters/`, `drafts/`, `reviews/`, `summaries/`, `foundation/`
- **RunMeta** — `meta/run.json` — user's run intent (planning tier, advance mode, pending steer)
- **Decisions** — `meta/decisions.jsonl` — every Arbiter ruling, replayable offline
- **Signals** — `PendingCommit` for crash recovery of chapter commit saga

### Key Design Rules

1. **Route is a pure function** — no IO, no Store access. `flow.LoadState` collects facts, `flow.Route(state)` produces `*Instruction`.
2. **Tools return facts, not instructions** — structured JSON fields, never system-level directives.
3. **Tools are idempotent** — check checkpoint before writing; same `Step+Digest` → skip.
4. **Engine is single-goroutine serial** — `ctx` cancel = pause (crash-safe via checkpoints).
5. **No auto-resume** — Engine loop ends = host terminal state. Only user `Continue` or restart `Resume` restarts it.
6. **Observer (diag) is read-only** — diagnoses but never auto-fixes, never modifies control flow.
7. **Control surface changes must modify exhaustive specs first** — `flow/router_exhaustive_test.go` before implementation.

### Testing Strategy

| Layer | What | File Pattern |
|---|---|---|
| Control surface spec | 120k-combination exhaustive route tests | `flow/router_exhaustive_test.go` |
| Framework contract | 5 agentcore behavior assumptions | `agents/agentcore_contract_test.go` |
| Engine E2E | Fake model + real tools: complete book, failure/deadlock arbitration | `host/engine_test.go` |
| Arbitration | Parse, retry, validation matrix, fact collection | `arbiter/arbiter_test.go` |
| Store/Tools | Cross-restart feedback pool, rule violations, PlanStart retention | Per-package test files |
| Voice layer | Byte-level consistency, 3-layer override semantics | `assets/load_test.go` |

### go.work Dependencies

The project uses `go.work` to reference two sibling modules:
- `../agentcore` — generic agent framework (Runner, StopGuard, ContextManager)
- `../litellm` — LLM gateway (provider failover, streaming, prompt caching)

New capabilities go into agentcore/litellm only if they are not business-specific. Business models and business tools never go into these shared libraries.

### Assets Structure

```
assets/
  voice.md          — Built-in writing standards ({{VOICE}} placeholder, user-overridable)
  references/       — Writing techniques, anti-AI-tone, genre templates
  styles/           — Default/fantasy/romance/mystery style presets
  prompts/          — Per-agent prompt templates: arbiter-*, architect-*, writer, editor, import-*
  testdata/         — Test fixtures
```

### Configuration

User config is a JSONC file (default: `ainovel.jsonc`). The `config.example.jsonc` documents all fields:
- `provider` / `model` — primary LLM provider + model
- `providers` — provider credential map (OpenRouter, Anthropic, Gemini, OpenAI, etc.)
- Role-level model overrides: `architect_model`, `writer_model`, `editor_model`
- `reasoning_effort` — default inference effort level
- `novel_dir` — output directory for novels