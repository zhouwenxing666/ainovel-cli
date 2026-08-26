package domain

import (
	"strings"
	"testing"
)

func validCoverPromptSet(title string) CoverPromptSet {
	return CoverPromptSet{Prompts: []CoverPrompt{
		{Title: title, GenreTone: "末日异能热血", Subject: "黑衣少年握刀冲向镜头", Background: "异兽围城的未来废墟", PrimaryColors: "极致红黑对比色调", TextPosition: "上方"},
		{Title: "末日开局觉醒神级天赋", GenreTone: "末日异能逆袭", Subject: "负伤少年抬手唤醒金色符文", Background: "碎石飞舞的地下避难所", PrimaryColors: "暗黑与璀璨暗金色调", TextPosition: "正中央"},
		{Title: "全民求生我能无限进化", GenreTone: "全民求生升级", Subject: "少年双眼迸发蓝色电光", Background: "巨兽盘踞的钢铁城市", PrimaryColors: "深蓝与炽白高对比色调", TextPosition: "下方"},
		{Title: "灾变降临我杀穿禁区", GenreTone: "灾变杀伐爽文", Subject: "少年踏着断裂机甲向前", Background: "天穹崩裂的禁区战场", PrimaryColors: "猩红与冷金色调", TextPosition: "上方"},
		{Title: "我在废土镇压诸神", GenreTone: "废土神秘暗黑", Subject: "染血少年张开恶魔化手掌", Background: "邪神虚影笼罩荒芜都市", PrimaryColors: "紫黑与血红爆裂色调", TextPosition: "正中央"},
	}}
}

func TestCoverPromptSetRenderAndValidate(t *testing.T) {
	set := validCoverPromptSet("长夜燃灯")
	if err := ValidateCoverPromptSet(set, "长夜燃灯"); err != nil {
		t.Fatalf("ValidateCoverPromptSet: %v", err)
	}
	section := RenderCoverPromptSection(set)
	for _, want := range []string{
		"## 封面提示词", "### 方案一｜原书名《长夜燃灯》", "### 方案五｜候选书名《我在废土镇压诸神》",
		"画面风格：动漫风二次元", "作者名：“ 寒霄揽月 著”", "竖版尺寸比例 3:4。",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("render missing %q:\n%s", want, section)
		}
	}
	if strings.ContainsAny(section, "[]") {
		t.Fatalf("render must not contain placeholders: %s", section)
	}
	premise := "# 长夜燃灯\n\n## 作品简介\n简介\n\n" + section
	if !HasCompleteCoverPromptSection(premise, "长夜燃灯") {
		t.Fatal("rendered section should pass completeness check")
	}
	if synopsis := ExtractSynopsisFromPremise(premise); synopsis != "简介" {
		t.Fatalf("cover prompts must not leak into exported synopsis, got %q", synopsis)
	}
}

func TestCoverPromptSetRejectsLongOrDuplicateCandidates(t *testing.T) {
	set := validCoverPromptSet("长夜燃灯")
	set.Prompts[1].Title = "这是一个严格超过十五个汉字长度限制的候选书名"
	if err := ValidateCoverPromptSet(set, "长夜燃灯"); err == nil || !strings.Contains(err.Error(), "15") {
		t.Fatalf("long candidate should fail, got %v", err)
	}

	set = validCoverPromptSet("长夜燃灯")
	set.Prompts[4].Subject = set.Prompts[3].Subject
	set.Prompts[4].Background = set.Prompts[3].Background
	set.Prompts[4].PrimaryColors = set.Prompts[3].PrimaryColors
	set.Prompts[4].TextPosition = set.Prompts[3].TextPosition
	if err := ValidateCoverPromptSet(set, "长夜燃灯"); err == nil || !strings.Contains(err.Error(), "不同视觉方向") {
		t.Fatalf("duplicate visual direction should fail, got %v", err)
	}
}

func TestUpsertCoverPromptSectionReplacesAndDeduplicates(t *testing.T) {
	first := RenderCoverPromptSection(validCoverPromptSet("长夜燃灯"))
	secondSet := validCoverPromptSet("长夜燃灯")
	secondSet.Prompts[0].PrimaryColors = "青白与深黑对比色调"
	second := RenderCoverPromptSection(secondSet)
	premise := "# 长夜燃灯\n\n## 核心冲突\n冲突\n\n" + first + "\n\n## 作品简介\n简介\n\n" + first
	if HasCompleteCoverPromptSection(premise, "长夜燃灯") {
		t.Fatal("duplicate cover sections must fail completeness check")
	}
	got := UpsertCoverPromptSection(premise, second)
	if strings.Count(got, "## 封面提示词") != 1 {
		t.Fatalf("cover section must be unique:\n%s", got)
	}
	if !strings.Contains(got, "## 核心冲突\n冲突") || !strings.Contains(got, "## 作品简介\n简介") {
		t.Fatalf("other premise sections must be preserved:\n%s", got)
	}
	if !strings.Contains(got, "青白与深黑对比色调") {
		t.Fatalf("replacement content missing:\n%s", got)
	}
}
