package codexcli_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/arbiter"
	"github.com/voocel/ainovel-cli/internal/codexcli"
)

// TestRealArbiterPlanStartOptIn verifies the exact structured contract that
// starts an ainovel run. It is skipped by default because it consumes saved-
// login Codex quota.
func TestRealArbiterPlanStartOptIn(t *testing.T) {
	if os.Getenv("AINOVEL_CODEX_ARBITER_SMOKE") != "1" {
		t.Skip("set AINOVEL_CODEX_ARBITER_SMOKE=1 to run the real Codex Arbiter smoke test")
	}
	modelName := strings.TrimSpace(os.Getenv("AINOVEL_CODEX_SMOKE_MODEL"))
	if modelName == "" {
		t.Fatal("AINOVEL_CODEX_SMOKE_MODEL is required when the real Arbiter smoke test is enabled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	model := codexcli.NewCompletionModel(
		codexcli.NewRuntime(codexcli.RuntimeConfig{Command: "codex"}),
		codexcli.CompletionModelConfig{Provider: "local-codex", Model: modelName, Timeout: 3 * time.Minute},
	)
	decision, err := arbiter.DecidePlanStart(ctx, model,
		"You are the ainovel Arbiter. Return a valid plan-start decision.",
		"写一个关于旧书店守夜人与会说话的黑猫互相救赎的短篇故事。", "default")
	if err != nil {
		t.Fatal(err)
	}
	if decision.Planner != "architect_short" && decision.Planner != "architect_long" {
		t.Fatalf("unexpected Arbiter decision: %+v", decision)
	}
}
