package bootstrap

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

const legacyReviewerConfig = `{
  "provider": "openrouter", "model": "test-model",
  "providers": {
    "openrouter": {"api_key": "test-key"},
    "unused-codex": {"driver": "codex_cli", "command": "/missing/obsolete-reviewer-codex"}
  },
  "roles": {
    "reviewer": {
      "provider": "unused-codex", "model": "obsolete-model", "timeout": "invalid",
      "fallbacks": [{"provider": "removed-provider", "model": "obsolete-model"}]
    },
    "writer": {"provider": "openrouter", "model": "writer-global"}
  }
}`

func assertLegacyReviewerIgnored(t *testing.T, cfg Config) {
	t.Helper()
	if _, exists := cfg.Roles["reviewer"]; exists {
		t.Fatal("legacy reviewer role remains active")
	}
	if err := cfg.ValidateBase(); err != nil {
		t.Fatalf("obsolete reviewer settings blocked validation: %v", err)
	}
	if err := PreflightCodexProviders(context.Background(), cfg); err != nil {
		t.Fatalf("unused reviewer provider was preflighted: %v", err)
	}
	if _, err := NewModelSet(cfg); err != nil {
		t.Fatalf("obsolete reviewer settings blocked model initialization: %v", err)
	}
}

func TestLoadConfigIgnoresLegacyReviewerBeforeAndAfterMerge(t *testing.T) {
	writeGlobal(t, legacyReviewerConfig)
	t.Chdir(t.TempDir())
	writeProjectConfig(t, `{"roles": {
    "reviewer": {"provider": "missing-provider", "model": "unused"},
    "writer": {"provider": "openrouter", "model": "writer-project"}
  }}`)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	assertLegacyReviewerIgnored(t, cfg)
	if cfg.Roles["writer"].Model != "writer-project" {
		t.Fatal("writer project override lost")
	}
}

func TestLoadConfigFileIgnoresLegacyReviewer(t *testing.T) {
	dir := writeGlobal(t, legacyReviewerConfig)
	cfg, err := LoadConfigFile(filepath.Join(dir, ".ainovel", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	assertLegacyReviewerIgnored(t, cfg)
	if cfg.Roles["writer"].Model != "writer-global" {
		t.Fatal("writer config lost")
	}
}

func TestFillDefaultsIgnoresLegacyReviewer(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(legacyReviewerConfig), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.FillDefaults()
	assertLegacyReviewerIgnored(t, cfg)
}
