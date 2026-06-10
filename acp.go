package main

// ACP (Agent Client Protocol) server —— 让 Zed / Neovim 等 ACP 客户端把 claude-p
// 当成一个 agent 接入：每个 ACP session 背一个常驻交互 claude（订阅档），
// session/prompt 注入 prompt、用 Stop hook 取回答案、以 session/update 流回。
//
// 协议：JSON-RPC 2.0 over stdio，按行分隔（ndjson）。
// 实现的方法：initialize / session/new / session/prompt / session/cancel / authenticate。
// 限制（v1）：答案在本轮结束后整段以一个 agent_message_chunk 回传，非逐 token 流式。

import (
	"bufio"
	"encoding/json"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type acpServer struct {
	self     string
	wmu      sync.Mutex
	smu      sync.Mutex
	sessions map[string]*liveSession
}

func acpMain() {
	self, _ := os.Executable()
	s := &acpServer{self: self, sessions: map[string]*liveSession{}}
	cleanup := func() {
		s.smu.Lock()
		for _, ls := range s.sessions {
			ls.close()
		}
		s.smu.Unlock()
	}
	defer cleanup()

	// Zed 关闭 agent 多半发 SIGTERM/SIGINT —— 兜底杀掉常驻 tmux session。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		cleanup()
		os.Exit(0)
	}()

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m rpcMsg
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		// 每条消息独立 goroutine：prompt 处理可能很久，期间仍能响应 cancel。
		go s.dispatch(m)
	}
}

func (s *acpServer) dispatch(m rpcMsg) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion int `json:"protocolVersion"`
		}
		json.Unmarshal(m.Params, &p)
		ver := p.ProtocolVersion
		if ver == 0 {
			ver = 1
		}
		s.reply(m.ID, map[string]any{
			"protocolVersion": ver,
			"agentCapabilities": map[string]any{
				"loadSession": false,
				"promptCapabilities": map[string]any{
					"image":           false,
					"audio":           false,
					"embeddedContext": true,
				},
			},
			"authMethods": []any{},
		})

	case "authenticate":
		s.reply(m.ID, map[string]any{})

	case "session/new":
		var p struct {
			Cwd string `json:"cwd"`
		}
		json.Unmarshal(m.Params, &p)
		ls, err := newLiveSession(s.self, sessionConfig{cwd: p.Cwd, autoApprove: true, useProxy: false})
		if err != nil {
			s.replyErr(m.ID, -32000, "session/new 失败: "+err.Error())
			return
		}
		id := uuidv4()
		s.smu.Lock()
		s.sessions[id] = ls
		s.smu.Unlock()
		s.reply(m.ID, map[string]any{"sessionId": id})

	case "session/prompt":
		var p struct {
			SessionID string         `json:"sessionId"`
			Prompt    []contentBlock `json:"prompt"`
		}
		json.Unmarshal(m.Params, &p)
		ls := s.get(p.SessionID)
		if ls == nil {
			s.replyErr(m.ID, -32602, "unknown sessionId")
			return
		}
		ans, err := ls.prompt(blocksToText(p.Prompt), 600*time.Second)
		if err != nil {
			s.replyErr(m.ID, -32000, err.Error())
			return
		}
		s.notify("session/update", map[string]any{
			"sessionId": p.SessionID,
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]any{"type": "text", "text": ans.Answer},
			},
		})
		s.reply(m.ID, map[string]any{"stopReason": "end_turn"})

	case "session/cancel":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		json.Unmarshal(m.Params, &p)
		if ls := s.get(p.SessionID); ls != nil {
			ls.interrupt()
		}
		// notification，无需回复

	default:
		if len(m.ID) > 0 {
			s.replyErr(m.ID, -32601, "method not found: "+m.Method)
		}
	}
}

func (s *acpServer) get(id string) *liveSession {
	s.smu.Lock()
	defer s.smu.Unlock()
	return s.sessions[id]
}

// contentBlock 是 ACP prompt 里的内容块；我们只取 text。
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func blocksToText(blocks []contentBlock) string {
	out := ""
	for _, b := range blocks {
		if b.Type == "text" {
			if out != "" {
				out += "\n"
			}
			out += b.Text
		}
	}
	return out
}

func (s *acpServer) reply(id json.RawMessage, result any) {
	r, _ := json.Marshal(result)
	s.send(rpcMsg{JSONRPC: "2.0", ID: id, Result: r})
}

func (s *acpServer) replyErr(id json.RawMessage, code int, msg string) {
	s.send(rpcMsg{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *acpServer) notify(method string, params any) {
	p, _ := json.Marshal(params)
	s.send(rpcMsg{JSONRPC: "2.0", Method: method, Params: p})
}

func (s *acpServer) send(m rpcMsg) {
	b, _ := json.Marshal(m)
	s.wmu.Lock()
	os.Stdout.Write(append(b, '\n'))
	s.wmu.Unlock()
}
