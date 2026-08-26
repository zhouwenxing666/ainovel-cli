package imp

import (
	"encoding/json"

	"github.com/voocel/ainovel-cli/internal/domain"
)

func coverPromptsForTest(title string) domain.CoverPromptSet {
	return domain.CoverPromptSet{Prompts: []domain.CoverPrompt{
		{Title: title, GenreTone: "东方玄幻热血", Subject: "黑衣少年持剑立于风暴之中", Background: "崩裂的古城与盘旋雷云", PrimaryColors: "极致红黑对比色调", TextPosition: "上方"},
		{Title: "开局逆天改命", GenreTone: "东方玄幻逆袭", Subject: "负伤少年抬手唤醒金色符文", Background: "碎石飞舞的宗门战场", PrimaryColors: "暗黑与璀璨暗金色调", TextPosition: "正中央"},
		{Title: "全民觉醒我无敌", GenreTone: "高燃异能升级", Subject: "少年双眼迸发蓝色电光俯视镜头", Background: "异兽围城的未来废墟", PrimaryColors: "深蓝与炽白高对比色调", TextPosition: "下方"},
		{Title: "废柴崛起镇万界", GenreTone: "废柴逆袭爽文", Subject: "少年踏着断剑向王座逼近", Background: "万族强者匍匐的破碎天宫", PrimaryColors: "猩红与冷金色调", TextPosition: "上方"},
		{Title: "我靠禁术杀穿诸天", GenreTone: "暗黑玄幻杀伐", Subject: "染血少年张开布满禁纹的手掌", Background: "诸天裂缝吞噬黑暗战场", PrimaryColors: "紫黑与血红爆裂色调", TextPosition: "正中央"},
	}}
}

func boundaryFixture(unitID, anchor, kind, title string) map[string]any {
	var anchorValue, titleValue any
	if anchor != "" {
		anchorValue = anchor
	}
	if title != "" {
		titleValue = title
	}
	return map[string]any{
		"unit_id": unitID, "anchor": anchorValue, "kind": kind, "title": titleValue,
		"uncertain": false, "reason": nil,
	}
}

func boundariesJSON(boundaries ...map[string]any) string {
	data, err := json.Marshal(map[string]any{"boundaries": boundaries})
	if err != nil {
		panic(err)
	}
	return string(data)
}

func rangeDigestJSON(start, end int, plot string) string {
	data, err := json.Marshal(map[string]any{
		"start_chapter":    start,
		"end_chapter":      end,
		"plot":             plot,
		"characters":       []string{},
		"world_facts":      []string{},
		"opened_threads":   []string{},
		"resolved_threads": []string{},
	})
	if err != nil {
		panic(err)
	}
	return string(data)
}

func synthesisFixtureJSON(endChapter int, status string) string {
	data, err := json.Marshal(map[string]any{
		"premise":       "# 测试书\n前提",
		"synopsis":      "这是一个关于甲直面困境、守住信念并寻找出路的故事。",
		"cover_prompts": coverPromptsForTest("测试书"),
		"characters": []any{map[string]any{
			"name": "甲", "aliases": []string{}, "role": "protagonist", "description": "d",
			"arc": "a", "traits": []string{"坚韧"}, "tier": nil,
		}},
		"world_rules": []any{},
		"structure": []any{map[string]any{
			"title": "卷一", "theme": "主题", "arcs": []any{map[string]any{
				"title": "弧一", "goal": "目标", "start_chapter": 1, "end_chapter": endChapter,
			}},
		}},
		"compass": map[string]any{
			"ending_direction": "终局", "open_threads": []string{},
			"estimated_scale": nil, "last_updated": nil,
		},
		"planning_tier": "short",
		"story_status":  status,
		"status_reason": "根据正文判断",
	})
	if err != nil {
		panic(err)
	}
	return string(data)
}
