package store

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain"
)

func storeCoverPromptSet(title string) domain.CoverPromptSet {
	return domain.CoverPromptSet{Prompts: []domain.CoverPrompt{
		{Title: title, GenreTone: "玄幻热血", Subject: "少年持剑", Background: "雷云古城", PrimaryColors: "红黑色调", TextPosition: "上方"},
		{Title: "开局觉醒逆天改命", GenreTone: "玄幻逆袭", Subject: "少年唤醒符文", Background: "宗门战场", PrimaryColors: "暗金色调", TextPosition: "正中央"},
		{Title: "全民修仙我无敌", GenreTone: "全民修仙", Subject: "少年踏空而立", Background: "万峰崩裂", PrimaryColors: "蓝白色调", TextPosition: "下方"},
		{Title: "废柴崛起镇万界", GenreTone: "废柴逆袭", Subject: "少年登临王座", Background: "破碎天宫", PrimaryColors: "猩红冷金色调", TextPosition: "上方"},
		{Title: "我靠禁术杀穿诸天", GenreTone: "暗黑杀伐", Subject: "少年张开禁纹手掌", Background: "诸天裂缝", PrimaryColors: "紫黑血红色调", TextPosition: "正中央"},
	}}
}

func TestFoundationMissingReturnsReadError(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "outline.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FoundationMissing(); err == nil {
		t.Fatal("损坏的大纲必须返回读取错误，不能降级成缺失项")
	}
}

func TestFoundationMissingRequiresCoverBeforeAuditForNewBook(t *testing.T) {
	st := NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("长夜燃灯", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SavePremise("# 长夜燃灯\n\n## 作品简介\n简介"); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{{Chapter: 1, Title: "开局", CoreEvent: "启程"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Characters.Save([]domain.Character{{Name: "林舟", Role: "主角"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.World.SaveWorldRules([]domain.WorldRule{{Category: "力量", Rule: "能力有代价", Boundary: "不可无限使用"}}); err != nil {
		t.Fatal(err)
	}
	missing, err := st.FoundationMissing()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(missing, []string{"cover_prompt"}) {
		t.Fatalf("missing = %v, want [cover_prompt]", missing)
	}
	if err := st.Outline.SaveCoverPromptSet(storeCoverPromptSet("长夜燃灯")); err != nil {
		t.Fatal(err)
	}
	missing, err = st.FoundationMissing()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(missing, []string{"foundation_audit"}) {
		t.Fatalf("missing = %v, want [foundation_audit]", missing)
	}
}

func TestFoundationMissingMigratesLegacyWritingBookWithoutOverwritingExistingSection(t *testing.T) {
	st := NewStore(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	if err := st.Progress.Init("旧书", 1); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []domain.Phase{domain.PhasePremise, domain.PhaseOutline, domain.PhaseWriting} {
		if err := st.Progress.UpdatePhase(phase); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Outline.SavePremise("# 旧书"); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SaveOutline([]domain.OutlineEntry{{Chapter: 1, Title: "旧章", CoreEvent: "旧事"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Characters.Save([]domain.Character{{Name: "旧主角"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.World.SaveWorldRules([]domain.WorldRule{{Category: "旧规则", Rule: "存在", Boundary: "有限"}}); err != nil {
		t.Fatal(err)
	}
	missing, err := st.FoundationMissing()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(missing, []string{"cover_prompt"}) {
		t.Fatalf("legacy writing book should request cover migration, got %v", missing)
	}

	// 只要旧书已有二级标题（即使是人工自由格式），迁移就不得覆盖。
	if err := st.Outline.SavePremise("# 旧书\n\n## 封面提示词\n人工润色的旧封面方案"); err != nil {
		t.Fatal(err)
	}
	missing, err = st.FoundationMissing()
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("existing manual cover section must be preserved, got %v", missing)
	}

	// complete 是终态，即使删除封面章节也不自动迁移。
	if err := st.Progress.UpdatePhase(domain.PhaseComplete); err != nil {
		t.Fatal(err)
	}
	if err := st.Outline.SavePremise("# 旧书"); err != nil {
		t.Fatal(err)
	}
	missing, err = st.FoundationMissing()
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("complete book must not be migrated, got %v", missing)
	}
}

func TestClearHandledSteerKeepsIntentWhenProgressReadFails(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	if err := st.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.RunMeta.Init("default", "test", "model"); err != nil {
		t.Fatalf("RunMeta.Init: %v", err)
	}
	if err := st.RunMeta.SetPendingSteer("保留这条干预"); err != nil {
		t.Fatalf("SetPendingSteer: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta", "progress.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearHandledSteer(); err == nil {
		t.Fatal("corrupt progress should make ClearHandledSteer fail")
	}
	meta, err := st.RunMeta.Load()
	if err != nil {
		t.Fatalf("RunMeta.Load: %v", err)
	}
	if meta == nil || meta.PendingSteer != "保留这条干预" {
		t.Fatalf("recovery intent was lost after partial clear: %+v", meta)
	}
}
