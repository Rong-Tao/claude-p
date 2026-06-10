// claude-p —— 用「真·交互 session」驱动 claude 的 `claude -p` 平替，内嵌透明监测代理。
//
// 设计（详见 README）：
//   - 内嵌透明代理：ANTHROPIC_BASE_URL 指向本地代理 → 转发 api.anthropic.com，
//     原样保留订阅 OAuth token，仅用于「监测本次所有 call」（也是 6/15 计费分轨的探针）。
//   - tmux 起真·交互 claude（不带 -p），带 --session-id 与一份含 Stop / PreToolUse hook 的 --settings。
//   - 输入经 tmux send-keys 注入；输出靠 Stop hook 的 last_assistant_message，
//     经 localhost TCP（非 FIFO，便于将来跨平台）回传主进程。
//
// 同一个二进制有三种模式：
//
//	claude-p "prompt"            正常一次问答
//	claude-p __hook <port>       作为 claude 的 Stop hook（内部用），把答案发回 TCP
//	claude-p __approve           作为 PreToolUse hook（内部用），自动放行工具
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

func main() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "__hook":
			hookMain(os.Args[2:])
			return
		case "__approve":
			approveMain()
			return
		case "acp":
			acpMain()
			return
		}
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "claude-p: "+err.Error())
		os.Exit(1)
	}
}

// ---------- hook 模式 ----------

// Stop hook：claude 每轮结束时调用，stdin 收到含 last_assistant_message 的 JSON，
// 我们拨号回主进程的 TCP 端口把答案送回。
func hookMain(args []string) {
	if len(args) < 1 {
		return
	}
	in, _ := io.ReadAll(os.Stdin)
	var p struct {
		LastAssistantMessage string `json:"last_assistant_message"`
		SessionID            string `json:"session_id"`
	}
	_ = json.Unmarshal(in, &p)
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+args[0], 5*time.Second)
	if err != nil {
		return
	}
	defer c.Close()
	out, _ := json.Marshal(map[string]string{"answer": p.LastAssistantMessage, "session_id": p.SessionID})
	c.Write(append(out, '\n'))
}

// PreToolUse hook：自动放行所有工具调用（无人值守用）。
func approveMain() {
	io.Copy(io.Discard, os.Stdin)
	fmt.Println(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","permissionDecisionReason":"claude-p auto-approve"}}`)
}

// ---------- 正常模式 ----------

type options struct {
	prompt        string
	jsonOut       bool
	monitor       bool
	keep          bool
	noAutoApprove bool
	cwd           string
	timeout       time.Duration
	passthrough   []string
}

func run(argv []string) error {
	opt, err := parse(argv)
	if err != nil {
		return err
	}
	if opt.prompt == "" && isPipe(os.Stdin) {
		b, _ := io.ReadAll(os.Stdin)
		opt.prompt = strings.TrimRight(string(b), "\n")
	}
	if strings.TrimSpace(opt.prompt) == "" {
		return fmt.Errorf("没有 prompt。用法：claude-p [flags] \"你的问题\"  或  echo ... | claude-p")
	}
	if strings.HasPrefix(strings.TrimSpace(opt.prompt), "/") {
		return fmt.Errorf("prompt 以 / 开头会被交互 REPL 当成 slash 命令；请改写或加前缀空格")
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}

	ls, err := newLiveSession(self, sessionConfig{
		cwd:         opt.cwd,
		passthrough: opt.passthrough,
		autoApprove: !opt.noAutoApprove,
		useProxy:    true,
	})
	if err != nil {
		return err
	}
	if opt.keep {
		// 保留 tmux session 供调试；进程退出时只关代理/listener，不杀 session
		defer func() {
			fmt.Fprintf(os.Stderr, "[claude-p] 保留 tmux session：tmux attach -t %s\n", ls.sess)
			if ls.prox != nil {
				ls.prox.stop()
			}
			ls.ln.Close()
		}()
	} else {
		defer ls.close()
	}

	ans, err := ls.prompt(opt.prompt, opt.timeout)
	if err != nil {
		return err
	}

	if opt.jsonOut {
		// 贴近 claude -p --output-format json 的字段（result/session_id），claude_p 下放自有信息
		out, _ := json.Marshal(map[string]any{
			"type":       "result",
			"subtype":    "success",
			"is_error":   false,
			"result":     ans.Answer,
			"session_id": ans.SessionID,
			"claude_p":   map[string]any{"calls": ls.prox.count(), "models": ls.prox.models()},
		})
		fmt.Println(string(out))
	} else {
		fmt.Println(ans.Answer)
	}
	if opt.monitor {
		ls.prox.report(os.Stderr)
	}
	return nil
}

// 从父进程继承、会污染子 claude 的环境变量（典型场景：在另一个 claude/agent 里调用 claude-p）。
// 不 scrub 的话：子 claude 会继承父的 CLAUDE_CODE_SESSION_ID → 对话写进父 session、自身 transcript 为空、
// --resume 失效；CLAUDE_CODE_ENTRYPOINT=sdk-ts → 被当 SDK；ANTHROPIC_API_KEY → 可能走 API 计费而非订阅。
var scrubEnv = []string{
	"CLAUDE_CODE_SESSION_ID",
	"CLAUDECODE",
	"CLAUDE_CODE_ENTRYPOINT",
	"CLAUDE_AGENT_SDK_VERSION",
	"CLAUDE_CODE_EXECPATH",
	"CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS",
	"ANTHROPIC_API_KEY",
}

func buildClaudeCmd(sid, settings string, passthrough []string) string {
	parts := []string{"claude", "--settings", settings}
	// 用户自带 --resume/--continue/--session-id 时不再强加我们的 --session-id（冲突）
	if !hasAny(passthrough, "--resume", "-r", "--continue", "-c", "--session-id") {
		parts = append(parts, "--session-id", sid)
	}
	parts = append(parts, passthrough...)
	q := make([]string, len(parts))
	for i, p := range parts {
		q[i] = "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
	}
	// 用 env -u 剥掉污染变量，保证子 claude 是干净的交互 session
	env := "env"
	for _, v := range scrubEnv {
		env += " -u " + v
	}
	return env + " " + strings.Join(q, " ")
}

// waitReady 轮询 capture-pane，直到 REPL 就绪；遇到 trust 提示自动确认。
func waitReady(sess string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	trusted := false
	for time.Now().Before(deadline) {
		// claude 启动后立即退出（如 --resume 一个不存在/无法恢复的 session）→ 快速报错
		if exec.Command("tmux", "has-session", "-t", sess).Run() != nil {
			return fmt.Errorf("claude 启动后立即退出（--resume 多轮本版本不支持：交互 REPL 不持久化对话，无法恢复）")
		}
		out, _ := exec.Command("tmux", "capture-pane", "-t", sess, "-p").Output()
		pane := string(out)
		if !trusted && (strings.Contains(pane, "trust this folder") || strings.Contains(pane, "Yes, I trust")) {
			exec.Command("tmux", "send-keys", "-t", sess, "Enter").Run() // 默认选项 1 = 信任
			trusted = true
			time.Sleep(2 * time.Second)
			continue
		}
		if strings.Contains(pane, "? for shortcuts") || strings.Contains(pane, "for agents") {
			return nil
		}
		time.Sleep(800 * time.Millisecond)
	}
	return fmt.Errorf("等待 claude REPL 就绪超时")
}

type answer struct {
	Answer    string `json:"answer"`
	SessionID string `json:"session_id"`
}

func writeSettings(path, stopCmd, approveCmd string, autoApprove bool) error {
	hooks := map[string]any{
		"Stop": []any{map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": stopCmd}},
		}},
	}
	if autoApprove {
		hooks["PreToolUse"] = []any{map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": approveCmd}},
		}}
	}
	b, _ := json.Marshal(map[string]any{"hooks": hooks})
	return os.WriteFile(path, b, 0o600)
}

// ---------- 监测代理 ----------

type proxy struct {
	port string
	srv  *http.Server
	mu   sync.Mutex
	reqs []proxReq
}

type proxReq struct {
	Path  string
	Model string
}

func startProxy() (*proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	target, _ := url.Parse("https://api.anthropic.com")
	p := &proxy{port: fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = -1
	orig := rp.Director
	rp.Director = func(r *http.Request) {
		orig(r)
		r.Host = target.Host
		if strings.HasPrefix(r.URL.Path, "/v1/messages") {
			model := ""
			if r.Body != nil {
				body, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(body))
				var m struct {
					Model string `json:"model"`
				}
				json.Unmarshal(body, &m)
				model = m.Model
			}
			p.mu.Lock()
			p.reqs = append(p.reqs, proxReq{Path: r.URL.Path, Model: model})
			p.mu.Unlock()
		}
	}
	rp.ErrorLog = log.New(io.Discard, "", 0) // 清理时关连接的 context-canceled 噪音
	p.srv = &http.Server{Handler: rp, ErrorLog: log.New(io.Discard, "", 0)}
	go p.srv.Serve(ln)
	return p, nil
}

func (p *proxy) stop()      { p.srv.Close() }
func (p *proxy) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.reqs) }
func (p *proxy) models() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, r := range p.reqs {
		if r.Model != "" && !seen[r.Model] {
			seen[r.Model] = true
			out = append(out, r.Model)
		}
	}
	return out
}
func (p *proxy) report(w io.Writer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(w, "[claude-p monitor] %d 次 /v1/messages 调用\n", len(p.reqs))
	for i, r := range p.reqs {
		fmt.Fprintf(w, "  #%d %s model=%s\n", i+1, r.Path, r.Model)
	}
}

// ---------- 杂项 ----------

// 透传给 claude 的带值 flag（其后紧跟一个值，不能被误当 prompt）。
// 注：claude 里写作 <x...> 的变长 flag（如 --allowedTools），这里按「吃一个值」处理，
// 多值请用逗号形式（--allowedTools Bash,Read），避免与 positional prompt 混淆。
var valueFlags = map[string]bool{
	"--model": true, "--fallback-model": true,
	"--append-system-prompt": true, "--system-prompt": true,
	"--add-dir": true, "--mcp-config": true,
	"--allowedTools": true, "--allowed-tools": true,
	"--disallowedTools": true, "--disallowed-tools": true, "--tools": true,
	"--permission-mode": true, "--resume": true, "-r": true,
	"--session-id": true, "--agent": true, "--effort": true,
}

// claude -p / SDK 专用、对交互 REPL 无效或会报错的 flag —— 拦下丢弃。
var ignoreBoolFlags = map[string]bool{
	"--include-partial-messages": true, "--replay-user-messages": true,
	"--include-hook-events": true, "--verbose": true,
}
var ignoreValueFlags = map[string]bool{
	"--input-format": true,
}

func parse(argv []string) (options, error) {
	o := options{timeout: 240 * time.Second}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "-h" || a == "--help":
			printHelp()
			os.Exit(0)
		case a == "--json":
			o.jsonOut = true
		case a == "--output-format" || strings.HasPrefix(a, "--output-format="):
			// claude -p 的 --output-format：映射到我们的 text/json（拦截，不透传）
			var v string
			if strings.HasPrefix(a, "--output-format=") {
				v = strings.TrimPrefix(a, "--output-format=")
			} else if i+1 < len(argv) {
				i++
				v = argv[i]
			}
			switch v {
			case "text", "":
				o.jsonOut = false
			case "json":
				o.jsonOut = true
			default:
				return o, fmt.Errorf("--output-format 只支持 text|json（claude-p 不透传 stream-json，得到的是交互流）")
			}
		case ignoreBoolFlags[a]:
			// -p/SDK 专用，丢弃
		case ignoreValueFlags[a]:
			if i+1 < len(argv) {
				i++ // 连值一起丢弃
			}
		case a == "--monitor":
			o.monitor = true
		case a == "--keep":
			o.keep = true
		case a == "--no-auto-approve":
			o.noAutoApprove = true
		case a == "-p" || a == "--print":
			// drop-in 兼容：忽略
		case a == "--cwd":
			if i+1 >= len(argv) {
				return o, fmt.Errorf("--cwd 需要参数")
			}
			i++
			o.cwd = argv[i]
		case a == "--timeout":
			if i+1 >= len(argv) {
				return o, fmt.Errorf("--timeout 需要参数")
			}
			i++
			d, err := time.ParseDuration(argv[i])
			if err != nil {
				return o, fmt.Errorf("--timeout: %w", err)
			}
			o.timeout = d
		case valueFlags[a]:
			// 带值的透传 flag：flag 与其值一起进 passthrough，
			// 否则值（如 --resume 的 session id）会被误当成 prompt
			o.passthrough = append(o.passthrough, a)
			if i+1 < len(argv) {
				i++
				o.passthrough = append(o.passthrough, argv[i])
			}
		case !strings.HasPrefix(a, "-") && o.prompt == "":
			o.prompt = a
		default:
			o.passthrough = append(o.passthrough, a)
		}
	}
	return o, nil
}

func hasAny(s []string, vals ...string) bool {
	for _, x := range s {
		for _, v := range vals {
			if x == v {
				return true
			}
		}
	}
	return false
}

func isPipe(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice == 0
}

func uuidv4() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func printHelp() {
	fmt.Print(`claude-p —— claude -p 的平替：用真·交互 session 驱动 claude（不走 -p/Agent SDK），
            为 2026-06-15 计费分轨预判而建；内嵌透明监测代理。

用法：
  claude-p [flags] "你的问题"
  echo "你的问题" | claude-p [flags]
  claude-p --resume <session-id> "后续问题"        # 多轮（用 --json 拿上一轮 session_id）

claude-p 自有 flags：
  --json                 JSON 输出（贴近 claude -p：{type,result,session_id,...}）
  --monitor              结束后向 stderr 打印代理监测到的所有 /v1/messages 调用
  --keep                 结束后保留 tmux session（tmux attach -t 调试）
  --no-auto-approve      关闭工具自动放行（默认放行所有工具，无人值守用）
  --cwd <dir>            claude 工作目录（默认当前目录）
  --timeout <d>          等待回答超时（默认 240s，如 90s / 5m）
  -h, --help             本帮助

兼容 claude -p 的 flags：
  -p, --print            忽略（claude-p 本就不调 -p；放着是为了 drop-in）
  --output-format text|json   映射到上面的 text / --json（stream-json 不支持）
  --input-format / --verbose / --include-partial-messages 等  -p 专用，忽略

透传给底层 claude 的常用 flags（交互模式同样有效）：
  --model <m>  --fallback-model <m>  --append-system-prompt <s>  --system-prompt <s>
  --add-dir <d>  --mcp-config <c>  --allowedTools <t>  --disallowedTools <t>  --tools <t>
  --permission-mode <m>  --agent <a>  --effort <lvl>  --session-id <uuid>
  --resume/-r <id>  --continue/-c  --dangerously-skip-permissions  (其余未识别 flag 也会透传)
  注：变长值请用逗号形式，如 --allowedTools Bash,Read（避免与 prompt 混淆）

说明：
  - 以真·交互 REPL（tmux PTY）驱动 claude，spawn 前会 scrub 继承的 CLAUDE_*/ANTHROPIC_API_KEY，
    保证子 claude 是干净的订阅交互 session。属灰色地带，6/15 后行为可能变化。
  - 默认自动放行所有工具（PreToolUse hook），无人值守方便但有风险——慎用于不可信 prompt。
  - prompt 以 / 开头会被 REPL 当 slash 命令（已拦截）；多行 prompt 建议改单行。
  - 依赖 tmux + claude 在 PATH。详见 README.md。
`)
}
