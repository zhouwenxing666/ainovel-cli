---
name: install-ainovel-cli-byhost
description: Safely replace the ainovel-cli installed on this macOS host with a binary built from the latest local state of this repository, including uncommitted source changes. Use when the user asks to uninstall the current local ainovel-cli and reinstall, rebuild, refresh, or test the host command from this checkout, especially for the local Codex CLI backend.
---

# Install ainovel-cli from the host checkout

Reinstall the host command from this repository while preserving user configuration and novel data. Use the bundled script for the destructive and build steps.

## Safety contract

- Require explicit user authorization to replace the installed command.
- Operate only in the repository containing module `github.com/voocel/ainovel-cli`.
- Treat `/usr/local/bin/ainovel-cli` as the only supported install target.
- Refuse to remove a command resolved from any other path. Report it for manual handling instead.
- Never delete or alter `~/.ainovel`, novel projects, `~/.codex`, Codex authentication, or repository source changes.
- Build the current working tree, including uncommitted changes. Do not use `go install ...@latest`, a GitHub Release, or `scripts/install.sh`.
- Explain that macOS may show administrator dialogs for the exact uninstall and install operations. Never request or handle the user's password directly.

## Workflow

1. From the repository root, inspect without changing state:

   ```bash
   git status --short --branch
   ./.codex/skills/install-ainovel-cli-byhost/scripts/reinstall-local.sh --check
   ```

2. Tell the user the detected installed path and version. State that `~/.ainovel` and project data will be preserved.

3. Run the bundled installer from the repository root:

   ```bash
   ./.codex/skills/install-ainovel-cli-byhost/scripts/reinstall-local.sh
   ```

   The script performs this fixed sequence:

   - validate the repository, platform, dependencies, and existing command path;
   - uninstall only `/usr/local/bin/ainovel-cli`;
   - confirm no old command remains;
   - run `go test ./...`;
   - run the real, quota-free Codex CLI preflight;
   - build an arm64 or amd64 macOS binary from the current working tree with local version metadata;
   - install it to `/usr/local/bin/ainovel-cli`;
   - verify command resolution, version output, and SHA-256 equality;
   - clean the temporary build artifact.

4. If the script is waiting, tell the user to check the macOS administrator dialog and continue waiting. The authorization applies only to the exact target path.

5. Report the installed path, local version, commit, test result, Codex preflight result, and checksum verification. Remind the user to start it with `ainovel-cli`.

## Failure handling

- If another install path is detected, stop. Do not delete it or silently install a second copy.
- If tests, Codex preflight, or compilation fail after uninstall, leave the failure visible and do not install an unverified binary.
- If administrator authorization is cancelled, report that the requested replacement is incomplete.
- Do not repair source, configuration, Codex login, or unrelated toolchain problems unless the user separately authorizes that work.
