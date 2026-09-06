package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"unicode/utf8"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/store"
)

// MigrateLegacyChapterProgress 移除旧版逐章润色的等待标记，保留已落盘正文，
// 并补齐原本延后到润色结束时执行的字数和完本判定。
// 仅在 Host 持有书籍独占锁、尚未启动 Worker 时调用。
func MigrateLegacyChapterProgress(st *store.Store, styleStats *StyleStatsIndex) error {
	data, err := os.ReadFile(filepath.Join(st.Dir(), "meta", "progress.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var legacy struct {
		domain.Progress
		PendingReviewChapter int `json:"pending_review_chapter"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return fmt.Errorf("decode legacy progress: %w", err)
	}
	pending, err := st.Signals.LoadPendingCommit()
	if err != nil {
		return err
	}
	lateCommit := pending != nil && (pending.Stage == domain.CommitStageProgressMarked || pending.Stage == domain.CommitStageSignalSaved)
	if chapter := legacy.PendingReviewChapter; chapter > 0 {
		p := &legacy.Progress
		if !slices.Contains(p.CompletedChapters, chapter) {
			return fmt.Errorf("旧版待润色章节 %d 未标记提交完成", chapter)
		}
		content, err := st.Drafts.LoadChapterText(chapter)
		if err != nil {
			return err
		}
		if content == "" {
			return fmt.Errorf("旧版待润色章节 %d 缺少终稿", chapter)
		}
		// 旧进程可能在覆盖终稿之后、更新字数之前退出。
		if p.ChapterWordCounts == nil {
			p.ChapterWordCounts = make(map[int]int)
		}
		wordCount := utf8.RuneCountInString(content)
		p.TotalWordCount += wordCount - p.ChapterWordCounts[chapter]
		p.ChapterWordCounts[chapter] = wordCount
		// 早期提交仍需按冻结载荷重放；不能先把它的书标记为完结。
		if p.Phase == domain.PhaseWriting && len(p.PendingRewrites) == 0 && (pending == nil || lateCommit) {
			complete := false
			switch {
			case p.Layered && p.ReopenedFromComplete:
				complete, err = layeredStructurallyComplete(st, p)
			case p.Layered:
				complete, err = layeredComplete(st, p)
			default:
				complete = p.TotalChapters > 0 && len(p.CompletedChapters) >= p.TotalChapters
			}
			if err != nil {
				return fmt.Errorf("evaluate legacy completion: %w", err)
			}
			if complete {
				p.Phase = domain.PhaseComplete
				p.ReopenedFromComplete = false
			}
		}
		// 同一次原子写入删除旧字段并更新字数/终态，重试不会丢失迁移依据。
		if err := st.Progress.Save(p); err != nil {
			return err
		}
	}
	// 完本状态保存后若崩溃，Engine 不会再派 Worker。无论旧标记是否还在，
	// 都复用提交工具收尾已完成事实写入的 Saga；该路径只补 checkpoint 和清理。
	if lateCommit {
		args, err := json.Marshal(map[string]int{"chapter": pending.Chapter})
		if err != nil {
			return err
		}
		if _, err := NewCommitChapterTool(st, styleStats).Execute(context.Background(), args); err != nil {
			return fmt.Errorf("recover committed chapter: %w", err)
		}
	}
	return nil
}
