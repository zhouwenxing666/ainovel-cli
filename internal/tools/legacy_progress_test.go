package tools

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"unicode/utf8"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

func TestMigrateLegacyChapterProgress(t *testing.T) {
	for _, tc := range []struct {
		name         string
		total        int
		layered      bool
		reopened     bool
		final        bool
		wrapped      bool
		openThread   bool
		queue        []int
		withoutMark  bool
		phase        domain.Phase
		pendingStage domain.CommitStage
		wantComplete bool
	}{
		{name: "short final chapter", total: 2, wantComplete: true},
		{name: "continue next chapter", total: 3},
		{name: "remaining rewrites", total: 2, queue: []int{1}},
		{name: "reopened layered book", layered: true, reopened: true, openThread: true, wantComplete: true},
		{name: "layered open thread", layered: true, openThread: true},
		{name: "layered complete", layered: true, wantComplete: true},
		{name: "finale still needs editor", layered: true, final: true, openThread: true},
		{name: "finale editor finished", layered: true, final: true, wrapped: true, openThread: true, wantComplete: true},
		{name: "reopened continuation has no old marker", total: 2, withoutMark: true},
		{name: "late commit before checkpoint", total: 2, pendingStage: domain.CommitStageProgressMarked, wantComplete: true},
		{name: "late commit after checkpoint", total: 2, pendingStage: domain.CommitStageSignalSaved, wantComplete: true},
		{name: "restart after migration saved completion", total: 2, withoutMark: true, phase: domain.PhaseComplete, pendingStage: domain.CommitStageProgressMarked, wantComplete: true},
		{name: "early commit must finish state first", total: 2, pendingStage: domain.CommitStageStateApplied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := store.NewStore(t.TempDir())
			if err := st.Init(); err != nil {
				t.Fatal(err)
			}
			const content = "# 第二章\n\n她把钥匙放回桌上。\n「回家吧。」\n"
			if err := st.Drafts.SaveFinalChapter(2, content); err != nil {
				t.Fatal(err)
			}
			if err := st.Summaries.SaveSummary(domain.ChapterSummary{Chapter: 2, Title: "第二章", Summary: "归家"}); err != nil {
				t.Fatal(err)
			}
			p := domain.Progress{
				Phase: domain.PhaseWriting, Flow: domain.FlowWriting, TotalChapters: tc.total,
				CurrentChapter: 3, CompletedChapters: []int{1, 2}, TotalWordCount: 12,
				ChapterWordCounts: map[int]int{1: 7, 2: 5}, PendingRewrites: tc.queue,
				Layered: tc.layered, ReopenedFromComplete: tc.reopened,
			}
			if tc.phase != "" {
				p.Phase = tc.phase
			}
			if tc.layered {
				if err := st.Outline.SaveLayeredOutline([]domain.VolumeOutline{{
					Index: 1, Title: "终卷", Final: tc.final,
					Arcs: []domain.ArcOutline{{Index: 1, Title: "归家", Chapters: []domain.OutlineEntry{
						{Chapter: 1, Title: "第一章"}, {Chapter: 2, Title: "第二章"},
					}}},
				}}); err != nil {
					t.Fatal(err)
				}
				compass := domain.StoryCompass{EndingDirection: "归家"}
				if tc.openThread {
					compass.OpenThreads = []string{"旧事未了"}
				}
				if err := st.Outline.SaveCompass(compass); err != nil {
					t.Fatal(err)
				}
				if tc.wrapped {
					if err := st.World.SaveReview(domain.ReviewEntry{Chapter: 2, Scope: "arc", Verdict: "accept"}); err != nil {
						t.Fatal(err)
					}
					if err := st.Summaries.SaveArcSummary(domain.ArcSummary{Volume: 1, Arc: 1, Summary: "归家"}); err != nil {
						t.Fatal(err)
					}
					if err := st.Summaries.SaveVolumeSummary(domain.VolumeSummary{Volume: 1, Summary: "归家"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			legacy := struct {
				domain.Progress
				PendingReviewChapter int `json:"pending_review_chapter,omitempty"`
			}{Progress: p}
			if !tc.withoutMark {
				legacy.PendingReviewChapter = 2
			}
			before, err := json.Marshal(legacy)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(st.Dir(), "meta", "progress.json")
			if err := os.WriteFile(path, before, 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.pendingStage != "" {
				// 晚期恢复只有终稿/摘要，没有 draft 或 payload；旧响应仍写着未完本。
				if err := st.Signals.SavePendingCommit(domain.PendingCommit{
					Chapter: 2, Stage: tc.pendingStage, Output: json.RawMessage(`{"book_complete":false}`),
				}); err != nil {
					t.Fatal(err)
				}
				if tc.pendingStage == domain.CommitStageSignalSaved {
					if err := newTestCommitChapterTool(st).appendCommitCheckpoint(2); err != nil {
						t.Fatal(err)
					}
				}
			}
			stats := NewStyleStatsIndex(st)
			if err := MigrateLegacyChapterProgress(st, stats); err != nil {
				t.Fatal(err)
			}
			got, err := st.Progress.Load()
			if err != nil {
				t.Fatal(err)
			}
			if (got.Phase == domain.PhaseComplete) != tc.wantComplete {
				t.Fatalf("phase = %s, want complete=%v", got.Phase, tc.wantComplete)
			}
			if tc.wantComplete && got.ReopenedFromComplete {
				t.Fatal("completed book still marked reopened")
			}
			if !reflect.DeepEqual(got.PendingRewrites, tc.queue) || got.NextChapter() != 3 {
				t.Fatalf("migration changed next work: %+v", got)
			}
			if !tc.withoutMark && (got.ChapterWordCounts[2] != utf8.RuneCountInString(content) || got.TotalWordCount != 7+utf8.RuneCountInString(content)) {
				t.Fatalf("word counts do not match preserved final: %+v", got)
			}
			body, err := st.Drafts.LoadChapterText(2)
			if err != nil || body != content {
				t.Fatalf("final text changed: %q, %v", body, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || bytes.Contains(after, []byte("pending_review_chapter")) {
				t.Fatalf("old marker remains: %s, %v", after, err)
			}
			if tc.withoutMark && tc.pendingStage == "" && !bytes.Equal(before, after) {
				t.Fatal("book without legacy marker must stay unchanged")
			}
			pending, err := st.Signals.LoadPendingCommit()
			if err != nil {
				t.Fatal(err)
			}
			if tc.pendingStage == domain.CommitStageStateApplied {
				if pending == nil || pending.Stage != tc.pendingStage {
					t.Fatal("early commit recovery was discarded")
				}
			} else if pending != nil {
				t.Fatal("committed chapter recovery was not finished")
			}
			if tc.pendingStage == domain.CommitStageProgressMarked || tc.pendingStage == domain.CommitStageSignalSaved {
				if st.Checkpoints.LatestByStep(domain.ChapterScope(2), "commit") == nil {
					t.Fatal("commit checkpoint missing after recovery")
				}
			}
			if err := MigrateLegacyChapterProgress(st, stats); err != nil {
				t.Fatal(err)
			}
			again, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, again) {
				t.Fatalf("migration is not idempotent: %v", err)
			}
		})
	}
}

func TestMigrateLegacyChapterProgressKeepsMarkerOnReadFailure(t *testing.T) {
	for _, brokenOutline := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing final", true: "corrupt outline"}[brokenOutline], func(t *testing.T) {
			st := store.NewStore(t.TempDir())
			if err := st.Init(); err != nil {
				t.Fatal(err)
			}
			before := []byte(`{"phase":"writing","layered":true,"completed_chapters":[1],"pending_review_chapter":1}`)
			path := filepath.Join(st.Dir(), "meta", "progress.json")
			if err := os.WriteFile(path, before, 0o644); err != nil {
				t.Fatal(err)
			}
			if brokenOutline {
				if err := st.Drafts.SaveFinalChapter(1, "现有终稿"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(st.Dir(), "layered_outline.json"), []byte("{"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := MigrateLegacyChapterProgress(st, NewStyleStatsIndex(st)); err == nil {
				t.Fatal("missing/corrupt source should stop migration")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("failed migration lost original progress: %v", err)
			}
		})
	}
}
