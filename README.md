# claude-p

> 🌐 **English** (this page) ｜ [中文](README_cn.md)

A `claude -p` replacement that drives a **real interactive claude session** instead of headless `-p`, so scripted/agent usage stays on the interactive **subscription** track. Single-file Go. Ships with a transparent monitoring proxy.

> ⚠️ Gray-area tool, built to **anticipate** the 2026-06-15 billing split. Read **Why / Risks** before using.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/Rong-Tao/claude-p/main/install.sh | bash
```

Installs the right prebuilt binary for your OS/arch into `~/.local/bin` (override with `INSTALL_DIR=...`).
Or build from source: `go build -o claude-p .` (Go ≥ 1.21).

**Runtime deps:** `claude` and `tmux` must be on your `PATH`.

## Usage

```sh
claude-p "Explain the CAP theorem in one sentence"
echo "Explain this code" | claude-p
claude-p -p --output-format json "..."   # claude -p drop-in: JSON output {type,result,session_id,...}
claude-p --model opus --append-system-prompt "You are..." "..."   # common claude flags pass through
claude-p --resume <session-id> "follow-up question"   # multi-turn
claude-p --monitor "..."   # print all /v1/messages calls made during the run
```

## Why

On 2026-06-15, `claude -p` / Agent SDK usage moves out of the subscription pool into a separate
monthly credit (Pro $20 / Max5x $100 / Max20x $200, billed at API rates, no rollover; once spent,
automation stops unless extra usage is enabled). **Interactive claude is unaffected.**

Goal: a `claude -p`-style CLI (prompt in, answer out) whose underlying process is a **real interactive
REPL** — the form most likely to land on the subscription side after the split.

## How it works

```
claude-p "prompt"
  ├─ embedded transparent proxy 127.0.0.1:<port> → https://api.anthropic.com   (monitors every call)
  ├─ tmux runs a real interactive `claude --session-id <uuid> --settings <hooks>`
  │     (inherited CLAUDE_*/ANTHROPIC_API_KEY are scrubbed so it's a clean subscription session)
  ├─ poll capture-pane until ready (auto-confirms the trust prompt) → send-keys the prompt
  └─ a Stop hook (this binary in __hook mode) sends the clean `last_assistant_message`
     back over localhost TCP → printed to stdout
```

Essentially an unattended version of [happy](https://github.com/slopus/happy)'s local mode (real REPL,
program-driven), plus a transparent monitoring proxy.

### Verified facts (the design rests on these, not guesses)

- `-p` and interactive requests are **byte-identical on the wire** (same `agent-sdk` UA, same OAuth
  token, same body) — so today there is no billing difference, and the split is not visible in the
  request. The proxy is the instrument to catch whatever discriminator 06-15 introduces.
- Setting `ANTHROPIC_BASE_URL` does **not** drop the subscription: claude still sends its OAuth token,
  so a transparent forward to the real endpoint is billed as subscription.
- The interactive REPL doesn't flush turns to the project transcript reliably, but the **Stop hook's
  `last_assistant_message`** gives a clean, complete reply — the stable output channel.

## ACP mode (use it from Zed / Neovim)

`claude-p acp` starts an [Agent Client Protocol](https://agentclientprotocol.com) server (JSON-RPC 2.0
over stdio). Each ACP session is backed by a persistent interactive claude (subscription), so it
sidesteps the path where Zed's bundled `claude-code-acp` uses the Agent SDK (metered after 06-15).

Implemented: `initialize`, `session/new`, `session/prompt` (multi-turn within a session, tools
auto-approved), `session/cancel`, and cleanup of live sessions on SIGTERM/SIGINT.
Limitations: the reply is sent as one chunk at turn end (not token-streamed); `fs/*` and forwarding
permission requests to the client are not yet implemented (tools are auto-approved locally).

Zed `settings.json`:

```json
{
  "agent_servers": {
    "claude-p": { "command": "claude-p", "args": ["acp"] }
  }
}
```

## Flags

| flag | meaning |
|---|---|
| `--json` | JSON output (claude -p-like: `{type,result,session_id,...}`) |
| `--monitor` | after the run, print every `/v1/messages` call seen by the proxy |
| `--keep` | keep the tmux session (`tmux attach -t ...` to debug) |
| `--no-auto-approve` | disable tool auto-approval (**on by default** for unattended use) |
| `--cwd <dir>` | working directory for claude (default: current) |
| `--timeout <d>` | wait-for-answer timeout (default 240s, e.g. `90s` / `5m`) |
| others | passed through to claude: `--model`, `--append-system-prompt`, `--allowedTools`, `--permission-mode`, `--resume`/`-r`, `--continue`/`-c`, `--dangerously-skip-permissions`, ... |

`claude -p` compatibility: `-p/--print` accepted and ignored; `--output-format text\|json` mapped to
text/`--json` (stream-json not supported); `-p`/SDK-only flags (`--input-format`, `--verbose`,
`--include-partial-messages`, ...) are silently ignored so they don't error. Run `claude-p -h` for the
full grouped list. Use comma form for multi-value flags (`--allowedTools Bash,Read`).

## Cross-platform

| OS | build | run |
|---|---|---|
| Linux | ✅ | ✅ |
| macOS | ✅ | ✅ (needs `tmux` + `claude`) |
| Windows | ✅ (compiles) | ❌ needs tmux (use WSL) |

The only portability blocker is the tmux-based terminal driver.

## Risks / limitations

- **ToS gray area**: this is exactly the kind of unattended automation the new policy targets; whether
  it stays on subscription after 06-15, and whether it's allowed, is **not guaranteed**.
- Auto-approve runs tools without asking — be careful with untrusted prompts (`--no-auto-approve` off).
- Depends on the `last_assistant_message` field and REPL behavior; a claude update may break it.
- Prompts starting with `/` (slash commands) are rejected; multi-line prompts are best avoided.

## License

MIT — see [LICENSE](LICENSE).
