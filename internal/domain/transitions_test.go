package domain

import "testing"

func TestCanTransitionPhase(t *testing.T) {
	tests := []struct {
		from Phase
		to   Phase
		want bool
	}{
		{from: "", to: PhaseInit, want: true},
		{from: PhaseInit, to: PhasePremise, want: true},
		{from: PhaseInit, to: PhaseOutline, want: true},
		{from: PhaseOutline, to: PhaseWriting, want: true},
		{from: PhaseWriting, to: PhaseComplete, want: true},
		{from: PhaseOutline, to: PhasePremise, want: false},
		{from: PhaseComplete, to: PhaseWriting, want: false},
	}
	for _, tt := range tests {
		if got := CanTransitionPhase(tt.from, tt.to); got != tt.want {
			t.Fatalf("CanTransitionPhase(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
		}
	}
}

func TestCanTransitionFlow(t *testing.T) {
	tests := []struct {
		from FlowState
		to   FlowState
		want bool
	}{
		{from: "", to: FlowRewriting, want: true},
		{from: FlowWriting, to: FlowReviewing, want: true},
		{from: FlowReviewing, to: FlowPolishing, want: true},
		{from: FlowRewriting, to: FlowWriting, want: true},
		{from: FlowSteering, to: FlowRewriting, want: true},
		{from: FlowRewriting, to: FlowReviewing, want: false},
		{from: FlowPolishing, to: FlowReviewing, want: false},
	}
	for _, tt := range tests {
		if got := CanTransitionFlow(tt.from, tt.to); got != tt.want {
			t.Fatalf("CanTransitionFlow(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
		}
	}
}

func TestExtractNovelNameFromPremise_Placeholder(t *testing.T) {
	cases := []struct {
		name    string
		premise string
		want    string
	}{
		{"真实书名", "# 长夜将明\n\n## 题材", "长夜将明"},
		{"带书名号", "# 《星河彼岸》\n## 题材", "星河彼岸"},
		{"占位-书名", "# 书名\n## 题材", ""},
		{"占位-示例书名", "# 《示例书名》\n## 题材", ""},
		{"占位-实际书名", "# 实际书名\n## 题材", ""},
		{"首行非标题", "纯文本第一行\n# 书名", ""},
	}
	for _, c := range cases {
		if got := ExtractNovelNameFromPremise(c.premise); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestExtractSynopsisFromPremise(t *testing.T) {
	cases := []struct {
		name    string
		premise string
		want    string
	}{
		{
			"正常段落",
			"# 书名\n\n## 题材和基调\n玄幻\n\n## 作品简介\n这是一个关于勇气的故事。\n不剧透转折。\n\n## 核心冲突\n冲突",
			"这是一个关于勇气的故事。\n不剧透转折。",
		},
		{"缺失段落", "# 书名\n\n## 题材和基调\n玄幻\n\n## 核心冲突\n冲突", ""},
		{"段落到文末", "# 书名\n\n## 作品简介\n末段简介，后面没有别的标题。", "末段简介，后面没有别的标题。"},
		{"空内容段落", "# 书名\n\n## 作品简介\n\n## 核心冲突\n冲突", ""},
		{
			"一级标题也结束段落",
			"# 书名\n\n## 作品简介\n简介内容\n\n# 另一个标题",
			"简介内容",
		},
	}
	for _, c := range cases {
		if got := ExtractSynopsisFromPremise(c.premise); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
