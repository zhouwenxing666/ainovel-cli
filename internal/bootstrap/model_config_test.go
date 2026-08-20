package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/codexcli"
)

type dynamicFailoverTestModel struct {
	err   error
	final string
}

func (m *dynamicFailoverTestModel) Generate(context.Context, []agentcore.Message, []agentcore.ToolSpec, ...agentcore.CallOption) (*agentcore.LLMResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &agentcore.LLMResponse{Message: agentcore.Message{
		Role: agentcore.RoleAssistant, Content: []agentcore.ContentBlock{agentcore.TextBlock(m.final)},
	}}, nil
}
func (m *dynamicFailoverTestModel) GenerateStream(context.Context, []agentcore.Message, []agentcore.ToolSpec, ...agentcore.CallOption) (<-chan agentcore.StreamEvent, error) {
	return nil, m.err
}
func (m *dynamicFailoverTestModel) SupportsTools() bool { return false }

func TestDynamicRoleWithFailoverTracksHotAppliedArbiterFallbacks(t *testing.T) {
	primary := &dynamicFailoverTestModel{err: agentcore.ErrProviderNetwork}
	fallback := &dynamicFailoverTestModel{final: "fallback"}
	set := &ModelSet{
		Default: NewSwappableModel("primary", "model", primary, nil),
		models: map[string]*SwappableModel{
			"arbiter": NewSwappableModel("primary", "model", primary, nil),
		},
		fallbacks: map[string][]modelTarget{
			"arbiter": {{provider: "backup", name: "model", model: fallback}},
		},
	}
	dynamic := set.DynamicRoleWithFailover("arbiter", nil)
	response, err := dynamic.Generate(context.Background(), nil, nil)
	if err != nil || response.Message.TextContent() != "fallback" {
		t.Fatalf("arbiter fallback did not run: response=%+v err=%v", response, err)
	}

	set.ApplyPrepared(&ModelSet{
		Default: NewSwappableModel("primary", "model", primary, nil),
		models: map[string]*SwappableModel{
			"arbiter": NewSwappableModel("primary", "model", primary, nil),
		},
		fallbacks: map[string][]modelTarget{},
	})
	if _, err := dynamic.Generate(context.Background(), nil, nil); !errors.Is(err, agentcore.ErrProviderNetwork) {
		t.Fatalf("dynamic wrapper kept stale fallback after hot apply: %v", err)
	}
}

func TestModelConfigAcceptsLegacyAndObjectEntries(t *testing.T) {
	var cfg Config
	input := `{
  "provider":"custom","model":"legacy-model",
  "providers":{"custom":{"type":"openai","models":[
    "legacy-model",
    {"name":"large-model","context_window":400000}
  ]}}
}`
	if err := json.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	models := cfg.Providers["custom"].Models
	if len(models) != 2 || models[0].Name != "legacy-model" || models[0].ContextWindow != 0 {
		t.Fatalf("legacy model decode = %#v", models)
	}
	if models[1].Name != "large-model" || models[1].ContextWindow != 400000 {
		t.Fatalf("object model decode = %#v", models[1])
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), `"models":["legacy-model"`) {
		t.Fatalf("models should be normalized to objects: %s", data)
	}
	if !strings.Contains(string(data), `"name":"legacy-model"`) {
		t.Fatalf("normalized model missing: %s", data)
	}
}

func TestNewModelSetBuildsCodexCompletionModelsForDefaultAndArbiter(t *testing.T) {
	cfg := Config{
		Provider:        "local",
		ModelName:       "gpt-5.3-codex",
		ReasoningEffort: "medium",
		Providers: map[string]ProviderConfig{
			"local": {
				Driver:            "codex_cli",
				Command:           "codex",
				SingleCallTimeout: "2m",
			},
		},
		Roles: map[string]RoleConfig{
			"arbiter": {
				Provider:        "local",
				Model:           "gpt-5.3-codex",
				ReasoningEffort: "high",
				Timeout:         "90s",
			},
		},
	}
	models, err := NewModelSet(cfg)
	if err != nil {
		t.Fatalf("NewModelSet: %v", err)
	}
	defaultModel, ok := models.Default.CurrentModel().(*codexcli.CompletionModel)
	if !ok {
		t.Fatalf("default model type = %T", models.Default.CurrentModel())
	}
	if defaultModel.ModelName() != "gpt-5.3-codex" || defaultModel.SupportsTools() {
		t.Fatalf("default completion model = %#v", defaultModel)
	}
	arbiterModel, ok := models.ForRole("arbiter").(*SwappableModel)
	if !ok {
		t.Fatalf("arbiter wrapper type = %T", models.ForRole("arbiter"))
	}
	arbiterCompletion, ok := arbiterModel.CurrentModel().(*codexcli.CompletionModel)
	if !ok {
		t.Fatalf("arbiter model type = %T", arbiterModel.CurrentModel())
	}
	if got := arbiterCompletion.Timeout(); got != 90*time.Second {
		t.Fatalf("arbiter timeout = %v, want 90s", got)
	}
	dynamic := models.DynamicRoleWithFailover("arbiter", nil)
	timed, ok := dynamic.(interface{ OverallTimeout() time.Duration })
	if !ok {
		t.Fatalf("dynamic Arbiter model dropped OverallTimeout: %T", dynamic)
	}
	if got := timed.OverallTimeout(); got != 90*time.Second {
		t.Fatalf("dynamic Arbiter timeout = %v, want 90s", got)
	}
}

func TestWorkerSelectionSnapshotStaysInternallyConsistentAcrossSwap(t *testing.T) {
	cfg := Config{
		Provider: "first", ModelName: "model-1",
		Providers: map[string]ProviderConfig{
			"first":  {Driver: "codex_cli", Command: "first-codex"},
			"second": {Driver: "codex_cli", Command: "second-codex"},
		},
	}
	models, err := NewModelSet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, provider, model, _ := models.WorkerSelection("writer")
	if err := models.Swap("default", "second", "model-2"); err != nil {
		t.Fatal(err)
	}
	if provider != "first" || model != "model-1" || snapshot.Providers[provider].Command != "first-codex" {
		t.Fatalf("first task snapshot became inconsistent: provider=%q model=%q config=%+v", provider, model, snapshot)
	}
	next, provider, model, _ := models.WorkerSelection("writer")
	if provider != "second" || model != "model-2" || next.Providers[provider].Command != "second-codex" {
		t.Fatalf("next task did not see the atomic swap: provider=%q model=%q config=%+v", provider, model, next)
	}
}

// json_schema 三态：未配置=nil（按 adapter 能力）、true/false=显式声明；
// legacy 字符串条目读入为 nil；写回再读取不得改变三态。
func TestModelConfigJSONSchemaTriState(t *testing.T) {
	var cfg Config
	input := `{"providers":{"custom":{"models":[
    {"name":"a","json_schema":true},
    {"name":"b","json_schema":false},
    {"name":"c"},
    "legacy"
  ]}}}`
	if err := json.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	assertTriState := func(models []ModelConfig, stage string) {
		t.Helper()
		if models[0].JSONSchema == nil || !*models[0].JSONSchema {
			t.Fatalf("%s: a 应为 true, got %v", stage, models[0].JSONSchema)
		}
		if models[1].JSONSchema == nil || *models[1].JSONSchema {
			t.Fatalf("%s: b 应为 false, got %v", stage, models[1].JSONSchema)
		}
		if models[2].JSONSchema != nil || models[3].JSONSchema != nil {
			t.Fatalf("%s: c/legacy 应为 nil, got %v %v", stage, models[2].JSONSchema, models[3].JSONSchema)
		}
	}
	assertTriState(cfg.Providers["custom"].Models, "decode")

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var again Config
	if err := json.Unmarshal(data, &again); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	assertTriState(again.Providers["custom"].Models, "round-trip")

	if v := cfg.ModelJSONSchema("custom", "a"); v == nil || !*v {
		t.Fatalf("ModelJSONSchema(custom,a) = %v", v)
	}
	if v := cfg.ModelJSONSchema("custom", "missing"); v != nil {
		t.Fatalf("未列入模型应为 nil, got %v", v)
	}
	if v := cfg.ModelJSONSchema("nope", "a"); v != nil {
		t.Fatalf("未知 provider 应为 nil, got %v", v)
	}
}

// SwappableModel 的 json_schema 覆盖值必须随热切换原子更新：
// 切到声明不同的模型后，下一次 JSONSchemaOverride 现读即得新事实。
func TestSwappableModelJSONSchemaOverrideFollowsSwap(t *testing.T) {
	tr, fa := true, false
	cfg := Config{
		Provider: "proxy", ModelName: "a",
		Providers: map[string]ProviderConfig{"proxy": {
			Type: "openai", APIKey: "k", BaseURL: "https://example.com/v1",
			Models: []ModelConfig{{Name: "a", JSONSchema: &tr}, {Name: "b", JSONSchema: &fa}, {Name: "c"}},
		}},
	}
	ms, err := NewModelSet(cfg)
	if err != nil {
		t.Fatalf("new model set: %v", err)
	}
	if v := ms.Default.JSONSchemaOverride(); v == nil || !*v {
		t.Fatalf("初始应为 true, got %v", v)
	}
	facts := ms.Default.StructuredOutputFacts()
	if facts.Info.Name != "a" || facts.Info.Provider != "openai" || facts.JSONSchemaOverride == nil || !*facts.JSONSchemaOverride {
		t.Fatalf("初始结构化事实快照不一致: %+v", facts)
	}
	if err := ms.Swap("default", "proxy", "b"); err != nil {
		t.Fatalf("swap b: %v", err)
	}
	if v := ms.Default.JSONSchemaOverride(); v == nil || *v {
		t.Fatalf("切到 b 后应为 false, got %v", v)
	}
	facts = ms.Default.StructuredOutputFacts()
	if facts.Info.Name != "b" || facts.JSONSchemaOverride == nil || *facts.JSONSchemaOverride {
		t.Fatalf("切换后结构化事实快照不一致: %+v", facts)
	}
	if err := ms.Swap("default", "proxy", "c"); err != nil {
		t.Fatalf("swap c: %v", err)
	}
	if v := ms.Default.JSONSchemaOverride(); v != nil {
		t.Fatalf("切到未声明的 c 后应为 nil, got %v", v)
	}
}

func TestResolveContextWindowIsProviderAware(t *testing.T) {
	cfg := Config{
		ContextWindow: 300000,
		Providers: map[string]ProviderConfig{
			"one": {Models: []ModelConfig{{Name: "same", ContextWindow: 128000}}},
			"two": {Models: []ModelConfig{{Name: "same", ContextWindow: 900000}}},
		},
	}
	if got, source := cfg.ResolveContextWindow("one", "same"); got != 128000 || source != CtxWindowModelConfig {
		t.Fatalf("one/same = %d %s", got, source)
	}
	if got, source := cfg.ResolveContextWindow("two", "same"); got != 900000 || source != CtxWindowModelConfig {
		t.Fatalf("two/same = %d %s", got, source)
	}
	if got, source := cfg.ResolveContextWindow("one", "unknown"); got != 300000 || source != CtxWindowConfig {
		t.Fatalf("legacy fallback = %d %s", got, source)
	}
}

func TestSaveProviderConfigPreservesSelectionAndUsesPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".ainovel", "config.json")
	original := Config{
		Provider: "old", ModelName: "old-model", Style: "fantasy",
		Providers: map[string]ProviderConfig{"old": {Type: "openai", Models: []ModelConfig{{Name: "old-model"}}}},
		Budget:    BudgetConfig{BookUSD: 20, WarnRatio: 0.8},
	}
	if err := SaveConfig(path, original); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pc := ProviderConfig{Type: "openai", Models: []ModelConfig{{Name: "new-model", ContextWindow: 500000}}}
	if err := SaveProviderConfig(path, "new", pc); err != nil {
		t.Fatalf("save provider config: %v", err)
	}
	got, err := LoadConfigFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// 只补 providers 段：无关字段与顶层 provider/model 选择必须原样保留。
	if got.Style != "fantasy" || got.Budget.BookUSD != 20 || got.Provider != "old" || got.ModelName != "old-model" {
		t.Fatalf("selection or unrelated fields mutated: %#v", got)
	}
	if _, ok := got.Providers["old"]; !ok {
		t.Fatal("existing provider was removed")
	}
	if got.Providers["new"].Models[0].ContextWindow != 500000 {
		t.Fatalf("new provider not patched in: %#v", got.Providers["new"])
	}
	// 权限断言只在有 POSIX 权限位语义的平台上有意义：Windows 把一切上报为
	// 0666/0444，此断言在该平台恒假（参见 version.TestReplaceExecutable 同款处理）。
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
		}
	}
}
