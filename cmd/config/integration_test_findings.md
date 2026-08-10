# Integration Test Findings — `cmd/config/`

## Integration Architecture

Two interfaces, five integrations:

| Integration | Runner | Editor | Binary | Config files |
|---|---|---|---|---|
| **Claude** | yes | no | `claude` | none (env vars only) |
| **Codex** | yes | no | `codex` | none (uses `--oss` flag) |
| **Clawdbot** | yes | yes | `clawdbot` | `~/.clawdbot/clawdbot.json` |
| **Droid** | yes | yes | `droid` | `~/.factory/settings.json` |
| **OpenCode** | yes | yes | `opencode` | `~/.config/opencode/opencode.json` + `~/.local/state/opencode/model.json` |

## Current Test Coverage

Existing tests are **config-only** — they cover `Edit()`, `Models()`, `Paths()`, `args()`, JSON round-trips, and edge cases (corrupted JSON, wrong types, unicode, idempotency, backups). This is thorough for the Editor side.

**Zero coverage on `Run()`** — every test explicitly skips it with comments like "we can't call it without actually running the command."

## How to do in-depth E2E testing with an agent in yolo mode

### Tier 1: Mock binary tests (no external tools needed)

For each integration, create fake binaries (shell scripts) in a temp `$PATH` that:

- **Assert correct args** — e.g. Claude gets `--model llama3.2`, Codex gets `--oss -m llama3.2`
- **Assert correct env vars** — Claude must have `ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY=""`, `ANTHROPIC_AUTH_TOKEN=ollama`
- **Assert correct version output** — mock `codex --version` returning various versions to test `checkCodexVersion()`
- **Exit 0 or 1** to test error propagation

This is the most valuable gap to fill. The agent writes Go test functions that create temp dirs with mock scripts, set `$PATH`, and call `Run()`.

### Tier 2: Real binary E2E (needs tools installed + Ollama running)

For each integration, the agent:

1. **Backs up** existing config files
2. **Calls `Edit()`** with a real model name like `llama3.2:1b`
3. **Launches the binary** in non-interactive mode (where possible):
   - `claude --model llama3.2:1b -p "say hi"` with Ollama env vars
   - `codex --oss -m llama3.2:1b` (needs a way to send input and exit)
   - `opencode`, `droid`, `clawdbot` — these are TUI apps, harder to drive non-interactively
4. **Verifies** the tool connected to Ollama (check Ollama server logs for requests)
5. **Restores** backups

The agent-runnable ones are **Claude** (has `-p` print mode) and **Codex** (has `--quiet`). The TUI-based ones (OpenCode, Droid, Clawdbot) need either:

- A **CUA** (Computer Use Agent) to visually drive the TUI — screenshot, send keystrokes, verify output
- Or a **pty wrapper** that scripts stdin/stdout interactions

### Tier 3: CUA-driven TUI testing

A CUA agent (Anthropic computer use or OpenAI Operator) could:

- Run `ollama launch droid` in a terminal
- See the TUI selector, arrow-key to a model, press Enter
- Verify Droid launches and shows the chat interface
- Type a prompt, verify a response comes back from Ollama
- Screenshot-assert the UI state at each step

This is the only way to test the **full interactive flow** (the selector TUI in `selector.go`, confirm prompts, multi-select, sign-in flow).

### Tier 4: Fuzz testing

An agent could fuzz:

- **`Edit()` inputs** — random model names (empty strings, 10KB strings, null bytes, path traversal strings like `../../etc/passwd`)
- **Config file inputs** — feed random/adversarial JSON to each integration's config path before calling `Edit()`, verify no panics and valid JSON output
- **`checkCodexVersion()`** — mock `codex --version` returning garbage, empty output, very old versions, future versions
- **`findPath()`** — symlinks, broken symlinks, directories named `claude`, read-only paths

## Agent Capability Matrix

| Test type | Claude | Codex | Clawdbot | Droid | OpenCode |
|---|---|---|---|---|---|
| Mock binary `Run()` | yes | yes | yes | yes | yes |
| Real binary `-p` mode | **yes** | partial | no (TUI) | no (TUI) | no (TUI) |
| Config `Edit()` fuzz | n/a | n/a | **yes** | **yes** | **yes** |
| CUA TUI testing | needs CUA | needs CUA | needs CUA | needs CUA | needs CUA |

## Recommendation

Highest-value work is **Tier 1** (mock binary tests for `Run()`) — covers the biggest gap with no external dependencies and can be written as standard Go tests.
