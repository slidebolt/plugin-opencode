# Git Workflow for plugin-opencode

This repository contains the Slidebolt OpenCode Plugin. It translates between `opencode serve` and Slidebolt: each configured agent is a device, and scripts send it prompts and react to its results. It holds no behavior of its own. It produces a standalone binary.

## Dependencies
- **Internal:** `sb-contract`, `sb-domain`, `sb-messenger-sdk`, `sb-runtime`, `sb-storage-sdk`, `sb-testkit`.
- **Runtime:** an `opencode serve` instance, reached through `OPENCODE_HOST_URL` and `OPENCODE_HOST_PASSWORD`. See `opencode-host`.

## Build Process
- **Type:** Go Application (Plugin).
- **Command:** `go build -o plugin-opencode ./cmd/plugin-opencode`
- **Validation:** `go test ./...`. The real server is covered by the `voice` suite in `sb-e2e`.
