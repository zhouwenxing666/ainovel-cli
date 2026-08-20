package bootstrap

import (
	"errors"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/errs"
	"github.com/voocel/ainovel-cli/internal/notify"
)

func TestNewDefaultCodexConfigUsesProductDefaults(t *testing.T) {
	cfg := NewDefaultCodexConfig("local-codex", ProviderConfig{
		Driver:  "codex_cli",
		Command: "codex",
	})

	if cfg.ModelName != "gpt-5.6-sol" {
		t.Fatalf("default Codex model = %q, want gpt-5.6-sol", cfg.ModelName)
	}
	if cfg.ReasoningEffort != "xhigh" {
		t.Fatalf("default Codex reasoning effort = %q, want xhigh", cfg.ReasoningEffort)
	}
	models := cfg.Providers["local-codex"].Models
	if len(models) != 1 || models[0].Name != "gpt-5.6-sol" {
		t.Fatalf("default Codex models = %#v, want gpt-5.6-sol", models)
	}
}

func TestConfigResolveReasoningEffort(t *testing.T) {
	cfg := Config{
		ReasoningEffort: "low", // 顶层默认
		Roles: map[string]RoleConfig{
			"writer":    {Provider: "p", Model: "m", ReasoningEffort: "high"}, // 角色覆盖
			"architect": {Provider: "p", Model: "m"},                          // 无 reasoning_effort，应回落默认
		},
	}

	cases := []struct {
		role string
		want string
	}{
		{"writer", "high"},   // 角色覆盖优先
		{"architect", "low"}, // 角色未配 → 回落顶层默认
		{"editor", "low"},    // 角色不存在 → 顶层默认
		{"", "low"},          // 空 → 顶层默认
		{"default", "low"},   // default → 顶层默认
		{"arbiter", "low"},   // 非配置角色（裁定恒随顶层默认）
	}
	for _, c := range cases {
		if got := cfg.ResolveReasoningEffort(c.role); got != c.want {
			t.Errorf("ResolveReasoningEffort(%q) = %q, want %q", c.role, got, c.want)
		}
	}

	// 顶层默认也为空时，未覆盖角色返回 ""（不覆盖）。
	empty := Config{Roles: map[string]RoleConfig{"writer": {ReasoningEffort: "xhigh"}}}
	if got := empty.ResolveReasoningEffort("editor"); got != "" {
		t.Errorf("空默认下 editor 应返回 \"\"，得 %q", got)
	}
	if got := empty.ResolveReasoningEffort("writer"); got != "xhigh" {
		t.Errorf("空默认下 writer 覆盖应生效，得 %q", got)
	}
}

func TestValidateBaseRejectsNonConfigurableRoles(t *testing.T) {
	cfg := Config{
		Provider:  "openrouter",
		ModelName: "test-model",
		Providers: map[string]ProviderConfig{
			"openrouter": {APIKey: "sk-test-123456"},
		},
		Roles: map[string]RoleConfig{
			"coordinator": {Provider: "openrouter", Model: "test-model"},
		},
	}

	err := cfg.ValidateBase()
	if err == nil {
		t.Fatal("roles.coordinator 应被拒绝")
	}
	if !errors.Is(err, errs.ErrConfig) {
		t.Fatalf("应包装 errs.ErrConfig，得到: %v", err)
	}
}

func TestValidateBaseAcceptsCodexCLIAndArbiterRole(t *testing.T) {
	cfg := Config{
		Provider:  "local-codex",
		ModelName: "gpt-5.3-codex",
		Providers: map[string]ProviderConfig{
			"local-codex": {
				Driver:  "codex_cli",
				Command: "/opt/homebrew/bin/codex",
			},
		},
		Roles: map[string]RoleConfig{
			"arbiter": {
				Provider: "local-codex",
				Model:    "gpt-5.3-codex",
			},
		},
	}

	if err := cfg.ValidateBase(); err != nil {
		t.Fatalf("codex_cli 不应要求 api_key，且 roles.arbiter 应合法: %v", err)
	}
}

func TestCodexCLIProviderTimeoutsAndValidation(t *testing.T) {
	pc := ProviderConfig{Driver: "codex_cli", Command: "codex"}
	if got, err := pc.SingleCallTimeoutValue(); err != nil || got != 3*time.Minute {
		t.Fatalf("single-call default = (%v, %v), want 3m", got, err)
	}
	if got, err := pc.WorkerTimeoutValue(); err != nil || got != 30*time.Minute {
		t.Fatalf("worker default = (%v, %v), want 30m", got, err)
	}

	pc.SingleCallTimeout = "45s"
	pc.WorkerTimeout = "12m"
	if got, err := pc.SingleCallTimeoutValue(); err != nil || got != 45*time.Second {
		t.Fatalf("single-call override = (%v, %v), want 45s", got, err)
	}
	if got, err := pc.WorkerTimeoutValue(); err != nil || got != 12*time.Minute {
		t.Fatalf("worker override = (%v, %v), want 12m", got, err)
	}

	invalid := []ProviderConfig{
		{Driver: "shell", Command: "codex"},
		{Driver: "codex_cli"},
		{Driver: "codex_cli", Command: "codex", APIKey: "must-not-be-used"},
		{Driver: "codex_cli", Command: "codex", BaseURL: "https://example.com"},
		{Driver: "codex_cli", Command: "codex", Extra: map[string]any{"env": "unsafe"}},
		{Driver: "codex_cli", Command: "codex", SingleCallTimeout: "soon"},
		{Driver: "codex_cli", Command: "codex", WorkerTimeout: "0"},
		{Driver: "codex_cli", Command: "./codex"},
		{Driver: "codex_cli", Command: "codex", CodexHome: ".codex"},
	}
	for i, provider := range invalid {
		cfg := Config{
			Provider:  "local",
			ModelName: "gpt-5.3-codex",
			Providers: map[string]ProviderConfig{"local": provider},
		}
		if err := cfg.ValidateBase(); !errors.Is(err, errs.ErrConfig) {
			t.Errorf("invalid provider[%d] 应返回配置错误，得到 %v", i, err)
		}
	}
}

func TestValidateBaseAcceptsRoleTimeout(t *testing.T) {
	cfg := Config{
		Provider:  "local",
		ModelName: "gpt-5.3-codex",
		Providers: map[string]ProviderConfig{
			"local": {Driver: "codex_cli", Command: "codex"},
		},
		Roles: map[string]RoleConfig{
			"writer": {Provider: "local", Model: "gpt-5.3-codex", Timeout: "20m"},
		},
	}
	if err := cfg.ValidateBase(); err != nil {
		t.Fatalf("合法 role timeout 应通过: %v", err)
	}
	cfg.Roles["writer"] = RoleConfig{Provider: "local", Model: "gpt-5.3-codex", Timeout: "forever"}
	if err := cfg.ValidateBase(); !errors.Is(err, errs.ErrConfig) {
		t.Fatalf("非法 role timeout 应返回配置错误，得到 %v", err)
	}
}

func TestValidateBaseNotifyEventsMatchRuntimeContract(t *testing.T) {
	validConfig := func(events []string) Config {
		return Config{
			Provider:  "openrouter",
			ModelName: "test-model",
			Providers: map[string]ProviderConfig{
				"openrouter": {APIKey: "sk-test-123456"},
			},
			Notify: NotifyConfig{Events: events},
		}
	}

	cfg := validConfig(notify.Kinds())
	if err := cfg.ValidateBase(); err != nil {
		t.Fatalf("当前通知事件契约应全部通过配置校验: %v", err)
	}

	cfg = validConfig([]string{"repeat"})
	if err := cfg.ValidateBase(); !errors.Is(err, errs.ErrConfig) {
		t.Fatalf("旧 repeat 事件应被拒绝，得到: %v", err)
	}
}

func TestProviderStreamIdleTimeoutValue(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", defaultStreamIdleTimeout, false},
		{"900s", 15 * time.Minute, false},
		{"15m", 15 * time.Minute, false},
		{"abc", 0, true},
		{"-5s", 0, true},
		{"0", 0, true}, // 不提供"关闭看门狗"——真死流需要有限界
	}
	for _, c := range cases {
		got, err := ProviderConfig{StreamIdleTimeout: c.in}.StreamIdleTimeoutValue()
		if c.wantErr {
			if err == nil {
				t.Errorf("%q 应报错", c.in)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q = (%v, %v), want %v", c.in, got, err, c.want)
		}
	}
}

func TestValidateBaseRejectsBadStreamIdleTimeout(t *testing.T) {
	cfg := Config{
		Provider:  "openrouter",
		ModelName: "test-model",
		Providers: map[string]ProviderConfig{
			"openrouter": {APIKey: "sk-test-123456", StreamIdleTimeout: "fast"},
		},
	}
	if err := cfg.ValidateBase(); !errors.Is(err, errs.ErrConfig) {
		t.Fatalf("非法 stream_idle_timeout 应拒绝并包装 ErrConfig，得到: %v", err)
	}
}
