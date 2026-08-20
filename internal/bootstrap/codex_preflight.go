package bootstrap

import (
	"context"
	"fmt"
	"sort"

	"github.com/voocel/ainovel-cli/internal/codexcli"
)

// PreflightCodexProvider verifies one trusted process-backed provider without
// making a model request or modifying the user's Codex configuration.
func PreflightCodexProvider(ctx context.Context, name string, provider ProviderConfig) error {
	if !provider.IsCodexCLI() {
		return nil
	}
	runtime := codexcli.NewRuntime(codexcli.RuntimeConfig{
		Command: provider.Command, CodexHome: provider.CodexHome,
	})
	if err := runtime.Preflight(ctx); err != nil {
		return fmt.Errorf("codex_cli provider %q preflight: %w", name, err)
	}
	return nil
}

// PreflightCodexProviders checks every configured Codex process boundary at
// startup. Stable ordering keeps diagnostics deterministic.
func PreflightCodexProviders(ctx context.Context, cfg Config) error {
	referenced := map[string]bool{cfg.Provider: true}
	for _, role := range cfg.Roles {
		referenced[role.Provider] = true
		for _, fallback := range role.Fallbacks {
			referenced[fallback.Provider] = true
		}
	}
	names := make([]string, 0, len(referenced))
	for name := range referenced {
		if provider, ok := cfg.Providers[name]; ok && provider.IsCodexCLI() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if err := PreflightCodexProvider(ctx, name, cfg.Providers[name]); err != nil {
			return err
		}
	}
	return nil
}
