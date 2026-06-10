package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// sessionConfig 描述一个被驱动的交互 claude session 怎么起。
type sessionConfig struct {
	cwd         string
	passthrough []string
	autoApprove bool
	useProxy    bool
}

// liveSession 持有一个 tmux 里运行的真·交互 claude，可被多次 prompt（多轮）。
type liveSession struct {
	tmp  string
	sess string // tmux session 名
	ln   net.Listener
	prox *proxy // 可为 nil
}

// newLiveSession 起 tmux 交互 claude、装好 Stop/PreToolUse hook，等就绪。
func newLiveSession(self string, cfg sessionConfig) (*liveSession, error) {
	if cfg.cwd == "" {
		cfg.cwd, _ = os.Getwd()
	}
	tmp, err := os.MkdirTemp("", "claude-p-")
	if err != nil {
		return nil, err
	}
	ls := &liveSession{tmp: tmp}
	ok := false
	defer func() {
		if !ok {
			ls.close()
		}
	}()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ls.ln = ln
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)

	settings := filepath.Join(tmp, "settings.json")
	if err := writeSettings(settings, fmt.Sprintf("%q __hook %s", self, port),
		fmt.Sprintf("%q __approve", self), cfg.autoApprove); err != nil {
		return nil, err
	}

	var baseURL string
	if cfg.useProxy {
		prox, err := startProxy()
		if err != nil {
			return nil, err
		}
		ls.prox = prox
		baseURL = "http://127.0.0.1:" + prox.port
	}

	sid := uuidv4()
	ls.sess = "claudep_" + sid[:8]
	claudeCmd := buildClaudeCmd(sid, settings, cfg.passthrough)
	args := []string{"new-session", "-d", "-s", ls.sess, "-x", "220", "-y", "50", "-c", cfg.cwd}
	if baseURL != "" {
		args = append(args, "-e", "ANTHROPIC_BASE_URL="+baseURL)
	}
	args = append(args, claudeCmd)
	if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("tmux 启动失败: %v: %s", err, out)
	}
	if err := waitReady(ls.sess, 40*time.Second); err != nil {
		return nil, err
	}
	ok = true
	return ls, nil
}

// prompt 注入一条 prompt 并等本轮答案（Stop hook 经 TCP 回传）。
func (ls *liveSession) prompt(text string, timeout time.Duration) (answer, error) {
	exec.Command("tmux", "send-keys", "-t", ls.sess, "-l", "--", text).Run()
	time.Sleep(150 * time.Millisecond)
	exec.Command("tmux", "send-keys", "-t", ls.sess, "Enter").Run()
	return acceptAnswer(ls.ln, timeout)
}

// interrupt 给交互 REPL 发 Esc（best-effort 取消当前轮）。
func (ls *liveSession) interrupt() {
	exec.Command("tmux", "send-keys", "-t", ls.sess, "Escape").Run()
}

func (ls *liveSession) close() {
	if ls.sess != "" {
		exec.Command("tmux", "kill-session", "-t", ls.sess).Run()
	}
	if ls.ln != nil {
		ls.ln.Close()
	}
	if ls.prox != nil {
		ls.prox.stop()
	}
	if ls.tmp != "" {
		os.RemoveAll(ls.tmp)
	}
}

func acceptAnswer(ln net.Listener, timeout time.Duration) (answer, error) {
	ch := make(chan answer, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadBytes('\n')
		var a answer
		json.Unmarshal(line, &a)
		ch <- a
	}()
	select {
	case a := <-ch:
		return a, nil
	case <-time.After(timeout):
		return answer{}, fmt.Errorf("等待回答超时（%s）；可能卡在工具/权限，试试 --keep 后 tmux attach 查看", timeout)
	}
}
