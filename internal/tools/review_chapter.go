package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/voocel/agentcore/schema"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/rules"
	"github.com/voocel/ainovel-cli/internal/store"
)

// FinalizeReviewedChapterTool 是 Writer 与 Editor 之间的章节质量闸。
// 模型先执行 Humanizer，再做只改台词的情绪优化；工具机械验证处理顺序、
// 乐观锁、台词边界和 5% 字数上限，最后覆盖终稿并清除待 Reviewer 事实。
type FinalizeReviewedChapterTool struct {
	store      *store.Store
	styleStats *StyleStatsIndex
}

func NewFinalizeReviewedChapterTool(st *store.Store, styleStats *StyleStatsIndex) *FinalizeReviewedChapterTool {
	return &FinalizeReviewedChapterTool{store: st, styleStats: styleStats}
}

func (t *FinalizeReviewedChapterTool) Name() string                           { return "finalize_reviewed_chapter" }
func (t *FinalizeReviewedChapterTool) Label() string                          { return "保存 Reviewer 终稿" }
func (t *FinalizeReviewedChapterTool) ReadOnly(_ json.RawMessage) bool        { return false }
func (t *FinalizeReviewedChapterTool) ConcurrencySafe(_ json.RawMessage) bool { return false }
func (t *FinalizeReviewedChapterTool) StrictSchema() bool                     { return true }
func (t *FinalizeReviewedChapterTool) ActivityDescription(_ json.RawMessage) string {
	return "保存去 AI 味与情绪优化后的章节"
}

func (t *FinalizeReviewedChapterTool) Description() string {
	return "保存 Reviewer 处理后的章节终稿。必须先 read_chapter(source=\"final\")，把返回的 digest 原样传入 source_digest。" +
		"先按 Humanizer 检测：没有 AI 痕迹时 ai_patterns=[] 且 humanized_content 留空；发现 AI 痕迹时列出具体模式并提供去 AI 后的完整 humanized_content。" +
		"再以原文或 humanized_content 为底稿，只改成对出现的引号内台词，完整结果放 content。工具会拒绝陈旧来源、台词外改动、排版变化和全文超过原稿 5% 的增幅。"
}

func (t *FinalizeReviewedChapterTool) Schema() map[string]any {
	return schema.Object(
		schema.Property("chapter", schema.Int("章节号")).Required(),
		schema.Property("source_digest", schema.String("最近一次 read_chapter(final) 返回的 digest")).Required(),
		schema.Property("ai_patterns", schema.Array("Humanizer 实际发现的具体 AI 写作模式；没有则为空数组", schema.String("模式与原文位置"))).Required(),
		schema.Property("humanized_content", schema.String("去 AI 后的完整章节；ai_patterns 为空时必须留空")).Required(),
		schema.Property("content", schema.String("在 Humanizer 结果上仅优化台词后的完整终稿")).Required(),
	)
}

type finalizeReviewedChapterArgs struct {
	Chapter          int      `json:"chapter"`
	SourceDigest     string   `json:"source_digest"`
	AIPatterns       []string `json:"ai_patterns"`
	HumanizedContent string   `json:"humanized_content"`
	Content          string   `json:"content"`
}

func (t *FinalizeReviewedChapterTool) Execute(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var a finalizeReviewedChapterArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid args: %w: %w", errs.ErrToolArgs, err)
	}
	if a.Chapter <= 0 {
		return nil, fmt.Errorf("chapter must be > 0: %w", errs.ErrToolArgs)
	}

	progress, err := t.store.Progress.Load()
	if err != nil {
		return nil, fmt.Errorf("load progress: %w: %w", errs.ErrStoreRead, err)
	}
	if progress == nil || progress.Phase != domain.PhaseWriting {
		return nil, fmt.Errorf("Reviewer 仅允许在 writing 阶段工作: %w", errs.ErrToolPrecondition)
	}
	if progress.PendingReviewChapter != a.Chapter {
		return nil, fmt.Errorf("Reviewer 目标章不匹配：待处理第 %d 章，收到第 %d 章: %w",
			progress.PendingReviewChapter, a.Chapter, errs.ErrToolConflict)
	}
	if !slices.Contains(progress.CompletedChapters, a.Chapter) {
		return nil, fmt.Errorf("第 %d 章尚未由 Writer 提交: %w", a.Chapter, errs.ErrToolPrecondition)
	}

	source, err := t.store.Drafts.LoadChapterText(a.Chapter)
	if err != nil {
		return nil, fmt.Errorf("load final chapter: %w: %w", errs.ErrStoreRead, err)
	}
	if source == "" {
		return nil, fmt.Errorf("第 %d 章终稿不存在: %w", a.Chapter, errs.ErrToolPrecondition)
	}

	// 若上次调用已写正文和 checkpoint、但在清 progress 前崩溃，直接收尾。
	// 不再次 Humanizer/情绪加码，避免同一章被重复放大。
	if reviewerCheckpointMatches(t.store, a.Chapter, source) || reviewerIntentMatches(t.store, a.Chapter, source) {
		// intent 与当前正文一致说明终稿原子写入已成功，只差正式 checkpoint/progress。
		// 先补正式 checkpoint，让 StopGuard 和后续恢复看到统一的完成事实。
		if !reviewerCheckpointMatches(t.store, a.Chapter, source) {
			if _, err := t.store.Checkpoints.AppendArtifact(
				domain.ChapterScope(a.Chapter), "chapter_review", fmt.Sprintf("chapters/%02d.md", a.Chapter),
			); err != nil {
				return nil, fmt.Errorf("recover reviewer checkpoint: %w: %w", errs.ErrStoreWrite, err)
			}
		}
		complete, err := reviewerCompletesBook(t.store, progress)
		if err != nil {
			return nil, err
		}
		if err := t.store.Progress.CompleteChapterReview(a.Chapter, utf8.RuneCountInString(source), complete); err != nil {
			return nil, fmt.Errorf("recover reviewer progress: %w: %w", errs.ErrStoreWrite, err)
		}
		if _, err := t.store.Checkpoints.AppendArtifact(
			domain.ChapterScope(a.Chapter), "chapter_review_recovered", "meta/progress.json",
		); err != nil {
			return nil, fmt.Errorf("checkpoint recovered reviewer: %w: %w", errs.ErrStoreWrite, err)
		}
		t.refreshDerivedFacts(a.Chapter, source)
		return json.Marshal(map[string]any{
			"chapter": a.Chapter, "reviewed": true, "recovered": true,
			"word_count": utf8.RuneCountInString(source), "book_complete": complete,
		})
	}

	if strings.TrimSpace(a.SourceDigest) == "" || a.SourceDigest != chapterContentDigest(source) {
		return nil, fmt.Errorf("source_digest 已过期；重新 read_chapter(source=\"final\") 后再处理: %w", errs.ErrToolConflict)
	}
	patterns := normalizeFindings(a.AIPatterns)
	base := source
	if len(patterns) == 0 {
		if a.HumanizedContent != "" {
			return nil, fmt.Errorf("未发现 AI 痕迹时必须跳过 Humanizer 改写，humanized_content 应留空: %w", errs.ErrToolArgs)
		}
	} else {
		if a.HumanizedContent == "" || a.HumanizedContent == source {
			return nil, fmt.Errorf("已报告 AI 痕迹时必须提供实际改写后的 humanized_content: %w", errs.ErrToolArgs)
		}
		if err := validateHumanizerLayout(source, a.HumanizedContent); err != nil {
			return nil, err
		}
		base = a.HumanizedContent
	}
	if strings.TrimSpace(a.Content) == "" {
		return nil, fmt.Errorf("content 不能为空: %w", errs.ErrToolArgs)
	}
	changedDialogues, err := validateEmotionPass(base, a.Content)
	if err != nil {
		return nil, err
	}
	sourceRunes := utf8.RuneCountInString(source)
	finalRunes := utf8.RuneCountInString(a.Content)
	if finalRunes*100 > sourceRunes*105 {
		return nil, fmt.Errorf("Reviewer 终稿超过原稿 5%% 增幅：原稿 %d 字，终稿 %d 字: %w",
			sourceRunes, finalRunes, errs.ErrToolArgs)
	}

	// 先为“即将写入的终稿”记录 intent digest，再原子覆盖正文，最后追加正式
	// checkpoint。这样两侧崩溃都可判定：intent 后、正文前崩溃时当前正文 digest
	// 不匹配，安全重做；正文后、正式 checkpoint/progress 前崩溃时二者匹配，入口
	// 补齐 checkpoint 后收尾，不会对同一章重复 Humanizer/情绪加码。
	artifact := fmt.Sprintf("chapters/%02d.md", a.Chapter)
	if _, err := t.store.Checkpoints.Append(
		domain.ChapterScope(a.Chapter), "chapter_review_intent", artifact, chapterContentDigest(a.Content),
	); err != nil {
		return nil, fmt.Errorf("checkpoint chapter review intent: %w: %w", errs.ErrStoreWrite, err)
	}
	if err := t.store.Drafts.SaveFinalChapter(a.Chapter, a.Content); err != nil {
		return nil, fmt.Errorf("save reviewed chapter: %w: %w", errs.ErrStoreWrite, err)
	}
	if _, err := t.store.Checkpoints.AppendArtifact(
		domain.ChapterScope(a.Chapter), "chapter_review", artifact,
	); err != nil {
		return nil, fmt.Errorf("checkpoint chapter review: %w: %w", errs.ErrStoreWrite, err)
	}
	complete, err := reviewerCompletesBook(t.store, progress)
	if err != nil {
		return nil, err
	}
	if err := t.store.Progress.CompleteChapterReview(a.Chapter, finalRunes, complete); err != nil {
		return nil, fmt.Errorf("complete reviewer progress: %w: %w", errs.ErrStoreWrite, err)
	}
	t.refreshDerivedFacts(a.Chapter, a.Content)

	return json.Marshal(map[string]any{
		"chapter": a.Chapter, "reviewed": true, "humanizer_changed": len(patterns) > 0,
		"ai_patterns": patterns, "emotion_changes": changedDialogues,
		"word_count": finalRunes, "book_complete": complete,
	})
}

func chapterContentDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func reviewerCheckpointMatches(st *store.Store, chapter int, content string) bool {
	cp := st.Checkpoints.LatestByStep(domain.ChapterScope(chapter), "chapter_review")
	return cp != nil && cp.Digest != "" && cp.Digest == chapterContentDigest(content)
}

func reviewerIntentMatches(st *store.Store, chapter int, content string) bool {
	cp := st.Checkpoints.LatestByStep(domain.ChapterScope(chapter), "chapter_review_intent")
	return cp != nil && cp.Digest != "" && cp.Digest == chapterContentDigest(content)
}

func normalizeFindings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

// validateHumanizerLayout 允许 Humanizer 改写文字，但不允许改变章节的行数、
// 空行位置或对话定界符结构。中文小说中的弯引号是合法排版，不按英文规则替换。
func validateHumanizerLayout(source, humanized string) error {
	sourceLines := strings.Split(source, "\n")
	humanizedLines := strings.Split(humanized, "\n")
	if len(sourceLines) != len(humanizedLines) {
		return fmt.Errorf("Humanizer 不得改变换行排版（%d 行 → %d 行）: %w",
			len(sourceLines), len(humanizedLines), errs.ErrToolArgs)
	}
	for i := range sourceLines {
		if (strings.TrimSpace(sourceLines[i]) == "") != (strings.TrimSpace(humanizedLines[i]) == "") {
			return fmt.Errorf("Humanizer 不得改变第 %d 行的空行结构: %w", i+1, errs.ErrToolArgs)
		}
	}
	_, _, sourceDelimiters, err := dialogueStructure(source)
	if err != nil {
		return fmt.Errorf("原稿引号结构无效: %w", err)
	}
	_, _, humanizedDelimiters, err := dialogueStructure(humanized)
	if err != nil {
		return fmt.Errorf("Humanizer 结果引号结构无效: %w", err)
	}
	if sourceDelimiters != humanizedDelimiters {
		return fmt.Errorf("Humanizer 不得增删或替换对话引号: %w", errs.ErrToolArgs)
	}
	return nil
}

// validateEmotionPass 机械保证情绪优化只改成对引号内的台词。叙述、标题、动作、
// 引号本身、段落和每句台词内部的换行数都必须保持不变。
func validateEmotionPass(base, final string) (int, error) {
	baseSkeleton, baseDialogues, _, err := dialogueStructure(base)
	if err != nil {
		return 0, fmt.Errorf("情绪优化底稿引号结构无效: %w", err)
	}
	finalSkeleton, finalDialogues, _, err := dialogueStructure(final)
	if err != nil {
		return 0, fmt.Errorf("情绪优化结果引号结构无效: %w", err)
	}
	if baseSkeleton != finalSkeleton || len(baseDialogues) != len(finalDialogues) {
		return 0, fmt.Errorf("情绪优化只能修改成对引号内台词；检测到叙述、引号或排版变化: %w", errs.ErrToolArgs)
	}
	changed := 0
	for i := range baseDialogues {
		if strings.Count(baseDialogues[i], "\n") != strings.Count(finalDialogues[i], "\n") {
			return 0, fmt.Errorf("第 %d 句台词内部换行发生变化: %w", i+1, errs.ErrToolArgs)
		}
		if baseDialogues[i] != finalDialogues[i] {
			changed++
		}
	}
	return changed, nil
}

// dialogueStructure 把成对中文双引号和 ASCII 双引号中的内容替换为占位符，
// 返回可逐字比较的“台词外骨架”、台词列表和定界符序列。
func dialogueStructure(text string) (string, []string, string, error) {
	const placeholder = '\x00'
	var skeleton, dialogue, delimiters strings.Builder
	var closeQuote rune
	var dialogues []string
	for _, r := range text {
		if closeQuote == 0 {
			switch r {
			case '“':
				closeQuote = '”'
				skeleton.WriteRune(r)
				skeleton.WriteRune(placeholder)
				delimiters.WriteRune(r)
			case '"':
				closeQuote = '"'
				skeleton.WriteRune(r)
				skeleton.WriteRune(placeholder)
				delimiters.WriteRune(r)
			default:
				skeleton.WriteRune(r)
			}
			continue
		}
		if r == closeQuote {
			dialogues = append(dialogues, dialogue.String())
			dialogue.Reset()
			skeleton.WriteRune(r)
			delimiters.WriteRune(r)
			closeQuote = 0
			continue
		}
		dialogue.WriteRune(r)
	}
	if closeQuote != 0 {
		return "", nil, "", fmt.Errorf("存在未闭合的对话引号: %w", errs.ErrToolArgs)
	}
	return skeleton.String(), dialogues, delimiters.String(), nil
}

func reviewerCompletesBook(st *store.Store, p *domain.Progress) (bool, error) {
	if p == nil {
		return false, nil
	}
	if !p.Layered {
		return p.TotalChapters > 0 && len(p.CompletedChapters) >= p.TotalChapters, nil
	}
	copy := *p
	copy.PendingReviewChapter = 0
	if p.ReopenedFromComplete {
		// 返工重开的书沿用 commit_chapter 原有语义：Reviewer 是最后一块正文事实，
		// 清除待处理标记后按结构完整重新完结。
		return layeredStructurallyComplete(st, &copy)
	}
	// 正向分层书沿用原有完结谓词：未宣告收官时可走质量级兜底；已宣告
	// 收官卷时仍须等 Editor 的卷末三连，故这里会返回 false。
	return layeredComplete(st, &copy)
}

func (t *FinalizeReviewedChapterTool) refreshDerivedFacts(chapter int, content string) {
	structured := rules.SystemDefaults().Structured
	if snap, err := t.store.UserRules.Load(); err == nil && snap != nil {
		structured = snap.Structured
	}
	violations := append(rules.Lint(content), rules.Check(content, structured)...)
	if err := t.store.World.SaveRuleViolations(chapter, violations); err != nil {
		slog.Warn("Reviewer 后机械违规落盘失败", "module", "tools", "chapter", chapter, "err", err)
	}
	if t.styleStats != nil {
		t.styleStats.ChapterCommitted(chapter, content)
	}
}
