package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/llmcontract"
	"github.com/voocel/ainovel-cli/internal/store"
)

func setupReviewerChapter(t *testing.T, total int, content string) (*store.Store, *FinalizeReviewedChapterTool) {
	t.Helper()
	st := store.NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("test", total); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.UpdatePhase(domain.PhaseWriting); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveFinalChapter(1, content); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.MarkChapterComplete(1, len([]rune(content)), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.RequireChapterReview(1); err != nil {
		t.Fatal(err)
	}
	return st, NewFinalizeReviewedChapterTool(st, NewStyleStatsIndex(st))
}

func reviewerArgs(source string, patterns []string, humanized, final string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"chapter": 1, "source_digest": chapterContentDigest(source),
		"ai_patterns": patterns, "humanized_content": humanized, "content": final,
	})
	return raw
}

func TestFinalizeReviewedChapterSchemaStrictReady(t *testing.T) {
	tool := NewFinalizeReviewedChapterTool(store.NewStore(t.TempDir()), nil)
	if !tool.StrictSchema() {
		t.Fatal("reviewer terminal tool must use strict schema")
	}
	if err := llmcontract.ValidateStrictReady(tool.Schema()); err != nil {
		t.Fatalf("schema is not strict-ready: %v", err)
	}
}

func TestFinalizeReviewedChapterNoAIDirectlyOptimizesDialogue(t *testing.T) {
	source := "# 第一章\n\n他看着门口。“我不高兴。”\n\n雨还在下。"
	final := "# 第一章\n\n他看着门口。“我烦透了！”\n\n雨还在下。"
	st, tool := setupReviewerChapter(t, 5, source)
	raw, err := tool.Execute(context.Background(), reviewerArgs(source, nil, "", final))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out["humanizer_changed"] != false || out["emotion_changes"] != float64(1) {
		t.Fatalf("unexpected result: %v", out)
	}
	got, _ := st.Drafts.LoadChapterText(1)
	if got != final {
		t.Fatalf("final chapter mismatch:\n%s", got)
	}
	p, _ := st.Progress.Load()
	if p.PendingReviewChapter != 0 || p.ChapterWordCounts[1] != len([]rune(final)) {
		t.Fatalf("progress not finalized: %+v", p)
	}
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "chapter_review"); cp == nil {
		t.Fatal("missing chapter_review checkpoint")
	}
}

func TestFinalizeReviewedChapterHumanizerThenEmotion(t *testing.T) {
	source := "# 第一章\n\n他不禁露出一抹意味深长的笑。“我没有生气。”"
	humanized := "# 第一章\n\n他笑了。“我没有生气。”"
	final := "# 第一章\n\n他笑了。“我气得很。”"
	st, tool := setupReviewerChapter(t, 5, source)
	raw, err := tool.Execute(context.Background(), reviewerArgs(
		source, []string{"量词癖：一抹；虚词癖：不禁；抽象修饰：意味深长"}, humanized, final,
	))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out["humanizer_changed"] != true || out["emotion_changes"] != float64(1) {
		t.Fatalf("unexpected result: %v", out)
	}
	got, _ := st.Drafts.LoadChapterText(1)
	if got != final {
		t.Fatalf("final chapter mismatch: %q", got)
	}
}

func TestFinalizeReviewedChapterRejectsBrokenStageContracts(t *testing.T) {
	source := "# 第一章\n\n他站着。“我不高兴。”\n\n门关了。"
	tests := []struct {
		name      string
		patterns  []string
		humanized string
		final     string
		want      string
	}{
		{"无 AI 却提交 Humanizer 改写", nil, source, source, "必须跳过 Humanizer"},
		{"有 AI 却没有 Humanizer 结果", []string{"套句"}, "", source, "必须提供实际改写"},
		{"情绪阶段改了叙述", nil, "", "# 第一章\n\n他怒站着。“我不高兴。”\n\n门关了。", "只能修改成对引号内台词"},
		{"情绪阶段扩写超过五点", nil, "", "# 第一章\n\n他站着。“我真的真的真的真的非常非常不高兴。”\n\n门关了。", "超过原稿 5%"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, tool := setupReviewerChapter(t, 5, source)
			_, err := tool.Execute(context.Background(), reviewerArgs(source, tc.patterns, tc.humanized, tc.final))
			if err == nil || !strings.Contains(err.Error(), tc.want) || !errors.Is(err, errs.ErrToolArgs) {
				t.Fatalf("err=%v, want tool args containing %q", err, tc.want)
			}
		})
	}
}

func TestFinalizeReviewedChapterRejectsStaleDigest(t *testing.T) {
	source := "# 第一章\n\n“别走。”"
	_, tool := setupReviewerChapter(t, 5, source)
	args := reviewerArgs(source, nil, "", source)
	var payload map[string]any
	_ = json.Unmarshal(args, &payload)
	payload["source_digest"] = "sha256:stale"
	raw, _ := json.Marshal(payload)
	if _, err := tool.Execute(context.Background(), raw); err == nil || !errors.Is(err, errs.ErrToolConflict) {
		t.Fatalf("expected stale digest conflict, got %v", err)
	}
}

func TestFinalizeReviewedChapterRecoversCheckpointBeforeProgress(t *testing.T) {
	source := "# 第一章\n\n“到此为止。”"
	st, tool := setupReviewerChapter(t, 1, source)
	if _, err := st.Checkpoints.AppendArtifact(domain.ChapterScope(1), "chapter_review", "chapters/01.md"); err != nil {
		t.Fatal(err)
	}
	raw, err := tool.Execute(context.Background(), reviewerArgs(source, nil, "", source))
	if err != nil {
		t.Fatalf("Execute recovery: %v", err)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out["recovered"] != true || out["book_complete"] != true {
		t.Fatalf("unexpected recovery: %v", out)
	}
	p, _ := st.Progress.Load()
	if p.PendingReviewChapter != 0 || p.Phase != domain.PhaseComplete {
		t.Fatalf("recovery progress: %+v", p)
	}
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "chapter_review_recovered"); cp == nil {
		t.Fatal("missing recovery checkpoint")
	}
}

func TestFinalizeReviewedChapterRecoversFinalWriteBeforeCheckpoint(t *testing.T) {
	source := "# 第一章\n\n“我没事。”"
	final := "# 第一章\n\n“少管我。”"
	st, tool := setupReviewerChapter(t, 2, source)
	// 模拟 finalize 已写 intent 和终稿，但在追加正式 chapter_review 前崩溃。
	if _, err := st.Checkpoints.Append(
		domain.ChapterScope(1), "chapter_review_intent", "chapters/01.md", chapterContentDigest(final),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.Drafts.SaveFinalChapter(1, final); err != nil {
		t.Fatal(err)
	}

	raw, err := tool.Execute(context.Background(), reviewerArgs(final, nil, "", final))
	if err != nil {
		t.Fatalf("Execute recovery: %v", err)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out["recovered"] != true {
		t.Fatalf("unexpected recovery: %v", out)
	}
	if cp := st.Checkpoints.LatestByStep(domain.ChapterScope(1), "chapter_review"); cp == nil || cp.Digest != chapterContentDigest(final) {
		t.Fatalf("formal checkpoint was not recovered: %+v", cp)
	}
	p, _ := st.Progress.Load()
	if p.PendingReviewChapter != 0 {
		t.Fatalf("reviewer progress not recovered: %+v", p)
	}
}

func TestDialogueStructureSupportsChineseAndASCIIQuotes(t *testing.T) {
	base := "他说：“别走。” 她答：\"偏走。\""
	final := "他说：“站住！” 她答：\"就走。\""
	changed, err := validateEmotionPass(base, final)
	if err != nil || changed != 2 {
		t.Fatalf("changed=%d err=%v", changed, err)
	}
	if _, err := validateEmotionPass(base, fmt.Sprintf("旁白变了。%s", final)); err == nil {
		t.Fatal("outside-dialogue mutation must fail")
	}
}
