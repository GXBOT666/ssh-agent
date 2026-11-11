package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// AI 运维助手：对话循环 + 工具调用
//
// 模型只能通过下面这几个工具"动手"：
//   list_servers   查看已录入的服务器（含用途/负责人/最近结论）
//   add_server     新增服务器（IP/账号/密码由用户在终端里回答，不经过模型）
//   run_on_server  在指定服务器上执行命令
//   remember       把结论写入长期记忆
//
// 安全：所有命令仍然经过 policy 的黑名单与确认流程。
// ============================================================================

const defaultSystemPrompt = `你是一个 Linux 服务器运维助手，运行在用户的终端里，通过 SSH 管理服务器。

你能做的事（都用工具完成，不需要让用户记命令）：
- 查看/新增/删除服务器：list_servers、add_server、remove_server
- 执行命令：run_on_server（任何 shell 命令，包括查看服务状态、日志、进程、网络）
- 传文件：upload_file（下发配置/脚本，可顺带执行）、download_file（拉日志分析）
- 查历史/记结论：recall（查过去做过什么）、remember（记新结论）

工作原则：
1. 先看清楚再动手。不确定是哪台机器时，先用 list_servers 查看；不清楚现状时，先执行只读命令（uname -a、df -h、systemctl status 等）再下结论。
2. 一次只做一件事，命令要简短、可读、可复现。优先使用只读命令排查问题。
3. 绝不执行危险操作，除非用户明确要求：rm -rf、mkfs、dd 写盘、重启/关机、清空数据库、改防火墙、删除用户等。
4. 执行结果要如实转述：成功了说成功，失败了把关键错误贴出来并给出下一步建议。绝不要把失败说成成功。
5. 用户没说要改配置/删文件时，只做只读排查；要变更时先说清楚你要做什么。
5. 用户用中文提问时用中文回答。回答要直接、简短，先说结论，再说依据。

记忆规则（重要）：
每次完成一件有意义的运维工作（排查、变更、巡检结论）后，在回复末尾附加一段记忆，格式严格如下：
<memory>
summary: 一句话结论，例如「web-01 磁盘 / 使用率 91%，/var/log 日志占 30G，建议 logrotate」
host: web-01
detail: 关键数据或发现
note: 需要长期记住的注意事项（可选）
</memory>
规则：只有确实产生了有价值的信息才写记忆；纯闲聊或只是查看帮助时不要写。
summary 要具体，带上主机名和关键数字。
用户在添加服务器时描述的用途，会由程序自动记录，你不需要重复写。`

// Agent 是对话会话。
type Agent struct {
	cfg   *Config
	llm   *LLMClient
	store *HostStore
	mem   *Memory
	msgs  []chatMessage
	mu    sync.Mutex
	// 当前会话里已经连上的连接（按主机缓存，避免每次执行都重新登录）
	conns map[string]*Conn
	dial  *dialer
	// 交互回调（由 chat 命令注入，便于测试与不同的输入输出）
	ui UI
	// 本次回答里执行过的命令，用于写入记忆的执行记录
	sessionCmds []string
}

// UI 抽象终端输入输出，方便替换。
type UI interface {
	Println(a ...any)
	Printf(format string, a ...any)
	// Ask 提示用户输入一行（普通输入，可能为空表示使用默认值）
	Ask(prompt string) (string, error)
	// AskSecret 提示输入密码（不回显）
	AskSecret(prompt string) (string, error)
	// Confirm 是/否
	Confirm(prompt string) (bool, error)
}

// NewAgent 创建助手。
func NewAgent(cfg *Config, store *HostStore, mem *Memory, ui UI) (*Agent, error) {
	client, err := NewLLMClient(cfg.Model)
	if err != nil {
		return nil, err
	}
	a := &Agent{
		cfg: cfg, llm: client, store: store, mem: mem, ui: ui,
		conns: map[string]*Conn{},
		dial:  newDialer(cfg),
	}
	a.reset()
	return a, nil
}

// reset 清空对话，只保留系统提示。
func (a *Agent) reset() {
	a.msgs = []chatMessage{{Role: "system", Content: a.buildSystemPrompt()}}
}

// buildSystemPrompt 拼出系统提示：角色说明 + 服务器清单 + 最近记忆。
func (a *Agent) buildSystemPrompt() string {
	hosts, _ := a.store.List()
	var sb strings.Builder
	sys := a.cfg.Model.SystemPrompt
	if sys == "" {
		sys = defaultSystemPrompt
	}
	sb.WriteString(sys)
	sb.WriteString("\n\n# 已知服务器\n")
	if len(hosts) == 0 {
		sb.WriteString("（还没有录入任何服务器。请引导用户用 add_server 录入，或提示输入 /add）\n")
	} else {
		for _, h := range hosts {
			sb.WriteString("- ")
			sb.WriteString(h.Describe())
			sb.WriteString("\n")
			if len(h.Notes) > 0 {
				sb.WriteString("  备注: ")
				sb.WriteString(strings.Join(h.Notes, "；"))
				sb.WriteString("\n")
			}
		}
	}
	if memTail := a.mem.Tail(3000); strings.TrimSpace(memTail) != "" {
		sb.WriteString("\n# 最近的运维记忆（来自 MEMORY.md）\n")
		sb.WriteString(memTail)
		sb.WriteString("\n")
	}
	return sb.String()
}

// Reset 清空当前对话上下文（保留记忆文件）。
func (a *Agent) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closeAll()
	a.reset()
}

// Close 释放连接。
func (a *Agent) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closeAll()
	a.dial.Close()
}

func (a *Agent) closeAll() {
	for k, c := range a.conns {
		c.Close()
		delete(a.conns, k)
	}
}

// ============================================================================
// 工具定义
// ============================================================================

func (a *Agent) tools() []toolDef {
	return []toolDef{
		{
			Type: "function",
			Function: toolFuncDef{
				Name:        "list_servers",
				Description: "列出已录入的服务器，包含 IP、账号、用途、负责人和最近一次运维结论。不确定要操作哪台机器时先调用它。",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		},
		{
			Type: "function",
			Function: toolFuncDef{
				Name:        "run_on_server",
				Description: "在指定服务器上执行一条 shell 命令并返回输出。支持 && 和 | 。不要执行交互式命令（如 top、vim），它们会卡住等待输入。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"host": map[string]any{
							"type":        "string",
							"description": "服务器的 ID 或 IP，必须与 list_servers 返回的一一致",
						},
						"command": map[string]any{
							"type":        "string",
							"description": "要执行的 shell 命令，例如 df -h",
						},
						"purpose": map[string]any{
							"type":        "string",
							"description": "一句话说明为什么要执行这条命令",
						},
					},
					"required": []string{"host", "command"},
				},
			},
		},
		{
			Type: "function",
			Function: toolFuncDef{
				Name:        "recall",
				Description: "检索历史运维记忆。用户问\"之前/上次做过什么\"、或需要了解某台机器的历史时使用。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"host":  map[string]any{"type": "string", "description": "只看某台服务器（可选，留空=全部）"},
						"limit": map[string]any{"type": "integer", "description": "最多返回几条，默认 5"},
					},
				},
			},
		},
		{
			Type: "function",
			Function: toolFuncDef{
				Name:        "remember",
				Description: "把一条运维结论写入长期记忆文件。只记录有长期价值的信息（机器的用途变化、配置特点、踩过的坑、长期待办）。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"summary": map[string]any{"type": "string", "description": "一句话结论，带主机名和关键数字"},
						"hosts": map[string]any{
							"type":        "array",
							"items":       map[string]any{"type": "string"},
							"description": "相关服务器 ID 列表",
						},
						"notes": map[string]any{
							"type":        "array",
							"items":       map[string]any{"type": "string"},
							"description": "需要长期记住的条目（可选）",
						},
					},
					"required": []string{"summary"},
				},
			},
		},
		{
			Type: "function",
			Function: toolFuncDef{
				Name:        "upload_file",
				Description: "把本地文件上传到服务器。适合下发配置、脚本、证书等。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"host":    map[string]any{"type": "string", "description": "服务器 ID 或 IP"},
						"local":   map[string]any{"type": "string", "description": "本地文件路径"},
						"remote":  map[string]any{"type": "string", "description": "远端目标路径，例如 /etc/nginx/conf.d/app.conf"},
						"mode":    map[string]any{"type": "string", "description": "远端文件权限，默认 0644"},
						"command": map[string]any{"type": "string", "description": "上传后要执行的命令（可选），例如 nginx -t && nginx -s reload"},
					},
					"required": []string{"host", "local", "remote"},
				},
			},
		},
		{
			Type: "function",
			Function: toolFuncDef{
				Name:        "download_file",
				Description: "把服务器上的文件下载到本地，适合拉日志、配置做分析。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"host":   map[string]any{"type": "string", "description": "服务器 ID 或 IP"},
						"remote": map[string]any{"type": "string", "description": "远端文件路径"},
						"local":  map[string]any{"type": "string", "description": "本地保存路径，可留空自动放到 data/downloads/"},
					},
					"required": []string{"host", "remote"},
				},
			},
		},
		{
			Type: "function",
			Function: toolFuncDef{
				Name:        "remove_server",
				Description: "从管理列表中删除一台服务器（只删除本地记录，不影响服务器本身）。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"host": map[string]any{"type": "string", "description": "服务器 ID 或 IP"},
					},
					"required": []string{"host"},
				},
			},
		},
		{
			Type: "function",
			Function: toolFuncDef{
				Name:        "add_server",
				Description: "录入一台新服务器。调用前必须先问清楚 IP、登录账号、用途；调用后程序会在终端里请用户输入密码（密码不会经过你）。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"ip":       map[string]any{"type": "string", "description": "IP 或域名"},
						"username": map[string]any{"type": "string", "description": "登录账号，例如 root"},
						"purpose":  map[string]any{"type": "string", "description": "这台机器的用途，例如「生产环境 web 服务器，跑 Nginx」"},
						"port":     map[string]any{"type": "integer", "description": "SSH 端口，默认 22"},
						"owner":    map[string]any{"type": "string", "description": "负责人（可选）"},
						"tags": map[string]any{
							"type":        "array",
							"items":       map[string]any{"type": "string"},
							"description": "标签，例如 prod、nginx（可选）",
						},
					},
					"required": []string{"ip", "username", "purpose"},
				},
			},
		},
	}
}

// ============================================================================
// 对话主循环
// ============================================================================

// maxToolRounds 限制一次提问最多连续调用多少轮工具，防止模型死循环。
// 运维任务常常需要"查→改→验→再查"多轮迭代，因此给足空间。
const maxToolRounds = 30

// Ask 处理一句用户输入，返回最终回复文本（已剥离记忆标签）。
func (a *Agent) Ask(ctx context.Context, userInput string) (string, error) {
	a.mu.Lock()
	a.msgs = append(a.msgs, chatMessage{Role: "user", Content: userInput})
	a.sessionCmds = nil
	a.mu.Unlock()

	tools := a.tools()
	for round := 0; round < maxToolRounds; round++ {
		reply, err := a.llm.Chat(ctx, a.msgs, tools)
		if err != nil {
			return "", err
		}
		a.msgs = append(a.msgs, reply)

		if len(reply.ToolCalls) == 0 {
			text := reply.Content
			a.saveMemoryFromReply(text)
			a.refreshSystemPrompt()
			return StripMemoryTag(text), nil
		}

		for _, tc := range reply.ToolCalls {
			result := a.runTool(ctx, tc)
			a.msgs = append(a.msgs, chatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    result,
			})
		}
	}
	return "", errors.New("模型连续调用工具次数过多，已中止（可能是任务描述不清晰，换个问法试试）")
}

// refreshSystemPrompt 只重建系统提示（第 0 条），保留对话历史。
// 这样新录入的服务器和最新记忆能立刻被模型看到，又不会丢失上下文。
func (a *Agent) refreshSystemPrompt() {
	if len(a.msgs) == 0 {
		return
	}
	a.msgs[0] = chatMessage{Role: "system", Content: a.buildSystemPrompt()}
}

// saveMemoryFromReply 从回复中提取 <memory> 块并写入文件。
func (a *Agent) saveMemoryFromReply(reply string) {
	summary, hosts, details, notes, ok := ExtractMemory(reply)
	if !ok || strings.TrimSpace(summary) == "" {
		return
	}
	entry := MemoryEntry{
		Time:     time.Now(),
		Hosts:    hosts,
		Summary:  summary,
		Details:  details,
		Commands: append([]string{}, a.sessionCmds...),
	}
	if err := a.mem.Append(entry); err != nil {
		a.ui.Printf("  %s 记忆写入失败: %v\n", color("✘", 31), err)
		a.ui.Printf("    %s\n", dim("检查目录权限: ls -ld "+shortenPath(a.mem.DataDir)))
	} else {
		// 明确告诉用户存到哪了，并给出条目数
		n := a.mem.Count()
		a.ui.Printf("  %s 记忆 +1（共 %d 条）%s\n", color("✔", 32), n, dim("→ "+shortenPath(a.mem.MemoryMD)))
	}
	// 同步到机器级记忆：最近结论 + 长期备注
	for _, h := range hosts {
		rec, err := a.store.Find(h)
		if err != nil {
			continue
		}
		rec.LastSummary = summary
		if len(notes) > 0 {
			a.store.MergeNotes(rec, notes)
		}
		if err := a.store.Save(rec); err != nil {
			a.ui.Printf("  %s 更新 %s 的结论失败: %v\n", color("✘", 31), h, err)
		}
	}
}

// ============================================================================
// 工具执行
// ============================================================================

func (a *Agent) runTool(ctx context.Context, tc toolCall) string {
	var args map[string]any
	argStr := strings.TrimSpace(tc.Function.Arguments)
	if argStr == "" {
		argStr = "{}"
	}
	if err := json.Unmarshal([]byte(argStr), &args); err != nil {
		return toolError("参数不是合法 JSON: " + err.Error())
	}
	getStr := func(k string) string {
		if v, ok := args[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	getInt := func(k string) int {
		switch v := args[k].(type) {
		case float64:
			return int(v)
		case string:
			return atoiLoose(v)
		}
		return 0
	}
	getList := func(k string) []string {
		var out []string
		if arr, ok := args[k].([]any); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, strings.TrimSpace(s))
				}
			}
		}
		return out
	}

	switch tc.Function.Name {
	case "list_servers":
		return a.toolListServers()
	case "run_on_server":
		return a.toolRun(ctx, getStr("host"), getStr("command"), getStr("purpose"))
	case "recall":
		limit := getInt("limit")
		if limit <= 0 {
			limit = 5
		}
		return a.toolRecall(getStr("host"), limit)
	case "remember":
		return a.toolRemember(getStr("summary"), getList("hosts"), getList("notes"))
	case "upload_file":
		return a.toolUploadFile(ctx, getStr("host"), getStr("local"), getStr("remote"), getStr("mode"), getStr("command"))
	case "download_file":
		return a.toolDownloadFile(ctx, getStr("host"), getStr("remote"), getStr("local"))
	case "remove_server":
		return a.toolRemoveServer(getStr("host"))
	case "add_server":
		return a.toolAddServer(getStr("ip"), getStr("username"), getStr("purpose"), getInt("port"), getStr("owner"), getList("tags"))
	default:
		return toolError("未知工具: " + tc.Function.Name)
	}
}

func toolError(msg string) string {
	return "错误: " + msg
}

func (a *Agent) toolListServers() string {
	hosts, err := a.store.List()
	if err != nil {
		return toolError("读取服务器列表失败: " + err.Error())
	}
	if len(hosts) == 0 {
		return "还没有录入任何服务器。请提示用户先录入（或在终端输入 /add）。"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "共 %d 台服务器：\n", len(hosts))
	for _, h := range hosts {
		fmt.Fprintf(&sb, "- ID=%s  IP=%s:%d  账号=%s  用途=%s", h.ID, h.IP, portOrDefault(h.Port), h.Username, orNone(h.Purpose))
		if h.Owner != "" {
			fmt.Fprintf(&sb, "  负责人=%s", h.Owner)
		}
		if len(h.Tags) > 0 {
			fmt.Fprintf(&sb, "  标签=%s", strings.Join(h.Tags, ","))
		}
		if h.LastSummary != "" {
			fmt.Fprintf(&sb, "\n    最近结论: %s", firstLine(h.LastSummary))
		}
		if len(h.Notes) > 0 {
			fmt.Fprintf(&sb, "\n    备注: %s", strings.Join(h.Notes, "；"))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "（未填写）"
	}
	return s
}

// toolRun 在目标机上执行命令。
func (a *Agent) toolRun(ctx context.Context, hostArg, command, purpose string) string {
	if hostArg == "" {
		return toolError("缺少 host 参数")
	}
	if strings.TrimSpace(command) == "" {
		return toolError("缺少 command 参数")
	}
	rec, err := a.store.Find(hostArg)
	if err != nil {
		return toolError(err.Error())
	}

	conn, err := a.connectionFor(ctx, rec)
	if err != nil {
		return toolError(fmt.Sprintf("连接 %s 失败: %v", rec.ID, err))
	}

	// 单条命令在 AI 模式下最多跑 120 秒，避免模型触发长时间阻塞
	timeout := 120 * time.Second
	a.ui.Printf("  %s %s %s\n", dim(rec.ID), dim("$"), command)

	r := conn.Run(ctx, command, timeout)
	a.sessionCmds = append(a.sessionCmds, fmt.Sprintf("[%s] %s", rec.ID, command))

	var sb strings.Builder
	fmt.Fprintf(&sb, "主机: %s (%s)\n命令: %s\n退出码: %d\n", rec.ID, rec.Addr(), command, r.ExitCode)
	if r.Err != nil {
		fmt.Fprintf(&sb, "执行错误: %v\n", r.Err)
	}
	if strings.TrimSpace(r.Stdout) != "" {
		fmt.Fprintf(&sb, "--- stdout ---\n%s\n", truncateForModel(r.Stdout, 12000))
	}
	if strings.TrimSpace(r.Stderr) != "" {
		fmt.Fprintf(&sb, "--- stderr ---\n%s\n", truncateForModel(r.Stderr, 4000))
	}
	if strings.TrimSpace(r.Stdout) == "" && strings.TrimSpace(r.Stderr) == "" {
		sb.WriteString("（没有输出）\n")
	}
	return sb.String()
}

func truncateForModel(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n...（输出过长，已截断，原长 %d 字节）", len(s))
}

// connectionFor 获取（或建立）到服务器的连接。
func (a *Agent) connectionFor(ctx context.Context, rec *HostRecord) (*Conn, error) {
	if c, ok := a.conns[rec.ID]; ok {
		return c, nil
	}
	sub, srv, err := a.store.BuildSSHConfig(a.cfg, rec)
	if err != nil {
		return nil, err
	}
	d := newDialer(sub)
	conn, err := d.Connect(ctx, srv)
	if err != nil {
		d.Close()
		return nil, err
	}
	a.conns[rec.ID] = conn
	a.store.Touch(rec)
	return conn, nil
}

// toolRecall 检索历史记忆。
func (a *Agent) toolRecall(host string, limit int) string {
	if limit > 20 {
		limit = 20
	}
	var entries []string
	if host != "" {
		entries = a.mem.RecentEntries(host, limit)
	} else {
		recs, _ := a.mem.ReadAll()
		secs := lastSections(recs, limit)
		entries = secs
	}
	if len(entries) == 0 {
		return "没有找到相关记忆"
	}
	return strings.Join(entries, "\n\n")
}

func (a *Agent) toolRemember(summary string, hosts, notes []string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return toolError("summary 不能为空")
	}
	entry := MemoryEntry{Time: time.Now(), Hosts: hosts, Summary: summary, Commands: a.sessionCmds}
	if err := a.mem.Append(entry); err != nil {
		return toolError("写入记忆失败: " + err.Error())
	}
	for _, h := range hosts {
		if rec, err := a.store.Find(h); err == nil {
			rec.LastSummary = summary
			if len(notes) > 0 {
				a.store.MergeNotes(rec, notes)
			}
			_ = a.store.Save(rec)
		}
	}
	return "已写入记忆: " + summary
}

// toolAddServer 录入新服务器。密码由用户在终端直接输入。
func (a *Agent) toolAddServer(ip, username, purpose string, port int, owner string, tags []string) string {
	ip = strings.TrimSpace(ip)
	if ip == "" || username == "" {
		return toolError("ip 和 username 不能为空")
	}
	// 防止模型编造一个"看起来像"的 IP
	if !looksLikeHost(ip) {
		return toolError("ip 格式不像合法地址: " + ip)
	}
	if port == 0 {
		port = 22
	}
	rec := &HostRecord{
		ID:       ip,
		IP:       ip,
		Port:     port,
		Username: username,
		Purpose:  purpose,
		Owner:    owner,
		Tags:     tags,
	}
	if exist, err := a.store.Find(ip); err == nil && exist.Port == port {
		return fmt.Sprintf("服务器 %s 已存在（用途: %s）。如需修改请提示用户使用 /edit。", exist.ID, orNone(exist.Purpose))
	}

	a.ui.Printf("  %s@%s:%d %s\n", username, ip, port, purpose)
	pw, err := a.ui.AskSecret("  pass > ")
	if err != nil {
		return toolError("读取密码失败: " + err.Error())
	}
	rec.Password = pw
	if pw == "" {
		keyFile, _ := a.ui.Ask("  keyfile > ")
		rec.KeyFile = strings.TrimSpace(keyFile)
	}
	if err := a.store.Save(rec); err != nil {
		return toolError("保存失败: " + err.Error())
	}
	a.ui.Printf("  %s\n", dim(a.store.fileFor(rec.ID)))

	// 立刻验证一次登录，失败也要如实告诉模型和用户
	sub, srv, err := a.store.BuildSSHConfig(a.cfg, rec)
	if err != nil {
		return "已保存，但配置有误: " + err.Error()
	}
	d := newDialer(sub)
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := d.Connect(ctx, srv)
	if err != nil {
		return fmt.Sprintf("已保存服务器 %s，但连接测试失败: %v（请检查密码/端口/防火墙）", rec.ID, err)
	}
	conn.Close()
	return fmt.Sprintf("已保存服务器 %s，连接测试成功（%s@%s:%d，用途: %s）。", rec.ID, username, ip, port, orNone(purpose))
}

// toolUploadFile 上传本地文件到服务器，可选上传后执行命令。
func (a *Agent) toolUploadFile(ctx context.Context, hostArg, local, remote, modeStr, after string) string {
	if hostArg == "" || local == "" || remote == "" {
		return toolError("host / local / remote 都不能为空")
	}
	rec, err := a.store.Find(hostArg)
	if err != nil {
		return toolError(err.Error())
	}
	localPath := expandHome(local)
	if !filepath.IsAbs(localPath) {
		localPath = filepath.Join(a.cfg.dir, localPath)
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		return toolError("读取本地文件失败: " + err.Error())
	}
	perm, err := parseMode(orDefault(modeStr, "0644"))
	if err != nil {
		return toolError(err.Error())
	}
	conn, err := a.connectionFor(ctx, rec)
	if err != nil {
		return toolError("连接失败: " + err.Error())
	}
	a.ui.Printf("  %s %s %s -> %s\n", dim(rec.ID), dim("↑"), filepath.Base(localPath), remote)
	if err := uploadBytes(conn, data, remote, perm); err != nil {
		return toolError("上传失败: " + err.Error())
	}
	msg := fmt.Sprintf("已上传 %s 到 %s:%s（%d 字节，权限 %s）", filepath.Base(localPath), rec.ID, remote, len(data), perm)
	if strings.TrimSpace(after) != "" {
		a.ui.Printf("  %s %s %s\n", dim(rec.ID), dim("$"), after)
		r := conn.Run(ctx, after, 120*time.Second)
		msg += fmt.Sprintf("\n后续命令退出码 %d\n%s", r.ExitCode, truncateForModel(r.Stdout+r.Stderr, 4000))
	}
	return msg
}

// toolDownloadFile 下载远端文件到本地。
func (a *Agent) toolDownloadFile(ctx context.Context, hostArg, remote, local string) string {
	if hostArg == "" || remote == "" {
		return toolError("host / remote 不能为空")
	}
	rec, err := a.store.Find(hostArg)
	if err != nil {
		return toolError(err.Error())
	}
	if strings.TrimSpace(local) == "" {
		local = filepath.Join(a.cfg.DataDir, "downloads", sanitizeName(rec.ID)+"-"+filepath.Base(remote))
	} else {
		local = expandHome(local)
		if !filepath.IsAbs(local) {
			local = filepath.Join(a.cfg.dir, local)
		}
	}
	conn, err := a.connectionFor(ctx, rec)
	if err != nil {
		return toolError("连接失败: " + err.Error())
	}
	a.ui.Printf("  %s %s %s -> %s\n", dim(rec.ID), dim("↓"), remote, local)
	data, mode, err := downloadFile(conn, remote)
	if err != nil {
		return toolError("下载失败: " + err.Error())
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
		return toolError(err.Error())
	}
	perm := os.FileMode(0o600)
	if mode&os.ModePerm != 0 {
		perm = mode & os.ModePerm
	}
	if err := os.WriteFile(local, data, perm); err != nil {
		return toolError("保存失败: " + err.Error())
	}
	return fmt.Sprintf("已下载 %s:%s 到 %s（%d 字节）", rec.ID, remote, local, len(data))
}

// toolRemoveServer 删除服务器记录（需用户确认）。
func (a *Agent) toolRemoveServer(hostArg string) string {
	rec, err := a.store.Find(hostArg)
	if err != nil {
		return toolError(err.Error())
	}
	ok, err := a.ui.Confirm(fmt.Sprintf("确认删除服务器 %s（%s，%s）？只删除本地记录", rec.ID, rec.Addr(), orNone(rec.Purpose)))
	if err != nil || !ok {
		return "用户取消了删除"
	}
	if c, ok := a.conns[rec.ID]; ok {
		c.Close()
		delete(a.conns, rec.ID)
	}
	if err := a.store.Delete(rec.ID); err != nil {
		return toolError("删除失败: " + err.Error())
	}
	return "已删除服务器记录: " + rec.ID
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// looksLikeHost 粗校验 IP/域名格式，避免模型编造错误地址。
func looksLikeHost(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '-' || r == '_' || r == ':' {
			continue
		}
		return false
	}
	return true
}

// shortenPath 把数据目录、家目录替换成短写，让路径能放进行内提示。
func shortenPath(p string) string {
	if dir := execDir(); dir != "" && strings.HasPrefix(p, dir+string(filepath.Separator)) {
		return strings.TrimPrefix(p, dir+string(filepath.Separator))
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// dim 是终端灰字（非终端时原样返回）。
func dim(s string) string { return color(s, 90) }
