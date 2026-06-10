package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// usageMain: claude-p usage [--json] [--period 24h|7d|all]
// 优先读本地 ~/.claude/projects/**/*.jsonl，没数据时 fallback 到 TUI capture-pane。
func usageMain(argv []string) error {
	jsonOut := false
	period := "24h"
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--json", "-j":
			jsonOut = true
		case "--period", "-p":
			if i+1 < len(argv) {
				i++
				period = argv[i]
			}
		case "--help", "-h":
			fmt.Print(`claude-p usage — 查看本机 claude 用量（读本地 jsonl，无需联网）

用法：
  claude-p usage [--json] [--period 24h|7d|all]

  --json          JSON 输出
  --period        统计范围：24h（默认）/ 7d / all
`)
			return nil
		}
	}

	rows, err := readLocalUsage(period)
	if err != nil || len(rows) == 0 {
		// fallback: TUI capture
		return usageFallbackTUI(jsonOut)
	}

	type modelStat struct {
		Model       string `json:"model"`
		Input       int64  `json:"input_tokens"`
		Output      int64  `json:"output_tokens"`
		CacheRead   int64  `json:"cache_read_tokens"`
		CacheWrite  int64  `json:"cache_write_tokens"`
		Calls       int    `json:"calls"`
	}
	byModel := map[string]*modelStat{}
	for _, r := range rows {
		s, ok := byModel[r.Model]
		if !ok {
			s = &modelStat{Model: r.Model}
			byModel[r.Model] = s
		}
		s.Input += r.Input
		s.Output += r.Output
		s.CacheRead += r.CacheRead
		s.CacheWrite += r.CacheWrite
		s.Calls++
	}

	models := make([]modelStat, 0, len(byModel))
	var totalInput, totalOutput, totalCacheRead, totalCacheWrite int64
	totalCalls := 0
	for _, s := range byModel {
		models = append(models, *s)
		totalInput += s.Input
		totalOutput += s.Output
		totalCacheRead += s.CacheRead
		totalCacheWrite += s.CacheWrite
		totalCalls += s.Calls
	}

	if jsonOut {
		out, _ := json.MarshalIndent(map[string]any{
			"period":            period,
			"source":            "local_jsonl",
			"note":              "本机本地数据，不含其他设备与 claude.ai",
			"total_calls":       totalCalls,
			"total_input":       totalInput,
			"total_output":      totalOutput,
			"total_cache_read":  totalCacheRead,
			"total_cache_write": totalCacheWrite,
			"by_model":          models,
		}, "", "  ")
		fmt.Println(string(out))
		return nil
	}

	// text output
	fmt.Printf("claude-p usage  [%s，本机本地，不含其他设备/claude.ai]\n", period)
	fmt.Printf("─────────────────────────────────────────────────────\n")
	fmt.Printf("  总调用次数:       %d\n", totalCalls)
	fmt.Printf("  input tokens:     %s\n", fmtK(totalInput))
	fmt.Printf("  output tokens:    %s\n", fmtK(totalOutput))
	fmt.Printf("  cache read:       %s\n", fmtK(totalCacheRead))
	fmt.Printf("  cache write:      %s\n", fmtK(totalCacheWrite))
	fmt.Printf("\n  按 model:\n")
	for _, s := range models {
		fmt.Printf("    %-32s  calls=%d  in=%s  out=%s  cache_r=%s  cache_w=%s\n",
			s.Model, s.Calls, fmtK(s.Input), fmtK(s.Output), fmtK(s.CacheRead), fmtK(s.CacheWrite))
	}
	return nil
}

type usageRow struct {
	Timestamp  time.Time
	Model      string
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

func readLocalUsage(period string) ([]usageRow, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	projectsDir := filepath.Join(home, ".claude", "projects")

	var cutoff time.Time
	switch period {
	case "7d":
		cutoff = time.Now().Add(-7 * 24 * time.Hour)
	case "all":
		cutoff = time.Time{}
	default: // 24h
		cutoff = time.Now().Add(-24 * time.Hour)
	}

	var rows []usageRow
	err = filepath.WalkDir(projectsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		// 快速跳过太旧的文件（按文件 mtime 过滤，避免扫大量旧文件）
		if !cutoff.IsZero() {
			fi, err := d.Info()
			if err == nil && fi.ModTime().Before(cutoff) {
				return nil
			}
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var rec struct {
				Type      string `json:"type"`
				Timestamp string `json:"timestamp"`
				Message   *struct {
					Model string `json:"model"`
					Usage *struct {
						InputTokens              int64 `json:"input_tokens"`
						OutputTokens             int64 `json:"output_tokens"`
						CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
						CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(sc.Bytes(), &rec) != nil {
				continue
			}
			if rec.Type != "assistant" || rec.Message == nil || rec.Message.Usage == nil {
				continue
			}
			ts, err := time.Parse(time.RFC3339, rec.Timestamp)
			if err != nil {
				ts, err = time.Parse("2006-01-02T15:04:05.999999999Z07:00", rec.Timestamp)
				if err != nil {
					continue
				}
			}
			if !cutoff.IsZero() && ts.Before(cutoff) {
				continue
			}
			u := rec.Message.Usage
			rows = append(rows, usageRow{
				Timestamp:  ts,
				Model:      rec.Message.Model,
				Input:      u.InputTokens,
				Output:     u.OutputTokens,
				CacheRead:  u.CacheReadInputTokens,
				CacheWrite: u.CacheCreationInputTokens,
			})
		}
		return nil
	})
	return rows, err
}

// fmtK: 大数字加 k/M 后缀
func fmtK(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// usageFallbackTUI: 起一个 claude session，发 /usage，capture-pane 后截图文字输出。
func usageFallbackTUI(jsonOut bool) error {
	fmt.Fprintln(os.Stderr, "[claude-p] 本地无 jsonl 数据，fallback 到 TUI capture…")
	self, _ := os.Executable()
	tmp, err := os.MkdirTemp("", "claude-p-usage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	settings := filepath.Join(tmp, "settings.json")
	os.WriteFile(settings, []byte(`{}`), 0o600)

	cwd, _ := os.Getwd()
	sid := uuidv4()
	sess := "claudep_usage_" + sid[:8]

	env := "env"
	for _, v := range scrubEnv {
		env += " -u " + v
	}
	claudeCmd := env + " " + fmt.Sprintf("'%s'", self) + " --settings '" + settings + "'"
	// 直接用 claude binary（不通过 claude-p），避免循环
	claudeCmd = env + " claude --settings '" + settings + "'"

	args := []string{"new-session", "-d", "-s", sess, "-x", "200", "-y", "50", "-c", cwd, claudeCmd}
	if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux 启动失败: %v: %s", err, out)
	}
	defer exec.Command("tmux", "kill-session", "-t", sess).Run()

	if err := waitReady(sess, 30*time.Second); err != nil {
		return fmt.Errorf("claude 未就绪: %w", err)
	}

	// 发 /usage（不用 -l，直接 type 键入）
	exec.Command("tmux", "send-keys", "-t", sess, "/usage", "").Run()
	exec.Command("tmux", "send-keys", "-t", sess, "Enter", "").Run()

	// 等 "Esc to cancel" 出现
	deadline := time.Now().Add(15 * time.Second)
	var pane string
	for time.Now().Before(deadline) {
		out, _ := exec.Command("tmux", "capture-pane", "-t", sess, "-p").Output()
		pane = string(out)
		if strings.Contains(pane, "Esc to cancel") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	exec.Command("tmux", "send-keys", "-t", sess, "Escape", "").Run()

	// 提取 /usage 输出部分（从分隔线到 "Esc to cancel"）
	text := extractUsageSection(pane)
	if jsonOut {
		out, _ := json.Marshal(map[string]any{"source": "tui_capture", "raw": text})
		fmt.Println(string(out))
	} else {
		fmt.Println(text)
	}
	return nil
}

func extractUsageSection(pane string) string {
	lines := strings.Split(pane, "\n")
	start, end := -1, len(lines)
	for i, l := range lines {
		if start == -1 && strings.Contains(l, "Settings") && strings.Contains(l, "Usage") {
			start = i
		}
		if strings.Contains(l, "Esc to cancel") {
			end = i
			break
		}
	}
	if start == -1 {
		return strings.TrimSpace(pane)
	}
	return strings.TrimSpace(strings.Join(lines[start:end], "\n"))
}
