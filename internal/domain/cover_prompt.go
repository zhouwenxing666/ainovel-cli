package domain

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	CoverPromptHeading = "封面提示词"
	CoverPromptAuthor  = "寒霄揽月 著"
)

var coverPromptPositions = map[string]bool{
	"上方":  true,
	"正中央": true,
	"下方":  true,
}

// CoverPrompt 是一份封面方案中允许模型创作的字段。固定文案由代码渲染，
// 避免模型漏段、改作者名或遗留占位符。
type CoverPrompt struct {
	Title         string `json:"title"`
	GenreTone     string `json:"genre_tone"`
	Subject       string `json:"subject"`
	Background    string `json:"background"`
	PrimaryColors string `json:"primary_colors"`
	TextPosition  string `json:"text_position"`
}

type CoverPromptSet struct {
	Prompts []CoverPrompt `json:"prompts"`
}

// ValidateCoverPromptSet 校验五份封面方案的机械约束。officialTitle 为空时跳过
// 第一份书名一致性校验，供导入综合在代码补齐正式书名前做第一阶段校验。
func ValidateCoverPromptSet(set CoverPromptSet, officialTitle string) error {
	if len(set.Prompts) != 5 {
		return fmt.Errorf("封面提示词必须恰好包含 5 份方案，当前为 %d 份", len(set.Prompts))
	}
	officialTitle = strings.TrimSpace(officialTitle)
	seenTitles := make(map[string]bool, len(set.Prompts))
	seenVisuals := make(map[string]bool, len(set.Prompts))
	for i, prompt := range set.Prompts {
		label := fmt.Sprintf("prompts[%d]", i)
		if err := validateCoverTitle(prompt.Title, i > 0); err != nil {
			return fmt.Errorf("%s.title: %w", label, err)
		}
		title := strings.TrimSpace(prompt.Title)
		if i == 0 && officialTitle != "" && title != officialTitle {
			return fmt.Errorf("%s.title 必须使用正式书名 %q，实际为 %q", label, officialTitle, title)
		}
		if seenTitles[title] {
			return fmt.Errorf("%s.title %q 与其他方案重复", label, title)
		}
		seenTitles[title] = true
		if i > 0 && officialTitle != "" && title == officialTitle {
			return fmt.Errorf("%s.title 不能与正式书名重复", label)
		}

		fields := []struct {
			name  string
			value string
		}{
			{"genre_tone", prompt.GenreTone},
			{"subject", prompt.Subject},
			{"background", prompt.Background},
			{"primary_colors", prompt.PrimaryColors},
		}
		for _, field := range fields {
			if err := validateCoverField(field.value); err != nil {
				return fmt.Errorf("%s.%s: %w", label, field.name, err)
			}
		}
		if coverGenreTone(prompt.GenreTone) == "" {
			return fmt.Errorf("%s.genre_tone 不能只填写‘风格’", label)
		}
		if !coverPromptPositions[prompt.TextPosition] {
			return fmt.Errorf("%s.text_position 必须是上方、正中央或下方，实际为 %q", label, prompt.TextPosition)
		}

		visualKey := strings.Join([]string{
			strings.TrimSpace(prompt.Subject),
			strings.TrimSpace(prompt.Background),
			strings.TrimSpace(prompt.PrimaryColors),
			prompt.TextPosition,
		}, "\x00")
		if seenVisuals[visualKey] {
			return fmt.Errorf("%s 与其他方案的主体、背景、配色和文字位置完全相同，必须提供不同视觉方向", label)
		}
		seenVisuals[visualKey] = true
	}
	return nil
}

func validateCoverTitle(title string, candidate bool) error {
	if title == "" || strings.TrimSpace(title) != title {
		return fmt.Errorf("书名不能为空或带首尾空格")
	}
	if strings.ContainsAny(title, "\r\n\t《》“”[]\"'") {
		return fmt.Errorf("书名必须为单行且不能包含书名号、引号或方括号")
	}
	if !candidate {
		return nil
	}
	count := utf8.RuneCountInString(title)
	if count < 1 || count > 15 {
		return fmt.Errorf("候选中文书名必须严格在 15 字以内，当前为 %d 字", count)
	}
	hasHan := false
	for _, r := range title {
		if unicode.Is(unicode.Han, r) {
			hasHan = true
			break
		}
	}
	if !hasHan {
		return fmt.Errorf("候选书名必须是中文书名")
	}
	return nil
}

func validateCoverField(value string) error {
	if value == "" || strings.TrimSpace(value) == "" {
		return fmt.Errorf("不能为空")
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("不能带首尾空格")
	}
	if strings.ContainsAny(value, "\r\n[]") {
		return fmt.Errorf("必须为单段文本且不能遗留方括号占位符")
	}
	return nil
}

// RenderCoverPromptSection 把结构化字段渲染为用户约定的唯一 Markdown 模板。
// 返回值包含 `## 封面提示词` 标题。
func RenderCoverPromptSection(set CoverPromptSet) string {
	var b strings.Builder
	b.WriteString("## ")
	b.WriteString(CoverPromptHeading)
	for i, prompt := range set.Prompts {
		kind := "候选书名"
		if i == 0 {
			kind = "原书名"
		}
		fmt.Fprintf(&b, "\n\n### 方案%s｜%s《%s》\n\n", coverPromptPlanNumber(i), kind, prompt.Title)
		fmt.Fprintf(&b, "生成一张极具视觉冲击力的番茄网络小说封面，%s风格。\n\n", coverGenreTone(prompt.GenreTone))
		fmt.Fprintf(&b, "画面主体：%s。\n\n", coverClause(prompt.Subject))
		b.WriteString("画面风格：动漫风二次元\n\n")
		fmt.Fprintf(&b, "背景环境：%s。\n\n", coverClause(prompt.Background))
		fmt.Fprintf(&b, "光影与氛围：电影级史诗打光，强烈的视觉张力，画面充满压迫感。%s。魔法发光特效，高对比度，最高画质，8k分辨率，虚幻引擎5渲染，杰作。\n\n", coverClause(prompt.PrimaryColors))
		fmt.Fprintf(&b, "文字排版：画面%s有巨大的、醒目的、具有视觉冲击力的3D艺术中文字体，写着书名“ %s ”，字体带有发光边缘和金属/史诗质感。在书名下方，有稍小一些的精致中文字体写着作者名：“ %s”。竖版尺寸比例 3:4。", prompt.TextPosition, prompt.Title, CoverPromptAuthor)
	}
	return b.String()
}

func coverPromptPlanNumber(index int) string {
	numbers := [...]string{"一", "二", "三", "四", "五"}
	if index < 0 || index >= len(numbers) {
		return fmt.Sprintf("%d", index+1)
	}
	return numbers[index]
}

func coverClause(value string) string {
	return strings.TrimRight(strings.TrimSpace(value), "。.；; ")
}

func coverGenreTone(value string) string {
	return strings.TrimSuffix(coverClause(value), "风格")
}

// UpsertCoverPromptSection 在 premise 中追加或精准替换封面二级章节；若历史文件
// 意外存在重复章节，只保留第一处位置并删除其余重复章节。
func UpsertCoverPromptSection(premise, section string) string {
	premise = strings.ReplaceAll(premise, "\r\n", "\n")
	section = strings.TrimSpace(strings.ReplaceAll(section, "\r\n", "\n"))
	lines := strings.Split(premise, "\n")
	var out []string
	inserted := false
	for i := 0; i < len(lines); {
		if isCoverPromptH2(lines[i]) {
			if !inserted {
				appendMarkdownBlock(&out, strings.Split(section, "\n"))
				inserted = true
			}
			i++
			for i < len(lines) && markdownHeadingLevel(lines[i]) != 1 && markdownHeadingLevel(lines[i]) != 2 {
				i++
			}
			continue
		}
		out = append(out, lines[i])
		i++
	}
	if !inserted {
		appendMarkdownBlock(&out, strings.Split(section, "\n"))
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

func appendMarkdownBlock(dst *[]string, block []string) {
	for len(*dst) > 0 && strings.TrimSpace((*dst)[len(*dst)-1]) == "" {
		*dst = (*dst)[:len(*dst)-1]
	}
	if len(*dst) > 0 {
		*dst = append(*dst, "")
	}
	*dst = append(*dst, block...)
	*dst = append(*dst, "")
}

func isCoverPromptH2(line string) bool {
	return markdownHeadingLevel(line) == 2 && markdownHeadingTitle(line) == CoverPromptHeading
}

func markdownHeadingLevel(line string) int {
	trimmed := strings.TrimSpace(line)
	level := 0
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level == len(trimmed) || trimmed[level] != ' ' {
		return 0
	}
	return level
}

func markdownHeadingTitle(line string) string {
	level := markdownHeadingLevel(line)
	if level == 0 {
		return ""
	}
	return strings.TrimSpace(strings.TrimSpace(line)[level:])
}

// HasCompleteCoverPromptSection 对落盘 Markdown 做轻量完整性检查，供 Foundation
// 门禁识别缺失/截断章节。结构化写入时更严格的字段校验由 ValidateCoverPromptSet 完成。
func HasCompleteCoverPromptSection(premise, officialTitle string) bool {
	lines := strings.Split(strings.ReplaceAll(premise, "\r\n", "\n"), "\n")
	start, count := -1, 0
	for i, line := range lines {
		if isCoverPromptH2(line) {
			count++
			if start < 0 {
				start = i + 1
			}
		}
	}
	if start < 0 || count != 1 {
		return false
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		level := markdownHeadingLevel(lines[i])
		if level == 1 || level == 2 {
			end = i
			break
		}
	}
	body := strings.Join(lines[start:end], "\n")
	if strings.ContainsAny(body, "[]") {
		return false
	}
	seen := map[string]bool{}
	segments := make([]string, 0, 5)
	var current strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if markdownHeadingLevel(line) == 3 {
			if current.Len() > 0 {
				segments = append(segments, current.String())
				current.Reset()
			}
			current.WriteString(line)
			current.WriteByte('\n')
			continue
		}
		if current.Len() > 0 {
			current.WriteString(line)
			current.WriteByte('\n')
		}
	}
	if current.Len() > 0 {
		segments = append(segments, current.String())
	}
	if len(segments) != 5 {
		return false
	}
	for i, segment := range segments {
		firstLine, _, _ := strings.Cut(segment, "\n")
		prefix := fmt.Sprintf("### 方案%s｜候选书名《", coverPromptPlanNumber(i))
		if i == 0 {
			prefix = "### 方案一｜原书名《"
		}
		if !strings.HasPrefix(firstLine, prefix) || !strings.HasSuffix(firstLine, "》") {
			return false
		}
		title := strings.TrimSuffix(strings.TrimPrefix(firstLine, prefix), "》")
		if err := validateCoverTitle(title, i > 0); err != nil || seen[title] {
			return false
		}
		if i == 0 && strings.TrimSpace(officialTitle) != "" && title != strings.TrimSpace(officialTitle) {
			return false
		}
		if i > 0 && title == strings.TrimSpace(officialTitle) {
			return false
		}
		seen[title] = true
		for _, required := range []string{
			"生成一张极具视觉冲击力的番茄网络小说封面，",
			"画面主体：", "画面风格：动漫风二次元", "背景环境：",
			"光影与氛围：", "文字排版：", "写着书名“ " + title + " ”",
			"作者名：“ " + CoverPromptAuthor + "”", "竖版尺寸比例 3:4。",
		} {
			if !strings.Contains(segment, required) {
				return false
			}
		}
	}
	return true
}
