package flow

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
)

func TestLoadStateReturnsProgressReadError(t *testing.T) {
	dir := t.TempDir()
	st := storepkg.NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta", "progress.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(st); err == nil {
		t.Fatal("损坏的 progress 必须阻止路由")
	}
}

// helper：构造一个处于 Writing 阶段、分层模式的 Progress。
func writingProgress(completed []int, flow domain.FlowState) *domain.Progress {
	return &domain.Progress{
		Phase:             domain.PhaseWriting,
		Flow:              flow,
		Layered:           true,
		CompletedChapters: completed,
	}
}

func TestRoute_NilProgress(t *testing.T) {
	if got := Route(State{Progress: nil}); got != nil {
		t.Fatalf("expected nil for nil progress, got %+v", got)
	}
}

func TestRoute_PhaseComplete(t *testing.T) {
	// 完本期返回 nil：作品简介在初始化阶段写入 premise.md，完本后无需再派发任何 agent。
	s := State{Progress: &domain.Progress{Phase: domain.PhaseComplete}, FoundationMissing: []string{"cover_prompt"}}
	if got := Route(s); got != nil {
		t.Fatalf("expected nil at PhaseComplete, got %+v", got)
	}
}

func TestRoute_WritingCoverMigrationPreemptsAllWritingWork(t *testing.T) {
	p := writingProgress([]int{1, 2}, domain.FlowRewriting)
	p.PendingReviewChapter = 2
	p.PendingRewrites = []int{1}
	got := Route(State{
		Progress:          p,
		FoundationMissing: []string{"cover_prompt"},
		PlanningTier:      domain.PlanningTierShort,
	})
	if got == nil || got.Agent != "architect_short" || got.Chapter != 0 {
		t.Fatalf("cover migration must dispatch short architect before reviewer/rewrites, got %+v", got)
	}
	for _, want := range []string{"旧书封面提示词迁移", "save_foundation(type=cover_prompt)", "不得修改", "不在本轮续写正文"} {
		if !strings.Contains(got.Task, want) {
			t.Errorf("migration task missing %q: %s", want, got.Task)
		}
	}
	if !strings.Contains(got.Reason, "暂停续写") {
		t.Fatalf("migration reason must state writing is blocked: %s", got.Reason)
	}
}

func TestRoute_WritingCoverMigrationFallsBackToLongArchitect(t *testing.T) {
	p := writingProgress(nil, domain.FlowWriting)
	got := Route(State{Progress: p, FoundationMissing: []string{"cover_prompt"}})
	if got == nil || got.Agent != "architect_long" {
		t.Fatalf("unknown legacy tier should use long architect, got %+v", got)
	}
}

func TestLoadStateRoutesLegacyWritingBookThroughCoverMigrationThenResumes(t *testing.T) {
	st := storepkg.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("旧书迁移", 2); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []domain.Phase{domain.PhasePremise, domain.PhaseOutline, domain.PhaseWriting} {
		if err := st.Progress.UpdatePhase(phase); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RunMeta.SetPlanningTier(domain.PlanningTierShort); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SavePremise("# 旧书迁移\n\n## 作品简介\n一名少年在废土追寻失落真相。"); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{{Chapter: 1, Title: "废土启程", CoreEvent: "少年走入禁区"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Characters.Save([]domain.Character{{Name: "林夜", Role: "主角"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.World.SaveWorldRules([]domain.WorldRule{{Category: "禁区", Rule: "夜间异变", Boundary: "黎明消退"}}); err != nil {
		t.Fatal(err)
	}

	before, err := LoadState(st)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(before.FoundationMissing, "cover_prompt") {
		t.Fatalf("旧 writing 项目应识别出封面迁移缺项: %v", before.FoundationMissing)
	}
	migration := Route(before)
	if migration == nil || migration.Agent != "architect_short" || !strings.Contains(migration.Task, "cover_prompt") {
		t.Fatalf("应先派短篇规划师执行封面迁移, got %+v", migration)
	}

	set := domain.CoverPromptSet{Prompts: []domain.CoverPrompt{
		{Title: "旧书迁移", GenreTone: "废土悬疑", Subject: "少年背对镜头握住发光罗盘", Background: "被黑雾吞没的废土城市", PrimaryColors: "冷蓝与猩红对比色调", TextPosition: "上方"},
		{Title: "废土禁区我能看见真相", GenreTone: "末日异能", Subject: "少年抬手撕开黑雾裂缝", Background: "怪物盘踞的坍塌高楼", PrimaryColors: "暗黑与幽蓝色调", TextPosition: "正中央"},
		{Title: "末日开局一枚逆命罗盘", GenreTone: "末日逆袭", Subject: "少年托起燃烧的古老罗盘", Background: "红月笼罩的避难所", PrimaryColors: "红黑与灼金色调", TextPosition: "下方"},
		{Title: "禁区降临我杀穿黑夜", GenreTone: "暗黑杀伐", Subject: "少年持刀迎向成群异兽", Background: "碎石飞舞的禁区战场", PrimaryColors: "紫黑与血红色调", TextPosition: "上方"},
		{Title: "全民逃亡我独闯禁区", GenreTone: "全民末日", Subject: "少年站在巨兽头骨之上", Background: "雷暴撕裂的荒原禁区", PrimaryColors: "暗金与深灰色调", TextPosition: "正中央"},
	}}
	if err := st.Outline.SaveCoverPromptSet(set); err != nil {
		t.Fatal(err)
	}

	after, err := LoadState(st)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(after.FoundationMissing, "cover_prompt") {
		t.Fatalf("迁移落盘后 cover_prompt 缺项应消失: %v", after.FoundationMissing)
	}
	resumed := Route(after)
	if resumed == nil || resumed.Agent != "writer" || resumed.Chapter != 1 {
		t.Fatalf("迁移完成后应恢复原正文路由, got %+v", resumed)
	}
}

func TestRoute_NonWritingPhasesDelegateToLLM(t *testing.T) {
	for _, phase := range []domain.Phase{domain.PhaseInit, domain.PhasePremise, domain.PhaseOutline} {
		s := State{Progress: &domain.Progress{Phase: phase}, FoundationMissing: []string{"premise"}}
		if got := Route(s); got != nil {
			t.Fatalf("phase %s should return nil, got %+v", phase, got)
		}
	}
}

func TestRoute_PendingRewritesFirst(t *testing.T) {
	p := writingProgress([]int{1, 2}, domain.FlowRewriting)
	p.PendingRewrites = []int{3, 5}
	got := Route(State{Progress: p})
	if got == nil || got.Agent != "writer" {
		t.Fatalf("expected writer for rewrites, got %+v", got)
	}
	if got.Task != "重写第 3 章" {
		t.Errorf("expected '重写第 3 章', got %q", got.Task)
	}
	if got.Chapter != 3 {
		t.Errorf("expected Chapter=3, got %d", got.Chapter)
	}
}

func TestRoute_ReviewerRunsBeforeRewriteAndEditor(t *testing.T) {
	p := writingProgress([]int{1, 2}, domain.FlowRewriting)
	p.PendingReviewChapter = 2
	p.PendingRewrites = []int{1}
	got := Route(State{
		Progress: p, LastCompleted: 2,
		ArcBoundary: &storepkg.ArcBoundary{IsArcEnd: true, Volume: 1, Arc: 1},
	})
	if got == nil || got.Agent != "reviewer" || got.Chapter != 2 {
		t.Fatalf("Reviewer 应优先于返工队列和弧末 Editor，got %+v", got)
	}
	for _, want := range []string{"Humanizer", "情绪优化", "finalize_reviewed_chapter"} {
		if !strings.Contains(got.Task, want) {
			t.Errorf("Reviewer 任务缺少 %q: %s", want, got.Task)
		}
	}
}

func TestRoute_PendingPolishingVerb(t *testing.T) {
	p := writingProgress([]int{1}, domain.FlowPolishing)
	p.PendingRewrites = []int{2}
	got := Route(State{Progress: p})
	if got == nil || got.Task != "打磨第 2 章" {
		t.Fatalf("expected polish verb, got %+v", got)
	}
}

func TestRoute_ReviewingDelegatesToLLM(t *testing.T) {
	p := writingProgress([]int{1, 2}, domain.FlowReviewing)
	if got := Route(State{Progress: p}); got != nil {
		t.Fatalf("expected nil during reviewing, got %+v", got)
	}
}

func TestRoute_SteeringDelegatesToLLM(t *testing.T) {
	p := writingProgress([]int{1}, domain.FlowSteering)
	if got := Route(State{Progress: p}); got != nil {
		t.Fatalf("expected nil during steering, got %+v", got)
	}
}

func TestRoute_ArcEndNeedsReview(t *testing.T) {
	p := writingProgress([]int{10}, domain.FlowWriting)
	s := State{
		Progress:      p,
		LastCompleted: 10,
		ArcBoundary: &storepkg.ArcBoundary{
			IsArcEnd:     true,
			Volume:       1,
			Arc:          2,
			StartChapter: 11,
			EndChapter:   22,
		},
	}
	got := Route(s)
	if got == nil || got.Agent != "editor" {
		t.Fatalf("expected editor for arc review, got %+v", got)
	}
	if got.Reason != "弧末评审未完成" {
		t.Errorf("reason mismatch: %q", got.Reason)
	}
	if !strings.Contains(got.Task, "第 11-22 章") || !strings.Contains(got.Task, "chapter=22") {
		t.Fatalf("arc review task must carry exact span and endpoint: %q", got.Task)
	}
}

func TestRoute_ArcEndHasReviewNeedsSummary(t *testing.T) {
	p := writingProgress([]int{10}, domain.FlowWriting)
	s := State{
		Progress:      p,
		LastCompleted: 10,
		ArcBoundary: &storepkg.ArcBoundary{
			IsArcEnd: true,
			Volume:   1,
			Arc:      2,
		},
		HasArcReview: true,
	}
	got := Route(s)
	if got == nil || got.Agent != "editor" || got.Reason != "弧摘要未完成" {
		t.Fatalf("expected arc summary editor call, got %+v", got)
	}
}

func TestRoute_VolumeEndNeedsVolumeSummary(t *testing.T) {
	p := writingProgress([]int{20}, domain.FlowWriting)
	s := State{
		Progress:      p,
		LastCompleted: 20,
		ArcBoundary: &storepkg.ArcBoundary{
			IsArcEnd:    true,
			IsVolumeEnd: true,
			Volume:      1,
			Arc:         3,
		},
		HasArcReview:  true,
		HasArcSummary: true,
	}
	got := Route(s)
	if got == nil || got.Reason != "卷摘要未完成" {
		t.Fatalf("expected volume summary request, got %+v", got)
	}
}

func TestRoute_NeedsArcExpansion(t *testing.T) {
	p := writingProgress([]int{10}, domain.FlowWriting)
	s := State{
		Progress:      p,
		LastCompleted: 10,
		ArcBoundary: &storepkg.ArcBoundary{
			IsArcEnd:       true,
			Volume:         1,
			Arc:            2,
			NextVolume:     1,
			NextArc:        3,
			NeedsExpansion: true,
		},
		HasArcReview:  true,
		HasArcSummary: true,
	}
	got := Route(s)
	if got == nil || got.Agent != "architect_long" {
		t.Fatalf("expected architect_long for expansion, got %+v", got)
	}
	if got.Reason != "下一弧骨架待展开" {
		t.Errorf("reason mismatch: %q", got.Reason)
	}
}

func TestRoute_NeedsNewVolume(t *testing.T) {
	p := writingProgress([]int{30}, domain.FlowWriting)
	s := State{
		Progress:      p,
		LastCompleted: 30,
		ArcBoundary: &storepkg.ArcBoundary{
			IsArcEnd:       true,
			IsVolumeEnd:    true,
			Volume:         2,
			Arc:            4,
			NeedsNewVolume: true,
		},
		HasArcReview:     true,
		HasArcSummary:    true,
		HasVolumeSummary: true,
	}
	got := Route(s)
	if got == nil || got.Agent != "architect_long" || got.Reason != "卷末需决定追加新卷、收官卷或结束全书" {
		t.Fatalf("expected append_volume/complete_book dispatch, got %+v", got)
	}
}

func TestRoute_NormalContinue(t *testing.T) {
	p := writingProgress([]int{1, 2, 3}, domain.FlowWriting)
	p.TotalChapters = 20
	got := Route(State{Progress: p, LastCompleted: 3})
	if got == nil || got.Agent != "writer" {
		t.Fatalf("expected writer for next chapter, got %+v", got)
	}
	if got.Task != "写第 4 章" {
		t.Errorf("expected '写第 4 章', got %q", got.Task)
	}
	if got.Chapter != 4 {
		t.Errorf("expected Chapter=4, got %d", got.Chapter)
	}
}

func TestRoute_NonLayeredOutlineExhaustedDispatchesArchitect(t *testing.T) {
	p := &domain.Progress{
		Phase:             domain.PhaseWriting,
		Flow:              domain.FlowWriting,
		CompletedChapters: []int{1, 2, 3},
		TotalChapters:     3,
	}
	got := Route(State{Progress: p, LastCompleted: 3, PlanningTier: domain.PlanningTierShort})
	if got == nil || got.Agent != "architect_short" {
		t.Fatalf("expected architect_short at outline exhaustion, got %+v", got)
	}
	for _, want := range []string{"complete_book", "revise_outline", "第 4 章"} {
		if !strings.Contains(got.Task, want) {
			t.Errorf("task missing %q: %s", want, got.Task)
		}
	}
}

func TestRoute_ArcEndNonLayeredSkipsBoundary(t *testing.T) {
	// 非 Layered 模式即使 ArcBoundary 非 nil 也不走弧末分支
	p := &domain.Progress{
		Phase:             domain.PhaseWriting,
		Flow:              domain.FlowWriting,
		Layered:           false,
		CompletedChapters: []int{10},
		TotalChapters:     20,
	}
	s := State{
		Progress:      p,
		LastCompleted: 10,
		ArcBoundary:   &storepkg.ArcBoundary{IsArcEnd: true, Volume: 1, Arc: 2},
	}
	got := Route(s)
	if got == nil || got.Agent != "writer" {
		t.Fatalf("non-layered should fall through to writer, got %+v", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// 规划期补齐:设定缺项 + 规划师可判定 → 照缺项续派同一规划师。
func TestRoute_PlanningFillDispatchesSamePlanner(t *testing.T) {
	base := State{
		Progress:          &domain.Progress{Phase: domain.PhaseOutline},
		FoundationMissing: []string{"characters", "world_rules"},
	}

	short := base
	short.PlanningTier = domain.PlanningTierShort
	if got := Route(short); got == nil || got.Agent != "architect_short" {
		t.Fatalf("short tier 应续派 architect_short,got %+v", got)
	}

	long := base
	long.PlanningTier = domain.PlanningTierLong
	got := Route(long)
	if got == nil || got.Agent != "architect_long" {
		t.Fatalf("long tier 应续派 architect_long,got %+v", got)
	}
	for _, want := range []string{"补齐基础设定", "characters", "world_rules", "save_foundation"} {
		if !contains(got.Task, want) {
			t.Errorf("补齐任务缺少 %q: %s", want, got.Task)
		}
	}

	// 首次规划未落盘任何设定(tier 空)→ 选型是语义判断,交 LLM
	unknown := base
	if got := Route(unknown); got != nil {
		t.Fatalf("tier 未知时应交 LLM 裁定,got %+v", got)
	}

	// 缺项已齐 → 无补齐指令(等 phase 推进)
	done := base
	done.PlanningTier = domain.PlanningTierLong
	done.FoundationMissing = nil
	if got := Route(done); got != nil {
		t.Fatalf("缺项已齐时不应派补齐,got %+v", got)
	}
}
