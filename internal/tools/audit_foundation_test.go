package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/llmcontract"
	"github.com/voocel/ainovel-cli/internal/store"
)

func completeShortFoundation(t *testing.T) *store.Store {
	t.Helper()
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	if err := s.Progress.Init("审查测试", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Outline.SavePremise("# 审查测试\n\n## 主角目标\n林舟求生"); err != nil {
		t.Fatal(err)
	}
	if err := s.Outline.SaveOutline([]domain.OutlineEntry{{Chapter: 1, Title: "求生", CoreEvent: "林舟脱险"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Characters.Save([]domain.Character{{Name: "林舟", Role: "主角", Description: "求生者"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.World.SaveWorldRules([]domain.WorldRule{{Category: "society", Rule: "城门夜禁", Boundary: "入夜关闭"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Outline.SaveCoverPromptSet(coverPromptSetForTest("审查测试")); err != nil {
		t.Fatal(err)
	}
	return s
}

func coverPromptSetForTest(title string) domain.CoverPromptSet {
	return domain.CoverPromptSet{Prompts: []domain.CoverPrompt{
		{Title: title, GenreTone: "都市悬疑", Subject: "青年侦探站在雨中", Background: "霓虹闪烁的旧城街巷", PrimaryColors: "冷蓝与猩红对比色调", TextPosition: "上方"},
		{Title: "开局追凶震惊全城", GenreTone: "都市追凶", Subject: "青年侦探举起染血证物", Background: "警灯交错的封锁现场", PrimaryColors: "深黑与警灯蓝红色调", TextPosition: "正中央"},
		{Title: "我能看见罪恶真相", GenreTone: "异能悬疑", Subject: "青年眼中浮现发光线索", Background: "证据碎片悬浮的审讯室", PrimaryColors: "暗金与墨黑色调", TextPosition: "下方"},
		{Title: "全城通缉我破局", GenreTone: "高压逃亡", Subject: "青年回身冲破包围", Background: "直升机盘旋的城市天台", PrimaryColors: "炽白与血红色调", TextPosition: "上方"},
		{Title: "深夜档案局", GenreTone: "诡秘探案", Subject: "青年推开布满符咒的铁门", Background: "无尽档案柜延伸进黑雾", PrimaryColors: "幽绿与深紫色调", TextPosition: "正中央"},
	}}
}

func TestAuditFoundationControlsWritingTransition(t *testing.T) {
	s := completeShortFoundation(t)
	tool := NewAuditFoundationTool(s)
	if !tool.StrictSchema() {
		t.Fatal("audit_foundation must use strict schema")
	}
	if err := llmcontract.ValidateStrictReady(tool.Schema()); err != nil {
		t.Fatalf("audit_foundation schema is not strict-ready: %v", err)
	}
	missing, err := s.FoundationMissing()
	if err != nil || len(missing) != 1 || missing[0] != "foundation_audit" {
		t.Fatalf("expected only foundation_audit, got %v, err=%v", missing, err)
	}
	fingerprint, err := s.FoundationFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	failed, _ := json.Marshal(map[string]any{
		"fingerprint": fingerprint,
		"ready":       false,
		"summary":     "角色名不一致",
		"issues": []map[string]any{{
			"artifact": "characters", "description": "人物不一致", "evidence": "前提为林舟，角色表为他人", "suggestion": "统一角色",
		}},
	})
	if _, err := tool.Execute(context.Background(), failed); err != nil {
		t.Fatalf("failed audit should persist guidance: %v", err)
	}
	if p, _ := s.Progress.Load(); p.Phase == domain.PhaseWriting {
		t.Fatal("failed audit must not enter writing")
	}

	passed, _ := json.Marshal(map[string]any{
		"fingerprint": fingerprint,
		"ready":       true,
		"summary":     "基础设定一致",
		"issues":      []any{},
	})
	if _, err := tool.Execute(context.Background(), passed); err != nil {
		t.Fatalf("passed audit: %v", err)
	}
	if p, _ := s.Progress.Load(); p.Phase != domain.PhaseWriting {
		t.Fatalf("passed audit must enter writing, got %s", p.Phase)
	}
}

func TestSaveFoundationWaitsForSemanticAudit(t *testing.T) {
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	if err := s.Progress.Init("test", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Outline.SavePremise("# test"); err != nil {
		t.Fatal(err)
	}
	if err := s.Characters.Save([]domain.Character{{Name: "林舟", Role: "主角"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.World.SaveWorldRules([]domain.WorldRule{{Category: "society", Rule: "夜禁", Boundary: "入夜"}}); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{
		"type": "outline", "scale": "short",
		"content": []map[string]any{{"chapter": 1, "title": "开端", "core_event": "林舟入城", "hook": "夜禁", "scenes": []string{"入城"}}},
	})
	result, err := NewSaveFoundationTool(s).Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Ready     bool     `json:"foundation_ready"`
		Remaining []string `json:"remaining"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Ready || len(payload.Remaining) != 1 || payload.Remaining[0] != "cover_prompt" {
		t.Fatalf("outline complete should still wait for cover_prompt: %+v", payload)
	}
	coverArgs, _ := json.Marshal(map[string]any{"type": "cover_prompt", "content": coverPromptSetForTest("test")})
	result, err = NewSaveFoundationTool(s).Execute(context.Background(), coverArgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Ready || len(payload.Remaining) != 1 || payload.Remaining[0] != "foundation_audit" {
		t.Fatalf("cover_prompt complete must wait for audit: %+v", payload)
	}
	if p, _ := s.Progress.Load(); p.Phase == domain.PhaseWriting {
		t.Fatal("save_foundation must not enter writing before audit")
	}
}

func TestAuditFoundationRejectsStaleFingerprint(t *testing.T) {
	s := completeShortFoundation(t)
	fingerprint, err := s.FoundationFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	premise, err := s.Outline.LoadPremise()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Outline.SavePremise(strings.Replace(premise, "## 主角目标", "## 核心冲突", 1)); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{
		"fingerprint": fingerprint, "ready": true, "summary": "通过", "issues": []any{},
	})
	if _, err := NewAuditFoundationTool(s).Execute(context.Background(), args); err == nil || !strings.Contains(err.Error(), "重新调用 novel_context") {
		t.Fatalf("expected stale fingerprint rejection, got %v", err)
	}
}
