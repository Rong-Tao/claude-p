# claude-p

> 🌐 [English](README.md) ｜ **中文**（本页）

用**真·交互 session** 驱动 claude 的 `claude -p` 平替，内嵌**透明监测代理**。Go 单文件。

## 安装

```sh
curl -fsSL https://raw.githubusercontent.com/Rong-Tao/claude-p/main/install.sh | bash
```

或从源码：`go build -o claude-p .`（需 Go ≥ 1.21）。运行依赖 `claude` 与 `tmux` 在 PATH。

## 用法

```sh
claude-p "用一句话解释 CAP 定理"
echo "解释这段代码" | claude-p
claude-p -p --output-format json "..."   # claude -p drop-in：JSON 输出 {type,result,session_id,...}
claude-p --model opus --append-system-prompt "你是…" "..."   # 常用 flag 透传给底层 claude
claude-p --resume <session-id> "后续问题"  # 多轮
claude-p --monitor "..."     # 结束后打印本次所有 /v1/messages 调用
```

### 与 claude -p 的兼容

- `-p/--print` 接受并忽略（本就不调 -p）；`--output-format text|json` 映射到 text/`--json`（不支持 stream-json）。
- 交互模式也有效的 flag **原样透传**：`--model`、`--fallback-model`、`--append-system-prompt`、`--system-prompt`、`--add-dir`、`--mcp-config`、`--allowedTools`/`--disallowedTools`/`--tools`、`--permission-mode`、`--agent`、`--effort`、`--session-id`、`--resume`/`--continue`、`--dangerously-skip-permissions` 等（未识别的也透传）。
- `-p`/SDK 专用、对交互无效的 flag（`--input-format`、`--verbose`、`--include-partial-messages`、`--replay-user-messages` 等）**静默忽略**，不会报 “only works with --print”。
- 变长值用逗号形式：`--allowedTools Bash,Read`（避免与 positional prompt 混淆）。
- 完整说明见 `claude-p -h`。

> ⚠️ 灰色地带工具，为 2026-06-15 计费分轨**预判**而建。先读「为什么 / 风险」。

## 为什么

6/15 起 `claude -p` / Agent SDK 用量移出订阅，走独立月度 credit（Pro $20 / Max5x $100 / Max20x $200，用完按 API 原价计费、不滚存；若未开 extra usage 则用完直接停）。**交互式 claude 不受影响。**

目标：做一个 `claude -p` 那样「prompt 进、答案出」的 CLI，但底层跑的是**真·交互 REPL**——6/15 分轨后最可能落在订阅侧的形态。

## 实测得到的关键事实（2026-06-09）

逐一验证、并非推测：

1. **`-p` vs 交互在 HTTP 线缆层逐字节相同**（同 UA `agent-sdk`、同 `x-app=cli`、同 OAuth token、同 body，唯一差异是随机 session-id）。所以**今天两者计费无差别**，分轨判别信号**不在当前请求里**——很可能在 6/15 的新版客户端里才实装（新 header / token scope），或在服务端。
2. **改 `ANTHROPIC_BASE_URL` 不丢订阅**：claude 照样带 `Bearer sk-ant-oat…`（订阅 OAuth）走，透明转发回真 endpoint = 与直连无异，仍按订阅计费。
3. **交互 REPL 不把对话轮次 flush 进 `.jsonl`**（happy 读 transcript 那招在本版失效），但 **Stop hook 的 `last_assistant_message` 字段**给出完整干净回复——这是稳定的输出通道。
4. 每个交互 session ≈ claude node 进程 100MB RSS；终端尺寸随便给，不读屏所以无关。

## 架构

```
claude-p "prompt"
  ├─ 内嵌透明代理 127.0.0.1:<随机端口> → https://api.anthropic.com   （监测所有 call，也是 6/15 判别信号探针）
  ├─ tmux 起真·交互 claude --session-id <uuid> --settings <带 Stop hook>
  │     env: ANTHROPIC_BASE_URL=代理、CLAUDE_CODE_ENTRYPOINT=cli
  ├─ capture-pane 轮询就绪（自动确认 trust 提示）→ send-keys 注入 prompt
  └─ Stop hook（= 本二进制 __hook 模式）把 last_assistant_message 写进 fifo → 主进程读出 → 打印
```

本质 = **无人版的 happy local 模式**（真 REPL + 程序驱动），外加一层透明监测代理。

## flags

| flag | 作用 |
|---|---|
| `--json` | JSON 输出（answer/session_id/calls/models） |
| `--monitor` | 结束后向 stderr 打印代理抓到的所有 `/v1/messages` |
| `--keep` | 保留 tmux session（`tmux attach -t` 调试） |
| `--no-auto-approve` | 关闭工具自动放行（**默认自动放行所有工具**，无人值守用） |
| `--cwd <dir>` | claude 工作目录（默认当前目录） |
| `--timeout <d>` | 等回答超时（默认 240s） |
| 其余 | 透传给 claude，如 `--model opus` |

> **自动放行**：默认通过 PreToolUse hook 放行所有工具调用（否则触发工具的 prompt 会卡在权限弹窗超时）。无人值守方便，但对不可信 prompt 有风险——用 `--no-auto-approve` 关掉。

## 构建

需要 Go ≥ 1.21、`claude`、`tmux`，均在 PATH。

```sh
go build -o claude-p .
install -m755 claude-p ~/.local/bin/
```

## 跨平台

Go 可交叉编译；本代码已去掉 Unix-only 的 `syscall.Mkfifo`（hook→主进程改用 localhost TCP），所以：

| 平台 | 编译 | 运行 |
|---|---|---|
| Linux | ✅ | ✅ |
| macOS | ✅ | ✅（需 `tmux`+`claude`） |
| Windows | ✅（能编） | ❌ 需 tmux（Win 无）；将来要换 ConPTY/PTY 抽象 |

唯一剩下的可移植性障碍是**终端驱动用了 tmux**。

## 多轮

`claude-p --resume <session-id> "后续问题"` 即可续接上文（用 `--json` 拿到上一轮的 `session_id`）。实测可正确回忆上文。

> 前提：spawn 子 claude 前会 **scrub 继承的环境变量**（`CLAUDE_CODE_SESSION_ID`/`CLAUDE_CODE_ENTRYPOINT`/`ANTHROPIC_API_KEY` 等）。否则在「另一个 claude/agent 里调用 claude-p」时，子 claude 会继承父的 session id → 对话写进父 session、自身 transcript 为空、`--resume` 失效、甚至走错计费。这是早期踩过的坑，已修。

## 已知限制 / 脆点

- prompt **以 `/` 开头**会被 REPL 当 slash 命令（已拦截报错）；**多行** prompt 的换行会被当 Enter 提前提交（建议单行）。
- 依赖 `last_assistant_message` 字段名与 REPL 行为，**claude 升级可能破坏**。
- **ToS 灰色**：正是新政策针对的无人监督自动化；6/15 后是否仍算交互、会否被收紧或判违规，**无法保证**。

## 监测代理的高价值用法

当 6/15 的新 claude 落地，**让 `--monitor` 跑着 diff 升级前后的请求**，就能亲眼看到 Anthropic 引入的计费判别信号到底是什么：
- 若是**请求头/UA** → 透明代理改写它即可（灰色地）留订阅档；
- 若是**新 token scope / 服务端** → 客户端无解，老实用 SDK credit。

## 路线图

- [x] **Phase 1**：单发问答 + 内嵌监测代理
- [x] **Phase 2a**：PreToolUse hook 自动放行权限；hook 通道改 TCP（去 Unix 依赖）
- [x] **Phase 2b**：env scrub（隔离父 claude 环境）+ 带值 flag 解析修复 → `--resume` 多轮可用
- [x] **Phase 3**：ACP server（`claude-p acp`）—— 让 Zed / Neovim 等 ACP 客户端把 claude-p 当 agent 接入
- [ ] ACP 增强：逐 token 流式（现为整段回传）、`fs/*` 文件能力、`session/request_permission` 走客户端
- [ ] 代理增强：解析 SSE 响应统计 token / 成本；落盘可回放

## ACP 模式（Phase 3）

`claude-p acp` 启动一个 [Agent Client Protocol](https://agentclientprotocol.com) server（JSON-RPC 2.0 over stdio），
每个 ACP session 背一个常驻交互 claude（订阅档），把编辑器的 prompt 注入、用 Stop hook 取回答案以
`session/update` 流回。这样可绕开「Zed 自带 claude-code-acp 走 Agent SDK（6/15 后计费）」那条路。

已实现：`initialize`、`session/new`、`session/prompt`（同 session 多轮、自动放行工具）、`session/cancel`、
SIGTERM/SIGINT 退出时清理常驻 session。限制：答案整段回传（非逐 token 流式）；暂未实现 `fs/*` 与
向客户端转发 permission 请求（默认本地自动放行）。

Zed 配置（`settings.json`）示例：

```json
{
  "agent_servers": {
    "claude-p": { "command": "claude-p", "args": ["acp"] }
  }
}
```

手动验证：`claude-p acp` 后按行喂 JSON-RPC（`initialize` → `session/new` → `session/prompt`）。

## 技术选型

Go：子进程编排（tmux）+ HTTP 反代 + JSON-RPC/NDJSON，纯 IO 密集，单静态二进制好分发。
